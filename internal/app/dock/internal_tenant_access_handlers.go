package dock

// internal_tenant_access_handlers.go — plugin-facing
// /internal/v1/workspace-plugin-access endpoint. Plugins call this
// at request-time (after dock-verified workspace_id is in scope) to
// decide whether the caller's workspace may use this plugin.
//
// Closed-by-default: missing row OR enabled=false → 200 with
// {"enabled": false}. Root team always returns {"enabled": true}.
//
// Plugin-side caching is the plugin's responsibility — recommended
// 60s TTL since access changes are rare (admin grants once, leaves
// it for weeks).

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleInternalWorkspacePluginAccess(c *gin.Context) {
	workspaceID := strings.TrimSpace(c.Query("workspace_id"))
	pluginName := strings.TrimSpace(c.Query("plugin"))
	if workspaceID == "" || pluginName == "" {
		internalErr(c, http.StatusBadRequest, "workspace_id and plugin required")
		return
	}
	enabled, err := s.workspaceCanUsePlugin(workspaceID, pluginName)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "access lookup failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"workspace_id": workspaceID,
		"plugin":       pluginName,
		"enabled":      enabled,
	})
}
