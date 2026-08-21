package dock

// internal_api_handlers.go — Phase 1-A shipped the /ping smoke route.
// Phase 1-B adds the 8 documented v1 business endpoints. All are gated
// by hmacPluginAuth so c.Get("plugin_name") is always set.
//
// See doc/arch/internal-api-v1.md for request/response shapes.

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleInternalPing(c *gin.Context) {
	plugin, _ := c.Get("plugin_name")
	pluginStr, _ := plugin.(string)
	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"plugin":      pluginStr,
		"server_time": time.Now().UTC().Format(time.RFC3339),
	})
}

// internalErr is a thin helper for consistent error JSON across the
// v1 surface. Keeps plugins parsing a single shape: {"error": "..."}.
func internalErr(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": msg})
}

// ── 1. GET /internal/v1/auth/verify?token=<session-token> ──────────────────
// Validates an end-user session token (whatever the plugin received in
// Authorization: Bearer from its caller). Returns user_id + role +
// active workspace + expiry so the plugin can locally cache for 30s.

func (s *Server) handleInternalAuthVerify(c *gin.Context) {
	token := strings.TrimSpace(c.Query("token"))
	if token == "" {
		internalErr(c, http.StatusBadRequest, "token query required")
		return
	}
	sess := s.getAccessSession(token)
	if sess == nil {
		// Also accept a polar_ open-API token, so plugin services that verify
		// via this endpoint (zone-svc, release, …) accept api keys, not just
		// browser sessions. The token acts as its OWNER: we return the owner's
		// real role (so an admin-owned token clears admin-gated plugin
		// endpoints) + the token's workspace. Scope limits are enforced by
		// dock's own /api middleware, not here — a plugin-svc admin gate only
		// checks role, so use full-scope tokens for plugin ops.
		if s.openAPITokens {
			if tok := s.resolveAPIToken(token); tok != nil && s.writeAPITokenVerify(c, tok) {
				return
			}
		}
		internalErr(c, http.StatusUnauthorized, "invalid session")
		return
	}
	// Resolve workspace_id. Default = the user's personal workspace, but a
	// plugin may request the caller's ACTIVE workspace by forwarding
	// ?workspace_id=<id> (sourced from the browser's X-Workspace-Id header).
	// Honor it only when the user is actually a member of that team — so a
	// module-subdomain page shows data scoped to the selected workspace,
	// not just the personal one. Falls back to personal on non-membership.
	workspaceID, _ := s.personalWorkspaceForUser(sess.UserID)
	if req := strings.TrimSpace(c.Query("workspace_id")); req != "" && req != workspaceID {
		if m, err := s.getTeamMember(req, sess.UserID); err == nil && m != nil {
			workspaceID = req
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"user_id":      sess.UserID,
		"username":     sess.Username,
		"role":         sess.Role,
		"workspace_id": workspaceID,
		"expires_at":   sess.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// ── 1b. GET /internal/v1/auth/verify-agent?token=<polar_agent_…> ───────────
// Verify an AGENT bearer token on behalf of a plugin. Agent tokens were
// previously verifiable only inside dock (resolveAgentToken hits agent_tokens
// directly), which forced any agent-authenticated surface — e.g. the
// compute-task claim API — to live in dock. This endpoint + the SDK's
// AuthVerifyAgent lift that restriction: a plugin can authenticate an agent
// without being inside dock. Backed by the same redis verify-cache, so the
// hot path is a redis GET, not a Postgres query.
func (s *Server) handleInternalAuthVerifyAgent(c *gin.Context) {
	token := strings.TrimSpace(c.Query("token"))
	if token == "" {
		internalErr(c, http.StatusBadRequest, "token query required")
		return
	}
	tokenID, userID, err := s.resolveAgentToken(token)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "agent token lookup failed")
		return
	}
	if tokenID == "" {
		internalErr(c, http.StatusUnauthorized, "invalid agent token")
		return
	}
	// Default workspace = the token owner's personal workspace; an agent acts
	// as its owner. (Per-task workspace targeting is the caller's concern.)
	workspaceID, _ := s.personalWorkspaceForUser(userID)
	c.JSON(http.StatusOK, gin.H{
		"token_id":     tokenID,
		"user_id":      userID,
		"workspace_id": workspaceID,
	})
}

// writeAPITokenVerify answers /internal/v1/auth/verify for a polar_ open-API
// token: the token acts as its owner (real role from the users table), scoped
// to the token's workspace unless the caller forwards a ?workspace_id the owner
// is a member of. expires_at mirrors the token's expiry (or a short cache TTL
// when the token never expires). Returns false if the owner can't be resolved.
func (s *Server) writeAPITokenVerify(c *gin.Context, tok *APIToken) bool {
	u, err := s.getUserByID(tok.OwnerUserID)
	if err != nil || u == nil {
		return false
	}
	workspaceID := tok.WorkspaceID
	if req := strings.TrimSpace(c.Query("workspace_id")); req != "" && req != workspaceID {
		if m, err := s.getTeamMember(req, tok.OwnerUserID); err == nil && m != nil {
			workspaceID = req
		}
	}
	exp := time.Now().Add(30 * time.Second) // matches plugin-side AuthVerify cache
	if tok.ExpiresAt != nil && tok.ExpiresAt.Before(exp) {
		exp = *tok.ExpiresAt
	}
	go s.touchAPIToken(tok.ID)
	c.JSON(http.StatusOK, gin.H{
		"user_id":      u.ID,
		"username":     u.Username,
		"role":         u.Role,
		"workspace_id": workspaceID,
		"expires_at":   exp.UTC().Format(time.RFC3339),
	})
	return true
}

// ── 2. GET /internal/v1/users/:id ──────────────────────────────────────────
// Public profile snapshot. Strips email, password hash, sessions, etc.

func (s *Server) handleInternalUserGet(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		internalErr(c, http.StatusBadRequest, "id required")
		return
	}
	u, err := s.getUserByID(id)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "user lookup failed")
		return
	}
	if u == nil {
		internalErr(c, http.StatusNotFound, "user not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":         u.ID,
		"username":   u.Username,
		"icon_url":   u.IconURL,
		"bio":        u.Bio,
		"is_bot":     u.Role == "bot",
		"created_at": u.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// ── 3. GET /internal/v1/users/:id/workspace ────────────────────────────────
// Returns the user's personal team (Polar- guarantees one exists per user).

func (s *Server) handleInternalUserWorkspace(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		internalErr(c, http.StatusBadRequest, "id required")
		return
	}
	wid, err := s.personalWorkspaceForUser(id)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "workspace lookup failed")
		return
	}
	if wid == "" {
		internalErr(c, http.StatusNotFound, "user not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"workspace_id": wid})
}

// ── 4. GET /internal/v1/teams/:id ──────────────────────────────────────────

func (s *Server) handleInternalTeamGet(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		internalErr(c, http.StatusBadRequest, "id required")
		return
	}
	t, err := s.getTeam(id)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "team lookup failed")
		return
	}
	if t == nil {
		internalErr(c, http.StatusNotFound, "team not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":            t.ID,
		"slug":          t.Slug,
		"name":          t.Name,
		"owner_user_id": t.OwnerUserID,
		"created_at":    t.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// ── 5. GET /internal/v1/teams/:id/members/:user ────────────────────────────

func (s *Server) handleInternalTeamMember(c *gin.Context) {
	tid := strings.TrimSpace(c.Param("id"))
	uid := strings.TrimSpace(c.Param("user"))
	if tid == "" || uid == "" {
		internalErr(c, http.StatusBadRequest, "id and user required")
		return
	}
	m, err := s.getTeamMember(tid, uid)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "membership lookup failed")
		return
	}
	if m == nil {
		internalErr(c, http.StatusNotFound, "not a member")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"team_id":   m.TeamID,
		"user_id":   m.UserID,
		"role":      m.Role,
		"joined_at": m.JoinedAt.UTC().Format(time.RFC3339),
	})
}

