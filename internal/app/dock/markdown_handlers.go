package dock

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// assistData feeds markdown.assist / post.assist / post.reply_assist
// — three templates with the same shape: a base ruleset plus
// optional bot-side preferences and per-call instruction.
type assistData struct {
	BotPrompt   string
	Instruction string
}

const defaultMarkdownAssistTemplate = `你是一个中文写作润色助手。你的任务是基于用户原文做改写与润色，而不是替用户重新发明核心观点。

必须遵循：
1) 保留用户核心事实、观点、立场与结论，不擅自新增关键事实。
2) 优先做表达优化：结构、语序、逻辑、可读性、措辞、语法。
3) 如果用户给了明确风格要求，按要求润色；若无要求，默认清晰、自然、专业。
4) 输出只返回最终可直接粘贴的正文内容，不要解释、不要前后缀。
{{- if .BotPrompt }}

Bot 额外写作偏好：
{{.BotPrompt}}
{{- end -}}
{{- if .Instruction }}

本次润色要求：
{{.Instruction}}
{{- end }}`

func (s *Server) buildMarkdownAssistSystemPrompt(botPrompt, instruction string) string {
	return s.renderPrompt(promptSlugMarkdownAssist, assistData{
		BotPrompt:   strings.TrimSpace(botPrompt),
		Instruction: strings.TrimSpace(instruction),
	}, defaultMarkdownAssistTemplate)
}

func (s *Server) handleMarkdownAssistWithBot(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	if s.aiAgent == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "AI Agent 未初始化"})
		return
	}

	var req struct {
		BotID       int64  `json:"bot_id"`
		LLMConfigID int64  `json:"llm_config_id"`
		Title       string `json:"title"`
		Content     string `json:"content" binding:"required"`
		Instruction string `json:"instruction"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	sourceContent := strings.TrimSpace(req.Content)
	if sourceContent == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "内容不能为空"})
		return
	}

	// Bot is now optional. resolveAssistBot returns runtimeConfig
	// from (in priority): explicit llm_config_id, bot's bound LLM,
	// or workspace default — so assist still works without a bot.
	runtimeConfig, bot, llmConfig, rerr := s.resolveAssistBot(userIDStr, c.GetString("workspace_id"), req.BotID, req.LLMConfigID)
	if rerr != nil {
		if ae, ok := rerr.(*assistError); ok {
			c.JSON(ae.status, gin.H{"error": ae.message})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		}
		return
	}

	botPersona := ""
	if bot != nil {
		botPersona = bot.SystemPrompt
	}
	runtimeConfig.SystemPrompt = s.buildMarkdownAssistSystemPrompt(botPersona, req.Instruction)
	runtimeConfig.Endpoint = "markdown_assist"

	userPrompt := strings.TrimSpace(strings.Join([]string{
		"请润色以下正文，保持核心内容不变：",
		"标题：" + strings.TrimSpace(req.Title),
		"--- 原文开始 ---",
		sourceContent,
		"--- 原文结束 ---",
	}, "\n"))

	payload := aiChatCompletionRequest{
		Model: runtimeConfig.Model,
		Messages: []aiChatCompletionMessage{
			{Role: "system", Content: runtimeConfig.SystemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}

	result, err := s.aiAgent.requestChatCompletion(runtimeConfig, payload)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(result.Choices) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "模型返回为空"})
		return
	}

	refined := strings.TrimSpace(result.Choices[0].Message.Content)
	if refined == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "模型返回为空"})
		return
	}

	resp := gin.H{
		"message": "润色完成",
		"content": refined,
		"llm": gin.H{
			"config_id": llmConfig.ID,
			"model":     llmConfig.Model,
		},
	}
	// Same usage echo as the post-assist endpoints (issue #170); kept
	// in sync so all three assist callers (editor, creation bubble,
	// posts plaza) report token spend consistently.
	if result.Usage != nil {
		resp["usage"] = result.Usage
	}
	if bot != nil {
		resp["bot"] = gin.H{"id": bot.ID, "name": bot.Name}
	}
	c.JSON(http.StatusOK, resp)
}

func (s *Server) handleMarkdownSubmit(c *gin.Context) {
	var req struct {
		Title       string  `json:"title" binding:"required"`
		Content     string  `json:"content" binding:"required"`
		IsPublic    bool    `json:"is_public"`    // legacy; mapped via resolveMarkdownVisibility
		Visibility  string  `json:"visibility"`   // private | workspace | public
		WorkspaceID *string `json:"workspace_id"` // required when visibility=workspace
		EditorMode  string  `json:"editor_mode"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	userID, _ := c.Get("user_id")
	username, _ := c.Get("username")

	now := time.Now()
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	visibility, workspaceID, vErr := s.resolveMarkdownVisibility(userIDStr, req.Visibility, req.IsPublic, req.WorkspaceID)
	if vErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": vErr.Error()})
		return
	}

	entry, _, err := s.saveMarkdownDocument(userIDStr, req.Title, req.Content, req.EditorMode, visibility, workspaceID, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"message":      "保存成功",
		"id":           entry.ID,
		"file":         entry.FilePath,
		"username":     username,
		"is_public":    visibility == MarkdownVisibilityPublic,
		"visibility":   visibility,
		"workspace_id": workspaceID,
		"editor_mode":  entry.EditorMode,
	})
}

