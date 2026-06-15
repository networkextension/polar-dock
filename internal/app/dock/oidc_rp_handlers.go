package dock

// oidc_rp_handlers.go — dock as an OIDC RELYING PARTY (federated login). The
// mirror of the OAuth provider: instead of issuing identities, dock consumes an
// external provider (zen) so a user logs in once at zen and gets a local dock
// session on any zone. Flag-gated by POLAR_OIDC_LOGIN (routes 404 when off).
//
// Identity model (decided): auto-provision + link by email — find the local user
// by the verified email; create one (no password, email_verified=true, trusting
// zen) if absent; log into an existing account if the email already exists.
//
// zen issues opaque access tokens (not signed id_tokens), so identity comes from
// the userinfo endpoint, not id_token verification. See doc/arch/oauth-provider.md.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const oidcStateTTL = 5 * time.Minute

func (s *Server) oidcLoginEnabled(c *gin.Context) bool {
	if !s.oidcLogin {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return false
	}
	return true
}

func (s *Server) oidcRedirectURI() string {
	return strings.TrimRight(s.publicBaseURL, "/") + "/auth/oidc/callback"
}

// safeReturnTo accepts only a local absolute path (prevents an open redirect via
// ?next=//evil.com or ?next=https://evil). Defaults to the dashboard.
func safeReturnTo(next string) string {
	next = strings.TrimSpace(next)
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/dashboard.html"
	}
	return next
}

// ── discovery (cached ~1h) ─────────────────────────────────────────
type oidcEndpoints struct {
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	UserInfo      string `json:"userinfo_endpoint"`
}

var (
	oidcDiscMu    sync.Mutex
	oidcDiscCache *oidcEndpoints
	oidcDiscAt    time.Time
)

func (s *Server) oidcDiscover() (*oidcEndpoints, error) {
	oidcDiscMu.Lock()
	defer oidcDiscMu.Unlock()
	if oidcDiscCache != nil && time.Since(oidcDiscAt) < time.Hour {
		return oidcDiscCache, nil
	}
	u := strings.TrimRight(s.oidcIssuer, "/") + "/.well-known/openid-configuration"
	resp, err := s.oidcHTTPClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery HTTP %d", resp.StatusCode)
	}
	var e oidcEndpoints
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e); err != nil {
		return nil, err
	}
	if e.Authorization == "" || e.Token == "" || e.UserInfo == "" {
		return nil, fmt.Errorf("discovery missing endpoints")
	}
	oidcDiscCache, oidcDiscAt = &e, time.Now()
	return &e, nil
}

// ── state store (Redis, single-use; mirrors putOAuthCode/takeOAuthCode) ──
type oidcStatePayload struct {
	Verifier string `json:"verifier"`
	ReturnTo string `json:"return_to"`
}

func (s *Server) oidcStateKey(state string) string {
	p := DefaultRedisPrefix
	if s.redisPrefix != "" {
		p = s.redisPrefix
	}
	return fmt.Sprintf("%s:oidc:state:%s", p, state)
}

func (s *Server) putOIDCState(p oidcStatePayload) (string, error) {
	state := generateSessionID()
	b, _ := json.Marshal(p)
	if err := s.redis.Set(context.Background(), s.oidcStateKey(state), b, oidcStateTTL).Err(); err != nil {
		return "", err
	}
	return state, nil
}