// ── 6. GET /internal/v1/llm-configs/:id?workspace_id=<wid> ─────────────────
// Returns resolved cfg + api_key plaintext. Workspace scoping enforced
// by getAvailableLLMConfigWithAPIKey (same gate the in-tree handlers use).

func (s *Server) handleInternalLLMConfigGet(c *gin.Context) {
	idStr := strings.TrimSpace(c.Param("id"))
	wid := strings.TrimSpace(c.Query("workspace_id"))
	if idStr == "" || wid == "" {
		internalErr(c, http.StatusBadRequest, "id path + workspace_id query required")
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		internalErr(c, http.StatusBadRequest, "invalid id")
		return
	}
	cfg, apiKey, err := s.getAvailableLLMConfigWithAPIKey(wid, id)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "llm config lookup failed")
		return
	}
	if cfg == nil {
		// Store helper collapses "not found" and "not accessible to
		// workspace" both to (nil, "", nil). Pick the friendlier 403
		// response since exposing "not found" leaks existence to
		// unrelated workspaces.
		internalErr(c, http.StatusForbidden, "llm config not accessible to workspace")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":        cfg.ID,
		"name":      cfg.Name,
		"base_url":  cfg.BaseURL,
		"model":     cfg.Model,
		"api_key":   apiKey,
		"proxy_url": cfg.ProxyURL,
		"is_system": cfg.IsSystem,
		"is_shared": cfg.Shared,
	})
}

