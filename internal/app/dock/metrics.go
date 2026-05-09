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

// Phase 1 metric set: HTTP shape + Go runtime + DB pool. LLM / agent /
// business counters land in later phases (see doc/grafana-integration.md).
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
}

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
	}
	reg.MustRegister(m.httpRequests, m.httpLatency, m.goroutines, m.dbPoolOpen, m.dbPoolUse, m.dbPoolIdle)
	return m
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
