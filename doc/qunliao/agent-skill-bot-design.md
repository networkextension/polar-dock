# Design: `bot_kind='agent_skill'` —— polar-agent skill 作为 chat bot

**Status**: Draft, 2026-05-22
**Owner**: 待认
**Related**:
- doc/qunliao/dev-plan.md（当前 bot/room 主线）
- doc/arch/internal-api-v1.md v1.1（`POST /internal/v1/agent/dispatch` 已加在 F1 PR #292）
- internal/app/dock/ai_agent.go（aiAgent 队列 + LLM bot 分支）
- internal/app/dock/agent_hub.go（polar-agent WS 注册表）

## Motivation

当前 `bot_users` 都是 LLM persona：room 里 `@bot` → aiAgent 把房间近况 + bot system prompt 喂 LLM → reply 落库。

要新增一个口味：bot 背后是某台 polar-agent 上的 skill。第一个目标是 `coder`（Claude Code via MCP）—— room 里 `@coder fix login bug` 会在指定主机上跑一次有 file/git/build/exec 权限的编码会话，结果回到 room 里。

UX 跟普通 bot 一致（都是 @mention → reply）；底层走异步轨道（dispatch + callback），不阻塞 aiAgent 队列。

## Non-goals（明确不做）

- 不做：自动选 host —— bot create 时必须 pin 一个 `agent_id`
- 不做：单个 task 内 multi-turn —— 一次 @mention = 一个独立 session；要继续就再 @mention
- 不做：skill 之间编排 —— 一个 task 跑一个 skill；多 skill 流水线 P3 再谈
- 不做：approval gate（"apply / discard" 按钮）—— P2；P0 直接 fire-and-forget
- 不做：第三方 skill 接入 —— P3 marketplace 才做；P0 只支持仓库自带的 skill

## Schema 变更

### `bot_users` 加字段

```sql
ALTER TABLE bot_users
  ADD COLUMN IF NOT EXISTS bot_kind     TEXT NOT NULL DEFAULT 'llm',
  ADD COLUMN IF NOT EXISTS agent_id     TEXT,
  ADD COLUMN IF NOT EXISTS skill_name   TEXT,
  ADD COLUMN IF NOT EXISTS skill_config JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE bot_users
  ADD CONSTRAINT bot_users_kind_chk
    CHECK (bot_kind IN ('llm','agent_skill')),
  ADD CONSTRAINT bot_users_skill_fields_chk
    CHECK (
      (bot_kind = 'llm'         AND agent_id IS NULL    AND skill_name IS NULL) OR
      (bot_kind = 'agent_skill' AND agent_id IS NOT NULL AND skill_name IS NOT NULL)
    );
```

`skill_config` 是 skill 私有：coder 装 `{repo_path, branch_policy, auto_commit:false}`；
shell 装 `{cwd_whitelist:["..."], allow_network:false}`；library 装 `{collection_id}`；…

### `chat_messages` 加字段（async lifecycle）

```sql
ALTER TABLE chat_messages
  ADD COLUMN IF NOT EXISTS agent_task_id TEXT,
  ADD COLUMN IF NOT EXISTS agent_status  TEXT,   -- 'thinking'|'done'|'failed'|'cancelled'
  ADD COLUMN IF NOT EXISTS agent_meta    JSONB;  -- {duration_ms, exit_code, diff_url, ...}
```

复用现有 message row —— agent_skill 的 reply 在 placeholder → done 之间会被 edit
（普通 LLM bot 的 reply 仍是一次性写入，这两列保持 NULL）。

## Message lifecycle

```
user> @coder 把 login bug 修一下
       │
       ▼
aiAgent.fanOut(message)
       │
       │ bot_users[i].bot_kind == 'agent_skill'? ─── yes ──┐
       │                                                    │
       │ no → 走原 LLM 分支                                  │
       │                                                    ▼
       │                                       1) INSERT chat_messages
       │                                          body='正在处理…'
       │                                          agent_status='thinking'
       │                                          agent_task_id = <task uuid>
       │                                          → WS broadcast 'message_new'
       │                                                    │
       │                                                    ▼
       │                                       2) SDK.AgentDispatch({
       │                                            agent_id,
       │                                            skill_name,
       │                                            skill_config,
       │                                            prompt: <user text>,
       │                                            context: {room_id, message_id,
       │                                                      workspace_id, recent_msgs[10]},
       │                                          })
       │                                                    │
       │                                                    ▼
       │                                       3) dock /internal/v1/agent/dispatch
       │                                          → agentHub.lookup(agent_id).Send(...)
       │                                          → INSERT agent_tasks
       │                                                    │
       │                                                    ▼
       │                                       4) polar-agent 跑 skill
       │                                          (coder 可能几十秒到几分钟)
       │                                                    │
       │                                                    ▼
       │                                       5) polar-agent 通过同一条 WS 回包
       │                                          {task_id, status, body, meta}
       │                                          → dock agentHub 收到 → UPDATE chat_messages
       │                                          → WS broadcast 'message_edit'
       │                                                    │
       │                                                    ▼
       │                                       6) 客户端按 agent_status 重渲染
       │                                          thinking → spinner
       │                                          done     → ✅ + body (含 diff)
       │                                          failed   → ⚠️ + 摘要 + log 链接
```

