package dock

// Pure handler-level tests for the F1 additions (Phase 2 prep). Same
// scope as internal_api_handlers_test.go: cover the no-DB paths
// (empty params, bad JSON). DB-touching paths get covered in the
// manual smoke procedure documented in the PR.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHandleInternalBotUserGet_MissingID(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{gin.Param{Key: "id", Value: ""}}
	c.Request = httptest.NewRequest(http.MethodGet, "/internal/v1/bot-users/", nil)
	(&Server{}).handleInternalBotUserGet(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleInternalChatThreadGet_BadID(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{gin.Param{Key: "id", Value: "not-a-number"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/internal/v1/chat-threads/x", nil)
	(&Server{}).handleInternalChatThreadGet(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleInternalAgentPresence_NoHub(t *testing.T) {
	// agentHub nil means dock didn't init it (e.g. tests). Still returns
	// 200 with attached:false so plugins don't 500 on warm-up.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{gin.Param{Key: "bot_id", Value: "u_bot"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/internal/v1/agent-presence/u_bot", nil)
	(&Server{}).handleInternalAgentPresence(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["attached"] != false {
		t.Fatalf("attached: got %v want false", body["attached"])
	}
	if body["bot_user_id"] != "u_bot" {
		t.Fatalf("bot_user_id: got %v want u_bot", body["bot_user_id"])
	}
}

func TestHandleInternalAgentPresence_MissingBotID(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{gin.Param{Key: "bot_id", Value: ""}}
	c.Request = httptest.NewRequest(http.MethodGet, "/internal/v1/agent-presence/", nil)
	(&Server{}).handleInternalAgentPresence(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleInternalAgentDispatch_BadJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/v1/agent/dispatch",
		strings.NewReader(`not-json`))
	c.Request.Header.Set("Content-Type", "application/json")
	(&Server{}).handleInternalAgentDispatch(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleInternalAgentDispatch_NoAgent(t *testing.T) {
	// aiAgent nil → 503 (rather than panicking on enqueue against a
	// nil pointer). Plugins should treat 503 as "dock can't dispatch
	// right now; try again later".
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"thread_id":1,"user_id":"u","responder_user_id":"u_bot","content":"hi"}`
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/v1/agent/dispatch",
		strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	(&Server{}).handleInternalAgentDispatch(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503", w.Code)
	}
}

func TestHandleInternalLLMCallRecord_BadJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/internal/v1/agent/llm-call-record",
		strings.NewReader(`{`))
	c.Request.Header.Set("Content-Type", "application/json")
	(&Server{}).handleInternalLLMCallRecord(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}
