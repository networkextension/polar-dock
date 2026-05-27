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
