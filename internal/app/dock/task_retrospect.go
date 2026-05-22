package dock

// System-agent task retrospect (capability C from doc/agent/system-agent-design.md).
// One row per project_task; system agent emits a four-section post-
// mortem (summary / tools used / open issues / next time) when the
// task flips to done — also via manual POST .../retrospect.
//
// LLM resolution piggybacks on resolveSystemAgentLLM (PR #133) so
// the same platform AI_AGENT_* → workspace default fallback chain
// applies. 4-touchpoint transparency: llm_name + llm_model snapshot
// columns + UI rendering tag.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// retrospectAutoTimeout caps the auto-fired (status→done) goroutine
// so a hung LLM call doesn't pile up. Manual POSTs use the request
// context and inherit gin's timeout.
const retrospectAutoTimeout = 90 * time.Second

type ProjectTaskRetrospect struct {
	ID          int64      `json:"id"`
	TaskID      string     `json:"task_id"`
	Summary     string     `json:"summary"`
	ToolsUsed   string     `json:"tools_used"`
	OpenIssues  string     `json:"open_issues"`
	NextTime    string     `json:"next_time"`
	LLMName     string     `json:"llm_name"`
	LLMModel    string     `json:"llm_model"`
	GeneratedAt *time.Time `json:"generated_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type taskRetrospectPromptData struct {
	PlatformName string
	Industry     string
	RuntimeEnv   string
	Scenario     string
}

// rawTaskRetrospect is the strict-JSON shape we expect from the
// LLM. Mapped to ProjectTaskRetrospect after parse.
type rawTaskRetrospect struct {
	Summary    string `json:"summary"`
	ToolsUsed  string `json:"tools_used"`
	OpenIssues string `json:"open_issues"`
	NextTime   string `json:"next_time"`
}

func (s *Server) getProjectTaskRetrospect(taskID string) (*ProjectTaskRetrospect, error) {
	var r ProjectTaskRetrospect
	var generatedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, task_id, summary, tools_used, open_issues, next_time,
		        llm_name, llm_model, generated_at, updated_at
		   FROM project_task_retrospects
		  WHERE task_id = $1`,
		taskID,
	).Scan(
		&r.ID, &r.TaskID, &r.Summary, &r.ToolsUsed, &r.OpenIssues, &r.NextTime,
		&r.LLMName, &r.LLMModel, &generatedAt, &r.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if generatedAt.Valid {
		t := generatedAt.Time
		r.GeneratedAt = &t
	}
	return &r, nil
}

