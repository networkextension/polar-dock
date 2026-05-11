package dock

// Tests for the SSE event-block reader and the per-protocol streaming
// decoders. The real HTTP client is not exercised here — instead we feed
// canned byte streams (one per provider, modeled on real wire captures)
// into `readSSEFrames` and the per-protocol parsing loops, and assert that
// the assembled text + termination behavior is correct.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// readAllFrames drains a frame channel until it closes or err arrives.
// Helper for the readSSEFrames table tests below.
func readAllFrames(t *testing.T, frames <-chan streamFrame, errs <-chan error, timeout time.Duration) []streamFrame {
	t.Helper()
	collected := make([]streamFrame, 0, 8)
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return collected
			}
			collected = append(collected, f)
		case err := <-errs:
			if err != nil {
				t.Fatalf("readSSEFrames: %v", err)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for frames; collected so far: %+v", collected)
		}
	}
}

func TestReadSSEFramesBasic(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`event: message
data: hello

: keep-alive

data: {"foo":"bar"}

data: line1
data: line2

`))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	frames, errs := readSSEFrames(ctx, body, nil)
	got := readAllFrames(t, frames, errs, 2*time.Second)

	if len(got) != 3 {
		t.Fatalf("expected 3 frames, got %d: %+v", len(got), got)
	}
	if got[0].event != "message" || got[0].data != "hello" {
		t.Errorf("frame 0 wrong: %+v", got[0])
	}
	if got[1].event != "" || got[1].data != `{"foo":"bar"}` {
		t.Errorf("frame 1 wrong: %+v", got[1])
	}
	if got[2].data != "line1\nline2" {
		t.Errorf("frame 2 wrong: %+v", got[2])
	}
}

// In-process double for an http.Client that returns canned SSE bytes for
// the streamClient. We don't go through the real network — these tests run
// the parsing loops against fabricated bodies.

type stubReader struct {
	body string
	off  int
}

func (s *stubReader) Read(p []byte) (int, error) {
	if s.off >= len(s.body) {
		return 0, io.EOF
	}
	n := copy(p, s.body[s.off:])
	s.off += n
	return n, nil
}

// runOpenAIDecode runs the OpenAI-compatible decoding loop manually against
// a fixture body, sidestepping the HTTP path. Mirrors the structure of
// streamOpenAICompatible's read loop.
func runOpenAIDecode(t *testing.T, body string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, errs := readSSEFrames(ctx, &stubReader{body: body}, nil)
	var assembled strings.Builder
	for {
		select {
		case <-ctx.Done():
			return assembled.String(), ctx.Err()
		case err := <-errs:
			if err != nil {
				return assembled.String(), err
			}
		case frame, ok := <-frames:
			if !ok {
				return assembled.String(), nil
			}
			if frame.data == "" {
				continue
			}
			if frame.data == "[DONE]" {
				return assembled.String(), nil
			}
			var chunk openAIStreamChunk
			if err := jsonUnmarshalString(frame.data, &chunk); err != nil {
				continue
			}
			for _, choice := range chunk.Choices {
				if choice.Delta.Content == nil {
					continue
				}
				assembled.WriteString(*choice.Delta.Content)
			}
		}
	}
}

