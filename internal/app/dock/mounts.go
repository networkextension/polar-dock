package dock

// Workspace resource mounts — the 南北向 (sharing) axis of the workspace
// resource-visibility model. A mount makes resource(s) from a SOURCE workspace
// visible in a TARGET workspace without copying. See
// doc/arch/workspace-resource-visibility.md.
//
// P0: schema + CRUD + a resolver. Consumption is opt-in per resource kind and
// flag-gated (POLAR_WORKSPACE_MOUNTS); LLM-config listing is the reference
// consumer. Default off = behavior-preserving.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// ensureMountsSchema creates workspace_mounts (idempotent, boot-time).
func (s *Server) ensureMountsSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS workspace_mounts (
  id                  BIGSERIAL PRIMARY KEY,
  workspace_id        TEXT NOT NULL,            -- target: where resources become visible
  source_workspace_id TEXT NOT NULL,            -- source: where the resources live
  resource_kind       TEXT NOT NULL,            -- 'llm_config'|'asset'|'prompt'|'*'
  source_resource_id  TEXT,                     -- NULL = all of that kind in source
  mode                TEXT NOT NULL DEFAULT 'read',  -- 'read'|'readwrite'
  created_by          TEXT NOT NULL DEFAULT '',
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CHECK (workspace_id <> source_workspace_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS workspace_mounts_uniq
  ON workspace_mounts (workspace_id, source_workspace_id, resource_kind, COALESCE(source_resource_id, ''));
CREATE INDEX IF NOT EXISTS workspace_mounts_target
  ON workspace_mounts (workspace_id, resource_kind);`)
	return err
}

// mountedSourceWorkspaceIDs returns the source workspace ids whose `kind`
// resources are mounted into targetWorkspaceID (matching resource_kind = kind
// OR '*'). A consuming list query unions these with the target's own rows.
// Returns an empty slice when mounts are disabled or none exist.
func (s *Server) mountedSourceWorkspaceIDs(targetWorkspaceID, kind string) ([]string, error) {
	if !s.workspaceMounts || targetWorkspaceID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT source_workspace_id FROM workspace_mounts
		  WHERE workspace_id = $1 AND (resource_kind = $2 OR resource_kind = '*')`,
		targetWorkspaceID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil && id != "" {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// isWorkspaceMember reports whether the user holds any effective role in the
// workspace (legacy team_members.role or an explicit rbac_member_roles grant).
func (s *Server) isWorkspaceMember(userID, workspaceID string) bool {
	if ok, _ := s.IsSiteAdmin(userID); ok {
		return true
	}
	roles, err := s.effectiveRoleKeys(userID, workspaceID)
	return err == nil && len(roles) > 0
}

// mountOut is one mount row for the management API.
type mountOut struct {
	ID                int64  `json:"id"`
	WorkspaceID       string `json:"workspace_id"`
	SourceWorkspaceID string `json:"source_workspace_id"`
	ResourceKind      string `json:"resource_kind"`
	SourceResourceID  string `json:"source_resource_id,omitempty"`
	Mode              string `json:"mode"`
	CreatedBy         string `json:"created_by"`
	CreatedAt         string `json:"created_at"`
}

// GET /api/teams/:id/mounts — list mounts INTO this workspace. team:manage.
func (s *Server) handleListMounts(c *gin.Context) {
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
		`SELECT id, workspace_id, source_workspace_id, resource_kind,
		        COALESCE(source_resource_id, ''), mode, created_by, created_at
		   FROM workspace_mounts WHERE workspace_id = $1 ORDER BY id`, teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	defer rows.Close()
	out := []mountOut{}
	for rows.Next() {
		var m mountOut
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.SourceWorkspaceID, &m.ResourceKind,
			&m.SourceResourceID, &m.Mode, &m.CreatedBy, &m.CreatedAt); err == nil {
			out = append(out, m)
		}
	}
	c.JSON(http.StatusOK, gin.H{"mounts": out})
}

// POST /api/teams/:id/mounts {source_workspace_id, resource_kind, source_resource_id?, mode?}
// Mount source resources into this workspace. Requires team:manage on the
// target AND membership in the source ("can't mount what you can't see").
func (s *Server) handleCreateMount(c *gin.Context) {
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
		SourceWorkspaceID string `json:"source_workspace_id"`
		ResourceKind      string `json:"resource_kind"`
		SourceResourceID  string `json:"source_resource_id"`
		Mode              string `json:"mode"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.SourceWorkspaceID == "" || body.ResourceKind == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source_workspace_id and resource_kind required"})
		return
	}
	if body.SourceWorkspaceID == teamID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能挂载自身"})
		return
	}
	switch body.ResourceKind {
	case "llm_config", "asset", "prompt", "*":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported resource_kind"})
		return
	}
	// Can't mount what you can't see: actor must belong to the source.
	if !s.isWorkspaceMember(actor, body.SourceWorkspaceID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "你不是来源工作区的成员，无法挂载其资源"})
		return
	}
	mode := body.Mode
	if mode != "readwrite" {
		mode = "read"
	}
	var srcResID interface{}
	if body.SourceResourceID != "" {
		srcResID = body.SourceResourceID
	}
	var id int64
	if err := s.db.QueryRow(
		`INSERT INTO workspace_mounts (workspace_id, source_workspace_id, resource_kind, source_resource_id, mode, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (workspace_id, source_workspace_id, resource_kind, COALESCE(source_resource_id,''))
		 DO UPDATE SET mode = EXCLUDED.mode RETURNING id`,
		teamID, body.SourceWorkspaceID, body.ResourceKind, srcResID, mode, actor).Scan(&id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category: AuditCatConfig, Action: "workspace.mount_create",
		TargetTeamID: teamID, TargetKind: "workspace_mount", TargetID: strconv.FormatInt(id, 10),
		Payload: map[string]any{"source_workspace_id": body.SourceWorkspaceID, "resource_kind": body.ResourceKind, "mode": mode},
	})
	c.JSON(http.StatusOK, gin.H{"id": id})
}

// DELETE /api/teams/:id/mounts/:mountId — remove a mount. team:manage.
func (s *Server) handleDeleteMount(c *gin.Context) {
	actor, ok := requireUserID(c)
	if !ok {
		return
	}
	teamID := c.Param("id")
	if can, _ := s.Can(actor, teamID, "team:manage"); !can {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要 team:manage 权限"})
		return
	}
	mountID, err := strconv.ParseInt(c.Param("mountId"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad mount id"})
		return
	}
	res, err := s.db.Exec(`DELETE FROM workspace_mounts WHERE id=$1 AND workspace_id=$2`, mountID, teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "挂载不存在"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category: AuditCatConfig, Action: "workspace.mount_delete",
		TargetTeamID: teamID, TargetKind: "workspace_mount", TargetID: strconv.FormatInt(mountID, 10),
	})
	c.JSON(http.StatusOK, gin.H{"deleted": mountID})
}

// llmConfigWorkspaceScope returns the workspace ids whose LLM configs are
// visible in workspaceID: the workspace itself plus any mounted sources (when
// POLAR_WORKSPACE_MOUNTS is on). Used to union mounted configs into the list.
func (s *Server) llmConfigWorkspaceScope(workspaceID string) []string {
	scope := []string{workspaceID}
	if mounted, err := s.mountedSourceWorkspaceIDs(workspaceID, "llm_config"); err == nil {
		scope = append(scope, mounted...)
	}
	return scope
}
