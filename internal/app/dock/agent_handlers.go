package dock

// HTTP + WebSocket handlers for the polar-agent transport. Three
// surfaces:
//
//   POST   /api/agent/tokens      — issue a new bearer token (raw
//                                    returned once)
//   GET    /api/agent/tokens      — list (no raw)
//   DELETE /api/agent/tokens/:id  — revoke
//   GET    /ws/agent              — agent connects with bearer
//   POST   /api/agent/bots/:bot_id/dispatch  — debug-fire a tool
//                                    call to verify the round-trip
//                                    (admin or token owner)

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ---- token CRUD ---------------------------------------------------

// agentTokenCreateRequest is the typed payload for the create
// wizard. coders is a map of {kimi|claude|codex: {enabled, auth_mode}}
// captured at the wizard's coder-pick step. Sane defaults apply
// when fields are missing (legacy clients calling without coder
// config still get a working token).
//
// api_keys is a separate map from coders so the keys never accidentally
// land in the persisted coder_config JSONB — they flow into the
// install script render and are otherwise discarded after the
// HTTP handler returns. Only present for coders the user picked
// api_key auth_mode for.
type agentTokenCreateRequest struct {
	Name    string                     `json:"name"`
	Coders  map[string]AgentCoderEntry `json:"coders"`
	APIKeys map[string]string          `json:"api_keys"`
	// TargetOS picks the install-output flavor: "darwin" (default)
	// returns a curl-bash install.sh; "linux" returns a docker run
	// quick-start script for the polar-agent image.
	TargetOS string `json:"target_os"`
}

type agentTokenCreateResponse struct {
	Token         AgentToken `json:"token"`
	Raw           string     `json:"raw"`
	InstallScript string     `json:"install_script"`
	Hint          string     `json:"hint"`
}

func (s *Server) handleAgentTokenCreate(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	var req agentTokenCreateRequest
	_ = c.ShouldBindJSON(&req)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "polar-agent"
	}
	coders := AgentCoderConfig{}
	if entry, present := req.Coders["kimi"]; present {
		coders.Kimi = normalizeCoderEntry(entry)
	}
	if entry, present := req.Coders["claude"]; present {
		coders.Claude = normalizeCoderEntry(entry)
	}
	if entry, present := req.Coders["codex"]; present {
		coders.Codex = normalizeCoderEntry(entry)
	}
	raw, tok, err := s.createAgentToken(userID, name, coders)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Filter API keys to only those whose coder is enabled + api_key auth.
	// Keys for disabled coders or login-mode coders are silently dropped
	// (no point baking them into the script).
	apiKeys := map[string]string{}
	for _, name := range []string{"kimi", "claude", "codex"} {
		k := strings.TrimSpace(req.APIKeys[name])
		if k == "" {
			continue
		}
		var entry AgentCoderEntry
		switch name {
		case "kimi":
			entry = coders.Kimi
		case "claude":
			entry = coders.Claude
		case "codex":
			entry = coders.Codex
		}
		if entry.Enabled && entry.AuthMode == "api_key" {
			apiKeys[name] = k
		}
	}
	targetOS := strings.ToLower(strings.TrimSpace(req.TargetOS))
	switch targetOS {
	case "linux", "darwin":
		// ok
	default:
		targetOS = "darwin"
	}
	script, scriptErr := renderAgentInstallScript(installScriptInput{
		ServerURL: serverPublicURL(c),
		Token:     raw,
		Coders:    coders,
		APIKeys:   apiKeys,
		AgentName: tok.Name,
		TargetOS:  targetOS,
	})
	if scriptErr != nil {
		// Fall back to a minimal script — never fail the token create
		// over a template glitch.
		log.Printf("[agent install] render script failed: %v", scriptErr)
		script = fmt.Sprintf("# install script render failed: %v\n# raw token: %s\n", scriptErr, raw)
	}
	c.JSON(http.StatusCreated, agentTokenCreateResponse{
		Token:         *tok,
		Raw:           raw,
		InstallScript: script,
		Hint:          "复制 raw token 和安装脚本后立即保存到本地；服务端只保留哈希，事后无法重新展示",
	})
}

