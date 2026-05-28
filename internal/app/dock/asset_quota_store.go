package dock

// asset_quota_store.go — typed CRUD over asset_quotas + the
// site_settings root-quota columns. Quotas are per-workspace (cap
// in bytes) plus a global cap that lives on site_settings.
//
// Lifecycle:
//   1. First upload from a workspace lazily creates a row in
//      asset_quotas with quota_bytes = site_settings.asset_workspace_default_quota_bytes.
//   2. Every successful insert/delete in handleAsset{Upload,Delete}
//      bumps usage_bytes atomically.
//   3. A background goroutine recomputes from SUM(assets.size_bytes)
//      every 6h to catch drift.
//   4. Admin PATCH can shrink quota_bytes; if the new cap < current
//      usage, over_quota flips to true and subsequent uploads 413.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AssetQuota mirrors the asset_quotas row + the joined display name
// for the admin listing.
type AssetQuota struct {
	WorkspaceID     string     `json:"workspace_id"`
	QuotaBytes      int64      `json:"quota_bytes"`
	UsageBytes      int64      `json:"usage_bytes"`
	UsageComputedAt *time.Time `json:"usage_computed_at,omitempty"`
	OverQuota       bool       `json:"over_quota"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// AssetRootQuotaConfig is what site_settings stores for the global
// cap + the per-workspace default applied on lazy-init.
type AssetRootQuotaConfig struct {
	Mode                 string `json:"mode"`         // "absolute" | "partition_pct"
	Value                int64  `json:"value"`        // bytes if absolute; 0-100 if partition_pct
	WorkspaceDefault     int64  `json:"workspace_default_bytes"`
}

func (s *Server) assetRootQuotaConfig() (*AssetRootQuotaConfig, error) {
	var cfg AssetRootQuotaConfig
	err := s.db.QueryRow(
		`SELECT asset_root_quota_mode, asset_root_quota_value,
		        asset_workspace_default_quota_bytes
		   FROM site_settings WHERE id = 1`,
	).Scan(&cfg.Mode, &cfg.Value, &cfg.WorkspaceDefault)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// rootCapBytes resolves the configured root cap to a concrete byte
// count. For partition_pct, we'd want a `statvfs` style call to
// learn the partition size; that's deferred. For v1, partition_pct
// returns -1 (no cap enforced) so operators using that mode aren't
// blocked. Absolute mode returns Value directly.
//
// Once we add a real partition-size probe, this becomes the only
// place to change.
func (cfg *AssetRootQuotaConfig) rootCapBytes() int64 {
	if cfg == nil {
		return -1
	}
	switch cfg.Mode {
	case "absolute":
		return cfg.Value
	case "partition_pct":
		// TODO: probe partition size and return Value/100 * partitionBytes.
		// Until then, treat as no cap so operators aren't soft-broken.
		return -1
	default:
		return -1
	}
}

const assetQuotaSelectColumns = `
	workspace_id, quota_bytes, usage_bytes, usage_computed_at,
	over_quota, updated_at
`

func scanAssetQuota(r interface{ Scan(...any) error }) (*AssetQuota, error) {
	var (
		q          AssetQuota
		computedAt sql.NullTime
	)
	err := r.Scan(
		&q.WorkspaceID, &q.QuotaBytes, &q.UsageBytes,
		&computedAt, &q.OverQuota, &q.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if computedAt.Valid {
		t := computedAt.Time
		q.UsageComputedAt = &t
	}
	return &q, nil
}

// getOrCreateAssetQuota returns the quota row for workspaceID,
// inserting a fresh one (with the default from site_settings) if
// none exists. Idempotent under concurrent first-uploads via ON
// CONFLICT DO NOTHING + re-SELECT.
func (s *Server) getOrCreateAssetQuota(workspaceID string) (*AssetQuota, error) {
	if workspaceID == "" {
		return nil, errors.New("getOrCreateAssetQuota: workspaceID required")
	}
	row := s.db.QueryRow(
		`SELECT `+assetQuotaSelectColumns+`FROM asset_quotas WHERE workspace_id = $1`,
		workspaceID,
	)
	q, err := scanAssetQuota(row)
	if err == nil {
		return q, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	cfg, err := s.assetRootQuotaConfig()
	if err != nil {
		return nil, fmt.Errorf("read site_settings: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO asset_quotas (workspace_id, quota_bytes, usage_bytes, updated_at)
		 VALUES ($1, $2, 0, NOW())
		 ON CONFLICT (workspace_id) DO NOTHING`,
		workspaceID, cfg.WorkspaceDefault,
	)
	if err != nil {
		return nil, fmt.Errorf("insert default quota: %w", err)
	}
	row = s.db.QueryRow(
		`SELECT `+assetQuotaSelectColumns+`FROM asset_quotas WHERE workspace_id = $1`,
		workspaceID,
	)
	return scanAssetQuota(row)
}

