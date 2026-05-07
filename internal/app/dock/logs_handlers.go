package dock

// HTTP handlers for the logs read API. Two scopes:
//
//   GET /api/teams/:id/logs        — workspace-scoped, member+
//   GET /api/admin/logs            — site-wide, admin only
//
// Both share the same filter / cursor surface; the workspace
// handler pins workspace_id from the path and never returns
// site-wide rows. The admin handler is wide-open: pass
// workspace_id=<x> to scope, omit it to see everything.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// logQueryFromContext parses common filter args from the URL query.
// Caller-provided values override (so the handler can pin
// workspace_id / IncludeSiteWide independently).
func logQueryFromContext(c *gin.Context) LogQuery {
	q := LogQuery{
		Category:    strings.TrimSpace(c.Query("category")),
		Level:       strings.TrimSpace(c.Query("level")),
		Source:      strings.TrimSpace(c.Query("source")),
		ActorUserID: strings.TrimSpace(c.Query("actor_user_id")),
		SubjectKind: strings.TrimSpace(c.Query("subject_kind")),
		SubjectID:   strings.TrimSpace(c.Query("subject_id")),
	}
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Limit = n
		}
	}
	if v := strings.TrimSpace(c.Query("before_id")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			q.BeforeID = &n
		}
	}
	if v := strings.TrimSpace(c.Query("before_created_at")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.BeforeCreatedAt = &t
		}
	}
	return q
}

// handleTeamLogs serves logs for the team identified by the URL
// path. RequireTeamRole has already validated viewer+ on this team
// before we get here.
func (s *Server) handleTeamLogs(c *gin.Context) {
	teamID, _, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	q := logQueryFromContext(c)
	q.WorkspaceID = teamID
	q.IncludeSiteWide = false
	entries, err := s.listLogs(q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"entries": entries, "scope": "workspace"})
}

// handleAdminLogs is the site-wide firehose. AdminMiddleware has
// already gated this. workspace_id can be passed as a filter to
// drill into a specific team without leaving the admin scope.
func (s *Server) handleAdminLogs(c *gin.Context) {
	q := logQueryFromContext(c)
	if requested := strings.TrimSpace(c.Query("workspace_id")); requested != "" {
		q.WorkspaceID = requested
	}
	q.IncludeSiteWide = true
	entries, err := s.listLogs(q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"entries": entries, "scope": "site"})
}
