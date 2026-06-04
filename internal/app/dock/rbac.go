package dock

// RBAC foundation (learning from UniLake's role→permission model, adapted to
// dock's workspace scoping). Four tables: a platform permission catalog,
// roles (global system roles + per-workspace custom roles), role↔permission,
// and member↔role. A central Can(user, workspace, permission) resolves a
// user's effective permissions.
//
// BEHAVIOR-PRESERVING: until custom roles are assigned, Can() bridges to the
// existing model — a member's legacy team_members.role (owner/admin/member/
// viewer) maps to the same-named global system role, and owner/admin/
// site-admin short-circuit to full access. So existing checks keep working
// while fine-grained capabilities become available incrementally.

import (
	"context"
	"fmt"

	"github.com/lib/pq"
)

// ── schema ────────────────────────────────────────────────────────────

func (s *Server) ensureRBACSchema(ctx context.Context) error {
	// Runs after the main schema (teams/users exist). FKs reference tables
	// created earlier; CREATE order is permissions → roles → joins.
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS rbac_permissions (
  id          BIGSERIAL PRIMARY KEY,
  key         TEXT NOT NULL UNIQUE,        -- 'lawyer:doc:write' | 'platform:admin'
  module      TEXT NOT NULL,               -- 'lawyer' | 'platform' | 'team' | ...
  action      TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS rbac_roles (
  id           BIGSERIAL PRIMARY KEY,
  workspace_id TEXT REFERENCES teams(id) ON DELETE CASCADE,  -- NULL = global system role
  key          TEXT NOT NULL,              -- 'owner'|'admin'|'member'|'viewer'|<custom>
  name         TEXT NOT NULL,
  is_system    BOOLEAN NOT NULL DEFAULT FALSE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS rbac_roles_ws_key ON rbac_roles (COALESCE(workspace_id,''), key);

CREATE TABLE IF NOT EXISTS rbac_role_permissions (
  role_id       BIGINT NOT NULL REFERENCES rbac_roles(id) ON DELETE CASCADE,
  permission_id BIGINT NOT NULL REFERENCES rbac_permissions(id) ON DELETE CASCADE,
  PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE IF NOT EXISTS rbac_member_roles (
  workspace_id TEXT   NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  user_id      TEXT   NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role_id      BIGINT NOT NULL REFERENCES rbac_roles(id) ON DELETE CASCADE,
  granted_by   TEXT   NOT NULL DEFAULT '',
  granted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, user_id, role_id)
);
CREATE INDEX IF NOT EXISTS rbac_member_roles_user ON rbac_member_roles (user_id, workspace_id);
`)
	if err != nil {
		return fmt.Errorf("rbac schema: %w", err)
	}
	return s.seedRBAC(ctx)
}

// rbacPermissionCatalog — the starter capability set. Modules contribute
// their own; this is the platform baseline. Fine-grained per-module actions
// can be added over time.
var rbacPermissionCatalog = []struct{ key, module, action, desc string }{
	{"platform:admin", "platform", "admin", "站点级管理"},
	{"team:manage", "team", "manage", "管理工作区成员与角色"},
	{"llm:admin", "llm", "admin", "管理 LLM 配置（含标平台级）"},
	{"lawyer:use", "lawyer", "use", "访问律师模块"},
	{"lawyer:doc:write", "lawyer", "doc:write", "上传/删除案件文档"},
	{"lawyer:ai:run", "lawyer", "ai:run", "运行诊断/矩阵/策略/文书"},
	{"lawyer:kb:admin", "lawyer", "kb:admin", "管理法条库 A库"},
}

// systemRolePerms — what each global system role grants. owner/admin are
// full-access (short-circuited in Can); listed here for queryability.
var systemRolePerms = map[string][]string{
	"member": {"lawyer:use", "lawyer:doc:write", "lawyer:ai:run"},
	"viewer": {"lawyer:use"},
}

func (s *Server) seedRBAC(ctx context.Context) error {
	for _, p := range rbacPermissionCatalog {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO rbac_permissions (key, module, action, description) VALUES ($1,$2,$3,$4)
			 ON CONFLICT (key) DO NOTHING`, p.key, p.module, p.action, p.desc); err != nil {
			return fmt.Errorf("seed permission %s: %w", p.key, err)
		}
	}
	// Global system roles.
	for _, r := range []struct{ key, name string }{
		{"owner", "所有者"}, {"admin", "管理员"}, {"member", "成员"}, {"viewer", "只读"},
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO rbac_roles (workspace_id, key, name, is_system) VALUES (NULL,$1,$2,TRUE)
			 ON CONFLICT (COALESCE(workspace_id,''), key) DO NOTHING`, r.key, r.name); err != nil {
			return fmt.Errorf("seed role %s: %w", r.key, err)
		}
	}
	// owner/admin → all permissions; member/viewer → their explicit set.
	for roleKey, keys := range map[string][]string{
		"owner": allPermKeys(), "admin": allPermKeys(),
		"member": systemRolePerms["member"], "viewer": systemRolePerms["viewer"],
	} {
		for _, pk := range keys {
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO rbac_role_permissions (role_id, permission_id)
				 SELECT r.id, p.id FROM rbac_roles r, rbac_permissions p
				  WHERE r.workspace_id IS NULL AND r.key=$1 AND p.key=$2
				 ON CONFLICT DO NOTHING`, roleKey, pk); err != nil {
				return fmt.Errorf("seed role-perm %s/%s: %w", roleKey, pk, err)
			}
		}
	}
	return nil
}

