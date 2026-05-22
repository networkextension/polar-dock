package dock

// projects_agent_store.go — minimum surface area kept in dock to support
// the 7 project routes that didn't move to polar-projects:
//   /api/projects/:id/tasks/:task_id/sharpen
//   /api/projects/:id/tasks/:task_id/retrospect (GET + POST)
//   /api/projects/:id/tasks/:task_id/research/dispatch
//   /api/projects/:id/research/runs
//   /api/projects/research/runs/:run_id/{start,result}  (agent-auth)
//
// These handlers live in system_agent_handlers.go, task_retrospect.go,
// research_handlers.go and need:
//   - Project + ProjectTask + ProjectFeature type defs
//   - status const strings
//   - getProject(id, workspaceID) + loadProjectTask(projectID, taskID)
//
// Everything else in the former projects_store.go + projects_handlers.go
// moved to github.com/networkextension/polar-projects.

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Status constants — referenced by stay-in-dock retrospect + research
// handlers (e.g. mark a task "review" when retrospect generates).
const (
	ProjectStatusDraft    = "draft"
	ProjectStatusPlanning = "planning"
	ProjectStatusBuilding = "building"
	ProjectStatusBeta     = "beta"
	ProjectStatusShipped  = "shipped"
	ProjectStatusArchived = "archived"

	ProjectFeatureComplexitySimple  = "simple"
	ProjectFeatureComplexityMedium  = "medium"
	ProjectFeatureComplexityComplex = "complex"

	ProjectTaskStatusTodo       = "todo"
	ProjectTaskStatusInProgress = "in_progress"
	ProjectTaskStatusReview     = "review"
	ProjectTaskStatusDone       = "done"
	ProjectTaskStatusBlocked    = "blocked"
)

type Project struct {
	ID                 string    `json:"id"`
	WorkspaceID        string    `json:"workspace_id"`
	CreatorUserID      string    `json:"creator_user_id"`
	Name               string    `json:"name"`
	Description        string    `json:"description"`
	Status             string    `json:"status"`
	Template           string    `json:"template"`
	RuntimeEnv         string    `json:"runtime_env"`
	BusinessScenario   string    `json:"business_scenario"`
	CodeProvider       string    `json:"code_provider"`
	CurrentSprint      int       `json:"current_sprint"`
	GitRemoteURL       string    `json:"git_remote_url"`
	IOSDistAppID       *int64    `json:"iosdist_app_id,omitempty"`
	DefaultLLMConfigID *int64    `json:"default_llm_config_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type ProjectFeature struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	Ord          int       `json:"ord"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Complexity   string    `json:"complexity"`
	Selected     bool      `json:"selected"`
	SprintNumber int       `json:"sprint_number"`
	CreatedAt    time.Time `json:"created_at"`
}

type ProjectTask struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"project_id"`
	FeatureID    *string    `json:"feature_id,omitempty"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	Status       string     `json:"status"`
	BotUserID    *string    `json:"bot_user_id,omitempty"`
	ChatThreadID *int64     `json:"chat_thread_id,omitempty"`
	LLMThreadID  *int64     `json:"llm_thread_id,omitempty"`
	SprintNumber int        `json:"sprint_number"`
	Ord          int        `json:"ord"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

const projectSelectColumns = `id, workspace_id, creator_user_id, name, description, status, template, runtime_env, business_scenario, code_provider, COALESCE(current_sprint, 0), COALESCE(git_remote_url, ''), iosdist_app_id, default_llm_config_id, created_at, updated_at`

const projectTaskSelectColumns = `id, project_id, feature_id, title, description, status, bot_user_id, chat_thread_id, llm_thread_id, COALESCE(sprint_number, 1), ord, created_at, updated_at, started_at, completed_at`

func scanProject(scanFn func(...any) error) (*Project, error) {
	var p Project
	var iosAppID sql.NullInt64
	var defaultLLMID sql.NullInt64
	if err := scanFn(&p.ID, &p.WorkspaceID, &p.CreatorUserID, &p.Name, &p.Description, &p.Status, &p.Template, &p.RuntimeEnv, &p.BusinessScenario, &p.CodeProvider, &p.CurrentSprint, &p.GitRemoteURL, &iosAppID, &defaultLLMID, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if iosAppID.Valid {
		v := iosAppID.Int64
		p.IOSDistAppID = &v
	}
	if defaultLLMID.Valid {
		v := defaultLLMID.Int64
		p.DefaultLLMConfigID = &v
	}
	return &p, nil
}

func scanProjectTask(scanFn func(...any) error) (*ProjectTask, error) {
	var t ProjectTask
	var featureID, botUserID sql.NullString
	var chatThreadID, llmThreadID sql.NullInt64
	var startedAt, completedAt sql.NullTime
	if err := scanFn(&t.ID, &t.ProjectID, &featureID, &t.Title, &t.Description, &t.Status, &botUserID, &chatThreadID, &llmThreadID, &t.SprintNumber, &t.Ord, &t.CreatedAt, &t.UpdatedAt, &startedAt, &completedAt); err != nil {
		return nil, err
	}
	if featureID.Valid {
		v := featureID.String
		t.FeatureID = &v
	}
	if botUserID.Valid {
		v := botUserID.String
		t.BotUserID = &v
	}
	if chatThreadID.Valid {
		v := chatThreadID.Int64
		t.ChatThreadID = &v
	}
	if llmThreadID.Valid {
		v := llmThreadID.Int64
		t.LLMThreadID = &v
	}
	if startedAt.Valid {
		v := startedAt.Time
		t.StartedAt = &v
	}
	if completedAt.Valid {
		v := completedAt.Time
		t.CompletedAt = &v
	}
	return &t, nil
}

func (s *Server) getProject(id, workspaceID string) (*Project, error) {
	row := s.db.QueryRow(
		`SELECT `+projectSelectColumns+`
		 FROM projects WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID,
	)
	p, err := scanProject(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

func (s *Server) loadProjectTask(projectID, taskID string) (*ProjectTask, error) {
	row := s.db.QueryRow(
		`SELECT `+projectTaskSelectColumns+`
		 FROM project_tasks WHERE project_id = $1 AND id = $2`,
		projectID, taskID,
	)
	return scanProjectTask(row.Scan)
}

// runtimeEnvLabel + businessScenarioLabel: prompt-hint mappers used by
// sharpen + retrospect + research dispatch. Moved here from
// projects_handlers.go (deleted) along with the lookup tables.
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
	case "research":
		return "研究 / 技术预研 — 不写代码，agent 用本地工具 (read_file / write_file / list_dir / run_cmd) + 平台 LLM 做调研、阅读、汇总，产出 markdown 报告"
	}
	return ""
}

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

