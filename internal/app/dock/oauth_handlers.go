package dock

// OAuth2 / OIDC provider HTTP surface. See oauth_provider.go for the store
// layer and doc/arch/oauth-provider.md for the design. Every handler is inert
// (404) unless POLAR_OAUTH_PROVIDER=true.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// oauthEnabled gates the whole surface. Returns false + writes 404 when the
// provider flag is off, so the feature leaves no trace by default.
func (s *Server) oauthEnabled(c *gin.Context) bool {
	if !s.oauthProvider {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return false
	}
	return true
}

// oauthError writes an RFC 6749 §5.2 error body.
func oauthError(c *gin.Context, status int, code, desc string) {
	c.Header("Cache-Control", "no-store")
	c.JSON(status, gin.H{"error": code, "error_description": desc})
}

// issuer resolves the OIDC issuer/base URL: the configured public base URL, or
// the request's scheme+host as a fallback (dev / unset deploys).
func (s *Server) issuer(c *gin.Context) string {
	if s.publicBaseURL != "" {
		return s.publicBaseURL
	}
	scheme := "https"
	if c.Request.TLS == nil && c.GetHeader("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if p := c.GetHeader("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + c.Request.Host
}

// GET /.well-known/openid-configuration — discovery document so generic OIDC
// clients can self-configure (Plane lets you set endpoints manually too).
func (s *Server) handleOAuthDiscovery(c *gin.Context) {
	if !s.oauthEnabled(c) {
		return
	}
	iss := s.issuer(c)
	c.JSON(http.StatusOK, gin.H{
		"issuer":                                iss,
		"authorization_endpoint":                iss + "/oauth/authorize",
		"token_endpoint":                        iss + "/oauth/token",
		"userinfo_endpoint":                     iss + "/oauth/userinfo",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
		"subject_types_supported":               []string{"public"},
		"claims_supported":                      []string{"sub", "email", "email_verified", "name", "preferred_username", "picture"},
	})
}

// GET /oauth/authorize — authorization-code request. Requires a logged-in
// session (cookie or Bearer); if absent, bounces to /login and comes back.
func (s *Server) handleOAuthAuthorize(c *gin.Context) {
	if !s.oauthEnabled(c) {
		return
	}
	q := c.Request.URL.Query()
	responseType := q.Get("response_type")
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	scope := strings.TrimSpace(q.Get("scope"))
	state := q.Get("state")

	client := s.getOAuthClient(clientID)
	if client == nil {
		oauthError(c, http.StatusBadRequest, "invalid_client", "unknown or inactive client_id")
		return
	}
	// Per spec, an invalid redirect_uri must NOT be redirected to — render the
	// error directly so an attacker can't bounce codes to an unregistered URI.
	if !client.validRedirectURI(redirectURI) {
		oauthError(c, http.StatusBadRequest, "invalid_request", "redirect_uri not registered for this client")
		return
	}
	if responseType != "code" {
		s.oauthRedirectErr(c, redirectURI, state, "unsupported_response_type", "only response_type=code is supported")
		return
	}

	// Require an authenticated user. Reuse the standard token extraction so a
	// browser cookie OR an Authorization: Bearer both work.
	token := extractAccessToken(c)
	session := s.getAccessSession(token)
	if session == nil {
		next := c.Request.URL.RequestURI()
		c.Redirect(http.StatusFound, "/login?next="+url.QueryEscape(next))
		return
	}

	// Resolve granted scope: requested ∩ client-allowed; default to the
	// client's full scope set when the request omits scope.
	granted := intersectScopes(scope, client.Scopes)

	code, err := s.putOAuthCode(oauthCodePayload{
		ClientID:            client.ID,
		UserID:              session.UserID,
		RedirectURI:         redirectURI,
		Scope:               strings.Join(granted, " "),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
	})
	if err != nil {
		s.oauthRedirectErr(c, redirectURI, state, "server_error", "failed to issue code")
		return
	}

	dest := appendQuery(redirectURI, map[string]string{"code": code, "state": state})
	c.Redirect(http.StatusFound, dest)
}

// POST /oauth/token — exchange an authorization code for an access token.
func (s *Server) handleOAuthToken(c *gin.Context) {
	if !s.oauthEnabled(c) {
		return
	}
	grantType := c.PostForm("grant_type")
	if grantType != "authorization_code" {
		oauthError(c, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}

	clientID, clientSecret := oauthClientCreds(c)
	client := s.getOAuthClient(clientID)
	if client == nil {
		oauthError(c, http.StatusUnauthorized, "invalid_client", "unknown client")
		return
	}

	code := c.PostForm("code")
	redirectURI := c.PostForm("redirect_uri")
	payload := s.takeOAuthCode(code)
	if payload == nil || payload.ClientID != client.ID || payload.RedirectURI != redirectURI {
		oauthError(c, http.StatusBadRequest, "invalid_grant", "code invalid, expired, or mismatched")
		return
	}

	// Authenticate the client: confidential secret, OR PKCE if a challenge was
	// presented at /authorize (public clients).
	if payload.CodeChallenge != "" {
		if !verifyPKCE(payload.CodeChallenge, payload.CodeChallengeMethod, c.PostForm("code_verifier")) {
			oauthError(c, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
	} else {
		if subtle.ConstantTimeCompare([]byte(oauthSecretHash(clientSecret)), []byte(client.secretHash)) != 1 {
			oauthError(c, http.StatusUnauthorized, "invalid_client", "bad client secret")
			return
		}
	}

	user, err := s.getUserByID(payload.UserID)
	if err != nil || user == nil {
		oauthError(c, http.StatusBadRequest, "invalid_grant", "user no longer exists")
		return
	}
	scopes := strings.Fields(payload.Scope)
	access, expiresIn, err := s.mintOAuthAccessToken(user, scopes)
	if err != nil {
		oauthError(c, http.StatusInternalServerError, "server_error", "failed to mint token")
		return
	}

	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.JSON(http.StatusOK, gin.H{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   expiresIn,
		"scope":        payload.Scope,
	})
}

// GET /oauth/userinfo — OIDC userinfo. Bearer access token → user claims.
// Plane reads `email` from here for account linking.
func (s *Server) handleOAuthUserinfo(c *gin.Context) {
	if !s.oauthEnabled(c) {
		return
	}
	token := extractBearerToken(c)
	session := s.getAccessSession(token)
	if session == nil {
		c.Header("WWW-Authenticate", `Bearer error="invalid_token"`)
		oauthError(c, http.StatusUnauthorized, "invalid_token", "missing or invalid access token")
		return
	}
	user, err := s.getUserByID(session.UserID)
	if err != nil || user == nil {
		oauthError(c, http.StatusUnauthorized, "invalid_token", "user not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"sub":                user.ID,
		"email":              user.Email,
		"email_verified":     user.EmailVerified,
		"name":               user.Username,
		"preferred_username": user.Username,
		"picture":            user.IconURL,
	})
}

// ── admin: client registration ────────────────────────────────────────────

type oauthClientCreateReq struct {
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
	Scopes       []string `json:"scopes"`
}

// POST /api/oauth/clients (admin) — register a relying party. Returns the
// client_secret ONCE.
func (s *Server) handleOAuthClientCreate(c *gin.Context) {
	if !s.oauthEnabled(c) {
		return
	}
	var req oauthClientCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "common.bad_request")
		return
	}
	userID, _ := c.Get("user_id")
	uid, _ := userID.(string)
	secret, client, err := s.createOAuthClient(req.Name, req.RedirectURIs, req.Scopes, uid)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"client_id":     client.ID,
		"client_secret": secret, // shown once
		"name":          client.Name,
		"redirect_uris": client.RedirectURIs,
		"scopes":        client.Scopes,
		"created_at":    client.CreatedAt,
	})
}

