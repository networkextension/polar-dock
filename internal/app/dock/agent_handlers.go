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
type agentTokenCreateRequest struct {
	Name   string                     `json:"name"`
	Coders map[string]AgentCoderEntry `json:"coders"`
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
	script, scriptErr := renderAgentInstallScript(installScriptInput{
		ServerURL:   serverPublicURL(c),
		Token:       raw,
		Coders:      coders,
		AgentName:   tok.Name,
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

	ac := &agentConn{
		tokenID:           tokenID,
		requestRemoteAddr: c.Request.RemoteAddr,
		userID:            userID,
		botUserID:         botUserID,
		workspaceID:       strings.TrimSpace(c.Query("workspace_id")),
		workdir:           strings.TrimSpace(c.Query("workdir")),
		send:        make(chan []byte, 64),
		close:       make(chan struct{}),
		pending:     map[string]chan agentToolResult{},
		chatPending: map[string]chan agentChatReply{},
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
	c.JSON(http.StatusOK, gin.H{
		"attached":     true,
		"workdir":      conn.workdir,
		"capabilities": caps,
		"tool":         conn.tool,
	})
}
