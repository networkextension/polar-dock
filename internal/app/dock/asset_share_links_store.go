package dock

// asset_share_links_store.go — typed CRUD + short-code generator
// for WeTransfer-style anonymous links into the assets catalog.
//
// Short-code spec: 10-char base62 (~60 bits entropy). Generated via
// crypto/rand rejection sampling — no modulo bias. UNIQUE constraint
// on short_code; insertion retries on collision (statistically
// negligible at our scale).

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AssetShareLink mirrors a row in the asset_share_links table.
// MaxDownloads = nil means "unlimited". ExpiresAt = nil means
// "never expires" (operator can still revoke).
type AssetShareLink struct {
	ID            int64      `json:"id"`
	AssetID       int64      `json:"asset_id"`
	ShortCode     string     `json:"short_code"`
	CreatedBy     string     `json:"created_by"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	MaxDownloads  *int       `json:"max_downloads,omitempty"`
	DownloadCount int        `json:"download_count"`
	RequireLogin  bool       `json:"require_login"`
	CreatedAt     time.Time  `json:"created_at"`
}

const assetShareLinkColumns = `
	id, asset_id, short_code, created_by, expires_at, max_downloads,
	download_count, require_login, created_at
`

const shareCodeAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// generateAssetShareCode returns an n-character base62 random string
// via rejection sampling (no modulo bias). n must be in [4, 64].
func generateAssetShareCode(n int) (string, error) {
	if n < 4 || n > 64 {
		return "", fmt.Errorf("generateAssetShareCode: n=%d out of [4,64]", n)
	}
	const alphabetLen = byte(len(shareCodeAlphabet)) // 62
	// Largest multiple of alphabetLen that fits in a byte. Bytes above
	// this threshold get rejected to avoid modulo bias.
	const threshold = byte(256 - 256%62) // 248
	out := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; i++ {
		for {
			if _, err := rand.Read(buf); err != nil {
				return "", err
			}
			if buf[0] < threshold {
				out[i] = shareCodeAlphabet[buf[0]%alphabetLen]
				break
			}
		}
	}
	return string(out), nil
}

func scanAssetShareLink(r interface{ Scan(...any) error }) (*AssetShareLink, error) {
	var (
		link         AssetShareLink
		expiresAt    sql.NullTime
		maxDownloads sql.NullInt32
	)
	err := r.Scan(
		&link.ID, &link.AssetID, &link.ShortCode, &link.CreatedBy,
		&expiresAt, &maxDownloads, &link.DownloadCount,
		&link.RequireLogin, &link.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		link.ExpiresAt = &t
	}
	if maxDownloads.Valid {
		n := int(maxDownloads.Int32)
		link.MaxDownloads = &n
	}
	return &link, nil
}

// insertAssetShareLink creates a new row, retrying up to 3 times on
// short_code collision (very rare; documented in PR body). On
// repeated collision we give up — caller can retry the API call.
func (s *Server) insertAssetShareLink(link *AssetShareLink) (int64, error) {
	if link.AssetID <= 0 {
		return 0, errors.New("insertAssetShareLink: asset_id required")
	}
	if link.CreatedBy == "" {
		return 0, errors.New("insertAssetShareLink: created_by required")
	}
	var (
		id           int64
		now          = time.Now().UTC()
		expiresAt    = nullTimeOrPtr(link.ExpiresAt)
		maxDownloads = nullIntOrPtr(link.MaxDownloads)
	)
	for attempt := 0; attempt < 3; attempt++ {
		code, err := generateAssetShareCode(10)
		if err != nil {
			return 0, fmt.Errorf("generate code: %w", err)
		}
		err = s.db.QueryRow(
			`INSERT INTO asset_share_links
				(asset_id, short_code, created_by, expires_at,
				 max_downloads, require_login, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 RETURNING id`,
			link.AssetID, code, link.CreatedBy, expiresAt,
			maxDownloads, link.RequireLogin, now,
		).Scan(&id)
		if err == nil {
			link.ID = id
			link.ShortCode = code
			link.CreatedAt = now
			return id, nil
		}
		// Distinguish UNIQUE(short_code) collision from other errors.
		// pq error code "23505" = unique_violation.
		if isUniqueViolation(err) {
			continue
		}
		return 0, err
	}
	return 0, errors.New("insertAssetShareLink: short_code collision after 3 retries (transient — try again)")
}

// getAssetShareLinkByCode returns (nil, nil) on miss. Used by the
// public /s/:code route.
func (s *Server) getAssetShareLinkByCode(code string) (*AssetShareLink, error) {
	if code == "" {
		return nil, nil
	}
	row := s.db.QueryRow(
		`SELECT `+assetShareLinkColumns+`FROM asset_share_links WHERE short_code = $1`,
		code,
	)
	link, err := scanAssetShareLink(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return link, err
}

// getAssetShareLinkByID is the delete-own check pivot.
func (s *Server) getAssetShareLinkByID(id int64) (*AssetShareLink, error) {
	row := s.db.QueryRow(
		`SELECT `+assetShareLinkColumns+`FROM asset_share_links WHERE id = $1`,
		id,
	)
	link, err := scanAssetShareLink(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return link, err
}

// listAssetShareLinksByCaller returns the caller's own share links,
// most recent first. Doesn't join with assets (UI does that lookup
// separately if it wants the file name).
func (s *Server) listAssetShareLinksByCaller(userID string) ([]AssetShareLink, error) {
	rows, err := s.db.Query(
		`SELECT `+assetShareLinkColumns+`FROM asset_share_links
		  WHERE created_by = $1
		  ORDER BY created_at DESC LIMIT 200`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AssetShareLink, 0)
	for rows.Next() {
		link, err := scanAssetShareLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *link)
	}
	return out, rows.Err()
}

// incrementShareLinkDownload bumps the counter atomically and
// returns the new value. Done in the public route AFTER all gates
// pass (expiry, max_downloads, require_login) but BEFORE we start
// streaming bytes — accept a small over-count from interrupted
// downloads rather than risk under-counting on a successful stream.
func (s *Server) incrementShareLinkDownload(id int64) (int, error) {
	var newCount int
	err := s.db.QueryRow(
		`UPDATE asset_share_links
		    SET download_count = download_count + 1
		  WHERE id = $1
		  RETURNING download_count`,
		id,
	).Scan(&newCount)
	return newCount, err
}

// deleteAssetShareLink removes the row. ON DELETE SET NULL cascade
// in the asset_usage_events table preserves historical event rows.
func (s *Server) deleteAssetShareLink(id int64) error {
	res, err := s.db.Exec(`DELETE FROM asset_share_links WHERE id = $1`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ── tiny helpers ────────────────────────────────────────────────────

func nullTimeOrPtr(p *time.Time) sql.NullTime {
	if p == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *p, Valid: true}
}

func nullIntOrPtr(p *int) sql.NullInt32 {
	if p == nil {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Int32: int32(*p), Valid: true}
}

// isUniqueViolation classifies a Postgres unique_violation error
// without importing lib/pq at the type-assertion layer (kept loose
// so the test suite can fake it).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return containsCode(err, "23505")
}

// containsCode checks whether err's message includes the SQLSTATE
// code (looser than typed pq.Error matching but adequate for the
// 3-retry insertion path).
func containsCode(err error, code string) bool {
	return err != nil && code != "" &&
		(len(err.Error()) > 0 &&
			(stringContains(err.Error(), code)))
}

// stringContains is `strings.Contains` without the import (helpers
// file is intentionally tiny).
func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
