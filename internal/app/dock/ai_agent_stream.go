package dock

// LLM streaming layer.
//
// This file holds the SSE-based streaming variant of `requestChatCompletion`
// and per-protocol parsers for the four supported providers
// (OpenAI-compatible, Anthropic Messages, Google Gemini, xAI Responses).
//
// Design notes:
//   - One shared frame reader (`readSSEFrames`) splits the response body into
//     SSE event blocks separated by blank lines. Each frame is a slice of raw
//     lines with prefixes intact ("data: ...", "event: ..."). Per-protocol
//     decoders pull the bits they need.
//   - The caller owns `context.Context` cancellation. The handleTask wrapper
//     installs an idle-gap watchdog (`time.AfterFunc`) that cancels the
//     context if no chunk arrives within `streamIdleTimeout`.
//   - We never treat HTTP 200 as success until the protocol terminator fires.
//     Mid-stream `error` events return as Go errors.
//   - Logging discipline: log on stream open, on first chunk, on terminator,
//     on error. Do not log every chunk (long replies fill disks).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// streamIdleTimeout aborts the read loop when no chunk arrives for this long.
	// Generous enough to tolerate cold-start of slow self-hosted models, tight
	// enough that a stalled server is recovered from in human time.
	streamIdleTimeout = 90 * time.Second
)

// streamChunkCallback receives each non-empty text delta as it is decoded.
type streamChunkCallback func(delta string)

// requestStreamingChatCompletion dispatches a streaming chat completion to
// the right provider based on the BaseURL and returns the assembled string
// (also delivered piecewise via onDelta). Honors ctx cancellation.
//
// Records timing + status on every call. Tokens are recorded when the
// provider's stream emits a usage frame — today that's just the OpenAI-
// compat path, which sets stream_options.include_usage=true on the
// outgoing request and parses the final frame's `usage` block. Other
// SSE protocols (Anthropic / Gemini / XAI) leave usage nil.
func (a *aiAgent) requestStreamingChatCompletion(ctx context.Context, runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, onDelta streamChunkCallback) (string, error) {
	payload.Stream = true
	provider := detectAIProvider(runtimeConfig.BaseURL)
	start := time.Now()
	out, usage, err := a.requestStreamingChatCompletionInner(ctx, runtimeConfig, payload, onDelta)
	prompt, completion := 0, 0
	if usage != nil {
		prompt = usage.PromptTokens
		completion = usage.CompletionTokens
	}
	if a.server != nil {
		a.server.metricsRegistry.recordLLMCall(string(provider), runtimeConfig.Model, runtimeConfig.Endpoint, time.Since(start), err, prompt, completion)
	}
	return out, err
}

func (a *aiAgent) requestStreamingChatCompletionInner(ctx context.Context, runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, onDelta streamChunkCallback) (string, *aiUsage, error) {
	switch detectAIProvider(runtimeConfig.BaseURL) {
	case aiProviderAnthropic:
		out, err := a.streamAnthropic(ctx, runtimeConfig, payload, onDelta)
		return out, nil, err
	case aiProviderGemini:
		out, err := a.streamGemini(ctx, runtimeConfig, payload, onDelta)
		return out, nil, err
	case aiProviderXAIResponses:
		out, err := a.streamXAI(ctx, runtimeConfig, payload, onDelta)
		return out, nil, err
	case aiProviderVolcesResponses:
		out, err := a.streamVolces(ctx, runtimeConfig, payload, onDelta)
		return out, nil, err
	case aiProviderMiroMind:
		return a.streamMiroMind(ctx, runtimeConfig, payload, onDelta)
	default:
		return a.streamOpenAICompatible(ctx, runtimeConfig, payload, onDelta)
	}
}

// streamFrame is the parsed lines of one SSE event block.
type streamFrame struct {
	event string
	data  string
}

