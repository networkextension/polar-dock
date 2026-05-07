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
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ---- token CRUD ---------------------------------------------------

type agentTokenCreateRequest struct {
	Name string `json:"name"`
}

type agentTokenCreateResponse struct {
	Token AgentToken `json:"token"`
	Raw   string     `json:"raw"`
	Hint  string     `json:"hint"`
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
	raw, tok, err := s.createAgentToken(userID, name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusCreated, agentTokenCreateResponse{
		Token: *tok,
		Raw:   raw,
		Hint:  "复制 raw 后立即保存到本地；服务端只保留哈希，事后无法重新展示",
	})
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
		tokenID:     tokenID,
		userID:      userID,
		botUserID:   botUserID,
		workspaceID: strings.TrimSpace(c.Query("workspace_id")),
		workdir:     strings.TrimSpace(c.Query("workdir")),
		send:        make(chan []byte, 64),
		close:       make(chan struct{}),
		pending:     map[string]chan agentToolResult{},
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
		case "hello":
			// Optional handshake echo (agent describes itself).
			log.Printf("[agent ws] hello bot=%s payload=%s", ac.botUserID, truncateForLog(string(raw), 200))
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

// handleAgentStatus tells the UI whether a bot has an agent attached.
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
	c.JSON(http.StatusOK, gin.H{
		"attached": true,
		"workdir":  conn.workdir,
	})
}
