package dock

// wg-mac join control plane — typed CRUD for the wg_hub, wg_sites,
// wg_tokens, wg_devices, wg_bundles, wg_heartbeats tables defined in
// store.go. See doc/JOIN_PROTOCOL.md for the protocol the handlers
// in wg_handlers.go layer on top of these.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---- hub ----

type WGHub struct {
	Pubkey       string    `json:"pubkey"`
	Endpoint     string    `json:"endpoint"`
	WGIP         string    `json:"wg_ip"`
	MeshCIDR     string    `json:"mesh_cidr"`
	KeepaliveSec int       `json:"keepalive_sec"`
	RefreshSec   int       `json:"refresh_sec"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Configured reports whether the operator has set pubkey + endpoint.
// Before that, /v1/register still works (so admins can smoke-test the
// control plane) but registered devices will reference an unreachable
// hub. The UI surfaces a red warning in this state.
func (h *WGHub) Configured() bool {
	return strings.TrimSpace(h.Pubkey) != "" && strings.TrimSpace(h.Endpoint) != ""
}

func (s *Server) getWGHub() (*WGHub, error) {
	h := &WGHub{}
	err := s.db.QueryRow(
		`SELECT pubkey, endpoint, wg_ip, mesh_cidr, keepalive_sec, refresh_sec, updated_at
		   FROM wg_hub WHERE id = 1`,
	).Scan(&h.Pubkey, &h.Endpoint, &h.WGIP, &h.MeshCIDR, &h.KeepaliveSec, &h.RefreshSec, &h.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Should not happen — schema INSERTs a default row — but
			// don't panic if a hand-edit blew it away.
			return &WGHub{WGIP: "10.88.0.1", MeshCIDR: "10.88.0.0/16", KeepaliveSec: 25, RefreshSec: 300}, nil
		}
		return nil, err
	}
	return h, nil
}

func (s *Server) updateWGHub(h *WGHub, now time.Time) (*WGHub, error) {
	if h == nil {
		return nil, errors.New("nil hub")
	}
	if h.WGIP == "" {
		h.WGIP = "10.88.0.1"
	}
	if h.MeshCIDR == "" {
		h.MeshCIDR = "10.88.0.0/16"
	}
	if h.KeepaliveSec <= 0 {
		h.KeepaliveSec = 25
	}
	if h.RefreshSec <= 0 {
		h.RefreshSec = 300
	}
	_, err := s.db.Exec(
		`UPDATE wg_hub
		    SET pubkey = $1, endpoint = $2, wg_ip = $3, mesh_cidr = $4,
		        keepalive_sec = $5, refresh_sec = $6, updated_at = $7
		  WHERE id = 1`,
		strings.TrimSpace(h.Pubkey), strings.TrimSpace(h.Endpoint), h.WGIP, h.MeshCIDR,
		h.KeepaliveSec, h.RefreshSec, now,
	)
	if err != nil {
		return nil, err
	}
	return s.getWGHub()
}

// ---- sites ----

type WGSite struct {
	ID          int64     `json:"id"`
	Slug        string    `json:"slug"`
	SIndex      int       `json:"s_index"`
	Label       string    `json:"label"`
	PublicIP    string    `json:"public_ip"`
	LANCIDR     string    `json:"lan_cidr"`
	CreatedAt   time.Time `json:"created_at"`
	DeviceCount int       `json:"device_count,omitempty"`
}

func (s *Server) listWGSites() ([]WGSite, error) {
	rows, err := s.db.Query(
		`SELECT s.id, s.slug, s.s_index, s.label, s.public_ip, s.lan_cidr, s.created_at,
		        COALESCE((SELECT COUNT(*) FROM wg_devices d
		                   WHERE d.site_id = s.id AND d.removed_at IS NULL), 0)::INT
		   FROM wg_sites s
		  ORDER BY s.s_index`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGSite, 0)
	for rows.Next() {
		var x WGSite
		if err := rows.Scan(&x.ID, &x.Slug, &x.SIndex, &x.Label, &x.PublicIP, &x.LANCIDR, &x.CreatedAt, &x.DeviceCount); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---- tokens ----

type WGToken struct {
	ID                 int64      `json:"id"`
	Label              string     `json:"label"`
	Prefix             string     `json:"token_prefix"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
	ConsumedAt         *time.Time `json:"consumed_at,omitempty"`
	ConsumedByDeviceID *int64     `json:"consumed_by_device_id,omitempty"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	CreatedByUserID    string     `json:"created_by_user_id,omitempty"`
	// Plaintext is set ONLY on create. Never read back from DB.
	Plaintext string `json:"plaintext,omitempty"`
}

const wgTokenPlaintextPrefix = "polar_wg_"

func newWGTokenPlaintext() (plaintext, prefix string) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	plaintext = wgTokenPlaintextPrefix + hex.EncodeToString(b)
	prefix = plaintext[:len(wgTokenPlaintextPrefix)+8]
	return
}

func hashWGToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func (s *Server) createWGToken(label, createdByUserID string, expiresAt *time.Time, now time.Time) (*WGToken, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, errors.New("label required")
	}
	plaintext, prefix := newWGTokenPlaintext()
	hash := hashWGToken(plaintext)
	out := &WGToken{
		Label:           label,
		Prefix:          prefix,
		ExpiresAt:       expiresAt,
		CreatedAt:       now,
		CreatedByUserID: createdByUserID,
		Plaintext:       plaintext,
	}
	err := s.db.QueryRow(
		`INSERT INTO wg_tokens (label, token_hash, token_prefix, expires_at, created_at, created_by_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		label, hash, prefix, expiresAt, now, createdByUserID,
	).Scan(&out.ID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) listWGTokens() ([]WGToken, error) {
	rows, err := s.db.Query(
		`SELECT id, label, token_prefix, expires_at, consumed_at, consumed_by_device_id, revoked_at, created_at, created_by_user_id
		   FROM wg_tokens
		  ORDER BY created_at DESC, id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGToken, 0)
	for rows.Next() {
		var t WGToken
		var expiresAt, consumedAt, revokedAt sql.NullTime
		var consumedBy sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Label, &t.Prefix, &expiresAt, &consumedAt, &consumedBy, &revokedAt, &t.CreatedAt, &t.CreatedByUserID); err != nil {
			return nil, err
		}
		if expiresAt.Valid {
			v := expiresAt.Time
			t.ExpiresAt = &v
		}
		if consumedAt.Valid {
			v := consumedAt.Time
			t.ConsumedAt = &v
		}
		if consumedBy.Valid {
			v := consumedBy.Int64
			t.ConsumedByDeviceID = &v
		}
		if revokedAt.Valid {
			v := revokedAt.Time
			t.RevokedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Server) revokeWGToken(id int64, now time.Time) error {
	res, err := s.db.Exec(
		`UPDATE wg_tokens
		    SET revoked_at = COALESCE(revoked_at, $2)
		  WHERE id = $1`,
		id, now,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// lookupUnusedWGToken returns the row matching plaintext IFF the token
// is not yet consumed, not revoked, and not expired. The caller must
// still bind the token to a device within the same transaction (via
// allocateWGDevice).
func (s *Server) lookupUnusedWGToken(tx *sql.Tx, plaintext string) (*WGToken, error) {
	hash := hashWGToken(plaintext)
	row := tx.QueryRow(
		`SELECT id, label, token_prefix, expires_at, consumed_at, consumed_by_device_id, revoked_at, created_at, created_by_user_id
		   FROM wg_tokens
		  WHERE token_hash = $1`,
		hash,
	)
	var t WGToken
	var expiresAt, consumedAt, revokedAt sql.NullTime
	var consumedBy sql.NullInt64
	if err := row.Scan(&t.ID, &t.Label, &t.Prefix, &expiresAt, &consumedAt, &consumedBy, &revokedAt, &t.CreatedAt, &t.CreatedByUserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if expiresAt.Valid {
		v := expiresAt.Time
		t.ExpiresAt = &v
		if time.Now().UTC().After(v) {
			return nil, errWGTokenExpired
		}
	}
	if revokedAt.Valid {
		return nil, errWGTokenRevoked
	}
	if consumedAt.Valid {
		v := consumedAt.Time
		t.ConsumedAt = &v
	}
	if consumedBy.Valid {
		v := consumedBy.Int64
		t.ConsumedByDeviceID = &v
	}
	return &t, nil
}

var (
	errWGTokenExpired = errors.New("wg token expired")
	errWGTokenRevoked = errors.New("wg token revoked")
)

// ---- devices ----

// WGLanAddr matches the JSON the client sends for each LAN interface
// it has bound a wg listen port to.
type WGLanAddr struct {
	Iface string `json:"iface"`
	CIDR  string `json:"cidr"`
}

type WGDevice struct {
	ID               int64       `json:"id"`
	DeviceID         string      `json:"device_id"`
	SiteID           int64       `json:"site_id"`
	DIndex           int         `json:"d_index"`
	DeviceIP         string      `json:"device_ip"`
	Pubkey           string      `json:"pubkey"`
	Hostname         string      `json:"hostname"`
	OS               string      `json:"os"`
	Arch             string      `json:"arch"`
	AgentVer         string      `json:"agent_ver"`
	WGListenPort     int         `json:"wg_listen_port"`
	LANAddrs         []WGLanAddr `json:"lan_addrs,omitempty"`
	WGEndpoint       string      `json:"wg_endpoint"`
	TokenHash        string      `json:"-"`
	TokenExpiresAt   *time.Time  `json:"token_expires_at,omitempty"`
	CreatedAt        time.Time   `json:"created_at"`
	LastSeenAt       *time.Time  `json:"last_seen_at,omitempty"`
	RemovedAt        *time.Time  `json:"removed_at,omitempty"`
	SiteSlug         string      `json:"site_slug,omitempty"` // populated by joined list queries
}

func scanWGDevice(scanner interface {
	Scan(dest ...any) error
}, withSiteSlug bool) (*WGDevice, error) {
	var d WGDevice
	var lanJSON sql.NullString
	var tokenExpiresAt, lastSeenAt, removedAt sql.NullTime
	var siteSlug sql.NullString
	dest := []any{
		&d.ID, &d.DeviceID, &d.SiteID, &d.DIndex, &d.DeviceIP, &d.Pubkey, &d.Hostname, &d.OS, &d.Arch,
		&d.AgentVer, &d.WGListenPort, &lanJSON, &d.WGEndpoint, &d.TokenHash, &tokenExpiresAt,
		&d.CreatedAt, &lastSeenAt, &removedAt,
	}
	if withSiteSlug {
		dest = append(dest, &siteSlug)
	}
	if err := scanner.Scan(dest...); err != nil {
		return nil, err
	}
	if lanJSON.Valid && lanJSON.String != "" {
		_ = json.Unmarshal([]byte(lanJSON.String), &d.LANAddrs)
	}
	if tokenExpiresAt.Valid {
		v := tokenExpiresAt.Time
		d.TokenExpiresAt = &v
	}
	if lastSeenAt.Valid {
		v := lastSeenAt.Time
		d.LastSeenAt = &v
	}
	if removedAt.Valid {
		v := removedAt.Time
		d.RemovedAt = &v
	}
	if withSiteSlug && siteSlug.Valid {
		d.SiteSlug = siteSlug.String
	}
	return &d, nil
}

const wgDeviceColumns = `id, device_id, site_id, d_index, device_ip, pubkey, hostname, os, arch, agent_ver, wg_listen_port, lan_addrs_json, wg_endpoint, token_hash, token_expires_at, created_at, last_seen_at, removed_at`

func (s *Server) getWGDeviceByID(id int64) (*WGDevice, error) {
	row := s.db.QueryRow(`SELECT `+wgDeviceColumns+` FROM wg_devices WHERE id = $1`, id)
	d, err := scanWGDevice(row, false)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (s *Server) getWGDeviceByDeviceID(deviceID string) (*WGDevice, error) {
	row := s.db.QueryRow(`SELECT `+wgDeviceColumns+` FROM wg_devices WHERE device_id = $1`, deviceID)
	d, err := scanWGDevice(row, false)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

// getWGDeviceByTokenHash is the auth side of /v1/peers, /v1/heartbeat,
// /v1/leave: hash the bearer, look up the device. Skipped removed.
func (s *Server) getWGDeviceByTokenHash(hash string) (*WGDevice, error) {
	row := s.db.QueryRow(
		`SELECT `+wgDeviceColumns+`
		   FROM wg_devices
		  WHERE token_hash = $1 AND removed_at IS NULL`,
		hash,
	)
	d, err := scanWGDevice(row, false)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (s *Server) listWGDevices(includeRemoved bool) ([]WGDevice, error) {
	q := `SELECT d.id, d.device_id, d.site_id, d.d_index, d.device_ip, d.pubkey, d.hostname, d.os, d.arch,
	             d.agent_ver, d.wg_listen_port, d.lan_addrs_json, d.wg_endpoint, d.token_hash, d.token_expires_at,
	             d.created_at, d.last_seen_at, d.removed_at, s.slug
	        FROM wg_devices d
	        JOIN wg_sites s ON s.id = d.site_id`
	if !includeRemoved {
		q += ` WHERE d.removed_at IS NULL`
	}
	q += ` ORDER BY d.site_id, d.d_index`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGDevice, 0)
	for rows.Next() {
		d, err := scanWGDevice(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// listWGDevicesInSite returns un-removed peers in the given site,
// excluding the one with excludeDeviceID. Used by the /v1/peers
// peer-list builder.
func (s *Server) listWGDevicesInSite(siteID int64, excludeID int64) ([]WGDevice, error) {
	rows, err := s.db.Query(
		`SELECT `+wgDeviceColumns+`
		   FROM wg_devices
		  WHERE site_id = $1 AND removed_at IS NULL AND id <> $2
		  ORDER BY d_index`,
		siteID, excludeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGDevice, 0)
	for rows.Next() {
		d, err := scanWGDevice(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// markWGDeviceRemoved is the /v1/leave + admin force-remove path. The
// row stays in the table — bg cleanup is a separate Phase.
func (s *Server) markWGDeviceRemoved(id int64, now time.Time) error {
	res, err := s.db.Exec(
		`UPDATE wg_devices
		    SET removed_at = COALESCE(removed_at, $2)
		  WHERE id = $1`,
		id, now,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// updateWGDeviceTokenHash rotates a device's bearer token in place.
// /v1/token/refresh path. Old hash is replaced atomically.
func (s *Server) updateWGDeviceTokenHash(id int64, newHash string, expiresAt *time.Time) error {
	_, err := s.db.Exec(
		`UPDATE wg_devices
		    SET token_hash = $2, token_expires_at = $3
		  WHERE id = $1 AND removed_at IS NULL`,
		id, newHash, expiresAt,
	)
	return err
}

// otherSiteIndices returns distinct s_index for sites OTHER than the
// given one. Used to compute hub peer's allowed_extra: every other
// site's /24 has to be routable through hub.
func (s *Server) otherSiteIndices(excludeSiteID int64) ([]int, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT s_index FROM wg_sites WHERE id <> $1 ORDER BY s_index`,
		excludeSiteID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]int, 0)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- bundles ----

type WGBundle struct {
	ID              int64     `json:"id"`
	Version         string    `json:"version"`
	BlobURI         string    `json:"blob_uri"`
	BlobSHA256      string    `json:"blob_sha256"`
	SizeBytes       int64     `json:"size_bytes"`
	IsLatest        bool      `json:"is_latest"`
	Notes           string    `json:"notes"`
	AddedAt         time.Time `json:"added_at"`
	AddedByUserID   string    `json:"added_by_user_id"`
}

func (s *Server) listWGBundles() ([]WGBundle, error) {
	rows, err := s.db.Query(
		`SELECT id, version, blob_uri, blob_sha256, size_bytes, is_latest, notes, added_at, added_by_user_id
		   FROM wg_bundles
		  ORDER BY added_at DESC, id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WGBundle, 0)
	for rows.Next() {
		var b WGBundle
		if err := rows.Scan(&b.ID, &b.Version, &b.BlobURI, &b.BlobSHA256, &b.SizeBytes, &b.IsLatest, &b.Notes, &b.AddedAt, &b.AddedByUserID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Server) getWGBundleByID(id int64) (*WGBundle, error) {
	var b WGBundle
	err := s.db.QueryRow(
		`SELECT id, version, blob_uri, blob_sha256, size_bytes, is_latest, notes, added_at, added_by_user_id
		   FROM wg_bundles WHERE id = $1`,
		id,
	).Scan(&b.ID, &b.Version, &b.BlobURI, &b.BlobSHA256, &b.SizeBytes, &b.IsLatest, &b.Notes, &b.AddedAt, &b.AddedByUserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

func (s *Server) getWGBundleByVersion(version string) (*WGBundle, error) {
	var b WGBundle
	err := s.db.QueryRow(
		`SELECT id, version, blob_uri, blob_sha256, size_bytes, is_latest, notes, added_at, added_by_user_id
		   FROM wg_bundles WHERE version = $1`,
		version,
	).Scan(&b.ID, &b.Version, &b.BlobURI, &b.BlobSHA256, &b.SizeBytes, &b.IsLatest, &b.Notes, &b.AddedAt, &b.AddedByUserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

func (s *Server) getWGBundleBySHA(sha string) (*WGBundle, error) {
	var b WGBundle
	err := s.db.QueryRow(
		`SELECT id, version, blob_uri, blob_sha256, size_bytes, is_latest, notes, added_at, added_by_user_id
		   FROM wg_bundles WHERE blob_sha256 = $1`,
		sha,
	).Scan(&b.ID, &b.Version, &b.BlobURI, &b.BlobSHA256, &b.SizeBytes, &b.IsLatest, &b.Notes, &b.AddedAt, &b.AddedByUserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

func (s *Server) getLatestWGBundle() (*WGBundle, error) {
	var b WGBundle
	err := s.db.QueryRow(
		`SELECT id, version, blob_uri, blob_sha256, size_bytes, is_latest, notes, added_at, added_by_user_id
		   FROM wg_bundles WHERE is_latest = TRUE LIMIT 1`,
	).Scan(&b.ID, &b.Version, &b.BlobURI, &b.BlobSHA256, &b.SizeBytes, &b.IsLatest, &b.Notes, &b.AddedAt, &b.AddedByUserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

func (s *Server) insertWGBundle(b *WGBundle) (*WGBundle, error) {
	if b == nil {
		return nil, errors.New("nil bundle")
	}
	if strings.TrimSpace(b.Version) == "" || strings.TrimSpace(b.BlobSHA256) == "" {
		return nil, errors.New("version and blob_sha256 required")
	}
	err := s.db.QueryRow(
		`INSERT INTO wg_bundles (version, blob_uri, blob_sha256, size_bytes, is_latest, notes, added_at, added_by_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, added_at`,
		b.Version, b.BlobURI, b.BlobSHA256, b.SizeBytes, b.IsLatest, b.Notes, time.Now().UTC(), b.AddedByUserID,
	).Scan(&b.ID, &b.AddedAt)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// setWGBundleLatest is atomic: clears any existing is_latest row, then
// marks the given id. Wrapped in a txn so the partial unique index
// (ux_wg_bundles_latest) can never see two latest rows.
func (s *Server) setWGBundleLatest(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE wg_bundles SET is_latest = FALSE WHERE is_latest = TRUE`); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE wg_bundles SET is_latest = TRUE WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Server) deleteWGBundle(id int64) error {
	res, err := s.db.Exec(`DELETE FROM wg_bundles WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ---- heartbeats ----

type WGHeartbeatRecord struct {
	DeviceID         int64
	RXBytes          *int64
	TXBytes          *int64
	LastHandshakeSec *int64
	WGEndpoint       string
	LANAddrs         []WGLanAddr
	ReceivedAt       time.Time
}

// insertWGHeartbeat appends the row + mirrors the latest values onto
// wg_devices so /api/admin/wg-devices can render "last seen" without a
// per-row aggregate. Returns the count of heartbeats appended (always
// 1 on success; 0 if device gone).
func (s *Server) insertWGHeartbeat(rec *WGHeartbeatRecord, now time.Time) error {
	if rec == nil {
		return errors.New("nil heartbeat")
	}
	rec.ReceivedAt = now
	var lanJSON []byte
	if len(rec.LANAddrs) > 0 {
		lanJSON, _ = json.Marshal(rec.LANAddrs)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO wg_heartbeats (device_id, received_at, rx_bytes, tx_bytes, last_handshake_sec, wg_endpoint, lan_addrs_json)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		rec.DeviceID, now,
		nullInt64(rec.RXBytes), nullInt64(rec.TXBytes), nullInt64(rec.LastHandshakeSec),
		rec.WGEndpoint, nullJSONB(lanJSON),
	); err != nil {
		return err
	}
	// Mirror the latest endpoint/addrs onto the device row so admin
	// list views don't have to LEFT-JOIN-LATERAL on heartbeats.
	if _, err := tx.Exec(
		`UPDATE wg_devices
		    SET last_seen_at = $2,
		        wg_endpoint = CASE WHEN $3 = '' THEN wg_endpoint ELSE $3 END,
		        lan_addrs_json = COALESCE($4, lan_addrs_json)
		  WHERE id = $1`,
		rec.DeviceID, now, rec.WGEndpoint, nullJSONB(lanJSON),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullJSONB(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// newWGDeviceID returns a random opaque device id ("dev_" + 16 hex).
// Used at /v1/register to mint the device_id the client carries on
// subsequent calls.
func newWGDeviceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "dev_" + hex.EncodeToString(b)
}

// validatePubkey is a minimal sanity check on the base64-encoded wg
// public key the client posts. wg_core itself does the cryptographic
// validation; this just rejects empty/oversized payloads so the DB
// doesn't get junk.
func validatePubkey(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return errors.New("pubkey required")
	}
	if len(p) > 128 {
		return fmt.Errorf("pubkey too long (%d > 128)", len(p))
	}
	return nil
}
