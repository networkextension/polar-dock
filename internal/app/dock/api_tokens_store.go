package dock

// Open API tokens — store layer. See doc/arch/open-api-tokens.md.
// Self-service, scoped, hashed-at-rest Bearer tokens for the public /api/*
// surface. P0: full-access (scopes stored but not yet enforced — that's P1).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
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
	// The subquery locks the row and hands back the OLD hash, so the verify
	// cache entry for the old secret (keyed by hash) can be evicted — without
	// it a rotated-out secret keeps verifying for up to verifyCachePosTTL.
	var oldHash string
	row := s.db.QueryRow(
		`UPDATE api_tokens SET token_hash=$1, prefix=$2
		   FROM (SELECT id AS old_id, token_hash AS old_hash
		           FROM api_tokens WHERE id=$3 FOR UPDATE) old
		  WHERE api_tokens.id = old.old_id AND workspace_id=$4 AND status='active'
		  RETURNING `+apiTokenColsQualified+`, old.old_hash`,
		hashHex, prefix, id, workspaceID,
	)
	t, err := scanAPIToken(extraScan{row, []any{&oldHash}})
	if err != nil {
		return "", nil, err
	}
	s.verifyCacheDel("api", oldHash)
	return raw, t, nil
}

// apiTokenPatch is a partial edit of an active token (PATCH
// /api/api-tokens/:id). nil = leave unchanged. Expiry needs a separate
// ExpiresSet flag because "set to never" (ExpiresAt == nil) is a real edit.
// The secret / hash / prefix are never touched — the machines holding the
// token keep working with the new settings.
type apiTokenPatch struct {
	Name        *string
	Scopes      *[]string
	IPAllowlist *[]string
	RPM         *int
	ExpiresSet  bool
	ExpiresAt   *time.Time
}