func allPermKeys() []string {
	out := make([]string, 0, len(rbacPermissionCatalog))
	for _, p := range rbacPermissionCatalog {
		out = append(out, p.key)
	}
	return out
}

// ── resolution ────────────────────────────────────────────────────────

// effectiveRoleKeys returns the role keys a user holds in a workspace:
// explicit rbac_member_roles PLUS the legacy team_members.role (mapped to the
// same-named global system role) so existing members work with no migration.
func (s *Server) effectiveRoleKeys(userID, workspaceID string) (map[string]bool, error) {
	roles := map[string]bool{}
	// Legacy team membership role.
	var teamRole string
	if err := s.db.QueryRow(
		`SELECT role FROM team_members WHERE team_id=$1 AND user_id=$2`, workspaceID, userID).Scan(&teamRole); err == nil && teamRole != "" {
		roles[teamRole] = true
	}
	// Explicit RBAC role assignments (global or this workspace's).
	rows, err := s.db.Query(
		`SELECT rr.key FROM rbac_member_roles mr JOIN rbac_roles rr ON rr.id=mr.role_id
		  WHERE mr.user_id=$1 AND mr.workspace_id=$2`, userID, workspaceID)
	if err != nil {
		return roles, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			roles[k] = true
		}
	}
	return roles, rows.Err()
}

// Can reports whether the user may perform permKey in the workspace.
// Site-admin and owner/admin roles short-circuit to full access (matching
// today's behavior); member/viewer (and custom roles) are checked against
// rbac_role_permissions.
func (s *Server) Can(userID, workspaceID, permKey string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	if ok, _ := s.IsSiteAdmin(userID); ok {
		return true, nil
	}
	roles, err := s.effectiveRoleKeys(userID, workspaceID)
	if err != nil {
		return false, err
	}
	if roles["owner"] || roles["admin"] {
		return true, nil
	}
	if len(roles) == 0 {
		return false, nil
	}
	roleKeys := make([]string, 0, len(roles))
	for k := range roles {
		roleKeys = append(roleKeys, k)
	}
	// Does ANY held role (global or workspace-scoped) grant permKey?
	var n int
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM rbac_role_permissions rp
		   JOIN rbac_roles rr ON rr.id=rp.role_id
		   JOIN rbac_permissions p ON p.id=rp.permission_id
		  WHERE p.key=$1 AND rr.key = ANY($2)
		    AND (rr.workspace_id IS NULL OR rr.workspace_id=$3)`,
		permKey, pq.Array(roleKeys), workspaceID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// effectivePermissions returns the full permission-key set a user has in a
// workspace (for surfacing to the UI so it can gate affordances).
func (s *Server) effectivePermissions(userID, workspaceID string) ([]string, error) {
	if ok, _ := s.IsSiteAdmin(userID); ok {
		return allPermKeys(), nil
	}
	roles, err := s.effectiveRoleKeys(userID, workspaceID)
	if err != nil {
		return nil, err
	}
	if roles["owner"] || roles["admin"] {
		return allPermKeys(), nil
	}
	if len(roles) == 0 {
		return []string{}, nil
	}
	roleKeys := make([]string, 0, len(roles))
	for k := range roles {
		roleKeys = append(roleKeys, k)
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT p.key FROM rbac_role_permissions rp
		   JOIN rbac_roles rr ON rr.id=rp.role_id
		   JOIN rbac_permissions p ON p.id=rp.permission_id
		  WHERE rr.key = ANY($1) AND (rr.workspace_id IS NULL OR rr.workspace_id=$2)`,
		pq.Array(roleKeys), workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			out = append(out, k)
		}
	}
	return out, rows.Err()
}
