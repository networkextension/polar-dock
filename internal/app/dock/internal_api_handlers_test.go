package dock

// Pure tests for the Phase 1-B handlers that don't need DB/Redis.
// Anything that touches sessions / users / teams / LLM is exercised
// in the manual smoke procedure documented in the PR description.
// The bits worth fast-feedback unit coverage are: error JSON shape,
// request-body validation, and the heartbeat name-mismatch guard.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestInternalErrShape(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	internalErr(c, http.StatusBadRequest, "boom")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if body["error"] != "boom" {
		t.Fatalf("body error: got %q want boom", body["error"])
	}
}

func TestHandleInternalHeartbeat_NameMismatch(t *testing.T) {
	// Auth middleware sets plugin_name=wg; payload claims to be iosdist.
	// Handler must return 403 before touching the DB so a stolen key
	// can't pivot to another plugin's row.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("plugin_name", "wg")
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/plugin-registry/heartbeat",
		strings.NewReader(`{"name":"iosdist","version":"0.1.0"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	(&Server{}).handleInternalHeartbeat(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "does not match") {
		t.Fatalf("body: got %q want substring 'does not match'", w.Body.String())
	}
}

func TestHandleInternalHeartbeat_BadJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("plugin_name", "wg")
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/plugin-registry/heartbeat",
		strings.NewReader(`not json`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	(&Server{}).handleInternalHeartbeat(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleInternalNotify_MissingUser(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/notify",
		strings.NewReader(`{"title":"x"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	(&Server{}).handleInternalNotify(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleInternalAuthVerify_MissingToken(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/internal/v1/auth/verify", nil)

	(&Server{}).handleInternalAuthVerify(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleInternalLLMConfigGet_MissingParams(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{gin.Param{Key: "id", Value: ""}}
	c.Request = httptest.NewRequest(http.MethodGet, "/internal/v1/llm-configs/", nil)

	(&Server{}).handleInternalLLMConfigGet(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

// Task #216 — internal token/host issue endpoints. Validation-only
// tests (DB rows exercised via the polar-hosts integration smoke step).

func TestHandleInternalAgentTokenIssue_BadJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/agent-tokens/issue",
		strings.NewReader(`not json`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	(&Server{}).handleInternalAgentTokenIssue(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleInternalAgentTokenIssue_MissingRequired(t *testing.T) {
	// id, user_id, token_hash are required — missing any should 400.
	cases := []string{
		`{}`,
		`{"id":"tok_x"}`,
		`{"id":"tok_x","user_id":"u_x"}`,
		`{"user_id":"u_x","token_hash":"h"}`,
		`{"id":"tok_x","token_hash":"h"}`,
	}
	for _, body := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(
			http.MethodPost,
			"/internal/v1/agent-tokens/issue",
			strings.NewReader(body),
		)
		c.Request.Header.Set("Content-Type", "application/json")
		(&Server{}).handleInternalAgentTokenIssue(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status got %d want 400", body, w.Code)
		}
	}
}

func TestHandleInternalHostIssue_BadJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/hosts/issue",
		strings.NewReader(`not json`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	(&Server{}).handleInternalHostIssue(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleInternalHostIssue_MissingRequired(t *testing.T) {
	cases := []string{
		`{}`,
		`{"id":"h_x"}`,
		`{"id":"h_x","workspace_id":"t_x"}`,
		`{"id":"h_x","workspace_id":"t_x","slug":"s"}`, // missing name
		`{"workspace_id":"t_x","slug":"s","name":"n"}`, // missing id
	}
	for _, body := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(
			http.MethodPost,
			"/internal/v1/hosts/issue",
			strings.NewReader(body),
		)
		c.Request.Header.Set("Content-Type", "application/json")
		(&Server{}).handleInternalHostIssue(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status got %d want 400", body, w.Code)
		}
	}
}

func TestInternalHostIssueReq_MachineUUIDFieldDecodes(t *testing.T) {
	// Lock the wire shape so the polar-sdk side and the dock handler
	// agree on the JSON tag for machine_uuid. A bad refactor that
	// renamed the field would silently drop the dedup payload — this
	// test catches it without needing the DB / a live plugin.
	body := `{
	  "id":"h_xyz","workspace_id":"t_root","slug":"emei","name":"emei",
	  "agent_token_id":"tok_abc","os":"darwin","arch":"arm64",
	  "machine_uuid":"12345678-90AB-CDEF-1234-567890ABCDEF"
	}`
	var req internalHostIssueReq
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.MachineUUID != "12345678-90AB-CDEF-1234-567890ABCDEF" {
		t.Errorf("MachineUUID = %q, want round-trip value", req.MachineUUID)
	}
	if req.ID != "h_xyz" || req.WorkspaceID != "t_root" || req.Name != "emei" {
		t.Errorf("required fields lost: %+v", req)
	}
}

func TestInternalHostIssueReq_MissingMachineUUIDIsOK(t *testing.T) {
	// Legacy / collector-failed agents omit machine_uuid entirely.
	// The handler must NOT require it (no binding tag) — empty value
	// signals "use the legacy id-conflict path" downstream.
	body := `{"id":"h_legacy","workspace_id":"t_root","slug":"old","name":"old"}`
	var req internalHostIssueReq
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.MachineUUID != "" {
		t.Errorf("MachineUUID should default to empty, got %q", req.MachineUUID)
	}
}

// ── v4 — agents/register + workspace-proxy-tokens/ensure + bots/create-for-agent ──

func TestHandleInternalAgentRegister_BadJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/agents/register",
		strings.NewReader(`not json`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	(&Server{}).handleInternalAgentRegister(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleInternalAgentRegister_MissingRequired(t *testing.T) {
	// v4 requires workspace_id + name + machine_uuid_raw. Missing any
	// → 400, gin binding rejects before touching the DB.
	cases := []string{
		`{}`,
		`{"name":"emei-kimi"}`,
		`{"name":"emei-kimi","workspace_id":"t_x"}`,
		`{"workspace_id":"t_x","machine_uuid_raw":"u"}`,
	}
	for _, body := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(
			http.MethodPost,
			"/internal/v1/agents/register",
			strings.NewReader(body),
		)
		c.Request.Header.Set("Content-Type", "application/json")
		(&Server{}).handleInternalAgentRegister(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status got %d want 400", body, w.Code)
		}
	}
}

func TestHandleInternalWorkspaceProxyTokenEnsure_MissingWorkspace(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/internal/v1/workspace-proxy-tokens/ensure",
		strings.NewReader(`{"name":"agent-pool"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	(&Server{}).handleInternalWorkspaceProxyTokenEnsure(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (missing workspace_id)", w.Code)
	}
}

func TestHandleInternalBotForAgent_MissingFields(t *testing.T) {
	cases := []string{
		`{}`,                            // missing both
		`{"workspace_id":"t_x"}`,        // missing name
		`{"name":"bot-emei-kimi-abc"}`,  // missing workspace_id
	}
	for _, body := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(
			http.MethodPost,
			"/internal/v1/bots/create-for-agent",
			strings.NewReader(body),
		)
		c.Request.Header.Set("Content-Type", "application/json")
		(&Server{}).handleInternalBotForAgent(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status got %d want 400", body, w.Code)
		}
	}
}

func TestHashHostID_DeterministicWithSalt(t *testing.T) {
	// hashHostID is the security-critical part: same raw uuid + same
	// salt → same host_id; same raw uuid + different salt → different
	// host_id. We can't easily test the env-var case without process
	// isolation, so we exercise the helper's stability via the fallback
	// path (env unset → dev sentinel).
	a, err := hashHostID("12345678-90AB-CDEF-1234-567890ABCDEF")
	if err != nil {
		t.Fatalf("hashHostID: %v", err)
	}
	b, err := hashHostID("12345678-90AB-CDEF-1234-567890ABCDEF")
	if err != nil {
		t.Fatalf("hashHostID 2: %v", err)
	}
	if a != b {
		t.Fatalf("hashHostID not deterministic: %q vs %q", a, b)
	}
	if len(a) != 32 {
		t.Fatalf("host_id length = %d, want 32 hex chars", len(a))
	}
	c, _ := hashHostID("00000000-0000-0000-0000-000000000000")
	if a == c {
		t.Fatal("different inputs hashed to same host_id")
	}
}

func TestHashHostID_RejectsEmpty(t *testing.T) {
	if _, err := hashHostID(""); err == nil {
		t.Fatal("expected error for empty raw uuid")
	}
	if _, err := hashHostID("   "); err == nil {
		t.Fatal("expected error for whitespace raw uuid")
	}
}

func TestNewAgentID_Format(t *testing.T) {
	id, err := newAgentID()
	if err != nil {
		t.Fatalf("newAgentID: %v", err)
	}
	if !strings.HasPrefix(id, "ag_") {
		t.Fatalf("agent id should start with ag_: got %q", id)
	}
	if len(id) != 3+32 {
		t.Fatalf("agent id len = %d, want 35 (ag_ + 32 hex)", len(id))
	}
}

func TestAgentHub_AgentIDPreservesMultiAgentOnSameBot(t *testing.T) {
	// Two conns with same botUserID but different agentIDs MUST both
	// stay in the hub (v4: one bot can host N agents). The legacy
	// "register kicks any same-bot prev off" path only fires when both
	// conns have empty agentID (true v3 attaches).
	hub := newAgentHub()
	c1 := &agentConn{agentID: "ag_aaa", botUserID: "bot_shared", close: make(chan struct{})}
	c2 := &agentConn{agentID: "ag_bbb", botUserID: "bot_shared", close: make(chan struct{})}
	if prev := hub.register(c1); prev != nil {
		t.Fatalf("first register: got displaced %+v", prev)
	}
	if prev := hub.register(c2); prev != nil {
		t.Fatalf("second register (diff agent_id): got displaced %+v, want nil", prev)
	}
	if got := hub.lookupByAgentID("ag_aaa"); got != c1 {
		t.Errorf("ag_aaa lookup = %p, want %p", got, c1)
	}
	if got := hub.lookupByAgentID("ag_bbb"); got != c2 {
		t.Errorf("ag_bbb lookup = %p, want %p", got, c2)
	}
	// Bot lookup wins by last-write (legacy fallback).
	if got := hub.lookup("bot_shared"); got != c2 {
		t.Errorf("bot fallback lookup = %p, want %p (last-write)", got, c2)
	}
}

func TestAgentHub_EmptyAgentIDFallsBackToBotKey(t *testing.T) {
	// v3 conn with empty agentID — register MUST kick prev off
	// (preserves the old "one bot, one conn" semantics for legacy
	// agents that haven't been migrated to agent_id yet).
	hub := newAgentHub()
	c1 := &agentConn{botUserID: "bot_legacy", close: make(chan struct{})}
	c2 := &agentConn{botUserID: "bot_legacy", close: make(chan struct{})}
	hub.register(c1)
	if prev := hub.register(c2); prev != c1 {
		t.Fatalf("legacy second register: prev = %p, want %p", prev, c1)
	}
}

func TestHandleInternalNotify_MissingContent(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/v1/notify", strings.NewReader(`{"workspace_id":"ws1"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	(&Server{}).handleInternalNotify(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestFormatNotice(t *testing.T) {
	got := formatNotice(internalNotifyReq{Title: "余额不足", Body: "还剩 0.5 币", URL: "https://cloud.example/billing", Kind: "cloud.billing.grace"})
	want := "余额不足\n还剩 0.5 币\nhttps://cloud.example/billing\n#cloud.billing.grace"
	if got != want {
		t.Fatalf("formatNotice: got %q want %q", got, want)
	}
	if formatNotice(internalNotifyReq{Body: "b"}) != "b" {
		t.Fatalf("body-only notice should be bare body")
	}
}
