package dock

// plugin_ui_routes_handlers.go — GET /api/plugin-ui-routes.
//
// Returns the aggregated sidebar nav-entry list the UI uses to render
// dynamic plugin links. Source data: plugin_modules.ui_routes JSONB,
// populated by each plugin's heartbeat (see Sprint 2 — task #196).
//
// Filtering:
//   - Skip plugins with enabled=FALSE.
//   - For root team: return everything (admin sees all available
//     plugins regardless of grants).
//   - For non-root team: include a route only when
//     workspace_plugin_access(workspace_id, plugin_name).enabled=TRUE.
//   - admin_only flag inside each route is a hint for the UI; the
//     server doesn't drop those rows (UI hides them per session role).
//
// Auth: AuthMiddleware. workspace_id resolved from the active session.

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

// PluginUIRouteGroup is one plugin's contribution to the sidebar.
type PluginUIRouteGroup struct {
	Plugin      string `json:"plugin"`
	DisplayName string `json:"display_name"`
	// PublicBaseURL is the module's externally-reachable origin (e.g.
	// https://music.4950.store:2443). Lets native clients (iOS) reach a
	// module's own service per-endpoint instead of hardcoding it; empty
	// when the plugin declared none. Omitted from JSON when blank.
	PublicBaseURL string          `json:"public_base_url,omitempty"`
	Routes        json.RawMessage `json:"routes"`
}

// EndpointModule is one enabled module as reported by GET /api/modules.
type EndpointModule struct {
	Plugin        string `json:"plugin"`
	DisplayName   string `json:"display_name"`
	PublicBaseURL string `json:"public_base_url,omitempty"`
	HasUI         bool   `json:"has_ui"`
}

// handleModules returns every ENABLED module on this endpoint, NOT filtered by
// workspace_plugin_access. Native clients (iOS) use it to decide which feature
// tabs to surface — a tab should reflect "does this endpoint have the module"
// (e.g. music), independent of the active workspace's UI-sidebar grants (which
// /api/plugin-ui-routes enforces for the web). Authed; no workspace required.
func (s *Server) handleModules(c *gin.Context) {
	rows, err := s.db.Query(
		`SELECT name, display_name, COALESCE(public_base_url, ''), (ui_routes <> '[]'::jsonb)
		   FROM plugin_modules
		  WHERE enabled = TRUE
		  ORDER BY name`,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	defer rows.Close()
	modules := make([]EndpointModule, 0)
	for rows.Next() {
		var m EndpointModule
		if err := rows.Scan(&m.Plugin, &m.DisplayName, &m.PublicBaseURL, &m.HasUI); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			return
		}
		modules = append(modules, m)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"modules": modules})
}

func (s *Server) handlePluginUIRoutes(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	isRoot, err := s.isRootWorkspace(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	// Pull every enabled plugin with non-empty ui_routes. For non-
	// root callers, LEFT JOIN workspace_plugin_access and require
	// enabled=TRUE — single round-trip, no per-plugin extra check.
	var rows interface {
		Close() error
		Next() bool
		Scan(...interface{}) error
		Err() error
	}
	if isRoot {
		r, err := s.db.Query(
			`SELECT name, display_name, COALESCE(public_base_url, ''), ui_routes::text
			   FROM plugin_modules
			  WHERE enabled = TRUE AND ui_routes <> '[]'::jsonb
			  ORDER BY name`,
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			return
		}
		rows = r
	} else {
		r, err := s.db.Query(
			`SELECT p.name, p.display_name, COALESCE(p.public_base_url, ''), p.ui_routes::text
			   FROM plugin_modules p
			   JOIN workspace_plugin_access a
			     ON a.plugin_name = p.name
			    AND a.workspace_id = $1
			    AND a.enabled = TRUE
			  WHERE p.enabled = TRUE AND p.ui_routes <> '[]'::jsonb
			  ORDER BY p.name`,
			workspaceID,
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			return
		}
		rows = r
	}
	defer rows.Close()

	groups := make([]PluginUIRouteGroup, 0)
	for rows.Next() {
		var g PluginUIRouteGroup
		var raw string
		if err := rows.Scan(&g.Plugin, &g.DisplayName, &g.PublicBaseURL, &raw); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			return
		}
		g.Routes = json.RawMessage(raw)
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"workspace_id": workspaceID,
		"is_root":      isRoot,
		"groups":       groups,
	})
}
