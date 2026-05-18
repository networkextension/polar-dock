package dock

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// POST /api/proxy/v1/chat/completions — OpenAI-compatible chat
// completion proxy. P0: non-streaming only.
//
// Auth: Bearer <plaintext> in Authorization header.
// Routing: token's default_llm_config_id picks the upstream config.
// The client's `model` field is recorded for audit but the actual
// model dispatched is llm_configs.model on the upstream side.
//
// Audit: every dispatch (success or failure) inserts an
// llm_proxy_calls row.
func (s *Server) handleLLMProxyChatCompletions(c *gin.Context) {
	start := time.Now()

	// 1) Auth — bearer token.
	authz := strings.TrimSpace(c.GetHeader("Authorization"))
	if !strings.HasPrefix(authz, "Bearer ") {
		openAIError(c, http.StatusUnauthorized, "invalid_api_key", "missing bearer token")
		return
	}
	plaintext := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
	if plaintext == "" {
		openAIError(c, http.StatusUnauthorized, "invalid_api_key", "empty bearer token")
		return
	}
	hash := hashLLMProxyToken(plaintext)
	tok, err := s.lookupLLMProxyTokenByHash(hash)
	if err != nil {
		openAIError(c, http.StatusInternalServerError, "internal_error", "token lookup failed")
		return
	}
	if tok == nil {
		openAIError(c, http.StatusUnauthorized, "invalid_api_key", "token not recognized or revoked")
		return
	}

	// 2) Parse body.
	var req struct {
		Model              string                    `json:"model"`
		Messages           []aiChatCompletionMessage `json:"messages" binding:"required"`
		MaxTokens          int                       `json:"max_tokens,omitempty"`
		Stream             bool                      `json:"stream,omitempty"`
		// chat_template_kwargs is the vLLM/SGLang escape hatch
		// for provider-specific generation flags. Most-common use:
		// Qwen-3 family clients pass {"enable_thinking": false} to
		// suppress the thinking trace (5-10x speedup, much shorter
		// completions). We forward verbatim; the proxy doesn't
		// validate the inner keys because they're model-specific
		// and change between releases.
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "bad json body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "messages must be non-empty")
		return
	}
	mode := "nonstream"
	if req.Stream {
		mode = "stream"
	}
	// 3) Rate limit gate. Uses the token's stored rate_limit_rpm; 0
	// means unlimited. On reject we still write an audit row with
	// status=429 so the operator can see who's hammering the proxy.
	if s.llmProxyRateLimiter != nil {
		if ok, retryAfter := s.llmProxyRateLimiter.allow(tok.ID, tok.RateLimitRPM); !ok {
			s.recordProxyCallFailure(tok, req.Model, "", 0, http.StatusTooManyRequests, "rate limit exceeded", start)
			s.metricsRegistry.recordLLMProxyCall(tok.Name, req.Model, mode, http.StatusTooManyRequests, time.Since(start), 0, 0)
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			openAIError(c, http.StatusTooManyRequests, "rate_limit_exceeded", "token rate limit exceeded; retry in "+strconv.Itoa(retryAfter)+"s")
			return
		}
	}

	// 3b) Monthly USD quota gate (P4). 60s in-memory cache; fail-open
	// on DB error. quota=0 = unlimited. Status 402 Payment Required
	// + audit row so the operator can see who is hitting the cap.
	if s.llmProxyQuota != nil && tok.MonthlyQuotaUSD > 0 {
		if ok, spend := s.llmProxyQuota.allow(tok.ID, tok.MonthlyQuotaUSD); !ok {
			s.recordProxyCallFailure(tok, req.Model, "", 0, http.StatusPaymentRequired,
				fmt.Sprintf("monthly quota exceeded: $%.4f >= $%.4f", spend, tok.MonthlyQuotaUSD), start)
			s.metricsRegistry.recordLLMProxyCall(tok.Name, req.Model, mode, http.StatusPaymentRequired, time.Since(start), 0, 0)
			s.metricsRegistry.recordLLMProxyQuotaExceeded(tok.Name)
			openAIError(c, http.StatusPaymentRequired, "monthly_quota_exceeded",
				fmt.Sprintf("token monthly quota exceeded ($%.4f / $%.4f)", spend, tok.MonthlyQuotaUSD))
			return
		}
	}

	// 4) Resolve upstream LLM config — alias route first, then
	// fall back to the token's default. An alias matches the
	// client's `model` field exactly (case-sensitive).
	var resolvedLLMConfigID int64
	if cfgID, found, lookupErr := s.resolveLLMProxyRoute(tok.ID, req.Model); lookupErr == nil && found {
		resolvedLLMConfigID = cfgID
	} else if tok.DefaultLLMConfigID != nil && *tok.DefaultLLMConfigID > 0 {
		resolvedLLMConfigID = *tok.DefaultLLMConfigID
	}
	if resolvedLLMConfigID <= 0 {
		s.recordProxyCallFailure(tok, req.Model, "", 0, http.StatusBadRequest, "no route + no default_llm_config_id", start)
		s.metricsRegistry.recordLLMProxyCall(tok.Name, req.Model, mode, http.StatusBadRequest, time.Since(start), 0, 0)
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "no alias route matched model="+req.Model+" and token has no default LLM configured")
		return
	}
	cfg, apiKey, err := s.getLLMConfigForOwner(tok.WorkspaceID, resolvedLLMConfigID)
	if err != nil {
		s.recordProxyCallFailure(tok, req.Model, "", resolvedLLMConfigID, http.StatusInternalServerError, "llm config lookup failed", start)
		s.metricsRegistry.recordLLMProxyCall(tok.Name, req.Model, mode, http.StatusInternalServerError, time.Since(start), 0, 0)
		openAIError(c, http.StatusInternalServerError, "internal_error", "upstream config lookup failed")
		return
	}
	if cfg == nil {
		s.recordProxyCallFailure(tok, req.Model, "", resolvedLLMConfigID, http.StatusBadRequest, "llm config missing", start)
		s.metricsRegistry.recordLLMProxyCall(tok.Name, req.Model, mode, http.StatusBadRequest, time.Since(start), 0, 0)
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "configured upstream LLM is missing or out of scope")
		return
	}
	if strings.TrimSpace(apiKey) == "" {
		s.recordProxyCallFailure(tok, req.Model, cfg.Model, cfg.ID, http.StatusBadGateway, "upstream missing api key", start)
		s.metricsRegistry.recordLLMProxyCall(tok.Name, cfg.Model, mode, http.StatusBadGateway, time.Since(start), 0, 0)
		openAIError(c, http.StatusBadGateway, "upstream_error", "upstream LLM is not configured (missing api key)")
		return
	}

	// 4b) Per-team monthly USD budget gate (B1). Only enforced when an
	// llm_config_team_quotas row exists for (workspace, config) and
	// budget>0 — absent row means "no team-level cap" (back-compat
	// for tokens against admin-owned configs). Spend is summed from
	// cost_usd in llm_proxy_calls for the current calendar month.
	if quota, qErr := s.getLLMConfigTeamQuota(cfg.ID, tok.WorkspaceID); qErr == nil && quota != nil && quota.MonthlyBudgetUSD > 0 {
		spend, _ := s.teamLLMConfigMonthSpend(tok.WorkspaceID, cfg.ID)
		if spend >= quota.MonthlyBudgetUSD {
			s.recordProxyCallFailure(tok, req.Model, cfg.Model, cfg.ID, http.StatusPaymentRequired,
				fmt.Sprintf("team monthly budget exceeded: $%.4f >= $%.4f", spend, quota.MonthlyBudgetUSD), start)
			s.metricsRegistry.recordLLMProxyCall(tok.Name, cfg.Model, mode, http.StatusPaymentRequired, time.Since(start), 0, 0)
			s.metricsRegistry.recordLLMProxyQuotaExceeded(tok.Name)
			openAIError(c, http.StatusPaymentRequired, "team_quota_exceeded",
				fmt.Sprintf("team monthly budget for this LLM exceeded ($%.4f / $%.4f) — top up or wait for next month", spend, quota.MonthlyBudgetUSD))
			return
		}
	}

	// 4c) Prepaid credit balance gate (B4). Only enforced when at
	// least one recharge row exists for (workspace, config) — no
	// recharge = the team hasn't opted into the prepaid model, free
	// use continues (same back-compat shape as 4b). Balance is
	// SUM(recharges) - SUM(all-time cost_usd); negative or zero
	// rejects with 402 insufficient_credit so the UI can show a
	// "充值" prompt.
	if hasRecharge, _ := s.teamHasLLMConfigRecharge(tok.WorkspaceID, cfg.ID); hasRecharge {
		balance, _ := s.teamLLMConfigBalance(tok.WorkspaceID, cfg.ID)
		if balance <= 0 {
			s.recordProxyCallFailure(tok, req.Model, cfg.Model, cfg.ID, http.StatusPaymentRequired,
				fmt.Sprintf("credit balance exhausted: $%.4f", balance), start)
			s.metricsRegistry.recordLLMProxyCall(tok.Name, cfg.Model, mode, http.StatusPaymentRequired, time.Since(start), 0, 0)
			s.metricsRegistry.recordLLMProxyQuotaExceeded(tok.Name)
			openAIError(c, http.StatusPaymentRequired, "insufficient_credit",
				fmt.Sprintf("team prepaid balance for this LLM is exhausted (current: $%.4f) — please top up", balance))
			return
		}
	}

	// 5) Dispatch.
	runtimeConfig := aiRuntimeConfig{
		APIKey:        apiKey,
		BaseURL:       cfg.BaseURL,
		Model:         cfg.Model,
		SystemPrompt:  cfg.SystemPrompt,
		ProxyURL:      cfg.ProxyURL,
		Streaming:     req.Stream,
		LLMConfigID:   cfg.ID,
		LLMConfigName: cfg.Name,
		WorkspaceID:   tok.WorkspaceID,
		Endpoint:      "llm_proxy",
	}
	payload := aiChatCompletionRequest{
		Model:              cfg.Model,
		Messages:           req.Messages,
		MaxTokens:          req.MaxTokens,
		ChatTemplateKwargs: req.ChatTemplateKwargs,
	}
	if req.Stream {
		s.streamLLMProxyChatCompletion(c, tok, cfg, runtimeConfig, payload, req.Model, start)
		return
	}
	resp, err := s.aiAgent.requestChatCompletion(runtimeConfig, payload)
	if err != nil {
		// Upstream provider out of funds → surface as 402 not opaque 502,
		// so SDK clients see insufficient_quota (OpenAI-compat) and stop
		// retrying. errUpstreamOutOfCredit is set by classifyUpstreamCreditError
		// when the upstream returned 402 OR 403+credit-keyword.
		if errors.Is(err, errUpstreamOutOfCredit) {
			s.recordProxyCallFailure(tok, req.Model, cfg.Model, cfg.ID, http.StatusPaymentRequired, "upstream out of credit: "+err.Error(), start)
			s.metricsRegistry.recordLLMProxyCall(tok.Name, cfg.Model, "nonstream", http.StatusPaymentRequired, time.Since(start), 0, 0)
			openAIError(c, http.StatusPaymentRequired, "insufficient_quota", "upstream LLM provider account is out of credit — top up or switch model: "+err.Error())
			return
		}
		s.recordProxyCallFailure(tok, req.Model, cfg.Model, cfg.ID, http.StatusBadGateway, "upstream: "+err.Error(), start)
		s.metricsRegistry.recordLLMProxyCall(tok.Name, cfg.Model, "nonstream", http.StatusBadGateway, time.Since(start), 0, 0)
		openAIError(c, http.StatusBadGateway, "upstream_error", "upstream LLM error: "+err.Error())
		return
	}

	// 6) Audit + last-used.
	s.touchLLMProxyTokenLastUsed(tok.ID, time.Now().UTC())
	call := &LLMProxyCall{
		TokenID:        tok.ID,
		WorkspaceID:    tok.WorkspaceID,
		ModelRequested: req.Model,
		ModelResolved:  cfg.Model,
		LLMConfigID:    ptrInt64(cfg.ID),
		LatencyMS:      int(time.Since(start).Milliseconds()),
		StatusCode:     http.StatusOK,
		CreatedAt:      time.Now().UTC(),
	}
	if resp.Usage != nil {
		call.PromptTokens = resp.Usage.PromptTokens
		call.CompletionTokens = resp.Usage.CompletionTokens
		call.TotalTokens = resp.Usage.TotalTokens
		if call.TotalTokens == 0 {
			call.TotalTokens = call.PromptTokens + call.CompletionTokens
		}
	}
	if cost, ok := s.estimateLLMProxyCallCostWithConfig(cfg.ID, call.ModelResolved, call.PromptTokens, call.CompletionTokens); ok {
		call.CostUSD = &cost
	}
	_ = s.insertLLMProxyCall(call)
	s.metricsRegistry.recordLLMProxyCall(tok.Name, cfg.Model, "nonstream", http.StatusOK, time.Since(start), 0, call.CompletionTokens)

	// 7) Render OpenAI-compatible response envelope.
	c.JSON(http.StatusOK, gin.H{
		"id":      "chatcmpl-" + randHex(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   cfg.Model,
		"choices": resp.Choices,
		"usage":   resp.Usage, // may be nil for providers that don't report it
	})
}

