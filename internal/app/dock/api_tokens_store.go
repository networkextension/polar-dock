package dock

// Open API tokens — store layer. See doc/arch/open-api-tokens.md.
// Self-service, scoped, hashed-at-rest Bearer tokens for the public /api/*
// surface. P0: full-access (scopes stored but not yet enforced — that's P1).

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"
)

// apiTokenPrefix identifies an open-API token in the Authorization header.
// Self-describing like Stripe's sk_live_ / Cloudflare tokens. The full secret
// is apiTokenPrefix + 32 random bytes hex; only its sha256 is persisted.
const apiTokenPrefix = "polar_"

// APIToken is one api_tokens row (never carries the secret — that's returned
// once at create time as the raw string).
type APIToken struct {
	ID           int64      `json:"id"`
	WorkspaceID  string     `json:"workspace_id"`
	OwnerUserID  string     `json:"owner_user_id"`
	Name         string     `json:"name"`
	Prefix       string     `json:"prefix"`
	Scopes       []string   `json:"scopes"`
	IPAllowlist  []string   `json:"ip_allowlist"`
	RateLimitRPM int        `json:"rate_limit_rpm"`
	Status       string     `json:"status"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
}

func generateAPIToken() (raw, hashHex, prefix string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", err
	}
	raw = apiTokenPrefix + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	// prefix shown in the UI: polar_ + first 6 chars of the random part.
	prefix = apiTokenPrefix + hex.EncodeToString(b)[:6]
	return raw, hex.EncodeToString(sum[:]), prefix, nil
}

func hashAPIToken(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

func scanAPIToken(rs interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var scopesRaw, ipRaw []byte
	if err := rs.Scan(&t.ID, &t.WorkspaceID, &t.OwnerUserID, &t.Name, &t.Prefix,
		&scopesRaw, &ipRaw, &t.RateLimitRPM, &t.Status, &t.ExpiresAt, &t.CreatedAt,
		&t.LastUsedAt, &t.RevokedAt); err != nil {
		return nil, err
	}
	if len(scopesRaw) > 0 {
		_ = json.Unmarshal(scopesRaw, &t.Scopes)
	}
	if t.Scopes == nil {
		t.Scopes = []string{}
	}
	if len(ipRaw) > 0 {
		_ = json.Unmarshal(ipRaw, &t.IPAllowlist)
	}
	if t.IPAllowlist == nil {
		t.IPAllowlist = []string{}
	}
	return &t, nil
}

const apiTokenCols = `id, workspace_id, owner_user_id, name, prefix, scopes, ip_allowlist, rate_limit_rpm, status, expires_at, created_at, last_used_at, revoked_at`

// createAPIToken mints a token in the given workspace owned by the user.
// Returns the raw secret (shown once) + the stored row.
func (s *Server) createAPIToken(workspaceID, ownerUserID, name string, scopes []string, ipAllowlist []string, rpm int, expiresAt *time.Time) (raw string, tok *APIToken, err error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(ownerUserID) == "" {
		return "", nil, errors.New("workspace and owner required")
	}
	if scopes == nil {
		scopes = []string{}
	}
	if ipAllowlist == nil {
		ipAllowlist = []string{}
	}
	if rpm <= 0 {
		rpm = 120
	}
	raw, hashHex, prefix, err := generateAPIToken()
	if err != nil {
		return "", nil, err
	}
	scopesJSON, _ := json.Marshal(scopes)
	ipJSON, _ := json.Marshal(ipAllowlist)
	var id int64
	var created time.Time
	err = s.db.QueryRow(
		`INSERT INTO api_tokens (workspace_id, owner_user_id, name, prefix, token_hash, scopes, ip_allowlist, rate_limit_rpm, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id, created_at`,
		workspaceID, ownerUserID, strings.TrimSpace(name), prefix, hashHex, scopesJSON, ipJSON, rpm, expiresAt,
	).Scan(&id, &created)
	if err != nil {
		return "", nil, err
	}
	return raw, &APIToken{
		ID: id, WorkspaceID: workspaceID, OwnerUserID: ownerUserID,
		Name: strings.TrimSpace(name), Prefix: prefix, Scopes: scopes, IPAllowlist: ipAllowlist,
		RateLimitRPM: rpm, Status: "active", ExpiresAt: expiresAt, CreatedAt: created,
	}, nil
}

// rotateAPIToken mints a fresh secret for an existing active token, keeping its
// id (so audit history + scopes + rpm + expiry + ip carry over). The old secret
// stops working immediately. Returns the new raw secret (shown once) + the row.
func (s *Server) rotateAPIToken(id int64, workspaceID string) (raw string, tok *APIToken, err error) {
	raw, hashHex, prefix, err := generateAPIToken()
	if err != nil {
		return "", nil, err
	}
	row := s.db.QueryRow(
		`UPDATE api_tokens SET token_hash=$1, prefix=$2
		  WHERE id=$3 AND workspace_id=$4 AND status='active'
		  RETURNING `+apiTokenCols,
		hashHex, prefix, id, workspaceID,
	)
	t, err := scanAPIToken(row)
	if err != nil {
		return "", nil, err
	}
	return raw, t, nil
}

// ipAllowed reports whether clientIP is permitted by an api_token's allowlist.
// Empty allowlist = unrestricted. Each entry is an exact IP or a CIDR.
func ipAllowed(clientIP string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		return false
	}
	for _, entry := range allow {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		if e := net.ParseIP(entry); e != nil && e.Equal(ip) {
			return true
		}
	}
	return false
}

// validIPEntry reports whether s is a parseable IP or CIDR (create-time check).
func validIPEntry(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.Contains(s, "/") {
		_, _, err := net.ParseCIDR(s)
		return err == nil
	}
	return net.ParseIP(s) != nil
}

// listAPITokens returns the workspace's tokens (newest first), incl. revoked
// (the UI hides them behind a toggle, same as proxy tokens).
func (s *Server) listAPITokens(workspaceID string) ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT `+apiTokenCols+` FROM api_tokens WHERE workspace_id=$1 ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]APIToken, 0)
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// revokeAPIToken soft-revokes a token, scoped to the workspace so a caller
// can't revoke another workspace's token.
func (s *Server) revokeAPIToken(id int64, workspaceID string) error {
	res, err := s.db.Exec(
		`UPDATE api_tokens SET status='revoked', revoked_at=now()
		  WHERE id=$1 AND workspace_id=$2 AND status='active'`,
		id, workspaceID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// resolveAPIToken looks up an active, non-expired token by its raw secret.
// Returns nil when missing / revoked / expired. Used by AuthMiddleware.
func (s *Server) resolveAPIToken(raw string) *APIToken {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, apiTokenPrefix) {
		return nil
	}
	row := s.db.QueryRow(`SELECT `+apiTokenCols+` FROM api_tokens WHERE token_hash=$1 AND status='active'`, hashAPIToken(raw))
	t, err := scanAPIToken(row)
	if err != nil {
		return nil
	}
	if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		return nil
	}
	return t
}

// touchAPIToken bumps last_used_at (best-effort, async-safe).
func (s *Server) touchAPIToken(id int64) {
	_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at=now() WHERE id=$1`, id)
}

