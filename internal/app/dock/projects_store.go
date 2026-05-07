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
	ID            string    `json:"id"`
	WorkspaceID   string    `json:"workspace_id"`
	CreatorUserID string    `json:"creator_user_id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Status        string    `json:"status"`
	Template      string    `json:"template"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
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
		`INSERT INTO projects (id, workspace_id, creator_user_id, name, description, status, template, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
		p.ID, p.WorkspaceID, p.CreatorUserID, p.Name, p.Description, p.Status, p.Template, now,
	)
	return err
}

func (s *Server) listProjects(workspaceID string) ([]Project, error) {
	rows, err := s.db.Query(
		`SELECT id, workspace_id, creator_user_id, name, description, status, template, created_at, updated_at
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
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.CreatorUserID, &p.Name, &p.Description, &p.Status, &p.Template, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Server) getProject(id, workspaceID string) (*Project, error) {
	var p Project
	err := s.db.QueryRow(
		`SELECT id, workspace_id, creator_user_id, name, description, status, template, created_at, updated_at
		 FROM projects WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID,
	).Scan(&p.ID, &p.WorkspaceID, &p.CreatorUserID, &p.Name, &p.Description, &p.Status, &p.Template, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Server) updateProjectMetadata(id, workspaceID, name, description, status, template string) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE projects SET name = $1, description = $2, status = $3, template = $4, updated_at = $5
		 WHERE id = $6 AND workspace_id = $7`,
		name, description, status, template, now, id, workspaceID,
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
		`SELECT id, project_id, feature_id, title, description, status, bot_user_id, chat_thread_id, ord, created_at, updated_at, started_at, completed_at
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
		var chatThreadID sql.NullInt64
		var startedAt, completedAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.ProjectID, &featureID, &t.Title, &t.Description, &t.Status, &botUserID, &chatThreadID, &t.Ord, &t.CreatedAt, &t.UpdatedAt, &startedAt, &completedAt); err != nil {
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

// itoa lives in video_store.go — reusing it here.