// ── 7. POST /internal/v1/llm/chat-completion ───────────────────────────────
// Convenience: plugin sends {workspace_id, llm_config_id, model_override?,
// endpoint_tag, messages, max_tokens}; Dock runs the same gate + call +
// ledger pipeline used by chat. Plugin doesn't hold any LLM credentials.

type internalLLMChatReq struct {
	WorkspaceID   string                    `json:"workspace_id" binding:"required"`
	LLMConfigID   int64                     `json:"llm_config_id" binding:"required"`
	ModelOverride string                    `json:"model_override"`
	EndpointTag   string                    `json:"endpoint_tag"`
	Messages      []aiChatCompletionMessage `json:"messages" binding:"required"`
	MaxTokens     int                       `json:"max_tokens"`
}

func (s *Server) handleInternalLLMChatCompletion(c *gin.Context) {
	var req internalLLMChatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	if len(req.Messages) == 0 {
		internalErr(c, http.StatusBadRequest, "messages required")
		return
	}
	if req.MaxTokens <= 0 || req.MaxTokens > 4096 {
		req.MaxTokens = 1024
	}
	if req.EndpointTag == "" {
		plugin, _ := c.Get("plugin_name")
		pluginStr, _ := plugin.(string)
		req.EndpointTag = "plugin." + pluginStr
	}

	if blocked, msg := s.checkChatBillingGate(req.WorkspaceID, req.LLMConfigID); blocked {
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error": msg,
			"kind":  "billing_blocked",
		})
		return
	}

	cfg, apiKey, err := s.getAvailableLLMConfigWithAPIKey(req.WorkspaceID, req.LLMConfigID)
	if err != nil || cfg == nil || strings.TrimSpace(apiKey) == "" {
		internalErr(c, http.StatusForbidden, "llm config not accessible to workspace")
		return
	}

	modelToUse := cfg.Model
	if strings.TrimSpace(req.ModelOverride) != "" {
		modelToUse = strings.TrimSpace(req.ModelOverride)
	}

	runtimeCfg := aiRuntimeConfig{
		APIKey:        apiKey,
		BaseURL:       cfg.BaseURL,
		Model:         modelToUse,
		Streaming:     false,
		ProxyURL:      cfg.ProxyURL,
		LLMConfigID:   cfg.ID,
		LLMConfigName: cfg.Name,
		WorkspaceID:   req.WorkspaceID,
		Endpoint:      req.EndpointTag,
	}

	start := time.Now()
	resp, err := s.aiAgent.requestChatCompletion(runtimeCfg, aiChatCompletionRequest{
		Model:     modelToUse,
		Messages:  req.Messages,
		MaxTokens: req.MaxTokens,
	})
	latencyMS := int(time.Since(start).Milliseconds())

	// Cost ledger (success or fail).
	prompt, completion, total := 0, 0, 0
	status := http.StatusOK
	errText := ""
	if resp != nil && resp.Usage != nil {
		prompt = resp.Usage.PromptTokens
		completion = resp.Usage.CompletionTokens
		total = resp.Usage.TotalTokens
	}
	if err != nil {
		status = http.StatusInternalServerError
		errText = truncateForLog(err.Error(), 200)
	}
	if recErr := s.recordAgentLLMCall(
		req.WorkspaceID, cfg.ID, modelToUse, modelToUse,
		prompt, completion, total, latencyMS, status, errText, nil,
	); recErr != nil {
		// log only — never fail the plugin call because the ledger
		// write hiccuped.
	}

	if err != nil {
		kind, _ := classifyBotError(err.Error(), "plugin")
		c.JSON(http.StatusBadGateway, gin.H{
			"error": err.Error(),
			"kind":  string(kind),
		})
		return
	}
	if resp == nil || len(resp.Choices) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "empty upstream response"})
		return
	}
	cost, _ := s.estimateLLMProxyCallCostWithConfig(cfg.ID, modelToUse, prompt, completion)
	c.JSON(http.StatusOK, gin.H{
		"content":           resp.Choices[0].Message.Content,
		"model":             modelToUse,
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
		"latency_ms":        latencyMS,
		"cost_usd":          cost,
	})
}

