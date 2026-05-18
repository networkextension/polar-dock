package dock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	systemUserID    = "system"
	systemUsername  = "system"
	systemUserEmail = "system@local.polar"
)

type aiAgent struct {
	server       *Server
	apiKey       string
	baseURL      string
	model        string
	systemPrompt string
	streaming    bool
	tasks        chan aiAgentTask
	stopCh       chan struct{}
	stopOnce     sync.Once
	httpClient   *http.Client
	streamClient *http.Client

	// streams tracks in-flight streaming generations keyed by chat_messages.id
	// so a revoke handler can cancel the LLM HTTP call mid-stream.
	streamsMu sync.Mutex
	streams   map[int64]context.CancelFunc
}

type aiAgentTask struct {
	ThreadID        int64
	LLMThreadID     *int64
	UserID          string
	ResponderUserID string
	ResponderName   string
	Content         string
	// WorkdirSubpath, if set, is a relative path under the agent's
	// pinned workdir to chdir into before running the local tool.
	// Used to isolate per-project work — pickup populates it with
	// the project id so e.g. tasks for project A and project B
	// don't trample each other's files in $workdir/.
	WorkdirSubpath string
	// GitRemoteURL, if non-empty, asks the agent to (1) ensure the
	// effective workdir is a git repo with this remote configured,
	// (2) commit + push everything coder produced after the task,
	// (3) report commit SHAs back in the chat reply. Empty disables
	// the post-task git step entirely. Phase 1: relies on host's
	// own git auth (SSH key / credential helper).
	GitRemoteURL string
}

type aiRuntimeConfig struct {
	APIKey       string
	BaseURL      string
	Model        string
	SystemPrompt string
	Streaming    bool
	// ProxyURL: optional HTTP(S) proxy used for THIS provider's
	// API call. Empty = inherit dock's default Go transport
	// (which respects HTTP_PROXY / HTTPS_PROXY env). Plumbed in
	// from llm_configs.proxy_url.
	ProxyURL string
	// Snapshot of the source llm_configs row — written onto the
	// chat_messages we generate so history survives a later
	// re-binding of the bot to a different LLM config. 0 / empty
	// for paths that don't go through an llm_configs row (test
	// connectivity probes, passthrough bots).
	LLMConfigID   int64
	LLMConfigName string
	// WorkspaceID is the team being billed for this call. Populated
	// from the proxy token (`tok.WorkspaceID`) for /api/proxy paths
	// and from `personalWorkspaceForUser(task.UserID)` for in-app
	// chat (handleTask). Empty = billing gates skipped (back-compat
	// for paths that don't have a workspace context — system agent
	// probes, etc.).
	WorkspaceID string
	// Endpoint is a caller-supplied tag describing the *feature* that
	// initiated this LLM call — e.g. "thread_summary",
	// "task_retrospect", "chat", "post_assist". It surfaces as a label
	// on the polar_llm_* metrics so the operator can slice usage by
	// feature. Empty = "unknown" in metrics. Free-form but should be a
	// short, stable identifier; see metrics.go for cardinality notes.
	Endpoint string
}

type aiChatCompletionRequest struct {
	Model              string                    `json:"model"`
	Messages           []aiChatCompletionMessage `json:"messages"`
	MaxTokens          int                       `json:"max_tokens,omitempty"`
	Stream             bool                      `json:"stream,omitempty"`
	ChatTemplateKwargs map[string]any            `json:"chat_template_kwargs,omitempty"`
	// Tools / ToolChoice are OpenAI-compatible function-calling
	// fields used when the bot has a polar-agent attached. Kimi
	// supports the same shape via api.moonshot.cn.
	Tools      []aiToolSpec `json:"tools,omitempty"`
	ToolChoice string       `json:"tool_choice,omitempty"` // "auto" | "none" | "required"
	// StreamOptions is OpenAI-compat: include_usage=true makes the
	// stream emit a final frame carrying `usage`. Populated by
	// streamOpenAICompatible right before marshalling so callers
	// don't have to think about it.
	StreamOptions *aiStreamOptions `json:"stream_options,omitempty"`
}

type aiStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type aiChatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls is what the assistant emits when it wants to call
	// a tool. Mutually exclusive with Content in practice (Content
	// often empty when tool calls present).
	ToolCalls []aiToolCallMsg `json:"tool_calls,omitempty"`
	// ToolCallID + Name set when this is a "role: tool" message
	// echoing a tool's output back into the conversation.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// aiToolSpec is the OpenAI function-calling schema. We only use
// "function" type; type=function + function.name/description/parameters.
type aiToolSpec struct {
	Type     string         `json:"type"`
	Function aiToolFunction `json:"function"`
}

type aiToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// aiToolCallMsg is one tool call inside an assistant message. id
// is the model's correlation token (we echo it back on the tool
// result message). function.arguments is a JSON-encoded string.
type aiToolCallMsg struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type aiChatCompletionChoice struct {
	Message      aiChatCompletionMessage `json:"message"`
	FinishReason string                  `json:"finish_reason,omitempty"`
}

type aiChatCompletionResponse struct {
	Choices []aiChatCompletionChoice `json:"choices"`
	Error   json.RawMessage          `json:"error,omitempty"`
	Usage   *aiUsage                 `json:"usage,omitempty"`
}

// aiUsage mirrors the OpenAI-compatible `usage` block. Most upstream
// providers (Kimi/Moonshot, Volces/ARK, DeepSeek, OpenAI itself) emit
// it; non-OpenAI shapes (Anthropic Messages, Gemini, XAI Responses)
// don't, so for those paths Usage stays nil — token counters skip the
// sample rather than guessing.
type aiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

type aiProvider string

const (
	aiProviderOpenAICompatible aiProvider = "openai_compatible"
	aiProviderGemini           aiProvider = "gemini"
	aiProviderXAIResponses     aiProvider = "xai_responses"
	aiProviderVolcesResponses  aiProvider = "volces_responses"
	aiProviderAnthropic        aiProvider = "anthropic_messages"
	// MiroMind / MiroThinker (api.miromind.ai) speaks OpenAI Chat
	// Completions on the wire, but its streaming frames carry the
	// deepresearch thinking trace in `choices[].delta.reasoning_steps`
	// instead of (or before) `delta.content`. We give it a dedicated
	// stream decoder so users see the reasoning live; the non-streaming
	// path is identical to OpenAI-compat.
	aiProviderMiroMind aiProvider = "miromind"
)

type geminiGenerateContentRequest struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
}

type geminiGenerateContentResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
	// UsageMetadata is present on streamGenerateContent's terminal chunk
	// (and sometimes earlier ones) and on non-streaming responses. Used
	// by streamGemini to surface token counts via *aiUsage; nil when
	// missing so downstream metrics record 0/0 instead of guessing.
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
	Error         json.RawMessage      `json:"error,omitempty"`
}

type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text,omitempty"`
}

type xAIResponsesRequest struct {
	Model string                 `json:"model"`
	Input []xAIResponsesMessage  `json:"input"`
	Tools []xAIResponsesToolSpec `json:"tools,omitempty"`
}

type xAIResponsesMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type xAIResponsesToolSpec struct {
	Type string `json:"type"`
}

type xAIResponsesResponse struct {
	OutputText string               `json:"output_text,omitempty"`
	Output     []xAIResponsesOutput `json:"output,omitempty"`
	Error      json.RawMessage      `json:"error,omitempty"`
}

type xAIResponsesOutput struct {
	Type    string                      `json:"type,omitempty"`
	Text    string                      `json:"text,omitempty"`
	Content []xAIResponsesOutputContent `json:"content,omitempty"`
}

type xAIResponsesOutputContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type anthropicMessagesRequest struct {
	Model     string                     `json:"model"`
	System    string                     `json:"system,omitempty"`
	Messages  []anthropicMessagesMessage `json:"messages"`
	MaxTokens int                        `json:"max_tokens"`
}

type anthropicMessagesMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicMessagesResponse struct {
	Content []anthropicMessagesContent `json:"content,omitempty"`
	Error   json.RawMessage            `json:"error,omitempty"`
}

type anthropicMessagesContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

func newAIAgent(server *Server, cfg Config) *aiAgent {
	if server == nil {
		return nil
	}
	return &aiAgent{
		server:       server,
		apiKey:       strings.TrimSpace(cfg.AIAgentAPIKey),
		baseURL:      strings.TrimSpace(cfg.AIAgentBaseURL),
		model:        strings.TrimSpace(cfg.AIAgentModel),
		systemPrompt: strings.TrimSpace(cfg.AIAgentSystemPrompt),
		streaming:    cfg.AIAgentStreaming,
		tasks:        make(chan aiAgentTask, 64),
		stopCh:       make(chan struct{}),
		streams:      make(map[int64]context.CancelFunc),
		httpClient: &http.Client{
			// 300s wall-clock timeout. Was 180s — too tight for
			// reasoning models (grok-4-fast / o1-class) on long
			// prompts where the upstream sits silent for minutes
			// before emitting the first token. The non-streaming
			// path here returns a single completed body, so any
			// idle-gap observation has to be a hard ceiling. 5
			// minutes covers most real cases; genuine hangs still
			// surface within human time. Streaming path (separate
			// streamClient) has no overall timeout — uses an
			// idle-gap watchdog instead.
			Timeout: 300 * time.Second,
		},
		// streamClient has no overall timeout; the per-stream idle-gap
		// watchdog cancels the request context instead. A 180s wall-clock
		// limit would hard-kill long generations on slow self-hosted
		// inference, defeating the point of streaming.
		streamClient: &http.Client{Timeout: 0},
	}
}

func (a *aiAgent) stop() {
	if a == nil {
		return
	}
	a.stopOnce.Do(func() {
		close(a.stopCh)
	})
}

// httpClientFor returns a.httpClient when no proxy is configured
// (default Go transport, respects HTTP_PROXY / HTTPS_PROXY env on
// the dock machine) or a freshly-built client wired to the
// supplied proxy URL. Per-call build keeps the implementation
// trivial; cost is one Transport allocation, dominated by the LLM
// round-trip latency itself.
func (a *aiAgent) httpClientFor(proxyURL string) *http.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return a.httpClient
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		log.Printf("ai agent invalid proxy_url %q, falling back to default client: %v", proxyURL, err)
		return a.httpClient
	}
	return &http.Client{
		Timeout: 180 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(parsed),
		},
	}
}

// streamClientFor mirrors httpClientFor but returns a client with
// no overall timeout (matching a.streamClient) for SSE-style
// long-lived connections.
func (a *aiAgent) streamClientFor(proxyURL string) *http.Client {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return a.streamClient
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		log.Printf("ai agent invalid proxy_url %q, falling back to default stream client: %v", proxyURL, err)
		return a.streamClient
	}
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(parsed),
		},
	}
}

func (a *aiAgent) enqueue(task aiAgentTask) {
	if a == nil || task.ThreadID <= 0 || task.UserID == "" || task.ResponderUserID == "" || strings.TrimSpace(task.Content) == "" {
		return
	}
	select {
	case a.tasks <- task:
	default:
		log.Printf("ai agent queue full, dropping task for thread %d", task.ThreadID)
	}
}

func (a *aiAgent) run() {
	if a == nil {
		return
	}
	for {
		select {
		case <-a.stopCh:
			return
		case task := <-a.tasks:
			a.handleTask(task)
		}
	}
}

func (a *aiAgent) handleTask(task aiAgentTask) {
	cfg, err := a.runtimeConfig(task.ThreadID, task.LLMThreadID, task.ResponderUserID)
	if err != nil {
		log.Printf("ai agent resolve runtime config failed: %v", err)
		latency := int64(0)
		if _, sendErr := a.server.sendFailedBotMessage(task.ThreadID, task.LLMThreadID, task.ResponderUserID, task.ResponderName, "AI 助理配置无法加载，请联系管理员。", &latency, time.Now()); sendErr != nil {
			log.Printf("send failed ai agent chat message failed: %v", sendErr)
		}
		return
	}
	// B5a: in-app chat billing gate. Bills against the requester's
	// personal team (same shape as the LLM proxy's `tok.WorkspaceID`
	// gate at llm_proxy_chat_handlers.go:146,166). Absent llm config
	// or workspace = back-compat skip, matching the proxy.
	if a.server != nil && cfg.LLMConfigID > 0 && task.UserID != "" {
		if ws, wErr := a.server.personalWorkspaceForUser(task.UserID); wErr == nil && ws != "" {
			cfg.WorkspaceID = ws
			if blocked, body := a.server.checkChatBillingGate(ws, cfg.LLMConfigID); blocked {
				latency := int64(0)
				log.Printf("ai agent chat blocked by billing gate: workspace=%s llm_config=%d msg=%s", ws, cfg.LLMConfigID, body)
				if _, sendErr := a.server.sendFailedBotMessageWithLLM(task.ThreadID, task.LLMThreadID, task.ResponderUserID, task.ResponderName, body, &latency, chatLLMSnapshotFromCfg(cfg), time.Now()); sendErr != nil {
					log.Printf("send billing-gate failed message failed: %v", sendErr)
				}
				return
			}
		}
	}
	// Long-running path: bot has a polar-agent attached. Show a
	// streaming placeholder ("✦ thinking...") immediately so the
	// user sees the bot is working — kimi-cli / claude / codex can
	// take 30+ minutes on real coding tasks. handleAgentTask
	// reuses the same placeholder + finalize plumbing as
	// handleStreamingTask but drives one synchronous generateReply
	// instead of an SSE loop.
	if a.server != nil && a.server.agentHub != nil && task.ResponderUserID != "" &&
		a.server.agentHub.lookup(task.ResponderUserID) != nil {
		a.handleAgentTask(task, cfg)
		return
	}
	if cfg.Streaming {
		a.handleStreamingTask(task, cfg)
		return
	}
	a.handleSyncTask(task, cfg)
}

