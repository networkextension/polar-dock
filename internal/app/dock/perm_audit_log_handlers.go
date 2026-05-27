package dock

// perm_audit_log_handlers.go — read endpoints over admin_audit_log.
// Two surfaces, both filtered server-side:
//
//   GET /api/admin/audit-log          — site-admin only; whole table
//   GET /api/teams/:id/admin-actions  — any team member; rows where
//                                       target_team_id = :id
//
// The tenant-side endpoint is the transparency play from §3.7 — a
// tenant admin can see when a site admin flipped a plugin on/off,
// recharged the wallet, set a quota, etc. Read-only and restricted
// to rows where the team itself was the target.
//
// Filters via query params (repeatable for the slice-y ones):
//   actor_user_id    repeatable
//   action_category  repeatable
//   action           repeatable
//   target_kind      repeatable
//   since / until    RFC3339 timestamps
//   limit            int (default 100, capped 500)
//
// Reading the site-admin endpoint writes one self-log row with
// action_category = audit_read so the trail of "who looked at the
// audit trail" is closed.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

type adminAuditLogRow struct {
	ID             int64          `json:"id"`
	ActorUserID    string         `json:"actor_user_id"`
	ActorUsername  string         `json:"actor_username,omitempty"`
	ActorRole      string         `json:"actor_role"`
	ActionCategory string         `json:"action_category"`
	Action         string         `json:"action"`
	TargetTeamID   string         `json:"target_team_id,omitempty"`
	TargetKind     string         `json:"target_kind,omitempty"`
	TargetID       string         `json:"target_id,omitempty"`
	Payload        map[string]any `json:"payload,omitempty"`
	Note           string         `json:"note,omitempty"`
	SourceIP       string         `json:"source_ip,omitempty"`
	UserAgent      string         `json:"user_agent,omitempty"`
	CreatedAt      string         `json:"created_at"`
}

type auditLogQuery struct {
	ActorUserIDs []string
	Categories   []string
	Actions      []string
	TargetKinds  []string
	TargetTeamID string // forced filter for the tenant endpoint
	Since        *time.Time
	Until        *time.Time
	Limit        int
}

const (
	auditLogDefaultLimit = 100
	auditLogMaxLimit     = 500
)

func parseAuditLogQuery(c *gin.Context) (auditLogQuery, error) {
	q := auditLogQuery{
		ActorUserIDs: c.QueryArray("actor_user_id"),
		Categories:   c.QueryArray("action_category"),
		Actions:      c.QueryArray("action"),
		TargetKinds:  c.QueryArray("target_kind"),
		Limit:        auditLogDefaultLimit,
	}
	if s := strings.TrimSpace(c.Query("since")); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return q, errors.New("invalid 'since' (want RFC3339)")
		}
		q.Since = &t
	}
	if s := strings.TrimSpace(c.Query("until")); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return q, errors.New("invalid 'until' (want RFC3339)")
		}
		q.Until = &t
	}
	if s := strings.TrimSpace(c.Query("limit")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return q, errors.New("invalid 'limit'")
		}
		if n > auditLogMaxLimit {
			n = auditLogMaxLimit
		}
		q.Limit = n
	}
	return q, nil
}

