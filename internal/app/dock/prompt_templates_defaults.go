package dock

// Code-baked defaults for every prompt slug the platform knows.
// bootstrapPromptTemplates inserts these on first start and
// refreshes the default_content column on every restart so the
// admin UI's "reset to default" always reflects the latest code.
//
// Slug naming: kebab-cased domain.purpose. Stable — referenced by
// handlers via constant. Renaming a slug is a breaking change
// (orphans the user's edit); deprecate and migrate instead.

const (
	promptSlugProjectDecompose       = "project.decompose"
	promptSlugProjectPickupBody      = "project.pickup_body"
	promptSlugMarkdownAssist         = "markdown.assist"
	promptSlugPostAssist             = "post.assist"
	promptSlugPostReplyAssist        = "post.reply_assist"
	promptSlugAIDefaultSystem        = "ai.default_system"
	promptSlugAITestSystem           = "ai.test_system"
	promptSlugAgentToolsAddendum     = "agent.tools_addendum"
)

// promptTemplateDefaults is the canonical registry. Each entry is
// the source-of-truth for one prompt slug.
//
// Variables documented per template — keep in sync with the data
// struct passed at the render site.
func promptTemplateDefaults() []promptDefault {
	return []promptDefault{
		{
			Slug:        promptSlugProjectDecompose,
			Name:        "项目 AI 拆解",
			Description: "把项目想法拆成 5-12 个 feature。变量：{{.Persona}} bot 人格（可空），{{.EnvLabel}} 运行环境标签（可空），{{.ScenarioLabel}} 业务场景标签（可空）。",
			Content: `{{- if .Persona -}}
{{.Persona}}

---

{{ end -}}
你是一个产品经理助手。用户会提供一个产品想法（自然语言），你的任务是把它拆解成 5-12 个独立的功能（feature）。每个功能要可以单独实现、单独测试。

返回严格的 JSON 数组（不要 markdown 代码块、不要解释），每项包含：
- "title": 简短标题（中文，不超过 20 字）
- "description": 1-2 句话描述这个功能要做什么、用户会怎么用
- "complexity": "simple" | "medium" | "complex"，参考标准：simple = 单视图 + 静态数据；medium = 多视图 + 状态管理；complex = 涉及自定义动画、网络、持久化、设备能力

按用户预期的开发顺序排列功能（先做基础结构，再加内容，最后做动画 / 收尾）。
{{- if .EnvLabel }}

运行环境约束：
  {{.EnvLabel}}
所有 feature 必须在这个环境下可实现；不假设其他平台能力。
{{- end -}}
{{- if .ScenarioLabel }}

业务场景约束：
  {{.ScenarioLabel}}
拆解时聚焦此场景的核心交互。
{{- end -}}
{{- if and (not .EnvLabel) (not .ScenarioLabel) }}

对于「创意小游戏 / 文学+地理+历史 + 图 + 动画」类应用，重点关注表现层：UI 屏幕、内容展示方式、交互手势、动画效果。不要假设有后端 API（默认资源是 bundled 静态文件）。
{{- end }}

只返回 JSON 数组，不要任何其他文字。`,
		},
		{
			Slug:        promptSlugProjectPickupBody,
			Name:        "任务派单接场白",
			Description: "task pickup 时下发给 bot 的第一条消息。变量：{{.EnvLabel}} 运行环境标签（可空），{{.ScenarioLabel}} 业务场景标签（可空），{{.CodeProvider}} 首选编码工具（可空），{{.GitRemoteURL}} git 远端（可空），{{.Title}} 任务标题，{{.Description}} 任务描述（可空）。",
			Content: `{{- $hasHeader := or .EnvLabel .ScenarioLabel -}}
{{- $hasHeader = or $hasHeader .CodeProvider -}}
{{- $hasHeader = or $hasHeader .GitRemoteURL -}}
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
{{- end }}

【任务】
{{ end -}}
{{ .Title }}
{{- if .Description }}

{{ .Description }}
{{- end }}`,
		},
		{
			Slug:        promptSlugMarkdownAssist,
			Name:        "Markdown 编辑助手",
			Description: "Markdown 编辑器调用 AI 润色时的 system prompt。变量：{{.BotPrompt}} bot 持有的额外偏好（可空），{{.Instruction}} 用户本次润色要求（可空）。",
			Content: `你是一个中文写作润色助手。你的任务是基于用户原文做改写与润色，而不是替用户重新发明核心观点。

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
{{- end }}`,
		},
		{
			Slug:        promptSlugPostAssist,
			Name:        "Post 创作助手",
			Description: "Post 撰写时调用 AI 的 system prompt。变量同 markdown.assist。",
			Content: `你是一个中文社交帖子写作助手。你的任务是基于用户草稿润色或生成帖子正文，保持用户的核心观点、语气和立场。

必须遵循：
1) 保留用户的关键事实、观点与结论，不擅自新增未提及的事实。
2) 输出控制在 280 字以内，适合社交平台发布；段落短，观点清晰。
3) 如果用户给了明确风格要求按要求润色；否则默认自然、真诚。
4) 仅输出最终可直接发布的正文，不要前后缀、不要解释、不要引号包裹。
{{- if .BotPrompt }}

Bot 额外写作偏好：
{{.BotPrompt}}
{{- end -}}
{{- if .Instruction }}

本次写作要求：
{{.Instruction}}
{{- end }}`,
		},
		{
			Slug:        promptSlugPostReplyAssist,
			Name:        "Post 回复助手",
			Description: "Post 回复时调用 AI 的 system prompt。变量同 markdown.assist。",
			Content: `你是一个中文评论写作助手。你的任务是基于原帖与用户草稿生成一条简洁、得体、真诚的评论。

必须遵循：
1) 先理解原帖内容与作者立场，评论要具体、不空泛。
2) 长度 20-140 字之间，语气友好，避免说教，避免过度敬语。
3) 保留用户草稿中的核心观点；若草稿为空，根据原帖给出一个合理角度。
4) 仅输出最终可直接发布的评论内容，不要前后缀、不要解释。
{{- if .BotPrompt }}

Bot 额外写作偏好：
{{.BotPrompt}}
{{- end -}}
{{- if .Instruction }}

本次评论要求：
{{.Instruction}}
{{- end }}`,
		},
		{
			Slug:        promptSlugAIDefaultSystem,
			Name:        "AI 默认 system prompt",
			Description: "Bot 没配 system_prompt 时用这个 fallback。无变量。",
			Content:     `你是站内的 AI 助手，请基于上下文和当前问题，用清晰、可靠、结构化的中文回复。`,
		},
		{
			Slug:        promptSlugAITestSystem,
			Name:        "LLM 连通性测试 prompt",
			Description: "测试 LLM config 时用的 prompt（system_prompt 为空时）。无变量。",
			Content:     `你是一个用于连通性验证的 AI 助手，请简短回复 ok。`,
		},
		{
			Slug:        promptSlugAgentToolsAddendum,
			Name:        "polar-agent tool-use 模式追加内容",
			Description: "tool-loop 模式（非 passthrough）下追加在 bot system_prompt 末尾的工具规则说明。变量：{{.Workdir}} polar-agent 的工作目录。",
			Content: `
You have a polar-agent attached on the user's Mac. The working
directory is: {{.Workdir}}

Use the provided tools (read_file / write_file / list_dir / run_cmd)
to inspect and modify the codebase. Important rules:

- DO NOT dump file contents or code in chat replies. Use write_file
  for new/changed files; the user reviews via git diff afterwards.
- Plan first, then execute. Brief progress notes in chat are fine,
  but don't repeat tool output verbatim.
- For multi-step changes, do them in one stretch (sequence tool
  calls in the same response when independent, or chain across
  turns when later steps depend on earlier results).
- When done, give a short summary in chat (what was changed, how
  to test, anything that still needs human attention).
- If a command fails, read the stderr carefully and fix the root
  cause; don't blindly retry.`,
		},
	}
}
