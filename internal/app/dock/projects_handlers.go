package dock

// HTTP handlers for the projects/features/tasks pipeline. All routes
// are workspace-scoped (X-Workspace-Id from AuthMiddleware).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const aiDecomposeTimeout = 90 * time.Second

// ---- project CRUD --------------------------------------------------

type projectCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Template    string `json:"template"`
}

func (s *Server) handleProjectCreate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	var req projectCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 120 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "项目名称不能为空且不超过 120 字"})
		return
	}
	p := &Project{
		WorkspaceID:   workspaceID,
		CreatorUserID: userID,
		Name:          name,
		Description:   strings.TrimSpace(req.Description),
		Template:      strings.TrimSpace(req.Template),
		Status:        ProjectStatusDraft,
	}
	if err := s.createProject(p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"project": p})
}

func (s *Server) handleProjectList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	projects, err := s.listProjects(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

func (s *Server) handleProjectDetail(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	project, err := s.getProject(id, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if project == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	features, err := s.listProjectFeatures(project.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	tasks, err := s.listProjectTasks(project.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"project": project, "features": features, "tasks": tasks})
}

type projectUpdateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Template    string `json:"template"`
}

func (s *Server) handleProjectUpdate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	existing, err := s.getProject(id, workspaceID)
	if err != nil || existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	var req projectUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = existing.Name
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = existing.Status
	}
	if err := s.updateProjectMetadata(id, workspaceID, name, strings.TrimSpace(req.Description), status, strings.TrimSpace(req.Template)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	updated, _ := s.getProject(id, workspaceID)
	c.JSON(http.StatusOK, gin.H{"project": updated})
}

func (s *Server) handleProjectDelete(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if err := s.deleteProject(id, workspaceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- decompose -----------------------------------------------------

const projectDecomposeSystemPrompt = `你是一个 iOS 应用产品经理助手。用户会提供一个产品想法（自然语言），你的任务是把它拆解成 5-12 个独立的功能（feature）。每个功能要可以单独实现、单独测试。

返回严格的 JSON 数组（不要 markdown 代码块、不要解释），每项包含：
- "title": 简短标题（中文，不超过 20 字）
- "description": 1-2 句话描述这个功能要做什么、用户会怎么用
- "complexity": "simple" | "medium" | "complex"，参考标准：simple = 一两个 SwiftUI view + 静态数据；medium = 多 view + 一些状态管理；complex = 涉及自定义动画、网络、持久化、设备能力

对于「创意小游戏 / 文学+地理+历史 + 图 + 动画」类应用，重点关注表现层：UI 屏幕、内容展示方式、交互手势、动画效果。不要假设有后端 API（默认资源是 bundled 静态文件）。

按用户预期的开发顺序排列功能（先做基础结构，再加内容，最后做动画 / 收尾）。

只返回 JSON 数组，不要任何其他文字。`

type decomposedFeature struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Complexity  string `json:"complexity"`
}

func (s *Server) handleProjectDecompose(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	project, err := s.getProject(id, workspaceID)
	if err != nil || project == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	if strings.TrimSpace(project.Description) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先填写项目描述（产品想法）"})
		return
	}
	if s.aiAgent == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "AI Agent 未初始化"})
		return
	}

	// Pick a text-kind LLM available to this workspace. Platform
	// default (is_platform=TRUE) sorts first in listAvailableLLMConfigs,
	// so the natural choice is configs[0].
	configs, err := s.listAvailableLLMConfigs(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	var chosen *LLMConfig
	for i := range configs {
		if configs[i].HasAPIKey {
			chosen = &configs[i]
			break
		}
	}
	if chosen == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可用的 LLM 配置；请联系管理员开通平台默认 LLM 或自行添加配置"})
		return
	}
	cfg, apiKey, err := s.getAvailableLLMConfigWithAPIKey(workspaceID, chosen.ID)
	if err != nil || cfg == nil || strings.TrimSpace(apiKey) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "选定的 LLM 配置无 API Key"})
		return
	}

	runtimeConfig := aiRuntimeConfig{
		APIKey:       apiKey,
		BaseURL:      cfg.BaseURL,
		Model:        cfg.Model,
		SystemPrompt: projectDecomposeSystemPrompt,
	}
	payload := aiChatCompletionRequest{
		Model: runtimeConfig.Model,
		Messages: []aiChatCompletionMessage{
			{Role: "system", Content: runtimeConfig.SystemPrompt},
			{Role: "user", Content: "项目名称：" + project.Name + "\n\n产品描述：\n" + project.Description},
		},
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), aiDecomposeTimeout)
	defer cancel()
	_ = ctx // requestChatCompletion uses its own deadline; ctx kept for future streaming
	result, err := s.aiAgent.requestChatCompletion(runtimeConfig, payload)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "调用 LLM 失败：" + err.Error()})
		return
	}
	if len(result.Choices) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "LLM 返回为空"})
		return
	}
	raw := strings.TrimSpace(result.Choices[0].Message.Content)
	// Some models still wrap JSON in code fences despite the system
	// prompt. Strip those to be tolerant.
	raw = stripCodeFences(raw)

	var decomposed []decomposedFeature
	if err := json.Unmarshal([]byte(raw), &decomposed); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   "LLM 返回不是合法 JSON 数组：" + err.Error(),
			"raw":     raw,
			"hint":    "可能是模型对 prompt 理解不到位；可改写描述后重试",
		})
		return
	}
	if len(decomposed) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "LLM 没有拆出任何功能"})
		return
	}

	features := make([]ProjectFeature, 0, len(decomposed))
	for _, d := range decomposed {
		title := strings.TrimSpace(d.Title)
		if title == "" {
			continue
		}
		complexity := strings.TrimSpace(d.Complexity)
		switch complexity {
		case ProjectFeatureComplexitySimple, ProjectFeatureComplexityMedium, ProjectFeatureComplexityComplex:
		default:
			complexity = ProjectFeatureComplexityMedium
		}
		features = append(features, ProjectFeature{
			Title:       title,
			Description: strings.TrimSpace(d.Description),
			Complexity:  complexity,
			Selected:    true, // default pre-checked; user un-checks unwanted ones
		})
	}
	if err := s.replaceProjectFeatures(project.ID, features); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Bump status to planning (unless we're already past it)
	if project.Status == ProjectStatusDraft {
		_ = s.updateProjectMetadata(project.ID, workspaceID, project.Name, project.Description, ProjectStatusPlanning, project.Template)
	}
	c.JSON(http.StatusOK, gin.H{"features": features})
}

