package dock

// HTTP handlers for system-agent endpoints. Currently:
//   POST /api/system-agent/bio/refine — capability A (bio rewrite)
//
// Each handler returns drafts/output for the user to review; nothing
// gets written to user-owned tables here. The user adopts the
// suggestion via the normal write path (e.g. PUT /users/me/profile
// for bio adoption). This avoids needing two write paths and keeps
// audit / events consistent.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type bioRefineRequest struct {
	CurrentBio string   `json:"current_bio"`
	Hints      []string `json:"hints"`
	Extra      string   `json:"extra"`
}

type bioRefineDraft struct {
	Label     string `json:"label"`
	Text      string `json:"text"`
	Rationale string `json:"rationale"`
}

type bioRefineResponse struct {
	Drafts []bioRefineDraft `json:"drafts"`
	Notes  string           `json:"notes"`
	LLM    bioRefineLLMTag  `json:"llm"`
}

type bioRefineLLMTag struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

type bioRefinePromptData struct {
	PlatformName string
}

func (s *Server) handleBioRefine(c *gin.Context) {
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

	var req bioRefineRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	runtimeConfig, llmName, err := s.resolveSystemAgentLLM(c.GetString("workspace_id"))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}

	siteName := "Polar"
	if site, err := s.getSiteSettings(); err == nil && site != nil && strings.TrimSpace(site.Name) != "" {
		siteName = site.Name
	}
	systemPrompt := s.renderPrompt(promptSlugBioRefine, bioRefinePromptData{PlatformName: siteName}, "")
	runtimeConfig.SystemPrompt = systemPrompt

	userPrompt := buildBioRefineUserPrompt(req)
	payload := aiChatCompletionRequest{
		Model: runtimeConfig.Model,
		Messages: []aiChatCompletionMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		MaxTokens: 800,
	}
	resp, err := s.aiAgent.requestChatCompletion(runtimeConfig, payload)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if len(resp.Choices) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "模型返回为空"})
		return
	}
	parsed, perr := parseBioRefineOutput(resp.Choices[0].Message.Content)
	if perr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "无法解析模型输出: " + perr.Error()})
		return
	}
	parsed.LLM = bioRefineLLMTag{Name: llmName, Model: runtimeConfig.Model}
	c.JSON(http.StatusOK, parsed)
}

// resolveSystemAgentLLM picks the runtime config the system agent
// uses. Priority:
//
//  1. Platform AI_AGENT_* env (the same config the "system" user
//     replies with) — preferred because the system agent is a
//     platform-level utility, not a workspace bot.
//  2. Workspace default LLM (PR #104's getWorkspaceDefaultLLMConfigID)
//     — fallback when AI_AGENT_* isn't configured.
//
// Returns the runtime config + a human-readable LLM name to
// surface in the response (4-touchpoint transparency rule).
func (s *Server) resolveSystemAgentLLM(workspaceID string) (aiRuntimeConfig, string, error) {
	if s.aiAgent != nil {
		apiKey := strings.TrimSpace(s.aiAgent.apiKey)
		baseURL := strings.TrimSpace(s.aiAgent.baseURL)
		model := strings.TrimSpace(s.aiAgent.model)
		if apiKey != "" && baseURL != "" && model != "" {
			return aiRuntimeConfig{
				APIKey:  apiKey,
				BaseURL: baseURL,
				Model:   model,
			}, "AI Agent (platform)", nil
		}
	}
	if strings.TrimSpace(workspaceID) == "" {
		return aiRuntimeConfig{}, "", errors.New("AI 助理未配置 — 请管理员设置 AI_AGENT_API_KEY / AI_AGENT_BASE_URL / AI_AGENT_MODEL，或在工作区设置默认 LLM")
	}
	fallbackID, err := s.getWorkspaceDefaultLLMConfigID(workspaceID)
	if err != nil {
		return aiRuntimeConfig{}, "", errors.New("加载工作区默认 LLM 失败")
	}
	if fallbackID == 0 {
		return aiRuntimeConfig{}, "", errors.New("工作区没有默认 LLM 可用")
	}
	cfg, apiKey, err := s.getAvailableLLMConfigWithAPIKey(workspaceID, fallbackID)
	if err != nil || cfg == nil || strings.TrimSpace(apiKey) == "" {
		return aiRuntimeConfig{}, "", errors.New("工作区默认 LLM 不可用")
	}
	return aiRuntimeConfig{
		APIKey:        strings.TrimSpace(apiKey),
		BaseURL:       strings.TrimSpace(cfg.BaseURL),
		Model:         strings.TrimSpace(cfg.Model),
		ProxyURL:      strings.TrimSpace(cfg.ProxyURL),
		LLMConfigID:   cfg.ID,
		LLMConfigName: cfg.Name,
	}, cfg.Name, nil
}