// apply returns a copy of t with the patch applied, plus the names of the
// fields whose value actually changed (JSON field names, for the audit row).
func (p apiTokenPatch) apply(t APIToken) (APIToken, []string) {
	out := t
	changed := []string{}
	if p.Name != nil && *p.Name != t.Name {
		out.Name = *p.Name
		changed = append(changed, "name")
	}
	if p.Scopes != nil && !equalStrings(*p.Scopes, t.Scopes) {
		out.Scopes = append([]string{}, (*p.Scopes)...)
		changed = append(changed, "scopes")
	}
	if p.IPAllowlist != nil && !equalStrings(*p.IPAllowlist, t.IPAllowlist) {
		out.IPAllowlist = append([]string{}, (*p.IPAllowlist)...)
		changed = append(changed, "ip_allowlist")
	}
	if p.RPM != nil && *p.RPM != t.RateLimitRPM {
		out.RateLimitRPM = *p.RPM
		changed = append(changed, "rate_limit_rpm")
	}
	if p.ExpiresSet && !equalTimePtr(p.ExpiresAt, t.ExpiresAt) {
		out.ExpiresAt = p.ExpiresAt
		changed = append(changed, "expires_at")
	}
	return out, changed
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// getAPIToken loads one token row of the workspace (any status).
func (s *Server) getAPIToken(id int64, workspaceID string) (*APIToken, error) {
	return scanAPIToken(s.db.QueryRow(
		`SELECT `+apiTokenCols+` FROM api_tokens WHERE id=$1 AND workspace_id=$2`, id, workspaceID))
}

// updateAPIToken applies p to an active token of the workspace, in one
// transaction (row locked FOR UPDATE so the before-snapshot matches what was
// overwritten). Returns the before / after rows and the changed field names;
// sql.ErrNoRows when the token is missing, revoked or in another workspace.
// Evicts the verify-cache entry (which holds the whole row), so new scopes /
// IP allowlist / rpm / expiry apply on the very next request.
func (s *Server) updateAPIToken(id int64, workspaceID string, p apiTokenPatch) (before, after *APIToken, changed []string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var tokenHash string
	before, err = scanAPIToken(extraScan{tx.QueryRow(
		`SELECT `+apiTokenCols+`, token_hash FROM api_tokens
		  WHERE id=$1 AND workspace_id=$2 AND status='active' FOR UPDATE`,
		id, workspaceID), []any{&tokenHash}})
	if err != nil {
		return nil, nil, nil, err
	}
	next, changed := p.apply(*before)
	if len(changed) > 0 {
		scopesJSON, _ := json.Marshal(next.Scopes)
		ipJSON, _ := json.Marshal(next.IPAllowlist)
		if _, err = tx.Exec(
			`UPDATE api_tokens SET name=$1, scopes=$2, ip_allowlist=$3, rate_limit_rpm=$4, expires_at=$5
			  WHERE id=$6`,
			next.Name, scopesJSON, ipJSON, next.RateLimitRPM, next.ExpiresAt, id,
		); err != nil {
			return nil, nil, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, nil, err
	}
	if len(changed) > 0 {
		s.verifyCacheDel("api", tokenHash)
	}
	return before, &next, changed, nil
}

// apiTokenColsQualified is apiTokenCols prefixed with the table name, for
// statements that join another relation (rotate's UPDATE … FROM).
var apiTokenColsQualified = func() string {
	parts := strings.Split(apiTokenCols, ",")
	for i, p := range parts {
		parts[i] = "api_tokens." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}()

// extraScan appends extra destinations after the ones scanAPIToken passes,
// so a RETURNING list can carry columns beyond apiTokenCols.
type extraScan struct {
	rs    interface{ Scan(...any) error }
	extra []any
}

func (e extraScan) Scan(dest ...any) error {
	return e.rs.Scan(append(dest, e.extra...)...)
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
	// RETURNING token_hash so the verify-cache entry (keyed by hash) can be
	// evicted — revocation takes effect immediately instead of after the
	// positive TTL. Same shape as revokeAgentToken.
	var tokenHash string
	err := s.db.QueryRow(
		`UPDATE api_tokens SET status='revoked', revoked_at=now()
		  WHERE id=$1 AND workspace_id=$2 AND status='active'
		  RETURNING token_hash`,
		id, workspaceID,
	).Scan(&tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	s.verifyCacheDel("api", tokenHash)
	return nil
}

// apiTokenEntry is the cached positive result of an api-token lookup: the
// token row plus its owner's username / platform role, so the
// /internal/v1/auth/verify hot path needs neither api_tokens nor users from
// Postgres while the entry is warm. Owner fields are "" when the owner row is
// gone (LEFT JOIN) — /api middleware never needed them, verify rejects it.
type apiTokenEntry struct {
	Token         APIToken `json:"token"`
	OwnerUsername string   `json:"owner_username"`
	OwnerRole     string   `json:"owner_role"`
	OwnerFound    bool     `json:"owner_found"`
}

// apiTokenCacheTTL is how long a positive entry may live: the default
// verify-cache window, cut short when the token expires sooner. <= 0 means
// "do not cache" (already expired).
func apiTokenCacheTTL(expiresAt *time.Time, now time.Time) time.Duration {
	if expiresAt == nil {
		return verifyCachePosTTL
	}
	left := expiresAt.Sub(now)
	if left < verifyCachePosTTL {
		return left
	}
	return verifyCachePosTTL
}

// resolveAPIToken looks up an active, non-expired token by its raw secret.
// Returns nil when missing / revoked / expired. Used by AuthMiddleware.
func (s *Server) resolveAPIToken(raw string) *APIToken {
	e := s.resolveAPITokenEntry(raw)
	if e == nil {
		return nil
	}
	return &e.Token
}

// resolveAPITokenEntry is resolveAPIToken plus the owner snapshot, behind the
// redis verify cache (key verify:api:{sha256}, same scheme as agent tokens —
// the raw secret is never a key). Revoke / rotate evict the entry; expiry is
// re-checked on every hit; DB errors are never cached.
func (s *Server) resolveAPITokenEntry(raw string) *apiTokenEntry {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, apiTokenPrefix) {
		return nil
	}
	h := hashAPIToken(raw)
	now := time.Now()

	var cached apiTokenEntry
	if neg, hit := s.verifyCacheGet("api", h, &cached); hit {
		if neg {
			return nil
		}
		if cached.Token.ExpiresAt != nil && cached.Token.ExpiresAt.Before(now) {
			return nil
		}
		return &cached
	}

	var ownerName, ownerRole sql.NullString
	row := s.db.QueryRow(
		`SELECT `+apiTokenColsQualified+`, u.username, u.role
		   FROM api_tokens
		   LEFT JOIN users u ON u.id = api_tokens.owner_user_id
		  WHERE api_tokens.token_hash=$1 AND api_tokens.status='active'`, h)
	t, err := scanAPIToken(extraScan{row, []any{&ownerName, &ownerRole}})
	if errors.Is(err, sql.ErrNoRows) {
		s.verifyCacheSetNegative("api", h)
		return nil
	}
	if err != nil {
		return nil // transient DB error — do not cache
	}
	if t.ExpiresAt != nil && t.ExpiresAt.Before(now) {
		s.verifyCacheSetNegative("api", h)
		return nil
	}
	e := &apiTokenEntry{
		Token:         *t,
		OwnerUsername: ownerName.String,
		OwnerRole:     ownerRole.String,
		OwnerFound:    ownerName.Valid,
	}
	s.verifyCacheSetTTL("api", h, e, apiTokenCacheTTL(t.ExpiresAt, now))
	return e
}

// touchAPIToken bumps last_used_at (best-effort, async-safe).
func (s *Server) touchAPIToken(id int64) {
	_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at=now() WHERE id=$1`, id)
}

// apiTokenTouchInterval bounds last_used_at writes to one per token per
// interval — the column is a "last seen around" hint, not an audit trail
// (api_token_calls is the audit), so a write per request is wasted IO.
const apiTokenTouchInterval = 60 * time.Second

// apiTokenTouchLocal is the in-process fallback throttle (token id →
// time.Time of the last write) used when redis is absent or erroring.
var apiTokenTouchLocal sync.Map

func (s *Server) apiTokenTouchKey(id int64) string {
	prefix := DefaultRedisPrefix
	if s.redisPrefix != "" {
		prefix = s.redisPrefix
	}
	return fmt.Sprintf("%s:api_token_touch:%d", prefix, id)
}

// shouldTouchAPIToken reports whether this process should write last_used_at
// now. With redis: SET NX EX 60 across all dock replicas. Without (or on a
// redis error): a per-process sync.Map gate.
func (s *Server) shouldTouchAPIToken(id int64, now time.Time) bool {
	if s.redis != nil {
		ok, err := s.redis.SetNX(context.Background(), s.apiTokenTouchKey(id), "1", apiTokenTouchInterval).Result()
		if err == nil {
			return ok
		}
		log.Printf("api token touch: redis gate failed, falling back to local: %v", err)
	}
	if v, ok := apiTokenTouchLocal.Load(id); ok {
		if last, _ := v.(time.Time); now.Sub(last) < apiTokenTouchInterval {
			return false
		}
	}
	apiTokenTouchLocal.Store(id, now)
	return true
}

// touchAPITokenThrottled is touchAPIToken behind shouldTouchAPIToken. Call it
// from the request hot paths (AuthMiddleware, /internal/v1/auth/verify).
func (s *Server) touchAPITokenThrottled(id int64) {
	if !s.shouldTouchAPIToken(id, time.Now()) {
		return
	}
	s.touchAPIToken(id)
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
