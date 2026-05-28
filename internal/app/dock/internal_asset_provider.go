package dock

// internal_asset_provider.go — POST /internal/v1/assets/provider-heartbeat
// — HMAC-plugin-authed; the polar-assets-svc plugin calls this every
// 60s to keep its asset_providers.last_seen_at fresh. Separate from
// the generic /internal/v1/plugin-registry/heartbeat because the
// row identity is by `slug` not by plugin name (one polar-assets-svc
// process can register as multiple providers if needed).

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleInternalAssetProviderHeartbeat(c *gin.Context) {
	var body struct {
		Slug string `json:"slug"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug required"})
		return
	}
	// Confirm the slug exists; touchAssetProviderLastSeen no-ops on
	// missing rows so we return a clearer error.
	p, err := s.getAssetProviderBySlug(body.Slug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown provider slug — admin must register first"})
		return
	}
	if err := s.touchAssetProviderLastSeen(body.Slug); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ack": true})
}
