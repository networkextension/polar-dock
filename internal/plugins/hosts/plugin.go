// Package hosts is the polar-agent host management plugin (hosts,
// host_skills, host_skill_runs, host_skill_credentials, agent_tokens,
// console_layouts).
//
// Phase 2-W3 skeleton: DB pool + heartbeat + /healthz only. Handler
// PR moves the substantial set of files across:
//   - hosts_store.go / agent_store.go / host_skill_runs_store.go
//   - agent_handlers.go (WebSocket hub for polar-agent attach)
//   - agent_hub.go + agent_install_script.go + agent_binary_handler.go
//   - host_handlers.go + host_local_bootstrap_handler.go
//   - host_shell_*.go (PTY supervisor + WS shell handlers + backscroll)
//   - host_vnc_*.go (VNC TCP relay + WS)
//   - host_skill_credentials_*.go (AES-GCM encrypted env vars)
//
// Cross-domain user/team/bot lookups go through dock SDK. Notably:
//   - bot_users.host_skill_id is a reverse FK FROM dock TO this
//     plugin's host_skills(id); the cutover PR drops dock's FK and
//     leaves it as a TEXT pointer there (lookups via this plugin's
//     own future /internal/v1/host-skills/:id endpoint).
package hosts

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
	BlobDir    string // $POLAR_HOSTS_BLOB_DIR — agent install scripts, skill logs
	MetricsTok string

	metrics   *hostsMetrics
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
		cfg.PluginName = "hosts"
	}
	if strings.TrimSpace(cfg.DBDSN) == "" {
		return nil, errors.New("hosts.New: DBDSN required")
	}
	if strings.TrimSpace(cfg.DockBase) == "" {
		return nil, errors.New("hosts.New: DockBase required")
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		return nil, errors.New("hosts.New: PluginToken required")
	}
	if strings.TrimSpace(cfg.BlobDir) == "" {
		return nil, errors.New("hosts.New: BlobDir required")
	}

	db, err := sql.Open("postgres", cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("open polar_hosts: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping polar_hosts: %w", err)
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

	return &Plugin{
		DB:         db,
		Dock:       dock,
		Name:       cfg.PluginName,
		Listen:     cfg.Listen,
		Ver:        cfg.BuildVersion,
		BlobDir:    cfg.BlobDir,
		MetricsTok: cfg.MetricsToken,
		metrics:    newHostsMetrics(),
		startedAt:  time.Now(),
	}, nil
}

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
		log.Printf("hosts: heartbeat failed: %v", err)
	}
}

type hostsMetrics struct {
	registry *prometheus.Registry
	upGauge  prometheus.Gauge
}

func newHostsMetrics() *hostsMetrics {
	m := &hostsMetrics{registry: prometheus.NewRegistry()}
	m.upGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "polar_hosts_up",
		Help: "Always 1 while hosts-svc is serving. Phase 2-W3 placeholder.",
	})
	m.registry.MustRegister(m.upGauge)
	m.upGauge.Set(1)
	return m
}
