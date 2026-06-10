package dock

// OAuth2 / OpenID-Connect PROVIDER (Polar as an IdP). Lets external apps
// ("Login with Polar" — e.g. Plane) authenticate users against dock via the
// authorization-code flow. Flag-gated by POLAR_OAUTH_PROVIDER; inert when off.
//
// Design (doc/arch/oauth-provider.md):
//   - oauth_clients         — registered relying parties (Postgres).
//   - authorization codes   — short-lived, single-use (Redis, 5 min).
//   - access tokens         — reuse the access-session Redis records, so the
//                             same Bearer works on /oauth/userinfo, /api/me and
//                             the whole AuthMiddleware surface. No new token type.
//
// Endpoints live in oauth_handlers.go:
//   GET  /.well-known/openid-configuration
//   GET  /oauth/authorize     POST /oauth/token     GET /oauth/userinfo
//   POST /api/oauth/clients   GET /api/oauth/clients  (admin)

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// oauthAccessTokenTTL is the lifetime of an OAuth-minted access token. Shorter
// than the first-party session (24h) since third-party apps re-auth cheaply via
// the code flow; the token is a normal access session under the hood.
const oauthAccessTokenTTL = time.Hour

// oauthCodeTTL bounds how long an authorization code is valid before exchange.
const oauthCodeTTL = 5 * time.Minute

