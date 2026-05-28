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
// Returns (nil, nil) when no candidate.
//
// Health signal: plugin_modules.last_heartbeat for plugin name='assets'
// (the polar-assets-svc plugin runs every provider as a single
// process today). We do NOT use asset_providers.last_seen_at — that
// column was a P3a design artifact that no code path updates;
// removing it is a v2 migration.
//
// Future: weighting by capacity_gb, latency probes, sticky-by-IP.
func (s *Server) pickProviderForAsset(assetID int64, callerRegion string) (*AssetProvider, error) {
	rows, err := s.db.Query(
		`SELECT p.`+strings.ReplaceAll(assetProviderColumns, "\n", " ")+`
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

// pickAnyHealthyProvider — for the upload path where we need *a*
// provider to receive the new blob (the locations row doesn't exist
// yet). v1 picks any healthy provider, prefer same region. Same
// heartbeat-via-plugin_modules signal as pickProviderForAsset.
func (s *Server) pickAnyHealthyProvider(callerRegion string) (*AssetProvider, error) {
	rows, err := s.db.Query(
		`SELECT p.` + strings.ReplaceAll(assetProviderColumns, "\n", " ") + `
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