**关键决策**：callback 走 polar-agent → dock 的现有 WS 通道（agentHub 已经握手过），
而不是开一个 polar-agent → dock 的 HMAC HTTP callback。理由：
- polar-agent 已经有持久 WS（用于 host_shell/host_vnc）
- 反向 HMAC channel 要在 polar-agent 端管 dock 的 HMAC key —— 多一个秘密扩散面
- 长任务结果通过 WS 推回，dock 端只需要在 agent_hub.go 里加 message type `task_result`

## 路由集成（aiAgent 改动）

`internal/app/dock/ai_agent.go::fanOutMention(roomID, msg, mentions)`：

```go
for _, bot := range mentions {
    switch bot.BotKind {
    case "llm":
        a.enqueueLLMReply(ctx, room, msg, bot)        // 原逻辑
    case "agent_skill":
        a.enqueueAgentSkillReply(ctx, room, msg, bot) // 新分支
    default:
        log.Printf("ai_agent: unknown bot_kind=%q for bot=%s", bot.BotKind, bot.ID)
    }
}
```

`enqueueAgentSkillReply` 做四件事：
1. INSERT placeholder message + 拿 task_id
2. 调 SDK.AgentDispatch
3. 调用同步返回 200 即可结束（callback 由 agentHub 推回时落库）
4. 失败（无 agent / dispatch 超时）→ UPDATE 那条 placeholder 为 `agent_status='failed'`

注意 **aiAgent 队列不等 task 完成** —— 这是 agent_skill 跟 LLM bot 在并发模型上的核心区别。
LLM bot 是同步占队列，task 跑完才释放；agent_skill 派完就放手，agent 端自己限并发。

## Auth / 计费

- Bot create 时必须挑一个 `llm_config_id`（即使是 agent_skill —— skill 内部跑 LLM 时反向走 dock LLM proxy）
- polar-agent skill **不允许**直接连外部 LLM 厂商：所有 LLM 调用经由 dock `/api/proxy/v1/chat/completions`
  - 复用 [[project-llm-billing-state.md]] 的 team quota + cost ledger
  - polar-agent 那侧只需要 dock 给的 llm_proxy_token —— 通过 dispatch payload 一次性下发
- 一次 @coder → 一行 `agent_tasks` + N 行 `llm_proxy_calls`，审计/billing 同一面板就能看完

## 失败处理（classifyBotError 复用）

| 场景                          | agent_status | reply body                            |
|------------------------------|--------------|---------------------------------------|
| 该 agent_id 未注册或 offline    | failed       | "执行该任务的主机离线，请稍后重试"        |
| dispatch 超时（agent 5min 不回）| failed       | "执行超时（5 分钟）"                    |
| skill 内部异常                 | failed       | error 摘要 + "查看完整日志 →" 链接       |
| LLM quota exhausted          | failed       | "本团队 LLM 配额已用完，请联系管理员"      |
| 用户撤回 @mention             | cancelled    | "用户取消了任务"                        |

`classifyBotError` 加一个 `kind='agent_skill'` 分支，错误码 → 友好文案的映射跟 LLM bot 各自维护。

## UI 变更

### Bot 创建 modal（/bots.html）
- 新增 radio：`Bot 类型` = `LLM` | `Agent Skill`
- 选 `Agent Skill` 时表单展开：
  - **Agent 主机** dropdown（从 `/api/agents` 拉，显示 hostname + presence dot）
  - **Skill** dropdown（hardcoded：`coder` / `shell` / `library`；P3 改成从 agent capabilities 拉）
  - **Skill 配置** JSON editor（schema 按 skill 不同走前端校验）