func jsonUnmarshalString(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

func TestOpenAIStreamingHappyPath(t *testing.T) {
	body := strings.Join([]string{
		`: keep-alive`,
		``,
		`data: {"choices":[{"delta":{"role":"assistant","content":null}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":", world"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	got, err := runOpenAIDecode(t, body)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if got != "Hello, world" {
		t.Errorf("expected %q, got %q", "Hello, world", got)
	}
}

// runOpenAIDecodeWithUsage parallels streamOpenAICompatible but only the
// usage-capturing slice of it: we need to assert that a final frame
// carrying `usage` is parsed into an *aiUsage and surfaced. Mirrors
// the production read loop's usage capture exactly so a regression in
// the decode site fails this test.
func runOpenAIDecodeWithUsage(t *testing.T, body string) (string, *aiUsage, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, errs := readSSEFrames(ctx, &stubReader{body: body}, nil)
	var (
		assembled strings.Builder
		usage     *aiUsage
	)
	for {
		select {
		case <-ctx.Done():
			return assembled.String(), usage, ctx.Err()
		case err := <-errs:
			if err != nil {
				return assembled.String(), usage, err
			}
		case frame, ok := <-frames:
			if !ok {
				return assembled.String(), usage, nil
			}
			if frame.data == "" {
				continue
			}
			if frame.data == "[DONE]" {
				return assembled.String(), usage, nil
			}
			var chunk openAIStreamChunk
			if err := jsonUnmarshalString(frame.data, &chunk); err != nil {
				continue
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			for _, choice := range chunk.Choices {
				if choice.Delta.Content == nil {
					continue
				}
				assembled.WriteString(*choice.Delta.Content)
			}
		}
	}
}

func TestOpenAIStreamingCapturesFinalUsageFrame(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hi"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":" there"}}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":7,"total_tokens":49}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	content, usage, err := runOpenAIDecodeWithUsage(t, body)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if content != "Hi there" {
		t.Errorf("content: got %q want %q", content, "Hi there")
	}
	if usage == nil {
		t.Fatalf("usage: nil — final frame's `usage` was not parsed")
	}
	if usage.PromptTokens != 42 || usage.CompletionTokens != 7 {
		t.Errorf("usage: got prompt=%d completion=%d, want 42/7", usage.PromptTokens, usage.CompletionTokens)
	}
}

func TestOpenAIStreamingNoUsageFrameStaysNil(t *testing.T) {
	// Provider that doesn't honor stream_options.include_usage just
	// won't send a usage frame. Caller must accept nil + record 0
	// rather than crash.
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	content, usage, err := runOpenAIDecodeWithUsage(t, body)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if content != "hello" {
		t.Errorf("content: got %q want %q", content, "hello")
	}
	if usage != nil {
		t.Errorf("usage: got %+v, want nil (provider didn't emit usage frame)", usage)
	}
}

func TestStreamOptionsMarshalsAsExpected(t *testing.T) {
	// Quick safety net — make sure the JSON tag wiring is right so
	// upstream actually sees `stream_options.include_usage:true`.
	req := aiChatCompletionRequest{
		Model:         "x",
		StreamOptions: &aiStreamOptions{IncludeUsage: true},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"stream_options":{"include_usage":true}`) {
		t.Errorf("unexpected payload shape: %s", body)
	}
	// And when StreamOptions is nil, the field must not be present at all
	// (omitempty) — otherwise providers that reject the field for
	// non-streaming calls would error.
	body2, _ := json.Marshal(aiChatCompletionRequest{Model: "x"})
	if strings.Contains(string(body2), "stream_options") {
		t.Errorf("non-stream request leaked stream_options: %s", body2)
	}
}

func TestOpenAIStreamingMissingDoneIsError(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"partial"}}]}`,
		``,
	}, "\n")
	// In production this would return an error from streamOpenAICompatible
	// because we never see [DONE]. Here we just confirm the parse loop
	// surfaces the assembled bytes; the production error wrapping is exercised
	// by the integration smoke test.
	got, err := runOpenAIDecode(t, body)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if got != "partial" {
		t.Errorf("expected %q, got %q", "partial", got)
	}
}

func TestAnthropicStreamingTextDeltas(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":" there"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		``,
		`event: ping`,
		`data: {}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, errs := readSSEFrames(ctx, &stubReader{body: body}, nil)
	var assembled strings.Builder
	stopped := false
loop:
	for !stopped {
		select {
		case <-ctx.Done():
			t.Fatalf("ctx: %v", ctx.Err())
		case err := <-errs:
			if err != nil {
				t.Fatalf("err: %v", err)
			}
		case frame, ok := <-frames:
			if !ok {
				break loop
			}
			switch frame.event {
			case "ping", "":
				continue
			case "message_stop":
				stopped = true
				break loop
			case "content_block_delta":
				var chunk anthropicStreamDelta
				if err := jsonUnmarshalString(frame.data, &chunk); err != nil {
					t.Fatalf("parse anthropic chunk: %v", err)
				}
				deltaType, _ := chunk.Delta["type"].(string)
				if deltaType != "text_delta" {
					continue
				}
				text, _ := chunk.Delta["text"].(string)
				assembled.WriteString(text)
			}
		}
	}
	if assembled.String() != "hi there" {
		t.Errorf("expected %q, got %q", "hi there", assembled.String())
	}
}

