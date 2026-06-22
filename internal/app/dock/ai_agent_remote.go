package dock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// P2a-2: delegate dock's in-app LLM dispatch to the polar-llm gateway.
//
// When POLAR_LLM_REMOTE is engaged (flag on + shared secret set), both
// requestChatCompletionInner and requestStreamingChatCompletionInner POST the
// already-resolved runtime config (base_url / api_key / model / proxy_url /
// system_prompt) plus the chat payload to polar-llm's
// /internal/v1/llm/complete. polar-llm runs the billing gate, dispatches
// upstream and writes the single cost-ledger row — so dock skips its own
// recordAgentLLMCall to avoid a double-write. Default OFF: dock dispatches
// locally exactly as before. See doc/arch/chat-llm-extraction.md.

// llmCompleteRequest is the inline-config body dock sends to polar-llm. The
// resolved config travels with the request so polar-llm needs no config
// lookup for the in-app path. Extra dispatch-affecting fields (max_tokens,
// tools, …) are forwarded so the gateway can dispatch identically; a gateway
// that ignores them simply degrades to plain chat.
type llmCompleteRequest struct {
	WorkspaceID  string `json:"workspace_id"`
	LLMConfigID  int64  `json:"llm_config_id"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKey       string `json:"api_key"`
	ProxyURL     string `json:"proxy_url,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	Requester    string `json:"requester,omitempty"`
	IsSystemBot  bool   `json:"is_system_bot,omitempty"`

	Messages           []aiChatCompletionMessage `json:"messages"`
	MaxTokens          int                       `json:"max_tokens,omitempty"`
	Stream             bool                      `json:"stream"`
	ChatTemplateKwargs map[string]any            `json:"chat_template_kwargs,omitempty"`
	Tools              []aiToolSpec              `json:"tools,omitempty"`
	ToolChoice         string                    `json:"tool_choice,omitempty"`
}

// llmRemoteEngaged reports whether in-app dispatch should be delegated to
// polar-llm. Both the flag and the shared secret must be present — an unset
// secret keeps dock dispatching locally even if the flag is on, so a
// half-configured deploy can never silently send keyless requests.
func (a *aiAgent) llmRemoteEngaged() bool {
	return a.server != nil && a.server.polarLLMRemote && a.server.polarLLMInternalSecret != ""
}

func (a *aiAgent) newLLMCompleteRequest(runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, stream bool) llmCompleteRequest {
	return llmCompleteRequest{
		WorkspaceID:        runtimeConfig.WorkspaceID,
		LLMConfigID:        runtimeConfig.LLMConfigID,
		BaseURL:            runtimeConfig.BaseURL,
		Model:              runtimeConfig.Model,
		APIKey:             runtimeConfig.APIKey,
		ProxyURL:           runtimeConfig.ProxyURL,
		SystemPrompt:       runtimeConfig.SystemPrompt,
		Requester:          runtimeConfig.Endpoint,
		IsSystemBot:        runtimeConfig.IsSystemBot,
		Messages:           payload.Messages,
		MaxTokens:          payload.MaxTokens,
		Stream:             stream,
		ChatTemplateKwargs: payload.ChatTemplateKwargs,
		Tools:              payload.Tools,
		ToolChoice:         payload.ToolChoice,
	}
}

// callPolarLLMComplete delegates a non-streaming completion to polar-llm and
// returns the OpenAI-shaped response verbatim.
func (a *aiAgent) callPolarLLMComplete(runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest) (*aiChatCompletionResponse, error) {
	body, err := json.Marshal(a.newLLMCompleteRequest(runtimeConfig, payload, false))
	if err != nil {
		return nil, err
	}
	url := a.server.polarLLMCompleteURL
	log.Printf("ai agent remote: url=%s model=%s workspace=%s cfg=%d", url, runtimeConfig.Model, runtimeConfig.WorkspaceID, runtimeConfig.LLMConfigID)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Polar-Internal-Secret", a.server.polarLLMInternalSecret)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent remote response: status=%d body=%s", resp.StatusCode, compactLogJSON(responseBody))

	var result aiChatCompletionResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		// Non-JSON gateway error (auth reject, panic page, …).
		if resp.StatusCode >= 400 {
			return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, fmt.Errorf("polar-llm returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody))))
		}
		return nil, err
	}
	if resp.StatusCode >= 400 {
		if message := parseAIErrorMessage(result.Error); message != "" {
			return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, errors.New(message))
		}
		return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, fmt.Errorf("polar-llm returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody))))
	}
	return &result, nil
}

// callPolarLLMCompleteStream delegates a streaming completion to polar-llm and
// relays each SSE delta back through onDelta, mirroring streamOpenAICompatible.
// polar-llm emits OpenAI chat.completion.chunk frames terminated by [DONE],
// with a final usage frame when available.
func (a *aiAgent) callPolarLLMCompleteStream(ctx context.Context, runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, onDelta streamChunkCallback) (string, *aiUsage, error) {
	body, err := json.Marshal(a.newLLMCompleteRequest(runtimeConfig, payload, true))
	if err != nil {
		return "", nil, err
	}
	// proxyURL is empty: dock → polar-llm is a loopback hop, the upstream
	// proxy (if any) is applied gateway-side from proxy_url in the body.
	session, err := a.startStreamingRequest(ctx, "openai", a.server.polarLLMCompleteURL, "application/json", body, map[string]string{
		"X-Polar-Internal-Secret": a.server.polarLLMInternalSecret,
	}, "")
	if err != nil {
		return "", nil, err
	}
	defer session.close()

	var (
		assembled strings.Builder
		seenFirst bool
		gotDone   bool
		usage     *aiUsage
	)
	for {
		select {
		case <-ctx.Done():
			return assembled.String(), usage, ctx.Err()
		case err := <-session.errs:
			if err != nil {
				return assembled.String(), usage, err
			}
		case frame, ok := <-session.frames:
			if !ok {
				if !gotDone {
					return assembled.String(), usage, errors.New("polar-llm stream ended without [DONE]")
				}
				return assembled.String(), usage, nil
			}
			if frame.data == "" {
				continue
			}
			if frame.data == "[DONE]" {
				gotDone = true
				return assembled.String(), usage, nil
			}
			var chunk openAIStreamChunk
			if err := json.Unmarshal([]byte(frame.data), &chunk); err != nil {
				log.Printf("ai agent remote stream parse chunk failed: %v body=%s", err, truncateForLog(frame.data, 200))
				continue
			}
			if message := parseAIErrorMessage(chunk.Error); message != "" {
				return assembled.String(), usage, errors.New(message)
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			for _, choice := range chunk.Choices {
				if choice.Delta.Content == nil {
					continue
				}
				delta := *choice.Delta.Content
				if delta == "" {
					continue
				}
				assembled.WriteString(delta)
				if !seenFirst {
					seenFirst = true
					log.Printf("ai agent remote stream first chunk: bytes=%d", len(delta))
				}
				if onDelta != nil {
					onDelta(delta)
				}
			}
		}
	}
}