### Room 消息渲染（/rooms.html）
检查 `message.agent_status`：
- `thinking`：bot 头像 + spinner + body（"正在处理…"）
- `done`：bot 头像 + ✅ chip + body（markdown，可能含 ``` diff 块）+ meta（耗时 / token 数）
- `failed`：bot 头像 + ⚠️ chip + body（红字摘要）+ "查看日志" 链接 → 跳 agent task detail
- `cancelled`：灰色淡显示

### Bot 列表
头像旁加 skill 类型 chip：`LLM` / `Coder` / `Shell` / `Library`。

## Dispatch payload 详细 schema

```json
POST /internal/v1/agent/dispatch  (已在 F1 PR #292)
{
  "agent_id":     "agent-zen-01",
  "skill_name":   "coder",
  "skill_config": {"repo_path": "/Users/local/github/Polar-", "auto_commit": false},
  "prompt":       "fix login bug",
  "context": {
    "task_id":      "tsk_01HZ...",
    "room_id":      "rm_42",
    "message_id":   "msg_998",
    "workspace_id": "ws_main",
    "llm_proxy_token": "polar_proxy_...",
    "recent_messages": [
      {"role": "user", "name": "lushan", "body": "..."},
      ...
    ]
  }
}
→ 200 {"task_id": "tsk_01HZ..."}
```

callback（WS message_type='task_result'）：

```json
{
  "type": "task_result",
  "task_id": "tsk_01HZ...",
  "status": "done",                  // done | failed | cancelled
  "body":   "已完成。修改了 2 个文件：\n```diff\n...```",
  "meta": {
    "duration_ms": 87420,
    "files_changed": 2,
    "diff_url": "/api/agent/tasks/tsk_01HZ.../diff",
    "log_url":  "/api/agent/tasks/tsk_01HZ.../log",
    "llm_tokens": {"in": 4120, "out": 1830}
  }
}
```

## Phasing

| Phase | Scope | 工作量 |
|---|---|---|
| **P0** | schema migration + coder skill + dispatch + callback + UI 渲染 + 1 demo | 1 周 |
| **P1** | shell + library skill；agent_meta 渲染（diff/log inline）；retry button | 3 天 |
| **P2** | approval gate（apply / discard 按钮，coder 默认 dry-run）；within-task multi-turn | 2 周 |
| **P3** | skill marketplace（第三方 skill 通过 HMAC + manifest 自动注册） | 自由发挥 |

## Open questions（需要拍板）

1. **coder 默认是否 commit？** 强烈建议 **不**：默认 dry-run，diff 贴 reply 里，用户去手 commit。auto_commit 留 skill_config 但默认 false。
2. **agent offline 时降级？** fail-fast，不排队 —— 用户能看到立刻 retry 比闷头等好。
3. **同一 bot 并发 @mention？** 每个 message_id 一个独立 task，互不阻塞；agent 端自己 throttle（coder skill 一台机器同时跑 1 个 session 比较稳）。
4. **task 取消？** P0 不做（user 撤 @mention 也不 abort 已派的 task）；P1 加 `DELETE /api/agent/tasks/:id` 给 UI 用。
5. **task 历史在哪查？** 复用现有 `/agents.html`（按 agent 看历史）+ 新 tab "Tasks"。

## DoD（P0 收工）

- [ ] `bot_users` 4 字段 migration apply 到 .57
- [ ] `chat_messages` 3 字段 migration apply
- [ ] dock `ai_agent.go` 加 `enqueueAgentSkillReply` 分支 + 单测
- [ ] dock `agent_hub.go` 处理 inbound `task_result` 消息 + UPDATE chat_messages + WS broadcast
- [ ] polar-agent `skills/coder/` 实现（封装 claude-code via MCP）
- [ ] UI bot create 支持 agent_skill 分支
- [ ] UI room message 按 agent_status 渲染（spinner / ✅ / ⚠️）
- [ ] 端到端 demo：在 zen room 里 @coder "给 polar 加一个 hello world 子命令" → 远端 coder 跑通 → reply 里看到 diff

## 与现有架构的兼容性 check

| 现状 | 影响 |
|---|---|
| `aiAgent` 是 dock 内 in-process singleton（[[project-wg-mac-state.md]] 决定不搬到 plugin） | OK —— agent_skill 分支也只跑在 dock 内 |
| `agentHub` 同上 | OK —— callback 走 hub 现有 WS |
| `agent_tokens` canonical 在 dock | OK —— polar-agent 已经用 dock 的 token |
| LLM proxy 计费已就位（[[project-llm-billing-state.md]]） | OK —— skill 内部 LLM 调用反向走 proxy 即可 |
| Phase 2 plugin 拆分中（W1-W6） | 无冲突 —— bot/room/agent 都在 dock core |
| room.topic governs bot replies | agent_skill 不消费 topic（skill 有自己的 prompt 结构）；topic 可作为 hint 写进 context 但不强制 |

## Out of scope（明确推后）

- skill marketplace + 第三方 HMAC 注册 → P3 / Phase 4
- multi-agent 一个 task → 不做（架构上 bot:agent = N:1 锁定）
- 自动 commit / push / open PR → 永远不在默认路径上
- skill 之间数据 pipeline（coder 产 diff → reviewer skill 评） → P3
- 跨 room 的 task → 不做（task 绑 room_id）
