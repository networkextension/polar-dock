package dock

// asset_provider_store.go — typed CRUD over asset_providers
// (catalog of edge-cache providers) + asset_locations (the
// per-asset, per-provider replication index).
//
// Two responsibilities:
//
//   1. Provider registry — admin creates rows; provider plugin
//      heartbeat keeps them fresh. Insertion mints a fresh HMAC
//      token (returned plaintext to the admin once).
//
//   2. Locations index — when a successful upload-to-provider
//      completes, we INSERT into asset_locations(status='ready')
//      so the picker knows that provider has the blob. Future P4
//      warm-pull will write 'fetching'/'missing' rows too.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AssetProvider mirrors asset_providers row plus a transient
// HMACToken field for the create-flow one-time plaintext return.
type AssetProvider struct {
	ID          int64      `json:"id"`
	Slug        string     `json:"slug"`
	BaseURL     string     `json:"base_url"`
	Region      string     `json:"region"`
	CapacityGB  int        `json:"capacity_gb"`
	HMACToken   string     `json:"hmac_token,omitempty"` // plaintext; populated only on insert response
	LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

const assetProviderColumns = `
	id, slug, base_url, region, capacity_gb, hmac_token,
	last_seen_at, created_at
`

// assetProviderColumnsP is the same list with each column prefixed by
// `p.`, for use in joined queries where bare column names (notably
// created_at) would be ambiguous against plugin_modules.
const assetProviderColumnsP = `
	p.id, p.slug, p.base_url, p.region, p.capacity_gb, p.hmac_token,
	p.last_seen_at, p.created_at
`

func scanAssetProvider(r interface{ Scan(...any) error }) (*AssetProvider, error) {
	var (
		p          AssetProvider
		lastSeenAt sql.NullTime
	)
	err := r.Scan(
		&p.ID, &p.Slug, &p.BaseURL, &p.Region, &p.CapacityGB,
		&p.HMACToken, &lastSeenAt, &p.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if lastSeenAt.Valid {
		t := lastSeenAt.Time
		p.LastSeenAt = &t
	}
	return &p, nil
}

// generateProviderHMACToken returns 32 random bytes hex-encoded
// (64 chars). Unguessable; used as the shared HMAC key for the
// signed-URL flow.
func generateProviderHMACToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "polar_assets_" + hex.EncodeToString(b), nil
}

// insertAssetProvider mints a fresh HMAC token + inserts the row.
// Returns the row with HMACToken populated for the one-time
// admin-side display.
func (s *Server) insertAssetProvider(in *AssetProvider) (*AssetProvider, error) {
	if in.Slug == "" || in.BaseURL == "" || in.Region == "" {
		return nil, errors.New("insertAssetProvider: slug/base_url/region required")
	}
	if in.CapacityGB <= 0 {
		return nil, errors.New("insertAssetProvider: capacity_gb must be > 0")
	}
	token, err := generateProviderHMACToken()
	if err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	now := time.Now().UTC()
	var id int64
	err = s.db.QueryRow(
		`INSERT INTO asset_providers
			(slug, base_url, region, capacity_gb, hmac_token, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		in.Slug, in.BaseURL, in.Region, in.CapacityGB, token, now,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	in.ID = id
	in.HMACToken = token
	in.CreatedAt = now
	return in, nil
}

// listAssetProviders returns all providers ordered by creation
// time. Admin viewer only.
func (s *Server) listAssetProviders() ([]AssetProvider, error) {
	rows, err := s.db.Query(`SELECT ` + assetProviderColumns + `FROM asset_providers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AssetProvider, 0)
	for rows.Next() {
		p, err := scanAssetProvider(rows)
		if err != nil {
			return nil, err
		}
		// Don't leak the HMAC token to listers — only the create
		// response gets the plaintext. The DB column stays populated
		// for verification; UI just strips it.
		p.HMACToken = ""
		out = append(out, *p)
	}
	return out, rows.Err()
}

// getAssetProviderByID returns one row (with HMACToken populated —
// used for signing the URL). Returns (nil, nil) on miss.
func (s *Server) getAssetProviderByID(id int64) (*AssetProvider, error) {
	row := s.db.QueryRow(
		`SELECT `+assetProviderColumns+`FROM asset_providers WHERE id = $1`,
		id,
	)
	p, err := scanAssetProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// getAssetProviderBySlug — heartbeat path; identifies provider by
// the slug it sends. Returns (nil, nil) on miss.
func (s *Server) getAssetProviderBySlug(slug string) (*AssetProvider, error) {
	row := s.db.QueryRow(
		`SELECT `+assetProviderColumns+`FROM asset_providers WHERE slug = $1`,
		slug,
	)
	p, err := scanAssetProvider(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *Server) deleteAssetProvider(id int64) error {
	res, err := s.db.Exec(`DELETE FROM asset_providers WHERE id = $1`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// touchAssetProviderLastSeen was the v1 heartbeat hook. Health is
// now derived from plugin_modules.last_heartbeat instead (see
// pickProviderForAsset). Kept as a no-op so older callers that may
// still try it don't crash; will be removed once the column drops
// in the v2 schema migration.
func (s *Server) touchAssetProviderLastSeen(slug string) error {
	_ = slug
	return nil
}

// pickProviderForAsset picks the best provider to serve a download
// of the given asset. v1 logic: any provider with status='ready' in
// asset_locations AND a fresh heartbeat. If multiple match, prefer
// same region as a caller hint (caller passes "" if unknown).
//
// Returns:
//   - (provider, false, nil)  same-region provider found (or caller
//                             region unknown so any provider is fine)
//   - (provider, true,  nil)  cross-region fallback — caller is in a
//                             known region but no in-region provider
//                             has this asset yet; needsWarmPull tells
//                             the handler to fire-and-forget a P4c
//                             warm-pull onto an in-region target
//   - (nil,      false, nil)  no candidate at all (download 503)
//
// Health signal: plugin_modules.last_heartbeat for plugin name='assets'
// (the polar-assets-svc plugin runs every provider as a single
// process today). We do NOT use asset_providers.last_seen_at — that
// column was a P3a design artifact that no code path updates;
// removing it is a v2 migration.
//
// Future: weighting by capacity_gb, latency probes, sticky-by-IP.
func (s *Server) pickProviderForAsset(assetID int64, callerRegion string) (provider *AssetProvider, needsWarmPull bool, err error) {
	rows, err := s.db.Query(
		`SELECT `+assetProviderColumnsP+`
		   FROM asset_providers p
		   JOIN asset_locations loc ON loc.provider_id = p.id
		   JOIN plugin_modules pm ON pm.name = 'assets'
		  WHERE loc.asset_id = $1
		    AND loc.status = 'ready'
		    AND pm.enabled = true
		    AND pm.last_heartbeat IS NOT NULL
		    AND pm.last_heartbeat > NOW() - INTERVAL '5 minutes'`,
		assetID,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var candidates []AssetProvider
	for rows.Next() {
		p, err := scanAssetProvider(rows)
		if err != nil {
			return nil, false, err
		}
		candidates = append(candidates, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(candidates) == 0 {
		return nil, false, nil
	}
	if callerRegion != "" {
		for _, c := range candidates {
			if c.Region == callerRegion {
				v := c
				return &v, false, nil
			}
		}
		// Caller region known but nothing in-region holds it →
		// cross-region 302 + signal warm-pull.
		v := candidates[0]
		return &v, true, nil
	}
	v := candidates[0]
	return &v, false, nil
}

// regionOfWorkspace looks up teams.region for the given workspace,
// returning "" when the row is missing or the column is empty. Used
// by the asset blob handler to feed callerRegion into the picker.
func (s *Server) regionOfWorkspace(workspaceID string) string {
	if workspaceID == "" {
		return ""
	}
	var r sql.NullString
	_ = s.db.QueryRow(`SELECT region FROM teams WHERE id = $1`, workspaceID).Scan(&r)
	if r.Valid {
		return r.String
	}
	return ""
}

// pickHealthyProviderInRegionWithoutAsset — P4c target picker. Picks
// a healthy provider in `region` that does NOT yet have a 'ready'
// location for `assetID`. Used to choose where to warm-pull on
// cross-region cache miss.
func (s *Server) pickHealthyProviderInRegionWithoutAsset(region string, assetID int64) (*AssetProvider, error) {
	if strings.TrimSpace(region) == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT `+assetProviderColumnsP+`
		   FROM asset_providers p
		   JOIN plugin_modules pm ON pm.name = 'assets'
		  WHERE p.region = $1
		    AND pm.enabled = true
		    AND pm.last_heartbeat IS NOT NULL
		    AND pm.last_heartbeat > NOW() - INTERVAL '5 minutes'
		    AND NOT EXISTS (
		      SELECT 1 FROM asset_locations loc
		       WHERE loc.asset_id = $2
		         AND loc.provider_id = p.id
		         AND loc.status = 'ready'
		    )
		  LIMIT 1`,
		region, assetID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanAssetProvider(rows)
}

// pickAnyHealthyProvider — for the upload path where we need *a*
// provider to receive the new blob (the locations row doesn't exist
// yet). v1 picks any healthy provider, prefer same region. Same
// heartbeat-via-plugin_modules signal as pickProviderForAsset.
func (s *Server) pickAnyHealthyProvider(callerRegion string) (*AssetProvider, error) {
	rows, err := s.db.Query(
		`SELECT ` + assetProviderColumnsP + `
		   FROM asset_providers p
		   JOIN plugin_modules pm ON pm.name = 'assets'
		  WHERE pm.enabled = true
		    AND pm.last_heartbeat IS NOT NULL
		    AND pm.last_heartbeat > NOW() - INTERVAL '5 minutes'`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []AssetProvider
	for rows.Next() {
		p, err := scanAssetProvider(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if callerRegion != "" {
		for _, c := range candidates {
			if c.Region == callerRegion {
				v := c
				return &v, nil
			}
		}
	}
	v := candidates[0]
	return &v, nil
}

// markAssetLocationReady upserts the asset_locations row for the
// (asset, provider) pair to status='ready'.
func (s *Server) markAssetLocationReady(assetID, providerID int64) error {
	_, err := s.db.Exec(
		`INSERT INTO asset_locations
			(asset_id, provider_id, status, last_used_at, bytes_served)
		 VALUES ($1, $2, 'ready', NOW(), 0)
		 ON CONFLICT (asset_id, provider_id) DO UPDATE
		   SET status = 'ready', last_used_at = NOW()`,
		assetID, providerID,
	)
	return err
}

// markAssetLocationFetching upserts the row to status='fetching'.
// Used by warm-pull before calling the target provider's /v1/pull
// so the picker won't route reads to a half-populated blob.
func (s *Server) markAssetLocationFetching(assetID, providerID int64) error {
	_, err := s.db.Exec(
		`INSERT INTO asset_locations
			(asset_id, provider_id, status, last_used_at, bytes_served)
		 VALUES ($1, $2, 'fetching', NOW(), 0)
		 ON CONFLICT (asset_id, provider_id) DO UPDATE
		   SET status = 'fetching', last_used_at = NOW()`,
		assetID, providerID,
	)
	return err
}

// assetLocationStatus returns the status string of the (asset,
// provider) location row, or "" if no row exists. Used by the
// upload-grant finalize path to stay idempotent: only bump the
// workspace usage counter the first time a location flips to ready,
// so a re-sent finalize can't double-count quota.
func (s *Server) assetLocationStatus(assetID, providerID int64) (string, error) {
	var status string
	err := s.db.QueryRow(
		`SELECT status FROM asset_locations WHERE asset_id = $1 AND provider_id = $2`,
		assetID, providerID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return status, err
}

// deleteAssetLocation removes the row for (asset, provider). Used
// by warm-pull rollback when the target provider rejects the pull
// — leaves the catalog clean so a retry starts fresh.
func (s *Server) deleteAssetLocation(assetID, providerID int64) error {
	_, err := s.db.Exec(
		`DELETE FROM asset_locations WHERE asset_id = $1 AND provider_id = $2`,
		assetID, providerID,
	)
	return err
}
