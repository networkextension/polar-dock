package dock

// HTTP handlers for the research-mode dispatch pipeline. Three roles:
//
//   POST /api/projects/:id/tasks/:task_id/research/dispatch  (member)
//     Operator clicks "派研究任务" — creates a research_run row, opens
//     a chat/llm thread, pushes a research_task envelope to the bot's
//     attached polar-agent over WS, returns the LLM snapshot so the
//     UI can show the operator which model is doing the work.
//
//   POST /api/projects/research/runs/:run_id/start            (agent)
//     Agent ack — flips status queued→running, posts a system message
//     into the chat thread so users see "🔬 LLM: <name> (<model>)".
//
//   POST /api/projects/research/runs/:run_id/result           (agent)
//     Terminal — agent reports commit_sha + files_written + summary +
//     log; row finalizes succeeded/failed.
//
//   GET  /api/projects/:id/research/runs                      (member)
//     History tab.
//
// LLM transparency rule: 4 visible touchpoints — dispatch confirm
// (UI), thread system message (here), DB columns (history list),
// commit trailer (agent-side). All four must show the same llm_name +
// llm_model snapshot.

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type researchDispatchRequest struct {
	BotUserID   string `json:"bot_user_id" binding:"required"`
	LLMConfigID int64  `json:"llm_config_id"`
}

type researchLLMSnapshotResponse struct {
	ConfigID *int64 `json:"config_id,omitempty"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url,omitempty"`
}

type researchDispatchResponse struct {
	Run     *ResearchRun                `json:"research_run"`
	Bot     gin.H                       `json:"bot"`
	LLM     researchLLMSnapshotResponse `json:"llm"`
	Workdir string                      `json:"workdir,omitempty"`
}

