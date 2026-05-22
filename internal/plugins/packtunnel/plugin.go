// Package packtunnel is the proxy/VPN profile management plugin.
// Phase 2-W2 skeleton: DB pool + heartbeat + /healthz only.
// Handler PR moves the 2 dock files (packtunnel_handlers.go,
// packtunnel_rules_handlers.go) across.
package packtunnel

import (
	"context"
	"database/sql"
	"encoding/json"
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
	BlobDir    string // $POLAR_PACKTUNNEL_BLOB_DIR — rules/ subdir
	MetricsTok string

	// SystemWorkspaceID is dock's "system" user's personal team.
	// packtunnel data is platform-wide (a single proxy config drives
	// the network agent regardless of who's logged in) but the schema
	// still wants a workspace_id, so all handlers stamp this value.
	// Resolved once at New() via /internal/v1/users/system/workspace
	// so the hot path doesn't hit dock per request.
	SystemWorkspaceID string

	metrics   *packtunnelMetrics
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
		cfg.PluginName = "packtunnel"
	}
	if strings.TrimSpace(cfg.DBDSN) == "" {
		return nil, errors.New("packtunnel.New: DBDSN required")
	}
	if strings.TrimSpace(cfg.DockBase) == "" {
		return nil, errors.New("packtunnel.New: DockBase required")
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		return nil, errors.New("packtunnel.New: PluginToken required")
	}
	if strings.TrimSpace(cfg.BlobDir) == "" {
		return nil, errors.New("packtunnel.New: BlobDir required")
	}

	db, err := sql.Open("postgres", cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("open polar_packtunnel: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping polar_packtunnel: %w", err)
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
		return nil, fmt.Errorf("dock /ping rejected: HTTP %d", resp.StatusCode)
	}

	// Pre-fetch the system workspace ID. packtunnel data is platform-
	// wide so every row stamps this. /internal/v1/users/system/workspace
	// returns it. If dock hasn't bootstrapped the system user yet (cold
	// box, never-seen), we log + continue with empty — the first POST
	// will surface a clean DB error rather than a confusing 500.
	sysWS, err := dock.Do(http.MethodGet, "/internal/v1/users/system/workspace", nil)
	systemWorkspaceID := ""
	if err == nil {
		var body struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := readJSON(sysWS, &body); err == nil {
			systemWorkspaceID = body.WorkspaceID
		}
	}
	if systemWorkspaceID == "" {
		log.Printf("packtunnel: WARN system workspace lookup failed; handlers will return 500 until dock bootstraps system user (err=%v)", err)
	}

	return &Plugin{
		DB:                db,
		Dock:              dock,
		Name:              cfg.PluginName,
		Listen:            cfg.Listen,
		Ver:               cfg.BuildVersion,
		BlobDir:           cfg.BlobDir,
		MetricsTok:        cfg.MetricsToken,
		SystemWorkspaceID: systemWorkspaceID,
		metrics:           newPacktunnelMetrics(),
		startedAt:         time.Now(),
	}, nil
}

// readJSON — minimal JSON unmarshaller; SDK has the same helper but
// it's unexported. Local copy avoids the import-cycle gymnastics.
func readJSON(resp *http.Response, out any) error {
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	return dec.Decode(out)
}

func (p *Plugin) RegisterRoutes(r gin.IRouter) {
	r.GET("/healthz", p.handleHealthz)
	r.GET("/metrics", p.handleMetricsExposition)

	// /api/packtunnel/* — match the dock-side route prefix so nginx
	// can flip with a single proxy_pass redirect (see
	// scripts/nginx/packtunnel-svc-snippet.conf). Routes that mutate
	// state require admin role; reads of the active profile + rules
	// download are open to any logged-in user (matches dock behavior).
	api := r.Group("/api")
	{
		admin := api.Group("", p.requireAdminViaDock())
		{
			admin.GET("/packtunnel/profiles", p.handlePackTunnelProfileList)
			admin.GET("/packtunnel/profiles/:id", p.handlePackTunnelProfileGet)
			admin.POST("/packtunnel/profiles", p.handlePackTunnelProfileCreate)
			admin.PUT("/packtunnel/profiles/:id", p.handlePackTunnelProfileUpdate)
			admin.DELETE("/packtunnel/profiles/:id", p.handlePackTunnelProfileDelete)
			admin.PUT("/packtunnel/profiles/:id/activate", p.handlePackTunnelProfileActivate)
			admin.POST("/packtunnel/rules", p.handlePackTunnelRuleUpload)
			admin.DELETE("/packtunnel/rules", p.handlePackTunnelRuleDelete)
		}
		authed := api.Group("", p.requireAuthViaDock())
		{
			authed.GET("/packtunnel/profiles/active", p.handlePackTunnelActiveProfileGet)
			authed.GET("/packtunnel/rules", p.handlePackTunnelRuleDownload)
		}
	}
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
		log.Printf("packtunnel: heartbeat failed: %v", err)
	}
}

type packtunnelMetrics struct {
	registry *prometheus.Registry
	upGauge  prometheus.Gauge
}

func newPacktunnelMetrics() *packtunnelMetrics {
	m := &packtunnelMetrics{registry: prometheus.NewRegistry()}
	m.upGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "polar_packtunnel_up",
		Help: "Always 1 while packtunnel-svc is serving. Phase 2-W2 placeholder.",
	})
	m.registry.MustRegister(m.upGauge)
	m.upGauge.Set(1)
	return m
}
