package dock

// Storage layer for R3 module-marketplace publishing + tenant access
// requests. Tables: plugin_modules (publish flags + api_routes columns)
// and plugin_access_requests, both declared in store.go's openDB()
// schema block. Handlers (admin publish, tenant request, approve) land
// in a follow-up PR and consume these helpers.

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	pluginAccessScopeModule = "module"
	pluginAccessScopeAPI    = "api"

	pluginAccessStatusPending  = "pending"
	pluginAccessStatusApproved = "approved"
	pluginAccessStatusDenied   = "denied"
)

// PluginAccessRequest is a tenant team-admin's request for a published
// module (whole module, or a specific API route within it).
type PluginAccessRequest struct {
	ID                int64      `json:"id"`
	WorkspaceID       string     `json:"workspace_id"`
	PluginName        string     `json:"plugin_name"`
	Scope             string     `json:"scope"`               // module | api
	APIRoute          string     `json:"api_route,omitempty"` // set when scope=api
	Status            string     `json:"status"`              // pending | approved | denied
	RequestedByUserID string     `json:"requested_by_user_id"`
	ReviewedByUserID  string     `json:"reviewed_by_user_id,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	ReviewedAt        *time.Time `json:"reviewed_at,omitempty"`
}

// normalizePluginAccessScope validates + normalizes a request scope and
// its api_route. Pure (no DB) so it's unit-testable and reusable by the
// handler layer. An api-scope request must name a route; a module-scope
// request must not. Returns the cleaned (scope, apiRoute) or an error.
func normalizePluginAccessScope(scope, apiRoute string) (string, string, error) {
	scope = strings.TrimSpace(strings.ToLower(scope))
	apiRoute = strings.TrimSpace(apiRoute)
	switch scope {
	case "", pluginAccessScopeModule:
		if apiRoute != "" {
			return "", "", errors.New("module-scope request must not carry an api_route")
		}
		return pluginAccessScopeModule, "", nil
	case pluginAccessScopeAPI:
		if apiRoute == "" {
			return "", "", errors.New("api-scope request requires an api_route")
		}
		return pluginAccessScopeAPI, apiRoute, nil
	default:
		return "", "", errors.New("invalid scope: must be 'module' or 'api'")
	}
}

// createPluginAccessRequest inserts a pending request. The partial
// unique index uniq_plugin_access_requests_open dedups open requests for
// the same (workspace, plugin, scope, route) — a duplicate surfaces as a
// unique-violation the handler maps to 409.
func (s *Server) createPluginAccessRequest(workspaceID, pluginName, scope, apiRoute, requestedByUserID, reason string) (*PluginAccessRequest, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	pluginName = strings.TrimSpace(pluginName)
	requestedByUserID = strings.TrimSpace(requestedByUserID)
	if workspaceID == "" || pluginName == "" || requestedByUserID == "" {
		return nil, errors.New("workspace_id, plugin_name and requested_by_user_id are required")
	}
	normScope, normRoute, err := normalizePluginAccessScope(scope, apiRoute)
	if err != nil {
		return nil, err
	}
	var r PluginAccessRequest
	err = s.db.QueryRow(`
		INSERT INTO plugin_access_requests
		    (workspace_id, plugin_name, scope, api_route, requested_by_user_id, reason)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, workspace_id, plugin_name, scope, api_route, status,
		          requested_by_user_id, reviewed_by_user_id, reason, created_at, reviewed_at`,
		workspaceID, pluginName, normScope, normRoute, requestedByUserID, strings.TrimSpace(reason),
	).Scan(&r.ID, &r.WorkspaceID, &r.PluginName, &r.Scope, &r.APIRoute, &r.Status,
		&r.RequestedByUserID, &r.ReviewedByUserID, &r.Reason, &r.CreatedAt, &nullTimePtr{&r.ReviewedAt})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// listPendingPluginAccessRequests returns all open requests, oldest
// first, for the site-admin approval queue.
func (s *Server) listPendingPluginAccessRequests() ([]PluginAccessRequest, error) {
	return s.queryPluginAccessRequests(
		`WHERE status = 'pending' ORDER BY created_at ASC`)
}

// listPluginAccessRequestsForWorkspace returns a team's own requests,
// newest first, for the tenant-side status view.
func (s *Server) listPluginAccessRequestsForWorkspace(workspaceID string) ([]PluginAccessRequest, error) {
	return s.queryPluginAccessRequests(
		`WHERE workspace_id = $1 ORDER BY created_at DESC`, workspaceID)
}

func (s *Server) queryPluginAccessRequests(where string, args ...interface{}) ([]PluginAccessRequest, error) {
	rows, err := s.db.Query(`
		SELECT id, workspace_id, plugin_name, scope, api_route, status,
		       requested_by_user_id, reviewed_by_user_id, reason, created_at, reviewed_at
		  FROM plugin_access_requests `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PluginAccessRequest{}
	for rows.Next() {
		var r PluginAccessRequest
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.PluginName, &r.Scope, &r.APIRoute, &r.Status,
			&r.RequestedByUserID, &r.ReviewedByUserID, &r.Reason, &r.CreatedAt, &nullTimePtr{&r.ReviewedAt}); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// getPluginAccessRequest fetches one request by id (nil if absent).
func (s *Server) getPluginAccessRequest(id int64) (*PluginAccessRequest, error) {
	reqs, err := s.queryPluginAccessRequests(`WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return nil, nil
	}
	return &reqs[0], nil
}

// setPluginAccessRequestStatus transitions a pending request to
// approved/denied and stamps the reviewer. Returns false if the row was
// not pending (already reviewed / missing), so the handler can 409.
func (s *Server) setPluginAccessRequestStatus(id int64, status, reviewerUserID string) (bool, error) {
	if status != pluginAccessStatusApproved && status != pluginAccessStatusDenied {
		return false, errors.New("status must be 'approved' or 'denied'")
	}
	res, err := s.db.Exec(`
		UPDATE plugin_access_requests
		   SET status = $2, reviewed_by_user_id = $3, reviewed_at = NOW()
		 WHERE id = $1 AND status = 'pending'`,
		id, status, strings.TrimSpace(reviewerUserID))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// setPluginModulePublished flips the 上架 flag. Returns false if no such
// module.
func (s *Server) setPluginModulePublished(name string, published bool) (bool, error) {
	return s.execPluginModuleFlag(`UPDATE plugin_modules SET published = $2 WHERE name = $1`, name, published)
}

// setPluginModuleAcceptsApplications flips the per-module
// "tenants may request" toggle. Returns false if no such module.
func (s *Server) setPluginModuleAcceptsApplications(name string, accepts bool) (bool, error) {
	return s.execPluginModuleFlag(`UPDATE plugin_modules SET accepts_applications = $2 WHERE name = $1`, name, accepts)
}

func (s *Server) execPluginModuleFlag(query, name string, val bool) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errors.New("name required")
	}
	res, err := s.db.Exec(query, name, val)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// nullTimePtr scans a nullable TIMESTAMPTZ into a **time.Time, leaving it
// nil on SQL NULL. Avoids threading sql.NullTime through every Scan.
type nullTimePtr struct{ dst **time.Time }

func (n *nullTimePtr) Scan(src interface{}) error {
	var nt sql.NullTime
	if err := nt.Scan(src); err != nil {
		return err
	}
	if nt.Valid {
		t := nt.Time
		*n.dst = &t
	} else {
		*n.dst = nil
	}
	return nil
}
