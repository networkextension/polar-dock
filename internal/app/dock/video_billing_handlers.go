package dock

// video_billing_handlers.go — read-side endpoints for the per-shot
// video billing ledger. Write side is /internal/v1/billing/video-shots
// (see internal_billing_handlers.go), called by polar-video.
//
// Two workspace-scoped reads + one admin global aggregate:
//
//   GET  /api/video-shot-calls?project_id=&limit=
//   GET  /api/video-burn?since=&until=
//   GET  /api/admin/video-burn?since=&until=&workspace_id=

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// handleVideoShotCallsList returns the recent per-shot ledger rows
// for one project. Workspace-scoped: drops rows whose workspace_id
// doesn't match the caller's active workspace, so a user can't peek
// at a project ID that belongs to a different workspace.
func (s *Server) handleVideoShotCallsList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	projectStr := strings.TrimSpace(c.Query("project_id"))
	if projectStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project_id required"})
		return
	}
	projectID, err := strconv.ParseInt(projectStr, 10, 64)
	if err != nil || projectID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project_id must be a positive int"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.listVideoShotCallsByProject(projectID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	// Workspace isolation: filter to rows the caller owns. The store
	// helper is project-scoped; the caller could pass any project_id,
	// so we filter here to keep the public contract simple (single
	// query param + 403-by-empty-list rather than 403-by-error).
	filtered := make([]VideoShotCall, 0, len(rows))
	for _, r := range rows {
		if r.WorkspaceID == workspaceID {
			filtered = append(filtered, r)
		}
	}
	c.JSON(http.StatusOK, gin.H{"calls": filtered})
}

// handleVideoBurn returns the per-provider rollup for the caller's
// active workspace over a [since, until) window. Both query params
// are optional — defaults are "since=start of current month" and
// "until=now".
func (s *Server) handleVideoBurn(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	since, until, err := parseSinceUntil(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rows, err := s.aggregateVideoBurn(workspaceID, since, until)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"workspace_id": workspaceID,
		"since":        since,
		"until":        until,
		"rows":         rows,
	})
}

// handleAdminVideoBurn lets the platform operator pull any
// workspace's burn (or omit workspace_id for a global aggregate
// across all workspaces). AdminMiddleware enforced at the route.
func (s *Server) handleAdminVideoBurn(c *gin.Context) {
	since, until, err := parseSinceUntil(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	workspaceID := strings.TrimSpace(c.Query("workspace_id"))
	rows, err := s.aggregateVideoBurn(workspaceID, since, until)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"workspace_id": workspaceID, // "" → global
		"since":        since,
		"until":        until,
		"rows":         rows,
	})
}

// parseSinceUntil pulls since= and until= query params. Defaults to
// start-of-current-month → now if either is missing. Returns a
// half-open [since, until) — matches aggregateVideoBurn's SQL.
func parseSinceUntil(c *gin.Context) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	var since, until time.Time

	if s := strings.TrimSpace(c.Query("since")); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			// Be forgiving on date-only inputs.
			t2, err2 := time.Parse("2006-01-02", s)
			if err2 != nil {
				return time.Time{}, time.Time{}, errInvalidSince
			}
			t = t2
		}
		since = t.UTC()
	} else {
		since = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	if u := strings.TrimSpace(c.Query("until")); u != "" {
		t, err := time.Parse(time.RFC3339, u)
		if err != nil {
			t2, err2 := time.Parse("2006-01-02", u)
			if err2 != nil {
				return time.Time{}, time.Time{}, errInvalidUntil
			}
			t = t2
		}
		until = t.UTC()
	} else {
		until = now
	}
	if !until.After(since) {
		return time.Time{}, time.Time{}, errBadRange
	}
	return since, until, nil
}

var (
	errInvalidSince = &handlerErr{"invalid 'since' (use RFC3339 or YYYY-MM-DD)"}
	errInvalidUntil = &handlerErr{"invalid 'until' (use RFC3339 or YYYY-MM-DD)"}
	errBadRange     = &handlerErr{"'until' must be strictly greater than 'since'"}
)

type handlerErr struct{ msg string }

func (e *handlerErr) Error() string { return e.msg }