func (s *Server) handleResearchDispatch(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userIDAny, _ := c.Get("user_id")
	userIDStr, _ := userIDAny.(string)
	if userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	project, err := s.getProject(projectID, workspaceID)
	if err != nil || project == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	if !IsResearchProject(project) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该项目不是研究项目，请用普通派单"})
		return
	}
	taskID := strings.TrimSpace(c.Param("task_id"))
	task, err := s.loadProjectTask(projectID, taskID)
	if err != nil || task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}

	var req researchDispatchRequest
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
	if bot == nil || bot.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "bot 不存在或不属于当前工作区"})
		return
	}
	if bot.BotKind != BotKindClassic {
		c.JSON(http.StatusBadRequest, gin.H{"error": "研究项目只接受 classic bot（passthrough/coder bot 不适合）"})
		return
	}
	if s.agentHub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent hub 未就绪"})
		return
	}
	conn := s.agentHub.lookup(botUserID)
	if conn == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "该 bot 的本地 polar-agent 还没连上。请先在终端 `polar-agent attach --bot=" + botUserID + "` 再点派单。",
			"code":  "agent_not_attached",
		})
		return
	}

	// LLM resolution: explicit picker > project default > bot's
	// bound LLM > workspace default. The project default is the
	// LLM the user picked at AI 拆解 time, persisted on the project
	// row. Skipping it would let the agent silently hop to the
	// workspace default which is rarely what the operator wants.
	if req.LLMConfigID <= 0 && project.DefaultLLMConfigID != nil && *project.DefaultLLMConfigID > 0 {
		req.LLMConfigID = *project.DefaultLLMConfigID
	}
	rt, _, llmConfig, rerr := s.resolveAssistBot(userIDStr, workspaceID, bot.ID, req.LLMConfigID)
	if rerr != nil {
		writeAssistError(c, rerr)
		return
	}
	if llmConfig == nil || strings.TrimSpace(rt.APIKey) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "工作区没有可用的 LLM 配置（设默认 LLM 或在 bot 上绑一个）"})
		return
	}

	now := time.Now()
	chatThread, err := s.ensureChatThread(userIDStr, botUserID, now)
	if err != nil {
		log.Printf("research dispatch: ensureChatThread project=%s task=%s bot=%s: %v", projectID, taskID, botUserID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	llmThread, err := s.createLLMThread(chatThread.ID, userIDStr, botUserID, "[Research] "+task.Title, now)
	if err != nil || llmThread == nil {
		log.Printf("research dispatch: createLLMThread project=%s task=%s bot=%s: %v", projectID, taskID, botUserID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "初始化任务话题失败"})
		return
	}

	taskIDPtr := &task.ID
	llmConfigIDPtr := &llmConfig.ID
	run := &ResearchRun{
		WorkspaceID:  workspaceID,
		ProjectID:    project.ID,
		TaskID:       taskIDPtr,
		BotUserID:    botUserID,
		ChatThreadID: &chatThread.ID,
		LLMThreadID:  &llmThread.ID,
		LLMConfigID:  llmConfigIDPtr,
		LLMName:      strings.TrimSpace(llmConfig.Name),
		LLMModel:     strings.TrimSpace(llmConfig.Model),
		LLMBaseURL:   strings.TrimSpace(llmConfig.BaseURL),
		Status:       ResearchRunStatusQueued,
	}
	if _, err := s.createResearchRun(run); err != nil {
		log.Printf("research dispatch: createResearchRun: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	s.metricsRegistry.recordResearchDispatch(ResearchRunStatusQueued)

	// Persist the wiring on the task so UI shows it as in-progress
	// just like normal pickup does.
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
		log.Printf("research dispatch: update task: %v", err)
	}

	taskContent := s.buildPickupBody(project, task.Title, task.Description)

	envelope := agentResearchTask{
		ResearchRunID: run.ID,
		ProjectID:     project.ID,
		TaskID:        task.ID,
		TaskTitle:     task.Title,
		ChatThreadID:  chatThread.ID,
		LLMThreadID:   llmThread.ID,
		Workdir:       project.ID,
		GitRemoteURL:  strings.TrimSpace(project.GitRemoteURL),
		TaskContent:   taskContent,
		LLM: agentResearchLLMSnapshot{
			ConfigID: llmConfig.ID,
			Name:     llmConfig.Name,
			Model:    llmConfig.Model,
			Provider: strings.TrimSpace(llmConfig.ProviderKind),
			BaseURL:  llmConfig.BaseURL,
			APIKey:   rt.APIKey,
			ProxyURL: strings.TrimSpace(llmConfig.ProxyURL),
		},
	}
	if err := s.agentHub.dispatchResearchTask(botUserID, envelope); err != nil {
		log.Printf("research dispatch: ws send: %v", err)
		// Mark the run failed so the operator sees the dispatch failure
		// instead of a permanently-queued ghost row.
		_ = s.finalizeResearchRun(run.ID, ResearchRunResult{
			OK:           false,
			ErrorMessage: "dispatch to agent failed: " + err.Error(),
		})
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "向 agent 发送任务失败：" + err.Error()})
		return
	}

	c.JSON(http.StatusOK, researchDispatchResponse{
		Run: run,
		Bot: gin.H{"id": bot.ID, "user_id": bot.BotUserID, "name": bot.Name},
		LLM: researchLLMSnapshotResponse{
			ConfigID: llmConfigIDPtr,
			Name:     llmConfig.Name,
			Model:    llmConfig.Model,
			BaseURL:  llmConfig.BaseURL,
		},
		Workdir: project.ID,
	})
}