// resolveMarkdownVisibility takes the request fields and produces a
// canonical (visibility, workspace_id) tuple. Backward compat: when
// only `is_public` is sent, true→public / false→private. Workspace
// tier validates the user is a member of the supplied workspace_id.
func (s *Server) resolveMarkdownVisibility(userID, requestedVis string, isPublic bool, workspaceID *string) (string, *string, error) {
	visibility := strings.TrimSpace(requestedVis)
	if visibility == "" {
		if isPublic {
			visibility = MarkdownVisibilityPublic
		} else {
			visibility = MarkdownVisibilityPrivate
		}
	}
	switch visibility {
	case MarkdownVisibilityPrivate, MarkdownVisibilityPublic:
		return visibility, nil, nil
	case MarkdownVisibilityWorkspace:
		if workspaceID == nil || strings.TrimSpace(*workspaceID) == "" {
			return "", nil, fmt.Errorf("visibility=workspace 需要 workspace_id")
		}
		member, err := s.getTeamMember(*workspaceID, userID)
		if err != nil {
			return "", nil, fmt.Errorf("workspace 验证失败")
		}
		if member == nil {
			return "", nil, fmt.Errorf("不是该 workspace 的成员")
		}
		return visibility, workspaceID, nil
	default:
		return "", nil, fmt.Errorf("无效 visibility: %s", visibility)
	}
}

func (s *Server) handleMarkdownList(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	limit := 0
	if limitStr := c.Query("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
			return
		}
		limit = parsed
	}

	offset := 0
	if offsetStr := c.Query("offset"); offsetStr != "" {
		parsed, err := strconv.Atoi(offsetStr)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
			return
		}
		offset = parsed
	}

	entries, hasMore, err := s.listMarkdownEntries(userIDStr, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	nextOffset := offset + len(entries)
	c.JSON(http.StatusOK, gin.H{
		"entries":     entries,
		"has_more":    hasMore,
		"next_offset": nextOffset,
	})
}

func (s *Server) handlePublicMarkdownList(c *gin.Context) {
	limit := 0
	if limitStr := c.Query("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
			return
		}
		limit = parsed
	}

	offset := 0
	if offsetStr := c.Query("offset"); offsetStr != "" {
		parsed, err := strconv.Atoi(offsetStr)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
			return
		}
		offset = parsed
	}

	entries, hasMore, err := s.listPublicMarkdownEntries(limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	nextOffset := offset + len(entries)
	c.JSON(http.StatusOK, gin.H{
		"entries":     entries,
		"has_more":    hasMore,
		"next_offset": nextOffset,
	})
}

func (s *Server) handleMarkdownRead(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	entryID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || entryID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	entry, canEdit, err := s.getMarkdownEntryForUser(userIDStr, entryID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if entry == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到记录"})
		return
	}

	content, err := os.ReadFile(entry.FilePath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"entry":    entry,
		"content":  string(content),
		"can_edit": canEdit,
	})
}