// GET /api/oauth/clients (admin) — list registered clients (no secrets).
func (s *Server) handleOAuthClientList(c *gin.Context) {
	if !s.oauthEnabled(c) {
		return
	}
	clients, err := s.listOAuthClients()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"clients": clients})
}

// ── helpers ────────────────────────────────────────────────────────────────

// oauthClientCreds pulls client_id/secret from HTTP Basic auth first, then form
// body (client_secret_basic / client_secret_post — both advertised in discovery).
func oauthClientCreds(c *gin.Context) (id, secret string) {
	if u, p, ok := c.Request.BasicAuth(); ok {
		return u, p
	}
	return c.PostForm("client_id"), c.PostForm("client_secret")
}

// intersectScopes returns the requested scopes that the client is allowed to
// hold. Empty request → the client's full allowed set.
func intersectScopes(requested string, allowed []string) []string {
	if strings.TrimSpace(requested) == "" {
		return allowed
	}
	allow := map[string]bool{}
	for _, a := range allowed {
		allow[a] = true
	}
	out := make([]string, 0)
	for _, r := range strings.Fields(requested) {
		if allow[r] {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		// Nothing requested overlapped; still grant openid so the flow works.
		out = []string{"openid"}
	}
	return out
}

// verifyPKCE checks a code_verifier against the stored challenge (S256 or plain).
func verifyPKCE(challenge, method, verifier string) bool {
	if verifier == "" {
		return false
	}
	switch strings.ToUpper(method) {
	case "", "S256":
		sum := sha256.Sum256([]byte(verifier))
		computed := base64.RawURLEncoding.EncodeToString(sum[:])
		return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
	case "PLAIN":
		return subtle.ConstantTimeCompare([]byte(verifier), []byte(challenge)) == 1
	default:
		return false
	}
}

// appendQuery adds non-empty key/values to a URL, preserving any existing query.
func appendQuery(base string, params map[string]string) string {
	u, err := url.Parse(base)
	if err != nil {
		// Fall back to naive concatenation; should not happen (validated earlier).
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		parts := make([]string, 0)
		for k, v := range params {
			if v != "" {
				parts = append(parts, k+"="+url.QueryEscape(v))
			}
		}
		return base + sep + strings.Join(parts, "&")
	}
	qq := u.Query()
	for k, v := range params {
		if v != "" {
			qq.Set(k, v)
		}
	}
	u.RawQuery = qq.Encode()
	return u.String()
}

// oauthRedirectErr bounces an OAuth error back to a (validated) redirect_uri.
func (s *Server) oauthRedirectErr(c *gin.Context, redirectURI, state, code, desc string) {
	dest := appendQuery(redirectURI, map[string]string{"error": code, "error_description": desc, "state": state})
	c.Redirect(http.StatusFound, dest)
}