// ── P2: per-call audit (api_token_calls) ──────────────────────────────────

// APITokenCall is one recorded open-API-token request (P2 audit).
type APITokenCall struct {
	ID          int64     `json:"id"`
	TokenID     int64     `json:"token_id"`
	WorkspaceID string    `json:"workspace_id"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	StatusCode  int       `json:"status_code"`
	Scope       string    `json:"scope"`
	IP          string    `json:"ip"`
	LatencyMS   int       `json:"latency_ms"`
	CreatedAt   time.Time `json:"created_at"`
}

// insertAPITokenCall records one call. Best-effort (called in a goroutine off
// the request path); a failed insert must never affect the served response.
func (s *Server) insertAPITokenCall(c *APITokenCall) {
	_, _ = s.db.Exec(
		`INSERT INTO api_token_calls
		   (token_id, workspace_id, method, path, status_code, scope, ip, latency_ms)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		c.TokenID, c.WorkspaceID, c.Method, c.Path, c.StatusCode, c.Scope, c.IP, c.LatencyMS,
	)
}

// listAPITokenCalls returns the most-recent calls for a token, newest first.
// Workspace-scoped; caller verifies the token belongs to the active workspace.
func (s *Server) listAPITokenCalls(workspaceID string, tokenID int64, limit int) ([]APITokenCall, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, token_id, workspace_id, method, path, status_code, scope, ip, latency_ms, created_at
		   FROM api_token_calls
		  WHERE workspace_id = $1 AND token_id = $2
		  ORDER BY id DESC
		  LIMIT $3`,
		workspaceID, tokenID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]APITokenCall, 0)
	for rows.Next() {
		var c APITokenCall
		if err := rows.Scan(&c.ID, &c.TokenID, &c.WorkspaceID, &c.Method, &c.Path, &c.StatusCode, &c.Scope, &c.IP, &c.LatencyMS, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// APITokenCallSummary is the rollup the usage modal shows over the trailing
// `days` days.
type APITokenCallSummary struct {
	Calls    int        `json:"calls"`
	OkCount  int        `json:"ok_count"`
	ErrCount int        `json:"err_count"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

// aggregateAPITokenCalls returns counts (total / 2xx-3xx ok / >=400 err) and
// the last-seen timestamp for a token over the trailing `days` days.
func (s *Server) aggregateAPITokenCalls(workspaceID string, tokenID int64, days int) (*APITokenCallSummary, error) {
	if days <= 0 || days > 365 {
		days = 7
	}
	var sum APITokenCallSummary
	var last sql.NullTime
	err := s.db.QueryRow(
		`SELECT COUNT(*),
		        COUNT(*) FILTER (WHERE status_code < 400),
		        COUNT(*) FILTER (WHERE status_code >= 400),
		        MAX(created_at)
		   FROM api_token_calls
		  WHERE workspace_id = $1 AND token_id = $2
		    AND created_at >= now() - ($3 || ' days')::interval`,
		workspaceID, tokenID, days,
	).Scan(&sum.Calls, &sum.OkCount, &sum.ErrCount, &last)
	if err != nil {
		return nil, err
	}
	if last.Valid {
		sum.LastUsed = &last.Time
	}
	return &sum, nil
}
