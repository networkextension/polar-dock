package dock

// plugin_nav_handlers.go — GET /api/nav.
//
// The platform's shared sidebar fragment: dock assembles ONE nav list that
// every page (dock-ui + each module subdomain) renders identically via
// polar-ui-common's mountPlatformNav().
//
//   - CORE: the dock-owned pages, static, as ABSOLUTE links to dock's own
//     public origin (s.publicBaseURL, e.g. https://zen.4950.store:2443) so
//     they resolve correctly even from a module subdomain.
//   - MODULES: each enabled plugin that declared a public_base_url + ui_routes
//     at heartbeat. Its routes become absolute links (base + path). This is the
//     self-registering part — add/remove/upgrade a module and the nav follows
//     (within the cache TTL). 3rd-party modules on any domain work, because the
//     module declares its own base URL.
//
// Access: AuthMiddleware. For non-root workspaces the module list is filtered
// by workspace_plugin_access (same gate as /api/plugin-ui-routes). admin /
// site-admin items carry a `gate` hint the client hides per /api/me — real
// enforcement is the API, this is cosmetic.
//
// Caching: the assembled module list is cached process-wide for navCacheTTL
// (gating is client-side, so one cache serves all users); core is static.

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// navCoreBase is the absolute origin prefixed to dock-owned CORE nav links so
// they resolve to dock even from a module subdomain. POLAR_PUBLIC_BASE_URL is
// reused elsewhere (agent WS) and may point at a subdomain, so the nav uses a
// dedicated POLAR_NAV_CORE_BASE override, falling back to s.publicBaseURL.
func (s *Server) navCoreBase() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("POLAR_NAV_CORE_BASE")), "/"); v != "" {
		return v
	}
	return s.publicBaseURL
}

type navItem struct {
	Href     string `json:"href"`
	Label    string `json:"label"`
	I18n     string `json:"i18n,omitempty"`
	Icon     string `json:"icon,omitempty"`
	Gate     string `json:"gate,omitempty"` // "" | "admin" | "site-admin"
	External bool   `json:"external,omitempty"`
}

type navGroup struct {
	Label string    `json:"label,omitempty"`
	Items []navItem `json:"items"`
}

// coreNav is the dock-owned sidebar, paths relative to dock's public origin.
// Module pages (expense/latch/hosts/wg/iosdist/library/projects/video) are NOT
// here — they self-register via heartbeat and land in the "模块" group.
var coreNav = []navGroup{
	{Items: []navItem{{Href: "/dashboard.html", Label: "Dashboard", Icon: "layout-dashboard"}}},
	{Label: "工作区", Items: []navItem{
		{Href: "/teams.html", Label: "工作区", I18n: "nav.teams", Icon: "users-round"},
		{Href: "/assets.html", Label: "资产", Icon: "folder"},
		{Href: "/api-tokens.html", Label: "开放 API", Icon: "key-round"},
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
		{Href: "/llm-billing.html", Label: "LLM 计费", Icon: "dollar-sign", Gate: "admin"},
		{Href: "/llm-apps.html", Label: "LLM 申请", Icon: "inbox", Gate: "admin"},
		{Href: "/bots.html", Label: "Bot Management", I18n: "nav.bots", Icon: "bot", Gate: "admin"},
		{Href: "/prompts.html", Label: "Prompt 模板", Icon: "sparkles", Gate: "admin"},
	}},
	{Label: "设备", Items: []navItem{
		{Href: "/agents.html", Label: "Agents", I18n: "nav.agents", Icon: "cpu", Gate: "site-admin"},
	}},
	{Label: "管理", Items: []navItem{
		{Href: "/invites.html", Label: "邀请码", Icon: "ticket", Gate: "admin"},
		{Href: "/admin-plugins.html", Label: "Plugin 注册", Icon: "puzzle", Gate: "admin"},
		{Href: "/admin-assets.html", Label: "资产管理（管理员）", Icon: "database", Gate: "site-admin"},
		{Href: "/admin-audit-log.html", Label: "审计日志", Icon: "clipboard-list", Gate: "site-admin"},
		{Href: "/proxy-tokens.html", Label: "API Tokens", Icon: "key", Gate: "admin"},
		{Href: "/grafana/", Label: "Grafana", Icon: "line-chart", Gate: "admin", External: true},
		{Href: "/admin.html", Label: "用户管理", I18n: "nav.userAdmin", Icon: "users-cog", Gate: "admin"},
	}},
}