// ── 8. POST /internal/v1/notify ── see internal_notify.go ──────────────────

// ── 9. POST /internal/v1/plugin-registry/heartbeat ─────────────────────────
// Plugin announces it is alive. Updates last_heartbeat + version on
// the plugin_modules row. Plugin name must match the auth header
// (gates against an attacker with one key impersonating another
// plugin in the registry).

type internalHeartbeatReq struct {
	Name          string `json:"name" binding:"required"`
	Version       string `json:"version"`
	Endpoint      string `json:"endpoint"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	MetricsURL    string `json:"metrics_url"`
	// UIRoutes: optional; raw JSON array of sidebar nav entries the
	// plugin offers. See plugin_modules.ui_routes schema. Empty /
	// missing = preserve current value in the column (don't blank
	// existing routes on plugins that haven't yet been updated).
	UIRoutes json.RawMessage `json:"ui_routes,omitempty"`
	// APIRoutes: optional; raw JSON array of the plugin's API surface
	// (the marketplace "manifest" — see plugin_modules.api_routes / R3).
	// Same preserve-on-empty semantics as UIRoutes. Element:
	// {"method":"GET","path":"/api/expense/items","label":"...","scope":"read"}.
	APIRoutes json.RawMessage `json:"api_routes,omitempty"`
	// OS/Arch: the platform this instance runs on (runtime.GOOS/GOARCH).
	// Used to resolve an OTA update directive matching a prebuilt binary
	// (Track 3). Empty = module doesn't participate in self-update.
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
	// PublicBaseURL: the plugin's externally-reachable origin (e.g.
	// "https://hosts.4950.store:2443"); joined with ui_routes paths to
	// build absolute links in the shared platform nav (GET /api/nav).
	// Preserve-on-empty, same as UIRoutes.
	PublicBaseURL string `json:"public_base_url,omitempty"`
}

// ── F1 additions (Phase 2 handlers prep) ─────────────────────────
//
// These 5 endpoints unblock cross-domain calls from the 6 remaining
// plugin migrations (packtunnel / library / video / projects / hosts
// / iosdist). Same hmacPluginAuth gate as the rest of /internal/v1.

// ── 10. GET /internal/v1/bot-users/:id ─────────────────────────────
// id is the bot's user_id (the "u_..." string). Used by projects +
// hosts handler migrations for bot metadata + LLM binding.

func (s *Server) handleInternalBotUserGet(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		internalErr(c, http.StatusBadRequest, "id required")
		return
	}
	b, err := s.getBotUserByUserID(id)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "bot lookup failed")
		return
	}
	if b == nil {
		internalErr(c, http.StatusNotFound, "bot not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":             b.ID,
		"bot_user_id":    b.BotUserID,
		"workspace_id":   b.WorkspaceID,
		"owner_user_id":  b.OwnerUserID,
		"name":           b.Name,
		"description":    b.Description,
		"llm_config_id":  b.LLMConfigID,
		"bot_kind":       b.BotKind,
		"preferred_tool": b.PreferredTool,
		"host_skill_id":  b.HostSkillID,
		"model_override": b.ModelOverride,
	})
}

// ── 11. GET /internal/v1/chat-threads/:id ───────────────────────────
// LastMessage is truncated to 200 chars at the gateway to keep this
// endpoint small; full chat history goes through the dock-side
// /api/chat/* surface as the end-user.

func (s *Server) handleInternalChatThreadGet(c *gin.Context) {
	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		internalErr(c, http.StatusBadRequest, "invalid id")
		return
	}
	t, err := s.getChatThread(id)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "thread lookup failed")
		return
	}
	if t == nil {
		internalErr(c, http.StatusNotFound, "thread not found")
		return
	}
	excerpt := t.LastMessage
	if len(excerpt) > 200 {
		excerpt = excerpt[:200] + "…"
	}
	out := gin.H{
		"id":           t.ID,
		"user_low":     t.UserLow,
		"user_high":    t.UserHigh,
		"last_message": excerpt,
		"created_at":   t.CreatedAt.UTC().Format(time.RFC3339),
	}
	if t.LastMessageAt != nil {
		out["last_message_at"] = t.LastMessageAt.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, out)
}

// ── 12. GET /internal/v1/agent-presence/:bot_id ─────────────────────
// Returns the live agentHub attach state for one bot user. Plugins
// (projects in particular) use this to decide whether to dispatch
// an agent task vs. surface "agent offline" in the UI.

func (s *Server) handleInternalAgentPresence(c *gin.Context) {
	botID := strings.TrimSpace(c.Param("bot_id"))
	if botID == "" {
		internalErr(c, http.StatusBadRequest, "bot_id required")
		return
	}
	out := gin.H{"bot_user_id": botID, "attached": false}
	if s.agentHub != nil {
		if conn := s.agentHub.lookup(botID); conn != nil {
			out["attached"] = true
			if conn.hostID != "" {
				out["host_id"] = conn.hostID
			}
			if conn.tool != "" {
				out["tool"] = conn.tool
			}
			if !conn.attachedAt.IsZero() {
				out["last_seen_at"] = conn.attachedAt.UTC().Format(time.RFC3339)
			}
			c.JSON(http.StatusOK, out)
			return
		}
	}
	// Phase 4b: agent may live in polar-hosts' hub.
	if info := s.routeAgentLookup(botID); info != nil && info.Attached {
		out["attached"] = true
		out["remote"] = true
		if info.HostID != "" {
			out["host_id"] = info.HostID
		}
		if info.Tool != "" {
			out["tool"] = info.Tool
		}
	}
	c.JSON(http.StatusOK, out)
}

// ── 13. POST /internal/v1/agent/dispatch ────────────────────────────
// Plugin asks dock to enqueue an aiAgent task. Returns 200 +
// {queued:true} immediately; the LLM round-trip happens asynchronously
// inside dock's aiAgent loop and the result is delivered as a new
// message on the chat_thread (UI poll-or-WS-watches it).

type internalAgentDispatchReq struct {
	ThreadID        int64  `json:"thread_id" binding:"required"`
	LLMThreadID     *int64 `json:"llm_thread_id"`
	UserID          string `json:"user_id" binding:"required"`
	ResponderUserID string `json:"responder_user_id" binding:"required"`
	ResponderName   string `json:"responder_name"`
	Content         string `json:"content" binding:"required"`
}

func (s *Server) handleInternalAgentDispatch(c *gin.Context) {
	var req internalAgentDispatchReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	if s.aiAgent == nil {
		internalErr(c, http.StatusServiceUnavailable, "aiAgent not initialized")
		return
	}
	s.aiAgent.enqueue(aiAgentTask{
		ThreadID:        req.ThreadID,
		LLMThreadID:     req.LLMThreadID,
		UserID:          req.UserID,
		ResponderUserID: req.ResponderUserID,
		ResponderName:   req.ResponderName,
		Content:         req.Content,
	})
	c.JSON(http.StatusOK, gin.H{"queued": true})
}

// ── 14. POST /internal/v1/agent/llm-call-record ─────────────────────
// Cost-ledger insert for plugins that drive their own LLM dispatch
// (anything not going through AgentDispatch). Keeps /llm-billing.html
// totals consistent across all caller paths.

type internalLLMCallRecordReq struct {
	WorkspaceID      string   `json:"workspace_id" binding:"required"`
	LLMConfigID      int64    `json:"llm_config_id" binding:"required"`
	ModelRequested   string   `json:"model_requested"`
	ModelResolved    string   `json:"model_resolved"`
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	LatencyMS        int      `json:"latency_ms"`
	StatusCode       int      `json:"status_code"`
	ErrorText        string   `json:"error_text"`
	CostOverrideUSD  *float64 `json:"cost_override_usd"`
}

func (s *Server) handleInternalLLMCallRecord(c *gin.Context) {
	var req internalLLMCallRecordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	if err := s.recordAgentLLMCall(
		req.WorkspaceID, req.LLMConfigID, req.ModelRequested, req.ModelResolved,
		req.PromptTokens, req.CompletionTokens, req.TotalTokens,
		req.LatencyMS, req.StatusCode, req.ErrorText, req.CostOverrideUSD,
	); err != nil {
		internalErr(c, http.StatusInternalServerError, "ledger write failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"recorded": true})
}

// ── 15. POST /internal/v1/agent-tokens/issue ────────────────────────
// Plugin dual-write surface: polar-hosts (and any future plugin that
// mints agent_tokens locally) calls this right after its own local
// INSERT so the canonical row lands in ideamesh.agent_tokens too.
// Without this, dock's /ws/agent auth (resolveAgentToken) 401s on
// every newly-registered agent. See task #216 (polar-hosts split-brain).
//
// ID is caller-supplied — the plugin already generated it locally and
// we honor that so the two copies share the same primary key. TokenHash
// is sha256-hex of the raw bearer; raw never crosses this wire.
// CoderConfigJSON is the JSON literal the plugin wrote into its own
// row (pending-enrollment marker or "{}") so dock's consume path sees
// the same shape.
//
// Idempotent: ON CONFLICT (id) DO NOTHING returns 200 with
// {deduped: true} rather than 409 so the plugin's transient-failure
// retry path is a no-op rather than an error to log.

type internalAgentTokenIssueReq struct {
	ID              string `json:"id" binding:"required"`
	UserID          string `json:"user_id" binding:"required"`
	Name            string `json:"name"`
	TokenHash       string `json:"token_hash" binding:"required"`
	CoderConfigJSON string `json:"coder_config_json"`
}

func (s *Server) handleInternalAgentTokenIssue(c *gin.Context) {
	var req internalAgentTokenIssueReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	inserted, err := s.insertAgentTokenRow(
		req.ID, req.UserID, req.Name, req.TokenHash, req.CoderConfigJSON, time.Now().UTC(),
	)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "agent_token insert failed: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":      req.ID,
		"deduped": !inserted,
	})
}

// ── 16. POST /internal/v1/hosts/issue ───────────────────────────────
// Companion to agent-tokens/issue. Plugin already generated id + slug
// locally; dock just records the canonical hosts row so its
// getHostByAgentToken (called on every /ws/agent message) resolves.
//
// v4 (doc/arch/agent-identity-v4.md): the MachineUUID dedup branch
// was dropped — registerAgent owns host identity (UPSERT keyed on
// id = sha256(salt + raw_uuid)). This handler stays for back-compat
// with v3 plugin callers that haven't moved to AgentRegister yet.
// The handler ignores any machine_uuid field a v3 caller sends.

type internalHostIssueReq struct {
	ID           string `json:"id" binding:"required"`
	WorkspaceID  string `json:"workspace_id" binding:"required"`
	Slug         string `json:"slug" binding:"required"`
	Name         string `json:"name" binding:"required"`
	AgentTokenID string `json:"agent_token_id"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	// Deprecated: v4 — accepted for v3 caller back-compat, ignored.
	MachineUUID string `json:"machine_uuid,omitempty"`
}

