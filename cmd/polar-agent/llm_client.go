package main

// Minimal OpenAI-compatible chat completions client used by the
// agent's research runner. Same wire shape as dock-side
// internal/app/dock/ai_agent.go, kept independent so the agent
// binary doesn't import the dock package.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type llmChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []llmToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type llmToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function llmToolCallFunc `json:"function"`
}

type llmToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type llmToolSpec struct {
	Type     string          `json:"type"`
	Function llmToolFunction `json:"function"`
}

type llmToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type llmChatRequest struct {
	Model      string           `json:"model"`
	Messages   []llmChatMessage `json:"messages"`
	Tools      []llmToolSpec    `json:"tools,omitempty"`
	ToolChoice string           `json:"tool_choice,omitempty"`
	MaxTokens  int              `json:"max_tokens,omitempty"`
}

type llmChatResponse struct {
	Choices []struct {
		Message      llmChatMessage `json:"message"`
		FinishReason string         `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

type llmClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newLLMClient(baseURL, apiKey string) *llmClient {
	return &llmClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		http:    &http.Client{Timeout: 180 * time.Second},
	}
}

// chatCompletions hits POST <baseURL>/chat/completions. baseURL
// follows the OpenAI convention — callers supply the prefix that
// already ends just before "/chat/completions" (matches the dock-
// side LLMConfig.BaseURL contract).
func (c *llmClient) chatCompletions(ctx context.Context, req llmChatRequest) (*llmChatResponse, error) {
	if c.baseURL == "" || c.apiKey == "" {
		return nil, errors.New("llm client missing base_url or api_key")
	}
	endpoint, err := url.JoinPath(c.baseURL, "chat", "completions")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("llm http %d: %s", resp.StatusCode, truncateForErr(string(raw)))
	}
	var parsed llmChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("llm decode: %w (body: %s)", err, truncateForErr(string(raw)))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("llm error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("llm response has no choices")
	}
	return &parsed, nil
}

func truncateForErr(s string) string {
	if len(s) > 512 {
		return s[:512] + "...[truncated]"
	}
	return s
}
