package dock

// RBAC HTTP surface (foundation slice): surface a user's effective
// permissions to the UI, expose the permission catalog, and let a workspace
// manager list + assign member roles. Role CRUD / a full permission-matrix
// editor (UniLake-style) is the next slice.

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GET /api/me/permissions — the caller's effective permission keys in the
// active workspace (X-Workspace-Id). The UI uses this to gate affordances.
func (s *Server) handleMyPermissions(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	perms, err := s.effectivePermissions(userID, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	siteAdmin, _ := s.IsSiteAdmin(userID)
	c.JSON(http.StatusOK, gin.H{"workspace_id": workspaceID, "permissions": perms, "site_admin": siteAdmin})
}

// GET /api/rbac/permissions — the platform permission catalog.
func (s *Server) handleRBACCatalog(c *gin.Context) {
	rows, err := s.db.Query(`SELECT key, module, action, description FROM rbac_permissions ORDER BY module, action`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var key, module, action, desc string
		if err := rows.Scan(&key, &module, &action, &desc); err == nil {
			out = append(out, gin.H{"key": key, "module": module, "action": action, "description": desc})
		}
	}
	c.JSON(http.StatusOK, gin.H{"permissions": out})
}

// GET /api/teams/:id/member-roles — each member's explicitly-assigned RBAC
// roles in the team (the legacy team_members.role still applies on top).
func (s *Server) handleListMemberRoles(c *gin.Context) {
	actor, ok := requireUserID(c)
	if !ok {
		return
	}
	teamID := c.Param("id")
	if can, _ := s.Can(actor, teamID, "team:manage"); !can {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要 team:manage 权限"})
		return
	}
	rows, err := s.db.Query(
		`SELECT mr.user_id, rr.key, rr.name FROM rbac_member_roles mr
		   JOIN rbac_roles rr ON rr.id=mr.role_id
		  WHERE mr.workspace_id=$1 ORDER BY mr.user_id`, teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var uid, key, name string
		if err := rows.Scan(&uid, &key, &name); err == nil {
			out = append(out, gin.H{"user_id": uid, "role_key": key, "role_name": name})
		}
	}
	c.JSON(http.StatusOK, gin.H{"member_roles": out})
}

// POST /api/teams/:id/member-roles  {user_id, role_key}
// Assign a (global system) role to a member. Requires team:manage.
func (s *Server) handleAssignMemberRole(c *gin.Context) {
	actor, ok := requireUserID(c)
	if !ok {
		return
	}
	teamID := c.Param("id")
	if can, _ := s.Can(actor, teamID, "team:manage"); !can {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要 team:manage 权限"})
		return
	}
	var body struct {
		UserID  string `json:"user_id"`
		RoleKey string `json:"role_key"`
		Revoke  bool   `json:"revoke"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.UserID == "" || body.RoleKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id and role_key required"})
		return
	}
	var roleID int64
	if err := s.db.QueryRow(`SELECT id FROM rbac_roles WHERE workspace_id IS NULL AND key=$1`, body.RoleKey).Scan(&roleID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown role"})
		return
	}
	if body.Revoke {
		_, _ = s.db.Exec(`DELETE FROM rbac_member_roles WHERE workspace_id=$1 AND user_id=$2 AND role_id=$3`, teamID, body.UserID, roleID)
	} else {
		if _, err := s.db.Exec(
			`INSERT INTO rbac_member_roles (workspace_id, user_id, role_id, granted_by)
			 VALUES ($1,$2,$3,$4) ON CONFLICT (workspace_id,user_id,role_id) DO NOTHING`,
			teamID, body.UserID, roleID, actor); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			return
		}
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category: AuditCatConfig, Action: "rbac.assign_role",
		TargetTeamID: teamID, TargetKind: "user", TargetID: body.UserID,
		Payload: map[string]any{"role_key": body.RoleKey, "revoke": body.Revoke},
	})
	c.JSON(http.StatusOK, gin.H{"user_id": body.UserID, "role_key": body.RoleKey, "revoked": body.Revoke})
}