func (s *Server) handleInternalHostIssue(c *gin.Context) {
	var req internalHostIssueReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	var agentTokenID *string
	if v := strings.TrimSpace(req.AgentTokenID); v != "" {
		agentTokenID = &v
	}
	resolvedID, deduped, err := s.insertHostRow(
		req.ID, req.WorkspaceID, req.Slug, req.Name, agentTokenID, req.OS, req.Arch, time.Now().UTC(),
	)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "host insert failed: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":      resolvedID,
		"deduped": deduped,
	})
}

// ── 17. v4 — POST /internal/v1/agents/register ──────────────────────
// Full agent-identity-v4 register chain. See doc/arch/agent-identity-v4.md.
//
//   1. host_id = sha256(POLAR_HOST_UUID_SALT + req.MachineUUIDRaw)[:32] hex
//   2. UPSERT hosts (id, workspace_id, …) on conflict refresh last_seen
//   3. mint agent_token (sha256-hashed)
//   4. ensure workspace agent-pool llm_proxy_token (idempotent)
//   5. resolve / auto-create bot_user bound to that proxy token
//   6. INSERT agents (id=ag_<random32hex>, host_id, …)
//
// Salt is read from $POLAR_HOST_UUID_SALT; missing salt falls back
// to the development sentinel + logs a WARN — the PR body documents
// this as a deploy-time secret to set.