// handleAgentTask is the placeholder-bracketed sync path used for
// any bot with a polar-agent attached (kimi passthrough or tool
// loop). It mirrors handleSyncTask but inserts a streaming
// placeholder up front so the user sees a "thinking" bubble
// during the (potentially long) wait, then finalizes the same
// row with the agent's reply.
func (a *aiAgent) handleAgentTask(task aiAgentTask, cfg aiRuntimeConfig) {
	now := time.Now()
	placeholderID, phErr := a.server.sendStreamingPlaceholder(task.ThreadID, task.LLMThreadID, task.ResponderUserID, task.ResponderName, now)
	if phErr != nil {
		// Fallback to plain sync flow if the placeholder fails — at
		// least the user still gets the final reply.
		log.Printf("ai agent placeholder insert failed (falling back to sync): %v", phErr)
		a.handleSyncTask(task, cfg)
		return
	}
	start := time.Now()
	reply, err := a.generateReply(task, cfg)
	latencyMs := time.Since(start).Milliseconds()
	latencyPtr := &latencyMs
	if err != nil {
		log.Printf("ai agent reply failed after %dms: %v", latencyMs, err)
		body := userFriendlyChatErrorBody(err)
		if finalErr := a.server.finalizeChatMessageWithLLM(placeholderID, body, nil, "", latencyPtr, true, chatLLMSnapshotFromCfg(cfg)); finalErr != nil {
			log.Printf("finalize agent placeholder (failed) error: %v", finalErr)
		}
		return
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		reply = "（agent 没有可返回的结果）"
	}
	log.Printf("ai agent reply ok in %dms (thread=%d) [agent path]", latencyMs, task.ThreadID)

	// Mirror handleStreamingTask success path: stash full reply as
	// a markdown entry so the user can re-open the long output, and
	// finalize the placeholder with a short preview + link.
	title := buildSystemMarkdownTitle(reply, time.Now())
	entry, _, mdErr := a.server.saveMarkdownDocument(task.ResponderUserID, title, reply, "markdown", MarkdownVisibilityPrivate, nil, time.Now())
	if mdErr != nil {
		log.Printf("save agent markdown failed: %v (finalizing as plain text)", mdErr)
		if finalErr := a.server.finalizeChatMessageWithLLM(placeholderID, reply, nil, "", latencyPtr, false, chatLLMSnapshotFromCfg(cfg)); finalErr != nil {
			log.Printf("finalize agent placeholder (no markdown) error: %v", finalErr)
		}
		return
	}
	preview := buildMarkdownPreview(reply, 120)
	if err := a.server.finalizeChatMessageWithLLM(placeholderID, preview, &entry.ID, entry.Title, latencyPtr, false, chatLLMSnapshotFromCfg(cfg)); err != nil {
		log.Printf("finalize agent placeholder error: %v", err)
	}
}

// handleSyncTask is the original non-streaming flow: one HTTP call, save
// markdown, send the shared_markdown message. Kept verbatim so flipping
// streaming=false is a true regression gate.
func (a *aiAgent) handleSyncTask(task aiAgentTask, cfg aiRuntimeConfig) {
	start := time.Now()
	reply, err := a.generateReply(task, cfg)
	latencyMs := time.Since(start).Milliseconds()
	latencyPtr := &latencyMs
	snap := chatLLMSnapshotFromCfg(cfg)
	if err != nil {
		log.Printf("ai agent reply failed after %dms: %v", latencyMs, err)
		reply = userFriendlyChatErrorBody(err)
		if _, sendErr := a.server.sendFailedBotMessageWithLLM(task.ThreadID, task.LLMThreadID, task.ResponderUserID, task.ResponderName, reply, latencyPtr, snap, time.Now()); sendErr != nil {
			log.Printf("send failed ai agent chat message failed: %v", sendErr)
		}
		return
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		reply = "我暂时没有可返回的结果。"
	}
	log.Printf("ai agent reply ok in %dms (thread=%d)", latencyMs, task.ThreadID)
	now := time.Now()
	title := buildSystemMarkdownTitle(reply, now)
	entry, _, err := a.server.saveMarkdownDocument(task.ResponderUserID, title, reply, "markdown", MarkdownVisibilityPrivate, nil, now)
	if err != nil {
		log.Printf("save ai markdown failed: %v", err)
		if _, sendErr := a.server.sendBotChatMessageWithLLM(task.ThreadID, task.LLMThreadID, task.ResponderUserID, task.ResponderName, reply, latencyPtr, snap, now); sendErr != nil {
			log.Printf("send fallback ai agent chat message failed: %v", sendErr)
		}
		return
	}
	if _, err := a.server.sendSharedMarkdownMessageWithLLM(task.ThreadID, task.LLMThreadID, task.ResponderUserID, task.ResponderName, entry.ID, entry.Title, buildMarkdownPreview(reply, 120), latencyPtr, snap, now); err != nil {
		log.Printf("send ai shared markdown message failed: %v", err)
	}
}

