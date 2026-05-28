package dock

// asset_provider_handlers.go — admin CRUD for provider registry +
// the public-side stats helper used by the admin UI.
//
//   POST   /api/admin/asset-providers      create (returns plaintext token once)
//   GET    /api/admin/asset-providers      list
//   DELETE /api/admin/asset-providers/:id  remove
//
// All admin-gated. The HMAC token is returned plaintext on create
// only — same pattern as plugin_modules at line 26-75 of
// plugin_modules_handlers.go.

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleAssetProviderAdminCreate(c *gin.Context) {
	var body struct {
		Slug       string `json:"slug"`
		BaseURL    string `json:"base_url"`
		Region     string `json:"region"`
		CapacityGB int    `json:"capacity_gb"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	in := &AssetProvider{
		Slug:       body.Slug,
		BaseURL:    body.BaseURL,
		Region:     body.Region,
		CapacityGB: body.CapacityGB,
	}
	out, err := s.insertAssetProvider(in)
	if err != nil {
		// pq unique_violation on slug → 409
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "slug already registered"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Audit — redact the plaintext token in the audit payload (only
	// the admin who made the call ever sees the plaintext).
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "asset_provider_create",
		TargetKind: "asset_provider",
		TargetID:   strconv.FormatInt(out.ID, 10),
		Payload: map[string]any{
			"slug":        out.Slug,
			"base_url":    out.BaseURL,
			"region":      out.Region,
			"capacity_gb": out.CapacityGB,
			"hmac_token":  "REDACTED",
		},
	})
	// Return the row with HMACToken populated for the one-time display.
	c.JSON(http.StatusCreated, gin.H{
		"provider": out,
		"hint":     "POLAR_ASSETS_DOCK_HMAC_SECRET on the provider box must be set to hmac_token below. Save it now — it won't be shown again on list calls.",
	})
}

func (s *Server) handleAssetProviderAdminList(c *gin.Context) {
	rows, err := s.listAssetProviders()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"providers": rows})
}

func (s *Server) handleAssetProviderAdminDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id invalid"})
		return
	}
	if err := s.deleteAssetProvider(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "asset_provider_delete",
		TargetKind: "asset_provider",
		TargetID:   strconv.FormatInt(id, 10),
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