type internalAgentRegisterReq struct {
	EnrollToken    string         `json:"enroll_token"`
	WorkspaceID    string         `json:"workspace_id" binding:"required"`
	Name           string         `json:"name" binding:"required"`
	MachineUUIDRaw string         `json:"machine_uuid_raw" binding:"required"`
	HostInfo       map[string]any `json:"host_info"`
	OS             string         `json:"os"`
	Arch           string         `json:"arch"`
	BotUserID      string         `json:"bot_user_id"`
}

func (s *Server) handleInternalAgentRegister(c *gin.Context) {
	var req internalAgentRegisterReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json: "+err.Error())
		return
	}
	resp, err := s.registerAgent(c.Request.Context(), req)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "register: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ── 18. v4 — POST /internal/v1/workspace-proxy-tokens/ensure ────────
// Idempotent return-or-create for the workspace's llm_proxy_tokens row
// matching (workspace_id, name). polar-hosts calls this with
// name="agent-pool" so every auto-created bot binds to one
// workspace-scoped proxy token (so admins can SUM(cost_usd) per
// workspace across all auto bots).

type internalWorkspaceProxyTokenEnsureReq struct {
	WorkspaceID string `json:"workspace_id" binding:"required"`
	OwnerUserID string `json:"owner_user_id"`
	Name        string `json:"name"`
}