// handleStreamingTask:
//  1. Insert an empty placeholder shared_markdown row (streaming=true) and
//     broadcast it so the UI can show a "thinking…" bubble immediately.
//  2. Call requestStreamingChatCompletion with an onDelta that accumulates
//     text in memory and flushes to DB at most once per second (republishing
//     the message-created event each time so WS subscribers see the growth).
//  3. On success: saveMarkdownDocument + finalizeChatMessage with the
//     markdown link, latency, streaming=false. Republish.
//  4. On error: finalizeChatMessage with failed=true and partial text.
//
// Cancellation is honored via context: revoke handler can call
// aiAgent.cancelStream(messageID) which fires the stored CancelFunc.
func (a *aiAgent) handleStreamingTask(task aiAgentTask, cfg aiRuntimeConfig) {
	if strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" {
		// Fall back to the synchronous error message — same UX as before.
		a.handleSyncTask(task, cfg)
		return
	}

	now := time.Now()
	placeholderID, err := a.server.sendStreamingPlaceholder(task.ThreadID, task.LLMThreadID, task.ResponderUserID, task.ResponderName, now)
	if err != nil {
		log.Printf("ai agent placeholder insert failed: %v", err)
		a.handleSyncTask(task, cfg)
		return
	}

	contextText, ctxErr := a.buildContext(task.ThreadID, task.LLMThreadID)
	if ctxErr != nil {
		log.Printf("build ai context failed: %v", ctxErr)
	}

	cfg.Endpoint = "chat"
	payload := aiChatCompletionRequest{
		Model: cfg.Model,
		Messages: []aiChatCompletionMessage{
			{Role: "system", Content: cfg.SystemPrompt},
			{Role: "system", Content: contextText},
			{Role: "user", Content: task.Content},
		},
		MaxTokens:          32768,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	}

	streamCtx, cancel := context.WithCancel(context.Background())
	a.registerStream(placeholderID, cancel)
	defer a.unregisterStream(placeholderID)

	var (
		accumulator   strings.Builder
		lastFlushed   time.Time
		lastFlushLen  int
		flushInterval = time.Second
		streamStart   = time.Now()
	)

	flush := func(force bool) {
		current := accumulator.String()
		if !force && (len(current) == lastFlushLen || time.Since(lastFlushed) < flushInterval) {
			return
		}
		ok, err := a.server.updateChatMessageContent(placeholderID, current)
		if err != nil {
			log.Printf("ai agent update streaming content failed: %v", err)
			return
		}
		if !ok {
			log.Printf("ai agent stream flush row not updated (id=%d, len=%d) — finalized or revoked", placeholderID, len(current))
			cancel()
			return
		}
		lastFlushed = time.Now()
		lastFlushLen = len(current)
		log.Printf("ai agent stream flush id=%d len=%d", placeholderID, len(current))
		a.republishMessageCreatedSilent(task.ThreadID, placeholderID, task.ResponderUserID)
	}

	final, streamErr := a.requestStreamingChatCompletion(streamCtx, cfg, payload, func(delta string) {
		accumulator.WriteString(delta)
		flush(false)
	})

	// Drain any unflushed tail so the user sees the last chunks before we
	// transition to the finalized markdown card.
	flush(true)

	totalLatency := time.Since(streamStart).Milliseconds()
	totalLatencyPtr := &totalLatency

	if streamErr != nil {
		partial := strings.TrimSpace(accumulator.String())
		if partial == "" {
			partial = strings.TrimSpace(final)
		}
		if errors.Is(streamErr, errUpstreamOutOfCredit) {
			// Credit errors usually surface before any tokens arrive
			// (provider rejects the request outright), so prefer the
			// clean friendly message over a "got 3 tokens then died" hybrid.
			partial = userFriendlyChatErrorBody(streamErr)
		} else if partial == "" {
			partial = userFriendlyChatErrorBody(streamErr)
		} else {
			partial += "\n\n（生成被中断：" + streamErr.Error() + "）"
		}
		log.Printf("ai agent stream failed after %dms (thread=%d): %v", totalLatency, task.ThreadID, streamErr)
		if err := a.server.finalizeChatMessageWithLLM(placeholderID, partial, nil, "", totalLatencyPtr, true, chatLLMSnapshotFromCfg(cfg)); err != nil {
			log.Printf("finalize streaming failed: %v", err)
		}
		a.republishMessageCreated(task.ThreadID, placeholderID, task.ResponderUserID)
		return
	}

	finalText := strings.TrimSpace(final)
	if finalText == "" {
		finalText = strings.TrimSpace(accumulator.String())
	}
	if finalText == "" {
		finalText = "我暂时没有可返回的结果。"
	}
	log.Printf("ai agent stream ok in %dms (thread=%d, bytes=%d)", totalLatency, task.ThreadID, len(finalText))

	completedAt := time.Now()
	title := buildSystemMarkdownTitle(finalText, completedAt)
	entry, _, err := a.server.saveMarkdownDocument(task.ResponderUserID, title, finalText, "markdown", MarkdownVisibilityPrivate, nil, completedAt)
	if err != nil {
		log.Printf("save ai markdown failed: %v", err)
		// Finalize without a markdown link; the streaming bubble keeps the
		// final text and just loses the "expand" button.
		if finalErr := a.server.finalizeChatMessageWithLLM(placeholderID, finalText, nil, "", totalLatencyPtr, false, chatLLMSnapshotFromCfg(cfg)); finalErr != nil {
			log.Printf("finalize streaming (no markdown) failed: %v", finalErr)
		}
		a.republishMessageCreated(task.ThreadID, placeholderID, task.ResponderUserID)
		return
	}

	preview := buildMarkdownPreview(finalText, 120)
	if err := a.server.finalizeChatMessageWithLLM(placeholderID, preview, &entry.ID, entry.Title, totalLatencyPtr, false, chatLLMSnapshotFromCfg(cfg)); err != nil {
		log.Printf("finalize streaming success failed: %v", err)
	}
	a.republishMessageCreated(task.ThreadID, placeholderID, task.ResponderUserID)
}

// republishMessageCreated re-fires the chatEventMessageCreated event so the
// WS hub re-fetches the row and broadcasts the updated state. The hub uses
// getChatMessageByID, so each republish carries the latest content/seq.
// Mid-stream republishes are marked silent so we don't fire push-prep work
// once per second; the final post-finalize republish leaves silent=false so
// the receiver still gets a notification for the completed reply.
func (a *aiAgent) republishMessageCreated(threadID, messageID int64, senderID string) {
	a.server.publishChatInternalEvent(chatInternalEvent{
		Event:     chatEventMessageCreated,
		ChatID:    threadID,
		MessageID: messageID,
		SenderID:  senderID,
	})
}

func (a *aiAgent) republishMessageCreatedSilent(threadID, messageID int64, senderID string) {
	a.server.publishChatInternalEvent(chatInternalEvent{
		Event:     chatEventMessageCreated,
		ChatID:    threadID,
		MessageID: messageID,
		SenderID:  senderID,
		Silent:    true,
	})
}

func (a *aiAgent) registerStream(messageID int64, cancel context.CancelFunc) {
	if a == nil || messageID <= 0 || cancel == nil {
		return
	}
	a.streamsMu.Lock()
	defer a.streamsMu.Unlock()
	if a.streams == nil {
		a.streams = make(map[int64]context.CancelFunc)
	}
	a.streams[messageID] = cancel
}

func (a *aiAgent) unregisterStream(messageID int64) {
	if a == nil || messageID <= 0 {
		return
	}
	a.streamsMu.Lock()
	defer a.streamsMu.Unlock()
	delete(a.streams, messageID)
}

