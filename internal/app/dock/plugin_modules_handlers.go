package dock

// plugin_modules_handlers.go — Phase 1-A: admin CRUD for the plugin
// registry. All routes gated by AuthMiddleware + AdminMiddleware (see
// app.go wiring). The plaintext key is returned ONCE on create + on
// rotate; admins must hand it to the plugin operator out-of-band.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// handleAdminPluginModuleList — GET /api/admin/plugin-modules.
func (s *Server) handleAdminPluginModuleList(c *gin.Context) {
	items, err := s.listPluginModules()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_modules.list_failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"plugins": items})
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
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
