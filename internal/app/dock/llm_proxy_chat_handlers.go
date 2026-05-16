package dock

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
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
	if req.Stream {
		// P0 doesn't support streaming. Returning 400 (not 501) so OpenAI
		// SDKs that auto-retry on 5xx don't hammer us.
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "stream=true not yet supported by this proxy")
		return
	}

	// 3) Resolve upstream LLM config.
	if tok.DefaultLLMConfigID == nil || *tok.DefaultLLMConfigID <= 0 {
		s.recordProxyCallFailure(tok, req.Model, "", 0, http.StatusBadRequest, "token has no default_llm_config_id", start)
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "token has no default LLM configured")
		return
	}
	cfg, apiKey, err := s.getLLMConfigForOwner(tok.WorkspaceID, *tok.DefaultLLMConfigID)
	if err != nil {
		s.recordProxyCallFailure(tok, req.Model, "", *tok.DefaultLLMConfigID, http.StatusInternalServerError, "llm config lookup failed", start)
		openAIError(c, http.StatusInternalServerError, "internal_error", "upstream config lookup failed")
		return
	}
	if cfg == nil {
		s.recordProxyCallFailure(tok, req.Model, "", *tok.DefaultLLMConfigID, http.StatusBadRequest, "llm config missing", start)
		openAIError(c, http.StatusBadRequest, "invalid_request_error", "configured upstream LLM is missing or out of scope")
		return
	}
	if strings.TrimSpace(apiKey) == "" {
		s.recordProxyCallFailure(tok, req.Model, cfg.Model, cfg.ID, http.StatusBadGateway, "upstream missing api key", start)
		openAIError(c, http.StatusBadGateway, "upstream_error", "upstream LLM is not configured (missing api key)")
		return
	}

	// 4) Dispatch.
	runtimeConfig := aiRuntimeConfig{
		APIKey:        apiKey,
		BaseURL:       cfg.BaseURL,
		Model:         cfg.Model,
		SystemPrompt:  cfg.SystemPrompt,
		ProxyURL:      cfg.ProxyURL,
		Streaming:     false,
		LLMConfigID:   cfg.ID,
		LLMConfigName: cfg.Name,
		Endpoint:      "llm_proxy",
	}
	payload := aiChatCompletionRequest{
		Model:     cfg.Model,
		Messages:  req.Messages,
		MaxTokens: req.MaxTokens,
	}
	resp, err := s.aiAgent.requestChatCompletion(runtimeConfig, payload)
	if err != nil {
		s.recordProxyCallFailure(tok, req.Model, cfg.Model, cfg.ID, http.StatusBadGateway, "upstream: "+err.Error(), start)
		openAIError(c, http.StatusBadGateway, "upstream_error", "upstream LLM error: "+err.Error())
		return
	}

	// 5) Audit + last-used.
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
	_ = s.insertLLMProxyCall(call)

	// 6) Render OpenAI-compatible response envelope.
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