// IsResearchProject returns true when the project's runtime_env signals
// a research workflow. Used by research dispatch to pick the
// auto-registered research bot over coder bots.
func IsResearchProject(p *Project) bool {
	if p == nil {
		return false
	}
	return strings.TrimSpace(p.RuntimeEnv) == "research"
}

// pickupBodyData mirrors the project.pickup_body template variables.
type pickupBodyData struct {
	EnvLabel      string
	ScenarioLabel string
	CodeProvider  string
	GitRemoteURL  string
	BundleID      string
	Title         string
	Description   string
}

// buildPickupBody composes the first chat message for a task pickup.
// research_handlers needs it for the research-dispatch chat seed.
func (s *Server) buildPickupBody(project *Project, title, description string) string {
	bundleID := ""
	if project.IOSDistAppID != nil && *project.IOSDistAppID > 0 {
		if app, err := s.getIOSAppByID(*project.IOSDistAppID); err == nil && app != nil {
			bundleID = strings.TrimSpace(app.BundleID)
		}
	}
	data := pickupBodyData{
		EnvLabel:      runtimeEnvLabel(project.RuntimeEnv),
		ScenarioLabel: businessScenarioLabel(project.BusinessScenario),
		CodeProvider:  strings.TrimSpace(project.CodeProvider),
		GitRemoteURL:  strings.TrimSpace(project.GitRemoteURL),
		BundleID:      bundleID,
		Title:         strings.TrimSpace(title),
		Description:   strings.TrimSpace(description),
	}
	return s.renderPrompt(promptSlugProjectPickupBody, data, defaultPickupBodyTemplate)
}

const defaultPickupBodyTemplate = `{{- $hasHeader := or .EnvLabel .ScenarioLabel -}}
{{- $hasHeader = or $hasHeader .CodeProvider -}}
{{- $hasHeader = or $hasHeader .GitRemoteURL -}}
{{- $hasHeader = or $hasHeader .BundleID -}}
{{- if $hasHeader -}}
【任务约束】
{{- if .EnvLabel }}
运行环境：{{.EnvLabel}}
{{- end -}}
{{- if .ScenarioLabel }}
业务场景：{{.ScenarioLabel}}
{{- end -}}
{{- if .CodeProvider }}
首选编码工具：{{.CodeProvider}}
{{- end -}}
{{- if .GitRemoteURL }}
Git 远端：{{.GitRemoteURL}}（polar-agent 已 chdir 到 cwd 并配好 origin；只在 cwd 操作，不要 cd 到兄弟目录）
{{- end -}}
{{- if .BundleID }}
iOS Bundle ID：{{.BundleID}}
构建工具：用 xcodegen 生成 .xcodeproj，不要手编。Project.swift / project.yml 放在 cwd 根。
最终输出：unsigned IPA。xcodebuild archive 时用 CODE_SIGN_IDENTITY="" CODE_SIGNING_REQUIRED=NO CODE_SIGNING_ALLOWED=NO，再 xcodebuild -exportArchive 用一份 method=development 的 ExportOptions.plist。
任务结束前调用：polar-agent submit-build <path-to-ipa>，由 agent 完成签名 + 上传 TestFlight。
{{- end }}

【任务】
{{ end -}}
{{ .Title }}
{{- if .Description }}

{{ .Description }}
{{- end }}`
