package dock

import (
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Phase 1 metric set: HTTP shape + Go runtime + DB pool.
// Phase 2 added: LLM call counter / latency / token consumption.
// Phase 3+ will add agent fleet + business counters
// (see doc/ops/grafana-integration.md).
//
// All metrics live on a private registry so the /metrics endpoint can be
// gated independently of any other Prometheus surface a future reverse
// proxy might add.
type metrics struct {
	registry *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpLatency  *prometheus.HistogramVec

	goroutines  prometheus.Gauge
	dbPoolOpen  prometheus.Gauge
	dbPoolUse   prometheus.Gauge
	dbPoolIdle  prometheus.Gauge

	// LLM call instrumentation. status is a small fixed vocabulary
	// ("ok" / "error") to keep cardinality bounded; endpoint is a
	// caller-supplied tag (e.g. "thread_summary", "task_retrospect")
	// so we can slice by feature without leaking arbitrary strings.
	// kind on the token counter is "prompt" or "completion".
	llmRequests *prometheus.CounterVec
	llmLatency  *prometheus.HistogramVec
	llmTokens   *prometheus.CounterVec

	// Phase 3: agent fleet + business event counters.
	// agentAttached is sampled (alongside the goroutine / DB pool gauges)
	// from agentHub.snapshotByTool. Tool is the passthrough tool name
	// declared by each polar-agent in its hello frame; empty becomes
	// "tool_loop" so the operator can distinguish protocol modes.
	agentAttached      *prometheus.GaugeVec
	researchDispatches *prometheus.CounterVec
	iosdistBuilds      *prometheus.CounterVec

	// LLM proxy instrumentation (P0 = audit row in DB; this is the
	// real-time slice for Grafana). e2e covers the wall time the
	// client experienced; ttft is set only on streaming success
	// frames carrying the first content delta. completionTokens is
	// a histogram because per-request size matters more than rate
	// for spotting "is the model truncating" anomalies.
	llmProxyE2E         *prometheus.HistogramVec
	llmProxyTTFT        *prometheus.HistogramVec
	llmProxyCompletion  *prometheus.HistogramVec
	llmProxyRequests    *prometheus.CounterVec

	// Upstream-health probe gauge — set to 1 when a synthetic
	// completion call to the upstream returns 200 within
	// timeout, 0 when it errors or times out. Sampled by the
	// prober goroutine (see llm_proxy_probe.go), NOT updated on
	// production proxy traffic — keeps the signal stable even
	// when no real customer is calling.
	llmUpstreamUp *prometheus.GaugeVec

	// P4: count of /api/proxy/v1/chat/completions calls rejected
	// for monthly USD quota exhaustion. Labelled by token so
	// operator can spot which one is hitting the cap.
	llmProxyQuotaExceeded *prometheus.CounterVec

	// wg-mac metrics (wgDevicesAlive, wgHubsBound, wgRegisterTotal,
	// wgHeartbeatTotal, wgStaleGCTotal, wgStaleGCRemoved,
	// wgTokensExpiringSoon) moved to
	// github.com/networkextension/polar-wg.
}

// llmLatencyBuckets are tuned for chat-completion calls, which run from
// ~200ms (small prompt, fast model) to 30s+ (long generation). Default
// Prometheus buckets top out at 10s and would dump everything past that
// into the +Inf overflow.
var llmLatencyBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120}

