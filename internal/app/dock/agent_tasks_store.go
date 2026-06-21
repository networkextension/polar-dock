package dock

// agent_tasks_store.go — the general on-device task queue (polar-task T0).
// Submit → durable claim (FOR UPDATE SKIP LOCKED) → result. See polar-task
// doc/02. The queue lives in dock because only dock validates agent tokens.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// AgentTask is one queued/claimed/finished unit of on-device work.
type AgentTask struct {
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	CreatedBy   string          `json:"created_by"`
	Skill       string          `json:"skill"`
	Input       json.RawMessage `json:"input"`
	Constraints json.RawMessage `json:"constraints,omitempty"`
	Priority    int             `json:"priority"`
	Status      string          `json:"status"`
	AgentID     string          `json:"agent_id,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	ClaimedAt   *time.Time      `json:"claimed_at,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at"`
	// P2 — module origination + default-hold lifecycle.
	RequesterModule string     `json:"requester_module,omitempty"`
	RequesterRef    string     `json:"requester_ref,omitempty"`
	CallbackPath    string     `json:"callback_path,omitempty"`
	CallbackStatus  string     `json:"callback_status,omitempty"`
	ReleasedAt      *time.Time `json:"released_at,omitempty"`
}

// agentTaskCols is the canonical column list for every SELECT that feeds
// scanAgentTaskRows (keep this in sync with the scan order below).
const agentTaskCols = `id, workspace_id, created_by, skill, input_json, constraints_json, priority,
	status, COALESCE(agent_id,''), result_json, error, created_at, claimed_at, updated_at,
	requester_module, requester_ref, callback_path, callback_status, released_at`

// enqueueTask inserts a queued task. input/constraints are raw JSON objects.
func (s *Server) enqueueTask(workspaceID, createdBy, skill string, input, constraints json.RawMessage, priority int) (*AgentTask, error) {
	id := "tk_" + generateResourceID()
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	if len(constraints) == 0 {
		constraints = json.RawMessage("{}")
	}
	_, err := s.db.Exec(
		`INSERT INTO agent_tasks (id, workspace_id, created_by, skill, input_json, constraints_json, priority, status)
		 VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,'queued')`,
		id, workspaceID, createdBy, skill, string(input), string(constraints), priority,
	)
	if err != nil {
		return nil, err
	}
	return s.getTask(workspaceID, id)
}

// claimTask atomically pulls the next queued task whose skill the agent can do,
// in the given workspace, and marks it running. Returns nil if none.
func (s *Server) claimTask(workspaceID, agentID string, skills []string) (*AgentTask, error) {
	if len(skills) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var id string
	err = tx.QueryRow(
		`SELECT id FROM agent_tasks
		 WHERE status='queued' AND workspace_id=$1 AND skill = ANY($2)
		 ORDER BY priority DESC, created_at
		 FOR UPDATE SKIP LOCKED LIMIT 1`,
		workspaceID, pq.Array(skills),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`UPDATE agent_tasks SET status='running', agent_id=$2, claimed_at=now(), updated_at=now() WHERE id=$1`,
		id, agentID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.getTask(workspaceID, id)
}

// TaskArtifact is a file output of a task (stored in polar-assets).
type TaskArtifact struct {
	Kind        string `json:"kind"`
	AssetID     int64  `json:"asset_id"`
	Mime        string `json:"mime"`
	Bytes       int64  `json:"bytes"`
	DownloadURL string `json:"download_url,omitempty"`
}

// insertTaskArtifact records one artifact for a task.
func (s *Server) insertTaskArtifact(taskID, kind string, assetID int64, mime string, bytes int64) error {
	_, err := s.db.Exec(
		`INSERT INTO task_artifacts (task_id, kind, asset_id, mime, bytes) VALUES ($1,$2,$3,$4,$5)`,
		taskID, kind, assetID, mime, bytes)
	return err
}

// listTaskArtifacts returns a task's artifacts (asset_id only; caller mints URLs).
func (s *Server) listTaskArtifacts(taskID string) ([]TaskArtifact, error) {
	rows, err := s.db.Query(
		`SELECT kind, asset_id, mime, bytes FROM task_artifacts WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskArtifact{}
	for rows.Next() {
		var a TaskArtifact
		if err := rows.Scan(&a.Kind, &a.AssetID, &a.Mime, &a.Bytes); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// finalizeTask records a terminal result. Only the claiming agent may finalize.
func (s *Server) finalizeTask(workspaceID, taskID, agentID string, ok bool, result json.RawMessage, taskErr string) (bool, error) {
	status := "done"
	if !ok {
		status = "failed"
	}
	var resArg any
	if len(result) > 0 {
		resArg = string(result)
	}
	res, err := s.db.Exec(
		`UPDATE agent_tasks
		   SET status=$4, result_json=$5::jsonb, error=LEFT($6, 4096), updated_at=now()
		 WHERE id=$1 AND workspace_id=$2 AND agent_id=$3 AND status='running'`,
		taskID, workspaceID, agentID, status, resArg, strings.TrimSpace(taskErr),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Server) getTask(workspaceID, id string) (*AgentTask, error) {
	return scanAgentTask(s.db.QueryRow(
		`SELECT `+agentTaskCols+` FROM agent_tasks WHERE id=$1 AND workspace_id=$2`, id, workspaceID))
}

func (s *Server) listTasks(workspaceID string, limit int) ([]AgentTask, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT `+agentTaskCols+` FROM agent_tasks WHERE workspace_id=$1 ORDER BY created_at DESC LIMIT $2`,
		workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentTask{}
	for rows.Next() {
		t, err := scanAgentTaskRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// enqueueModuleTask inserts a task originated by a plugin module. status is
// "hold" (default) until released, or "queued" when auto_start. requester_module
// scopes the requester self-query; callback_path/requester_ref are echoed back in
// the P3 completion callback. See polar-task doc/07 P2.
func (s *Server) enqueueModuleTask(workspaceID, createdBy, skill string, input, constraints json.RawMessage,
	priority int, requesterModule, requesterRef, callbackPath, status string) (*AgentTask, error) {
	id := "tk_" + generateResourceID()
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	if len(constraints) == 0 {
		constraints = json.RawMessage("{}")
	}
	if status != "queued" {
		status = "hold"
	}
	var releasedAt any
	if status == "queued" {
		releasedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO agent_tasks
		   (id, workspace_id, created_by, skill, input_json, constraints_json, priority, status,
		    requester_module, requester_ref, callback_path, released_at)
		 VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9,$10,$11,$12)`,
		id, workspaceID, createdBy, skill, string(input), string(constraints), priority, status,
		requesterModule, requesterRef, callbackPath, releasedAt,
	)
	if err != nil {
		return nil, err
	}
	return s.getTask(workspaceID, id)
}

// listTasksByRequester returns a module's own tasks (newest first), optionally
// filtered by status. Scoped to requester_module so a module never sees another's.
func (s *Server) listTasksByRequester(module, status string, limit int) ([]AgentTask, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + agentTaskCols + ` FROM agent_tasks WHERE requester_module=$1`
	args := []any{module}
	if strings.TrimSpace(status) != "" {
		q += ` AND status=$2`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT $` + strconv.Itoa(len(args)+1)
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentTask{}
	for rows.Next() {
		t, err := scanAgentTaskRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// getTaskByRequester fetches one task scoped to the owning module.
func (s *Server) getTaskByRequester(module, id string) (*AgentTask, error) {
	return scanAgentTask(s.db.QueryRow(
		`SELECT `+agentTaskCols+` FROM agent_tasks WHERE id=$1 AND requester_module=$2`, id, module))
}

// releaseTask flips a held task to queued (claimable). Owner-scoped to the
// requester_module; returns true only if a held task was actually released.
func (s *Server) releaseTask(module, id string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE agent_tasks SET status='queued', released_at=now(), updated_at=now()
		 WHERE id=$1 AND requester_module=$2 AND status='hold'`, id, module)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func scanAgentTask(r rowScanner) (*AgentTask, error) {
	t, err := scanAgentTaskRows(r)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return t, err
}

func scanAgentTaskRows(r rowScanner) (*AgentTask, error) {
	var t AgentTask
	var input, constraints, result []byte
	var claimedAt, releasedAt sql.NullTime
	if err := r.Scan(&t.ID, &t.WorkspaceID, &t.CreatedBy, &t.Skill, &input, &constraints, &t.Priority,
		&t.Status, &t.AgentID, &result, &t.Error, &t.CreatedAt, &claimedAt, &t.UpdatedAt,
		&t.RequesterModule, &t.RequesterRef, &t.CallbackPath, &t.CallbackStatus, &releasedAt); err != nil {
		return nil, err
	}
	t.Input = json.RawMessage(input)
	t.Constraints = json.RawMessage(constraints)
	if len(result) > 0 {
		t.Result = json.RawMessage(result)
	}
	if claimedAt.Valid {
		t.ClaimedAt = &claimedAt.Time
	}
	if releasedAt.Valid {
		t.ReleasedAt = &releasedAt.Time
	}
	return &t, nil
}
