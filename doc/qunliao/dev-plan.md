# Cumora-style AI Coworkers — Polar- Development Plan

参考愿景：`doc/qunliao/prod.md`（Cumora.ai 风格，AI 同事 = 活的团队成员）。
落地策略：**复用 Polar- 现有 chat / bot / billing 基建，不引新栈**。

## 现状

| 模块 | 已有 | 文件 |
|---|---|---|
| Workspaces | `teams` + `team_members` | `internal/app/dock/store.go` |
| Bot users | `bot_users` → `users` + `llm_configs` | `internal/app/dock/store.go` |
| LLM configs | owned + shared + `is_system` (PR #261) + billing (PR #262) | `internal/app/dock/llm_*` |
| 1:1 DM | `chat_threads` + `chat_messages` | `internal/app/dock/chat_*` |
| Realtime | `wsHub` + Redis `chat:events` | `internal/app/dock/ws_*` |
| 文件附件 | `attachments` + `chat_message_attachments` | `internal/app/dock/chat_handlers.go` |
| 短期上下文 | `chat_thread_summaries` | `internal/app/dock/chat_thread_summary_handlers.go` |

## 差距 → 路线图

### P0 · 多参与者房间（基础设施）
- 新表 `chat_rooms(id, workspace_id, kind, name, topic, created_by, created_at, archived_at)`
- 新表 `chat_room_members(room_id, user_id, role, joined_at, last_read_message_id)`
- `chat_messages` 加 `room_id BIGINT` 可空列（双轨：thread_id XOR room_id）
- 路由 `POST/GET/PUT/DELETE /api/rooms`、`POST/DELETE /api/rooms/:id/members`
- WS broadcast 改为按 room 订阅（沿用 `wsHub.threadViewers` 改 `roomViewers`）
- 旧 `chat_threads` API **不动**，前端渐进切换

### P1 · 多 bot 共存 + @mention
- `chat_send` 解析 `@bot_name` → 对每个被点名 bot fanout 一份 `aiAgent.requestChatCompletion`
- 无 @ 不响应（避免群里 bot 自嗨）
- 复用 PR #262 的 `recordAgentLLMCall`（cost_usd 按已有 pricing 计）

### P2 · Persona 扩展
- `bot_users` 加 `persona_json JSONB`（voice/style/拒答规则）
- `bot_users` 加 `status TEXT`（available/working/idle/offline）
- `bot_users` 加 `avatar_url TEXT`
- 前端 bot 卡片渲染状态点 + 头像

### P3 · 主动性（Idle Loop）★差异化
- 新表 `chat_room_agent_loops(room_id, bot_user_id, interval_secs, last_decided_at, enabled)`
- 后台 goroutine pool：每 N 秒扫一次 enabled rows，到点的拉最近上下文 → 跑决策 prompt → JSON `{action: post|dm|convene|nothing, content?}`
- 护栏：
  - 每 bot 每小时硬上限（默认 4 次）
  - 走 `checkChatBillingGate`，超额自动转 idle
  - workspace owner 在 room 设置面板手动开关 + 调频率
- 默认 **OFF**（避免新 workspace 莫名烧钱）

### P4 · 长期向量记忆
- 启用 PGVector（Postgres.app 内置，`CREATE EXTENSION vector`）
- 新表 `agent_memory(id, bot_user_id, workspace_id, kind=fact|preference|relation, text, embedding vector(1536), source_message_id, created_at)`
- bot 回复前 RAG 检索 top-K → 注入 context window
- 对话后 LLM 异步提取 key facts → 入库
- 用 `text-embedding-3-small` 或本地 ollama 嵌入（admin 选）

### P5 · Whisper / Convene
- `chat_rooms.kind` 加 `whisper`（只读观察者，`chat_room_members.role=observer`）
- `chat_rooms.kind` 加 `convene`（带 `convene_decisions JSONB` 字段）
- UI 加投票 / 总结按钮

### P6 · Agent↔Agent DM
- 放开 `chat_send` 的 "另一端必须是 human" 校验
- bot↔bot 受 P3 的 Idle Loop 成本闸门约束

## 不做（明确取舍）

| 不做 | 理由 |
|---|---|
| Tauri / Electron 客户端 | web UI 已能跑桌面+移动浏览器，ROI 低 |
| Kafka / RabbitMQ / Celery | Redis pub/sub + goroutine pool 在单 Mac 部署够用 |
| LangGraph / CrewAI / AutoGen | 1-2 跳 LLM 调用，Go switch 即可 |
| Qdrant / Chroma / 其他向量 DB | Postgres 已在跑，PGVector 不引新服务 |
| FastAPI / NestJS | 全栈 Go，不割裂 |
| 独立 Agent 进程隔离 | 单 process goroutine + per-bot rate cap 足够 |

## 节奏

- 7-8 周走完 P0-P6
- 想先做 boss demo：**P0+P1+P3 ≈ 3 周**（多 bot 群聊 + 主动发言的 wow 核心）

## 关键决策（开工前已拍板）

1. **双轨迁移**：旧 `chat_threads` API 保留，新 `/api/rooms/*` 并存，前端渐进切换
2. **Idle Loop 默认 OFF**：workspace owner 显式开启 + 调频率，避免无声烧钱
3. **复用 LLM billing**：所有 bot LLM 调用走 `checkChatBillingGate`，customer team 付费即玩

## 当前进度

- [x] 计划落地（本文档）
- [ ] **P0a 在做**：`chat_rooms` + `chat_room_members` 表 + typed CRUD + handlers + 路由
- [ ] P0b：`chat_messages.room_id` 列 + 消息发送/拉取双轨 + WS broadcast 改 room
- [ ] P0c：前端 chat.ts 加 room 列表 + 成员侧栏
- [ ] P1 ...
