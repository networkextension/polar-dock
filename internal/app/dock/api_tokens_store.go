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
	var scopesRaw []byte
	if err := rs.Scan(&t.ID, &t.WorkspaceID, &t.OwnerUserID, &t.Name, &t.Prefix,
		&scopesRaw, &t.RateLimitRPM, &t.Status, &t.ExpiresAt, &t.CreatedAt,
		&t.LastUsedAt, &t.RevokedAt); err != nil {
		return nil, err
	}
	if len(scopesRaw) > 0 {
		_ = json.Unmarshal(scopesRaw, &t.Scopes)
	}
	if t.Scopes == nil {
		t.Scopes = []string{}
	}
	return &t, nil
}

const apiTokenCols = `id, workspace_id, owner_user_id, name, prefix, scopes, rate_limit_rpm, status, expires_at, created_at, last_used_at, revoked_at`

// createAPIToken mints a token in the given workspace owned by the user.
// Returns the raw secret (shown once) + the stored row.
func (s *Server) createAPIToken(workspaceID, ownerUserID, name string, scopes []string, rpm int, expiresAt *time.Time) (raw string, tok *APIToken, err error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(ownerUserID) == "" {
		return "", nil, errors.New("workspace and owner required")
	}
	if scopes == nil {
		scopes = []string{}
	}
	if rpm <= 0 {
		rpm = 120
	}
	raw, hashHex, prefix, err := generateAPIToken()
	if err != nil {
		return "", nil, err
	}
	scopesJSON, _ := json.Marshal(scopes)
	var id int64
	var created time.Time
	err = s.db.QueryRow(
		`INSERT INTO api_tokens (workspace_id, owner_user_id, name, prefix, token_hash, scopes, rate_limit_rpm, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id, created_at`,
		workspaceID, ownerUserID, strings.TrimSpace(name), prefix, hashHex, scopesJSON, rpm, expiresAt,
	).Scan(&id, &created)
	if err != nil {
		return "", nil, err
	}
	return raw, &APIToken{
		ID: id, WorkspaceID: workspaceID, OwnerUserID: ownerUserID,
		Name: strings.TrimSpace(name), Prefix: prefix, Scopes: scopes,
		RateLimitRPM: rpm, Status: "active", ExpiresAt: expiresAt, CreatedAt: created,
	}, nil
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