// readSSEFrames pulls SSE event blocks (delimited by a blank line) off r.
// SSE comment lines (starting with ':') are skipped. The watchdog timer is
// reset whenever a non-empty frame is delivered.
func readSSEFrames(ctx context.Context, body io.Reader, watchdog *time.Timer) (<-chan streamFrame, <-chan error) {
	frames := make(chan streamFrame)
	errs := make(chan error, 1)
	go func() {
		defer close(frames)
		defer close(errs)
		reader := bufio.NewReaderSize(body, 64*1024)
		var (
			eventName string
			dataBuf   strings.Builder
		)
		flush := func() {
			if dataBuf.Len() == 0 && eventName == "" {
				return
			}
			frame := streamFrame{event: eventName, data: dataBuf.String()}
			eventName = ""
			dataBuf.Reset()
			if watchdog != nil {
				watchdog.Reset(streamIdleTimeout)
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
			}
		}
		for {
			if ctx.Err() != nil {
				errs <- ctx.Err()
				return
			}
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				trimmed := strings.TrimRight(string(line), "\r\n")
				switch {
				case trimmed == "":
					flush()
				case strings.HasPrefix(trimmed, ":"):
					// SSE comment (keep-alive). Reset watchdog so a stream
					// that emits only pings doesn't trigger the idle timeout.
					if watchdog != nil {
						watchdog.Reset(streamIdleTimeout)
					}
				case strings.HasPrefix(trimmed, "event:"):
					eventName = strings.TrimSpace(trimmed[len("event:"):])
				case strings.HasPrefix(trimmed, "data:"):
					payload := trimmed[len("data:"):]
					if strings.HasPrefix(payload, " ") {
						payload = payload[1:]
					}
					if dataBuf.Len() > 0 {
						dataBuf.WriteByte('\n')
					}
					dataBuf.WriteString(payload)
				default:
					// Ignore unknown SSE field (id:, retry:, etc.).
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					errs <- err
				}
				flush()
				return
			}
		}
	}()
	return frames, errs
}

// startStreamingRequest runs the HTTP request and wires up the watchdog
// + frame reader. Caller drains frames+errs and decides when to return.
type streamSession struct {
	resp     *http.Response
	frames   <-chan streamFrame
	errs     <-chan error
	cancel   context.CancelFunc
	watchdog *time.Timer
}

func (a *aiAgent) startStreamingRequest(ctx context.Context, providerLabel, reqURL, contentType string, body []byte, headers map[string]string, proxyURL string) (*streamSession, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	watchdog := time.AfterFunc(streamIdleTimeout, cancel)

	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		watchdog.Stop()
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	log.Printf("ai agent stream open: provider=%s url=%s proxy=%q body=%s", providerLabel, reqURL, proxyURL, compactLogJSON(body))

	resp, err := a.streamClientFor(proxyURL).Do(req)
	if err != nil {
		watchdog.Stop()
		cancel()
		return nil, err
	}
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		watchdog.Stop()
		cancel()
		message := strings.TrimSpace(string(errBody))
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("%s api returned status %d: %s", providerLabel, resp.StatusCode, message)
	}

	frames, errs := readSSEFrames(streamCtx, resp.Body, watchdog)
	return &streamSession{
		resp:     resp,
		frames:   frames,
		errs:     errs,
		cancel:   cancel,
		watchdog: watchdog,
	}, nil
}

func (s *streamSession) close() {
	if s == nil {
		return
	}
	if s.watchdog != nil {
		s.watchdog.Stop()
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.resp != nil && s.resp.Body != nil {
		_ = s.resp.Body.Close()
	}
}

// ---- OpenAI-compatible -----------------------------------------------------

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content *string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	// Usage is non-nil only on the final frame, and only when the
	// request was sent with stream_options.include_usage=true.
	// Choices is empty in that frame; clients should accept either
	// shape per OpenAI's spec.
	Usage *aiUsage        `json:"usage,omitempty"`
	Error json.RawMessage `json:"error,omitempty"`
}