func (s *Server) handleInternalWorkspaceProxyTokenEnsure(c *gin.Context) {
	var req internalWorkspaceProxyTokenEnsureReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "agent-pool"
	}
	id, created, err := s.ensureWorkspaceProxyToken(req.WorkspaceID, req.OwnerUserID, name)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "ensure proxy token: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "created": created})
}

// ── 19. v4 — POST /internal/v1/bots/create-for-agent ────────────────
// Idempotent bot_user creation keyed on (workspace_id, name). polar-hosts
// calls this for each registered agent so the operator gets a stable bot
// to attach to. LLMConfig carries the proxy-token binding hint;
// {"proxy_token_id": N} translates server-side into a passthrough bot
// pointing at that token's llm_config row.

type internalBotForAgentReq struct {
	WorkspaceID string         `json:"workspace_id" binding:"required"`
	OwnerUserID string         `json:"owner_user_id"`
	Name        string         `json:"name" binding:"required"`
	Description string         `json:"description"`
	LLMConfig   map[string]any `json:"llm_config"`
}

func (s *Server) handleInternalBotForAgent(c *gin.Context) {
	var req internalBotForAgentReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	botUserID, created, err := s.ensureBotForAgent(req.WorkspaceID, req.OwnerUserID, req.Name, req.Description, req.LLMConfig)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "ensure bot: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"bot_user_id": botUserID, "created": created})
}

func (s *Server) handleInternalHeartbeat(c *gin.Context) {
	plugin, _ := c.Get("plugin_name")
	pluginStr, _ := plugin.(string)
	var req internalHeartbeatReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	if req.Name != pluginStr {
		internalErr(c, http.StatusForbidden, "heartbeat name does not match auth header")
		return
	}
	if err := s.updatePluginHeartbeat(req.Name, req.Version, req.Endpoint, req.PublicBaseURL, []byte(req.UIRoutes), []byte(req.APIRoutes)); err != nil {
		internalErr(c, http.StatusInternalServerError, "heartbeat write failed")
		return
	}
	// Track 3 (OTA): if the module reported its platform and dock has a
	// newer/pinned binary for it, hand back an update directive. Resolution
	// failures are non-fatal — the heartbeat itself already succeeded, so we
	// just omit the directive rather than 500 a working module.
	resp := gin.H{"ack": true}
	if req.OS != "" && req.Arch != "" {
		if upd, err := s.resolvePluginUpdate(req.Name, req.Version, req.OS, req.Arch); err != nil {
			log.Printf("dock: resolvePluginUpdate(%s) failed: %v", req.Name, err)
		} else if upd != nil {
			resp["update"] = upd
		}
	}
	c.JSON(http.StatusOK, resp)
}

