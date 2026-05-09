package dock

// Storage layer for iosdist_agent_builds — the agent-driven build
// pipeline (coder produces unsigned IPA → polar-agent uploads to
// dock → fetches creds → signs locally → uploads to TestFlight).
// Distinct from iosdist_sign_tasks (server-side zsign worker).

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	IOSAgentBuildStatusInit      = "init"
	IOSAgentBuildStatusUploaded  = "uploaded"
	IOSAgentBuildStatusSigning   = "signing"
	IOSAgentBuildStatusUploading = "uploading"
	IOSAgentBuildStatusSucceeded = "succeeded"
	IOSAgentBuildStatusFailed    = "failed"
)

type IOSAgentBuild struct {
	ID             int64      `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	ProjectID      *string    `json:"project_id,omitempty"`
	IOSDistAppID   *int64     `json:"iosdist_app_id,omitempty"`
	AgentUserID    string     `json:"agent_user_id"`
	Status         string     `json:"status"`
	SignMethod     string     `json:"sign_method"`
	SourceFilename string     `json:"source_filename"`
	SourceSize     int64      `json:"source_size"`
	SourceIPAUrl   string     `json:"source_ipa_url"`
	SignedIPAUrl   string     `json:"signed_ipa_url"`
	ASCBuildID     string     `json:"asc_build_id"`
	ErrorMessage   string     `json:"error_message"`
	LogText        string     `json:"log_text"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

const iosAgentBuildSelectColumns = `id, workspace_id, project_id, iosdist_app_id, agent_user_id, status, sign_method, source_filename, source_size, source_ipa_url, signed_ipa_url, asc_build_id, error_message, log_text, created_at, updated_at, completed_at`

func scanIOSAgentBuild(scanFn func(...any) error) (*IOSAgentBuild, error) {
	var b IOSAgentBuild
	var projectID sql.NullString
	var appID sql.NullInt64
	if err := scanFn(
		&b.ID, &b.WorkspaceID, &projectID, &appID, &b.AgentUserID,
		&b.Status, &b.SignMethod, &b.SourceFilename, &b.SourceSize,
		&b.SourceIPAUrl, &b.SignedIPAUrl, &b.ASCBuildID,
		&b.ErrorMessage, &b.LogText, &b.CreatedAt, &b.UpdatedAt,
		&b.CompletedAt,
	); err != nil {
		return nil, err
	}
	if projectID.Valid {
		v := projectID.String
		b.ProjectID = &v
	}
	if appID.Valid {
		v := appID.Int64
		b.IOSDistAppID = &v
	}
	return &b, nil
}

func (s *Server) createIOSAgentBuild(b *IOSAgentBuild) (int64, error) {
	if b.WorkspaceID == "" {
		return 0, errors.New("workspace_id required")
	}
	if b.Status == "" {
		b.Status = IOSAgentBuildStatusInit
	}
	row := s.db.QueryRow(
		`INSERT INTO iosdist_agent_builds
		   (workspace_id, project_id, iosdist_app_id, agent_user_id,
		    status, source_filename, source_size, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		 RETURNING id`,
		b.WorkspaceID, b.ProjectID, b.IOSDistAppID, b.AgentUserID,
		b.Status, b.SourceFilename, b.SourceSize,
	)
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	b.ID = id
	return id, nil
}

func (s *Server) getIOSAgentBuild(id int64) (*IOSAgentBuild, error) {
	row := s.db.QueryRow(
		`SELECT `+iosAgentBuildSelectColumns+` FROM iosdist_agent_builds WHERE id = $1`,
		id,
	)
	b, err := scanIOSAgentBuild(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}

func (s *Server) updateIOSAgentBuildStatus(id int64, status, signMethod, sourceURL string) error {
	_, err := s.db.Exec(
		`UPDATE iosdist_agent_builds
		 SET status = $2,
		     sign_method = COALESCE(NULLIF($3, ''), sign_method),
		     source_ipa_url = COALESCE(NULLIF($4, ''), source_ipa_url),
		     updated_at = NOW()
		 WHERE id = $1`,
		id, status, strings.TrimSpace(signMethod), strings.TrimSpace(sourceURL),
	)
	return err
}

func (s *Server) markIOSAgentBuildSucceeded(id int64, signedURL, ascBuildID, signMethod, log string) error {
	_, err := s.db.Exec(
		`UPDATE iosdist_agent_builds
		 SET status = $2,
		     signed_ipa_url = $3,
		     asc_build_id = $4,
		     sign_method = COALESCE(NULLIF($5, ''), sign_method),
		     log_text = LEFT($6, 4096),
		     updated_at = NOW(),
		     completed_at = NOW()
		 WHERE id = $1`,
		id, IOSAgentBuildStatusSucceeded, signedURL, ascBuildID, signMethod, log,
	)
	return err
}

func (s *Server) markIOSAgentBuildFailed(id int64, errMsg, signMethod, log string) error {
	_, err := s.db.Exec(
		`UPDATE iosdist_agent_builds
		 SET status = $2,
		     error_message = LEFT($3, 1024),
		     sign_method = COALESCE(NULLIF($4, ''), sign_method),
		     log_text = LEFT($5, 4096),
		     updated_at = NOW(),
		     completed_at = NOW()
		 WHERE id = $1`,
		id, IOSAgentBuildStatusFailed, errMsg, signMethod, log,
	)
	return err
}

// listIOSAgentBuildsForWorkspace returns most recent N builds for
// surfacing in the project UI.
func (s *Server) listIOSAgentBuildsForWorkspace(workspaceID string, limit int) ([]IOSAgentBuild, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT `+iosAgentBuildSelectColumns+`
		 FROM iosdist_agent_builds
		 WHERE workspace_id = $1
		 ORDER BY updated_at DESC
		 LIMIT $2`,
		workspaceID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IOSAgentBuild{}
	for rows.Next() {
		b, err := scanIOSAgentBuild(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// pickDefaultDistributionCert returns the most recently updated
// non-expired distribution cert in the workspace. Returns nil + nil
// if none found (caller decides 404 vs ambiguity error). Looks up
// the id, then loads via getIOSCertificate to reuse its decryption
// + scan path.
func (s *Server) pickDefaultDistributionCert(workspaceID string) (*IOSCertificate, error) {
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM iosdist_certificates
		 WHERE workspace_id = $1 AND kind = 'distribution'
		   AND (expires_at IS NULL OR expires_at > NOW())
		 ORDER BY updated_at DESC LIMIT 1`,
		workspaceID,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return s.getIOSCertificate(id, workspaceID)
}

// pickDefaultAppStoreProfile returns the most recent non-expired
// app_store profile in the workspace. Bundle-id matching is
// best-effort: prefers exact match, falls back to wildcard or empty.
func (s *Server) pickDefaultAppStoreProfile(workspaceID, bundleID string) (*IOSProvisioningProfile, error) {
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM iosdist_profiles
		 WHERE workspace_id = $1 AND kind = 'app_store'
		   AND (expires_at IS NULL OR expires_at > NOW())
		   AND ($2 = '' OR app_id = $2 OR app_id = '' OR app_id LIKE '%.*')
		 ORDER BY (CASE WHEN app_id = $2 THEN 0 ELSE 1 END), updated_at DESC LIMIT 1`,
		workspaceID, strings.TrimSpace(bundleID),
	).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return s.getIOSProfileForOwner(id, workspaceID)
}