// listAllAssetQuotas — admin viewer; ordered by usage_bytes DESC so
// the most-used workspaces float to the top.
func (s *Server) listAllAssetQuotas() ([]AssetQuota, error) {
	rows, err := s.db.Query(
		`SELECT ` + assetQuotaSelectColumns + `FROM asset_quotas ORDER BY usage_bytes DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AssetQuota, 0)
	for rows.Next() {
		q, err := scanAssetQuota(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *q)
	}
	return out, rows.Err()
}

// setAssetQuota changes the per-workspace cap (admin op). Returns
// the post-update row + a boolean indicating whether we tripped the
// shrink-below-usage condition (over_quota flipped to true). Caller
// handler turns that into the "must delete N GB" warning string.
func (s *Server) setAssetQuota(workspaceID string, newBytes int64) (*AssetQuota, bool, error) {
	if workspaceID == "" {
		return nil, false, errors.New("setAssetQuota: workspaceID required")
	}
	if newBytes < 0 {
		return nil, false, errors.New("setAssetQuota: quota_bytes must be >= 0")
	}
	// Ensure the row exists first so the UPDATE has something to hit.
	if _, err := s.getOrCreateAssetQuota(workspaceID); err != nil {
		return nil, false, err
	}
	// One-shot SQL that returns the resulting row + the over_quota
	// bool. We compute over_quota as `usage_bytes > new_quota` since
	// that's the meaningful invariant for the gate.
	row := s.db.QueryRow(
		`UPDATE asset_quotas
		    SET quota_bytes = $1,
		        over_quota  = (usage_bytes > $1),
		        updated_at  = NOW()
		  WHERE workspace_id = $2
		  RETURNING `+assetQuotaSelectColumns,
		newBytes, workspaceID,
	)
	q, err := scanAssetQuota(row)
	if err != nil {
		return nil, false, err
	}
	return q, q.OverQuota, nil
}

// recomputeAssetUsage reads the canonical SUM from the assets table
// and overwrites usage_bytes. Also flips over_quota based on the
// authoritative value. Called by the 6h background sweeper and by
// any handler that wants to drop drift before making a decision.
func (s *Server) recomputeAssetUsage(workspaceID string) error {
	if workspaceID == "" {
		return errors.New("recomputeAssetUsage: workspaceID required")
	}
	if _, err := s.getOrCreateAssetQuota(workspaceID); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`UPDATE asset_quotas q
		    SET usage_bytes = COALESCE(
		          (SELECT SUM(size_bytes) FROM assets WHERE workspace_id = $1), 0),
		        usage_computed_at = NOW(),
		        over_quota = (COALESCE(
		          (SELECT SUM(size_bytes) FROM assets WHERE workspace_id = $1), 0)
		          > q.quota_bytes),
		        updated_at = NOW()
		  WHERE q.workspace_id = $1`,
		workspaceID,
	)
	return err
}

// listAssetQuotaWorkspaces enumerates every workspace that has either
// (a) an asset_quotas row OR (b) any assets rows. Used by the
// recompute sweeper so we don't miss a workspace that uploaded but
// somehow lacks a quota row (shouldn't happen, but defense in depth).
func (s *Server) listAssetQuotaWorkspaces() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT workspace_id FROM asset_quotas
		UNION
		SELECT DISTINCT workspace_id FROM assets WHERE workspace_id IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// sumAllAssetUsage returns the sum of usage_bytes across all
// workspaces — i.e. the current value of the root-cap accounting.
// Cheap (single SUM query); fine to call on every upload's
// preflight.
func (s *Server) sumAllAssetUsage() (int64, error) {
	var total int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(usage_bytes), 0) FROM asset_quotas`).Scan(&total)
	return total, err
}

// bumpAssetUsage adjusts the cached usage_bytes by delta (positive
// on upload, negative on delete). Atomic UPDATE — safe under
// concurrent writers without the row getting double-bumped.
//
// On the upload path, the bump happens AFTER insertAsset succeeds so
// failed inserts don't leave phantom usage. On delete, the bump
// happens AFTER the row + blob are gone.
func (s *Server) bumpAssetUsage(workspaceID string, delta int64) error {
	if workspaceID == "" {
		return nil // platform asset; no per-workspace quota
	}
	// `GREATEST` clamps at 0 so a bug double-counting deletes can't
	// drive usage negative.
	_, err := s.db.Exec(
		`UPDATE asset_quotas
		    SET usage_bytes = GREATEST(usage_bytes + $1, 0),
		        updated_at  = NOW()
		  WHERE workspace_id = $2`,
		delta, workspaceID,
	)
	return err
}

// checkAssetUploadAllowed is the gate handlers call before letting
// an upload proceed. Returns (allowed, reasonHTTPCode, errMsg).
// reasonHTTPCode is 413 for quota issues (size, root cap, over_quota),
// 500 for any DB failure, 200 when allowed.
func (s *Server) checkAssetUploadAllowed(workspaceID string, size int64) (int, string) {
	if workspaceID == "" {
		// Platform-asset writes aren't exposed via /api/assets in v1;
		// guard for completeness.
		return 200, ""
	}
	q, err := s.getOrCreateAssetQuota(workspaceID)
	if err != nil {
		return 500, "quota lookup failed"
	}
	if q.OverQuota {
		return 413, fmt.Sprintf(
			"workspace over quota: usage %.2f GB > cap %.2f GB. delete files before uploading",
			float64(q.UsageBytes)/float64(1<<30), float64(q.QuotaBytes)/float64(1<<30))
	}
	if q.UsageBytes+size > q.QuotaBytes {
		return 413, fmt.Sprintf(
			"upload would exceed workspace quota: %.2f + %.2f GB > %.2f GB cap",
			float64(q.UsageBytes)/float64(1<<30),
			float64(size)/float64(1<<30),
			float64(q.QuotaBytes)/float64(1<<30))
	}
	cfg, err := s.assetRootQuotaConfig()
	if err != nil {
		return 500, "root quota lookup failed"
	}
	if rootCap := cfg.rootCapBytes(); rootCap > 0 {
		total, err := s.sumAllAssetUsage()
		if err != nil {
			return 500, "root usage lookup failed"
		}
		if total+size > rootCap {
			return 413, fmt.Sprintf(
				"platform out of capacity: %.2f + %.2f GB > %.2f GB root cap. contact root admin",
				float64(total)/float64(1<<30),
				float64(size)/float64(1<<30),
				float64(rootCap)/float64(1<<30))
		}
	}
	return 200, ""
}
