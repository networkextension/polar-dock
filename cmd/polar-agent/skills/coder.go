package skills

// Coder skill — wraps polar-agent's existing per-tool dispatch
// (kimi / claude / codex / operator-defined tools.json entries) as
// a Skill so dock-side UI can render which coding tools each host
// makes available without inspecting the agent's CLI flags.
//
// P1a (this file): Skill metadata only. Capabilities() reports the
// resolved tool list + the active mode (tool-loop vs passthrough);
// Start() returns ErrNotImplemented because runtime dispatch still
// flows through loop.go's chat_message / tool_call branches. P1b
// will wire Start() to invokeTool so dock can dispatch skill.start
// envelopes and replace the legacy attach path.

import (
	"context"
	"encoding/json"
	"errors"
)

// CoderConfig is the operator-supplied blob for a host_skills row of
// kind=coder. Dock surfaces these via the skill config form in
// /hosts/:id; agent receives the same JSON inside skill.start at
// dispatch time. Phase P1a only uses Mode + Tool + Tools for the
// advertise payload; the rest land in P1b.
type CoderConfig struct {
	// Mode: "passthrough" (one specific tool, agent was started with
	// --tool=X) or "tool-loop" (agent supports all configured tools,
	// dock picks per message).
	Mode string `json:"mode"`
	// Tool: the single tool name when Mode=="passthrough". Empty in
	// tool-loop mode.
	Tool string `json:"tool,omitempty"`
	// Tools: the full set of tool names this host can dispatch.
	// Always populated so the UI can show "this host has kimi +
	// claude + codex available" regardless of how the operator
	// started the agent.
	Tools []string `json:"tools"`
	// Workdir: absolute path the agent pinned at startup (the
	// directory tools cwd into for code edits). Surface-only for now.
	Workdir string `json:"workdir,omitempty"`
}

// ErrCoderStartNotImplemented signals that P1a hasn't wired the
// dispatch path yet. Runtime keeps flowing through the legacy
// chat_message branch in loop.go; this is the explicit "do not
// dispatch skill.start to me yet" sentinel that dock can check
// against if it ever tries.
var ErrCoderStartNotImplemented = errors.New("coder skill: skill.start dispatch not yet implemented in P1a — use chat_message path")

type coderSkill struct {
	config CoderConfig
}

// NewCoderSkill constructs a Coder skill instance the agent will
// register at startup. Caller passes the runtime tool list (resolved
// from tools.json + builtins) plus the active mode so Capabilities()
// reports the truth, not a hardcoded default.
func NewCoderSkill(cfg CoderConfig) Skill {
	if cfg.Mode == "" {
		cfg.Mode = "tool-loop"
	}
	if cfg.Tools == nil {
		cfg.Tools = []string{}
	}
	return &coderSkill{config: cfg}
}

func (c *coderSkill) Kind() SkillKind { return KindCoder }

func (c *coderSkill) Version() string { return "1.0" }

func (c *coderSkill) Capabilities() map[string]any {
	out := map[string]any{
		"mode":  c.config.Mode,
		"tools": c.config.Tools,
	}
	if c.config.Tool != "" {
		out["tool"] = c.config.Tool
	}
	if c.config.Workdir != "" {
		out["workdir"] = c.config.Workdir
	}
	return out
}

func (c *coderSkill) Validate(config json.RawMessage) error {
	if len(config) == 0 || string(config) == "null" {
		return nil
	}
	var probe CoderConfig
	if err := json.Unmarshal(config, &probe); err != nil {
		return err
	}
	return nil
}

func (c *coderSkill) Start(ctx context.Context, runID int64, config json.RawMessage) (Run, error) {
	return nil, ErrCoderStartNotImplemented
}
