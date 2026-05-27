package dock

// internal_api_handlers.go — Phase 1-A shipped the /ping smoke route.
// Phase 1-B adds the 8 documented v1 business endpoints. All are gated
// by hmacPluginAuth so c.Get("plugin_name") is always set.
//
// See doc/arch/internal-api-v1.md for request/response shapes.

import (
	"encoding/json"
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
		internalErr(c, http.StatusUnauthorized, "invalid session")
		return
	}
	// Resolve workspace_id — same logic as resolveActiveWorkspace, but
	// without a request header to peek at. Plugins that want a specific
	// workspace pass it explicitly via X-Workspace-Id on the forwarded
	// user request and check membership separately.
	workspaceID, _ := s.personalWorkspaceForUser(sess.UserID)
	c.JSON(http.StatusOK, gin.H{
		"user_id":      sess.UserID,
		"username":     sess.Username,
		"role":         sess.Role,
		"workspace_id": workspaceID,
		"expires_at":   sess.ExpiresAt.UTC().Format(time.RFC3339),
	})
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
	WorkspaceID   string                     `json:"workspace_id" binding:"required"`
	LLMConfigID   int64                      `json:"llm_config_id" binding:"required"`
	ModelOverride string                     `json:"model_override"`
	EndpointTag   string                     `json:"endpoint_tag"`
	Messages      []aiChatCompletionMessage  `json:"messages" binding:"required"`
	MaxTokens     int                        `json:"max_tokens"`
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

// ── 8. POST /internal/v1/notify ────────────────────────────────────────────
// Phase 1-B ships a STUB: counts the user's pushable devices but
// doesn't actually dispatch via APNs/FCM. Real push wiring lands once
// a plugin needs it (Phase 1-C+) — the current chat push pipeline
// expects a chat_messages.id, which doesn't exist for plugin-initiated
// notifications without a richer model. Doc this in the response
// body so plugins know to gracefully degrade.

type internalNotifyReq struct {
	UserID string                 `json:"user_id" binding:"required"`
	Title  string                 `json:"title"`
	Body   string                 `json:"body"`
	Kind   string                 `json:"kind"`
	Data   map[string]any         `json:"data"`
}

func (s *Server) handleInternalNotify(c *gin.Context) {
	var req internalNotifyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	if strings.TrimSpace(req.UserID) == "" {
		internalErr(c, http.StatusBadRequest, "user_id required")
		return
	}
	devices, err := s.listPushableUserDevices(req.UserID)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "device lookup failed")
		return
	}
	// Stub: don't actually dispatch. Returning a 200 with a {stub:true}
	// hint so plugins can degrade gracefully without breaking their
	// callers. Real dispatch path is documented as Phase 1-C+.
	c.JSON(http.StatusOK, gin.H{
		"delivered":  0,
		"skipped":    len(devices),
		"stub":       true,
		"stub_note":  "notify dispatch not yet wired for plugins; tracked for Phase 1-C+",
	})
}

// ── 9. POST /internal/v1/plugin-registry/heartbeat ─────────────────────────
// Plugin announces it is alive. Updates last_heartbeat + version on
// the plugin_modules row. Plugin name must match the auth header
// (gates against an attacker with one key impersonating another
// plugin in the registry).

type internalHeartbeatReq struct {
	Name          string          `json:"name" binding:"required"`
	Version       string          `json:"version"`
	Endpoint      string          `json:"endpoint"`
	UptimeSeconds int64           `json:"uptime_seconds"`
	MetricsURL    string          `json:"metrics_url"`
	// UIRoutes: optional; raw JSON array of sidebar nav entries the
	// plugin offers. See plugin_modules.ui_routes schema. Empty /
	// missing = preserve current value in the column (don't blank
	// existing routes on plugins that haven't yet been updated).
	UIRoutes json.RawMessage `json:"ui_routes,omitempty"`
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
// MachineUUID (added with the host-machine-uuid PR series): when
// non-empty, insertHostRow upserts on (workspace_id, machine_uuid)
// instead of just (id), so a re-register from the same physical box
// — which the plugin already dedup'd to its own existing hosts.id —
// dedups onto dock's existing row too. Empty = legacy agent or
// collector failure; falls back to the pre-PR id-only conflict path.
//
// Response: the {id} field returns dock's resolved id (req.ID when we
// inserted or hit on-id, the EXISTING row's id when we hit on the
// (workspace_id, machine_uuid) dedup index). {deduped} is true when
// we did NOT insert a new row in either path.

type internalHostIssueReq struct {
	ID           string `json:"id" binding:"required"`
	WorkspaceID  string `json:"workspace_id" binding:"required"`
	Slug         string `json:"slug" binding:"required"`
	Name         string `json:"name" binding:"required"`
	AgentTokenID string `json:"agent_token_id"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	MachineUUID  string `json:"machine_uuid"`
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
		req.ID, req.WorkspaceID, req.Slug, req.Name, agentTokenID, req.OS, req.Arch, req.MachineUUID, time.Now().UTC(),
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
	if err := s.updatePluginHeartbeat(req.Name, req.Version, req.Endpoint, []byte(req.UIRoutes)); err != nil {
		internalErr(c, http.StatusInternalServerError, "heartbeat write failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ack": true})
}