func newMetrics() *metrics {
	reg := prometheus.NewRegistry()
	m := &metrics{
		registry: reg,
		httpRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "polar_http_requests_total",
				Help: "Count of HTTP requests served by dock, labelled by gin route template, method, and status code.",
			},
			[]string{"method", "path", "status"},
		),
		httpLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "polar_http_request_duration_seconds",
				Help:    "HTTP request latency in seconds, labelled by gin route template and method.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"method", "path"},
		),
		goroutines: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "polar_goroutines",
			Help: "Live goroutine count, sampled periodically.",
		}),
		dbPoolOpen: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "polar_db_pool_open",
			Help: "Open Postgres connections (in use + idle).",
		}),
		dbPoolUse: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "polar_db_pool_in_use",
			Help: "Postgres connections currently checked out by callers.",
		}),
		dbPoolIdle: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "polar_db_pool_idle",
			Help: "Idle Postgres connections held in the pool.",
		}),
		llmRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "polar_llm_request_total",
				Help: "Count of LLM provider chat-completion calls, labelled by provider, model, caller endpoint, and outcome.",
			},
			[]string{"provider", "model", "endpoint", "status"},
		),
		llmLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "polar_llm_request_duration_seconds",
				Help:    "LLM chat-completion latency in seconds.",
				Buckets: llmLatencyBuckets,
			},
			[]string{"provider", "model", "endpoint"},
		),
		llmTokens: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "polar_llm_tokens_total",
				Help: "Tokens billed by upstream provider (kind=prompt or completion). Only set on responses that include a usage block — passthrough/CLI bots stay at zero.",
			},
			[]string{"provider", "model", "endpoint", "kind"},
		),
		agentAttached: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "polar_agent_attached",
				Help: "Currently-attached polar-agent connections, labelled by tool flavor (kimi/claude/codex/custom or 'tool_loop' for tool-loop mode).",
			},
			[]string{"tool"},
		),
		researchDispatches: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "polar_research_dispatch_total",
				Help: "Research-run state transitions. status one of queued / running / succeeded / failed.",
			},
			[]string{"status"},
		),
		iosdistBuilds: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "polar_iosdist_build_total",
				Help: "iOS distribution sign-task terminal outcomes. sign_method=zsign for the in-process worker; status=success or failed.",
			},
			[]string{"sign_method", "status"},
		),
		llmProxyE2E: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "polar_llm_proxy_e2e_seconds",
				Help:    "End-to-end latency for /api/proxy/v1/chat/completions, labelled by token name, model, mode (stream|nonstream) and status.",
				Buckets: llmLatencyBuckets,
			},
			[]string{"token", "model", "mode", "status"},
		),
		llmProxyTTFT: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "polar_llm_proxy_ttft_seconds",
				Help:    "Time-to-first-token for streaming /chat/completions. Recorded only when the first content delta arrives.",
				Buckets: []float64{0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10},
			},
			[]string{"token", "model"},
		),
		llmProxyCompletion: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "polar_llm_proxy_completion_tokens",
				Help:    "Completion tokens returned per /chat/completions call (excludes 4xx/5xx).",
				Buckets: []float64{16, 64, 256, 1024, 4096, 16384, 65536},
			},
			[]string{"token", "model"},
		),
		llmProxyRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "polar_llm_proxy_requests_total",
				Help: "Count of /api/proxy/v1/chat/completions calls labelled by token, model, mode and HTTP status code.",
			},
			[]string{"token", "model", "mode", "status"},
		),
		llmUpstreamUp: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "polar_llm_upstream_up",
				Help: "1 = the synthetic probe call to this llm_configs row returned 200 within timeout; 0 = error/timeout. Updated every 30s by the prober goroutine.",
			},
			[]string{"config_id", "name", "model"},
		),
		llmProxyQuotaExceeded: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "polar_llm_proxy_quota_exceeded_total",
				Help: "Count of /api/proxy/v1/chat/completions calls rejected because the token's monthly USD quota was exhausted.",
			},
			[]string{"token"},
		),
	}
	reg.MustRegister(
		m.httpRequests, m.httpLatency,
		m.goroutines, m.dbPoolOpen, m.dbPoolUse, m.dbPoolIdle,
		m.llmRequests, m.llmLatency, m.llmTokens,
		m.agentAttached, m.researchDispatches, m.iosdistBuilds,
		m.llmProxyE2E, m.llmProxyTTFT, m.llmProxyCompletion, m.llmProxyRequests,
		m.llmUpstreamUp, m.llmProxyQuotaExceeded,
	)
	return m
}

// recordLLMProxyQuotaExceeded bumps the per-token quota-rejection counter.
func (m *metrics) recordLLMProxyQuotaExceeded(tokenName string) {
	if m == nil {
		return
	}
	m.llmProxyQuotaExceeded.WithLabelValues(labelOr(tokenName, "unknown")).Inc()
}

// recordLLMProxyCall is the dock-side hook for the LLM proxy chat handler.
// Called on every terminal state (success or failure). When mode == "stream"
// and ttftSec > 0, also records TTFT. completionTokens is recorded
// independently so 4xx/5xx samples (which produce no tokens) don't drag the
// histogram floor.
func (m *metrics) recordLLMProxyCall(tokenName, model, mode string, status int, e2e time.Duration, ttftSec float64, completionTokens int) {
	if m == nil {
		return
	}
	tokenName = labelOr(tokenName, "unknown")
	model = labelOr(model, "unknown")
	mode = labelOr(mode, "nonstream")
	statusStr := strconv.Itoa(status)
	m.llmProxyE2E.WithLabelValues(tokenName, model, mode, statusStr).Observe(e2e.Seconds())
	m.llmProxyRequests.WithLabelValues(tokenName, model, mode, statusStr).Inc()
	if mode == "stream" && ttftSec > 0 {
		m.llmProxyTTFT.WithLabelValues(tokenName, model).Observe(ttftSec)
	}
	if status >= 200 && status < 300 && completionTokens > 0 {
		m.llmProxyCompletion.WithLabelValues(tokenName, model).Observe(float64(completionTokens))
	}
}

// setLLMUpstreamUp updates the gauge for one upstream config. Called by
// the prober (not by chat traffic).
func (m *metrics) setLLMUpstreamUp(configID int64, name, model string, up bool) {
	if m == nil {
		return
	}
	v := 0.0
	if up {
		v = 1.0
	}
	m.llmUpstreamUp.WithLabelValues(strconv.FormatInt(configID, 10), labelOr(name, "unknown"), labelOr(model, "unknown")).Set(v)
}