// handleResearchRunStart is the agent ack endpoint. Authentication is
// the agent token (PolarAgentAuthMiddleware). The run id is passed in
// the path; we look up the row, flip it to running, and post a system
// message into the chat thread for transparency.
func (s *Server) handleResearchRunStart(c *gin.Context) {
	runID, err := strconv.ParseInt(c.Param("run_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	run, err := s.getResearchRun(runID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "load run failed"})
		return
	}
	if run == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "research run not found"})
		return
	}
	if run.Status == ResearchRunStatusSucceeded || run.Status == ResearchRunStatusFailed {
		c.JSON(http.StatusConflict, gin.H{"error": "run is already terminal"})
		return
	}
	if err := s.markResearchRunRunning(runID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update status failed"})
		return
	}
	s.metricsRegistry.recordResearchDispatch(ResearchRunStatusRunning)

	// Transparency: write a system-side chat message into the thread
	// announcing the LLM in use. The operator sees this whenever they
	// open the bot chat — independent of whether the dispatch UI was
	// visible at the time.
	if run.ChatThreadID != nil {
		body := "🔬 研究任务启动 · LLM: " + strings.TrimSpace(run.LLMName)
		if run.LLMModel != "" {
			body += " (" + strings.TrimSpace(run.LLMModel) + ")"
		}
		if _, sendErr := s.sendChatMessage(*run.ChatThreadID, run.LLMThreadID, run.BotUserID, run.LLMName, body, time.Now()); sendErr != nil {
			log.Printf("research start: post system message run=%d: %v", runID, sendErr)
		}
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type researchRunResultRequest struct {
	OK           bool     `json:"ok"`
	Iterations   int      `json:"iterations"`
	FilesWritten []string `json:"files_written"`
	CommitSHA    string   `json:"commit_sha"`
	Summary      string   `json:"summary"`
	ErrorMessage string   `json:"error_message"`
	LogText      string   `json:"log_text"`
}

func (s *Server) handleResearchRunResult(c *gin.Context) {
	runID, err := strconv.ParseInt(c.Param("run_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	run, err := s.getResearchRun(runID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "load run failed"})
		return
	}
	if run == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "research run not found"})
		return
	}
	if run.Status == ResearchRunStatusSucceeded || run.Status == ResearchRunStatusFailed {
		c.JSON(http.StatusConflict, gin.H{"error": "run is already terminal"})
		return
	}
	var req researchRunResultRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}
	finalErr := s.finalizeResearchRun(runID, ResearchRunResult{
		OK:           req.OK,
		Iterations:   req.Iterations,
		FilesWritten: req.FilesWritten,
		CommitSHA:    req.CommitSHA,
		Summary:      req.Summary,
		ErrorMessage: req.ErrorMessage,
		LogText:      req.LogText,
	})
	if finalErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "finalize failed"})
		return
	}
	if req.OK {
		s.metricsRegistry.recordResearchDispatch(ResearchRunStatusSucceeded)
	} else {
		s.metricsRegistry.recordResearchDispatch(ResearchRunStatusFailed)
	}

	// Optional: post a terminal status message back into chat for
	// visibility. Brief — long log goes to DB log_text, not chat.
	if run.ChatThreadID != nil {
		var body string
		if req.OK {
			body = "✅ 研究任务完成"
			if req.CommitSHA != "" {
				body += " · commit `" + shortSHA(req.CommitSHA) + "`"
			}
			if len(req.FilesWritten) > 0 {
				body += " · 写入 " + strconv.Itoa(len(req.FilesWritten)) + " 个文件"
			}
			if strings.TrimSpace(req.Summary) != "" {
				body += "\n\n" + strings.TrimSpace(req.Summary)
			}
		} else {
			body = "❌ 研究任务失败"
			if strings.TrimSpace(req.ErrorMessage) != "" {
				body += "：" + strings.TrimSpace(req.ErrorMessage)
			}
		}
		if _, sendErr := s.sendChatMessage(*run.ChatThreadID, run.LLMThreadID, run.BotUserID, run.LLMName, body, time.Now()); sendErr != nil {
			log.Printf("research result: post terminal message run=%d: %v", runID, sendErr)
		}
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleResearchRunsList(c *gin.Context) {
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
	limit := 20
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	runs, err := s.listResearchRunsForProject(project.ID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}

func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// errResearchAlreadyTerminal helps avoid the "magic string" anti-pattern
// in the conflict checks above. Internal use only.
var errResearchAlreadyTerminal = errors.New("research run already terminal")