func (a *aiAgent) streamOpenAICompatible(ctx context.Context, runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, onDelta streamChunkCallback) (string, *aiUsage, error) {
	// Ask the upstream to emit a final `usage` frame so we can record
	// streaming token counts. Setting it unconditionally is safe for
	// OpenAI-compat providers (Kimi, DeepSeek, Volces non-Responses,
	// OpenAI itself); ones that don't support the field tend to ignore
	// unknown JSON keys, so the worst case is just no usage frame —
	// callers already handle nil usage.
	if payload.StreamOptions == nil {
		payload.StreamOptions = &aiStreamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	session, err := a.startStreamingRequest(ctx, "openai", runtimeConfig.BaseURL, "application/json", body, map[string]string{
		"Authorization": "Bearer " + runtimeConfig.APIKey,
	}, runtimeConfig.ProxyURL)
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
					return assembled.String(), usage, errors.New("openai stream ended without [DONE]")
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
				log.Printf("ai agent stream parse openai chunk failed: %v body=%s", err, truncateForLog(frame.data, 200))
				continue
			}
			if message := parseAIErrorMessage(chunk.Error); message != "" {
				return assembled.String(), usage, errors.New(message)
			}
			// Capture usage on whatever frame carries it (typically the
			// last one with empty Choices). Last writer wins if multiple
			// frames somehow set it.
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
					log.Printf("ai agent stream first chunk: provider=openai bytes=%d", len(delta))
				}
				if onDelta != nil {
					onDelta(delta)
				}
			}
		}
	}
}

// ---- MiroMind / MiroThinker -----------------------------------------------
//
// MiroMind speaks OpenAI Chat Completions on the wire (Bearer auth, /v1/chat/
// completions, SSE frames carrying chat.completion.chunk shapes). The wrinkle
// is that deepresearch models stream their thinking trace as
//
//	"delta": { "reasoning_steps": [ {"type":"thinking","thought":"…"} ] }
//
// instead of (or before) delta.content. We surface that trace to the user by
// wrapping the concatenated thoughts in <think>…</think> tags — the same
// convention DeepSeek-R1 / QwQ use, which downstream renderers already know
// how to collapse or strip. Final answer text (delta.content) is emitted
// after the closing </think> tag.

