// Package wg is the wg-mac control plane plugin. Phase 1-C ships a
// skeleton: DB connection to polar_wg + dock heartbeat + /healthz.
// Phase 1-D moves the actual hub/token/device/site/bundle handlers
// out of dock and into this package, then flips nginx to route
// /api/wg/* + /v1/* to wg-svc.
//
// Design:
//   - Each plugin owns its DB. polar_wg is a separate Postgres
//     database (same instance OK). Cross-domain user/team lookups
//     go via the SDK's HMAC client (sdk.Client).
//   - HTTP server is a plain gin engine — wg-svc's cmd/main.go
//     wires it up. The plugin doesn't manage process lifecycle.
//   - Heartbeat goroutine pings dock every 60s so /admin-plugins.html
//     shows fresh state and so plugin GC (Phase 1-D) can spot dead
//     workers.
package wg

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

	"gin-auth-app/internal/plugins/sdk"
)

// Plugin holds the runtime state for wg-svc. Build via New + call
// RegisterRoutes(engine) and Start(ctx).
type Plugin struct {
	DB     *sql.DB
	Dock   *sdk.Client
	Name   string // "wg" — must match plugin_modules.name in dock
	Listen string // e.g. "127.0.0.1:8090" — for the heartbeat endpoint field
	Ver    string // build version stamped at compile time

	startedAt time.Time
}

// Config is the inputs to New.
type Config struct {
	DBDSN        string // postgres DSN for polar_wg
	DockBase     string // "http://127.0.0.1:8080"
	PluginName   string // "wg"
	PluginToken  string // plaintext token from /admin-plugins.html (one-time print)
	Listen       string // public/internal endpoint for the heartbeat row
	BuildVersion string // optional, "" allowed
}

// New opens the DB pool, builds the dock SDK client, and verifies the
// HMAC handshake is plausible by sending a /ping. Failing /ping is
// fatal — better to crash now than serve 500s for an hour.
func New(ctx context.Context, cfg Config) (*Plugin, error) {
	cfg.PluginName = strings.TrimSpace(cfg.PluginName)
	if cfg.PluginName == "" {
		cfg.PluginName = "wg"
	}
	if strings.TrimSpace(cfg.DBDSN) == "" {
		return nil, errors.New("wg.New: DBDSN required")
	}
	if strings.TrimSpace(cfg.DockBase) == "" {
		return nil, errors.New("wg.New: DockBase required")
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		return nil, errors.New("wg.New: PluginToken required (plaintext from /admin-plugins.html)")
	}

	db, err := sql.Open("postgres", cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("open polar_wg: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping polar_wg: %w", err)
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
		DB:        db,
		Dock:      dock,
		Name:      cfg.PluginName,
		Listen:    cfg.Listen,
		Ver:       cfg.BuildVersion,
		startedAt: time.Now(),
	}, nil
}

// RegisterRoutes attaches the wg plugin's HTTP surface to an existing
// gin engine. Phase 1-C only exposes /healthz; the real /api/wg/*
// and /v1/* handlers move here in Phase 1-D.
func (p *Plugin) RegisterRoutes(r gin.IRouter) {
	r.GET("/healthz", p.handleHealthz)
}

// Start kicks off background work — currently just the heartbeat
// loop. Returns when ctx is cancelled.
func (p *Plugin) Start(ctx context.Context) {
	go p.heartbeatLoop(ctx)
}

// Close releases the DB pool. Call after Start's ctx is cancelled +
// the HTTP server has drained.
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
		"go":             runtime.Version(),
	})
}

// heartbeatLoop pings dock once at startup, then every 60s. Failures
// log + continue; dock's plugin GC tolerates one missed beat (5m
// window). If dock is unreachable for longer the plugin still serves
// its own routes — only the registry view in /admin-plugins.html
// goes stale.
func (p *Plugin) heartbeatLoop(ctx context.Context) {
	// One immediate ping so /admin-plugins.html shows the new mtime
	// without waiting a minute.
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
		log.Printf("wg: heartbeat failed: %v", err)
	}
}