func (s *Server) upsertProjectTaskRetrospect(taskID string, r rawTaskRetrospect, llmName, llmModel string, generatedAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO project_task_retrospects
		   (task_id, summary, tools_used, open_issues, next_time,
		    llm_name, llm_model, generated_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		 ON CONFLICT (task_id) DO UPDATE SET
		   summary = EXCLUDED.summary,
		   tools_used = EXCLUDED.tools_used,
		   open_issues = EXCLUDED.open_issues,
		   next_time = EXCLUDED.next_time,
		   llm_name = EXCLUDED.llm_name,
		   llm_model = EXCLUDED.llm_model,
		   generated_at = EXCLUDED.generated_at,
		   updated_at = EXCLUDED.updated_at`,
		taskID,
		strings.TrimSpace(r.Summary),
		strings.TrimSpace(r.ToolsUsed),
		strings.TrimSpace(r.OpenIssues),
		strings.TrimSpace(r.NextTime),
		strings.TrimSpace(llmName),
		strings.TrimSpace(llmModel),
		generatedAt,
	)
	return err
}

// generateTaskRetrospect runs the LLM call to produce a fresh
// retrospect for the task and writes it to project_task_retrospects.
// projectID is needed so we can pull project.runtime_env /
// business_scenario into the prompt context.
//
// triggeringUserID is used for resolving workspace LLM fallback —
// for manual triggers it's the request's user_id; for the auto path
// (status→done) it's the user who flipped the status.
func (s *Server) generateTaskRetrospect(ctx context.Context, projectID, taskID, workspaceID, triggeringUserID string) (*ProjectTaskRetrospect, error) {
	if s.aiAgent == nil {
		return nil, errors.New("ai agent not initialized")
	}

	project, err := s.getProject(projectID, workspaceID)
	if err != nil || project == nil {
		return nil, errors.New("project not found")
	}
	task, err := s.loadProjectTask(projectID, taskID)
	if err != nil || task == nil {
		return nil, errors.New("task not found")
	}

	runtimeConfig, llmName, err := s.resolveSystemAgentLLM(workspaceID)
	if err != nil {
		return nil, err
	}

	// Build the chat-thread message dump. If the task has no
	// chat_thread_id (e.g. completed without going through pickup)
	// we still produce a retrospect — based on title + description
	// alone.
	var messages []ChatMessage
	if task.ChatThreadID != nil {
		msgs, _, err := s.listChatMessages(*task.ChatThreadID, task.LLMThreadID, 200, 0, nil)
		if err == nil {
			messages = msgs
		}
	}

	siteName := "Polar"
	if site, err := s.getSiteSettings(); err == nil && site != nil && strings.TrimSpace(site.Name) != "" {
		siteName = site.Name
	}
	industry := s.getUserIndustry(triggeringUserID)
	envLabel := runtimeEnvLabel(project.RuntimeEnv)
	scenario := businessScenarioLabel(project.BusinessScenario)
	systemPrompt := s.renderPrompt(promptSlugTaskRetrospect, taskRetrospectPromptData{
		PlatformName: siteName,
		Industry:     industry,
		RuntimeEnv:   envLabel,
		Scenario:     scenario,
	}, "")
	runtimeConfig.SystemPrompt = systemPrompt
	runtimeConfig.Endpoint = "task_retrospect"

	userPrompt := buildTaskRetrospectUserPrompt(task.Title, task.Description, messages)
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
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("empty llm response")
	}
	parsed, perr := parseTaskRetrospectOutput(resp.Choices[0].Message.Content)
	if perr != nil {
		return nil, fmt.Errorf("parse: %w", perr)
	}

	now := time.Now()
	if err := s.upsertProjectTaskRetrospect(taskID, *parsed, llmName, runtimeConfig.Model, now); err != nil {
		return nil, err
	}
	_ = ctx // ctx kept on signature for future cancellation support
	return s.getProjectTaskRetrospect(taskID)
}

func buildTaskRetrospectUserPrompt(title, description string, messages []ChatMessage) string {
	var b strings.Builder
	b.WriteString("任务：\n\n标题：")
	b.WriteString(strings.TrimSpace(title))
	b.WriteString("\n\n描述：\n")
	d := strings.TrimSpace(description)
	if d == "" {
		b.WriteString("（空）")
	} else {
		b.WriteString(d)
	}
	if len(messages) == 0 {
		b.WriteString("\n\n（无关联聊天历史 — 仅基于任务标题/描述给出复盘）")
		return b.String()
	}
	b.WriteString("\n\n以下是任务执行过程中的聊天历史（按时间正序）：\n\n")
	for _, m := range messages {
		ts := m.CreatedAt.Format("01/02 15:04")
		who := m.SenderUsername
		if who == "" {
			who = m.SenderID
		}
		content := strings.TrimSpace(m.Content)
		content = strings.ReplaceAll(content, "\n", " ")
		// Generous truncation here vs thread-summary — the retrospect
		// wants more signal per message to spot tools_used / open
		// issues. 600 chars per message is still cheap.
		if len(content) > 600 {
			content = content[:600] + "...[truncated]"
		}
		b.WriteString("[")
		b.WriteString(ts)
		b.WriteString("] @")
		b.WriteString(who)
		b.WriteString(": ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	return b.String()
}

func parseTaskRetrospectOutput(raw string) (*rawTaskRetrospect, error) {
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
	var parsed rawTaskRetrospect
	if err := json.Unmarshal([]byte(jsonChunk), &parsed); err != nil {
		return nil, err
	}
	if strings.TrimSpace(parsed.Summary) == "" {
		return nil, fmt.Errorf("model returned empty summary")
	}
	return &parsed, nil
}

// autoRetrospectEnabled returns whether the on-status→done auto
// trigger should fire. Default OFF — design doc said "ship manual
// first, observe, then auto". Operators opt in via env var
// POLAR_AUTO_RETROSPECT=1 (any truthy: 1, true, yes, on).
//
// Workspace-scoped opt-in is the next refinement; for now this is
// a global toggle so the field stays small and migration-free.
func autoRetrospectEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("POLAR_AUTO_RETROSPECT")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// fireRetrospectAsync kicks off a retrospect generation in the
// background; used by the auto-trigger path (status→done). Errors
// are logged, not returned. Bounded by retrospectAutoTimeout so a
// stuck LLM doesn't park a goroutine forever. No-op when the
// auto-trigger env var isn't set (default behavior).
func (s *Server) fireRetrospectAsync(projectID, taskID, workspaceID, triggeringUserID string) {
	if !autoRetrospectEnabled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), retrospectAutoTimeout)
		defer cancel()
		if _, err := s.generateTaskRetrospect(ctx, projectID, taskID, workspaceID, triggeringUserID); err != nil {
			log.Printf("[retrospect auto] project=%s task=%s err=%v", projectID, taskID, err)
		}
	}()
}