// stripCodeFences removes wrapping ```json ... ``` fences that some
// models add. Tolerant: if the input doesn't have fences it's
// returned as-is.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// Drop everything up to the first newline (the ``` or ```json line)
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		}
	}
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}

// ---- features ------------------------------------------------------

type projectFeatureUpdateRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Complexity  string `json:"complexity"`
	Selected    bool   `json:"selected"`
}

func (s *Server) handleProjectFeatureUpdate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if _, err := s.getProject(projectID, workspaceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	featureID := strings.TrimSpace(c.Param("feature_id"))
	var req projectFeatureUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	complexity := strings.TrimSpace(req.Complexity)
	switch complexity {
	case ProjectFeatureComplexitySimple, ProjectFeatureComplexityMedium, ProjectFeatureComplexityComplex:
	default:
		complexity = ProjectFeatureComplexityMedium
	}
	if err := s.updateProjectFeature(projectID, featureID, strings.TrimSpace(req.Title), strings.TrimSpace(req.Description), complexity, req.Selected); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "功能不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- plan generation -----------------------------------------------

func (s *Server) handleProjectGeneratePlan(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	project, err := s.getProject(projectID, workspaceID)
	if err != nil || project == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	tasks, err := s.generatePlanFromSelectedFeatures(projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks, "created_count": len(tasks)})
}

// ---- task pickup (assign to bot) ----------------------------------

type projectTaskPickupRequest struct {
	BotUserID string `json:"bot_user_id" binding:"required"`
}

type projectTaskPickupResponse struct {
	Task         ProjectTask `json:"task"`
	ChatThreadID int64       `json:"chat_thread_id"`
	LLMThreadID  int64       `json:"llm_thread_id"`
	BotName      string      `json:"bot_name"`
}

