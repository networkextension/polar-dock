package dock

// Append-only structured log service. Three scopes (site / workspace
// / per-category), five canonical categories (sign, team_audit,
// deploy, agent, server). All writes flow through appendLog or its
// shorthands; readers query via listLogs with the appropriate scope
// gate enforced by the handler.
//
// Don't put hot-path noise here — DB-backed logs are for events
// users / operators want to query later, not for raw debug
// breadcrumbs (those still belong in log.Printf). Rule of thumb:
// if it would help reconstruct who-did-what or what-went-wrong on
// a multi-day-old incident, write it; otherwise skip.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	LogCategorySign      = "sign"
	LogCategoryTeamAudit = "team_audit"
	LogCategoryDeploy    = "deploy"
	LogCategoryAgent     = "agent"
	LogCategoryServer    = "server"

	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"

	LogActorUser   = "user"
	LogActorBot    = "bot"
	LogActorSystem = "system"
	LogActorAgent  = "agent"
)

// LogEntry is the canonical write payload. WorkspaceID empty + nil
// ActorUserID = site-wide system event. WorkspaceID set = workspace
// member readable. Everything else is structured for filtering.
type LogEntry struct {
	ID            int64           `json:"id"`
	Category      string          `json:"category"`
	Level         string          `json:"level"`
	Source        string          `json:"source"`
	WorkspaceID   string          `json:"workspace_id,omitempty"`
	ActorUserID   string          `json:"actor_user_id,omitempty"`
	ActorKind     string          `json:"actor_kind"`
	SubjectKind   string          `json:"subject_kind,omitempty"`
	SubjectID     string          `json:"subject_id,omitempty"`
	Action        string          `json:"action,omitempty"`
	Message       string          `json:"message"`
	Details       json.RawMessage `json:"details,omitempty"`
	RequestID     string          `json:"request_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// appendLog inserts the entry. Defaults: Level=info, ActorKind=system
// when ActorUserID is empty, ActorKind=user otherwise. Failure is
// returned but most callers should treat logging failures as
// non-fatal (don't 500 a successful action because a log row
// couldn't be written).
func (s *Server) appendLog(e LogEntry) error {
	if strings.TrimSpace(e.Category) == "" {
		return errors.New("log: category required")
	}
	if e.Level == "" {
		e.Level = LogLevelInfo
	}
	if e.ActorKind == "" {
		if e.ActorUserID == "" {
			e.ActorKind = LogActorSystem
		} else {
			e.ActorKind = LogActorUser
		}
	}
	if len(e.Details) == 0 {
		e.Details = json.RawMessage(`{}`)
	}
	var workspaceID, actorUserID sql.NullString
	if e.WorkspaceID != "" {
		workspaceID = sql.NullString{String: e.WorkspaceID, Valid: true}
	}
	if e.ActorUserID != "" {
		actorUserID = sql.NullString{String: e.ActorUserID, Valid: true}
	}
	return s.db.QueryRow(
		`INSERT INTO logs_entries
			(category, level, source, workspace_id, actor_user_id, actor_kind, subject_kind, subject_id, action, message, details, request_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW())
		 RETURNING id, created_at`,
		e.Category, e.Level, e.Source, workspaceID, actorUserID, e.ActorKind,
		e.SubjectKind, e.SubjectID, e.Action, e.Message, []byte(e.Details), e.RequestID,
	).Scan(&e.ID, &e.CreatedAt)
}

// ---- shorthand emit helpers ---------------------------------------
//
// These wrap the common cases so call sites stay one-liner-ish.
// All silently swallow errors after a single best-effort attempt
// (panicking the caller because logging failed is worse than a
// missing log row).

// logTeamAudit records workspace-scoped events (transfers, invites,
// kicks, deletions, role changes).
func (s *Server) logTeamAudit(workspaceID, actorUserID, action, subjectKind, subjectID, message string, details map[string]any) {
	d := encodeLogDetails(details)
	_ = s.appendLog(LogEntry{
		Category:    LogCategoryTeamAudit,
		Level:       LogLevelInfo,
		WorkspaceID: workspaceID,
		ActorUserID: actorUserID,
		Action:      action,
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		Message:     message,
		Details:     d,
	})
}

// logSignEvent records iosdist sign worker transitions. WorkspaceID
// is the iosdist app's workspace; SubjectID is the sign task id.
func (s *Server) logSignEvent(workspaceID string, taskID int64, action, message, level string, details map[string]any) {
	if level == "" {
		level = LogLevelInfo
	}
	_ = s.appendLog(LogEntry{
		Category:    LogCategorySign,
		Level:       level,
		Source:      "iosdist.sign",
		WorkspaceID: workspaceID,
		ActorKind:   LogActorSystem,
		SubjectKind: "iosdist_sign_task",
		SubjectID:   strconv.FormatInt(taskID, 10),
		Action:      action,
		Message:     message,
		Details:     encodeLogDetails(details),
	})
}

// logServerError records site-wide opportunistic server errors that
// operators may want to find without grepping log files. Use sparingly;
// don't replace every log.Printf.
func (s *Server) logServerError(source, message string, details map[string]any) {
	_ = s.appendLog(LogEntry{
		Category:  LogCategoryServer,
		Level:     LogLevelError,
		Source:    source,
		ActorKind: LogActorSystem,
		Message:   message,
		Details:   encodeLogDetails(details),
	})
}

// logAgentActivity records AI / bot agent execution traces. Workspace
// scoped (the workspace whose bot ran).
func (s *Server) logAgentActivity(workspaceID, actorUserID, source, action, message string, details map[string]any) {
	_ = s.appendLog(LogEntry{
		Category:    LogCategoryAgent,
		Level:       LogLevelInfo,
		Source:      source,
		WorkspaceID: workspaceID,
		ActorUserID: actorUserID,
		ActorKind:   LogActorAgent,
		Action:      action,
		Message:     message,
		Details:     encodeLogDetails(details),
	})
}

// logDeployEvent records video render / external deploy outcomes.
// Workspace scoped if the deploy was on a workspace resource;
// site-wide if it's a platform-level deploy.
func (s *Server) logDeployEvent(workspaceID, actorUserID, source, action, message, level string, details map[string]any) {
	if level == "" {
		level = LogLevelInfo
	}
	_ = s.appendLog(LogEntry{
		Category:    LogCategoryDeploy,
		Level:       level,
		Source:      source,
		WorkspaceID: workspaceID,
		ActorUserID: actorUserID,
		Action:      action,
		Message:     message,
		Details:     encodeLogDetails(details),
	})
}

func encodeLogDetails(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return raw
}

// ---- read API -----------------------------------------------------

// LogQuery filters listLogs. Empty fields are skipped. Cursor pages
// from the most recent backwards via (BeforeCreatedAt, BeforeID).
type LogQuery struct {
	Category        string
	Level           string
	Source          string
	WorkspaceID     string // empty = no workspace filter (admin scope)
	ActorUserID     string
	SubjectKind     string
	SubjectID       string
	BeforeCreatedAt *time.Time
	BeforeID        *int64
	Limit           int

	// IncludeSiteWide toggles whether workspace_id IS NULL rows are
	// returned. Workspace handlers always pass false; admin handlers
	// pass true (and leave WorkspaceID empty for the full firehose).
	IncludeSiteWide bool
}

// listLogs returns entries newest-first, capped at Limit (max 200).
// Caller is responsible for authorization — the workspace handler
// pins WorkspaceID and IncludeSiteWide=false; the admin handler
// passes through user filters and IncludeSiteWide=true.
func (s *Server) listLogs(q LogQuery) ([]LogEntry, error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	conds := []string{}
	args := []any{}
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}

	if q.WorkspaceID != "" {
		add(`workspace_id = ?`, q.WorkspaceID)
	} else if !q.IncludeSiteWide {
		// No workspace, no site-wide allowance → caller wants
		// "show me logs from any workspace I'm a member of",
		// which would need a JOIN to team_members. Not
		// supported in the current handler set; return empty.
		return []LogEntry{}, nil
	}
	if q.Category != "" {
		add(`category = ?`, q.Category)
	}
	if q.Level != "" {
		add(`level = ?`, q.Level)
	}
	if q.Source != "" {
		add(`source = ?`, q.Source)
	}
	if q.ActorUserID != "" {
		add(`actor_user_id = ?`, q.ActorUserID)
	}
	if q.SubjectKind != "" {
		add(`subject_kind = ?`, q.SubjectKind)
		if q.SubjectID != "" {
			add(`subject_id = ?`, q.SubjectID)
		}
	}
	// Cursor: (created_at, id) tuple comparison — Postgres handles
	// row-level less-than as expected.
	if q.BeforeCreatedAt != nil && q.BeforeID != nil {
		args = append(args, *q.BeforeCreatedAt, *q.BeforeID)
		conds = append(conds, `(created_at, id) < ($`+strconv.Itoa(len(args)-1)+`, $`+strconv.Itoa(len(args))+`)`)
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)
	query := `SELECT id, category, level, source, workspace_id, actor_user_id, actor_kind, subject_kind, subject_id, action, message, details, request_id, created_at
	          FROM logs_entries ` + where + `
	          ORDER BY created_at DESC, id DESC
	          LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LogEntry{}
	for rows.Next() {
		var e LogEntry
		var workspaceID, actorUserID sql.NullString
		var details []byte
		if err := rows.Scan(&e.ID, &e.Category, &e.Level, &e.Source,
			&workspaceID, &actorUserID, &e.ActorKind,
			&e.SubjectKind, &e.SubjectID, &e.Action, &e.Message,
			&details, &e.RequestID, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		if workspaceID.Valid {
			e.WorkspaceID = workspaceID.String
		}
		if actorUserID.Valid {
			e.ActorUserID = actorUserID.String
		}
		if len(details) > 0 {
			e.Details = json.RawMessage(details)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