func (s *Server) handleMarkdownUpdate(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	entryID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || entryID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	var req struct {
		Title       string  `json:"title" binding:"required"`
		Content     string  `json:"content" binding:"required"`
		IsPublic    bool    `json:"is_public"`
		Visibility  string  `json:"visibility"`
		WorkspaceID *string `json:"workspace_id"`
		EditorMode  string  `json:"editor_mode"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	entry, err := s.getOwnedMarkdownEntry(userIDStr, entryID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if entry == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到记录"})
		return
	}

	content := req.Content
	if !strings.HasPrefix(strings.TrimSpace(content), "#") {
		content = "# " + req.Title + "\n\n" + content
	}

	if err := os.WriteFile(entry.FilePath, []byte(content), 0o644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	editorMode := strings.TrimSpace(req.EditorMode)
	if editorMode == "" {
		editorMode = entry.EditorMode
	}
	editorMode = normalizeEditorMode(editorMode)
	summary, coverURL := extractMarkdownMeta(content)
	visibility, workspaceID, vErr := s.resolveMarkdownVisibility(userIDStr, req.Visibility, req.IsPublic, req.WorkspaceID)
	if vErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": vErr.Error()})
		return
	}
	if err := s.updateMarkdownEntry(userIDStr, entryID, req.Title, entry.FilePath, summary, coverURL, editorMode, visibility, workspaceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":      "更新成功",
		"id":           entryID,
		"is_public":    visibility == MarkdownVisibilityPublic,
		"visibility":   visibility,
		"workspace_id": workspaceID,
		"editor_mode":  editorMode,
	})
}

// handleWorkspaceMarkdownList lists entries with visibility=workspace
// for a workspace the caller is a member of. workspace_id comes from
// the X-Workspace-Id header (set by AuthMiddleware) so the URL is
// stable and the membership is enforced upstream.
func (s *Server) handleWorkspaceMarkdownList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	limit := 0
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
			return
		}
	}
	offset := 0
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
			return
		}
	}
	entries, hasMore, err := s.listWorkspaceMarkdownEntries(workspaceID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"entries":     entries,
		"has_more":    hasMore,
		"next_offset": offset + len(entries),
	})
}

func (s *Server) handleMarkdownDelete(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	entryID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || entryID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	entry, err := s.getOwnedMarkdownEntry(userIDStr, entryID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if entry == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到记录"})
		return
	}

	_ = os.Remove(entry.FilePath)
	if err := s.deleteMarkdownEntry(userIDStr, entryID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "删除成功"})
}

func (s *Server) handlePublicMarkdownRead(c *gin.Context) {
	entryID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || entryID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	viewerUserID := ""
	if session := s.getAccessSession(extractAccessToken(c)); session != nil {
		viewerUserID = session.UserID
	}

	entry, _, err := s.getMarkdownEntryForUser(viewerUserID, entryID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if entry == nil || (!entry.IsPublic && entry.UserID != viewerUserID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到记录"})
		return
	}

	content, err := os.ReadFile(entry.FilePath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"entry":    entry,
		"content":  string(content),
		"can_edit": false,
	})
}

func (s *Server) handleMarkdownLike(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	markdownID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || markdownID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	if err := s.likeMarkdown(markdownID, userIDStr, time.Now()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	likeCount, likedByMe, _ := s.markdownLikeState(markdownID, userIDStr)
	c.JSON(http.StatusOK, gin.H{
		"message":      "已点赞",
		"like_count":   likeCount,
		"liked_by_me":  likedByMe,
	})
}

func (s *Server) handleMarkdownUnlike(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	markdownID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || markdownID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	if err := s.unlikeMarkdown(markdownID, userIDStr); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	likeCount, likedByMe, _ := s.markdownLikeState(markdownID, userIDStr)
	c.JSON(http.StatusOK, gin.H{
		"message":      "已取消点赞",
		"like_count":   likeCount,
		"liked_by_me":  likedByMe,
	})
}

func (s *Server) handleMarkdownReplyCreate(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	markdownID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || markdownID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	var req struct {
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	content := strings.TrimSpace(req.Content)
	if content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "评论不能为空"})
		return
	}

	replyID, err := s.createMarkdownReply(markdownID, userIDStr, content, time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "评论成功",
		"id":      replyID,
	})
}

func (s *Server) handleMarkdownReplyList(c *gin.Context) {
	markdownID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || markdownID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	limit := 0
	if limitStr := c.Query("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
			return
		}
		limit = parsed
	}

	offset := 0
	if offsetStr := c.Query("offset"); offsetStr != "" {
		parsed, err := strconv.Atoi(offsetStr)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
			return
		}
		offset = parsed
	}

	replies, hasMore, err := s.listMarkdownReplies(markdownID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	nextOffset := offset + len(replies)
	c.JSON(http.StatusOK, gin.H{
		"replies":     replies,
		"has_more":    hasMore,
		"next_offset": nextOffset,
	})
}

func (s *Server) handleMarkdownBookmark(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	markdownID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || markdownID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	if err := s.bookmarkMarkdown(markdownID, userIDStr, time.Now()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "已收藏", "bookmarked_by_me": true})
}

func (s *Server) handleMarkdownUnbookmark(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	markdownID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || markdownID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	if err := s.unbookmarkMarkdown(markdownID, userIDStr); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "已取消收藏", "bookmarked_by_me": false})
}
