# 系统 Agent 设计稿

## Context

平台已经有一个特殊用户 `system`（`systemUserID = "system"`，定义在
`internal/app/dock/ai_agent.go:22`），用于「AI 助理」入口（侧栏底部
"AI Assistant"）。现在它只能进行普通的 chat 对话，没有专门为「帮用户
做事」设计的能力。

需求（用户原话拆解）：

1. 用户和系统 agent **交互**（已有，需要增强）
2. AI **协助写自我介绍**（生成 / 优化 `users.bio`）
3. 根据自我介绍的**行业**，反向**优化任务需求**（projects 模块的 task 描述）
4. 对**任务的总结和优化**（任务跑完后的 retrospect / 跑前的 sharpen）
5. 聊天单 thread **最近 msg summary**（一种 thread-level 自动总结）

整体定位：把 system agent 从「通用 chat 助理」升级成**「跟着用户的
profile + 上下文，主动做事」的助手**——它知道你是谁、你在做什么，
所以能给出针对性的输出，而不是每次都要用户重新交代上下文。

## 角色 & 身份模型

```
[用户 user (operator)]
   │
   │ chat / 调用工具 / 接受输出
   ▼
[系统 agent — system user]
   │ 读: users.bio + users.industry + 当前 chat thread + 当前 project task
   │ 调: LLM (workspace default 或站点默认)
   │ 出: 文本回复 / 直接修改用户字段（带确认）
```

**关键**：system agent **没有自己的 LLM 配置**——它复用调用上下文里
的 workspace default LLM（PR #104 的 `getWorkspaceDefaultLLMConfigID`）
或站点级 fallback（`AI_AGENT_*` 环境变量）。这是平台级 utility，不
是 workspace-scoped bot。

## 4 项能力的具体设计

### 能力 A：协助写 / 优化自我介绍

**用户路径：**

1. 用户在 `/profile.html` 或 dashboard settings 看到 "用 AI 改写自我
   介绍" 按钮
2. 表单里有：当前 bio（textarea）+ 「方向 / 重点」（chip 选择
   "更专业 / 更简短 / 更结构化 / 加我所在行业"）+ optional 自由文本
3. 提交 → 后端 `POST /api/system-agent/bio/refine` →
   返回改写后版本，用户预览后「采纳」才写入 `users.bio`

**Schema 改动：** 无（复用现有 `users.bio`）

**新 endpoint：**
- `POST /api/system-agent/bio/refine` (auth)
  - body: `{current_bio, hints: string[], extra: string}`
  - 调 LLM（workspace default LLM）+ 一个 system prompt 模板
    `system_agent.bio_refine`（DB-backed via PR #87）
  - 返回 `{drafts: [{text, label}], rationale: string}` —— 给两三个版本
  - **不直接写库**——前端预览 + 用户点确认走现有 `PUT /api/users/me/profile`

**system prompt 草案（落到 prompt_templates 表）：**

> 你是个人简介助手。基于用户的当前自我介绍 + 期望方向，输出 2-3 个候
> 选版本，每个不超过 120 字。每个候选写明你做了什么调整。**不要凭空
> 加事实**——如果用户没说自己是哪个行业，绝不替他指定。如果当前简介
> 太空泛，候选可以提示「补充行业 / 项目 / 关键词后再用」。

### 能力 B：根据行业优化任务需求

**前置改动：** 给 `users` 加 `industry TEXT NOT NULL DEFAULT ''`
列。这个值由用户手填或由能力 A 的 follow-up（"想为这条简介标个行
业吗？"）回写。

**用户路径：**

1. 在 `/projects.html` 项目页，每个 task 行旁边加「✨ 优化需求」按钮
2. 点击 → 弹窗显示「优化前 / 优化后」对比 + system agent 的 rationale
3. 用户接受 → 写回 `project_tasks.description`（保留旧版本到一个
   audit 表 — 见下）

**优化逻辑：**

- 输入：task.title + task.description + project.runtime_env +
  project.business_scenario + **users.industry**
- LLM prompt：以行业为视角，把 task 描述改写得 a) 更具体 b) 加上
  行业惯常的验收标准 c) 标出可能的盲点

