package dock

// wg_peer_status_handlers.go — `/internal/v1/wg-peer-status`:
// HMAC-authenticated read of the cache populated by
// agent → dock skill.event{metric, wg_peer_status} frames.
//
// Polar-wg polls this every ~30s and joins each sample's
// iface_public_key against wg_hubs.pubkey to render a per-hub
// status table. Other plugins can use it for their own dashboards
// without re-doing the agent-side plumbing.
//
// Filter params:
//   - host_id          — only return entries for that host
//   - iface_public_key — only return entries for that wireguard iface
//
// Both filters are AND'd. With no filters, returns every cached
// sample. Samples older than wgPeerSampleMaxAge are silently
// dropped at read time (no separate eviction goroutine).

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type wgPeerStatusResponseEntry struct {
	HostID         string         `json:"host_id"`
	Iface          string         `json:"iface"`
	IfacePublicKey string         `json:"iface_public_key,omitempty"`
	RunID          int64          `json:"run_id,omitempty"`
	RecordedAt     string         `json:"recorded_at"`
	Data           map[string]any `json:"data"`
}

func (s *Server) handleInternalWGPeerStatus(c *gin.Context) {
	if s.wgPeerCache == nil {
		c.JSON(http.StatusOK, gin.H{"samples": []wgPeerStatusResponseEntry{}})
		return
	}
	hostID := c.Query("host_id")
	ifacePub := c.Query("iface_public_key")
	samples := s.wgPeerCache.list(hostID, ifacePub)

	out := make([]wgPeerStatusResponseEntry, 0, len(samples))
	for _, s := range samples {
		out = append(out, wgPeerStatusResponseEntry{
			HostID:         s.HostID,
			Iface:          s.Iface,
			IfacePublicKey: s.IfacePublicKey,
			RunID:          s.RunID,
			RecordedAt:     s.RecordedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			Data:           s.Data,
		})
	}
	c.JSON(http.StatusOK, gin.H{"samples": out})
}

// internalWGPeerStatusPushReq — POST body. Mirrors the wire shape of
// the agent's `skill.event{metric, wg_peer_status}` Data payload, plus
// the routing keys (host_id, iface) that the cache is indexed by.
//
// Used by sibling plugins running on the wireguard host itself (e.g.
// polar-wg-svc on a hub box) to push their local `wg show <iface>
// dump` snapshot directly to the cache, bypassing the per-host
// polar-agent. Same cache, same readers — the only difference is the
// transport (HMAC plugin POST vs agent WS).
type internalWGPeerStatusPushReq struct {
	HostID         string         `json:"host_id"` // "" => synthesise from plugin name
	Iface          string         `json:"iface"`
	IfacePublicKey string         `json:"iface_public_key"`
	Data           map[string]any `json:"data"` // same shape as agent's data payload
}

func (s *Server) handleInternalWGPeerStatusPush(c *gin.Context) {
	if s.wgPeerCache == nil {
		internalErr(c, http.StatusServiceUnavailable, "wg_peer_cache not initialised")
		return
	}
	var req internalWGPeerStatusPushReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	if strings.TrimSpace(req.Iface) == "" || strings.TrimSpace(req.IfacePublicKey) == "" {
		internalErr(c, http.StatusBadRequest, "iface + iface_public_key required")
		return
	}
	hostID := strings.TrimSpace(req.HostID)
	if hostID == "" {
		// Synthesise a stable id from the plugin name so the cache row
		// is identifiable and won't collide with agent-sourced entries
		// (which always carry a real host_id).
		plugin, _ := c.Get("plugin_name")
		if pluginStr, _ := plugin.(string); pluginStr != "" {
			hostID = "plugin:" + pluginStr
		} else {
			hostID = "plugin:unknown"
		}
	}
	s.wgPeerCache.upsert(wgPeerSample{
		HostID:         hostID,
		Iface:          strings.TrimSpace(req.Iface),
		IfacePublicKey: strings.TrimSpace(req.IfacePublicKey),
		RecordedAt:     time.Now().UTC(),
		Data:           req.Data,
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