// ── Phase 4a: POST /internal/v1/agent-tokens/verify ───────────────────────
// Called by polar-hosts /ws/agent to resolve a raw polar-agent bearer token
// without direct access to dock's agent_tokens table.
// Body: {"token": "<raw>"}
// Response (valid):   {"valid": true,  "token_id": "...", "user_id": "..."}
// Response (invalid): {"valid": false}

func (s *Server) handleInternalAgentTokenVerify(c *gin.Context) {
	var req struct {
		Token string `json:"token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "token required")
		return
	}
	tokenID, userID, err := s.resolveAgentToken(req.Token)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "db error")
		return
	}
	if tokenID == "" {
		c.JSON(http.StatusOK, gin.H{"valid": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"valid": true, "token_id": tokenID, "user_id": userID})
}

// ── 21. GET /internal/v1/agents/:id ─────────────────────────────────
// Resolves a dock agents-v4 id ("ag_…") to its machine facts (os/arch/
// hostname + liveness). First consumer: polar-firewall's fw-svc, which
// needs the target OS to decide pf compilation variants (macOS keeps the
// com.apple anchor scaffolding) and enriches its machines overlay.
// Optional ?workspace_id= narrows the lookup (404 on mismatch) so a
// workspace-scoped caller can't probe other tenants' agents.
//
// online = live hub attach (via the agent's bot user, local or
// polar-hosts hub) OR hosts.last_seen_at within 60s — the same
// freshness window polar-hosts' UI uses (hosts.ts ONLINE_MS).

func (s *Server) handleInternalAgentGet(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		internalErr(c, http.StatusBadRequest, "id required")
		return
	}
	var (
		workspaceID, name, os, arch, hostID   string
		hostName, hostOS, hostArch, botUserID sql.NullString
		lastHello, hostLastSeen               sql.NullTime
	)
	err := s.db.QueryRow(`
		SELECT a.workspace_id, a.name, COALESCE(a.os, COALESCE(h.os, '')), COALESCE(a.arch, COALESCE(h.arch, '')),
		       a.host_id, a.bot_user_id, h.name, h.os, h.arch, a.last_hello_at, h.last_seen_at
		FROM agents a LEFT JOIN hosts h ON h.id = a.host_id
		WHERE a.id = $1`, id).
		Scan(&workspaceID, &name, &os, &arch, &hostID, &botUserID,
			&hostName, &hostOS, &hostArch, &lastHello, &hostLastSeen)
	if err == sql.ErrNoRows {
		internalErr(c, http.StatusNotFound, "agent not found")
		return
	}
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "agent lookup failed")
		return
	}
	if want := strings.TrimSpace(c.Query("workspace_id")); want != "" && want != workspaceID {
		internalErr(c, http.StatusNotFound, "agent not found")
		return
	}

	const onlineWindow = 60 * time.Second
	online := hostLastSeen.Valid && time.Since(hostLastSeen.Time) < onlineWindow ||
		lastHello.Valid && time.Since(lastHello.Time) < onlineWindow
	if !online && botUserID.Valid && botUserID.String != "" {
		if s.agentHub != nil && s.agentHub.lookup(botUserID.String) != nil {
			online = true
		} else if info := s.routeAgentLookup(botUserID.String); info != nil && info.Attached {
			online = true
		}
	}

	out := gin.H{
		"id":           id,
		"workspace_id": workspaceID,
		"name":         name,
		"os":           os,
		"arch":         arch,
		"host_id":      hostID,
		"host_name":    hostName.String,
		"host_os":      hostOS.String,
		"host_arch":    hostArch.String,
		"online":       online,
	}
	if lastHello.Valid {
		out["last_hello_at"] = lastHello.Time.UTC().Format(time.RFC3339)
	}
	if hostLastSeen.Valid {
		out["host_last_seen_at"] = hostLastSeen.Time.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, out)
}
