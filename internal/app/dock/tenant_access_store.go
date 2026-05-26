package dock

// tenant_access_store.go — typed helpers for the
// teams.is_root flag + workspace_plugin_access table.
// See doc/arch/open-platform.md.

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// isRootWorkspace returns true when the given workspace is the
// platform operator's singleton tenant (teams.is_root = TRUE). Used
// by write-side gates that should only apply to non-root tenants.
//
// Returns false (no error) if no team is flagged as root yet — that's
// the post-migration / pre-seed state. Callers must handle "no root
// yet" gracefully (gate stays closed by default, which is the safe
// direction for `shared` and plugin access).
func (s *Server) isRootWorkspace(workspaceID string) (bool, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return false, nil
	}
	var isRoot bool
	err := s.db.QueryRow(
		`SELECT is_root FROM teams WHERE id = $1`, workspaceID,
	).Scan(&isRoot)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return isRoot, nil
}

// workspaceCanUsePlugin returns true when the workspace is allowed
// to use the named plugin. Root workspace bypasses the check (always
// allowed). For non-root, requires a workspace_plugin_access row
// with enabled=TRUE.
//
// Closed-by-default: missing row = denied.
func (s *Server) workspaceCanUsePlugin(workspaceID, pluginName string) (bool, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(pluginName) == "" {
		return false, nil
	}
	isRoot, err := s.isRootWorkspace(workspaceID)
	if err != nil {
		return false, err
	}
	if isRoot {
		return true, nil
	}
	var enabled bool
	err = s.db.QueryRow(
		`SELECT enabled FROM workspace_plugin_access
		  WHERE workspace_id = $1 AND plugin_name = $2`,
		workspaceID, pluginName,
	).Scan(&enabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return enabled, nil
}

// WorkspacePluginAccess is one row of the workspace_plugin_access
// table — used by the admin list endpoint + the SDK response shape.
type WorkspacePluginAccess struct {
	WorkspaceID     string    `json:"workspace_id"`
	PluginName      string    `json:"plugin_name"`
	Enabled         bool      `json:"enabled"`
	GrantedAt       time.Time `json:"granted_at"`
	GrantedByUserID string    `json:"granted_by_user_id,omitempty"`
}

// upsertWorkspacePluginAccess flips a workspace's access for one
// plugin. INSERT on first call, UPDATE on subsequent. Returns the
// resulting row.
func (s *Server) upsertWorkspacePluginAccess(workspaceID, pluginName string, enabled bool, grantedByUserID string) (*WorkspacePluginAccess, error) {
	now := time.Now().UTC()
	row := s.db.QueryRow(
		`INSERT INTO workspace_plugin_access (
		    workspace_id, plugin_name, enabled, granted_at, granted_by_user_id
		 ) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (workspace_id, plugin_name) DO UPDATE
		    SET enabled = EXCLUDED.enabled,
		        granted_at = EXCLUDED.granted_at,
		        granted_by_user_id = EXCLUDED.granted_by_user_id
		 RETURNING workspace_id, plugin_name, enabled, granted_at, granted_by_user_id`,
		workspaceID, pluginName, enabled, now, grantedByUserID,
	)
	out := &WorkspacePluginAccess{}
	if err := row.Scan(&out.WorkspaceID, &out.PluginName, &out.Enabled, &out.GrantedAt, &out.GrantedByUserID); err != nil {
		return nil, err
	}
	return out, nil
}

// listWorkspacePluginAccess returns every access row for one
// workspace. Plugins with no row are omitted (= effectively
// disabled). Used by the admin UI to render the current state.
func (s *Server) listWorkspacePluginAccess(workspaceID string) ([]WorkspacePluginAccess, error) {
	rows, err := s.db.Query(
		`SELECT workspace_id, plugin_name, enabled, granted_at, granted_by_user_id
		   FROM workspace_plugin_access
		  WHERE workspace_id = $1
		  ORDER BY plugin_name`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WorkspacePluginAccess, 0)
	for rows.Next() {
		var r WorkspacePluginAccess
		if err := rows.Scan(&r.WorkspaceID, &r.PluginName, &r.Enabled, &r.GrantedAt, &r.GrantedByUserID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
