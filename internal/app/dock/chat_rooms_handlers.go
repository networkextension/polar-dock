package dock

// chat_rooms_handlers.go — P0a HTTP surface for multi-participant
// rooms. Workspace-scoped via requireWorkspaceID; member changes gated
// to room owners. Message wiring lands in P0b.

import (
	"net/http"
	"strings"

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