// cancelStream aborts an in-flight streaming generation for the given
// placeholder message id. Safe to call when no stream is active. Used by
// the revoke handler so deleting a streaming bubble immediately tears down
// the upstream LLM connection.
func (a *aiAgent) cancelStream(messageID int64) {
	if a == nil || messageID <= 0 {
		return
	}
	a.streamsMu.Lock()
	cancel, ok := a.streams[messageID]
	delete(a.streams, messageID)
	a.streamsMu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

func (a *aiAgent) generateReply(task aiAgentTask, runtimeConfig aiRuntimeConfig) (string, error) {
	// Look up the bot kind first. Passthrough bots have no LLM
	// config and the WS-attached agent IS their runtime; if we
	// can't reach an agent, refuse the request rather than fall
	// through to the missing-LLM error which would be confusing.
	var botKind, preferredTool string
	if task.ResponderUserID != "" && task.ResponderUserID != systemUserID {
		if bot, _ := a.server.getBotUserByUserID(task.ResponderUserID); bot != nil {
			botKind = bot.BotKind
			preferredTool = bot.PreferredTool
		}
	}

	// Host module P1c.3: when this bot is bound to a host_skill row
	// (bot_users.host_skill_id), the dispatch goes through the
	// skill.start / skill.event protocol against the BOUND host —
	// which may or may not be the host the bot's own attach token is
	// on. Legacy bots with NULL host_skill_id fall through to the
	// passthrough/tool-loop path below.
	if a.server != nil && task.ResponderUserID != "" {
		hostSkillID, err := a.server.getBotHostSkillID(task.ResponderUserID)
		if err != nil {
			log.Printf("[ai-agent] bot=%s getBotHostSkillID: %v (falling back to legacy path)", task.ResponderUserID, err)
		} else if hostSkillID > 0 {
			log.Printf("[ai-agent] bot=%s using host_skill dispatch skill_id=%d", task.ResponderUserID, hostSkillID)
			return a.generateReplyWithHostSkill(task, hostSkillID)
		}
	}

	// Phase 1.4: route to the right agent path when this bot has a
	// polar-agent attached. Two flavors:
	//   - kimi-cli passthrough — the agent has kimi-cli installed
	//     and declares the "passthrough" capability. Platform
	//     forwards the chat content as-is, agent shells out to kimi
	//     (or claude/codex/...), returns its stdout. Platform LLM
	//     not invoked at all.
	//   - tool loop (1.4A) — platform drives an OpenAI-compatible
	//     tool-calling loop, using read_file/write_file/list_dir/
	//     run_cmd tools that the agent executes.
	if a.server != nil && a.server.agentHub != nil && task.ResponderUserID != "" {
		if conn := a.server.agentHub.lookup(task.ResponderUserID); conn != nil {
			if conn.hasPassthrough() {
				toolName := conn.tool
				if toolName == "" {
					toolName = "kimi"
				}
				log.Printf("[ai-agent] bot=%s using passthrough tool=%s (workdir=%s)", task.ResponderUserID, toolName, conn.workdir)
				return a.generateReplyWithPassthrough(task, conn)
			}
			log.Printf("[ai-agent] bot=%s using tool loop (workdir=%s)", task.ResponderUserID, conn.workdir)
			return a.generateReplyWithTools(task, runtimeConfig, task.ResponderUserID, conn.workdir)
		}
	}

	// Passthrough bot without an attached agent: don't fall through
	// to the LLM (it has no config and the user explicitly opted
	// into "this bot is local-tool-only"). Return a clear actionable
	// error.
	if botKind == BotKindPassthrough {
		hint := preferredTool
		if hint == "" {
			hint = "kimi"
		}
		return fmt.Sprintf("该 Bot 仅支持本地工具接管（passthrough 模式），暂时没有 polar-agent 连上。请先在终端运行：\n```\npolar-agent attach --bot=%s --workdir=<path> --tool=%s\n```", task.ResponderUserID, hint), nil
	}

	if strings.TrimSpace(runtimeConfig.APIKey) == "" || strings.TrimSpace(runtimeConfig.BaseURL) == "" || strings.TrimSpace(runtimeConfig.Model) == "" {
		if task.ResponderUserID == systemUserID {
			return "AI 助理尚未配置完成，请联系管理员设置 `AI_AGENT_API_KEY`、`AI_AGENT_BASE_URL` 和 `AI_AGENT_MODEL`。", nil
		}
		return "这个 Bot 的模型配置还没准备好，请先补全 API Key、Base URL 和 Model。", nil
	}

	contextText, err := a.buildContext(task.ThreadID, task.LLMThreadID)
	if err != nil {
		log.Printf("build ai context failed: %v", err)
	}

	runtimeConfig.Endpoint = "chat"
	payload := aiChatCompletionRequest{
		Model: runtimeConfig.Model,
		Messages: []aiChatCompletionMessage{
			{
				Role:    "system",
				Content: runtimeConfig.SystemPrompt,
			},
			{
				Role:    "system",
				Content: contextText,
			},
			{
				Role:    "user",
				Content: task.Content,
			},
		},
		MaxTokens:          32768,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	}

	result, err := a.requestChatCompletion(runtimeConfig, payload)
	if err != nil {
		return "", err
	}
	if len(result.Choices) == 0 {
		return "", errors.New("empty ai response")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

func (a *aiAgent) testRuntimeConfig(runtimeConfig aiRuntimeConfig) error {
	if strings.TrimSpace(runtimeConfig.APIKey) == "" || strings.TrimSpace(runtimeConfig.BaseURL) == "" || strings.TrimSpace(runtimeConfig.Model) == "" {
		return errors.New("API Key、Base URL 和 Model 不能为空")
	}
	if strings.TrimSpace(runtimeConfig.SystemPrompt) == "" {
		runtimeConfig.SystemPrompt = a.server.renderPrompt(promptSlugAITestSystem, nil, "你是一个用于连通性验证的 AI 助手，请简短回复 ok。")
	}
	runtimeConfig.Endpoint = "test_connectivity"

	payload := aiChatCompletionRequest{
		Model: runtimeConfig.Model,
		Messages: []aiChatCompletionMessage{
			{Role: "system", Content: runtimeConfig.SystemPrompt},
			{Role: "user", Content: "请回复 ok"},
		},
	}
	result, err := a.requestChatCompletion(runtimeConfig, payload)
	if err != nil {
		return err
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return errors.New("模型已连通，但返回为空")
	}
	return nil
}

// requestChatCompletion is the funnel for all non-streaming LLM calls.
// Wraps requestChatCompletionInner with Phase 2 metrics: every dispatch
// records timing + status + (when the upstream response carried a usage
// block) prompt/completion token counts.
func (a *aiAgent) requestChatCompletion(runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest) (*aiChatCompletionResponse, error) {
	provider := detectAIProvider(runtimeConfig.BaseURL)
	start := time.Now()
	resp, err := a.requestChatCompletionInner(runtimeConfig, payload)
	prompt, completion := 0, 0
	if resp != nil && resp.Usage != nil {
		prompt = resp.Usage.PromptTokens
		completion = resp.Usage.CompletionTokens
	}
	if a.server != nil {
		a.server.metricsRegistry.recordLLMCall(string(provider), runtimeConfig.Model, runtimeConfig.Endpoint, time.Since(start), err, prompt, completion)
	}
	return resp, err
}

func (a *aiAgent) requestChatCompletionInner(runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest) (*aiChatCompletionResponse, error) {
	switch detectAIProvider(runtimeConfig.BaseURL) {
	case aiProviderAnthropic:
		return a.requestAnthropicMessages(runtimeConfig, payload)
	case aiProviderGemini:
		return a.requestGeminiContent(runtimeConfig, payload)
	case aiProviderXAIResponses:
		return a.requestXAIResponses(runtimeConfig, payload)
	case aiProviderVolcesResponses:
		return a.requestVolcesResponses(runtimeConfig, payload)
	case aiProviderMiroMind:
		// Wire shape is OpenAI Chat Completions; reasoning_steps only
		// appears in streaming deltas, so the non-streaming response
		// is just `choices[0].message.content`.
		return a.requestOpenAICompatibleChatCompletion(runtimeConfig, payload)
	default:
		return a.requestOpenAICompatibleChatCompletion(runtimeConfig, payload)
	}
}

func (a *aiAgent) requestOpenAICompatibleChatCompletion(runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest) (*aiChatCompletionResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent request: url=%s model=%s proxy=%q payload=%s", runtimeConfig.BaseURL, runtimeConfig.Model, runtimeConfig.ProxyURL, compactLogJSON(body))

	req, err := http.NewRequest(http.MethodPost, runtimeConfig.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+runtimeConfig.APIKey)

	resp, err := a.httpClientFor(runtimeConfig.ProxyURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent response: status=%d body=%s", resp.StatusCode, compactLogJSON(responseBody))

	var result aiChatCompletionResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		if message := parseAIErrorMessage(result.Error); message != "" {
			return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, errors.New(message))
		}
		return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, fmt.Errorf("ai api returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody))))
	}
	if len(result.Choices) == 0 {
		// Some OpenAI-compatible gateways (e.g. api.soxai.io) proxy Claude and
		// return Anthropic-native {"content":[{"type":"text","text":"..."}]}
		// instead of {"choices":[{"message":{...}}]}. Fall back to parsing
		// that shape so callers get a usable response.
		var anthropicResult anthropicMessagesResponse
		if err := json.Unmarshal(responseBody, &anthropicResult); err == nil {
			if text := extractAnthropicResponseText(anthropicResult); text != "" {
				return &aiChatCompletionResponse{
					Choices: []aiChatCompletionChoice{
						{Message: aiChatCompletionMessage{Role: "assistant", Content: text}},
					},
				}, nil
			}
		}
	}
	return &result, nil
}

func (a *aiAgent) requestGeminiContent(runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest) (*aiChatCompletionResponse, error) {
	reqURL, err := buildGeminiRequestURL(runtimeConfig.BaseURL, runtimeConfig.Model, runtimeConfig.APIKey)
	if err != nil {
		return nil, err
	}

	bodyPayload := convertToGeminiRequest(payload)
	body, err := json.Marshal(bodyPayload)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent request: provider=gemini url=%s model=%s proxy=%q payload=%s", reqURL, runtimeConfig.Model, runtimeConfig.ProxyURL, compactLogJSON(body))

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClientFor(runtimeConfig.ProxyURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent response: provider=gemini status=%d body=%s", resp.StatusCode, compactLogJSON(responseBody))

	var result geminiGenerateContentResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		if message := parseAIErrorMessage(result.Error); message != "" {
			return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, errors.New(message))
		}
		return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, fmt.Errorf("gemini api returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody))))
	}

	text := extractGeminiResponseText(result)
	return &aiChatCompletionResponse{
		Choices: []aiChatCompletionChoice{
			{
				Message: aiChatCompletionMessage{
					Role:    "assistant",
					Content: text,
				},
			},
		},
	}, nil
}

func (a *aiAgent) requestXAIResponses(runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest) (*aiChatCompletionResponse, error) {
	reqURL, err := buildXAIResponsesRequestURL(runtimeConfig.BaseURL)
	if err != nil {
		return nil, err
	}

	bodyPayload := convertToXAIResponsesRequest(payload)
	body, err := json.Marshal(bodyPayload)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent request: provider=xai url=%s model=%s proxy=%q payload=%s", reqURL, runtimeConfig.Model, runtimeConfig.ProxyURL, compactLogJSON(body))

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+runtimeConfig.APIKey)

	resp, err := a.httpClientFor(runtimeConfig.ProxyURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent response: provider=xai status=%d body=%s", resp.StatusCode, compactLogJSON(responseBody))

	var result xAIResponsesResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		if message := parseAIErrorMessage(result.Error); message != "" {
			return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, errors.New(message))
		}
		return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, fmt.Errorf("xai api returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody))))
	}

	text := extractXAIResponseText(result)
	return &aiChatCompletionResponse{
		Choices: []aiChatCompletionChoice{
			{
				Message: aiChatCompletionMessage{
					Role:    "assistant",
					Content: text,
				},
			},
		},
	}, nil
}

// requestVolcesResponses calls Volces' OpenAI Responses-compatible
// endpoint. Same wire shape as xAI (input array, output_text or
// output[].content[].text), but without the xAI-specific
// web_search tool — Volces returns 400 if you include it.
// Auth header: standard Bearer.
func (a *aiAgent) requestVolcesResponses(runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest) (*aiChatCompletionResponse, error) {
	reqURL := strings.TrimSpace(runtimeConfig.BaseURL)
	if reqURL == "" {
		return nil, errors.New("Volces 配置缺少 Base URL")
	}

	bodyPayload := convertToVolcesResponsesRequest(payload)
	body, err := json.Marshal(bodyPayload)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent request: provider=volces url=%s model=%s proxy=%q payload=%s", reqURL, runtimeConfig.Model, runtimeConfig.ProxyURL, compactLogJSON(body))

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+runtimeConfig.APIKey)

	resp, err := a.httpClientFor(runtimeConfig.ProxyURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent response: provider=volces status=%d body=%s", resp.StatusCode, compactLogJSON(responseBody))

	var result xAIResponsesResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		if message := parseAIErrorMessage(result.Error); message != "" {
			return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, errors.New(message))
		}
		return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, fmt.Errorf("volces api returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody))))
	}

	text := extractXAIResponseText(result)
	return &aiChatCompletionResponse{
		Choices: []aiChatCompletionChoice{
			{
				Message: aiChatCompletionMessage{
					Role:    "assistant",
					Content: text,
				},
			},
		},
	}, nil
}

func convertToVolcesResponsesRequest(payload aiChatCompletionRequest) xAIResponsesRequest {
	req := xAIResponsesRequest{
		Model: payload.Model,
		Input: make([]xAIResponsesMessage, 0, len(payload.Messages)),
	}
	for _, msg := range payload.Messages {
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		switch role {
		case "system", "assistant", "user":
		default:
			role = "user"
		}
		req.Input = append(req.Input, xAIResponsesMessage{
			Role:    role,
			Content: text,
		})
	}
	return req
}

func (a *aiAgent) requestAnthropicMessages(runtimeConfig aiRuntimeConfig, payload aiChatCompletionRequest) (*aiChatCompletionResponse, error) {
	reqURL, err := buildAnthropicMessagesRequestURL(runtimeConfig.BaseURL)
	if err != nil {
		return nil, err
	}

	bodyPayload := convertToAnthropicMessagesRequest(payload)
	body, err := json.Marshal(bodyPayload)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent request: provider=anthropic url=%s model=%s proxy=%q payload=%s", reqURL, runtimeConfig.Model, runtimeConfig.ProxyURL, compactLogJSON(body))

	req, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", runtimeConfig.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.httpClientFor(runtimeConfig.ProxyURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Printf("ai agent response: provider=anthropic status=%d body=%s", resp.StatusCode, compactLogJSON(responseBody))

	var result anthropicMessagesResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		if message := parseAIErrorMessage(result.Error); message != "" {
			return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, errors.New(message))
		}
		return nil, classifyUpstreamCreditError(resp.StatusCode, responseBody, fmt.Errorf("anthropic api returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody))))
	}

	text := extractAnthropicResponseText(result)
	return &aiChatCompletionResponse{
		Choices: []aiChatCompletionChoice{
			{
				Message: aiChatCompletionMessage{
					Role:    "assistant",
					Content: text,
				},
			},
		},
	}, nil
}

func (a *aiAgent) runtimeConfig(threadID int64, llmThreadID *int64, responderUserID string) (aiRuntimeConfig, error) {
	if responderUserID == systemUserID {
		return aiRuntimeConfig{
			APIKey:       strings.TrimSpace(a.apiKey),
			BaseURL:      strings.TrimSpace(a.baseURL),
			Model:        strings.TrimSpace(a.model),
			SystemPrompt: strings.TrimSpace(a.systemPrompt),
			Streaming:    a.streaming,
		}, nil
	}

	botUser, err := a.server.getBotUserByUserID(responderUserID)
	if err != nil {
		return aiRuntimeConfig{}, err
	}
	if botUser == nil {
		return aiRuntimeConfig{}, fmt.Errorf("bot config not found for %s", responderUserID)
	}

	// Passthrough bots have no LLM config — the polar-agent's
	// local tool is the runtime. Return an empty aiRuntimeConfig
	// (with the bot's system prompt, so the tool-loop fallback
	// still gets a useful prompt prefix when an agent IS attached
	// and we accidentally enter the LLM path). generateReply
	// detects the empty config + no agent case and returns a
	// helpful "attach polar-agent first" message instead of
	// dispatching a real LLM call.
	if botUser.BotKind == BotKindPassthrough {
		return aiRuntimeConfig{
			SystemPrompt: strings.TrimSpace(botUser.SystemPrompt),
		}, nil
	}

	var (
		item   *LLMConfig
		apiKey string
	)
	if llmThreadID != nil && *llmThreadID > 0 {
		item, apiKey, err = a.server.getLLMConfigForThread(threadID, *llmThreadID, responderUserID)
		if err != nil {
			return aiRuntimeConfig{}, err
		}
		if item == nil {
			return aiRuntimeConfig{}, fmt.Errorf("thread llm config not found for thread %d responder %s", *llmThreadID, responderUserID)
		}
	}
	// If the thread is bound to a project task, the project's
	// default LLM (set at AI 拆解 time) wins over the bot's bound
	// LLM. This keeps sub-task chats on the same model the user
	// chose when decomposing the project, instead of silently
	// flipping back to the workspace default. Pure chat threads
	// (not bound to any task) skip this path and fall through to
	// the bot's LLM as before.
	if item == nil {
		pi, pk, perr := a.server.getProjectDefaultLLMForChatThread(threadID)
		if perr == nil && pi != nil && strings.TrimSpace(pk) != "" {
			item = pi
			apiKey = pk
		}
	}
	if item == nil {
		item, apiKey, err = a.server.getLLMConfigForBot(responderUserID)
	}
	if err != nil {
		return aiRuntimeConfig{}, err
	}
	if item == nil {
		return aiRuntimeConfig{}, fmt.Errorf("bot llm config not found for %s", responderUserID)
	}
	systemPrompt := strings.TrimSpace(botUser.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = a.server.renderPrompt(promptSlugAIDefaultSystem, nil, "你是站内的 AI 助手，请基于上下文和当前问题，用清晰、可靠、结构化的中文回复。")
	}
	return aiRuntimeConfig{
		APIKey:        strings.TrimSpace(apiKey),
		BaseURL:       strings.TrimSpace(item.BaseURL),
		Model:         strings.TrimSpace(item.Model),
		SystemPrompt:  systemPrompt,
		ProxyURL:      strings.TrimSpace(item.ProxyURL),
		Streaming:     item.Streaming,
		LLMConfigID:   item.ID,
		LLMConfigName: strings.TrimSpace(item.Name),
	}, nil
}

func compactLogJSON(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err == nil {
		if normalized, err := json.Marshal(decoded); err == nil {
			text = string(normalized)
		}
	}

	const maxLogBytes = 4000
	if len(text) > maxLogBytes {
		return text[:maxLogBytes] + "...(truncated)"
	}
	return text
}

// errUpstreamOutOfCredit is a sentinel wrapped around upstream
// responses that look like the configured account is out of funds.
// Chat handlers test it via errors.Is and surface a clearer Chinese
// message ("provider 余额不足") instead of the generic
// "我暂时无法完成这次处理". DeepSeek returns 402 with this; xAI
// returns 403 + "team out of credits" — see classifyUpstreamCreditError.
var errUpstreamOutOfCredit = errors.New("upstream LLM provider out of credit")

func classifyUpstreamCreditError(status int, body []byte, fallback error) error {
	if fallback == nil {
		return nil
	}
	if status == http.StatusPaymentRequired {
		return fmt.Errorf("%w: %s", errUpstreamOutOfCredit, fallback.Error())
	}
	if status == http.StatusForbidden {
		b := strings.ToLower(string(body))
		if strings.Contains(b, "balance") || strings.Contains(b, "credit") ||
			strings.Contains(b, "billing") || strings.Contains(b, "insufficient") ||
			strings.Contains(b, "余额") || strings.Contains(b, "充值") {
			return fmt.Errorf("%w: %s", errUpstreamOutOfCredit, fallback.Error())
		}
	}
	return fallback
}

// userFriendlyChatErrorBody picks the user-visible chat bubble text
// for a failed LLM call. Default: the generic "暂时无法…" line that
// shipped pre-B5. When the underlying error is errUpstreamOutOfCredit
// (deepseek 402 / xai 403 "out of credits") surface a clearer message
// pointing at the provider account, not at "system problem".
func userFriendlyChatErrorBody(err error) string {
	if errors.Is(err, errUpstreamOutOfCredit) {
		return "上游 AI 提供商账户余额不足，请联系管理员充值或切换其他 LLM。"
	}
	return "我暂时无法完成这次处理，请稍后再试。"
}

func parseAIErrorMessage(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}

	var asObject struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		if strings.TrimSpace(asObject.Message) != "" {
			return strings.TrimSpace(asObject.Message)
		}
		if strings.TrimSpace(asObject.Error) != "" {
			return strings.TrimSpace(asObject.Error)
		}
	}

	return strings.TrimSpace(string(raw))
}

