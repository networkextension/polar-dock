package dock

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// heartbeatStaleAfter bounds how long after a plugin's last heartbeat we
// still consider it healthy. Plugins heartbeat every ~10s, so 90s tolerates
// a few missed beats (restart, GC pause, brief network blip) before a module
// is reported unhealthy.
const heartbeatStaleAfter = 90 * time.Second

// zoneModuleHealth is the per-module row in the public zone health summary.
// Secrets-free by construction: name, version, booleans, freshness only.
type zoneModuleHealth struct {
	Name          string  `json:"name"`
	Version       string  `json:"version,omitempty"`
	Enabled       bool    `json:"enabled"`
	Healthy       bool    `json:"healthy"`
	LastHeartbeat *string `json:"last_heartbeat,omitempty"`
	StaleSeconds  *int64  `json:"stale_seconds,omitempty"`
}

// handleZoneHealth is the public, unauthenticated health summary for this
// zone — the data source the Zone Federation control plane polls and the
// per-zone fleet board renders (doc/arch/zone-federation-platform.md).
//
// Health is derived from the plugin heartbeat (plugin_modules.last_heartbeat,
// refreshed by every plugin ~10s via /internal/v1/plugin-registry/heartbeat)
// rather than a live fan-out, so the endpoint stays cheap enough to poll
// frequently and never blocks on a wedged plugin. A module is healthy when it
// is enabled and its last heartbeat is within heartbeatStaleAfter.
//
// It deliberately leaks no secrets (no DSNs, tokens, endpoints) so it is safe
// to expose unauthenticated; the control plane authenticates zone *identity*
// separately when it polls.
func (s *Server) handleZoneHealth(c *gin.Context) {
	now := time.Now().UTC()

	mods, err := s.listPluginModules()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "health unavailable"})
		return
	}

	out := make([]zoneModuleHealth, 0, len(mods))
	healthy := 0
	for _, m := range mods {
		mh := zoneModuleHealth{Name: m.Name, Version: m.Version, Enabled: m.Enabled}
		if m.LastHeartbeatAt != nil {
			ts := m.LastHeartbeatAt.UTC().Format(time.RFC3339)
			mh.LastHeartbeat = &ts
			stale := int64(now.Sub(*m.LastHeartbeatAt).Seconds())
			mh.StaleSeconds = &stale
			mh.Healthy = m.Enabled && now.Sub(*m.LastHeartbeatAt) < heartbeatStaleAfter
		}
		if mh.Healthy {
			healthy++
		}
		out = append(out, mh)
	}

	c.JSON(http.StatusOK, gin.H{
		"zone":          s.zoneName(),
		"dock":          gin.H{"version": buildGitSHA, "healthy": true},
		"modules":       out,
		"module_count":  len(out),
		"healthy_count": healthy,
		"ts":            now.Format(time.RFC3339),
	})
}

// zoneName is a stable, human-readable label for this zone. Prefers an
// explicit POLAR_ZONE_NAME; otherwise derives it from the public base URL
// host (e.g. "dock.everest.4950.store" → "everest"), falling back to the raw
// host. Informational only — the control plane keys zones by registered URL.
func (s *Server) zoneName() string {
	if v := strings.TrimSpace(os.Getenv("POLAR_ZONE_NAME")); v != "" {
		return v
	}
	host := s.publicBaseURL
	if u, err := url.Parse(s.publicBaseURL); err == nil && u.Host != "" {
		host = u.Hostname()
	}
	host = strings.TrimPrefix(host, "dock.")
	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[:i]
	}
	return host
}
