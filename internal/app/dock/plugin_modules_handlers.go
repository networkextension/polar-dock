package dock

// plugin_modules_handlers.go — Phase 1-A: admin CRUD for the plugin
// registry. All routes gated by AuthMiddleware + AdminMiddleware (see
// app.go wiring). The plaintext key is returned ONCE on create + on
// rotate; admins must hand it to the plugin operator out-of-band.

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// handleAdminPluginModuleList — GET /api/admin/plugin-modules. Each module
// is enriched with latest_version (newest published stable build) and
// update_available so the UI can offer a one-click roll (Track 3, OTA).
func (s *Server) handleAdminPluginModuleList(c *gin.Context) {
	items, err := s.listPluginModules()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_modules.list_failed"})
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, m := range items {
		latest, err := s.latestPluginReleaseVersion(m.Name)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_releases.lookup_failed"})
			return
		}
		out = append(out, map[string]any{
			"name":                 m.Name,
			"display_name":         m.DisplayName,
			"enabled":              m.Enabled,
			"endpoint":             m.Endpoint,
			"last_heartbeat":       m.LastHeartbeatAt,
			"version":              m.Version,
			"published":            m.Published,
			"accepts_applications": m.AcceptsApps,
			"created_at":           m.CreatedAt,
			"target_version":       m.TargetVersion,
			"latest_version":       latest,
			"update_available":     latest != "" && latest != m.Version,
		})
	}
	c.JSON(http.StatusOK, gin.H{"plugins": out})
}

// handleAdminPluginModulePublishRelease — POST /api/admin/plugin-modules/:name/releases
// Records a published binary's manifest. The binary file is rsync'd to the
// dist dir out-of-band (publish-module.sh); this just registers the row so
// the heartbeat resolver and binary handler can find it.
func (s *Server) handleAdminPluginModulePublishRelease(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	var req struct {
		Version     string `json:"version" binding:"required"`
		OS          string `json:"os" binding:"required"`
		Arch        string `json:"arch" binding:"required"`
		SHA256      string `json:"sha256" binding:"required"`
		DistRelPath string `json:"dist_rel_path" binding:"required"`
		Channel     string `json:"channel"`
		Notes       string `json:"notes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json: version, os, arch, sha256, dist_rel_path required"})
		return
	}
	rel := PluginRelease{
		Name: name, Version: req.Version, OS: req.OS, Arch: req.Arch,
		SHA256: req.SHA256, DistRelPath: req.DistRelPath, Channel: req.Channel, Notes: req.Notes,
	}
	if err := s.insertPluginRelease(rel); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plugin_releases.publish_failed: " + err.Error()})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "plugin_module.publish_release",
		TargetKind: "plugin_module",
		TargetID:   name,
		Payload: map[string]any{"after": map[string]any{
			"version": req.Version, "os": strings.ToLower(req.OS), "arch": strings.ToLower(req.Arch),
			"sha256": strings.ToLower(req.SHA256), "channel": rel.Channel,
		}},
	})
	c.JSON(http.StatusCreated, gin.H{"ok": true, "name": name, "version": req.Version})
}

// handleAdminPluginModuleSetUpdate — POST /api/admin/plugin-modules/:name/update
// Pins target_version (the UI's 更新 button). The module rolls to it on its
// next heartbeat (≤60s). Empty version clears the pin (follow latest).
func (s *Server) handleAdminPluginModuleSetUpdate(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	var req struct {
		Version string `json:"version"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	ok, err := s.setPluginTargetVersion(name, req.Version)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_modules.set_target_failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatConfig,
		Action:     "plugin_module.set_target_version",
		TargetKind: "plugin_module",
		TargetID:   name,
		Payload:    map[string]any{"after": map[string]any{"target_version": strings.TrimSpace(req.Version)}},
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "target_version": strings.TrimSpace(req.Version)})
}

// handleAdminPluginModuleCreate — POST /api/admin/plugin-modules.
// Returns 201 with plaintext key (only chance to capture it).
func (s *Server) handleAdminPluginModuleCreate(c *gin.Context) {
	var req struct {
		Name        string `json:"name" binding:"required"`
		DisplayName string `json:"display_name"`
		Endpoint    string `json:"endpoint"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	item, err := s.insertPluginModule(CreatePluginModuleInput{
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Endpoint:    req.Endpoint,
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") ||
			strings.Contains(strings.ToLower(err.Error()), "unique") ||
			strings.Contains(strings.ToLower(err.Error()), "primary key") {
			c.JSON(http.StatusConflict, gin.H{"error": "plugin name already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_modules.create_failed"})
		return
	}
	// Audit: redact plaintext token before logging the row. The plaintext
	// is only on the response struct for the admin to copy once.
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "plugin_module.create",
		TargetKind: "plugin_module",
		TargetID:   item.Name,
		Payload: map[string]any{
			"after": map[string]any{
				"name":         item.Name,
				"display_name": item.DisplayName,
				"enabled":      item.Enabled,
				"endpoint":     item.Endpoint,
				"created_at":   item.CreatedAt,
			},
		},
	})
	c.JSON(http.StatusCreated, gin.H{"plugin": item})
}

// handleAdminPluginModuleEnable — PATCH /api/admin/plugin-modules/:name/enable
func (s *Server) handleAdminPluginModuleEnable(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	ok, err := s.setPluginModuleEnabled(name, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_modules.enable_failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "plugin_module.enable",
		TargetKind: "plugin_module",
		TargetID:   name,
		Payload: map[string]any{
			"after": map[string]any{"enabled": true},
		},
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleAdminPluginModuleDisable — PATCH /api/admin/plugin-modules/:name/disable
func (s *Server) handleAdminPluginModuleDisable(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	ok, err := s.setPluginModuleEnabled(name, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_modules.disable_failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "plugin_module.disable",
		TargetKind: "plugin_module",
		TargetID:   name,
		Payload: map[string]any{
			"after": map[string]any{"enabled": false},
		},
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleAdminPluginModuleRotateKey — POST /api/admin/plugin-modules/:name/rotate-key
// Returns the new plaintext (only chance to capture it).
func (s *Server) handleAdminPluginModuleRotateKey(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	plaintext, ok, err := s.rotatePluginModuleKey(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_modules.rotate_failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found"})
		return
	}
	// Audit: NEVER log the plaintext. Store the sha256 prefix of the new
	// key so a future leak investigation can pin the rotation event, plus
	// the rotation timestamp.
	newHash := hashPluginToken(plaintext)
	hashPrefix := newHash
	if len(hashPrefix) > 12 {
		hashPrefix = hashPrefix[:12]
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatConfig,
		Action:     "plugin_module.rotate_key",
		TargetKind: "plugin_module",
		TargetID:   name,
		Payload: map[string]any{
			"new_key_sha256_prefix": hashPrefix,
			"rotated_at":            time.Now().UTC(),
		},
	})
	c.JSON(http.StatusOK, gin.H{"plaintext": plaintext})
}

// handleAdminPluginModuleDelete — DELETE /api/admin/plugin-modules/:name
func (s *Server) handleAdminPluginModuleDelete(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	ok, err := s.deletePluginModule(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_modules.delete_failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "plugin_module.delete",
		TargetKind: "plugin_module",
		TargetID:   name,
		Payload: map[string]any{
			"before": map[string]any{"name": name},
		},
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