func (s *Server) recordProxyCallFailure(tok *LLMProxyToken, modelRequested, modelResolved string, llmConfigID int64, status int, errText string, start time.Time) {
	if tok == nil {
		return
	}
	var cfgID *int64
	if llmConfigID > 0 {
		cfgID = ptrInt64(llmConfigID)
	}
	_ = s.insertLLMProxyCall(&LLMProxyCall{
		TokenID:        tok.ID,
		WorkspaceID:    tok.WorkspaceID,
		ModelRequested: modelRequested,
		ModelResolved:  modelResolved,
		LLMConfigID:    cfgID,
		LatencyMS:      int(time.Since(start).Milliseconds()),
		StatusCode:     status,
		ErrorText:      errText,
		CreatedAt:      time.Now().UTC(),
	})
	// NOTE: Prometheus metric is emitted by the CALLER (not here),
	// because only the caller knows the mode (stream vs nonstream).
	// Recording it here would mis-label stream failures as
	// nonstream.
}

func openAIError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

func ptrInt64(v int64) *int64 { return &v }

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// streamLLMProxyChatCompletion fans the upstream stream to the client
// as OpenAI-compatible SSE frames (object="chat.completion.chunk").
//
// Frame sequence emitted:
//  1. initial role frame: choices[0].delta = {"role":"assistant"}
//  2. one content frame per upstream delta (delta = {"content":"..."})
//  3. terminal finish frame: choices[0].finish_reason="stop"
//  4. optional usage frame (choices=[], usage=...) when the upstream
//     provider reported it
//  5. literal "data: [DONE]\n\n"
//
// Audit is written once after the stream completes (success or error).
func (s *Server) streamLLMProxyChatCompletion(c *gin.Context, tok *LLMProxyToken, cfg *LLMConfig, runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, modelRequested string, start time.Time) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	chatID := "chatcmpl-" + randHex(12)
	created := time.Now().Unix()
	modelResolved := cfg.Model

	emit := func(frame map[string]any) bool {
		blob, err := json.Marshal(frame)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", blob); err != nil {
			return false
		}
		c.Writer.Flush()
		return true
	}

	mkChunk := func(delta map[string]any, finishReason *string) map[string]any {
		choice := map[string]any{
			"index": 0,
			"delta": delta,
		}
		if finishReason != nil {
			choice["finish_reason"] = *finishReason
		} else {
			choice["finish_reason"] = nil
		}
		return map[string]any{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelResolved,
			"choices": []any{choice},
		}
	}

	// 1) role frame
	if !emit(mkChunk(map[string]any{"role": "assistant"}, nil)) {
		elapsed := time.Since(start)
		s.recordProxyCallFailure(tok, modelRequested, modelResolved, cfg.ID, http.StatusGone, "client disconnected before stream started", start)
		s.metricsRegistry.recordLLMProxyCall(tok.Name, modelResolved, "stream", http.StatusGone, elapsed, 0, 0)
		return
	}

	// Ask upstream to include usage on the terminal frame when it can.
	payload.StreamOptions = &aiStreamOptions{IncludeUsage: true}

	// TTFT capture — first non-empty content delta from upstream.
	var firstDeltaAt time.Time

	// 2..N) content deltas
	_, usage, err := s.aiAgent.requestStreamingChatCompletionWithUsage(
		c.Request.Context(),
		runtimeConfig,
		payload,
		func(delta string) {
			if delta == "" {
				return
			}
			if firstDeltaAt.IsZero() {
				firstDeltaAt = time.Now()
			}
			_ = emit(mkChunk(map[string]any{"content": delta}, nil))
		},
	)
	if err != nil {
		// Try to deliver an error frame; even if it fails the audit
		// row still records the upstream failure. Credit-exhaustion
		// gets a typed code so SDK clients can distinguish "retry
		// later" from "give up, top up account".
		errMessage := "upstream LLM error: " + err.Error()
		errType := "upstream_error"
		errCode := "upstream_error"
		recordedStatus := http.StatusBadGateway
		if errors.Is(err, errUpstreamOutOfCredit) {
			errMessage = "upstream LLM provider account is out of credit — top up or switch model: " + err.Error()
			errType = "insufficient_quota"
			errCode = "insufficient_quota"
			recordedStatus = http.StatusPaymentRequired
		}
		_ = emit(map[string]any{
			"error": map[string]any{
				"message": errMessage,
				"type":    errType,
				"code":    errCode,
			},
		})
		_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
		c.Writer.Flush()
		elapsed := time.Since(start)
		s.recordProxyCallFailure(tok, modelRequested, modelResolved, cfg.ID, recordedStatus, "upstream: "+err.Error(), start)
		s.metricsRegistry.recordLLMProxyCall(tok.Name, modelResolved, "stream", recordedStatus, elapsed, 0, 0)
		return
	}

	// 3) terminal finish frame
	stop := "stop"
	_ = emit(mkChunk(map[string]any{}, &stop))

	// 4) usage frame (only when upstream gave us one)
	if usage != nil {
		usageFrame := map[string]any{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelResolved,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     usage.PromptTokens,
				"completion_tokens": usage.CompletionTokens,
				"total_tokens":      usage.TotalTokens,
			},
		}
		_ = emit(usageFrame)
	}

	// 5) sentinel
	_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	c.Writer.Flush()

	// Audit + last-used.
	s.touchLLMProxyTokenLastUsed(tok.ID, time.Now().UTC())
	call := &LLMProxyCall{
		TokenID:        tok.ID,
		WorkspaceID:    tok.WorkspaceID,
		ModelRequested: modelRequested,
		ModelResolved:  modelResolved,
		LLMConfigID:    ptrInt64(cfg.ID),
		LatencyMS:      int(time.Since(start).Milliseconds()),
		StatusCode:     http.StatusOK,
		CreatedAt:      time.Now().UTC(),
	}
	if usage != nil {
		call.PromptTokens = usage.PromptTokens
		call.CompletionTokens = usage.CompletionTokens
		call.TotalTokens = usage.TotalTokens
		if call.TotalTokens == 0 {
			call.TotalTokens = call.PromptTokens + call.CompletionTokens
		}
	}
	if cost, ok := s.estimateLLMProxyCallCostWithConfig(cfg.ID, call.ModelResolved, call.PromptTokens, call.CompletionTokens); ok {
		call.CostUSD = &cost
	}
	_ = s.insertLLMProxyCall(call)
	var ttftSec float64
	if !firstDeltaAt.IsZero() {
		ttftSec = firstDeltaAt.Sub(start).Seconds()
	}
	s.metricsRegistry.recordLLMProxyCall(tok.Name, modelResolved, "stream", http.StatusOK, time.Since(start), ttftSec, call.CompletionTokens)
}
