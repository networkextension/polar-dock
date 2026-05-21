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
