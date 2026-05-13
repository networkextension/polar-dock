package skills

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestCoderSkillKindAndVersion(t *testing.T) {
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}})
	if got := s.Kind(); got != KindCoder {
		t.Fatalf("Kind: got %q, want %q", got, KindCoder)
	}
	if s.Version() == "" {
		t.Fatal("Version: empty")
	}
}

func TestCoderSkillCapabilitiesToolLoopMode(t *testing.T) {
	s := NewCoderSkill(CoderConfig{
		Mode:    "tool-loop",
		Tools:   []string{"kimi", "claude", "codex"},
		Workdir: "/home/local/repo",
	})
	caps := s.Capabilities()
	if caps["mode"] != "tool-loop" {
		t.Errorf("mode: got %v, want tool-loop", caps["mode"])
	}
	tools, ok := caps["tools"].([]string)
	if !ok || len(tools) != 3 {
		t.Errorf("tools: got %v, want [kimi claude codex]", caps["tools"])
	}
	if caps["workdir"] != "/home/local/repo" {
		t.Errorf("workdir: got %v", caps["workdir"])
	}
	if _, present := caps["tool"]; present {
		t.Errorf("tool key should be absent in tool-loop mode, got %v", caps["tool"])
	}
}

func TestCoderSkillCapabilitiesPassthroughMode(t *testing.T) {
	s := NewCoderSkill(CoderConfig{
		Mode:  "passthrough",
		Tool:  "kimi",
		Tools: []string{"kimi", "claude", "codex"},
	})
	caps := s.Capabilities()
	if caps["mode"] != "passthrough" {
		t.Errorf("mode: got %v, want passthrough", caps["mode"])
	}
	if caps["tool"] != "kimi" {
		t.Errorf("tool: got %v, want kimi", caps["tool"])
	}
}

func TestCoderSkillDefaultsMode(t *testing.T) {
	// Empty Mode falls back to "tool-loop" so the agent main()
	// doesn't have to know the convention.
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}})
	if mode := s.Capabilities()["mode"]; mode != "tool-loop" {
		t.Errorf("default mode: got %v, want tool-loop", mode)
	}
}

func TestCoderSkillValidateAcceptsEmpty(t *testing.T) {
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}})
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage("{}")} {
		if err := s.Validate(raw); err != nil {
			t.Errorf("Validate(%q): %v, want nil", string(raw), err)
		}
	}
}

func TestCoderSkillValidateRejectsBadJSON(t *testing.T) {
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}})
	if err := s.Validate(json.RawMessage("{not-json")); err == nil {
		t.Fatal("Validate: want error on malformed JSON, got nil")
	}
}

func TestCoderSkillStartReturnsNotImplemented(t *testing.T) {
	s := NewCoderSkill(CoderConfig{Tools: []string{"kimi"}})
	run, err := s.Start(context.Background(), 1, json.RawMessage(`{"tool":"kimi"}`))
	if run != nil {
		t.Errorf("Start: got Run %v, want nil", run)
	}
	if !errors.Is(err, ErrCoderStartNotImplemented) {
		t.Errorf("Start: got err %v, want ErrCoderStartNotImplemented", err)
	}
}

func TestCoderSkillRegisterAdvertised(t *testing.T) {
	// Use an isolated registry — don't pollute the package-level
	// default registry (other tests in the agent's main package may
	// use Default()).
	r := NewRegistry()
	skill := NewCoderSkill(CoderConfig{
		Mode:  "passthrough",
		Tool:  "kimi",
		Tools: []string{"kimi"},
	})
	r.Register(skill)

	got := r.Advertised()
	if len(got) != 1 {
		t.Fatalf("Advertised: got %d entries, want 1", len(got))
	}
	if got[0].Kind != string(KindCoder) {
		t.Errorf("Advertised[0].Kind: got %q, want %q", got[0].Kind, KindCoder)
	}
	if got[0].Capabilities["mode"] != "passthrough" {
		t.Errorf("Advertised[0].Capabilities.mode: got %v", got[0].Capabilities["mode"])
	}
	if got[0].Capabilities["tool"] != "kimi" {
		t.Errorf("Advertised[0].Capabilities.tool: got %v", got[0].Capabilities["tool"])
	}
}
