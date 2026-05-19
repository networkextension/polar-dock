package dock

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleLLMConfigList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	items, err := s.listLLMConfigs(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"configs": items})
}

func (s *Server) handleLLMConfigTest(c *gin.Context) {
	var req struct {
		BaseURL      string `json:"base_url" binding:"required"`
		Model        string `json:"model" binding:"required"`
		APIKey       string `json:"api_key" binding:"required"`
		SystemPrompt string `json:"system_prompt"`
		ProxyURL     string `json:"proxy_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	if s.aiAgent == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "AI Agent 未初始化"})
		return
	}

	runtimeConfig := aiRuntimeConfig{
		APIKey:       strings.TrimSpace(req.APIKey),
		BaseURL:      strings.TrimSpace(req.BaseURL),
		Model:        strings.TrimSpace(req.Model),
		SystemPrompt: strings.TrimSpace(req.SystemPrompt),
		ProxyURL:     strings.TrimSpace(req.ProxyURL),
	}
	if err := s.aiAgent.testRuntimeConfig(runtimeConfig); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "连接成功，模型配置可用"})
}

func (s *Server) handleLLMConfigCreate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	var req struct {
		Name         string `json:"name" binding:"required"`
		BaseURL      string `json:"base_url" binding:"required"`
		Model        string `json:"model" binding:"required"`
		APIKey       string `json:"api_key"`
		SystemPrompt string `json:"system_prompt"`
		Shared       bool   `json:"shared"`
		IsSystem     bool   `json:"is_system"`
		Streaming    *bool  `json:"streaming,omitempty"`
		ProviderKind string `json:"provider_kind"`
		Extras       string `json:"extras"`
		ProxyURL     string `json:"proxy_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	name := strings.TrimSpace(req.Name)
	baseURL := strings.TrimSpace(req.BaseURL)
	model := strings.TrimSpace(req.Model)
	if name == "" || baseURL == "" || model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "名称、Base URL 和 Model 不能为空"})
		return
	}
	// is_system is the platform-wide system AI agent row; semantically
	// it's internal infra, not a marketplace offering. DB has a CHECK
	// constraint too — this is the friendly 400.
	if req.IsSystem && req.Shared {
		c.JSON(http.StatusBadRequest, gin.H{"error": "is_system 与 shared 互斥（系统助理是平台内部，不进客户市场）"})
		return
	}
	// Only platform admins may claim the singleton system slot. Per-
	// team owners can still create their own non-system configs.
	if req.IsSystem {
		if role, _ := c.Get("role"); role != "admin" {
			c.JSON(http.StatusForbidden, gin.H{"error": "仅管理员可设置系统助理（is_system）"})
			return
		}
	}
	now := time.Now()
	streaming := true
	if req.Streaming != nil {
		streaming = *req.Streaming
	}
	item, err := s.createLLMConfig(
		workspaceID,
		userIDStr,
		name,
		baseURL,
		model,
		strings.TrimSpace(req.APIKey),
		strings.TrimSpace(req.SystemPrompt),
		generateSessionID()[:24],
		strings.TrimSpace(req.ProxyURL),
		req.Shared,
		streaming,
		req.IsSystem,
		now,
	)
	if err != nil {
		// Partial-unique-index violation = someone else already holds
		// the singleton is_system slot. Translate to a clear 409.
		if strings.Contains(strings.ToLower(err.Error()), "uniq_llm_configs_one_system") {
			c.JSON(http.StatusConflict, gin.H{"error": "已存在系统级 LLM 配置（每平台仅一个）— 先关掉现有的再创建"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
		return
	}
	if kind := strings.TrimSpace(req.ProviderKind); kind != "" && kind != LLMConfigKindText {
		extras := strings.TrimSpace(req.Extras)
		if extras == "" {
			extras = "{}"
		}
		if err := s.upsertLLMConfigKindAndExtras(workspaceID, item.ID, kind, []byte(extras), now); err != nil {
			// Soft failure: the row exists, just without the kind/extras
			// override. Worth logging but don't fail the create.
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存 provider 类型失败"})
			return
		}
	}
	c.JSON(http.StatusCreated, gin.H{"config": item, "message": "配置已创建"})
}

func (s *Server) handleLLMConfigUpdate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效配置"})
		return
	}
	var req struct {
		Name         string `json:"name" binding:"required"`
		BaseURL      string `json:"base_url" binding:"required"`
		Model        string `json:"model" binding:"required"`
		APIKey       string `json:"api_key"`
		SystemPrompt string `json:"system_prompt"`
		Shared       bool   `json:"shared"`
		// IsSystem is *bool so absence (nil) means "don't touch" — same
		// reasoning as Streaming below. UI omits this on dashboard's
		// edit forms; we don't want to silently turn off is_system.
		IsSystem *bool `json:"is_system,omitempty"`
		// Streaming is *bool so absence (nil) means "don't touch" — the
		// dashboard UI form historically omitted this field on save,
		// which silently flipped streaming to false on every edit. With
		// *bool, omission preserves whatever the row already had.
		Streaming    *bool  `json:"streaming,omitempty"`
		UpdateAPIKey bool   `json:"update_api_key"`
		ProviderKind string `json:"provider_kind"`
		Extras       string `json:"extras"`
		ProxyURL     string `json:"proxy_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	name := strings.TrimSpace(req.Name)
	baseURL := strings.TrimSpace(req.BaseURL)
	model := strings.TrimSpace(req.Model)
	if name == "" || baseURL == "" || model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "名称、Base URL 和 Model 不能为空"})
		return
	}
	if req.IsSystem != nil && *req.IsSystem && req.Shared {
		c.JSON(http.StatusBadRequest, gin.H{"error": "is_system 与 shared 互斥（系统助理是平台内部，不进客户市场）"})
		return
	}
	// Toggling is_system (either direction) is admin-only. Customer
	// owners can edit name/baseURL/model on their own non-system rows
	// but cannot promote/demote the platform singleton.
	if req.IsSystem != nil {
		if role, _ := c.Get("role"); role != "admin" {
			c.JSON(http.StatusForbidden, gin.H{"error": "仅管理员可设置系统助理（is_system）"})
			return
		}
	}
	now := time.Now()
	item, err := s.updateLLMConfig(
		workspaceID, id,
		name,
		baseURL,
		model,
		strings.TrimSpace(req.APIKey),
		strings.TrimSpace(req.SystemPrompt),
		strings.TrimSpace(req.ProxyURL),
		req.Shared,
		req.Streaming,
		req.IsSystem,
		req.UpdateAPIKey,
		now,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "uniq_llm_configs_one_system") {
			c.JSON(http.StatusConflict, gin.H{"error": "已存在系统级 LLM 配置（每平台仅一个）— 先关掉现有的再启用"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
		return
	}
	if item == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在"})
		return
	}
	if kind := strings.TrimSpace(req.ProviderKind); kind != "" {
		extras := strings.TrimSpace(req.Extras)
		if extras == "" {
			extras = "{}"
		}
		if err := s.upsertLLMConfigKindAndExtras(workspaceID, item.ID, kind, []byte(extras), now); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存 provider 类型失败"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"config": item, "message": "配置已更新"})
}

func (s *Server) handleLLMConfigDelete(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效配置"})
		return
	}
	deleted, err := s.deleteLLMConfig(workspaceID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "配置已删除"})
}

func (s *Server) handleBotUserList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	if err := s.ensureBuiltinBotUsers(workspaceID, userIDStr, time.Now()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	items, err := s.listBotUsers(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"bots": items})
}

func (s *Server) handleBotUserCreate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	var req struct {
		Name          string `json:"name" binding:"required"`
		Description   string `json:"description"`
		SystemPrompt  string `json:"system_prompt"`
		LLMConfigID   int64  `json:"llm_config_id"`
		BotKind       string `json:"bot_kind"`       // "" → classic
		PreferredTool string `json:"preferred_tool"` // optional hint
		ModelOverride string `json:"model_override"` // qunliao β: empty = use cfg.model
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	name := strings.TrimSpace(req.Name)
	kind := strings.TrimSpace(req.BotKind)
	if kind == "" {
		kind = BotKindClassic
	}
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bot 名称不能为空"})
		return
	}
	if kind == BotKindClassic && req.LLMConfigID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Classic Bot 必须选择 LLM 配置"})
		return
	}
	item, err := s.createBotUser(CreateBotUserInput{
		WorkspaceID:   workspaceID,
		OwnerUserID:   userIDStr,
		Name:          name,
		Description:   strings.TrimSpace(req.Description),
		SystemPrompt:  strings.TrimSpace(req.SystemPrompt),
		LLMConfigID:   req.LLMConfigID,
		BotKind:       kind,
		PreferredTool: strings.TrimSpace(req.PreferredTool),
		ModelOverride: strings.TrimSpace(req.ModelOverride),
	}, time.Now())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "uniq_bot_users_workspace_name") {
			c.JSON(http.StatusConflict, gin.H{"error": "工作区内已有同名 Bot — 请换一个名字"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	if item == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "配置不存在或无权限"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"bot": item, "message": "Bot 已创建"})
}

func (s *Server) handleBotUserUpdate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效 Bot"})
		return
	}
	var req struct {
		Name          string `json:"name" binding:"required"`
		Description   string `json:"description"`
		SystemPrompt  string `json:"system_prompt"`
		LLMConfigID   int64  `json:"llm_config_id"`
		BotKind       string `json:"bot_kind"`
		PreferredTool string `json:"preferred_tool"`
		ModelOverride string `json:"model_override"`
		// HostSkillID (P1d): pointer so the FE can distinguish
		// "field omitted → don't touch" from "explicit null → unbind"
		// from "non-zero → bind". Validated server-side against the
		// workspace.
		HostSkillID *int64 `json:"host_skill_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	name := strings.TrimSpace(req.Name)
	kind := strings.TrimSpace(req.BotKind)
	if kind == "" {
		kind = BotKindClassic
	}
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Bot 名称不能为空"})
		return
	}
	if kind == BotKindClassic && req.LLMConfigID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Classic Bot 必须选择 LLM 配置"})
		return
	}
	input := UpdateBotUserInput{
		Name:          name,
		Description:   strings.TrimSpace(req.Description),
		SystemPrompt:  strings.TrimSpace(req.SystemPrompt),
		LLMConfigID:   req.LLMConfigID,
		BotKind:       kind,
		PreferredTool: strings.TrimSpace(req.PreferredTool),
		ModelOverride: strings.TrimSpace(req.ModelOverride),
	}
	if req.HostSkillID != nil {
		input.HostSkillIDSet = true
		input.HostSkillID = *req.HostSkillID
	}
	item, err := s.updateBotUser(workspaceID, id, input, time.Now())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "uniq_bot_users_workspace_name") {
			c.JSON(http.StatusConflict, gin.H{"error": "工作区内已有同名 Bot — 请换一个名字"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
		return
	}
	if item == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Bot 或配置不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"bot": item, "message": "Bot 已更新"})
}

func (s *Server) handleBotUserDelete(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效 Bot"})
		return
	}
	deleted, err := s.deleteBotUser(workspaceID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "Bot 不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Bot 已删除"})
}

func (s *Server) handleAvailableLLMConfigList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	items, err := s.listAvailableLLMConfigs(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"configs": items})
}

func (s *Server) handleLLMConfigGetByShareID(c *gin.Context) {
	shareID := c.Param("shareId")
	if shareID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的分享标识"})
		return
	}
	item, err := s.getLLMConfigByShareID(shareID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if item == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"config": item})
}
