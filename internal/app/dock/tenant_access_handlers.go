package dock

// tenant_access_handlers.go — admin-only REST for managing
// workspace_plugin_access rows. Browser UI hits these; plugin svc
// uses the /internal/v1 variant.
//
// Routes (registered in app.go under /api/admin):
//   GET  /api/admin/workspace-plugin-access?workspace_id=<id>
//   PUT  /api/admin/workspace-plugin-access  { workspace_id, plugin, enabled }
//
// Auth: AuthMiddleware + AdminMiddleware. Returning a grant to the
// root team would be a no-op (root bypasses the check) but is
// allowed for forward-compat in case the bypass rule changes.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleAdminWorkspacePluginAccessList(c *gin.Context) {
	workspaceID := strings.TrimSpace(c.Query("workspace_id"))
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace_id required"})
		return
	}
	rows, err := s.listWorkspacePluginAccess(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rows": rows})
}

func (s *Server) handleAdminWorkspacePluginAccessUpsert(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	var req struct {
		WorkspaceID string `json:"workspace_id" binding:"required"`
		Plugin      string `json:"plugin" binding:"required"`
		Enabled     bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	workspaceID := strings.TrimSpace(req.WorkspaceID)
	plugin := strings.TrimSpace(req.Plugin)
	out, err := s.upsertWorkspacePluginAccess(
		workspaceID,
		plugin,
		req.Enabled,
		userIDStr,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:     AuditCatProvisioning,
		Action:       "workspace_plugin_access.set",
		TargetTeamID: workspaceID,
		TargetKind:   "team",
		TargetID:     workspaceID,
		Payload: map[string]any{
			"after": map[string]any{
				"plugin":  plugin,
				"enabled": req.Enabled,
			},
		},
	})
	c.JSON(http.StatusOK, gin.H{"row": out})
}
