package dock

// asset_store.go — typed CRUD over the assets table (and friends).
//
// Scope here is **catalog only** — the actual blob bytes live on the
// filesystem under <uploadDir>/assets/ (stub mode for P1a). P3 swaps
// the in-handler `streamBlobFromLocalDir` for a 302 to a provider
// plugin; the store doesn't care which.
//
// asset_share_links, asset_quotas, asset_usage_events get their own
// stores in P1b/P1c (this PR doesn't touch them).

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Asset mirrors a row in the `assets` table. WorkspaceID is *string
// because the column is nullable (NULL = platform-owned). SourceURL
// is *string for the same reason. Metadata is JSONB carried as raw
// bytes — handlers marshal/unmarshal at the seam.
type Asset struct {
	ID          int64     `json:"id"`
	WorkspaceID *string   `json:"workspace_id,omitempty"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	SHA256      string    `json:"sha256"`
	SizeBytes   int64     `json:"size_bytes"`
	Mime        string    `json:"mime"`
	Visibility  string    `json:"visibility"`
	SourceURL   *string   `json:"source_url,omitempty"`
	UploadedBy  string    `json:"uploaded_by"`
	Pinned      bool      `json:"pinned"`
	Metadata    []byte    `json:"metadata,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AssetListFilter narrows a list query. All fields optional. WorkspaceID
// = nil means "no workspace filter" (caller wraps with public-platform
// fallback). When the caller wants the platform-only side, pass a
// sentinel "" string for ScopePlatform.
type AssetListFilter struct {
	Caller       string  // user id (for audit; unused in SQL today)
	WorkspaceID  *string // nil → don't filter on workspace
	Kind         string  // empty → all kinds
	NamePrefix   string  // empty → no prefix filter
	Limit        int     // <=0 → default 100
	Offset       int     // <0 → 0
	WantPlatform bool    // true → also include workspace_id IS NULL AND visibility = 'public'
}

const assetSelectColumns = `
	id, workspace_id, kind, name, version, sha256, size_bytes, mime,
	visibility, source_url, uploaded_by, pinned, metadata,
	created_at, updated_at
`

// scanAssetRow unpacks one row (works for *sql.Row and *sql.Rows).
type assetScanner interface {
	Scan(dest ...any) error
}

func scanAssetRow(r assetScanner) (*Asset, error) {
	var (
		a           Asset
		workspaceID sql.NullString
		sourceURL   sql.NullString
	)
	err := r.Scan(
		&a.ID, &workspaceID, &a.Kind, &a.Name, &a.Version,
		&a.SHA256, &a.SizeBytes, &a.Mime, &a.Visibility, &sourceURL,
		&a.UploadedBy, &a.Pinned, &a.Metadata, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if workspaceID.Valid {
		v := workspaceID.String
		a.WorkspaceID = &v
	}
	if sourceURL.Valid {
		v := sourceURL.String
		a.SourceURL = &v
	}
	return &a, nil
}

// insertAsset creates a new row; returns the assigned id. Caller MUST
// have already enforced quotas + ACL.
//
// Idempotency: the table has UNIQUE NULLS NOT DISTINCT
// (workspace_id, kind, name, version); a duplicate insert returns a
// pq unique-violation that the handler classifies as 409. We do NOT
// upsert here — overwriting silently on POST is a footgun.
func (s *Server) insertAsset(a *Asset) (int64, error) {
	if a.Kind == "" || a.Name == "" || a.SHA256 == "" {
		return 0, errors.New("insertAsset: kind/name/sha256 required")
	}
	if a.Version == "" {
		a.Version = "v1"
	}
	if a.Visibility == "" {
		a.Visibility = "private"
	}
	if a.Mime == "" {
		a.Mime = "application/octet-stream"
	}
	if a.UploadedBy == "" {
		return 0, errors.New("insertAsset: uploaded_by required (use 'system' for platform)")
	}
	metadata := a.Metadata
	if len(metadata) == 0 {
		metadata = []byte(`{}`)
	}
	var (
		id          int64
		now         = time.Now().UTC()
		workspaceID = nullStringOrPtr(a.WorkspaceID)
		sourceURL   = nullStringOrPtr(a.SourceURL)
	)
	err := s.db.QueryRow(
		`INSERT INTO assets
			(workspace_id, kind, name, version, sha256, size_bytes, mime,
			 visibility, source_url, uploaded_by, pinned, metadata,
			 created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13, $14)
		 RETURNING id`,
		workspaceID, a.Kind, a.Name, a.Version, a.SHA256, a.SizeBytes, a.Mime,
		a.Visibility, sourceURL, a.UploadedBy, a.Pinned, string(metadata), now, now,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	a.ID = id
	a.CreatedAt = now
	a.UpdatedAt = now
	return id, nil
}

// getAssetByID returns a single row, or (nil, nil) on miss.
func (s *Server) getAssetByID(id int64) (*Asset, error) {
	row := s.db.QueryRow(`SELECT `+assetSelectColumns+`FROM assets WHERE id = $1`, id)
	a, err := scanAssetRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// getAssetByName looks up by composite key. workspaceID = nil matches
// the platform rows (workspace_id IS NULL) — `UNIQUE NULLS NOT
// DISTINCT` makes the row unique on that key set.
func (s *Server) getAssetByName(workspaceID *string, kind, name, version string) (*Asset, error) {
	if version == "" {
		version = "v1"
	}
	var (
		row *sql.Row
	)
	if workspaceID == nil {
		row = s.db.QueryRow(
			`SELECT `+assetSelectColumns+`FROM assets
			  WHERE workspace_id IS NULL AND kind=$1 AND name=$2 AND version=$3`,
			kind, name, version,
		)
	} else {
		row = s.db.QueryRow(
			`SELECT `+assetSelectColumns+`FROM assets
			  WHERE workspace_id=$1 AND kind=$2 AND name=$3 AND version=$4`,
			*workspaceID, kind, name, version,
		)
	}
	a, err := scanAssetRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// listAssets handles the tenant + platform-public listing scope:
//
//	(workspace_id = filter.WorkspaceID)
//	  OR (filter.WantPlatform AND workspace_id IS NULL AND visibility = 'public')
//
// Adding `Kind` / `NamePrefix` narrows further. Always ORDER BY
// created_at DESC. Limit clamped to [1, 1000]; default 100.
func (s *Server) listAssets(filter AssetListFilter) ([]Asset, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > 1000 {
		filter.Limit = 1000
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	var (
		conds []string
		args  []any
	)
	idx := func() string {
		args = append(args, nil) // placeholder, real value pushed by caller right after
		return fmt.Sprintf("$%d", len(args))
	}
	_ = idx // kept for symmetry; we build args manually below

	// Visibility scope clause:
	switch {
	case filter.WorkspaceID != nil && filter.WantPlatform:
		args = append(args, *filter.WorkspaceID)
		conds = append(conds,
			fmt.Sprintf("(workspace_id = $%d OR (workspace_id IS NULL AND visibility = 'public'))", len(args)))
	case filter.WorkspaceID != nil:
		args = append(args, *filter.WorkspaceID)
		conds = append(conds, fmt.Sprintf("workspace_id = $%d", len(args)))
	case filter.WantPlatform:
		conds = append(conds, `workspace_id IS NULL`)
	default:
		// no scope → matches nothing; refuse rather than dump everything
		return []Asset{}, nil
	}

	if filter.Kind != "" {
		args = append(args, filter.Kind)
		conds = append(conds, fmt.Sprintf("kind = $%d", len(args)))
	}
	if filter.NamePrefix != "" {
		args = append(args, filter.NamePrefix+"%")
		conds = append(conds, fmt.Sprintf("name LIKE $%d", len(args)))
	}

	args = append(args, filter.Limit, filter.Offset)
	limitPh := fmt.Sprintf("$%d", len(args)-1)
	offsetPh := fmt.Sprintf("$%d", len(args))

	q := `SELECT ` + assetSelectColumns +
		`FROM assets WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY created_at DESC LIMIT ` + limitPh + ` OFFSET ` + offsetPh

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Asset, 0, filter.Limit)
	for rows.Next() {
		a, err := scanAssetRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// listAssetFolders does a path-prefix groupby for the fake-folder UI.
// Given prefix="video/sources/" returns the distinct next-segment
// children (e.g. ["video/sources/proj_42/", "video/sources/proj_43/"])
// plus the leaf asset names with no further "/" (e.g.
// ["video/sources/orphan.mp4"]).
//
// For v1 we just return the distinct path-prefix-up-to-next-slash
// strings; the UI decides folder vs file display by trailing slash.
func (s *Server) listAssetFolders(workspaceID *string, prefix string) ([]string, error) {
	prefix = strings.TrimLeft(prefix, "/")
	pattern := prefix + "%"
	var (
		rows *sql.Rows
		err  error
	)
	if workspaceID == nil {
		rows, err = s.db.Query(
			`SELECT DISTINCT name FROM assets
			  WHERE workspace_id IS NULL AND name LIKE $1
			  ORDER BY name`, pattern)
	} else {
		rows, err = s.db.Query(
			`SELECT DISTINCT name FROM assets
			  WHERE workspace_id = $1 AND name LIKE $2
			  ORDER BY name`, *workspaceID, pattern)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[string]struct{}{}
	out := make([]string, 0)
	plen := len(prefix)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		// Strip the prefix; the remainder is either "leaf" or "dir/...".
		rem := n[plen:]
		slashIdx := strings.IndexByte(rem, '/')
		var entry string
		if slashIdx < 0 {
			entry = n // leaf file
		} else {
			entry = prefix + rem[:slashIdx+1] // folder (trailing slash)
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// deleteAsset removes the catalog row. Cascades through
// asset_locations + asset_share_links + (asset_usage_events
// SET NULL). Blob file removal is the handler's job — the store
// only knows the DB.
func (s *Server) deleteAsset(id int64) error {
	res, err := s.db.Exec(`DELETE FROM assets WHERE id = $1`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// nullStringOrPtr converts a *string into sql.NullString. Empty
// strings on the pointer side still translate to Valid=true with ""
// — callers should pre-check if they want empty=NULL semantics.
func nullStringOrPtr(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}
