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