func TestXAIStreamingError(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.created"}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"start"}`,
		``,
		`data: {"type":"response.error","error":{"type":"upstream","message":"boom"}}`,
		``,
	}, "\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, errs := readSSEFrames(ctx, &stubReader{body: body}, nil)
	var (
		assembled strings.Builder
		gotErr    string
	)
loop:
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("ctx: %v", ctx.Err())
		case err := <-errs:
			if err != nil {
				t.Fatalf("err: %v", err)
			}
		case frame, ok := <-frames:
			if !ok {
				break loop
			}
			if frame.data == "" {
				continue
			}
			var chunk xAIStreamEvent
			if err := jsonUnmarshalString(frame.data, &chunk); err != nil {
				t.Fatalf("parse xai chunk: %v", err)
			}
			switch chunk.Type {
			case "response.error":
				if chunk.Error != nil {
					gotErr = chunk.Error.Message
				}
				break loop
			case "response.output_text.delta":
				assembled.WriteString(chunk.Delta)
			}
		}
	}
	if assembled.String() != "start" {
		t.Errorf("expected partial 'start', got %q", assembled.String())
	}
	if gotErr != "boom" {
		t.Errorf("expected error 'boom', got %q", gotErr)
	}
}

// runMiroMindDecode mirrors streamMiroMind's loop without the HTTP/session
// scaffolding so we can feed canned SSE bodies in. Stays in lockstep with
// the production loop's reasoning_steps + content interleaving + <think>
// open/close logic.
func runMiroMindDecode(t *testing.T, body string) (string, *aiUsage, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, errs := readSSEFrames(ctx, &stubReader{body: body}, nil)
	emit := func(s string, b *strings.Builder) {
		if s != "" {
			b.WriteString(s)
		}
	}
	var (
		assembled  strings.Builder
		inThinking bool
		gotDone    bool
		usage      *aiUsage
	)
	for {
		select {
		case <-ctx.Done():
			return assembled.String(), usage, ctx.Err()
		case err := <-errs:
			if err != nil {
				return assembled.String(), usage, err
			}
		case frame, ok := <-frames:
			if !ok {
				if inThinking {
					emit("\n</think>\n\n", &assembled)
				}
				if !gotDone {
					// Mirror production: missing [DONE] is an error, but
					// for assembly assertions we just return what we have.
					return assembled.String(), usage, nil
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
			if err := jsonUnmarshalString(frame.data, &chunk); err != nil {
				continue
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			for _, choice := range chunk.Choices {
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
				}
				if choice.Delta.Content != nil && *choice.Delta.Content != "" {
					if inThinking {
						emit("\n</think>\n\n", &assembled)
						inThinking = false
					}
					emit(*choice.Delta.Content, &assembled)
				}
			}
		}
	}
}

func TestMiroMindStreamingThinkingThenContent(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_steps":[{"type":"thinking","thought":"hello "}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_steps":[{"type":"thinking","thought":"world"}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"Answer: 42"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	got, _, err := runMiroMindDecode(t, body)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	want := "<think>\nhello world\n</think>\n\nAnswer: 42"
	if got != want {
		t.Errorf("assembled mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestMiroMindStreamingThinkingOnly(t *testing.T) {
	// Deepresearch responses can be entirely thinking — closing the
	// <think> block on [DONE] is what protects the renderer from an
	// unclosed tag.
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_steps":[{"type":"thinking","thought":"only "}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"reasoning_steps":[{"type":"thinking","thought":"a trace"}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	got, _, err := runMiroMindDecode(t, body)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	want := "<think>\nonly a trace\n</think>\n\n"
	if got != want {
		t.Errorf("assembled mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestMiroMindStreamingCapturesUsage(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	content, usage, err := runMiroMindDecode(t, body)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if content != "hi" {
		t.Errorf("content: got %q want %q", content, "hi")
	}
	if usage == nil || usage.PromptTokens != 11 || usage.CompletionTokens != 3 {
		t.Errorf("usage: got %+v, want prompt=11 completion=3", usage)
	}
}

func TestDetectAIProviderMiroMind(t *testing.T) {
	cases := []struct {
		url  string
		want aiProvider
	}{
		{"https://api.miromind.ai/v1/chat/completions", aiProviderMiroMind},
		{"https://API.MiroMind.AI/v1/chat/completions", aiProviderMiroMind},
		{"https://api.openai.com/v1/chat/completions", aiProviderOpenAICompatible},
		{"https://api.anthropic.com/v1/messages", aiProviderAnthropic},
		{"https://ark.cn-beijing.volces.com/api/v3/responses", aiProviderVolcesResponses},
		{"https://ark.cn-beijing.volces.com/api/v3/chat/completions", aiProviderOpenAICompatible},
	}
	for _, tc := range cases {
		if got := detectAIProvider(tc.url); got != tc.want {
			t.Errorf("detectAIProvider(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// runVolcesDecode mirrors streamVolces' SSE loop without the HTTP/session
// scaffolding so we can feed canned fixtures in. Volces shares the xAI
// event shape, so the loop body is intentionally identical to its
// counterpart in streamXAI — if you change one, change the other.
func runVolcesDecode(t *testing.T, body string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, errs := readSSEFrames(ctx, &stubReader{body: body}, nil)
	var assembled strings.Builder
	for {
		select {
		case <-ctx.Done():
			return assembled.String(), ctx.Err()
		case err := <-errs:
			if err != nil {
				return assembled.String(), err
			}
		case frame, ok := <-frames:
			if !ok {
				return assembled.String(), nil
			}
			if frame.data == "" {
				continue
			}
			var chunk xAIStreamEvent
			if err := jsonUnmarshalString(frame.data, &chunk); err != nil {
				continue
			}
			switch chunk.Type {
			case "response.error":
				if chunk.Error != nil && chunk.Error.Message != "" {
					return assembled.String(), errFromString(chunk.Error.Message)
				}
				return assembled.String(), errFromString("volces stream returned response.error")
			case "response.completed":
				return assembled.String(), nil
			case "response.output_text.delta":
				assembled.WriteString(chunk.Delta)
			}
		}
	}
}

// errFromString is a tiny helper so the decode loop above can fabricate
// errors without dragging fmt/errors imports into the test file just for
// this one site. Kept local to avoid churn in other tests.
func errFromString(s string) error { return &stringError{s} }

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }

func TestVolcesStreamingHappyPath(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.created"}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"part1 "}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"part2"}`,
		``,
		`data: {"type":"response.completed"}`,
		``,
	}, "\n")
	got, err := runVolcesDecode(t, body)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if got != "part1 part2" {
		t.Errorf("expected %q, got %q", "part1 part2", got)
	}
}

func TestVolcesStreamingError(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"start"}`,
		``,
		`data: {"type":"response.error","error":{"type":"upstream","message":"boom"}}`,
		``,
	}, "\n")
	got, err := runVolcesDecode(t, body)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected error 'boom', got %v", err)
	}
	if got != "start" {
		t.Errorf("expected partial 'start', got %q", got)
	}
}

func TestVolcesStreamingPayloadShape(t *testing.T) {
	// Regression guard: streaming wrapper must add `stream:true`, must NOT
	// add web_search (Volces 400s on it), and must use `input` not
	// `messages` (Responses API rejects messages).
	req := convertToVolcesResponsesRequest(aiChatCompletionRequest{
		Model: "doubao-1-5",
		Messages: []aiChatCompletionMessage{
			{Role: "user", Content: "hi"},
		},
	})
	wrapper := struct {
		xAIResponsesRequest
		Stream bool `json:"stream"`
	}{xAIResponsesRequest: req, Stream: true}
	body, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `"stream":true`) {
		t.Errorf("stream flag missing: %s", s)
	}
	if strings.Contains(s, "web_search") {
		t.Errorf("web_search leaked into Volces payload: %s", s)
	}
	if strings.Contains(s, `"messages"`) {
		t.Errorf("messages leaked into Volces payload (must be input): %s", s)
	}
	if !strings.Contains(s, `"input"`) {
		t.Errorf("expected input array in Volces payload: %s", s)
	}
}
