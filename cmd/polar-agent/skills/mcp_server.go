package skills

// MCP-server skill (Host module P3, P0a slice — protocol scaffold +
// built-in echo tool). See doc/mcp-skill-design.md (the "why") and
// doc/mcp-skill-dev.md (the phase plan).
//
// Wire shape: byte-stream skill, same as shell — operator-side stdin
// is line-delimited JSON-RPC 2.0 (per MCP spec); EventStdout emits
// the JSON-RPC responses. No TTY semantics (no resize). This means
// the existing test-serve / WS bridge / supervisor all work
// unchanged — JSON-RPC framing is just bytes from the transport's
// perspective.
//
// What's implemented in P0a:
//   - `initialize`     — handshake, returns server capabilities
//   - `tools/list`     — enumerate registered tools (just `echo` for now)
//   - `tools/call`     — dispatch to tool; echo returns its `message` arg
//   - Standard JSON-RPC error returns on bad method / params / parse
//
// What's NOT in P0a (deferred to P0b+):
//   - External adapters (gdb-mi, lldb-dap, ida-rpc, frida, radare2)
//   - Persistent sessions across tool calls
//   - resources/* / prompts/* methods
//   - Notifications (server-pushed)
//   - The `github.com/modelcontextprotocol/go-sdk` integration — we
//     hand-roll the 3 RPC methods to keep P0a dependency-free; swap
//     in later PR when the adapter surface grows.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	mcpProtocolVersion = "2024-11-05"
	mcpServerName      = "polar-mcp-server"
	mcpServerVersion   = "0.1-p0a"
	mcpStdoutChunkCap  = 64 << 10
	mcpDefaultIdleSec  = 1800
	mcpHardCapSec      = 21600
)

// MCPConfig is the per-run config inside skill.start. P0a has no
// configurable options — adapters list lands in P0b. The struct exists
// so future fields can be added without breaking the wire.
type MCPConfig struct {
	// Adapters: list of adapter names to load. Ignored in P0a (only
	// the built-in echo tool is available). Will be honored in P0b
	// when the gdb-mi adapter lands.
	Adapters []string `json:"adapters,omitempty"`
}

type mcpSkill struct{}

// NewMCPServerSkill returns the MCP server skill instance. Single
// global server per agent — multiple skill.start calls each get their
// own Run, but tools are shared (stateless in P0a; per-session state
// lands in P0c).
func NewMCPServerSkill() Skill { return &mcpSkill{} }

func (m *mcpSkill) Kind() SkillKind { return KindMCPServer }
func (m *mcpSkill) Version() string { return mcpServerVersion }

func (m *mcpSkill) Capabilities() map[string]any {
	return map[string]any{
		"protocol":         "jsonrpc-ndjson",
		"mcp_version":      mcpProtocolVersion,
		"tools":            mcpBuiltinToolNames(),
		"default_idle_sec": mcpDefaultIdleSec,
		"hard_cap_sec":     mcpHardCapSec,
	}
}

func (m *mcpSkill) Validate(config json.RawMessage) error {
	if len(config) == 0 || string(config) == "null" {
		return nil
	}
	var cfg MCPConfig
	return json.Unmarshal(config, &cfg)
}

