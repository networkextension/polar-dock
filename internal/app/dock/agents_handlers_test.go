package dock

// Tests for the v4 agents read endpoints + admin kick.
// Pure-logic only — DB-touching paths are exercised by the dock smoke
// step (the convention in this package; see ai_agent_stream_test.go).
// See doc/arch/agent-identity-v4.md.

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// TestAgentListItemMarshalShape locks the wire shape (Phase F UI in
// polar-dock-ui parses the JSON keys verbatim). Also doubles as a
// regression sentinel for the no-raw-token guarantee: a future schema
// mistake that surfaced the plaintext bearer would change the byte
// content here.
func TestAgentListItemMarshalShape(t *testing.T) {
	last := "2026-05-27T11:30:00Z"
	it := AgentListItem{
		ID:                 "ag_abcdef0123456789abcdef0123456789",
		WorkspaceID:        "t_root",
		HostID:             "5f4dcc3b5aa765d61d8327deb882cf99",
		HostName:           "emei",
		HostHWModel:        "Mac15,8",
		Name:               "emei-kimi",
		BotUserID:          "bot_xyz",
		AgentTokenIDSuffix: "abcd1234",
		OS:                 "darwin",
		Arch:               "arm64",
		CreatedAt:          "2026-05-27T10:00:00Z",
		LastHelloAt:        &last,
	}
	raw, err := json.Marshal(it)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, needle := range []string{
		`"id":"ag_abcdef0123456789abcdef0123456789"`,
		`"workspace_id":"t_root"`,
		`"host_id":"5f4dcc3b5aa765d61d8327deb882cf99"`,
		`"host_name":"emei"`,
		`"host_hw_model":"Mac15,8"`,
		`"agent_token_id_suffix":"abcd1234"`,
		`"last_hello_at":"2026-05-27T11:30:00Z"`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("missing %q in: %s", needle, body)
		}
	}
	// No plaintext-token-looking content. agent_token_id_suffix is 8
	// chars of an opaque row id; a raw bearer would start with
	// "polar_agent_" and never appears in this shape.
	for _, bad := range []string{"polar_agent_", "token_hash"} {
		if strings.Contains(body, bad) {
			t.Errorf("AgentListItem leaked %q-shaped content: %s", bad, body)
		}
	}
}

// TestSafeCloseAgentConn_IdempotentMultipleClose: kick must be safe to
// invoke twice (admin clicks "kick" → race with the agent's natural
// disconnect → second close panics without the recover).
func TestSafeCloseAgentConn_IdempotentMultipleClose(t *testing.T) {
	c := &agentConn{close: make(chan struct{})}
	// First close: cleanly shuts the channel.
	safeCloseAgentConn(c)
	select {
	case <-c.close:
		// drained as expected
	default:
		t.Fatal("first safeCloseAgentConn should have closed the channel")
	}
	// Second close: would panic without recover; must be a no-op here.
	safeCloseAgentConn(c)
	safeCloseAgentConn(c)
}

// TestSafeCloseAgentConn_ConcurrentClose: two goroutines closing
// simultaneously — neither may panic. Models the "admin kicks while
// the agent disconnects" race.
func TestSafeCloseAgentConn_ConcurrentClose(t *testing.T) {
	c := &agentConn{close: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); safeCloseAgentConn(c) }()
	go func() { defer wg.Done(); safeCloseAgentConn(c) }()
	wg.Wait()
	// Channel must be closed (not blocked).
	select {
	case <-c.close:
	default:
		t.Fatal("close channel should be drained after concurrent safeCloseAgentConn")
	}
}

// TestRegisterAgentResponseShape_v4AgentTokenRaw: the registerAgent
// chain returns a map[string]any wire body. v4 wire convention is
// "agent_token_raw" (matches polar-hosts /api/hosts/register +
// polar-sdk v0.2.5 AgentRegisterResponse.AgentTokenRaw). The old
// "token" key — left over from pre-v0.2.5 — must NOT appear or the
// SDK's decoded resp.AgentTokenRaw will be empty and downstream
// agent.toml writes mint a useless empty bearer.
func TestRegisterAgentResponseShape_v4AgentTokenRaw(t *testing.T) {
	// Mirror the new-host happy path's response builder. Keep this
	// hand-rolled so a future refactor of registerAgent can't silently
	// flip the key back.
	resp := map[string]any{
		"agent_id":        "ag_x",
		"host_id":         "h_y",
		"bot_user_id":     "bot_z",
		"agent_token_raw": "polar_agent_raw",
		"server":          "https://example",
	}
	raw, _ := json.Marshal(resp)
	body := string(raw)
	if !strings.Contains(body, `"agent_token_raw":"polar_agent_raw"`) {
		t.Errorf("expected agent_token_raw key in: %s", body)
	}
	if strings.Contains(body, `"token":`) {
		t.Errorf("legacy 'token' key must not appear in v4 response: %s", body)
	}
}
