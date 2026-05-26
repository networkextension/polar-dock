package dock

// video_billing_store.go — typed CRUD for video_shot_calls, the
// dock-side roll-up table that polar-video plugin POSTs into via
// /internal/v1/billing/video-shots. Mirrors llm_proxy_store.go so the
// existing workspace-wallet plumbing (recharge / month-spend SUM)
// consumes both ledgers without special-casing.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// VideoShotCall is one shot's billing row in the workspace-scoped
// ledger. Nullable fields are pointers so a "provider didn't return
// fps" miss can be distinguished from a real 0.
type VideoShotCall struct {
	ID                 int64      `json:"id"`
	WorkspaceID        string     `json:"workspace_id"`
	ProjectID          int64      `json:"project_id"`
	ShotID             int64      `json:"shot_id"`
	Provider           string     `json:"provider"`
	Model              string     `json:"model,omitempty"`
	Resolution         string     `json:"resolution,omitempty"`
	DurationChargedSec *float64   `json:"duration_charged_sec,omitempty"`
	FPS                *int       `json:"fps,omitempty"`
	FramesTotal        *int       `json:"frames_total,omitempty"`
	CostUSD            *float64   `json:"cost_usd,omitempty"`
	CostPerFrameUSD    *float64   `json:"cost_per_frame_usd,omitempty"`
	BillingMeta        []byte     `json:"billing_meta,omitempty"` // raw JSONB
	CreatedAt          time.Time  `json:"created_at"`
}

// insertVideoShotCall writes one ledger row. Idempotent: relies on
// UNIQUE(shot_id) — a re-post for the same shot returns
// (inserted=false, err=nil). Callers do not need to distinguish
// between never-seen and already-billed for the 200 OK response.
func (s *Server) insertVideoShotCall(call *VideoShotCall) (inserted bool, err error) {
	if call == nil {
		return false, fmt.Errorf("nil call")
	}
	if call.WorkspaceID == "" || call.ShotID == 0 || call.Provider == "" {
		return false, fmt.Errorf("workspace_id, shot_id, provider required")
	}
	dur := nullFloat(call.DurationChargedSec)
	fps := nullInt(call.FPS)
	frames := nullInt(call.FramesTotal)
	cost := nullFloat(call.CostUSD)
	perFrame := nullFloat(call.CostPerFrameUSD)
	var meta interface{}
	if len(call.BillingMeta) > 0 {
		meta = call.BillingMeta
	}
	createdAt := call.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	row := s.db.QueryRow(
		`INSERT INTO video_shot_calls (
		    workspace_id, project_id, shot_id, provider, model, resolution,
		    duration_charged_sec, fps, frames_total,
		    cost_usd, cost_per_frame_usd, billing_meta, created_at
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 ON CONFLICT (shot_id) DO NOTHING
		 RETURNING id`,
		call.WorkspaceID, call.ProjectID, call.ShotID, call.Provider, call.Model, call.Resolution,
		dur, fps, frames,
		cost, perFrame, meta, createdAt,
	)
	var newID int64
	if err := row.Scan(&newID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// already billed for this shot — that's the expected
			// dedup path, not a failure.
			return false, nil
		}
		return false, err
	}
	call.ID = newID
	return true, nil
}

// listVideoShotCallsByProject returns one project's shot calls, newest
// first. Workspace scoping is the caller's responsibility (handlers
// should join project_id → workspace check or pass workspace_id too).
func (s *Server) listVideoShotCallsByProject(projectID int64, limit int) ([]VideoShotCall, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT id, workspace_id, project_id, shot_id, provider,
		        COALESCE(model, ''), COALESCE(resolution, ''),
		        duration_charged_sec, fps, frames_total,
		        cost_usd, cost_per_frame_usd, billing_meta, created_at
		   FROM video_shot_calls
		  WHERE project_id = $1
		  ORDER BY created_at DESC, id DESC
		  LIMIT $2`,
		projectID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]VideoShotCall, 0, limit)
	for rows.Next() {
		c, scanErr := scanVideoShotCall(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// VideoBurnRow is one provider's per-period aggregate.
type VideoBurnRow struct {
	Provider           string  `json:"provider"`
	Shots              int64   `json:"shots"`
	TotalDurationSec   float64 `json:"total_duration_sec"`
	TotalFrames        int64   `json:"total_frames"`
	TotalCostUSD       float64 `json:"total_cost_usd"`
	AvgCostPerFrameUSD float64 `json:"avg_cost_per_frame_usd"`
}

// aggregateVideoBurn returns the per-provider rollup for a workspace
// over a half-open [since, until) window. Pass workspaceID="" to get
// global numbers (admin only — handler must enforce).
func (s *Server) aggregateVideoBurn(workspaceID string, since, until time.Time) ([]VideoBurnRow, error) {
	q := `SELECT provider,
	             COUNT(*) AS shots,
	             COALESCE(SUM(duration_charged_sec), 0) AS total_dur,
	             COALESCE(SUM(frames_total), 0) AS total_frames,
	             COALESCE(SUM(cost_usd), 0) AS total_cost,
	             CASE WHEN COALESCE(SUM(frames_total), 0) = 0 THEN 0
	                  ELSE COALESCE(SUM(cost_usd), 0) / SUM(frames_total)
	             END AS avg_per_frame
	        FROM video_shot_calls
	       WHERE created_at >= $1 AND created_at < $2 `
	args := []interface{}{since, until}
	if workspaceID != "" {
		q += `AND workspace_id = $3 `
		args = append(args, workspaceID)
	}
	q += `GROUP BY provider ORDER BY total_cost DESC, provider`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]VideoBurnRow, 0, 4)
	for rows.Next() {
		var r VideoBurnRow
		if err := rows.Scan(&r.Provider, &r.Shots, &r.TotalDurationSec, &r.TotalFrames, &r.TotalCostUSD, &r.AvgCostPerFrameUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanVideoShotCall is the row -> struct adapter. Pass row.Scan or
// rows.Scan directly. Keeps null handling centralised.
func scanVideoShotCall(scan func(...interface{}) error) (VideoShotCall, error) {
	var (
		c        VideoShotCall
		dur      sql.NullFloat64
		fps      sql.NullInt64
		frames   sql.NullInt64
		cost     sql.NullFloat64
		perFrame sql.NullFloat64
	)
	err := scan(
		&c.ID, &c.WorkspaceID, &c.ProjectID, &c.ShotID, &c.Provider,
		&c.Model, &c.Resolution,
		&dur, &fps, &frames,
		&cost, &perFrame, &c.BillingMeta, &c.CreatedAt,
	)
	if err != nil {
		return c, err
	}
	if dur.Valid {
		v := dur.Float64
		c.DurationChargedSec = &v
	}
	if fps.Valid {
		v := int(fps.Int64)
		c.FPS = &v
	}
	if frames.Valid {
		v := int(frames.Int64)
		c.FramesTotal = &v
	}
	if cost.Valid {
		v := cost.Float64
		c.CostUSD = &v
	}
	if perFrame.Valid {
		v := perFrame.Float64
		c.CostPerFrameUSD = &v
	}
	return c, nil
}

func nullFloat(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}
