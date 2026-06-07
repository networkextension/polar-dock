package dock

// Open API tokens — HTTP handlers (self-service CRUD). All under
// AuthMiddleware: a logged-in user manages tokens for their active workspace.
// See doc/arch/open-api-tokens.md.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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
	raw, tok, err := s.createAPIToken(ws, userID, req.Name, req.Scopes, req.RateLimitRPM, req.ExpiresAt)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server.error")
		return
	}
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
	c.JSON(http.StatusOK, gin.H{"groups": apiScopeCatalog(), "wildcard": "*"})
}

// handleAPITokenRevoke — DELETE /api/api-tokens/:id (scoped to workspace).
func (s *Server) handleAPITokenRevoke(c *gin.Context) {
	if _, ok := requireUserID(c); !ok {
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
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