func detectAIProvider(baseURL string) aiProvider {
	text := strings.ToLower(strings.TrimSpace(baseURL))
	if strings.Contains(text, "api.anthropic.com") {
		return aiProviderAnthropic
	}
	if strings.Contains(text, "generativelanguage.googleapis.com") || strings.Contains(text, "googleapis.com") && strings.Contains(text, "generatecontent") {
		return aiProviderGemini
	}
	if strings.Contains(text, "api.x.ai") {
		return aiProviderXAIResponses
	}
	if strings.Contains(text, "api.miromind.ai") {
		return aiProviderMiroMind
	}
	// Volcano Engine (字节豆包) exposes both Chat Completions
	// (/api/v3/chat/completions) and Responses API
	// (/api/v3/responses). The latter rejects {"messages": [...]}
	// payload with `unknown field "messages"` since it expects
	// {"input": [...]} like OpenAI's Responses API.
	if strings.Contains(text, "volces.com") && strings.Contains(text, "/responses") {
		return aiProviderVolcesResponses
	}
	return aiProviderOpenAICompatible
}

func buildGeminiRequestURL(baseURL, model, apiKey string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	model = strings.TrimSpace(model)
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" || model == "" || apiKey == "" {
		return "", errors.New("Gemini 配置缺少 Base URL、Model 或 API Key")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.Contains(path, ":generateContent"):
		if idx := strings.Index(path, "/models/"); idx >= 0 {
			prefix := path[:idx+len("/models/")]
			suffix := path[strings.Index(path, ":generateContent"):]
			path = prefix + url.PathEscape(model) + suffix
		}
	case strings.HasSuffix(path, "/models"):
		path = path + "/" + url.PathEscape(model) + ":generateContent"
	case strings.Contains(path, "/models/"):
		if !strings.HasSuffix(path, "/"+model) {
			path = path + "/" + url.PathEscape(model)
		}
		path = path + ":generateContent"
	default:
		path = path + "/models/" + url.PathEscape(model) + ":generateContent"
	}
	parsed.Path = path

	query := parsed.Query()
	if query.Get("key") == "" {
		query.Set("key", apiKey)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func buildXAIResponsesRequestURL(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", errors.New("xAI 配置缺少 Base URL")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == "":
		path = "/v1/responses"
	case strings.HasSuffix(path, "/responses"):
		// Already points to the responses endpoint.
	case strings.HasSuffix(path, "/v1"):
		path = path + "/responses"
	default:
		path = path + "/responses"
	}
	parsed.Path = path
	return parsed.String(), nil
}

