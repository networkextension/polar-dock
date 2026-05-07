package dock

// HTTP handlers for the projects/features/tasks pipeline. All routes
// are workspace-scoped (X-Workspace-Id from AuthMiddleware).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const aiDecomposeTimeout = 90 * time.Second

// ---- project CRUD --------------------------------------------------

type projectCreateRequest struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	Template         string `json:"template"`
	RuntimeEnv       string `json:"runtime_env"`
	BusinessScenario string `json:"business_scenario"`
	CodeProvider     string `json:"code_provider"`
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
		WorkspaceID:      workspaceID,
		CreatorUserID:    userID,
		Name:             name,
		Description:      strings.TrimSpace(req.Description),
		Template:         strings.TrimSpace(req.Template),
		RuntimeEnv:       strings.TrimSpace(req.RuntimeEnv),
		BusinessScenario: strings.TrimSpace(req.BusinessScenario),
		CodeProvider:     strings.TrimSpace(req.CodeProvider),
		Status:           ProjectStatusDraft,
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
	Name             *string `json:"name"`
	Description      *string `json:"description"`
	Status           *string `json:"status"`
	Template         *string `json:"template"`
	RuntimeEnv       *string `json:"runtime_env"`
	BusinessScenario *string `json:"business_scenario"`
	CodeProvider     *string `json:"code_provider"`
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
	u := ProjectMetaUpdate{}
	if req.Name != nil {
		v := strings.TrimSpace(*req.Name)
		if v != "" {
			u.Name = &v
		}
	}
	if req.Description != nil {
		v := strings.TrimSpace(*req.Description)
		u.Description = &v
	}
	if req.Status != nil {
		v := strings.TrimSpace(*req.Status)
		if v != "" {
			u.Status = &v
		}
	}
	if req.Template != nil {
		v := strings.TrimSpace(*req.Template)
		u.Template = &v
	}
	if req.RuntimeEnv != nil {
		v := strings.TrimSpace(*req.RuntimeEnv)
		u.RuntimeEnv = &v
	}
	if req.BusinessScenario != nil {
		v := strings.TrimSpace(*req.BusinessScenario)
		u.BusinessScenario = &v
	}
	if req.CodeProvider != nil {
		v := strings.TrimSpace(*req.CodeProvider)
		u.CodeProvider = &v
	}
	if err := s.updateProjectMetadata(id, workspaceID, u); err != nil {
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

// projectDecomposeBasePrompt is the constant top of the system
// prompt — JSON shape, complexity definitions, ordering rules.
// Per-project context (runtime env + business scenario) is
// inserted between this header and the closing instruction by
// buildDecomposePrompt.
const projectDecomposeBasePrompt = `你是一个产品经理助手。用户会提供一个产品想法（自然语言），你的任务是把它拆解成 5-12 个独立的功能（feature）。每个功能要可以单独实现、单独测试。

返回严格的 JSON 数组（不要 markdown 代码块、不要解释），每项包含：
- "title": 简短标题（中文，不超过 20 字）
- "description": 1-2 句话描述这个功能要做什么、用户会怎么用
- "complexity": "simple" | "medium" | "complex"，参考标准：simple = 单视图 + 静态数据；medium = 多视图 + 状态管理；complex = 涉及自定义动画、网络、持久化、设备能力

按用户预期的开发顺序排列功能（先做基础结构，再加内容，最后做动画 / 收尾）。`

const projectDecomposeClosingPrompt = `只返回 JSON 数组，不要任何其他文字。`

// runtimeEnvLabel returns a human-readable description of the
// runtime_env code stored on the project. Empty input → empty
// output (no constraint added to the prompt).
func runtimeEnvLabel(code string) string {
	switch strings.TrimSpace(code) {
	case "ios_swiftui":
		return "iOS native (SwiftUI) — 所有功能必须是单 app bundle 内可实现的，不假设有 backend API；资源用 bundle 静态文件"
	case "ios_uikit":
		return "iOS native (UIKit) — 同上但 UI 用 UIKit"
	case "macos_native":
		return "macOS native (SwiftUI / AppKit) — 桌面应用，可用本地文件系统"
	case "web_static":
		return "Web 静态页 (HTML + CSS + 原生 JS) — 没有后端，状态用 localStorage / IndexedDB"
	case "web_react":
		return "Web React 单页应用 — 用 Vite + React + TypeScript；没有后端假设"
	case "web_nextjs":
		return "Web Next.js 应用 — 可用 API routes 做轻量后端"
	case "cli_tool":
		return "命令行工具 (Python / Go / Bash 脚本) — 单文件可执行优先"
	case "chrome_ext":
		return "Chrome 扩展 (manifest v3) — 内容脚本 + 弹窗"
	}
	return ""
}

// businessScenarioLabel maps the curated dropdown code to a
// natural-language hint for the decompose prompt.
func businessScenarioLabel(code string) string {
	switch strings.TrimSpace(code) {
	case "creative_game":
		return "创意小游戏 — 关注核心交互手势、关卡 / 关进阶、动画反馈"
	case "content_app":
		return "文学 / 历史 / 地理 / 教育内容应用 — 关注内容展示、检索、收藏、阅读体验"
	case "productivity":
		return "生产力 / 工具 — 关注核心任务流、键盘快捷键、稳定性"
	case "social":
		return "社交 / 互动 — 关注好友 / 互动 / 通知 / 分享"
	case "data_viz":
		return "数据可视化 — 关注图表类型、交互筛选、性能"
	case "commerce":
		return "商业 / 交易 — 关注商品展示、下单、支付链路（注意：本平台 MVP 不接真实支付，用 mock）"
	case "media":
		return "媒体 / 内容消费 — 关注播放、订阅、推荐"
	}
	return ""
}

// buildPickupBody composes the first chat message we send the bot
// when a task is picked up. It prepends a constraint header
// derived from the project's runtime_env + business_scenario so
// the downstream coding tool stays inside the right stack/context
// instead of free-styling. Empty constraints are skipped (no
// header at all if project has neither set).
func buildPickupBody(project *Project, title, description string) string {
	header := []string{}
	if env := runtimeEnvLabel(project.RuntimeEnv); env != "" {
		header = append(header, "运行环境："+env)
	}
	if scen := businessScenarioLabel(project.BusinessScenario); scen != "" {
		header = append(header, "业务场景："+scen)
	}
	if project.CodeProvider != "" {
		// Soft signal — the tool may or may not be the one actually
		// running, but echoing the user's preference here primes the
		// agent's tone (e.g. claude-style explanations vs codex
		// brevity).
		header = append(header, "首选编码工具："+project.CodeProvider)
	}
	body := strings.TrimSpace(title)
	if len(header) > 0 {
		body = "【任务约束】\n" + strings.Join(header, "\n") + "\n\n【任务】\n" + body
	}
	if d := strings.TrimSpace(description); d != "" {
		body += "\n\n" + d
	}
	return body
}

// buildDecomposePrompt assembles the per-project system prompt by
// stitching the base prompt + any non-empty runtime_env /
// business_scenario hints + the closing instruction. When neither
// constraint is set, fall back to the legacy iOS-flavored hint
// so existing behavior is preserved for projects created before
// this PR.
func buildDecomposePrompt(p *Project) string {
	parts := []string{projectDecomposeBasePrompt}
	if env := runtimeEnvLabel(p.RuntimeEnv); env != "" {
		parts = append(parts, "运行环境约束：\n  "+env+"\n所有 feature 必须在这个环境下可实现；不假设其他平台能力。")
	}
	if scen := businessScenarioLabel(p.BusinessScenario); scen != "" {
		parts = append(parts, "业务场景约束：\n  "+scen+"\n拆解时聚焦此场景的核心交互。")
	}
	if p.RuntimeEnv == "" && p.BusinessScenario == "" {
		// Legacy fallback — keep the old hint so projects created
		// before this PR still get coherent output.
		parts = append(parts, `对于「创意小游戏 / 文学+地理+历史 + 图 + 动画」类应用，重点关注表现层：UI 屏幕、内容展示方式、交互手势、动画效果。不要假设有后端 API（默认资源是 bundled 静态文件）。`)
	}
	parts = append(parts, projectDecomposeClosingPrompt)
	return strings.Join(parts, "\n\n")
}

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

	// Optional explicit LLM pick (for A/B comparison: same project,
	// different LLM each click). Body is JSON; missing/0 = auto-pick
	// the first config with an API key.
	var req struct {
		LLMConfigID int64 `json:"llm_config_id"`
	}
	_ = c.ShouldBindJSON(&req)

	configs, err := s.listAvailableLLMConfigs(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	var chosen *LLMConfig
	if req.LLMConfigID > 0 {
		for i := range configs {
			if configs[i].ID == req.LLMConfigID {
				chosen = &configs[i]
				break
			}
		}
		if chosen == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "选定的 LLM 配置在此工作区不可用"})
			return
		}
		if !chosen.HasAPIKey {
			c.JSON(http.StatusBadRequest, gin.H{"error": "选定的 LLM 配置没有 API Key"})
			return
		}
	} else {
		// Auto-pick: platform default (is_platform=TRUE) sorts first
		// in listAvailableLLMConfigs, so configs[0] when present.
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
		SystemPrompt: buildDecomposePrompt(project),
		ProxyURL:     cfg.ProxyURL,
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
		st := ProjectStatusPlanning
		_ = s.updateProjectMetadata(project.ID, workspaceID, ProjectMetaUpdate{Status: &st})
	}
	// Include the LLM that actually produced the result so the FE
	// can display "拆解 by claude-3.5-sonnet · 12s" — handy when
	// re-decomposing across providers for A/B comparison.
	c.JSON(http.StatusOK, gin.H{
		"features": features,
		"llm_used": gin.H{
			"id":    chosen.ID,
			"name":  chosen.Name,
			"model": chosen.Model,
		},
	})
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
	project, err := s.getProject(projectID, workspaceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if project == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
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

	// Guard against the most common foot-gun: pickup before
	// polar-agent has reconnected. If no agent is attached, the
	// platform falls through to the bot's configured LLM, which
	// usually isn't what users want here (the LLM may be unset,
	// have a wrong base URL, or be a reachable-but-paid model).
	// Cheaper to refuse with a clear message than to bill a
	// confusing 400/500 from the LLM API.
	if s.agentHub == nil || s.agentHub.lookup(botUserID) == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "该 bot 的本地 polar-agent 还没连上。请先在终端 `polar-agent attach --bot=" + botUserID + " --kimi` 再点派单。",
			"code":  "agent_not_attached",
		})
		return
	}

	now := time.Now()
	chatThread, err := s.ensureChatThread(userIDStr, botUserID, now)
	if err != nil {
		log.Printf("pickup: ensureChatThread project=%s task=%s bot=%s: %v", projectID, taskID, botUserID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	llmThread, err := s.createLLMThread(chatThread.ID, userIDStr, botUserID, "[Task] "+task.Title, now)
	if err != nil {
		log.Printf("pickup: createLLMThread project=%s task=%s bot=%s: %v", projectID, taskID, botUserID, err)
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

	// First message body: title + project constraints (runtime env
	// + business scenario) + description. The constraint header is
	// what stops the local coding tool from picking the wrong stack
	// — without it, kimi/claude/codex tend to default to whatever
	// they're freshest on (often Web React when the project is
	// supposed to be SwiftUI).
	body := buildPickupBody(project, task.Title, task.Description)
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
			// Per-project subdir under the agent's pinned workdir
			// (e.g. ~/work/myrepo/<project_id>/) so two concurrent
			// projects on one agent don't fight over the same files.
			WorkdirSubpath: project.ID,
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