// normalizeCoderEntry coerces user input into a known auth_mode set.
// Unknown auth_mode strings degrade to "api_key" so the install
// script always knows what to do.
func normalizeCoderEntry(e AgentCoderEntry) AgentCoderEntry {
	if !e.Enabled {
		return AgentCoderEntry{}
	}
	mode := strings.TrimSpace(e.AuthMode)
	switch mode {
	case "api_key", "login":
		// ok
	default:
		mode = "api_key"
	}
	return AgentCoderEntry{Enabled: true, AuthMode: mode}
}

// serverPublicURL derives the absolute base URL the install script
// should embed (so curl-pipe-sh works regardless of where the user
// runs it). Prefers X-Forwarded-Proto/Host (when behind a proxy);
// falls back to the gin request's Host + scheme.
func serverPublicURL(c *gin.Context) string {
	scheme := c.GetHeader("X-Forwarded-Proto")
	if scheme == "" {
		if c.Request.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	return scheme + "://" + host
}

func (s *Server) handleAgentTokenList(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	tokens, err := s.listAgentTokensForUser(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": tokens})
}

func (s *Server) handleAgentTokenRevoke(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	if err := s.revokeAgentToken(id, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Token 不存在或已撤销"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- /ws/agent ----------------------------------------------------

var agentWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool { return true },
}

// handleAgentWS accepts a bearer token via either the
// "Authorization: Bearer <raw>" header or a ?token=<raw> query
// param (CLI websocket libraries can be picky about headers).
// Required query: bot_id (the bot this agent is binding to).
// Optional: workdir, version (informational).
func (s *Server) handleAgentWS(c *gin.Context) {
	raw := strings.TrimSpace(c.Query("token"))
	if raw == "" {
		auth := c.GetHeader("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			raw = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if raw == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing agent token"})
		return
	}
	tokenID, userID, err := s.resolveAgentToken(raw)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if tokenID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "agent token invalid or revoked"})
		return
	}
	botUserID := strings.TrimSpace(c.Query("bot_id"))
	if botUserID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bot_id query required"})
		return
	}
	bot, err := s.getBotUserByUserID(botUserID)
	if err != nil || bot == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "bot 不存在"})
		return
	}
	// Bot ownership: token's user must own the bot. (T6 aside —
	// bot_users still has owner_user_id audit field.)
	if bot.OwnerUserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "你没有权限附着到这个 bot"})
		return
	}

	conn, err := agentWSUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("[agent ws] upgrade failed user=%s bot=%s: %v", userID, botUserID, err)
		return
	}
	_ = s.touchAgentTokenUsed(tokenID)

	// Prefer the proxy-forwarded client IP (X-Real-IP / X-Forwarded-For)
	// when set. c.Request.RemoteAddr is the immediate TCP peer, which
	// is nginx (127.0.0.1) in the typical deployment — useless for
	// "where is this agent coming from". c.ClientIP() honors the
	// forwarded headers when gin's trusted-proxy list allows them
	// (default = trust all, see the gin startup warning).
	ac := &agentConn{
		tokenID:           tokenID,
		requestRemoteAddr: c.ClientIP(),
		attachedAt:        time.Now(),
		userID:            userID,
		botUserID:         botUserID,
		workspaceID:       strings.TrimSpace(c.Query("workspace_id")),
		workdir:           strings.TrimSpace(c.Query("workdir")),
		send:        make(chan []byte, 64),
		close:       make(chan struct{}),
		pending:            map[string]chan agentToolResult{},
		chatPending:        map[string]chan agentChatReply{},
		skillPending:       map[int64]chan skillEventFrame{},
		skillStdoutPending: map[int64]chan []byte{},
	}
	if displaced := s.agentHub.register(ac); displaced != nil {
		log.Printf("[agent ws] kicking previous connection bot=%s", botUserID)
		close(displaced.close)
	}
	log.Printf("[agent ws] connected user=%s bot=%s workdir=%s", userID, botUserID, ac.workdir)

	go s.runAgentWritePump(conn, ac)
	s.runAgentReadPump(conn, ac)
}