// queryAuditLog runs the filtered SELECT, joining users for the
// actor username so the UI doesn't have to round-trip per row.
//
// Filters use pq.Array for the repeatable params so we don't have to
// generate $1, $2, $3, … placeholders for each value.
func (s *Server) queryAuditLog(q auditLogQuery) ([]adminAuditLogRow, error) {
	where := []string{}
	args := []any{}
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, strings.Replace(clause, "$?", "$"+strconv.Itoa(len(args)), 1))
	}
	if len(q.ActorUserIDs) > 0 {
		add("al.actor_user_id = ANY($?)", pq.Array(q.ActorUserIDs))
	}
	if len(q.Categories) > 0 {
		add("al.action_category = ANY($?)", pq.Array(q.Categories))
	}
	if len(q.Actions) > 0 {
		add("al.action = ANY($?)", pq.Array(q.Actions))
	}
	if len(q.TargetKinds) > 0 {
		add("al.target_kind = ANY($?)", pq.Array(q.TargetKinds))
	}
	if q.TargetTeamID != "" {
		add("al.target_team_id = $?", q.TargetTeamID)
	}
	if q.Since != nil {
		add("al.created_at >= $?", *q.Since)
	}
	if q.Until != nil {
		add("al.created_at < $?", *q.Until)
	}
	args = append(args, q.Limit)
	limitPlaceholder := "$" + strconv.Itoa(len(args))

	sqlStmt := `
		SELECT al.id, al.actor_user_id, COALESCE(u.username, ''), al.actor_role,
		       al.action_category, al.action,
		       COALESCE(al.target_team_id, ''), al.target_kind, al.target_id,
		       al.payload::text, al.note, al.source_ip, al.user_agent,
		       al.created_at
		FROM admin_audit_log al
		LEFT JOIN users u ON u.id = al.actor_user_id`
	if len(where) > 0 {
		sqlStmt += " WHERE " + strings.Join(where, " AND ")
	}
	sqlStmt += " ORDER BY al.id DESC LIMIT " + limitPlaceholder

	rows, err := s.db.Query(sqlStmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]adminAuditLogRow, 0, q.Limit)
	for rows.Next() {
		var (
			r          adminAuditLogRow
			payloadStr string
			createdAt  time.Time
		)
		if err := rows.Scan(
			&r.ID, &r.ActorUserID, &r.ActorUsername, &r.ActorRole,
			&r.ActionCategory, &r.Action,
			&r.TargetTeamID, &r.TargetKind, &r.TargetID,
			&payloadStr, &r.Note, &r.SourceIP, &r.UserAgent,
			&createdAt,
		); err != nil {
			return nil, err
		}
		r.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		if payloadStr != "" {
			// Best-effort decode; on failure leave payload nil so the
			// response is still well-formed JSON.
			m := map[string]any{}
			if err := json.Unmarshal([]byte(payloadStr), &m); err == nil {
				r.Payload = m
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// handleAdminAuditLog — site-admin endpoint. Reading the audit log
// is itself an admin action; we self-log one row with action=audit_log.read
// so "who looked at the audit trail" is auditable.
func (s *Server) handleAdminAuditLog(c *gin.Context) {
	q, err := parseAuditLogQuery(c)
	if err != nil {
		jsonError(c, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := s.queryAuditLog(q)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "audit lookup failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"rows":  rows,
		"limit": q.Limit,
		"count": len(rows),
	})
	// Audit the read itself. Best-effort; failures only log.
	s.LogAdminAction(c, AdminAuditIn{
		Category: AuditCatAuditRead,
		Action:   "audit_log.read",
		Note:     "site-admin viewed admin_audit_log",
		Payload:  map[string]any{"returned_rows": len(rows), "limit": q.Limit},
	})
}

// handleTeamAdminActions — tenant-side view. Returns rows where
// target_team_id matches the team in the URL. Membership is gated
// by RequireTeamRole(TeamRoleViewer) in app.go — by the time we
// reach this handler the caller is a confirmed member.
//
// Doesn't self-log — tenants reading their own targeted-admin-actions
// list is normal product use, not an admin event.
//
// Site admins reading another tenant's audit trail must go through
// the impersonation flow (P5), not this endpoint.
func (s *Server) handleTeamAdminActions(c *gin.Context) {
	teamID := strings.TrimSpace(c.Param("id"))
	if teamID == "" {
		jsonError(c, http.StatusBadRequest, "team id required")
		return
	}
	q, err := parseAuditLogQuery(c)
	if err != nil {
		jsonError(c, http.StatusBadRequest, err.Error())
		return
	}
	q.TargetTeamID = teamID // force-scope
	rows, err := s.queryAuditLog(q)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "audit lookup failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"rows":  rows,
		"limit": q.Limit,
		"count": len(rows),
	})
}
