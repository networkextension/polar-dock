package dock

// chat_rooms_handlers.go — P0a HTTP surface for multi-participant
// rooms. Workspace-scoped via requireWorkspaceID; member changes gated
// to room owners. Message wiring lands in P0b.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ---- rooms ----

func (s *Server) handleChatRoomList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userIDAny, _ := c.Get("user_id")
	userID, _ := userIDAny.(string)
	if strings.TrimSpace(userID) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	includeArchived := c.Query("include_archived") == "1"
	rooms, err := s.listChatRoomsForUser(c.Request.Context(), workspaceID, userID, includeArchived)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rooms": rooms})
}

func (s *Server) handleChatRoomCreate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userIDAny, _ := c.Get("user_id")
	userID, _ := userIDAny.(string)
	if strings.TrimSpace(userID) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	var req struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		Topic string `json:"topic"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入"})
		return
	}
	if req.Kind == "" {
		req.Kind = ChatRoomKindRoom
	}
	if !validRoomKind(req.Kind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind 必须为 room/dm/whisper/convene"})
		return
	}
	room, err := s.createChatRoom(c.Request.Context(), workspaceID, req.Kind, req.Name, req.Topic, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"room": room})
}

func (s *Server) handleChatRoomGet(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userIDAny, _ := c.Get("user_id")
	userID, _ := userIDAny.(string)
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	room, err := s.getChatRoom(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if room == nil || room.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "房间不存在"})
		return
	}
	// Non-members can't peek at room metadata. Workspace-admin can
	// step in via the existing admin tools if needed.
	member, err := s.getChatRoomMember(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if member == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "你不在这个房间"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"room": room, "member": member})
}

func (s *Server) handleChatRoomUpdate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if !s.requireRoomOwner(c, id, workspaceID) {
		return
	}
	var req struct {
		Name  *string `json:"name"`
		Topic *string `json:"topic"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入"})
		return
	}
	if err := s.updateChatRoom(c.Request.Context(), id, req.Name, req.Topic); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleChatRoomDelete(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if !s.requireRoomOwner(c, id, workspaceID) {
		return
	}
	if err := s.archiveChatRoom(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- members ----

func (s *Server) handleChatRoomMemberList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userIDAny, _ := c.Get("user_id")
	userID, _ := userIDAny.(string)
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	room, err := s.getChatRoom(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if room == nil || room.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "房间不存在"})
		return
	}
	caller, err := s.getChatRoomMember(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if caller == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "你不在这个房间"})
		return
	}
	members, err := s.listChatRoomMembers(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"members": members})
}

func (s *Server) handleChatRoomMemberAdd(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if !s.requireRoomOwner(c, id, workspaceID) {
		return
	}
	var req struct {
		UserID string `json:"user_id" binding:"required"`
		Role   string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入"})
		return
	}
	if req.Role == "" {
		req.Role = ChatRoomRoleMember
	}
	if !validRoomRole(req.Role) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role 必须为 owner/member/observer"})
		return
	}
	if err := s.addChatRoomMember(c.Request.Context(), id, req.UserID, req.Role); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true})
}

func (s *Server) handleChatRoomMemberRemove(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if !s.requireRoomOwner(c, id, workspaceID) {
		return
	}
	userToRemove := c.Param("userId")
	if strings.TrimSpace(userToRemove) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id required"})
		return
	}
	removed, err := s.removeChatRoomMember(c.Request.Context(), id, userToRemove)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if !removed {
		c.JSON(http.StatusNotFound, gin.H{"error": "成员不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- messages ----

func (s *Server) handleChatRoomMessageList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userIDAny, _ := c.Get("user_id")
	userID, _ := userIDAny.(string)
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if !s.requireRoomMember(c, id, workspaceID, userID) {
		return
	}
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	var beforeID *int64
	if v := c.Query("before_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			beforeID = &n
		}
	}
	msgs, err := s.listRoomMessages(c.Request.Context(), id, beforeID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"messages": msgs})
}

func (s *Server) handleChatRoomMessageSend(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userIDAny, _ := c.Get("user_id")
	userID, _ := userIDAny.(string)
	id, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	if !s.requireRoomMember(c, id, workspaceID, userID) {
		return
	}
	var req struct {
		Content     string `json:"content" binding:"required"`
		MessageType string `json:"message_type"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入"})
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content required"})
		return
	}
	if req.MessageType == "" {
		req.MessageType = "text"
	}
	now := time.Now().UTC()
	msgID, err := s.insertRoomMessage(c.Request.Context(), id, userID, req.MessageType, req.Content, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Realtime fanout via Redis chat:events → wsHub → all room members'
	// connected clients. The subscriber runs on every dock instance so
	// fanout is cross-host correct. Same shape as thread message events.
	s.publishChatInternalEvent(chatInternalEvent{
		Event:     chatEventRoomMessageCreated,
		RoomID:    id,
		MessageID: msgID,
		SenderID:  userID,
	})
	// Qunliao P1: parse @bot_name mentions and fan out one LLM-backed
	// reply per matched bot. No @ = no reply (avoids in-room bot
	// self-chatter). Fire-and-forget; each goroutine handles its own
	// billing gate, ledger record, and WS publish.
	if mentions := parseBotMentions(req.Content); len(mentions) > 0 {
		s.fanoutRoomMentions(workspaceID, id, msgID, userID, req.Content, mentions)
	}
	c.JSON(http.StatusCreated, gin.H{
		"message": gin.H{
			"id":           msgID,
			"room_id":      id,
			"sender_id":    userID,
			"message_type": req.MessageType,
			"content":      req.Content,
			"created_at":   now,
		},
	})
}

// requireRoomMember gates message read/write to actual room members.
// Workspace admin override deferred to P0c. Writes 403/404 itself.
func (s *Server) requireRoomMember(c *gin.Context, roomID int64, workspaceID, userID string) bool {
	room, err := s.getChatRoom(c.Request.Context(), roomID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return false
	}
	if room == nil || room.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "房间不存在"})
		return false
	}
	caller, err := s.getChatRoomMember(c.Request.Context(), roomID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return false
	}
	if caller == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "你不在这个房间"})
		return false
	}
	return true
}

// requireRoomOwner checks (room belongs to workspace) AND (caller is
// owner role on that room). Workspace admins are NOT auto-promoted —
// adding admin-override hooks comes in P0c when the room ops UI lands.
// Writes the 403/404 response itself; returns true when caller may
// proceed.
func (s *Server) requireRoomOwner(c *gin.Context, roomID int64, workspaceID string) bool {
	userIDAny, _ := c.Get("user_id")
	userID, _ := userIDAny.(string)
	room, err := s.getChatRoom(c.Request.Context(), roomID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return false
	}
	if room == nil || room.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "房间不存在"})
		return false
	}
	caller, err := s.getChatRoomMember(c.Request.Context(), roomID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return false
	}
	if caller == nil || caller.Role != ChatRoomRoleOwner {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅房间 owner 可修改"})
		return false
	}
	return true
}