func (s *Server) runAgentWritePump(conn *websocket.Conn, ac *agentConn) {
	pingT := time.NewTicker(30 * time.Second)
	defer pingT.Stop()
	for {
		select {
		case msg, ok := <-ac.send:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-pingT.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-ac.close:
			return
		}
	}
}

func (s *Server) runAgentReadPump(conn *websocket.Conn, ac *agentConn) {
	defer func() {
		s.agentHub.unregister(ac)
		select {
		case <-ac.close:
		default:
			close(ac.close)
		}
		_ = conn.Close()
		log.Printf("[agent ws] disconnected user=%s bot=%s", ac.userID, ac.botUserID)
	}()
	conn.SetReadLimit(8 << 20) // 8 MiB cap (file_write payloads can be larger)
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		// Heartbeat: bump hosts.last_seen_at so /hosts.html flips
		// green. Without this the host shows green right after
		// skill.advertise, then ages back to gray once the UI's
		// 60s online-window expires — even though the WS is alive
		// and the agent is happily pinging.
		if ac.hostID != "" {
			_ = s.touchHostLastSeen(ac.hostID, time.Now().UTC())
		}
		return nil
	})
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		// Parse just enough to dispatch by kind.
		var head struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			log.Printf("[agent ws] bad message bot=%s: %v", ac.botUserID, err)
			continue
		}
		switch head.Kind {
		case "tool_result":
			var r agentToolResult
			if err := json.Unmarshal(raw, &r); err != nil {
				log.Printf("[agent ws] tool_result parse failed bot=%s: %v", ac.botUserID, err)
				continue
			}
			ac.deliverResult(r)
		case "chat_reply":
			var r agentChatReply
			if err := json.Unmarshal(raw, &r); err != nil {
				log.Printf("[agent ws] chat_reply parse failed bot=%s: %v", ac.botUserID, err)
				continue
			}
			ac.deliverChatReply(r)
		case "hello":
			var h struct {
				Kind         string   `json:"kind"`
				Version      string   `json:"version"`
				Capabilities []string `json:"capabilities"`
				Tool         string   `json:"tool"`
				HostOS       string   `json:"host_os"`
				HostArch     string   `json:"host_arch"`
				HostName     string   `json:"host_name"`
			}
			if err := json.Unmarshal(raw, &h); err == nil {
				ac.capabilities = h.Capabilities
				ac.tool = strings.TrimSpace(h.Tool)
				// Persist host info to the agent_tokens row so the
				// admin UI can show "this token is on machine X".
				// Best-effort — don't fail the hello over a DB blip.
				if ac.tokenID != "" && (h.HostOS != "" || h.HostArch != "" || h.HostName != "") {
					hostIP := remoteIPForRequest(ac.requestRemoteAddr)
					if err := s.updateAgentTokenHostInfo(ac.tokenID, h.HostOS, h.HostArch, h.HostName, hostIP); err != nil {
						log.Printf("[agent ws] update host info token=%s: %v", ac.tokenID, err)
					}
				}
			}
			log.Printf("[agent ws] hello bot=%s caps=%v tool=%q payload=%s",
				ac.botUserID, ac.capabilities, ac.tool, truncateForLog(string(raw), 200))
		case "skill.advertise":
			// Host module Phase 0: agent reports its compiled-in skill
			// kinds. We persist them onto the hosts row + bump
			// last_seen_at so the UI shows the host as online with
			// the right set of enable-able cards. Legacy agents whose
			// tokens haven't been through /api/hosts/register have no
			// hosts row — that's fine, we log + drop.
			var msg struct {
				Skills []AdvertisedSkill `json:"skills"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				log.Printf("[agent ws] skill.advertise parse failed token=%s: %v", ac.tokenID, err)
				continue
			}
			if ac.hostID == "" {
				host, herr := s.getHostByAgentToken(ac.tokenID)
				if herr != nil {
					log.Printf("[agent ws] resolve host by token=%s: %v", ac.tokenID, herr)
					continue
				}
				if host == nil {
					// Legacy agent without a hosts row — drop quietly.
					// Once that operator runs `polar-agent register`,
					// they'll get a hosts row and next advertise lands.
					log.Printf("[agent ws] skill.advertise token=%s has no host row (legacy agent), dropping %d skills", ac.tokenID, len(msg.Skills))
					continue
				}
				ac.hostID = host.ID
			}
			remoteIP := remoteIPForRequest(ac.requestRemoteAddr)
			now := time.Now().UTC()
			if err := s.updateHostAdvertisedSkills(ac.hostID, msg.Skills, remoteIP, now); err != nil {
				log.Printf("[agent ws] persist advertised skills host=%s: %v", ac.hostID, err)
				continue
			}
			// P1b auto-shim: materialize a host_skills row per advertised
			// kind so operators can bind bots to specific host skills
			// without first running a manual "enable" UI step. Per-skill
			// upsert; failure of one doesn't drop the others.
			for _, sk := range msg.Skills {
				if _, err := s.ensureHostSkillFromAdvertise(ac.hostID, sk, now); err != nil {
					log.Printf("[agent ws] ensure host_skill host=%s kind=%s: %v", ac.hostID, sk.Kind, err)
				}
			}
			log.Printf("[agent ws] skill.advertise host=%s skills=%d", ac.hostID, len(msg.Skills))
		case "skill.event":
			// Host module P1c.3: agent reports lifecycle events for a
			// skill.start dispatch. Forward to the per-run channel
			// registered by the dispatcher in generateReplyWithHostSkill;
			// dropped silently when no consumer is waiting (legacy
			// agent or already-finished run).
			var ev skillEventFrame
			if err := json.Unmarshal(raw, &ev); err != nil {
				log.Printf("[agent ws] skill.event parse failed bot=%s: %v", ac.botUserID, err)
				continue
			}
			// P2 (Shell): stdout chunks route to the byte channel
			// instead of the lifecycle channel. Decoupling means a
			// slow xterm reader doesn't backpressure state/log/exit
			// events for this or any other run.
			if ev.EventKind == "stdout" {
				if b64, _ := ev.Data["bytes_b64"].(string); b64 != "" {
					chunk, err := base64.StdEncoding.DecodeString(b64)
					if err == nil {
						ac.deliverSkillStdout(ev.RunID, chunk)
					} else {
						log.Printf("[agent ws] skill.event stdout base64 decode run=%d: %v", ev.RunID, err)
					}
				}
				continue
			}
			ac.deliverSkillEvent(ev)
		default:
			log.Printf("[agent ws] unknown kind=%s bot=%s", head.Kind, ac.botUserID)
		}
	}
}

// ---- debug dispatch -----------------------------------------------

type agentDispatchRequest struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// handleAgentDispatch fires a tool call at the bot's attached agent
// and waits for the result. Used to verify the round-trip without
// going through the LLM. Auth: token owner only (the user who
// created the bot).
func (s *Server) handleAgentDispatch(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	botID := strings.TrimSpace(c.Param("bot_id"))
	if botID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	bot, err := s.getBotUserByUserID(botID)
	if err != nil || bot == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "bot 不存在"})
		return
	}
	if bot.OwnerUserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "你没有权限"})
		return
	}
	var req agentDispatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	if strings.TrimSpace(req.Tool) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tool required"})
		return
	}
	callID := generateResourceID()
	result, err := s.agentHub.dispatchToolCall(botID, callID, req.Tool, req.Args)
	if err != nil {
		switch err {
		case errAgentNotAttached:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent 未连接"})
			return
		case errAgentTimeout:
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "agent 没在 60 秒内返回"})
			return
		case errAgentDisconnected:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent 在等待结果时断开"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"call_id": callID, "result": result})
}

// handleAgentWhoAmI authenticates an agent token bearer (the same
// way /ws/agent does) and returns the token id + user id. Used by
// the polar-agent CLI's `login` / `status` commands to verify a
// raw token without needing a user session — agent tokens aren't
// valid on the user-session endpoints, so /api/agent/tokens GET
// can't double as a verify probe.
func (s *Server) handleAgentWhoAmI(c *gin.Context) {
	raw := strings.TrimSpace(c.Query("token"))
	if raw == "" {
		auth := c.GetHeader("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			raw = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if raw == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing agent token"})
		return
	}
	tokenID, userID, err := s.resolveAgentToken(raw)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if tokenID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "agent token invalid or revoked"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":       true,
		"token_id": tokenID,
		"user_id":  userID,
	})
}

// remoteIPForRequest extracts a clean ip-only string from a
// gin RemoteAddr value (`host:port` or just `host`). Returns the
// raw input on parse failure — caller treats it as opaque.
func remoteIPForRequest(remoteAddr string) string {
	if remoteAddr == "" {
		return ""
	}
	// Strip trailing :port if present. IPv6 has multiple colons; the
	// last one separates port. Use a simple split on last colon.
	for i := len(remoteAddr) - 1; i >= 0; i-- {
		if remoteAddr[i] == ':' {
			return strings.TrimPrefix(strings.TrimSuffix(remoteAddr[:i], "]"), "[")
		}
	}
	return remoteAddr
}

// handleAgentBotConfig returns a bot user's config that polar-agent
// needs at attach time (preferred_tool, bot_kind, bot_name).
// Auth: agent token bearer — distinct from /api/agent/bots/:bot_id/status
// which uses user session for the dashboard widget.
//
// Polar-agent uses this to implement `attach --tool=auto`: instead
// of operator manually setting POLAR_BOT_TOOLS env per bot, the
// agent fetches the bot's stored preferred_tool from dock and uses
// that. Empty preferred_tool → tool-loop mode (no --tool flag).
func (s *Server) handleAgentBotConfig(c *gin.Context) {
	botID := strings.TrimSpace(c.Param("bot_id"))
	if botID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bot_id required"})
		return
	}
	bot, err := s.getBotUserByUserID(botID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if bot == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "bot not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"bot_id":         bot.BotUserID,
		"bot_name":       bot.Name,
		"bot_kind":       bot.BotKind,
		"preferred_tool": bot.PreferredTool,
	})
}

// agentAutoRegisterRequest is what polar-agent POSTs at startup to
// declare its capabilities. Two registration tracks today:
//
//   coders   — one passthrough bot per coder (kimi/claude/codex);
//              agent's local CLI handles the LLM call. Code projects.
//   research — one classic bot using workspace default LLM + agent
//              for tool execution. Research / non-code projects;
//              dock makes the LLM call, agent runs read_file /
//              write_file / run_cmd / etc. tool_calls.
//
// Both tracks are idempotent per (token, kind) — re-registering
// returns the existing bot ids.
type agentAutoRegisterRequest struct {
	Coders    []string `json:"coders"`
	Research  bool     `json:"research"`
	AgentName string   `json:"agent_name"`
}

type agentAutoRegisteredBot struct {
	BotUserID     string `json:"bot_user_id"`
	Name          string `json:"name"`
	PreferredTool string `json:"preferred_tool"`
	BotKind       string `json:"bot_kind"`
}

// handleAgentAutoRegister upserts one bot_users row per declared
// coder. Idempotent: re-running with the same coder list returns
// the same bot ids. The agent token determines ownership; the
// owner's personal team is the workspace.
//
// Request body: {"coders": ["kimi","claude","codex"], "agent_name": "my-mac"}
// Response:     {"bots": [{"bot_user_id": "bot_xxx", "preferred_tool": "kimi"}, ...]}
func (s *Server) handleAgentAutoRegister(c *gin.Context) {
	tokenIDVal, _ := c.Get("agent_token_id")
	tokenID, _ := tokenIDVal.(string)
	userIDVal, _ := c.Get("agent_user_id")
	userID, _ := userIDVal.(string)
	if tokenID == "" || userID == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "agent context missing"})
		return
	}

	var req agentAutoRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid input"})
		return
	}
	if len(req.Coders) == 0 && !req.Research {
		c.JSON(http.StatusBadRequest, gin.H{"error": "specify at least one of: coders[], research:true"})
		return
	}

	workspaceID, err := s.personalWorkspaceForUser(userID)
	if err != nil || workspaceID == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "personal workspace lookup failed"})
		return
	}

	// Deduplicate + filter known coder names.
	seen := map[string]bool{}
	cleanCoders := []string{}
	for _, raw := range req.Coders {
		name := strings.ToLower(strings.TrimSpace(raw))
		switch name {
		case "kimi", "claude", "codex":
			// allow; expand if custom tools become a thing
		default:
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		cleanCoders = append(cleanCoders, name)
	}

	now := time.Now().UTC()
	out := []agentAutoRegisteredBot{}
	for _, coder := range cleanCoders {
		bot, err := s.ensureAutoRegisteredCoderBot(tokenID, userID, workspaceID, req.AgentName, coder, now)
		if err != nil {
			log.Printf("[agent auto-register] %s coder=%s failed: %v", tokenID, coder, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "auto-register failed: " + err.Error()})
			return
		}
		out = append(out, agentAutoRegisteredBot{
			BotUserID:     bot.BotUserID,
			Name:          bot.Name,
			PreferredTool: coder,
			BotKind:       "passthrough",
		})
	}

	if req.Research {
		bot, err := s.ensureAutoRegisteredResearchBot(tokenID, userID, workspaceID, req.AgentName, now)
		if err != nil {
			log.Printf("[agent auto-register] %s research failed: %v", tokenID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "research bot register failed: " + err.Error()})
			return
		}
		if bot == nil {
			// No workspace default LLM — surface a hint but don't
			// fail the whole request; coder bots may still have
			// registered successfully.
			log.Printf("[agent auto-register] %s research skipped: workspace %s has no default_llm_config_id", tokenID, workspaceID)
			c.JSON(http.StatusOK, gin.H{
				"bots":          out,
				"research_skip": "workspace has no default_llm_config_id; set one before research projects can use this agent",
			})
			return
		}
		out = append(out, agentAutoRegisteredBot{
			BotUserID:     bot.BotUserID,
			Name:          bot.Name,
			PreferredTool: "",
			BotKind:       "classic",
		})
	}

	c.JSON(http.StatusOK, gin.H{"bots": out})
}

// handleAgentStatus tells the UI whether a bot has an agent attached
// and which capabilities the agent declared at hello time. The UI
// uses capabilities to render passthrough mode badges (e.g. "本地
// kimi-cli 接管中"), avoiding confusion when the bot's configured
// model is bypassed by an external runtime.
func (s *Server) handleAgentStatus(c *gin.Context) {
	botID := strings.TrimSpace(c.Param("bot_id"))
	if botID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	conn := s.agentHub.lookup(botID)
	if conn == nil {
		c.JSON(http.StatusOK, gin.H{"attached": false})
		return
	}
	caps := conn.capabilities
	if caps == nil {
		caps = []string{}
	}
	// remote_addr is the Go-formatted "host:port" from r.RemoteAddr;
	// strip the port before exposing — UI just wants the IP. nginx
	// puts the proxy's address here unless we wire a forwarded-for
	// header through, so this is "where dock saw the connection
	// originate" — exactly the right answer for an operator.
	remoteIP := remoteIPForRequest(conn.requestRemoteAddr)
	uptimeSec := int(time.Since(conn.attachedAt).Seconds())
	c.JSON(http.StatusOK, gin.H{
		"attached":     true,
		"workdir":      conn.workdir,
		"capabilities": caps,
		"tool":         conn.tool,
		"remote_ip":    remoteIP,
		"attached_at":  conn.attachedAt.UTC().Format(time.RFC3339),
		"uptime_sec":   uptimeSec,
	})
}