func buildAnthropicMessagesRequestURL(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", errors.New("Claude 配置缺少 Base URL")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == "":
		path = "/v1/messages"
	case strings.HasSuffix(path, "/messages"):
		// already points to messages endpoint
	case strings.HasSuffix(path, "/v1"):
		path = path + "/messages"
	default:
		path = path + "/messages"
	}
	parsed.Path = path
	return parsed.String(), nil
}

func convertToGeminiRequest(payload aiChatCompletionRequest) geminiGenerateContentRequest {
	req := geminiGenerateContentRequest{
		Contents: make([]geminiContent, 0, len(payload.Messages)),
	}

	var systemTexts []string
	for _, msg := range payload.Messages {
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			continue
		}
		switch strings.TrimSpace(msg.Role) {
		case "system":
			systemTexts = append(systemTexts, text)
		case "assistant", "model":
			req.Contents = append(req.Contents, geminiContent{
				Role: "model",
				Parts: []geminiPart{
					{Text: text},
				},
			})
		default:
			req.Contents = append(req.Contents, geminiContent{
				Role: "user",
				Parts: []geminiPart{
					{Text: text},
				},
			})
		}
	}

	if len(systemTexts) > 0 {
		req.SystemInstruction = &geminiContent{
			Parts: []geminiPart{
				{Text: strings.Join(systemTexts, "\n\n")},
			},
		}
	}
	return req
}