func buildBioRefineUserPrompt(req bioRefineRequest) string {
	var b strings.Builder
	current := strings.TrimSpace(req.CurrentBio)
	if current == "" {
		b.WriteString("当前自我介绍：（空）\n\n")
	} else {
		b.WriteString("当前自我介绍：\n")
		b.WriteString(current)
		b.WriteString("\n\n")
	}
	cleanedHints := make([]string, 0, len(req.Hints))
	for _, h := range req.Hints {
		h = strings.TrimSpace(h)
		if h != "" {
			cleanedHints = append(cleanedHints, h)
		}
	}
	if len(cleanedHints) > 0 {
		b.WriteString("期望方向：")
		b.WriteString(strings.Join(cleanedHints, "、"))
		b.WriteString("\n\n")
	}
	if extra := strings.TrimSpace(req.Extra); extra != "" {
		b.WriteString("额外说明：\n")
		b.WriteString(extra)
		b.WriteString("\n")
	}
	return b.String()
}

// ── Capability B: project task sharpen ────────────────────────────

type taskSharpenRequest struct {
	Instruction string `json:"instruction"` // optional user note
}

type taskSharpenDraft struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Rationale   string `json:"rationale"`
}

type taskSharpenResponse struct {
	Drafts   []taskSharpenDraft `json:"drafts"`
	Notes    string             `json:"notes"`
	Original taskSharpenDraft   `json:"original"` // pre-rewrite snapshot
	LLM      bioRefineLLMTag    `json:"llm"`      // reuse the simple struct
}

type taskSharpenPromptData struct {
	PlatformName string
	Industry     string
	RuntimeEnv   string
	Scenario     string
}

func (s *Server) handleProjectTaskSharpen(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	projectID := strings.TrimSpace(c.Param("id"))
	taskID := strings.TrimSpace(c.Param("task_id"))

	project, err := s.getProject(projectID, workspaceID)
	if err != nil || project == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	task, err := s.loadProjectTask(projectID, taskID)
	if err != nil || task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}

	var req taskSharpenRequest
	_ = c.ShouldBindJSON(&req) // body is optional

	runtimeConfig, llmName, err := s.resolveSystemAgentLLM(workspaceID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}

	siteName := "Polar"
	if site, err := s.getSiteSettings(); err == nil && site != nil && strings.TrimSpace(site.Name) != "" {
		siteName = site.Name
	}
	industry := s.getUserIndustry(userIDStr)
	envLabel := runtimeEnvLabel(project.RuntimeEnv)
	scenario := businessScenarioLabel(project.BusinessScenario)
	systemPrompt := s.renderPrompt(promptSlugTaskSharpen, taskSharpenPromptData{
		PlatformName: siteName,
		Industry:     industry,
		RuntimeEnv:   envLabel,
		Scenario:     scenario,
	}, "")
	runtimeConfig.SystemPrompt = systemPrompt

	userPrompt := buildTaskSharpenUserPrompt(task.Title, task.Description, req.Instruction)
	payload := aiChatCompletionRequest{
		Model: runtimeConfig.Model,
		Messages: []aiChatCompletionMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		MaxTokens: 1200,
	}
	resp, err := s.aiAgent.requestChatCompletion(runtimeConfig, payload)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if len(resp.Choices) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "模型返回为空"})
		return
	}
	parsed, perr := parseTaskSharpenOutput(resp.Choices[0].Message.Content)
	if perr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "无法解析模型输出: " + perr.Error()})
		return
	}
	parsed.Original = taskSharpenDraft{Title: task.Title, Description: task.Description}
	parsed.LLM = bioRefineLLMTag{Name: llmName, Model: runtimeConfig.Model}
	c.JSON(http.StatusOK, parsed)
}

