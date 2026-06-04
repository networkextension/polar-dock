package dock

// RBAC HTTP surface (foundation slice): surface a user's effective
// permissions to the UI, expose the permission catalog, and let a workspace
// manager list + assign member roles. Role CRUD / a full permission-matrix
// editor (UniLake-style) is the next slice.

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
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
	// Resolve a global system role OR this workspace's own custom role.
	// Prefer the workspace-custom role when both share a key.
	var roleID int64
	if err := s.db.QueryRow(
		`SELECT id FROM rbac_roles WHERE (workspace_id IS NULL OR workspace_id=$2) AND key=$1
		 ORDER BY (workspace_id IS NULL) ASC LIMIT 1`, body.RoleKey, teamID).Scan(&roleID); err != nil {
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

// roleOut is one role in the management UI: system (global) or this
// workspace's custom role, with its granted permission keys.
type roleOut struct {
	ID          int64    `json:"id"`
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	IsSystem    bool     `json:"is_system"`
	IsGlobal    bool     `json:"is_global"`
	Permissions []string `json:"permissions"`
}

// GET /api/teams/:id/roles — global system roles + this workspace's custom
// roles, each with its permission keys (for the matrix UI).
func (s *Server) handleListRoles(c *gin.Context) {
	teamID := c.Param("id")
	rows, err := s.db.Query(
		`SELECT r.id, r.key, r.name, r.is_system, (r.workspace_id IS NULL) AS is_global,
		        COALESCE(array_agg(p.key) FILTER (WHERE p.key IS NOT NULL), '{}') AS perms
		   FROM rbac_roles r
		   LEFT JOIN rbac_role_permissions rp ON rp.role_id=r.id
		   LEFT JOIN rbac_permissions p ON p.id=rp.permission_id
		  WHERE r.workspace_id IS NULL OR r.workspace_id=$1
		  GROUP BY r.id ORDER BY (r.workspace_id IS NULL) DESC, r.key`, teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	defer rows.Close()
	out := []roleOut{}
	for rows.Next() {
		var r roleOut
		var perms pq.StringArray
		if err := rows.Scan(&r.ID, &r.Key, &r.Name, &r.IsSystem, &r.IsGlobal, &perms); err != nil {
			continue
		}
		r.Permissions = []string(perms)
		out = append(out, r)
	}
	c.JSON(http.StatusOK, gin.H{"roles": out})
}

// POST /api/teams/:id/roles  {key, name, permission_keys[]} — create a
// per-workspace custom role. Requires team:manage.
func (s *Server) handleCreateRole(c *gin.Context) {
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
		Key            string   `json:"key"`
		Name           string   `json:"name"`
		PermissionKeys []string `json:"permission_keys"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Key == "" || body.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key and name required"})
		return
	}
	var roleID int64
	if err := s.db.QueryRow(
		`INSERT INTO rbac_roles (workspace_id, key, name, is_system) VALUES ($1,$2,$3,FALSE)
		 ON CONFLICT (COALESCE(workspace_id,''), key) DO UPDATE SET name=EXCLUDED.name RETURNING id`,
		teamID, body.Key, body.Name).Scan(&roleID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	s.replaceRolePermissions(roleID, body.PermissionKeys)
	s.LogAdminAction(c, AdminAuditIn{Category: AuditCatConfig, Action: "rbac.role_create",
		TargetTeamID: teamID, TargetKind: "rbac_role", TargetID: body.Key,
		Payload: map[string]any{"permissions": body.PermissionKeys}})
	c.JSON(http.StatusOK, gin.H{"id": roleID, "key": body.Key})
}

// PUT /api/teams/:id/roles/:roleId  {name?, permission_keys[]} — replace a
// custom role's permission set. System/global roles are read-only.
func (s *Server) handleUpdateRole(c *gin.Context) {
	actor, ok := requireUserID(c)
	if !ok {
		return
	}
	teamID := c.Param("id")
	if can, _ := s.Can(actor, teamID, "team:manage"); !can {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要 team:manage 权限"})
		return
	}
	roleID, err := strconv.ParseInt(c.Param("roleId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad role id"})
		return
	}
	// Guard: must be this workspace's non-system role.
	var isSystem bool
	if err := s.db.QueryRow(`SELECT is_system FROM rbac_roles WHERE id=$1 AND workspace_id=$2`, roleID, teamID).Scan(&isSystem); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "自定义角色不存在"})
		return
	}
	if isSystem {
		c.JSON(http.StatusForbidden, gin.H{"error": "系统角色只读"})
		return
	}
	var body struct {
		Name           string   `json:"name"`
		PermissionKeys []string `json:"permission_keys"`
	}
	_ = c.ShouldBindJSON(&body)
	if body.Name != "" {
		_, _ = s.db.Exec(`UPDATE rbac_roles SET name=$2 WHERE id=$1`, roleID, body.Name)
	}
	s.replaceRolePermissions(roleID, body.PermissionKeys)
	s.LogAdminAction(c, AdminAuditIn{Category: AuditCatConfig, Action: "rbac.role_update",
		TargetTeamID: teamID, TargetKind: "rbac_role", TargetID: strconv.FormatInt(roleID, 10),
		Payload: map[string]any{"permissions": body.PermissionKeys}})
	c.JSON(http.StatusOK, gin.H{"id": roleID})
}

// DELETE /api/teams/:id/roles/:roleId — delete a custom role (cascades its
// permissions + member assignments). System roles cannot be deleted.
func (s *Server) handleDeleteRole(c *gin.Context) {
	actor, ok := requireUserID(c)
	if !ok {
		return
	}
	teamID := c.Param("id")
	if can, _ := s.Can(actor, teamID, "team:manage"); !can {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要 team:manage 权限"})
		return
	}
	roleID, err := strconv.ParseInt(c.Param("roleId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad role id"})
		return
	}
	res, err := s.db.Exec(`DELETE FROM rbac_roles WHERE id=$1 AND workspace_id=$2 AND is_system=FALSE`, roleID, teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "系统角色不可删除或角色不存在"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{Category: AuditCatConfig, Action: "rbac.role_delete",
		TargetTeamID: teamID, TargetKind: "rbac_role", TargetID: strconv.FormatInt(roleID, 10)})
	c.JSON(http.StatusOK, gin.H{"deleted": roleID})
}

// gateCan is the per-handler RBAC gate. When enforced is false it is a no-op
// (returns true) — preserving legacy behavior under a feature flag. When
// enforced is true it resolves the caller + active workspace and checks
// Can(permKey); on failure it writes 403 and returns false so the handler can
// `if !s.gateCan(c, "llm:admin", s.enforceLLMAdmin) { return }`.
func (s *Server) gateCan(c *gin.Context, permKey string, enforced bool) bool {
	if !enforced {
		return true
	}
	userID, ok := requireUserID(c)
	if !ok {
		return false
	}
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return false
	}
	can, err := s.Can(userID, workspaceID, permKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return false
	}
	if !can {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要 " + permKey + " 权限"})
		return false
	}
	return true
}

// replaceRolePermissions sets a role's permission set to exactly permKeys.
func (s *Server) replaceRolePermissions(roleID int64, permKeys []string) {
	_, _ = s.db.Exec(`DELETE FROM rbac_role_permissions WHERE role_id=$1`, roleID)
	for _, pk := range permKeys {
		_, _ = s.db.Exec(
			`INSERT INTO rbac_role_permissions (role_id, permission_id)
			 SELECT $1, id FROM rbac_permissions WHERE key=$2 ON CONFLICT DO NOTHING`, roleID, pk)
	}
}