func (s *Server) takeOIDCState(state string) *oidcStatePayload {
	if strings.TrimSpace(state) == "" {
		return nil
	}
	b, err := s.redis.GetDel(context.Background(), s.oidcStateKey(state)).Bytes()
	if err != nil {
		return nil
	}
	var p oidcStatePayload
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// ── handlers ───────────────────────────────────────────────────────

// handleOIDCLogin starts the federated flow: stash a state+PKCE verifier and
// bounce the browser to the provider's authorize endpoint.
func (s *Server) handleOIDCLogin(c *gin.Context) {
	if !s.oidcLoginEnabled(c) {
		return
	}
	ep, err := s.oidcDiscover()
	if err != nil {
		c.String(http.StatusBadGateway, "identity provider unavailable")
		return
	}
	verifier := generateSessionID()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	returnTo := safeReturnTo(c.Query("next"))
	state, err := s.putOIDCState(oidcStatePayload{Verifier: verifier, ReturnTo: returnTo})
	if err != nil {
		c.String(http.StatusInternalServerError, "login init failed")
		return
	}
	dest := appendQuery(ep.Authorization, map[string]string{
		"client_id":             s.oidcClientID,
		"redirect_uri":          s.oidcRedirectURI(),
		"response_type":         "code",
		"scope":                 "openid email profile",
		"state":                 state,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
	})
	c.Redirect(http.StatusFound, dest)
}

// handleOIDCCallback completes the flow: verify state, exchange the code, fetch
// userinfo, find-or-create the local user, and mint the same session a password
// login would.
func (s *Server) handleOIDCCallback(c *gin.Context) {
	if !s.oidcLoginEnabled(c) {
		return
	}
	if e := c.Query("error"); e != "" {
		c.String(http.StatusBadRequest, "login failed: "+e)
		return
	}
	code := strings.TrimSpace(c.Query("code"))
	st := s.takeOIDCState(c.Query("state"))
	if code == "" || st == nil {
		c.String(http.StatusBadRequest, "invalid or expired login state")
		return
	}
	ep, err := s.oidcDiscover()
	if err != nil {
		c.String(http.StatusBadGateway, "identity provider unavailable")
		return
	}
	accessToken, err := s.exchangeOIDCCode(ep.Token, code, st.Verifier)
	if err != nil {
		c.String(http.StatusBadGateway, "token exchange failed")
		return
	}
	info, err := s.fetchOIDCUserInfo(ep.UserInfo, accessToken)
	if err != nil || strings.TrimSpace(info.Email) == "" {
		c.String(http.StatusBadGateway, "could not fetch identity")
		return
	}
	user, err := s.findOrCreateOIDCUser(info)
	if err != nil {
		c.String(http.StatusInternalServerError, "account provisioning failed")
		return
	}
	deviceType, pushToken, deviceID := s.parseClientInfo(c.GetHeader("X-Device-Type"), c.GetHeader("X-Push-Token"), c.GetHeader("X-Device-Id"))
	access, refresh, _, err := s.createTokenFamily(user, deviceType, deviceID, pushToken)
	if err != nil {
		c.String(http.StatusInternalServerError, "session failed")
		return
	}
	s.setAuthCookies(c, access, refresh)
	s.recordLoginEvent(c, user.ID, "oidc", deviceType)
	c.Redirect(http.StatusFound, st.ReturnTo)
}

func (s *Server) exchangeOIDCCode(tokenURL, code, verifier string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", s.oidcRedirectURI())
	form.Set("client_id", s.oidcClientID)
	form.Set("client_secret", s.oidcClientSecret)
	form.Set("code_verifier", verifier)
	req, _ := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.oidcHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token HTTP %d: %s", resp.StatusCode, body)
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return "", err
	}
	if t.AccessToken == "" {
		return "", fmt.Errorf("no access_token in response")
	}
	return t.AccessToken, nil
}

type oidcUserInfo struct {
	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Sub               string `json:"sub"`
}

func (s *Server) fetchOIDCUserInfo(userInfoURL, accessToken string) (*oidcUserInfo, error) {
	req, _ := http.NewRequest(http.MethodGet, userInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := s.oidcHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo HTTP %d", resp.StatusCode)
	}
	var u oidcUserInfo
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// findOrCreateOIDCUser implements auto-provision + link by email. Email is used
// as-is (dock's existing lookups are case-sensitive — consistent with password
// login).
func (s *Server) findOrCreateOIDCUser(info *oidcUserInfo) (*User, error) {
	email := strings.TrimSpace(info.Email)
	if email == "" {
		return nil, fmt.Errorf("no email in userinfo")
	}
	if u, err := s.getUserByEmail(email); err != nil {
		return nil, err
	} else if u != nil {
		return u, nil // link to the existing account
	}
	username := firstNonEmpty(strings.TrimSpace(info.PreferredUsername), strings.TrimSpace(info.Name), strings.SplitN(email, "@", 2)[0])
	nu := &User{
		ID:            generateSessionID()[:16],
		Username:      username,
		Email:         email,
		EmailVerified: true, // zen verified it
		Password:      "",   // external-login-only
		CreatedAt:     time.Now(),
	}
	if err := s.createUser(nu); err != nil {
		// lost a create race? re-fetch.
		if existing, e2 := s.getUserByEmail(email); e2 == nil && existing != nil {
			return existing, nil
		}
		return nil, err
	}
	return nu, nil
}
