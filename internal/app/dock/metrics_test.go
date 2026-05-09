package dock

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Phase 1 metrics tests. Drive the gin handler directly via httptest so we
// can assert auth gate + middleware-recorded counters without bringing up
// the full server (and its DB / Redis dependencies).
func newMetricsTestServer(token string) *Server {
	gin.SetMode(gin.TestMode)
	s := &Server{
		metricsRegistry: newMetrics(),
		metricsToken:    token,
	}
	r := gin.New()
	r.Use(s.metricsRegistry.httpMiddleware())
	r.GET("/probe", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/metrics", s.handleMetrics)
	s.router = r
	return s
}

func TestMetricsAuthFailClosed(t *testing.T) {
	s := newMetricsTestServer("")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unset token should return 404, got %d", w.Code)
	}
}

func TestMetricsAuthMismatch(t *testing.T) {
	s := newMetricsTestServer("expected")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer should return 401, got %d", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("expected WWW-Authenticate hint, got %q", got)
	}
}

func TestMetricsAuthOK(t *testing.T) {
	s := newMetricsTestServer("good")
	// Drive a request through the middleware so the counter has a sample.
	probe := httptest.NewRecorder()
	s.router.ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/probe", nil))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer good")
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("good bearer should return 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"polar_http_requests_total",
		"polar_http_request_duration_seconds",
		"polar_goroutines",
		"polar_db_pool_open",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in /metrics output, missing", want)
		}
	}
	// /probe ran once before /metrics, so the counter must show >= 1 for that route.
	if !strings.Contains(body, `polar_http_requests_total{method="GET",path="/probe",status="200"} 1`) {
		t.Errorf("expected /probe counter sample in body, got snippet:\n%s",
			truncForLog(body, 400))
	}
}

func TestMetricsUnknownPathLabel(t *testing.T) {
	s := newMetricsTestServer("good")
	// hit a route gin doesn't have — middleware should bucket as "unknown",
	// not spawn a series per random URL.
	for _, p := range []string{"/random/aaa/111", "/random/bbb/222"} {
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer good")
	s.router.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `path="unknown"`) {
		t.Errorf("expected unmatched paths to bucket as 'unknown'")
	}
}

func truncForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Phase 2 — exercise the LLM call recorder directly. We can't bring up a
// real upstream provider in unit tests, so we go through the registry's
// helper and assert the exposition output shows what we expect.
func TestRecordLLMCallSuccessWithUsage(t *testing.T) {
	m := newMetrics()
	m.recordLLMCall("openai_compatible", "kimi-k2", "thread_summary",
		1500*time.Millisecond, nil, 320, 80)

	body := scrapeRegistry(t, m)
	for _, want := range []string{
		`polar_llm_request_total{endpoint="thread_summary",model="kimi-k2",provider="openai_compatible",status="ok"} 1`,
		`polar_llm_tokens_total{endpoint="thread_summary",kind="prompt",model="kimi-k2",provider="openai_compatible"} 320`,
		`polar_llm_tokens_total{endpoint="thread_summary",kind="completion",model="kimi-k2",provider="openai_compatible"} 80`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing line %q", want)
		}
	}
}

func TestRecordLLMCallErrorNoTokens(t *testing.T) {
	m := newMetrics()
	m.recordLLMCall("anthropic_messages", "claude-sonnet-4", "task_retrospect",
		3*time.Second, errBoom{}, 0, 0)

	body := scrapeRegistry(t, m)
	if !strings.Contains(body, `polar_llm_request_total{endpoint="task_retrospect",model="claude-sonnet-4",provider="anthropic_messages",status="error"} 1`) {
		t.Errorf("expected error counter to record sample")
	}
	if strings.Contains(body, `polar_llm_tokens_total{endpoint="task_retrospect"`) {
		t.Errorf("token counter should stay flat when no usage was reported")
	}
}

func TestRecordLLMCallEmptyLabelsBucketed(t *testing.T) {
	m := newMetrics()
	// Caller forgot to set Endpoint — should land in "unknown", not "".
	m.recordLLMCall("", "", "", 100*time.Millisecond, nil, 0, 0)
	body := scrapeRegistry(t, m)
	if !strings.Contains(body, `provider="unknown"`) || !strings.Contains(body, `endpoint="unknown"`) {
		t.Errorf("empty labels should fall back to 'unknown'")
	}
}

func scrapeRegistry(t *testing.T, m *metrics) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := &Server{metricsRegistry: m, metricsToken: "x"}
	r := gin.New()
	r.GET("/metrics", s.handleMetrics)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer x")
	r.ServeHTTP(w, req)
	return w.Body.String()
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