// OAuthClient is one registered relying party. secretHash is never serialized.
type OAuthClient struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	Scopes       []string  `json:"scopes"`
	Status       string    `json:"status"`
	CreatedBy    string    `json:"created_by,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	secretHash   string    // sha256(client_secret), hex — internal only
}

// oauthCodePayload is what an authorization code resolves to. Stored in Redis
// JSON-encoded, deleted on first read (single-use).
type oauthCodePayload struct {
	ClientID            string `json:"client_id"`
	UserID              string `json:"user_id"`
	RedirectURI         string `json:"redirect_uri"`
	Scope               string `json:"scope"`
	CodeChallenge       string `json:"code_challenge,omitempty"`
	CodeChallengeMethod string `json:"code_challenge_method,omitempty"`
}

func oauthSecretHash(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// generateOAuthClientCreds mints a public client_id (oc_…) and a confidential
// secret (os_…). Only the secret's hash is persisted; the raw secret is shown
// once at creation.
func generateOAuthClientCreds() (clientID, rawSecret, secretHash string) {
	clientID = "oc_" + generateSessionID()[:24]
	rawSecret = "os_" + generateSessionID()
	return clientID, rawSecret, oauthSecretHash(rawSecret)
}

// createOAuthClient registers a relying party. redirectURIs must be non-empty
// (exact-match validated at /authorize). Returns the raw secret once.
func (s *Server) createOAuthClient(name string, redirectURIs, scopes []string, createdBy string) (rawSecret string, client *OAuthClient, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, errors.New("name required")
	}
	redirectURIs = cleanStrings(redirectURIs)
	if len(redirectURIs) == 0 {
		return "", nil, errors.New("at least one redirect_uri required")
	}
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}
	clientID, rawSecret, secretHash := generateOAuthClientCreds()
	ruJSON, _ := json.Marshal(redirectURIs)
	scJSON, _ := json.Marshal(scopes)
	var created time.Time
	err = s.db.QueryRow(
		`INSERT INTO oauth_clients (id, secret_hash, name, redirect_uris, scopes, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING created_at`,
		clientID, secretHash, name, ruJSON, scJSON, nullIfEmpty(createdBy),
	).Scan(&created)
	if err != nil {
		return "", nil, err
	}
	return rawSecret, &OAuthClient{
		ID: clientID, Name: name, RedirectURIs: redirectURIs, Scopes: scopes,
		Status: "active", CreatedBy: createdBy, CreatedAt: created, secretHash: secretHash,
	}, nil
}

const oauthClientCols = `id, secret_hash, name, redirect_uris, scopes, status, COALESCE(created_by,''), created_at`

func scanOAuthClient(rs interface{ Scan(...any) error }) (*OAuthClient, error) {
	var c OAuthClient
	var ruRaw, scRaw []byte
	if err := rs.Scan(&c.ID, &c.secretHash, &c.Name, &ruRaw, &scRaw, &c.Status, &c.CreatedBy, &c.CreatedAt); err != nil {
		return nil, err
	}
	if len(ruRaw) > 0 {
		_ = json.Unmarshal(ruRaw, &c.RedirectURIs)
	}
	if len(scRaw) > 0 {
		_ = json.Unmarshal(scRaw, &c.Scopes)
	}
	if c.RedirectURIs == nil {
		c.RedirectURIs = []string{}
	}
	if c.Scopes == nil {
		c.Scopes = []string{}
	}
	return &c, nil
}

// getOAuthClient returns an active client by id, or nil if missing/inactive.
func (s *Server) getOAuthClient(clientID string) *OAuthClient {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return nil
	}
	row := s.db.QueryRow(`SELECT `+oauthClientCols+` FROM oauth_clients WHERE id=$1 AND status='active'`, clientID)
	c, err := scanOAuthClient(row)
	if err != nil {
		return nil
	}
	return c
}

// listOAuthClients returns every client (newest first) for the admin UI. No
// secrets are exposed (secretHash is unexported).
func (s *Server) listOAuthClients() ([]OAuthClient, error) {
	rows, err := s.db.Query(`SELECT ` + oauthClientCols + ` FROM oauth_clients ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OAuthClient, 0)
	for rows.Next() {
		c, err := scanOAuthClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// validRedirectURI reports whether uri exactly matches one of the client's
// registered redirect URIs (no wildcard / prefix matching — exact, per spec).
func (c *OAuthClient) validRedirectURI(uri string) bool {
	for _, r := range c.RedirectURIs {
		if r == uri {
			return true
		}
	}
	return false
}

// ── authorization codes (Redis, single-use) ──────────────────────────────

func (s *Server) oauthCodeKey(code string) string {
	prefix := DefaultRedisPrefix
	if s.redisPrefix != "" {
		prefix = s.redisPrefix
	}
	return fmt.Sprintf("%s:oauth:code:%s", prefix, code)
}

// putOAuthCode stores an authorization code payload with a 5-minute TTL.
func (s *Server) putOAuthCode(payload oauthCodePayload) (string, error) {
	code := generateSessionID()
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if err := s.redis.Set(context.Background(), s.oauthCodeKey(code), body, oauthCodeTTL).Err(); err != nil {
		return "", err
	}
	return code, nil
}

// takeOAuthCode atomically reads and deletes a code (single-use). Returns nil
// if the code is unknown, already used, or expired.
func (s *Server) takeOAuthCode(code string) *oauthCodePayload {
	if strings.TrimSpace(code) == "" {
		return nil
	}
	body, err := s.redis.GetDel(context.Background(), s.oauthCodeKey(code)).Bytes()
	if err != nil {
		return nil
	}
	var p oauthCodePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	return &p
}

// ── access tokens (reuse the access-session machinery) ────────────────────

// mintOAuthAccessToken writes an access session for user with the granted
// scopes and returns the opaque Bearer + its lifetime in seconds. The token is
// indistinguishable from a login session, so it works on /api/me and every
// AuthMiddleware route — userinfo is just one consumer.
func (s *Server) mintOAuthAccessToken(user *User, scopes []string) (string, int, error) {
	if user == nil {
		return "", 0, errors.New("nil user")
	}
	token := generateSessionID()
	now := time.Now()
	if len(scopes) == 0 {
		scopes = []string{"openid"}
	}
	session := &Session{
		ID:         token,
		UserID:     user.ID,
		Username:   user.Username,
		Role:       user.Role,
		DeviceType: "oauth",
		Scopes:     scopes,
		ExpiresAt:  now.Add(oauthAccessTokenTTL),
	}
	body, err := json.Marshal(session)
	if err != nil {
		return "", 0, err
	}
	if err := s.redis.Set(context.Background(), s.accessKey(token), body, oauthAccessTokenTTL).Err(); err != nil {
		return "", 0, err
	}
	return token, int(oauthAccessTokenTTL.Seconds()), nil
}

// ── small helpers ─────────────────────────────────────────────────────────

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
