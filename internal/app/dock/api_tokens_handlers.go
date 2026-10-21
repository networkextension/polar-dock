package dock

// Open API tokens — HTTP handlers (self-service CRUD). All under
// AuthMiddleware: a logged-in user manages tokens for their active workspace.
// See doc/arch/open-api-tokens.md.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gin-auth-app/internal/apiscopes"
)

// handleAPITokenCreate — POST /api/api-tokens
// Body: {name, scopes?[], rate_limit_rpm?, expires_at?}. Returns the raw
// secret ONCE (never retrievable again) + the stored row.
func (s *Server) handleAPITokenCreate(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	ws := strings.TrimSpace(c.GetString("workspace_id"))
	if ws == "" {
		jsonError(c, http.StatusBadRequest, "auth.no_workspace")
		return
	}
	var req struct {
		Name         string     `json:"name"`
		Scopes       []string   `json:"scopes"`
		IPAllowlist  []string   `json:"ip_allowlist"`
		RateLimitRPM int        `json:"rate_limit_rpm"`
		ExpiresAt    *time.Time `json:"expires_at"`
	}
	_ = c.ShouldBindJSON(&req)
	if strings.TrimSpace(req.Name) == "" {
		req.Name = "api-token"
	}
	// Validate any requested scopes against the known set so a typo'd scope
	// can't silently lock a token out of everything. Empty list = full access.
	for _, sc := range req.Scopes {
		if !knownScope(strings.TrimSpace(sc)) {
			jsonError(c, http.StatusBadRequest, "api.unknown_scope")
			return
		}
	}
	// Validate IP allowlist entries (exact IP or CIDR) so a bad entry can't
	// silently lock the token out of every source.
	for _, ip := range req.IPAllowlist {
		if !validIPEntry(ip) {
			jsonError(c, http.StatusBadRequest, "api.invalid_ip")
			return
		}
	}
	if !s.apiTokenAdminGate(c, userID, req.Scopes) {
		return
	}
	raw, tok, err := s.createAPIToken(ws, userID, req.Name, req.Scopes, req.IPAllowlist, req.RateLimitRPM, req.ExpiresAt)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server.error")
		return
	}
	s.logTeamAudit(ws, userID, "api_token.create", "api_token", strconv.FormatInt(tok.ID, 10),
		"签发 API Token", map[string]any{"token": apiTokenAuditView(tok), "prefix": tok.Prefix})
	c.JSON(http.StatusCreated, gin.H{
		"token":  tok,
		"secret": raw, // shown once — client must save it now
		"hint":   "复制 secret 立即保存；服务端只存哈希，事后无法再展示",
	})
}

// handleAPITokenList — GET /api/api-tokens (active workspace, newest first).
func (s *Server) handleAPITokenList(c *gin.Context) {
	if _, ok := requireUserID(c); !ok {
		return
	}
	ws := strings.TrimSpace(c.GetString("workspace_id"))
	if ws == "" {
		c.JSON(http.StatusOK, gin.H{"tokens": []APIToken{}})
		return
	}
	toks, err := s.listAPITokens(ws)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server.error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": toks})
}

// handleAPITokenScopes — GET /api/api-token-scopes. Returns the grantable
// scope catalog (grouped by domain) that the create-token UI renders as a
// picker. Derived from the embedded API catalog so it always matches what the
// middleware enforces.
func (s *Server) handleAPITokenScopes(c *gin.Context) {
	if _, ok := requireUserID(c); !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"groups":   apiScopeCatalog(),
		"wildcard": "*",
		// Scopes declared by registered module manifests (module_manifest.go).
		"modules": buildModuleScopeCatalog(manifestRegistry.modules(), s.pluginDisplayName),
	})
}

// handleAPITokenCalls — GET /api/api-tokens/:id/calls?limit=N. Recent calls
// for one token, workspace-scoped (the query filters on workspace_id so a
// caller can't read another workspace's token usage).
func (s *Server) handleAPITokenCalls(c *gin.Context) {
	if _, ok := requireUserID(c); !ok {
		return
	}
	ws := strings.TrimSpace(c.GetString("workspace_id"))
	id, perr := strconv.ParseInt(c.Param("id"), 10, 64)
	if perr != nil || ws == "" {
		jsonError(c, http.StatusBadRequest, "input.invalid")
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	calls, err := s.listAPITokenCalls(ws, id, limit)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server.error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"calls": calls})
}

// handleAPITokenCallsAggregate — GET /api/api-tokens/:id/calls/aggregate?days=N.
// Total / ok / error counts + last-seen over the window.
func (s *Server) handleAPITokenCallsAggregate(c *gin.Context) {
	if _, ok := requireUserID(c); !ok {
		return
	}
	ws := strings.TrimSpace(c.GetString("workspace_id"))
	id, perr := strconv.ParseInt(c.Param("id"), 10, 64)
	if perr != nil || ws == "" {
		jsonError(c, http.StatusBadRequest, "input.invalid")
		return
	}
	days, _ := strconv.Atoi(c.Query("days"))
	sum, err := s.aggregateAPITokenCalls(ws, id, days)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server.error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": sum})
}