func (m *mcpSkill) Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error) {
	var cfg MCPConfig
	if len(config) > 0 && string(config) != "null" {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("mcp: parse config: %w", err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &mcpRun{
		id:        runID,
		cfg:       cfg,
		events:    make(chan Event, 64),
		ctxCancel: cancel,
		inBuf:     &bytes.Buffer{},
		startedAt: time.Now(),
	}
	run.lastActivity.Store(time.Now().UnixNano())
	run.send(Event{Kind: EventState, Data: map[string]any{
		"status":       "running",
		"mcp_version":  mcpProtocolVersion,
		"server_name":  mcpServerName,
		"tools":        mcpBuiltinToolNames(),
	}})
	go run.idleWatchdog(runCtx)
	return run, nil
}

// mcpRun is one MCP server session. JSON-RPC requests arrive via
// WriteInput (line-delimited NDJSON over the skill's stdin), responses
// stream out as EventStdout.
type mcpRun struct {
	id  int64
	cfg MCPConfig

	events    chan Event
	ctxCancel context.CancelFunc
	startedAt time.Time

	inMu  sync.Mutex
	inBuf *bytes.Buffer // accumulates partial-line input across WriteInput calls

	lastActivity atomic.Int64
	stopOnce     sync.Once
	closed       atomic.Bool
}

func (r *mcpRun) ID() int64                { return r.id }
func (r *mcpRun) Events() <-chan Event     { return r.events }
func (r *mcpRun) Stop(reason string) error { return r.stop(reason) }

// Resize is a no-op for MCP — JSON-RPC has no terminal size concept.
func (r *mcpRun) Resize(rows, cols uint16) error { return nil }

// WriteInput accumulates bytes and dispatches complete JSON-RPC lines.
// Multiple requests can arrive in one WriteInput call; partial trailers
// are kept until the next call. Matches NDJSON conventions.
func (r *mcpRun) WriteInput(data []byte) error {
	if r.closed.Load() {
		return errors.New("mcp run closed")
	}
	r.lastActivity.Store(time.Now().UnixNano())
	r.inMu.Lock()
	r.inBuf.Write(data)
	scanner := bufio.NewScanner(bytes.NewReader(r.inBuf.Bytes()))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var consumed int
	var lines [][]byte
	for scanner.Scan() {
		raw := append([]byte(nil), scanner.Bytes()...)
		// Track consumed byte count: scanner gives us trimmed lines, so
		// we need to seek through inBuf to find the newline boundary.
		idx := bytes.IndexByte(r.inBuf.Bytes()[consumed:], '\n')
		if idx < 0 {
			// Scanner returned a final partial line; rewind so we keep
			// buffering until the trailing newline.
			break
		}
		consumed += idx + 1
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		lines = append(lines, raw)
	}
	// Drop consumed bytes, keep the (possibly empty) partial trailer.
	if consumed > 0 {
		trailer := append([]byte(nil), r.inBuf.Bytes()[consumed:]...)
		r.inBuf.Reset()
		r.inBuf.Write(trailer)
	}
	r.inMu.Unlock()

	for _, line := range lines {
		r.dispatchLine(line)
	}
	return nil
}

// dispatchLine parses one JSON-RPC request line and replies (request
// or notification). Per JSON-RPC 2.0: requests with `id` get a reply,
// notifications (no id) don't.
func (r *mcpRun) dispatchLine(line []byte) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		r.replyError(nil, -32700, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		r.replyError(req.ID, -32600, `"jsonrpc" must be "2.0"`)
		return
	}
	isNotif := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		if isNotif {
			return // ignore notification form
		}
		var params struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ClientInfo      map[string]any `json:"clientInfo"`
		}
		_ = json.Unmarshal(req.Params, &params)
		r.replyResult(req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    mcpServerName,
				"version": mcpServerVersion,
			},
		})
	case "notifications/initialized":
		// Notification, no reply. Could update state here later.
	case "tools/list":
		if isNotif {
			return
		}
		r.replyResult(req.ID, map[string]any{
			"tools": mcpBuiltinToolDefs(),
		})
	case "tools/call":
		if isNotif {
			return
		}
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			r.replyError(req.ID, -32602, "invalid params: "+err.Error())
			return
		}
		tool, ok := mcpBuiltinToolByName(p.Name)
		if !ok {
			r.replyError(req.ID, -32601, "unknown tool: "+p.Name)
			return
		}
		result, callErr := tool.Call(p.Arguments)
		if callErr != nil {
			r.replyError(req.ID, -32000, callErr.Error())
			return
		}
		r.replyResult(req.ID, result)
	default:
		if !isNotif {
			r.replyError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (r *mcpRun) replyResult(id json.RawMessage, result any) {
	frame := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	}
	r.emitJSON(frame)
}

func (r *mcpRun) replyError(id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	frame := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	r.emitJSON(frame)
}

func (r *mcpRun) emitJSON(frame map[string]any) {
	b, err := json.Marshal(frame)
	if err != nil {
		log.Printf("[mcp] run=%d marshal: %v", r.id, err)
		return
	}
	// NDJSON: every reply gets a trailing newline.
	b = append(b, '\n')
	r.send(Event{Kind: EventStdout, Data: map[string]any{
		"bytes_b64": base64.StdEncoding.EncodeToString(b),
	}})
}

func (r *mcpRun) idleWatchdog(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if r.closed.Load() {
				return
			}
			last := time.Unix(0, r.lastActivity.Load())
			if now.Sub(last) >= mcpDefaultIdleSec*time.Second {
				r.stop("idle")
				return
			}
			if now.Sub(r.startedAt) >= mcpHardCapSec*time.Second {
				r.stop("hard_cap")
				return
			}
		}
	}
}

func (r *mcpRun) stop(reason string) error {
	r.stopOnce.Do(func() {
		r.closed.Store(true)
		r.send(Event{Kind: EventExit, Data: map[string]any{
			"ok":     reason == "operator" || reason == "idle" || reason == "hard_cap",
			"reason": reason,
		}})
		r.ctxCancel()
		close(r.events)
	})
	return nil
}

func (r *mcpRun) send(ev Event) {
	defer func() { _ = recover() }()
	select {
	case r.events <- ev:
	default:
		if ev.Kind == EventStdout {
			return
		}
		select {
		case r.events <- ev:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// ---- built-in tools registry ----------------------------------------

type mcpTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Call        func(args json.RawMessage) (map[string]any, error)
}

// mcpBuiltinTools is the P0a tool set — just `echo`. Adapter-backed
// tools (gdb.run, gdb.read_register, lldb.*, ida.*) land in P0b+.
var mcpBuiltinTools = []mcpTool{
	{
		Name:        "echo",
		Description: "Echo the input message back. Used to validate the MCP transport end-to-end without any adapter dependency.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{
					"type":        "string",
					"description": "The text to echo back.",
				},
			},
			"required": []string{"message"},
		},
		Call: func(args json.RawMessage) (map[string]any, error) {
			var p struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			return map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": p.Message},
				},
			}, nil
		},
	},
}

func mcpBuiltinToolNames() []string {
	out := make([]string, len(mcpBuiltinTools))
	for i, t := range mcpBuiltinTools {
		out[i] = t.Name
	}
	return out
}

func mcpBuiltinToolDefs() []map[string]any {
	out := make([]map[string]any, len(mcpBuiltinTools))
	for i, t := range mcpBuiltinTools {
		out[i] = map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		}
	}
	return out
}

func mcpBuiltinToolByName(name string) (*mcpTool, bool) {
	for i := range mcpBuiltinTools {
		if mcpBuiltinTools[i].Name == name {
			return &mcpBuiltinTools[i], true
		}
	}
	return nil, false
}