func convertToXAIResponsesRequest(payload aiChatCompletionRequest) xAIResponsesRequest {
	req := xAIResponsesRequest{
		Model: payload.Model,
		Input: make([]xAIResponsesMessage, 0, len(payload.Messages)),
		Tools: []xAIResponsesToolSpec{
			{Type: "web_search"},
		},
	}

	for _, msg := range payload.Messages {
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			continue
		}

		role := strings.TrimSpace(msg.Role)
		switch role {
		case "system", "assistant", "user":
		default:
			role = "user"
		}

		req.Input = append(req.Input, xAIResponsesMessage{
			Role:    role,
			Content: text,
		})
	}

	return req
}

func convertToAnthropicMessagesRequest(payload aiChatCompletionRequest) anthropicMessagesRequest {
	req := anthropicMessagesRequest{
		Model:     payload.Model,
		Messages:  make([]anthropicMessagesMessage, 0, len(payload.Messages)),
		MaxTokens: 4096,
	}

	var systemParts []string
	for _, msg := range payload.Messages {
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			continue
		}
		switch strings.TrimSpace(msg.Role) {
		case "system":
			systemParts = append(systemParts, text)
		case "assistant":
			req.Messages = append(req.Messages, anthropicMessagesMessage{
				Role:    "assistant",
				Content: text,
			})
		default:
			req.Messages = append(req.Messages, anthropicMessagesMessage{
				Role:    "user",
				Content: text,
			})
		}
	}

	if len(systemParts) > 0 {
		req.System = strings.Join(systemParts, "\n\n")
	}
	return req
}

func extractGeminiResponseText(result geminiGenerateContentResponse) string {
	if len(result.Candidates) == 0 {
		return ""
	}

	var parts []string
	for _, part := range result.Candidates[0].Content.Parts {
		text := strings.TrimSpace(part.Text)
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func extractXAIResponseText(result xAIResponsesResponse) string {
	if text := strings.TrimSpace(result.OutputText); text != "" {
		return text
	}

	var parts []string
	for _, item := range result.Output {
		if text := strings.TrimSpace(item.Text); text != "" {
			parts = append(parts, text)
		}
		for _, content := range item.Content {
			if text := strings.TrimSpace(content.Text); text != "" {
				parts = append(parts, text)
			}
		}
	}

	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func extractAnthropicResponseText(result anthropicMessagesResponse) string {
	var parts []string
	for _, item := range result.Content {
		text := strings.TrimSpace(item.Text)
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func (a *aiAgent) buildContext(threadID int64, llmThreadID *int64) (string, error) {
	var parts []string
	if llmThreadID == nil {
		parts = append(parts, "以下是程序运行目录中的文档摘要和当前私聊上下文。")

		docText, err := a.loadRuntimeDocuments()
		if err != nil {
			parts = append(parts, "文档读取失败："+err.Error())
		} else if docText != "" {
			parts = append(parts, docText)
		}
	} else {
		parts = append(parts, "以下是当前话题的私聊上下文。")
	}

	messages, err := a.server.listRecentChatMessages(threadID, llmThreadID, 12)
	if err != nil {
		return strings.Join(parts, "\n\n"), err
	}
	if len(messages) > 0 {
		var b strings.Builder
		b.WriteString("最近会话消息：\n")
		for _, msg := range messages {
			if msg.Deleted {
				continue
			}
			name := msg.SenderUsername
			if name == "" {
				name = msg.SenderID
			}
			b.WriteString("- ")
			b.WriteString(name)
			b.WriteString(": ")
			b.WriteString(strings.TrimSpace(msg.Content))
			b.WriteString("\n")
		}
		parts = append(parts, b.String())
	}

	return strings.Join(parts, "\n\n"), nil
}

func (a *aiAgent) loadRuntimeDocuments() (string, error) {
	root := strings.TrimSpace(a.server.workDir)
	if root == "" {
		return "", nil
	}

	maxFiles := 8
	maxBytes := 24 * 1024
	used := 0
	collected := 0
	var b strings.Builder

	walkFn := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "dist" || base == ".gocache" || base == "data" {
				return filepath.SkipDir
			}
			return nil
		}
		if collected >= maxFiles || used >= maxBytes {
			return fs.SkipAll
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".md" && ext != ".txt" {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		text := strings.TrimSpace(string(content))
		if text == "" {
			return nil
		}
		remaining := maxBytes - used
		if remaining <= 0 {
			return fs.SkipAll
		}
		if len(text) > remaining {
			text = text[:remaining]
		}
		b.WriteString("文件：")
		b.WriteString(rel)
		b.WriteString("\n")
		b.WriteString(text)
		b.WriteString("\n\n")
		used += len(text)
		collected++
		return nil
	}

	if err := filepath.WalkDir(root, walkFn); err != nil && !errors.Is(err, fs.SkipAll) {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}
