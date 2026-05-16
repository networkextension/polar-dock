package dock

// Dashboard widget layout: per-user list of enabled widgets in
// display order. Stored as a comma-separated TEXT column on the
// users row. Each slug must be in dashboardWidgetCatalog or it's
// dropped from the served list (defends against stale slugs after
// a code deletion).
//
// Widget *data* is fetched from existing list endpoints — we don't
// add per-widget data routes here. The frontend reads the layout,
// then for each enabled widget calls the appropriate list endpoint
// (e.g. /api/projects?limit=5). This keeps the surface area small
// and lets widgets reuse pagination + filtering already in place.

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// dashboardWidget describes one slug — what code knows about it,
// what UI metadata to show in the customize modal.
type dashboardWidget struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// AdminOnly: when true, frontend hides this widget from
	// non-admin users in the customize modal.
	AdminOnly bool `json:"admin_only"`
	// Default: included in the platform's default layout for new
	// users. Distinct from "available" — admin-only iosdist widget
	// is available to admins but isn't in the default-on set.
	Default bool `json:"default"`
}

// dashboardWidgetCatalog is the canonical list of widget slugs.
// Adding a new widget means adding an entry here + the matching
// renderer in ui/src/dashboard.ts. The catalog drives both
// validation (drop unknown slugs) and the customize modal listing.
func dashboardWidgetCatalog() []dashboardWidget {
	return []dashboardWidget{
		{Slug: "quick_start", Name: "快速入口", Description: "9 个 tile 链接到主模块。", Default: true},
		{Slug: "recent_projects", Name: "最近项目", Description: "你的项目按更新时间倒序，top 5。", Default: true},
		{Slug: "recent_markdowns", Name: "最近 Markdown", Description: "最近编辑的 markdown entry top 5。", Default: true},
		{Slug: "recent_posts", Name: "最近 Post", Description: "你最近的 post top 5。"},
		{Slug: "recent_chat", Name: "最近 Chat", Description: "最近 chat thread top 5 + 未读数。"},
		{Slug: "iosdist_builds", Name: "iOS 分发构建", Description: "最近 5 个签名/分发构建状态（admin only）。", AdminOnly: true},
		{Slug: "system_info", Name: "系统信息", Description: "dock 进程所在主机的 OS / IP / 磁盘 / 内存 / CPU / 上线时间。", Default: true},
		{Slug: "llm_proxy_stats", Name: "LLM Proxy", Description: "本月 API 用量 USD、上游可用性、活跃 token 数（admin only）。", AdminOnly: true, Default: true},
	}
}

// dashboardDefaultLayout returns the slug list for new users.
// Pulled from the catalog's Default flag — single source of truth.
func dashboardDefaultLayout() []string {
	out := []string{}
	for _, w := range dashboardWidgetCatalog() {
		if w.Default {
			out = append(out, w.Slug)
		}
	}
	return out
}

// dashboardWidgetSlugs returns a set of every known slug for fast
// validation of incoming PUT payloads.
func dashboardWidgetSlugs() map[string]bool {
	m := map[string]bool{}
	for _, w := range dashboardWidgetCatalog() {
		m[w.Slug] = true
	}
	return m
}

func parseDashboardLayout(raw string) []string {
	known := dashboardWidgetSlugs()
	out := []string{}
	seen := map[string]bool{}
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" || !known[s] || seen[s] {
			continue
		}
		out = append(out, s)
		seen[s] = true
	}
	return out
}

func (s *Server) loadDashboardLayout(userID string) ([]string, error) {
	var raw string
	err := s.db.QueryRow(`SELECT COALESCE(dashboard_widgets, '') FROM users WHERE id = $1`, userID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	parsed := parseDashboardLayout(raw)
	if len(parsed) == 0 {
		// Empty (new user) or all-stale — fall back to default so
		// the dashboard isn't blank. Don't auto-write back; user can
		// still customize and that PUT will persist.
		return dashboardDefaultLayout(), nil
	}
	return parsed, nil
}

func (s *Server) saveDashboardLayout(userID string, slugs []string) error {
	known := dashboardWidgetSlugs()
	clean := []string{}
	seen := map[string]bool{}
	for _, slug := range slugs {
		slug = strings.TrimSpace(slug)
		if slug == "" || !known[slug] || seen[slug] {
			continue
		}
		clean = append(clean, slug)
		seen[slug] = true
	}
	_, err := s.db.Exec(
		`UPDATE users SET dashboard_widgets = $2 WHERE id = $1`,
		userID, strings.Join(clean, ","),
	)
	return err
}

// ---- handlers ----

func (s *Server) handleDashboardLayoutGet(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid, _ := userID.(string)
	if uid == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	layout, err := s.loadDashboardLayout(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Always include the catalog so the customize modal can render
	// without a separate fetch. AdminOnly widgets are filtered out
	// for non-admin viewers so they aren't surfaced as a tease.
	role, _ := c.Get("role")
	roleStr, _ := role.(string)
	isAdmin := roleStr == "admin"
	cat := dashboardWidgetCatalog()
	visible := cat[:0]
	for _, w := range cat {
		if w.AdminOnly && !isAdmin {
			continue
		}
		visible = append(visible, w)
	}
	// Don't mutate the slice returned by the function — defensive
	// re-sort by Slug for stable UI ordering.
	sort.Slice(visible, func(i, j int) bool { return visible[i].Slug < visible[j].Slug })
	c.JSON(http.StatusOK, gin.H{
		"layout":  layout,
		"catalog": visible,
	})
}

type dashboardLayoutUpdateRequest struct {
	Layout []string `json:"layout"`
}

func (s *Server) handleDashboardLayoutUpdate(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid, _ := userID.(string)
	if uid == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	var req dashboardLayoutUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	if err := s.saveDashboardLayout(uid, req.Layout); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	layout, _ := s.loadDashboardLayout(uid)
	c.JSON(http.StatusOK, gin.H{"layout": layout})
}
