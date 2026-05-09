package dock

// Storage layer for project_research_runs — research-mode dispatch
// lifecycle (research-runtime project → polar-agent runs local LLM
// loop → writes docs → git commit/push → reports back).
//
// LLM snapshot columns are stored at insert time so historical rows
// continue to show "this run used model X" even after the workspace's
// default LLM config is rebound or rolled.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	ResearchRunStatusQueued    = "queued"
	ResearchRunStatusRunning   = "running"
	ResearchRunStatusSucceeded = "succeeded"
	ResearchRunStatusFailed    = "failed"
)

type ResearchRun struct {
	ID            int64      `json:"id"`
	WorkspaceID   string     `json:"workspace_id"`
	ProjectID     string     `json:"project_id"`
	TaskID        *string    `json:"task_id,omitempty"`
	BotUserID     string     `json:"bot_user_id"`
	ChatThreadID  *int64     `json:"chat_thread_id,omitempty"`
	LLMThreadID   *int64     `json:"llm_thread_id,omitempty"`
	LLMConfigID   *int64     `json:"llm_config_id,omitempty"`
	LLMName       string     `json:"llm_name"`
	LLMModel      string     `json:"llm_model"`
	LLMBaseURL    string     `json:"llm_base_url"`
	Status        string     `json:"status"`
	Iterations    int        `json:"iterations"`
	FilesWritten  []string   `json:"files_written,omitempty"`
	CommitSHA     string     `json:"commit_sha"`
	Summary       string     `json:"summary"`
	ErrorMessage  string     `json:"error_message"`
	LogText       string     `json:"log_text"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

const researchRunSelectColumns = `id, workspace_id, project_id, task_id, bot_user_id, chat_thread_id, llm_thread_id, llm_config_id, llm_name, llm_model, llm_base_url, status, iterations, files_written, commit_sha, summary, error_message, log_text, created_at, started_at, completed_at, updated_at`

func scanResearchRun(scanFn func(...any) error) (*ResearchRun, error) {
	var r ResearchRun
	var taskID sql.NullString
	var chatThreadID, llmThreadID, llmConfigID sql.NullInt64
	var filesWritten []byte
	if err := scanFn(
		&r.ID, &r.WorkspaceID, &r.ProjectID, &taskID, &r.BotUserID,
		&chatThreadID, &llmThreadID, &llmConfigID,
		&r.LLMName, &r.LLMModel, &r.LLMBaseURL,
		&r.Status, &r.Iterations, &filesWritten,
		&r.CommitSHA, &r.Summary, &r.ErrorMessage, &r.LogText,
		&r.CreatedAt, &r.StartedAt, &r.CompletedAt, &r.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if taskID.Valid {
		v := taskID.String
		r.TaskID = &v
	}
	if chatThreadID.Valid {
		v := chatThreadID.Int64
		r.ChatThreadID = &v
	}
	if llmThreadID.Valid {
		v := llmThreadID.Int64
		r.LLMThreadID = &v
	}
	if llmConfigID.Valid {
		v := llmConfigID.Int64
		r.LLMConfigID = &v
	}
	if len(filesWritten) > 0 {
		_ = json.Unmarshal(filesWritten, &r.FilesWritten)
	}
	return &r, nil
}

func (s *Server) createResearchRun(r *ResearchRun) (int64, error) {
	if r.WorkspaceID == "" || r.ProjectID == "" || r.BotUserID == "" {
		return 0, errors.New("workspace_id, project_id, bot_user_id required")
	}
	if r.Status == "" {
		r.Status = ResearchRunStatusQueued
	}
	row := s.db.QueryRow(
		`INSERT INTO project_research_runs
		   (workspace_id, project_id, task_id, bot_user_id,
		    chat_thread_id, llm_thread_id,
		    llm_config_id, llm_name, llm_model, llm_base_url,
		    status, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NOW(),NOW())
		 RETURNING id, created_at, updated_at`,
		r.WorkspaceID, r.ProjectID, r.TaskID, r.BotUserID,
		r.ChatThreadID, r.LLMThreadID,
		r.LLMConfigID, r.LLMName, r.LLMModel, r.LLMBaseURL,
		r.Status,
	)
	if err := row.Scan(&r.ID, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return 0, err
	}
	return r.ID, nil
}

func (s *Server) getResearchRun(id int64) (*ResearchRun, error) {
	row := s.db.QueryRow(
		`SELECT `+researchRunSelectColumns+` FROM project_research_runs WHERE id = $1`,
		id,
	)
	r, err := scanResearchRun(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return r, nil
}

func (s *Server) markResearchRunRunning(id int64) error {
	_, err := s.db.Exec(
		`UPDATE project_research_runs
		 SET status = $2, started_at = COALESCE(started_at, NOW()), updated_at = NOW()
		 WHERE id = $1 AND status IN ($3, $2)`,
		id, ResearchRunStatusRunning, ResearchRunStatusQueued,
	)
	return err
}

// ResearchRunResult is the agent-supplied finalization payload.
type ResearchRunResult struct {
	OK           bool
	Iterations   int
	FilesWritten []string
	CommitSHA    string
	Summary      string
	ErrorMessage string
	LogText      string
}

func (s *Server) finalizeResearchRun(id int64, res ResearchRunResult) error {
	status := ResearchRunStatusSucceeded
	if !res.OK {
		status = ResearchRunStatusFailed
	}
	files := []byte("null")
	if len(res.FilesWritten) > 0 {
		b, err := json.Marshal(res.FilesWritten)
		if err == nil {
			files = b
		}
	}
	_, err := s.db.Exec(
		`UPDATE project_research_runs
		 SET status = $2,
		     iterations = $3,
		     files_written = $4::jsonb,
		     commit_sha = $5,
		     summary = LEFT($6, 4096),
		     error_message = LEFT($7, 1024),
		     log_text = LEFT($8, 8192),
		     completed_at = NOW(),
		     updated_at = NOW()
		 WHERE id = $1`,
		id, status,
		res.Iterations, string(files),
		strings.TrimSpace(res.CommitSHA),
		res.Summary, res.ErrorMessage, res.LogText,
	)
	return err
}

// listResearchRunsForProject returns most recent N runs for the given
// project. UI uses this to populate the research history tab.
func (s *Server) listResearchRunsForProject(projectID string, limit int) ([]ResearchRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT `+researchRunSelectColumns+`
		 FROM project_research_runs
		 WHERE project_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2`,
		projectID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ResearchRun, 0, limit)
	for rows.Next() {
		r, err := scanResearchRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