**新 endpoint：**
- `POST /api/projects/:id/tasks/:task_id/sharpen` (auth)
  - body: `{instruction: string}` (optional 用户额外要求)
  - 返回 `{drafts: [{title, description, rationale}], original}`
- 用户点确认后用现有 `PUT /api/projects/:id/tasks/:task_id`
  写回——不在 sharpen 里直接写

**Audit:** task 重要修改建议有版本，新增表 `project_task_revisions`：

```sql
CREATE TABLE project_task_revisions (
  id BIGSERIAL PK,
  task_id TEXT REFERENCES project_tasks(id) ON DELETE CASCADE,
  title TEXT,
  description TEXT,
  source TEXT,        -- "manual" | "system_agent_sharpen"
  llm_name TEXT,      -- transparency
  llm_model TEXT,
  created_at TIMESTAMPTZ
);
```

每次 sharpen 接受时记一行，方便回滚 / 看是谁改的。

### 能力 C：任务的总结和优化

**两种用法：**

1. **跑前 sharpen**（同能力 B，已覆盖）
2. **跑后总结**（task status 切到 `done` 或 `succeeded` 时自动）

**跑后总结流程：**

- task 状态 → done 时触发（异步）
- 拉 task 的 chat_thread 全部消息（user + bot）+ 关联的 commit
  history（如果 project 有 git remote） + 落盘文件清单
- 喂给 LLM，要求输出：
  - "做完了什么"（1-2 句）
  - "用了哪些工具 / 模块"
  - "遗留问题"（明确 [需核实] 标记）
  - "下次类似任务的注意事项"

存在哪：新表 `project_task_retrospects`：

```sql
CREATE TABLE project_task_retrospects (
  id BIGSERIAL PK,
  task_id TEXT UNIQUE REFERENCES project_tasks(id) ON DELETE CASCADE,
  summary TEXT,
  tools_used TEXT,
  open_issues TEXT,
  next_time TEXT,
  llm_name TEXT,
  llm_model TEXT,
  generated_at TIMESTAMPTZ
);
```

UI：项目页 task 行展开后多一个「📋 复盘」section。

**新 endpoint：**
- `POST /api/projects/:id/tasks/:task_id/retrospect` (auth) — 手动触发
- `GET  /api/projects/:id/tasks/:task_id/retrospect` (auth) — 取
  最新版本

**自动触发**：在 task `bulk-status` / `update` 把状态改成 `done`
时，dock 排队（不阻塞响应）。失败不重试——用户手动点「📋 复盘」按
钮可以重新生成。

### 能力 D：聊天 thread 最近 msg summary

**目的：** 用户重新打开一个老 chat thread 时，最上方显示「最近发生
了什么」的 1-2 句话，免得翻到底再翻回来。也供 system agent 在跨
thread 引用时复用。

**触发**：当 thread 累计未被 summarize 的消息超过 N 条（默认 20）
或时间跨度超过 M 小时（默认 24h），异步生成。

**Schema：**

```sql
CREATE TABLE chat_thread_summaries (
  id BIGSERIAL PK,
  thread_id BIGINT UNIQUE REFERENCES chat_threads(id) ON DELETE CASCADE,
  summary TEXT NOT NULL DEFAULT '',
  -- summary 覆盖到哪条 message 为止；超过这条的新消息算 stale
  up_to_message_id BIGINT,
  message_count INTEGER NOT NULL DEFAULT 0,
  llm_name TEXT,
  llm_model TEXT,
  generated_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ
);
```

**新 endpoint：**
- `GET /api/chats/:thread_id/summary` (auth) — 取（如 stale 异步触发刷新）
- `POST /api/chats/:thread_id/summary/refresh` (auth) — 强制刷新

**Prompt：**

> 用 1-2 句话总结这个对话的当前状态：用户在尝试什么、bot 回复了
> 什么、当前 stuck 在哪。**不要复述消息**，写得像「上次看你停在
> X，正在 Y」的朋友提醒口吻。

**展示位置：** chat 页面 thread 顶部，灰底小条；可折叠隐藏。

## 平台级身份配置

system agent 不是 workspace bot，需要平台级配置：