type miroMindStreamChunk struct {
	Choices []struct {
		Delta struct {
			Role           string                  `json:"role,omitempty"`
			Content        *string                 `json:"content,omitempty"`
			ReasoningSteps []miroMindReasoningStep `json:"reasoning_steps,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *aiUsage        `json:"usage,omitempty"`
	Error json.RawMessage `json:"error,omitempty"`
}

type miroMindReasoningStep struct {
	Type    string `json:"type"`
	Thought string `json:"thought"`
}

func (a *aiAgent) streamMiroMind(ctx context.Context, runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, onDelta streamChunkCallback) (string, *aiUsage, error) {
	if payload.StreamOptions == nil {
		payload.StreamOptions = &aiStreamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	session, err := a.startStreamingRequest(ctx, "miromind", runtimeConfig.BaseURL, "application/json", body, map[string]string{
		"Authorization": "Bearer " + runtimeConfig.APIKey,
	}, runtimeConfig.ProxyURL)
	if err != nil {
		return "", nil, err
	}
	defer session.close()

	emit := func(s string, assembled *strings.Builder) {
		if s == "" {
			return
		}
		assembled.WriteString(s)
		if onDelta != nil {
			onDelta(s)
		}
	}

	var (
		assembled  strings.Builder
		inThinking bool
		seenFirst  bool
		gotDone    bool
		usage      *aiUsage
	)
	for {
		select {
		case <-ctx.Done():
			if inThinking {
				emit("\n</think>\n\n", &assembled)
			}
			return assembled.String(), usage, ctx.Err()
		case err := <-session.errs:
			if err != nil {
				if inThinking {
					emit("\n</think>\n\n", &assembled)
				}
				return assembled.String(), usage, err
			}
		case frame, ok := <-session.frames:
			if !ok {
				if inThinking {
					emit("\n</think>\n\n", &assembled)
				}
				if !gotDone {
					return assembled.String(), usage, errors.New("miromind stream ended without [DONE]")
				}
				return assembled.String(), usage, nil
			}
			if frame.data == "" {
				continue
			}
			if frame.data == "[DONE]" {
				if inThinking {
					emit("\n</think>\n\n", &assembled)
					inThinking = false
				}
				gotDone = true
				return assembled.String(), usage, nil
			}
			var chunk miroMindStreamChunk
			if err := json.Unmarshal([]byte(frame.data), &chunk); err != nil {
				log.Printf("ai agent stream parse miromind chunk failed: %v body=%s", err, truncateForLog(frame.data, 200))
				continue
			}
			if message := parseAIErrorMessage(chunk.Error); message != "" {
				if inThinking {
					emit("\n</think>\n\n", &assembled)
				}
				return assembled.String(), usage, errors.New(message)
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			for _, choice := range chunk.Choices {
				// Reasoning trace first — open <think> on the first
				// thought we see, accumulate subsequent thoughts inside.
				for _, step := range choice.Delta.ReasoningSteps {
					if step.Type != "" && step.Type != "thinking" {
						continue
					}
					if step.Thought == "" {
						continue
					}
					if !inThinking {
						emit("<think>\n", &assembled)
						inThinking = true
					}
					emit(step.Thought, &assembled)
					if !seenFirst {
						seenFirst = true
						log.Printf("ai agent stream first chunk: provider=miromind phase=thinking bytes=%d", len(step.Thought))
					}
				}
				// Final content. Close the <think> block once on
				// transition, then stream content as plain deltas.
				if choice.Delta.Content != nil && *choice.Delta.Content != "" {
					if inThinking {
						emit("\n</think>\n\n", &assembled)
						inThinking = false
					}
					delta := *choice.Delta.Content
					emit(delta, &assembled)
					if !seenFirst {
						seenFirst = true
						log.Printf("ai agent stream first chunk: provider=miromind phase=content bytes=%d", len(delta))
					}
				}
			}
		}
	}
}

// ---- Anthropic Messages ---------------------------------------------------

type anthropicStreamDelta struct {
	Type   string                 `json:"type"`
	Delta  map[string]any         `json:"delta"`
	Index  int                    `json:"index"`
	Error  *anthropicErrorPayload `json:"error,omitempty"`
}

type anthropicErrorPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (a *aiAgent) streamAnthropic(ctx context.Context, runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, onDelta streamChunkCallback) (string, error) {
	reqURL, err := buildAnthropicMessagesRequestURL(runtimeConfig.BaseURL)
	if err != nil {
		return "", err
	}
	bodyPayload := convertToAnthropicMessagesRequest(payload)
	bodyPayloadStreaming := struct {
		anthropicMessagesRequest
		Stream bool `json:"stream"`
	}{anthropicMessagesRequest: bodyPayload, Stream: true}
	body, err := json.Marshal(bodyPayloadStreaming)
	if err != nil {
		return "", err
	}
	session, err := a.startStreamingRequest(ctx, "anthropic", reqURL, "application/json", body, map[string]string{
		"x-api-key":         runtimeConfig.APIKey,
		"anthropic-version": "2023-06-01",
	}, runtimeConfig.ProxyURL)
	if err != nil {
		return "", err
	}
	defer session.close()

	var (
		assembled strings.Builder
		seenFirst bool
	)
	for {
		select {
		case <-ctx.Done():
			return assembled.String(), ctx.Err()
		case err := <-session.errs:
			if err != nil {
				return assembled.String(), err
			}
		case frame, ok := <-session.frames:
			if !ok {
				return assembled.String(), nil
			}
			switch frame.event {
			case "ping", "":
				continue
			case "error":
				return assembled.String(), errors.New("anthropic stream error: " + frame.data)
			case "message_stop":
				return assembled.String(), nil
			case "content_block_delta":
				var chunk anthropicStreamDelta
				if err := json.Unmarshal([]byte(frame.data), &chunk); err != nil {
					log.Printf("ai agent stream parse anthropic delta failed: %v body=%s", err, truncateForLog(frame.data, 200))
					continue
				}
				deltaType, _ := chunk.Delta["type"].(string)
				if deltaType != "text_delta" {
					// Ignore tool-use / json deltas — out of scope for plain
					// text streaming.
					continue
				}
				text, _ := chunk.Delta["text"].(string)
				if text == "" {
					continue
				}
				assembled.WriteString(text)
				if !seenFirst {
					seenFirst = true
					log.Printf("ai agent stream first chunk: provider=anthropic bytes=%d", len(text))
				}
				if onDelta != nil {
					onDelta(text)
				}
			default:
				// content_block_start, content_block_stop, message_start,
				// message_delta — no text payload, no-op.
			}
		}
	}
}

// ---- Google Gemini ---------------------------------------------------------

func (a *aiAgent) streamGemini(ctx context.Context, runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, onDelta streamChunkCallback) (string, error) {
	reqURL, err := buildGeminiStreamRequestURL(runtimeConfig.BaseURL, runtimeConfig.Model, runtimeConfig.APIKey)
	if err != nil {
		return "", err
	}
	bodyPayload := convertToGeminiRequest(payload)
	body, err := json.Marshal(bodyPayload)
	if err != nil {
		return "", err
	}
	session, err := a.startStreamingRequest(ctx, "gemini", reqURL, "application/json", body, nil, runtimeConfig.ProxyURL)
	if err != nil {
		return "", err
	}
	defer session.close()

	var (
		assembled strings.Builder
		seenFirst bool
	)
	for {
		select {
		case <-ctx.Done():
			return assembled.String(), ctx.Err()
		case err := <-session.errs:
			if err != nil {
				return assembled.String(), err
			}
		case frame, ok := <-session.frames:
			if !ok {
				return assembled.String(), nil
			}
			if frame.data == "" {
				continue
			}
			var chunk geminiGenerateContentResponse
			if err := json.Unmarshal([]byte(frame.data), &chunk); err != nil {
				log.Printf("ai agent stream parse gemini chunk failed: %v body=%s", err, truncateForLog(frame.data, 200))
				continue
			}
			if message := parseAIErrorMessage(chunk.Error); message != "" {
				return assembled.String(), errors.New(message)
			}
			text := extractGeminiResponseText(chunk)
			if text == "" {
				continue
			}
			assembled.WriteString(text)
			if !seenFirst {
				seenFirst = true
				log.Printf("ai agent stream first chunk: provider=gemini bytes=%d", len(text))
			}
			if onDelta != nil {
				onDelta(text)
			}
		}
	}
}

// buildGeminiStreamRequestURL switches a generateContent URL to its
// streamGenerateContent counterpart and ensures `alt=sse` is set so the
// server emits SSE rather than a JSON array.
func buildGeminiStreamRequestURL(baseURL, model, apiKey string) (string, error) {
	resolved, err := buildGeminiRequestURL(baseURL, model, apiKey)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(resolved)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.Replace(parsed.Path, ":generateContent", ":streamGenerateContent", 1)
	if !strings.Contains(parsed.Path, ":streamGenerateContent") {
		parsed.Path += ":streamGenerateContent"
	}
	query := parsed.Query()
	query.Set("alt", "sse")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// ---- xAI Responses --------------------------------------------------------

type xAIStreamEvent struct {
	Type     string                 `json:"type"`
	Delta    string                 `json:"delta,omitempty"`
	Response map[string]any         `json:"response,omitempty"`
	Error    *anthropicErrorPayload `json:"error,omitempty"`
}

func (a *aiAgent) streamXAI(ctx context.Context, runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, onDelta streamChunkCallback) (string, error) {
	reqURL, err := buildXAIResponsesRequestURL(runtimeConfig.BaseURL)
	if err != nil {
		return "", err
	}
	bodyPayload := convertToXAIResponsesRequest(payload)
	bodyPayloadStreaming := struct {
		xAIResponsesRequest
		Stream bool `json:"stream"`
	}{xAIResponsesRequest: bodyPayload, Stream: true}
	body, err := json.Marshal(bodyPayloadStreaming)
	if err != nil {
		return "", err
	}
	session, err := a.startStreamingRequest(ctx, "xai", reqURL, "application/json", body, map[string]string{
		"Authorization": "Bearer " + runtimeConfig.APIKey,
	}, runtimeConfig.ProxyURL)
	if err != nil {
		return "", err
	}
	defer session.close()

	var (
		assembled strings.Builder
		seenFirst bool
	)
	for {
		select {
		case <-ctx.Done():
			return assembled.String(), ctx.Err()
		case err := <-session.errs:
			if err != nil {
				return assembled.String(), err
			}
		case frame, ok := <-session.frames:
			if !ok {
				return assembled.String(), nil
			}
			if frame.data == "" {
				continue
			}
			var chunk xAIStreamEvent
			if err := json.Unmarshal([]byte(frame.data), &chunk); err != nil {
				log.Printf("ai agent stream parse xai chunk failed: %v body=%s", err, truncateForLog(frame.data, 200))
				continue
			}
			switch chunk.Type {
			case "response.error":
				if chunk.Error != nil && chunk.Error.Message != "" {
					return assembled.String(), errors.New(chunk.Error.Message)
				}
				return assembled.String(), errors.New("xai stream returned response.error")
			case "response.completed":
				return assembled.String(), nil
			case "response.output_text.delta":
				if chunk.Delta == "" {
					continue
				}
				assembled.WriteString(chunk.Delta)
				if !seenFirst {
					seenFirst = true
					log.Printf("ai agent stream first chunk: provider=xai bytes=%d", len(chunk.Delta))
				}
				if onDelta != nil {
					onDelta(chunk.Delta)
				}
			default:
				// response.created / output_item.added / output_text.done /
				// content_part.* / content_part.done — no-op.
			}
		}
	}
}

// ---- Volces Responses -----------------------------------------------------
//
// Volces' Responses API (`/api/v3/responses` on ark.cn-beijing.volces.com)
// follows OpenAI's Responses SSE shape — same event types as xAI
// (`response.output_text.delta`, `response.completed`, `response.error`).
// Two differences from streamXAI:
//   - No web_search tool in the payload (Volces 400s on it; see
//     convertToVolcesResponsesRequest).
//   - BaseURL is used verbatim — Volces' URL is already the full
//     `/responses` path, no rewriting like buildXAIResponsesRequestURL.
func (a *aiAgent) streamVolces(ctx context.Context, runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest, onDelta streamChunkCallback) (string, error) {
	reqURL := strings.TrimSpace(runtimeConfig.BaseURL)
	if reqURL == "" {
		return "", errors.New("Volces 配置缺少 Base URL")
	}
	bodyPayload := convertToVolcesResponsesRequest(payload)
	bodyPayloadStreaming := struct {
		xAIResponsesRequest
		Stream bool `json:"stream"`
	}{xAIResponsesRequest: bodyPayload, Stream: true}
	body, err := json.Marshal(bodyPayloadStreaming)
	if err != nil {
		return "", err
	}
	session, err := a.startStreamingRequest(ctx, "volces", reqURL, "application/json", body, map[string]string{
		"Authorization": "Bearer " + runtimeConfig.APIKey,
	}, runtimeConfig.ProxyURL)
	if err != nil {
		return "", err
	}
	defer session.close()

	var (
		assembled strings.Builder
		seenFirst bool
	)
	for {
		select {
		case <-ctx.Done():
			return assembled.String(), ctx.Err()
		case err := <-session.errs:
			if err != nil {
				return assembled.String(), err
			}
		case frame, ok := <-session.frames:
			if !ok {
				return assembled.String(), nil
			}
			if frame.data == "" {
				continue
			}
			var chunk xAIStreamEvent
			if err := json.Unmarshal([]byte(frame.data), &chunk); err != nil {
				log.Printf("ai agent stream parse volces chunk failed: %v body=%s", err, truncateForLog(frame.data, 200))
				continue
			}
			switch chunk.Type {
			case "response.error":
				if chunk.Error != nil && chunk.Error.Message != "" {
					return assembled.String(), errors.New(chunk.Error.Message)
				}
				return assembled.String(), errors.New("volces stream returned response.error")
			case "response.completed":
				return assembled.String(), nil
			case "response.output_text.delta":
				if chunk.Delta == "" {
					continue
				}
				assembled.WriteString(chunk.Delta)
				if !seenFirst {
					seenFirst = true
					log.Printf("ai agent stream first chunk: provider=volces bytes=%d", len(chunk.Delta))
				}
				if onDelta != nil {
					onDelta(chunk.Delta)
				}
			default:
				// response.created / output_item.added / output_text.done /
				// content_part.* / content_part.done — no-op.
			}
		}
	}
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