// handleProjectTaskPickup wires a project task to a bot user. It
// creates (or reuses) the user↔bot chat thread, opens a fresh
// llm_thread titled after the task, writes the task description as
// the user's first message, and enqueues an aiAgentTask so the bot
// (or its attached polar-agent) starts responding immediately.
//
// Per 用户 ("1:1 task↔chat thread mapping"): each task gets its own
// llm_thread inside the shared (user, bot) chat thread. kimi-cli
// passthrough mode rides its own --continue session keyed on
// workdir, so cross-task continuity falls out for free.
func (s *Server) handleProjectTaskPickup(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	if userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if _, err := s.getProject(projectID, workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	taskID := strings.TrimSpace(c.Param("task_id"))
	task, err := s.loadProjectTask(projectID, taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	var req projectTaskPickupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	botUserID := strings.TrimSpace(req.BotUserID)
	if !strings.HasPrefix(botUserID, "bot_") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bot_user_id 必须是一个 bot 用户"})
		return
	}
	bot, err := s.getBotUserByUserID(botUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if bot == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "bot 不存在"})
		return
	}
	if bot.WorkspaceID != workspaceID {
		c.JSON(http.StatusForbidden, gin.H{"error": "该 bot 不属于当前工作区"})
		return
	}

	now := time.Now()
	chatThread, err := s.ensureChatThread(userIDStr, botUserID, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	llmThread, err := s.createLLMThread(chatThread.ID, userIDStr, botUserID, "[Task] "+task.Title, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if llmThread == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "初始化任务话题失败"})
		return
	}

	// Persist the wiring on the task before sending the message —
	// avoids a stale task state if the message send fails.
	if _, err := s.db.Exec(
		`UPDATE project_tasks
		 SET bot_user_id = $1,
		     chat_thread_id = $2,
		     llm_thread_id = $3,
		     status = CASE WHEN status IN ($4, $5) THEN $6 ELSE status END,
		     started_at = COALESCE(started_at, $7),
		     updated_at = $7
		 WHERE id = $8 AND project_id = $9`,
		botUserID, chatThread.ID, llmThread.ID,
		ProjectTaskStatusTodo, ProjectTaskStatusBlocked,
		ProjectTaskStatusInProgress,
		now, taskID, projectID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	// First message body: title + description so the bot has full
	// context. Don't bother with extra prompt scaffolding — the
	// system prompt is already on the bot, and kimi-cli passthrough
	// would skip it anyway.
	body := task.Title
	if strings.TrimSpace(task.Description) != "" {
		body += "\n\n" + task.Description
	}
	username, _ := c.Get("username")
	senderName, _ := username.(string)
	llmThreadIDPtr := &llmThread.ID
	if _, err := s.sendChatMessage(chatThread.ID, llmThreadIDPtr, userIDStr, senderName, body, now); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "消息发送失败"})
		return
	}
	if s.aiAgent != nil {
		s.aiAgent.enqueue(aiAgentTask{
			ThreadID:        chatThread.ID,
			LLMThreadID:     llmThreadIDPtr,
			UserID:          userIDStr,
			ResponderUserID: botUserID,
			ResponderName:   bot.Name,
			Content:         body,
		})
	}

	updated, err := s.loadProjectTask(projectID, taskID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, projectTaskPickupResponse{
		Task:         *updated,
		ChatThreadID: chatThread.ID,
		LLMThreadID:  llmThread.ID,
		BotName:      bot.Name,
	})
}

// ---- task update ---------------------------------------------------

type projectTaskUpdateRequest struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Status      *string `json:"status"`
}

func (s *Server) handleProjectTaskUpdate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if _, err := s.getProject(projectID, workspaceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	taskID := strings.TrimSpace(c.Param("task_id"))
	var req projectTaskUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	u := ProjectTaskUpdate{}
	if req.Title != nil {
		t := strings.TrimSpace(*req.Title)
		u.Title = &t
	}
	if req.Description != nil {
		d := strings.TrimSpace(*req.Description)
		u.Description = &d
	}
	if req.Status != nil {
		st := strings.TrimSpace(*req.Status)
		switch st {
		case ProjectTaskStatusTodo, ProjectTaskStatusInProgress, ProjectTaskStatusReview, ProjectTaskStatusDone, ProjectTaskStatusBlocked:
			u.Status = &st
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "状态值无效"})
			return
		}
	}
	if err := s.updateProjectTask(projectID, taskID, u); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
