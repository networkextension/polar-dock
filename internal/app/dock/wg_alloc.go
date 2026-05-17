package dock

// Site + device-IP allocator for /v1/register.
//
// One transaction does:
//   1. Resolve (or auto-create) the wg_sites row for this device's LAN.
//   2. Pick the lowest free d_index in [2,254] within that site.
//   3. Bind the supplied join token (mark consumed_at) to the new
//      wg_devices row.
//
// SERIALIZABLE isolation guards against two concurrent registers
// picking the same d_index; the handler retries once on serialization
// failure. Idempotent re-register with the same (pubkey, token) hits
// the early-return path and never touches the d_index allocator.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// wgRegisterInput is the parsed shape of POST /v1/register's body.
// Kept distinct from the JSON wire shape to keep allocator logic
// testable without going through gin.
type wgRegisterInput struct {
	Token        string      // plaintext as sent by client
	Pubkey       string      // wg pubkey (base64)
	Hostname     string
	OS           string
	Arch         string
	AgentVer     string
	LANAddrs     []WGLanAddr
	WGListenPort int
	// Override site_slug; when empty the allocator auto-picks based on
	// (publicIP, first LAN /24).
	SiteSlug string
	// PublicIP observed by the server (X-Forwarded-For chain or the
	// peer address). Used as part of the auto-site cache key.
	PublicIP string
}

// wgAllocateResult is what the handler turns into the /v1/register
// JSON response: the freshly-created device + the token's expires_at
// (if any) so the client knows when to refresh.
type wgAllocateResult struct {
	Device         *WGDevice
	PlaintextToken string // returned to caller; never persisted plaintext
	TokenExpiresAt *time.Time
}

// Sentinel errors mapped to HTTP status by the handler.
var (
	errWGTokenInvalid       = errors.New("token invalid")
	errWGTokenAlreadyBound  = errors.New("token already bound to another device")
	errWGPubkeyTaken        = errors.New("pubkey already registered to a different device/token")
	errWGSiteExhausted      = errors.New("site exhausted (d_index 2..254 full); manual site sharding required")
	errWGSiteSlotsExhausted = errors.New("no free s_index in 1..254; mesh is full")
)

