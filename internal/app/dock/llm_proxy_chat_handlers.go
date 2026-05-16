package dock

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
		Model     string                    `json:"model"`
		Messages  []aiChatCompletionMessage `json:"messages" binding:"required"`
		MaxTokens int                       `json:"max_tokens,omitempty"`
		Stream    bool                      `json:"stream,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "bad json body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "messages must be non-empty")
		return
	}
	// 3) Rate limit gate. Uses the token's stored rate_limit_rpm; 0
	// means unlimited. On reject we still write an audit row with
	// status=429 so the operator can see who's hammering the proxy.
	if s.llmProxyRateLimiter != nil {
		if ok, retryAfter := s.llmProxyRateLimiter.allow(tok.ID, tok.RateLimitRPM); !ok {
			s.recordProxyCallFailure(tok, req.Model, "", 0, http.StatusTooManyRequests, "rate limit exceeded", start)
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			openAIError(c, http.StatusTooManyRequests, "rate_limit_exceeded", "token rate limit exceeded; retry in "+strconv.Itoa(retryAfter)+"s")
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
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "no alias route matched model="+req.Model+" and token has no default LLM configured")
		return
	}
	cfg, apiKey, err := s.getLLMConfigForOwner(tok.WorkspaceID, resolvedLLMConfigID)
	if err != nil {
		s.recordProxyCallFailure(tok, req.Model, "", resolvedLLMConfigID, http.StatusInternalServerError, "llm config lookup failed", start)
		openAIError(c, http.StatusInternalServerError, "internal_error", "upstream config lookup failed")
		return
	}
	if cfg == nil {
		s.recordProxyCallFailure(tok, req.Model, "", resolvedLLMConfigID, http.StatusBadRequest, "llm config missing", start)
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "configured upstream LLM is missing or out of scope")
		return
	}
	if strings.TrimSpace(apiKey) == "" {
		s.recordProxyCallFailure(tok, req.Model, cfg.Model, cfg.ID, http.StatusBadGateway, "upstream missing api key", start)
		openAIError(c, http.StatusBadGateway, "upstream_error", "upstream LLM is not configured (missing api key)")
		return
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
		Endpoint:      "llm_proxy",
	}
	payload := aiChatCompletionRequest{
		Model:     cfg.Model,
		Messages:  req.Messages,
		MaxTokens: req.MaxTokens,
	}
	if req.Stream {
		s.streamLLMProxyChatCompletion(c, tok, cfg, runtimeConfig, payload, req.Model, start)
		return
	}
	resp, err := s.aiAgent.requestChatCompletion(runtimeConfig, payload)
	if err != nil {
		s.recordProxyCallFailure(tok, req.Model, cfg.Model, cfg.ID, http.StatusBadGateway, "upstream: "+err.Error(), start)
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
	if cost, ok := estimateLLMProxyCallCost(call.ModelResolved, call.PromptTokens, call.CompletionTokens); ok {
		call.CostUSD = &cost
	}
	_ = s.insertLLMProxyCall(call)

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
		s.recordProxyCallFailure(tok, modelRequested, modelResolved, cfg.ID, http.StatusGone, "client disconnected before stream started", start)
		return
	}

	// Ask upstream to include usage on the terminal frame when it can.
	payload.StreamOptions = &aiStreamOptions{IncludeUsage: true}

	// 2..N) content deltas
	_, usage, err := s.aiAgent.requestStreamingChatCompletionWithUsage(
		c.Request.Context(),
		runtimeConfig,
		payload,
		func(delta string) {
			if delta == "" {
				return
			}
			_ = emit(mkChunk(map[string]any{"content": delta}, nil))
		},
	)
	if err != nil {
		// Try to deliver an error frame; even if it fails the audit
		// row still records the upstream failure.
		_ = emit(map[string]any{
			"error": map[string]any{
				"message": "upstream LLM error: " + err.Error(),
				"type":    "upstream_error",
				"code":    "upstream_error",
			},
		})
		_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
		c.Writer.Flush()
		s.recordProxyCallFailure(tok, modelRequested, modelResolved, cfg.ID, http.StatusBadGateway, "upstream: "+err.Error(), start)
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
	if cost, ok := estimateLLMProxyCallCost(call.ModelResolved, call.PromptTokens, call.CompletionTokens); ok {
		call.CostUSD = &cost
	}
	_ = s.insertLLMProxyCall(call)
}