// pluginRoute mirrors one element of plugin_modules.ui_routes.
type pluginRoute struct {
	Path      string `json:"path"`
	Label     string `json:"label"`
	Icon      string `json:"icon"`
	AdminOnly bool   `json:"admin_only"`
	Order     int    `json:"order"`
}

// cachedModEntry keeps the plugin name alongside the rendered item so the
// per-workspace access filter can run against the cache.
type cachedModEntry struct {
	plugin string
	order  int
	item   navItem
}

const navCacheTTL = 30 * time.Second

var (
	navCacheMu  sync.Mutex
	navCacheAt  time.Time
	navCacheRaw []cachedModEntry
)

// moduleNavEntries returns every enabled plugin's nav items (absolute links),
// cached for navCacheTTL. Access filtering happens later, per request.
func (s *Server) moduleNavEntries() ([]cachedModEntry, error) {
	navCacheMu.Lock()
	defer navCacheMu.Unlock()
	if navCacheRaw != nil && time.Since(navCacheAt) < navCacheTTL {
		return navCacheRaw, nil
	}
	rows, err := s.db.Query(
		`SELECT name, public_base_url, ui_routes::text
		   FROM plugin_modules
		  WHERE enabled = TRUE
		    AND ui_routes <> '[]'::jsonb
		    AND public_base_url <> ''
		  ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]cachedModEntry, 0)
	for rows.Next() {
		var name, base, raw string
		if err := rows.Scan(&name, &base, &raw); err != nil {
			return nil, err
		}
		var routes []pluginRoute
		if err := json.Unmarshal([]byte(raw), &routes); err != nil {
			continue // skip a plugin with malformed ui_routes rather than 500
		}
		for _, r := range routes {
			if r.Path == "" {
				continue
			}
			gate := ""
			if r.AdminOnly {
				gate = "admin"
			}
			out = append(out, cachedModEntry{
				plugin: name,
				order:  r.Order,
				item: navItem{
					Href:  base + r.Path,
					Label: r.Label,
					Icon:  r.Icon,
					Gate:  gate,
				},
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].order < out[j].order })
	navCacheAt = time.Now()
	navCacheRaw = out
	return out, nil
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

	// CORE — static, rewritten to absolute links against dock's public origin.
	base := s.navCoreBase()
	groups := make([]navGroup, 0, len(coreNav)+1)
	for _, g := range coreNav {
		ng := navGroup{Label: g.Label, Items: make([]navItem, 0, len(g.Items))}
		for _, it := range g.Items {
			abs := it
			abs.Href = base + it.Href
			ng.Items = append(ng.Items, abs)
		}
		groups = append(groups, ng)
	}

	// MODULES — self-registered, access-filtered for non-root workspaces.
	entries, err := s.moduleNavEntries()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	var allowed map[string]bool
	if !isRoot {
		allowed, err = s.enabledPluginNamesForWorkspace(workspaceID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			return
		}
	}
	modItems := make([]navItem, 0, len(entries))
	for _, e := range entries {
		if !isRoot && !allowed[e.plugin] {
			continue
		}
		modItems = append(modItems, e.item)
	}
	if len(modItems) > 0 {
		groups = append(groups, navGroup{Label: "模块", Items: modItems})
	}

	c.JSON(http.StatusOK, gin.H{"groups": groups})
}

// enabledPluginNamesForWorkspace returns the set of plugin names a non-root
// workspace has been granted (workspace_plugin_access.enabled=TRUE). Mirrors
// the access gate used by handlePluginUIRoutes.
func (s *Server) enabledPluginNamesForWorkspace(workspaceID string) (map[string]bool, error) {
	rows, err := s.db.Query(
		`SELECT plugin_name FROM workspace_plugin_access
		  WHERE workspace_id = $1 AND enabled = TRUE`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}
