package dock

// nav_handlers.go — GET /api/nav. The single source of truth for the
// left sidebar, shared across dock + every module subdomain. dock-ui's
// renderSidebar() REPLACES its static fallback with these groups, so this
// returns the COMPLETE menu: the dock-native groups (kept in sync with
// polar-dock-ui/src/lib/sidebar.ts NAV) plus a dynamic "插件" group built
// from registered plugins.
//
// Plugin links are absolute (plugin_modules.public_base_url + ui_routes
// path) so they don't 404 from the dock origin, and external:true so they
// open the module's own subdomain. Workspace gating mirrors
// /api/plugin-ui-routes: root sees all enabled plugins; non-root sees only
// those with an enabled workspace_plugin_access row.
//
// Auth: AuthMiddleware.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type navItem struct {
	Href     string `json:"href"`
	Label    string `json:"label"`
	Icon     string `json:"icon"`
	I18n     string `json:"i18n,omitempty"`
	Gate     string `json:"gate,omitempty"` // "admin" | "site-admin"
	External bool   `json:"external,omitempty"`
}

type navGroup struct {
	Label string    `json:"label,omitempty"`
	Items []navItem `json:"items"`
}

// baseNav mirrors polar-dock-ui/src/lib/sidebar.ts NAV. Keep in sync.
func baseNav() []navGroup {
	return []navGroup{
		{Items: []navItem{{Href: "/dashboard.html", Label: "Dashboard", Icon: "layout-dashboard"}}},
		{Label: "工作区", Items: []navItem{
			{Href: "/teams.html", Label: "工作区", I18n: "nav.teams", Icon: "users-round"},
			{Href: "/assets.html", Label: "资产", Icon: "folder"},
		}},
		{Label: "内容", Items: []navItem{
			{Href: "/posts.html", Label: "Posts", Icon: "message-square"},
			{Href: "/markdowns.html", Label: "Markdowns", Icon: "file-text"},
			{Href: "/creation.html", Label: "Creation", I18n: "nav.creation", Icon: "pen-line"},
			{Href: "/editor.html", Label: "Editor", Icon: "code"},
			{Href: "/skills.html", Label: "Skills", Icon: "package"},
		}},
		{Label: "协作", Items: []navItem{
			{Href: "/rooms.html", Label: "群聊", Icon: "users"},
			{Href: "/chat.html", Label: "Chat", Icon: "message-circle"},
		}},
		{Label: "AI", Items: []navItem{
			{Href: "/llm.html", Label: "LLM", Icon: "sparkles"},
			{Href: "/llm-config.html", Label: "LLM 配置", Icon: "sliders"},
			{Href: "/expense.html", Label: "家庭账本", Icon: "receipt"},
			{Href: "/llm-billing.html", Label: "LLM 计费", Icon: "dollar-sign", Gate: "admin"},
			{Href: "/llm-apps.html", Label: "LLM 申请", Icon: "inbox", Gate: "admin"},
			{Href: "/bots.html", Label: "Bot Management", I18n: "nav.bots", Icon: "bot", Gate: "admin"},
			{Href: "/prompts.html", Label: "Prompt 模板", Icon: "sparkles", Gate: "admin"},
		}},
		{Label: "设备", Items: []navItem{
			{Href: "/agents.html", Label: "Agents", I18n: "nav.agents", Icon: "cpu", Gate: "site-admin"},
		}},
		{Label: "管理", Items: []navItem{
			{Href: "/latch.html", Label: "Latch", I18n: "nav.latch", Icon: "settings", Gate: "admin"},
			{Href: "/invites.html", Label: "邀请码", Icon: "ticket", Gate: "admin"},
			{Href: "/admin-plugins.html", Label: "Plugin 注册", Icon: "puzzle", Gate: "admin"},
			{Href: "/admin-assets.html", Label: "资产管理（管理员）", Icon: "database", Gate: "site-admin"},
			{Href: "/admin-audit-log.html", Label: "审计日志", Icon: "clipboard-list", Gate: "site-admin"},
			{Href: "/proxy-tokens.html", Label: "API Tokens", Icon: "key", Gate: "admin"},
			{Href: "/grafana/", Label: "Grafana", Icon: "line-chart", Gate: "admin", External: true},
			{Href: "/admin.html", Label: "用户管理", I18n: "nav.userAdmin", Icon: "users-cog", Gate: "admin"},
			{Href: "/admin-rbac.html", Label: "角色与权限", Icon: "shield-check", Gate: "admin"},
		}},
	}
}

type navUIRoute struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Icon  string `json:"icon"`
}

func (s *Server) handleNav(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	isRoot, err := s.isRootWorkspace(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	groups := baseNav()

	pluginItems, err := s.pluginNavItems(workspaceID, isRoot)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if len(pluginItems) > 0 {
		groups = append(groups, navGroup{Label: "插件", Items: pluginItems})
	}

	c.JSON(http.StatusOK, gin.H{"groups": groups})
}

// pluginNavItems builds absolute, external sidebar links from registered
// plugins that declared a public_base_url + ui_routes, workspace-gated.
func (s *Server) pluginNavItems(workspaceID string, isRoot bool) ([]navItem, error) {
	var rows interface {
		Close() error
		Next() bool
		Scan(...interface{}) error
		Err() error
	}
	if isRoot {
		r, err := s.db.Query(`
			SELECT display_name, ui_routes::text, public_base_url
			  FROM plugin_modules
			 WHERE enabled = TRUE AND ui_routes <> '[]'::jsonb AND public_base_url <> ''
			 ORDER BY name`)
		if err != nil {
			return nil, err
		}
		rows = r
	} else {
		r, err := s.db.Query(`
			SELECT p.display_name, p.ui_routes::text, p.public_base_url
			  FROM plugin_modules p
			  JOIN workspace_plugin_access a
			    ON a.plugin_name = p.name AND a.workspace_id = $1 AND a.enabled = TRUE
			 WHERE p.enabled = TRUE AND p.ui_routes <> '[]'::jsonb AND p.public_base_url <> ''
			 ORDER BY p.name`, workspaceID)
		if err != nil {
			return nil, err
		}
		rows = r
	}
	defer rows.Close()

	items := make([]navItem, 0)
	for rows.Next() {
		var displayName, routesRaw, baseURL string
		if err := rows.Scan(&displayName, &routesRaw, &baseURL); err != nil {
			return nil, err
		}
		var routes []navUIRoute
		if err := json.Unmarshal([]byte(routesRaw), &routes); err != nil {
			continue // skip a plugin with malformed ui_routes rather than fail the whole nav
		}
		base := strings.TrimRight(baseURL, "/")
		for _, rt := range routes {
			label := strings.TrimSpace(rt.Label)
			if label == "" {
				label = displayName
			}
			icon := strings.TrimSpace(rt.Icon)
			if icon == "" {
				icon = "puzzle"
			}
			path := rt.Path
			if path == "" {
				path = "/"
			}
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			items = append(items, navItem{Href: base + path, Label: label, Icon: icon, External: true})
		}
	}
	return items, rows.Err()
}