// recordLLMCall records one LLM chat-completion attempt. err non-nil ⇒
// status="error". promptTokens/completionTokens may be 0 if the upstream
// response didn't include a usage block; in that case the token counters
// stay flat for that sample (we don't extrapolate).
//
// Safe to call from any goroutine. No-op when the registry is nil so unit
// tests that don't construct metrics don't panic.
func (m *metrics) recordLLMCall(provider, model, endpoint string, dur time.Duration, err error, promptTokens, completionTokens int) {
	if m == nil {
		return
	}
	provider = labelOr(provider, "unknown")
	model = labelOr(model, "unknown")
	endpoint = labelOr(endpoint, "unknown")
	status := "ok"
	if err != nil {
		status = "error"
	}
	m.llmRequests.WithLabelValues(provider, model, endpoint, status).Inc()
	m.llmLatency.WithLabelValues(provider, model, endpoint).Observe(dur.Seconds())
	if promptTokens > 0 {
		m.llmTokens.WithLabelValues(provider, model, endpoint, "prompt").Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		m.llmTokens.WithLabelValues(provider, model, endpoint, "completion").Add(float64(completionTokens))
	}
}

func labelOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// httpMiddleware records request count + latency. Uses gin's matched route
// template (e.g. `/api/projects/:id`) instead of the raw URL so cardinality
// stays bounded; unmatched paths land in a single "unknown" bucket rather
// than spawning a series per 404.
func (m *metrics) httpMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		path := c.FullPath()
		if path == "" {
			path = "unknown"
		}
		status := strconv.Itoa(c.Writer.Status())
		m.httpRequests.WithLabelValues(c.Request.Method, path, status).Inc()
		m.httpLatency.WithLabelValues(c.Request.Method, path).Observe(time.Since(start).Seconds())
	}
}

// startSampler periodically refreshes the gauges that aren't event-driven
// (goroutines, DB pool stats). 15s cadence matches the prometheus.yml
// scrape interval — sampling faster wastes CPU; slower introduces
// staleness aliasing.
func (s *Server) startMetricsSampler() {
	if s.metricsRegistry == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		s.sampleMetrics()
		for {
			select {
			case <-s.backgroundCtx.Done():
				return
			case <-ticker.C:
				s.sampleMetrics()
			}
		}
	}()
}

func (s *Server) sampleMetrics() {
	s.metricsRegistry.goroutines.Set(float64(runtime.NumGoroutine()))
	if s.db != nil {
		st := s.db.Stats()
		s.metricsRegistry.dbPoolOpen.Set(float64(st.OpenConnections))
		s.metricsRegistry.dbPoolUse.Set(float64(st.InUse))
		s.metricsRegistry.dbPoolIdle.Set(float64(st.Idle))
	}
	if s.agentHub != nil {
		// Reset before re-populating so a tool flavor that has gone to
		// zero connections doesn't keep a stale non-zero sample.
		s.metricsRegistry.agentAttached.Reset()
		for tool, count := range s.agentHub.snapshotByTool() {
			s.metricsRegistry.agentAttached.WithLabelValues(tool).Set(float64(count))
		}
	}
	// wg-mac metrics moved to github.com/networkextension/polar-wg.
}

// wg-mac sample/record helpers (sampleWGMetrics, recordWGRegister,
// recordWGHeartbeat, recordWGStaleGC) moved to
// github.com/networkextension/polar-wg.

// recordResearchDispatch bumps the research-dispatch state counter.
// status: queued / running / succeeded / failed. Other strings are
// passed through verbatim so a future state ("cancelled") can be
// added without a registry change.
func (m *metrics) recordResearchDispatch(status string) {
	if m == nil {
		return
	}
	m.researchDispatches.WithLabelValues(labelOr(status, "unknown")).Inc()
}

// recordIOSDistBuild bumps the iosdist sign-task terminal outcome.
// signMethod: "zsign" today; reserve "codesign"/"asc" for the agent
// build path that lands in a later cut. status: success or failed.
func (m *metrics) recordIOSDistBuild(signMethod, status string) {
	if m == nil {
		return
	}
	m.iosdistBuilds.WithLabelValues(labelOr(signMethod, "unknown"), labelOr(status, "unknown")).Inc()
}

// handleMetrics serves the Prometheus exposition. Token gate is fail-closed:
// if METRICS_TOKEN is unset the endpoint refuses to serve at all (rather than
// silently exposing internals). Operators must set the env var to opt in.
func (s *Server) handleMetrics(c *gin.Context) {
	if s.metricsToken == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if c.GetHeader("Authorization") != "Bearer "+s.metricsToken {
		c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	promhttp.HandlerFor(s.metricsRegistry.registry, promhttp.HandlerOpts{}).ServeHTTP(c.Writer, c.Request)
}