func buildTaskSharpenUserPrompt(title, description, instruction string) string {
	var b strings.Builder
	b.WriteString("当前任务：\n\n标题：")
	b.WriteString(strings.TrimSpace(title))
	b.WriteString("\n\n描述：\n")
	d := strings.TrimSpace(description)
	if d == "" {
		b.WriteString("（空 — 用户尚未填写）")
	} else {
		b.WriteString(d)
	}
	if extra := strings.TrimSpace(instruction); extra != "" {
		b.WriteString("\n\n用户额外要求：\n")
		b.WriteString(extra)
	}
	return b.String()
}

func parseTaskSharpenOutput(raw string) (*taskSharpenResponse, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "```") {
		if idx := strings.Index(trimmed, "\n"); idx >= 0 {
			trimmed = trimmed[idx+1:]
		}
		trimmed = strings.TrimSuffix(strings.TrimSpace(trimmed), "```")
	}
	first := strings.Index(trimmed, "{")
	last := strings.LastIndex(trimmed, "}")
	if first < 0 || last <= first {
		return nil, fmt.Errorf("no JSON object in model output")
	}
	jsonChunk := trimmed[first : last+1]
	var parsed taskSharpenResponse
	if err := json.Unmarshal([]byte(jsonChunk), &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Drafts) == 0 {
		return nil, fmt.Errorf("model returned no drafts")
	}
	clean := make([]taskSharpenDraft, 0, len(parsed.Drafts))
	for _, d := range parsed.Drafts {
		d.Title = strings.TrimSpace(d.Title)
		d.Description = strings.TrimSpace(d.Description)
		d.Rationale = strings.TrimSpace(d.Rationale)
		// drafts must at least update title or description; reject
		// any "empty" suggestion the model emits.
		if d.Title == "" && d.Description == "" {
			continue
		}
		clean = append(clean, d)
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("model returned drafts with empty fields")
	}
	parsed.Drafts = clean
	parsed.Notes = strings.TrimSpace(parsed.Notes)
	return &parsed, nil
}

// parseBioRefineOutput is forgiving: tolerates ```json ... ``` fences
// or stray prose around the JSON object. Validates the schema enough
// to reject empty drafts.
func parseBioRefineOutput(raw string) (*bioRefineResponse, error) {
	trimmed := strings.TrimSpace(raw)
	// Strip a leading/trailing markdown code fence if present.
	if strings.HasPrefix(trimmed, "```") {
		if idx := strings.Index(trimmed, "\n"); idx >= 0 {
			trimmed = trimmed[idx+1:]
		}
		trimmed = strings.TrimSuffix(strings.TrimSpace(trimmed), "```")
	}
	// Find the first '{' and last '}'; skip any preface/postscript
	// the model snuck in despite our "no markdown" instruction.
	first := strings.Index(trimmed, "{")
	last := strings.LastIndex(trimmed, "}")
	if first < 0 || last <= first {
		return nil, fmt.Errorf("no JSON object in model output")
	}
	jsonChunk := trimmed[first : last+1]
	var parsed bioRefineResponse
	if err := json.Unmarshal([]byte(jsonChunk), &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Drafts) == 0 {
		return nil, fmt.Errorf("model returned no drafts")
	}
	// Trim whitespace + drop empty texts.
	clean := make([]bioRefineDraft, 0, len(parsed.Drafts))
	for _, d := range parsed.Drafts {
		d.Label = strings.TrimSpace(d.Label)
		d.Text = strings.TrimSpace(d.Text)
		d.Rationale = strings.TrimSpace(d.Rationale)
		if d.Text != "" {
			clean = append(clean, d)
		}
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("model returned drafts with empty text")
	}
	parsed.Drafts = clean
	parsed.Notes = strings.TrimSpace(parsed.Notes)
	return &parsed, nil
}