// allocateWGDevice is the entry point. Runs steps 1–3 above. On
// serialization failure (PG 40001), retries once.
func (s *Server) allocateWGDevice(ctx context.Context, in wgRegisterInput) (*wgAllocateResult, error) {
	if err := validatePubkey(in.Pubkey); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Token) == "" {
		return nil, errWGTokenInvalid
	}
	if in.WGListenPort <= 0 || in.WGListenPort > 65535 {
		in.WGListenPort = 51820
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		res, err := s.doAllocate(ctx, in)
		if err == nil {
			return res, nil
		}
		lastErr = err
		// PG SQLSTATE 40001 = serialization_failure. Generic message
		// match since the driver-specific *pq.Error type isn't pulled
		// into this file. Cheap enough.
		if !strings.Contains(err.Error(), "could not serialize") {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, lastErr
}

func (s *Server) doAllocate(ctx context.Context, in wgRegisterInput) (*wgAllocateResult, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	tok, err := s.lookupUnusedWGToken(tx, in.Token)
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, errWGTokenInvalid
	}

	// Idempotent path: the same pubkey is re-registering with the same
	// token. Return the existing device row untouched. Catches the
	// common case where install.sh re-runs after a partial install.
	pubkey := strings.TrimSpace(in.Pubkey)
	var existing *WGDevice
	{
		row := tx.QueryRow(`SELECT `+wgDeviceColumns+` FROM wg_devices WHERE pubkey = $1`, pubkey)
		d, err := scanWGDevice(row, false)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if d != nil {
			existing = d
		}
	}
	if existing != nil {
		// Existing pubkey + same token row → idempotent re-register.
		if existing.TokenHash == hashWGToken(in.Token) && existing.RemovedAt == nil {
			// Refresh lan_addrs / hostname / agent_ver in case install
			// is being re-run after a roam — purely a UI nicety.
			lanJSON, _ := json.Marshal(in.LANAddrs)
			if _, err := tx.Exec(
				`UPDATE wg_devices
				    SET hostname = $2, os = $3, arch = $4, agent_ver = $5,
				        lan_addrs_json = $6, wg_listen_port = $7
				  WHERE id = $1`,
				existing.ID,
				strings.TrimSpace(in.Hostname), strings.TrimSpace(in.OS), strings.TrimSpace(in.Arch),
				strings.TrimSpace(in.AgentVer), nullJSONB(lanJSON), in.WGListenPort,
			); err != nil {
				return nil, err
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			// Re-read so the returned struct reflects the update.
			refreshed, err := s.getWGDeviceByID(existing.ID)
			if err != nil {
				return nil, err
			}
			return &wgAllocateResult{
				Device:         refreshed,
				PlaintextToken: in.Token,
				TokenExpiresAt: tok.ExpiresAt,
			}, nil
		}
		// Same pubkey, different token → operator probably reused a wg
		// keypair on a new device. Block — keys are supposed to be
		// per-device.
		return nil, errWGPubkeyTaken
	}

	// Token already bound to a different device → 409.
	if tok.ConsumedAt != nil {
		return nil, errWGTokenAlreadyBound
	}

	// Resolve site.
	site, err := s.resolveOrCreateWGSite(tx, in)
	if err != nil {
		return nil, err
	}

	// Pick lowest free d_index in [2,254].
	dIndex, err := pickFreeDIndex(tx, site.ID)
	if err != nil {
		return nil, err
	}

	deviceID := newWGDeviceID()
	deviceIP := fmt.Sprintf("10.88.%d.%d", site.SIndex, dIndex)
	lanJSON, _ := json.Marshal(in.LANAddrs)
	tokenHash := hashWGToken(in.Token)
	now := time.Now().UTC()

	var newID int64
	err = tx.QueryRow(
		`INSERT INTO wg_devices (
		    device_id, site_id, d_index, device_ip, pubkey, hostname, os, arch, agent_ver,
		    wg_listen_port, lan_addrs_json, token_hash, token_expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 RETURNING id`,
		deviceID, site.ID, dIndex, deviceIP, pubkey,
		strings.TrimSpace(in.Hostname), strings.TrimSpace(in.OS), strings.TrimSpace(in.Arch), strings.TrimSpace(in.AgentVer),
		in.WGListenPort, nullJSONB(lanJSON), tokenHash, tok.ExpiresAt, now,
	).Scan(&newID)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(
		`UPDATE wg_tokens SET consumed_at = $2, consumed_by_device_id = $3 WHERE id = $1`,
		tok.ID, now, newID,
	); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	dev, err := s.getWGDeviceByID(newID)
	if err != nil {
		return nil, err
	}
	dev.SiteSlug = site.Slug
	return &wgAllocateResult{
		Device:         dev,
		PlaintextToken: in.Token,
		TokenExpiresAt: tok.ExpiresAt,
	}, nil
}

// resolveOrCreateWGSite returns the wg_sites row this device belongs
// to. If in.SiteSlug is set, that wins (existing or new). Otherwise
// (publicIP, firstLANCIDR) is the cache key.
func (s *Server) resolveOrCreateWGSite(tx *sql.Tx, in wgRegisterInput) (*WGSite, error) {
	if slug := strings.TrimSpace(in.SiteSlug); slug != "" {
		if site, err := lookupSiteBySlug(tx, slug); err != nil {
			return nil, err
		} else if site != nil {
			return site, nil
		}
		return createSite(tx, slug, "", in.PublicIP, firstLANCIDR(in.LANAddrs))
	}
	// Auto: hash by (public_ip, first_lan_cidr).
	pubIP := strings.TrimSpace(in.PublicIP)
	lan := firstLANCIDR(in.LANAddrs)
	site, err := lookupSiteByAutoKey(tx, pubIP, lan)
	if err != nil {
		return nil, err
	}
	if site != nil {
		return site, nil
	}
	// Auto-slug uses next free s_index. Pre-create the slug from that
	// so the user-visible label is stable.
	return createSite(tx, "", "", pubIP, lan)
}

func firstLANCIDR(addrs []WGLanAddr) string {
	for _, a := range addrs {
		if strings.TrimSpace(a.CIDR) != "" {
			return strings.TrimSpace(a.CIDR)
		}
	}
	return ""
}

func lookupSiteBySlug(tx *sql.Tx, slug string) (*WGSite, error) {
	var x WGSite
	err := tx.QueryRow(
		`SELECT id, slug, s_index, label, public_ip, lan_cidr, created_at
		   FROM wg_sites WHERE slug = $1`,
		slug,
	).Scan(&x.ID, &x.Slug, &x.SIndex, &x.Label, &x.PublicIP, &x.LANCIDR, &x.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &x, nil
}

func lookupSiteByAutoKey(tx *sql.Tx, publicIP, lanCIDR string) (*WGSite, error) {
	var x WGSite
	err := tx.QueryRow(
		`SELECT id, slug, s_index, label, public_ip, lan_cidr, created_at
		   FROM wg_sites
		  WHERE public_ip = $1 AND lan_cidr = $2
		  ORDER BY id LIMIT 1`,
		publicIP, lanCIDR,
	).Scan(&x.ID, &x.Slug, &x.SIndex, &x.Label, &x.PublicIP, &x.LANCIDR, &x.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &x, nil
}

// createSite picks the lowest free s_index in [1,254] and inserts a
// row. If slug is empty, "site_<sIdx>" is used.
func createSite(tx *sql.Tx, slug, label, publicIP, lanCIDR string) (*WGSite, error) {
	sIdx, err := pickFreeSIndex(tx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(slug) == "" {
		slug = fmt.Sprintf("site_%d", sIdx)
	}
	var newID int64
	var createdAt time.Time
	err = tx.QueryRow(
		`INSERT INTO wg_sites (slug, s_index, label, public_ip, lan_cidr)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, created_at`,
		slug, sIdx, label, publicIP, lanCIDR,
	).Scan(&newID, &createdAt)
	if err != nil {
		return nil, err
	}
	return &WGSite{
		ID:        newID,
		Slug:      slug,
		SIndex:    sIdx,
		Label:     label,
		PublicIP:  publicIP,
		LANCIDR:   lanCIDR,
		CreatedAt: createdAt,
	}, nil
}

// pickFreeSIndex returns the lowest unused s_index in [1,254]. Locks
// nothing extra — the SERIALIZABLE outer transaction makes the read +
// later INSERT race-safe (a colliding INSERT triggers the unique
// constraint on s_index, the outer caller retries once).
func pickFreeSIndex(tx *sql.Tx) (int, error) {
	rows, err := tx.Query(`SELECT s_index FROM wg_sites ORDER BY s_index`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := make(map[int]bool, 64)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		used[v] = true
	}
	for v := 1; v <= 254; v++ {
		if !used[v] {
			return v, nil
		}
	}
	return 0, errWGSiteSlotsExhausted
}

// pickFreeDIndex returns the lowest unused d_index in [2,254] for a
// given site. .1 is reserved for hub, .255 for broadcast.
func pickFreeDIndex(tx *sql.Tx, siteID int64) (int, error) {
	rows, err := tx.Query(
		`SELECT d_index FROM wg_devices WHERE site_id = $1 AND removed_at IS NULL ORDER BY d_index`,
		siteID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := make(map[int]bool, 64)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		used[v] = true
	}
	for v := 2; v <= 254; v++ {
		if !used[v] {
			return v, nil
		}
	}
	return 0, errWGSiteExhausted
}

// extractPublicIP picks a sensible public-facing IP from the request:
// X-Forwarded-For (first hop) wins, then X-Real-IP, then the raw
// RemoteAddr. Returns the IP only (no :port). Used as part of the
// auto-site key.
func extractPublicIP(xff, xrealip, remoteAddr string) string {
	if v := strings.TrimSpace(xff); v != "" {
		if comma := strings.Index(v, ","); comma >= 0 {
			v = v[:comma]
		}
		return strings.TrimSpace(v)
	}
	if v := strings.TrimSpace(xrealip); v != "" {
		return v
	}
	if v := strings.TrimSpace(remoteAddr); v != "" {
		// Strip trailing :port. IPv6 brackets break naive rsplit; keep
		// it pedestrian since v1 is IPv4-only.
		if i := strings.LastIndex(v, ":"); i > 0 && !strings.Contains(v[i+1:], ":") {
			return v[:i]
		}
		return v
	}
	return ""
}