- 复用 `AI_AGENT_API_KEY` / `AI_AGENT_BASE_URL` / `AI_AGENT_MODEL`
  环境变量（已有，给 `system` user chat 用的）
- 如果配了，能力 A/B/C/D 都用它
- 如果没配，能力 fallback 到 caller 的 workspace default LLM
- prompt 模板全部通过 `prompt_templates` 表（PR #87）DB-backed，
  admin 可在 `/prompts.html` 编辑：
  - `system_agent.bio_refine`
  - `system_agent.task_sharpen`
  - `system_agent.task_retrospect`
  - `system_agent.thread_summary`

LLM 透明度（前面 PR #111 立的规矩）：每个 endpoint 响应里带
`llm: {name, model}`，UI 显示「由 X 生成」字样。

## Schema 总览（增量）

```sql
ALTER TABLE users ADD COLUMN IF NOT EXISTS industry TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS project_task_revisions (...);     -- 能力 B audit
CREATE TABLE IF NOT EXISTS project_task_retrospects (...);   -- 能力 C
CREATE TABLE IF NOT EXISTS chat_thread_summaries (...);      -- 能力 D
```

预置 prompt_templates 行（bootstrap 时插入，admin 可编辑）：
- `system_agent.bio_refine`
- `system_agent.task_sharpen`
- `system_agent.task_retrospect`
- `system_agent.thread_summary`

## API 总览

| Endpoint | Auth | 说明 |
|---|---|---|
| `POST /api/system-agent/bio/refine` | user | 能力 A |
| `POST /api/projects/:id/tasks/:task_id/sharpen` | user | 能力 B |
| `POST /api/projects/:id/tasks/:task_id/retrospect` | user | 能力 C 手动 |
| `GET  /api/projects/:id/tasks/:task_id/retrospect` | user | 能力 C 读 |
| `GET  /api/chats/:thread_id/summary` | user | 能力 D 读（lazy-trigger） |
| `POST /api/chats/:thread_id/summary/refresh` | user | 能力 D 手动 |

## UI 改动

- `/profile.html` 自我介绍区加「✨ 帮我改写」按钮 → modal
- `/projects.html` task 行加「✨ 优化需求」按钮 + 完成态行加
  「📋 复盘」标识
- `/chat.html` thread 顶部加 summary 小条（lazy fetch）
- `/prompts.html` 自动多出 4 个 `system_agent.*` 模板供 admin 编辑

## Phase 切片（建议）

| Phase | 内容 | 工量 |
|---|---|---|
| 1 | 能力 D（chat thread summary）— 最小且对每个用户都受益 | 1.5 天 |
| 2 | 能力 A（bio refine）— 单 endpoint + UI 弹窗 | 1 天 |
| 3 | 能力 B（task sharpen + revisions 表） | 2 天 |
| 4 | 能力 C（task retrospect + retrospects 表 + 自动触发） | 2 天 |

每 phase 一个 PR。Phase 1 推荐先做，因为它不涉及 schema 设计的大头，
属于直接见效。Phase 4 自动触发是 retrospect 能力的高阶部分，可以
拆成「先手动按钮，下一刀再自动」。

## 显式 out-of-scope

- 跨 thread 长期记忆 / vector db
- system agent 主动「打断」用户（push notification）
- 计费 / 限流（用现有 LLM 调用计费即可）
- 自动行业推断（先让用户手填，后续再加从 bio 自动 infer）

## 关键文件清单（实施时）

服务端：
- `internal/app/dock/store.go` — 4 个 schema 变更
- `internal/app/dock/system_agent_handlers.go` (新)
- `internal/app/dock/system_agent_store.go` (新)
- `internal/app/dock/prompt_templates_defaults.go` — 4 个新模板
- `internal/app/dock/app.go` — 路由

UI：
- `ui/src/profile.ts` 加 bio refine 按钮 + modal
- `ui/src/projects.ts` 加 task sharpen + retrospect 按钮
- `ui/src/chat.ts` 加 thread summary 顶条
- `ui/src/api/system_agent.ts` (新) — 4 个 client

LLM 透明度：复用 `feedback_llm_transparency.md` 的 4 触点规则。