// handleAPITokenRotate — POST /api/api-tokens/:id/rotate. Issues a new secret
// for the same token row (id/scopes/rpm/expiry/ip preserved); the old secret
// stops working immediately. Returns the new secret once, like create.
func (s *Server) handleAPITokenRotate(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	ws := strings.TrimSpace(c.GetString("workspace_id"))
	id, perr := strconv.ParseInt(c.Param("id"), 10, 64)
	if perr != nil || ws == "" {
		jsonError(c, http.StatusBadRequest, "input.invalid")
		return
	}
	raw, tok, err := s.rotateAPIToken(id, ws)
	if err != nil {
		jsonError(c, http.StatusNotFound, "not_found")
		return
	}
	s.logTeamAudit(ws, userID, "api_token.rotate", "api_token", strconv.FormatInt(id, 10),
		"轮换 API Token secret", map[string]any{"name": tok.Name, "prefix": tok.Prefix})
	c.JSON(http.StatusOK, gin.H{
		"token":  tok,
		"secret": raw,
		"hint":   "已生成新 secret，旧 secret 立即失效；复制保存，事后无法再展示",
	})
}

// handleAPITokenRevoke — DELETE /api/api-tokens/:id (scoped to workspace).
func (s *Server) handleAPITokenRevoke(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	ws := strings.TrimSpace(c.GetString("workspace_id"))
	id, perr := strconv.ParseInt(c.Param("id"), 10, 64)
	if perr != nil || ws == "" {
		jsonError(c, http.StatusBadRequest, "input.invalid")
		return
	}
	if err := s.revokeAPIToken(id, ws); err != nil {
		jsonError(c, http.StatusNotFound, "not_found")
		return
	}
	s.logTeamAudit(ws, userID, "api_token.revoke", "api_token", strconv.FormatInt(id, 10),
		"吊销 API Token", nil)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// apiTokenMaxRPM caps rate_limit_rpm on edit (create still falls back to the
// 120 default for <= 0).
const apiTokenMaxRPM = 100000

// handleAPITokenUpdate — PATCH /api/api-tokens/:id. Edits an active token in
// place without touching its secret: body is any subset of
// {name, scopes[], ip_allowlist[], rate_limit_rpm, expires_at}; an absent field
// is left unchanged, "expires_at": null means never expires. Only a browser
// session may edit (an API token can't widen itself), only the token owner or
// a workspace owner/admin, and admin:* scopes need a site admin. Returns
// {token} (no secret). The verify cache is evicted so changes apply at once.
func (s *Server) handleAPITokenUpdate(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	if c.GetString("auth_kind") == "api_token" {
		jsonError(c, http.StatusForbidden, "api.session_required")
		return
	}
	ws := strings.TrimSpace(c.GetString("workspace_id"))
	id, perr := strconv.ParseInt(c.Param("id"), 10, 64)
	if perr != nil || ws == "" {
		jsonError(c, http.StatusBadRequest, "input.invalid")
		return
	}
	var req struct {
		Name         *string         `json:"name"`
		Scopes       *[]string       `json:"scopes"`
		IPAllowlist  *[]string       `json:"ip_allowlist"`
		RateLimitRPM *int            `json:"rate_limit_rpm"`
		ExpiresAt    json.RawMessage `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "input.invalid")
		return
	}
	cur, err := s.getAPIToken(id, ws)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && cur.Status != "active") {
		jsonError(c, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server.error")
		return
	}
	if !apiTokenCanEdit(userID, cur.OwnerUserID, workspaceRole(c)) {
		jsonError(c, http.StatusForbidden, "forbidden")
		return
	}
	// Validated against the stored scopes: a scope the token already holds may
	// stay even if the catalog no longer knows it (a module dropped it — the UI
	// sends such scopes back unchanged as read-only chips); only newly added
	// scopes must be known.
	patch, errKey := validateAPITokenPatch(req.Name, req.Scopes, req.IPAllowlist, req.RateLimitRPM, req.ExpiresAt, cur.Scopes, time.Now())
	if errKey != "" {
		jsonError(c, http.StatusBadRequest, errKey)
		return
	}
	// Only newly granted scopes go through the admin gate: a workspace admin
	// may rename a token a site admin already gave admin:* without tripping it.
	if patch.Scopes != nil && !s.apiTokenAdminGate(c, userID, addedScopes(cur.Scopes, *patch.Scopes)) {
		return
	}
	before, after, changed, err := s.updateAPIToken(id, ws, patch)
	if errors.Is(err, sql.ErrNoRows) {
		jsonError(c, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server.error")
		return
	}
	if len(changed) > 0 {
		s.logTeamAudit(ws, userID, "api_token.update", "api_token", strconv.FormatInt(id, 10),
			"编辑 API Token", map[string]any{
				"before":         apiTokenAuditView(before),
				"after":          apiTokenAuditView(after),
				"changed_fields": changed,
			})
	}
	c.JSON(http.StatusOK, gin.H{"token": after})
}

// validateAPITokenPatch turns the PATCH body into an apiTokenPatch, or returns
// the error key. Pure (existing scopes + now injected) so it's table-testable.
// existing = the token's stored scopes: those may be kept even when no longer
// in the catalog; any other scope must pass knownScope. expiresRaw: nil /
// empty = absent (unchanged), JSON null = never expires, else an RFC3339
// string that must be in the future.
func validateAPITokenPatch(name *string, scopes, ips *[]string, rpm *int, expiresRaw json.RawMessage, existing []string, now time.Time) (apiTokenPatch, string) {
	var p apiTokenPatch
	if name != nil {
		n := strings.TrimSpace(*name)
		if n == "" {
			return p, "input.invalid"
		}
		p.Name = &n
	}
	if scopes != nil {
		out := make([]string, 0, len(*scopes))
		for _, sc := range *scopes {
			sc = strings.TrimSpace(sc)
			if !knownScope(sc) && !containsString(existing, sc) {
				return p, "api.unknown_scope"
			}
			out = append(out, sc)
		}
		p.Scopes = &out
	}
	if ips != nil {
		out := make([]string, 0, len(*ips))
		for _, ip := range *ips {
			if !validIPEntry(ip) {
				return p, "api.invalid_ip"
			}
			out = append(out, strings.TrimSpace(ip))
		}
		p.IPAllowlist = &out
	}
	if rpm != nil {
		if *rpm <= 0 || *rpm > apiTokenMaxRPM {
			return p, "input.invalid"
		}
		v := *rpm
		p.RPM = &v
	}
	if raw := bytes.TrimSpace(expiresRaw); len(raw) > 0 {
		p.ExpiresSet = true
		if !bytes.Equal(raw, []byte("null")) {
			var t time.Time
			if err := json.Unmarshal(raw, &t); err != nil {
				return p, "input.invalid"
			}
			if !t.After(now) {
				return p, "input.invalid"
			}
			p.ExpiresAt = &t
		}
	}
	if p.Name == nil && p.Scopes == nil && p.IPAllowlist == nil && p.RPM == nil && !p.ExpiresSet {
		return p, "input.invalid"
	}
	return p, ""
}

// apiTokenCanEdit: the token's owner, or a workspace owner/admin.
func apiTokenCanEdit(callerID, ownerID, workspaceRole string) bool {
	if callerID != "" && callerID == ownerID {
		return true
	}
	return teamRoleAllows(workspaceRole, TeamRoleAdmin)
}

func containsString(list []string, v string) bool {
	if v == "" {
		return false
	}
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// addedScopes returns the scopes in next that cur doesn't already hold.
func addedScopes(cur, next []string) []string {
	have := make(map[string]bool, len(cur))
	for _, s := range cur {
		have[s] = true
	}
	var out []string
	for _, s := range next {
		if !have[s] {
			out = append(out, s)
		}
	}
	return out
}

// apiTokenGateDecision is the pure core of apiTokenAdminGate: "" = allowed,
// else the 403 error key. isSiteAdmin is only consulted when an admin scope
// is requested.
func apiTokenGateDecision(authKind string, scopes []string, isSiteAdmin func() (bool, error)) string {
	if authKind == "api_token" {
		return "api.session_required"
	}
	for _, sc := range scopes {
		if apiscopes.IsAdminScope(strings.TrimSpace(sc)) {
			ok, err := isSiteAdmin()
			if err != nil || !ok {
				return "api.admin_scope_forbidden"
			}
			return ""
		}
	}
	return ""
}

// apiTokenAdminGate — shared by create and update: tokens are issued / edited
// from a browser session only (an api_token caller can't mint or widen
// tokens), and admin:* scopes can only be granted by a site admin. Writes the
// 403 and returns false when denied.
func (s *Server) apiTokenAdminGate(c *gin.Context, userID string, scopes []string) bool {
	key := apiTokenGateDecision(c.GetString("auth_kind"), scopes, func() (bool, error) {
		ok, err := s.IsSiteAdmin(userID)
		if err != nil {
			log.Printf("api tokens: IsSiteAdmin(%s) failed: %v — denying admin scope", userID, err)
		}
		return ok, err
	})
	if key != "" {
		jsonError(c, http.StatusForbidden, key)
		return false
	}
	return true
}

// apiTokenAuditView is the token as recorded in team-audit details: the
// editable settings only — never the hash or secret.
func apiTokenAuditView(t *APIToken) map[string]any {
	if t == nil {
		return nil
	}
	return map[string]any{
		"name":           t.Name,
		"scopes":         t.Scopes,
		"ip_allowlist":   t.IPAllowlist,
		"rate_limit_rpm": t.RateLimitRPM,
		"expires_at":     t.ExpiresAt,
	}
}
