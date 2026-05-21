// Package iosdist is the iOS Distribution control plane plugin.
//
// Phase 2-W1 iosdist-skeleton ships the architecture, not the
// handlers. Dock still owns the /api/iosdist/* + /iosdist/* surface;
// iosdist-svc stands up alongside, owns polar_iosdist + its own
// blob root, and proves the layout end-to-end (heartbeat back to
// dock via /internal/v1/plugin-registry/heartbeat).
//
// Next PR moves the 7K LOC of handlers (iosdist_handlers.go,
// iosdist_asc_handlers.go, iosdist_public.go, iosdist_sign_worker.go,
// iosdist_status_sync.go, iosdist_agent_handlers.go, iosdist_store.go,
// iosdist_asc.go, iosdist_ipa.go, iosdist_crypto.go, iosdist_parsers.go)
// across, mirroring Phase 1-D-1's wg move.
//
// The Plugin owns:
//   - DB pool to polar_iosdist
//   - Dock SDK client for cross-domain user/team lookups
//   - Local blob roots: $IOSDIST_BLOB_DIR/{ipa,certs,profiles,asc-keys,icons}
//   - Encryption key for cert passwords + ASC .p8 contents
//     (IOSDIST_RESOURCE_KEY env, AES-GCM, mirrors dock's existing
//      iosdist_crypto.go)
//   - Heartbeat goroutine (60s cadence → dock's plugin registry)
package iosdist

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gin-auth-app/internal/plugins/sdk"
)

type Plugin struct {
	DB         *sql.DB
	Dock       *sdk.Client
	Name       string
	Listen     string
	Ver        string
	BlobDir    string // $IOSDIST_BLOB_DIR — ipa/, certs/, profiles/, asc-keys/, icons/
	MetricsTok string

	metrics   *iosdistMetrics
	startedAt time.Time
}

type Config struct {
	DBDSN        string
	DockBase     string
	PluginName   string
	PluginToken  string
	Listen       string
	BuildVersion string
	BlobDir      string
	MetricsToken string
}

func New(ctx context.Context, cfg Config) (*Plugin, error) {
	cfg.PluginName = strings.TrimSpace(cfg.PluginName)
	if cfg.PluginName == "" {
		cfg.PluginName = "iosdist"
	}
	if strings.TrimSpace(cfg.DBDSN) == "" {
		return nil, errors.New("iosdist.New: DBDSN required")
	}
	if strings.TrimSpace(cfg.DockBase) == "" {
		return nil, errors.New("iosdist.New: DockBase required")
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		return nil, errors.New("iosdist.New: PluginToken required (plaintext from /admin-plugins.html)")
	}
	if strings.TrimSpace(cfg.BlobDir) == "" {
		return nil, errors.New("iosdist.New: BlobDir required (iosdist-svc-local data root)")
	}

	db, err := sql.Open("postgres", cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("open polar_iosdist: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping polar_iosdist: %w", err)
	}

	dock := sdk.NewClient(cfg.DockBase, cfg.PluginName, sdk.DeriveHMACKey(cfg.PluginToken))
	resp, err := dock.Do(http.MethodGet, "/internal/v1/ping", nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("dock ping: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = db.Close()
		return nil, fmt.Errorf("dock /ping rejected: HTTP %d (check PLUGIN_TOKEN + plugin_modules row)", resp.StatusCode)
	}

	return &Plugin{
		DB:         db,
		Dock:       dock,
		Name:       cfg.PluginName,
		Listen:     cfg.Listen,
		Ver:        cfg.BuildVersion,
		BlobDir:    cfg.BlobDir,
		MetricsTok: cfg.MetricsToken,
		metrics:    newIOSDistMetrics(),
		startedAt:  time.Now(),
	}, nil
}

// RegisterRoutes attaches the iosdist plugin's HTTP surface.
// Phase 2-W1 skeleton: /healthz + /metrics only. The handler
// migration PR adds /api/iosdist/* admin CRUD + /iosdist/* public
// (install / manifest / share / plaza / test-request).
func (p *Plugin) RegisterRoutes(r gin.IRouter) {
	r.GET("/healthz", p.handleHealthz)
	r.GET("/metrics", p.handleMetricsExposition)
}

func (p *Plugin) Start(ctx context.Context) {
	go p.heartbeatLoop(ctx)
}

func (p *Plugin) Close() error {
	if p.DB != nil {
		return p.DB.Close()
	}
	return nil
}

func (p *Plugin) handleHealthz(c *gin.Context) {
	dbOK := true
	if err := p.DB.PingContext(c.Request.Context()); err != nil {
		dbOK = false
	}
	status := http.StatusOK
	if !dbOK {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{
		"plugin":         p.Name,
		"version":        p.Ver,
		"uptime_seconds": int64(time.Since(p.startedAt).Seconds()),
		"db_ok":          dbOK,
		"blob_dir":       p.BlobDir,
		"go":             runtime.Version(),
	})
}

func (p *Plugin) handleMetricsExposition(c *gin.Context) {
	if p.MetricsTok == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if c.GetHeader("Authorization") != "Bearer "+p.MetricsTok {
		c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	promhttp.HandlerFor(p.metrics.registry, promhttp.HandlerOpts{}).ServeHTTP(c.Writer, c.Request)
}

func (p *Plugin) heartbeatLoop(ctx context.Context) {
	p.beat(ctx)
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.beat(ctx)
		}
	}
}

func (p *Plugin) beat(_ context.Context) {
	err := p.Dock.Heartbeat(sdk.HeartbeatOpts{
		Version:       p.Ver,
		Endpoint:      p.Listen,
		UptimeSeconds: int64(time.Since(p.startedAt).Seconds()),
	})
	if err != nil {
		log.Printf("iosdist: heartbeat failed: %v", err)
	}
}

// ── metrics — placeholder set; handler migration adds more ─────────

type iosdistMetrics struct {
	registry *prometheus.Registry
	upGauge  prometheus.Gauge
}

func newIOSDistMetrics() *iosdistMetrics {
	m := &iosdistMetrics{registry: prometheus.NewRegistry()}
	m.upGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "polar_iosdist_up",
		Help: "Always 1 while iosdist-svc is serving. Phase 2-W1 placeholder.",
	})
	m.registry.MustRegister(m.upGauge)
	m.upGauge.Set(1)
	return m
}
