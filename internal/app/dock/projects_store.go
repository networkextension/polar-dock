package dock

// Storage layer for projects + features + tasks. Tables are
// declared in store.go's openDB() schema. The full pipeline:
//   project (description) → AI decompose → project_features
//                                          → user picks → project_tasks
//                                          → bot picks task → chat_thread bound
//                                          → bot codes → IPA / OTA dist

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	ProjectStatusDraft    = "draft"
	ProjectStatusPlanning = "planning"
	ProjectStatusBuilding = "building"
	ProjectStatusBeta     = "beta"
	ProjectStatusShipped  = "shipped"
	ProjectStatusArchived = "archived"

	ProjectFeatureComplexitySimple = "simple"
	ProjectFeatureComplexityMedium = "medium"
	ProjectFeatureComplexityComplex = "complex"

	ProjectTaskStatusTodo       = "todo"
	ProjectTaskStatusInProgress = "in_progress"
	ProjectTaskStatusReview     = "review"
	ProjectTaskStatusDone       = "done"
	ProjectTaskStatusBlocked    = "blocked"
)

type Project struct {
	ID               string    `json:"id"`
	WorkspaceID      string    `json:"workspace_id"`
	CreatorUserID    string    `json:"creator_user_id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	Status           string    `json:"status"`
	Template         string    `json:"template"`
	RuntimeEnv       string    `json:"runtime_env"`
	BusinessScenario string    `json:"business_scenario"`
	CodeProvider     string    `json:"code_provider"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type ProjectFeature struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Ord         int       `json:"ord"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Complexity  string    `json:"complexity"`
	Selected    bool      `json:"selected"`
	CreatedAt   time.Time `json:"created_at"`
}

type ProjectTask struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	FeatureID     *string    `json:"feature_id,omitempty"`
	Title         string     `json:"title"`
	Description   string     `json:"description"`
	Status        string     `json:"status"`
	BotUserID     *string    `json:"bot_user_id,omitempty"`
	ChatThreadID  *int64     `json:"chat_thread_id,omitempty"`
	LLMThreadID   *int64     `json:"llm_thread_id,omitempty"`
	Ord           int        `json:"ord"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// ---- projects ------------------------------------------------------

func (s *Server) createProject(p *Project) error {
	if p == nil {
		return errors.New("project is nil")
	}
	if p.ID == "" {
		p.ID = generateResourceID()
	}
	if p.Status == "" {
		p.Status = ProjectStatusDraft
	}
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now
	_, err := s.db.Exec(
		`INSERT INTO projects (id, workspace_id, creator_user_id, name, description, status, template, runtime_env, business_scenario, code_provider, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
		p.ID, p.WorkspaceID, p.CreatorUserID, p.Name, p.Description, p.Status, p.Template,
		p.RuntimeEnv, p.BusinessScenario, p.CodeProvider, now,
	)
	return err
}

func (s *Server) listProjects(workspaceID string) ([]Project, error) {
	rows, err := s.db.Query(
		`SELECT id, workspace_id, creator_user_id, name, description, status, template, runtime_env, business_scenario, code_provider, created_at, updated_at
		 FROM projects WHERE workspace_id = $1 ORDER BY updated_at DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.CreatorUserID, &p.Name, &p.Description, &p.Status, &p.Template, &p.RuntimeEnv, &p.BusinessScenario, &p.CodeProvider, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Server) getProject(id, workspaceID string) (*Project, error) {
	var p Project
	err := s.db.QueryRow(
		`SELECT id, workspace_id, creator_user_id, name, description, status, template, runtime_env, business_scenario, code_provider, created_at, updated_at
		 FROM projects WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID,
	).Scan(&p.ID, &p.WorkspaceID, &p.CreatorUserID, &p.Name, &p.Description, &p.Status, &p.Template, &p.RuntimeEnv, &p.BusinessScenario, &p.CodeProvider, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ProjectMetaUpdate carries the editable project fields, all
// optional. Nil pointers leave the existing value unchanged.
type ProjectMetaUpdate struct {
	Name             *string
	Description      *string
	Status           *string
	Template         *string
	RuntimeEnv       *string
	BusinessScenario *string
	CodeProvider     *string
}

func (s *Server) updateProjectMetadata(id, workspaceID string, u ProjectMetaUpdate) error {
	sets := []string{"updated_at = NOW()"}
	args := []any{}
	if u.Name != nil {
		args = append(args, *u.Name)
		sets = append(sets, "name = $"+itoa(len(args)))
	}
	if u.Description != nil {
		args = append(args, *u.Description)
		sets = append(sets, "description = $"+itoa(len(args)))
	}
	if u.Status != nil {
		args = append(args, *u.Status)
		sets = append(sets, "status = $"+itoa(len(args)))
	}
	if u.Template != nil {
		args = append(args, *u.Template)
		sets = append(sets, "template = $"+itoa(len(args)))
	}
	if u.RuntimeEnv != nil {
		args = append(args, *u.RuntimeEnv)
		sets = append(sets, "runtime_env = $"+itoa(len(args)))
	}
	if u.BusinessScenario != nil {
		args = append(args, *u.BusinessScenario)
		sets = append(sets, "business_scenario = $"+itoa(len(args)))
	}
	if u.CodeProvider != nil {
		args = append(args, *u.CodeProvider)
		sets = append(sets, "code_provider = $"+itoa(len(args)))
	}
	if len(args) == 0 {
		return nil
	}
	args = append(args, id, workspaceID)
	query := "UPDATE projects SET " + strings.Join(sets, ", ") + " WHERE id = $" + itoa(len(args)-1) + " AND workspace_id = $" + itoa(len(args))
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Server) deleteProject(id, workspaceID string) error {
	_, err := s.db.Exec(`DELETE FROM projects WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	return err
}

// ---- features ------------------------------------------------------

// replaceProjectFeatures atomically swaps the feature list. Used by
// the decompose endpoint — re-decomposing wipes the old list rather
// than appending, so the user iterates on the prompt without leftover
// stale items mixing in.
func (s *Server) replaceProjectFeatures(projectID string, features []ProjectFeature) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM project_features WHERE project_id = $1`, projectID); err != nil {
		return err
	}
	for i := range features {
		f := &features[i]
		if f.ID == "" {
			f.ID = generateResourceID()
		}
		f.ProjectID = projectID
		f.Ord = i
		if f.Complexity == "" {
			f.Complexity = ProjectFeatureComplexityMedium
		}
		f.CreatedAt = time.Now().UTC()
		if _, err := tx.Exec(
			`INSERT INTO project_features (id, project_id, ord, title, description, complexity, selected, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			f.ID, projectID, f.Ord, f.Title, f.Description, f.Complexity, f.Selected, f.CreatedAt,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) listProjectFeatures(projectID string) ([]ProjectFeature, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, ord, title, description, complexity, selected, created_at
		 FROM project_features WHERE project_id = $1 ORDER BY ord ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectFeature{}
	for rows.Next() {
		var f ProjectFeature
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.Ord, &f.Title, &f.Description, &f.Complexity, &f.Selected, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Server) updateProjectFeature(projectID, featureID, title, description, complexity string, selected bool) error {
	res, err := s.db.Exec(
		`UPDATE project_features SET title = $1, description = $2, complexity = $3, selected = $4
		 WHERE id = $5 AND project_id = $6`,
		title, description, complexity, selected, featureID, projectID,
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ---- tasks ---------------------------------------------------------

// generatePlanFromSelectedFeatures creates one task per selected
// feature that doesn't already have a task. Idempotent — calling
// twice doesn't duplicate. Tasks track the source feature_id so a
// re-decompose can leave the old tasks untouched.
func (s *Server) generatePlanFromSelectedFeatures(projectID string) ([]ProjectTask, error) {
	features, err := s.listProjectFeatures(projectID)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := []ProjectTask{}
	for _, f := range features {
		if !f.Selected {
			continue
		}
		// Skip if a task already exists for this feature.
		var existing string
		err := tx.QueryRow(
			`SELECT id FROM project_tasks WHERE project_id = $1 AND feature_id = $2 LIMIT 1`,
			projectID, f.ID,
		).Scan(&existing)
		if err == nil && existing != "" {
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		t := ProjectTask{
			ID:          generateResourceID(),
			ProjectID:   projectID,
			Title:       f.Title,
			Description: f.Description,
			Status:      ProjectTaskStatusTodo,
			Ord:         f.Ord,
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}
		fid := f.ID
		t.FeatureID = &fid
		if _, err := tx.Exec(
			`INSERT INTO project_tasks (id, project_id, feature_id, title, description, status, ord, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
			t.ID, t.ProjectID, t.FeatureID, t.Title, t.Description, t.Status, t.Ord, t.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if _, err := tx.Exec(
		`UPDATE projects SET status = $1, updated_at = $2 WHERE id = $3 AND status = $4`,
		ProjectStatusBuilding, time.Now().UTC(), projectID, ProjectStatusPlanning,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) listProjectTasks(projectID string) ([]ProjectTask, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, feature_id, title, description, status, bot_user_id, chat_thread_id, llm_thread_id, ord, created_at, updated_at, started_at, completed_at
		 FROM project_tasks WHERE project_id = $1 ORDER BY ord ASC, created_at ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectTask{}
	for rows.Next() {
		var t ProjectTask
		var featureID, botUserID sql.NullString
		var chatThreadID, llmThreadID sql.NullInt64
		var startedAt, completedAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.ProjectID, &featureID, &t.Title, &t.Description, &t.Status, &botUserID, &chatThreadID, &llmThreadID, &t.Ord, &t.CreatedAt, &t.UpdatedAt, &startedAt, &completedAt); err != nil {
			return nil, err
		}
		if featureID.Valid {
			v := featureID.String
			t.FeatureID = &v
		}
		if botUserID.Valid {
			v := botUserID.String
			t.BotUserID = &v
		}
		if chatThreadID.Valid {
			v := chatThreadID.Int64
			t.ChatThreadID = &v
		}
		if llmThreadID.Valid {
			v := llmThreadID.Int64
			t.LLMThreadID = &v
		}
		if startedAt.Valid {
			v := startedAt.Time
			t.StartedAt = &v
		}
		if completedAt.Valid {
			v := completedAt.Time
			t.CompletedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// loadProjectTask is the single-row variant of listProjectTasks.
// Used by pickup to fetch the task we're updating.
func (s *Server) loadProjectTask(projectID, taskID string) (*ProjectTask, error) {
	row := s.db.QueryRow(
		`SELECT id, project_id, feature_id, title, description, status, bot_user_id, chat_thread_id, llm_thread_id, ord, created_at, updated_at, started_at, completed_at
		 FROM project_tasks WHERE project_id = $1 AND id = $2`,
		projectID, taskID,
	)
	var t ProjectTask
	var featureID, botUserID sql.NullString
	var chatThreadID, llmThreadID sql.NullInt64
	var startedAt, completedAt sql.NullTime
	if err := row.Scan(&t.ID, &t.ProjectID, &featureID, &t.Title, &t.Description, &t.Status, &botUserID, &chatThreadID, &llmThreadID, &t.Ord, &t.CreatedAt, &t.UpdatedAt, &startedAt, &completedAt); err != nil {
		return nil, err
	}
	if featureID.Valid {
		v := featureID.String
		t.FeatureID = &v
	}
	if botUserID.Valid {
		v := botUserID.String
		t.BotUserID = &v
	}
	if chatThreadID.Valid {
		v := chatThreadID.Int64
		t.ChatThreadID = &v
	}
	if llmThreadID.Valid {
		v := llmThreadID.Int64
		t.LLMThreadID = &v
	}
	if startedAt.Valid {
		v := startedAt.Time
		t.StartedAt = &v
	}
	if completedAt.Valid {
		v := completedAt.Time
		t.CompletedAt = &v
	}
	return &t, nil
}

type ProjectTaskUpdate struct {
	Title       *string
	Description *string
	Status      *string
}

func (s *Server) updateProjectTask(projectID, taskID string, u ProjectTaskUpdate) error {
	sets := []string{"updated_at = NOW()"}
	args := []any{}
	if u.Title != nil {
		args = append(args, *u.Title)
		sets = append(sets, "title = $"+itoa(len(args)))
	}
	if u.Description != nil {
		args = append(args, *u.Description)
		sets = append(sets, "description = $"+itoa(len(args)))
	}
	if u.Status != nil {
		args = append(args, *u.Status)
		sets = append(sets, "status = $"+itoa(len(args)))
		// Track when in_progress / done occurs for timeline displays.
		switch *u.Status {
		case ProjectTaskStatusInProgress:
			sets = append(sets, "started_at = COALESCE(started_at, NOW())")
		case ProjectTaskStatusDone:
			sets = append(sets, "completed_at = NOW()")
		}
	}
	args = append(args, taskID, projectID)
	q := `UPDATE project_tasks SET ` + strings.Join(sets, ", ") +
		` WHERE id = $` + itoa(len(args)-1) + ` AND project_id = $` + itoa(len(args))
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// bulkUpdateProjectTaskStatus updates every task in the project
// (optionally filtered by current status) to a single new
// status. started_at / completed_at are stamped when transitioning
// to in_progress / done so the per-task timestamps stay coherent
// with the per-task updateProjectTask path. Returns the number
// of rows affected so the FE can show "已取消 5 个任务" etc.
func (s *Server) bulkUpdateProjectTaskStatus(projectID, target string, fromStatuses []string) (int64, error) {
	args := []any{target, projectID}
	q := `UPDATE project_tasks
	         SET status = $1,
	             updated_at = NOW(),
	             started_at = CASE WHEN $1 = 'in_progress' THEN COALESCE(started_at, NOW()) ELSE started_at END,
	             completed_at = CASE WHEN $1 = 'done' THEN NOW() ELSE completed_at END
	       WHERE project_id = $2`
	if len(fromStatuses) > 0 {
		args = append(args, pq.Array(fromStatuses))
		q += ` AND status = ANY($3)`
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// itoa lives in video_store.go — reusing it here.
