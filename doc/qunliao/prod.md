Cumora.ai 是一个 AI 代理团队工作空间，核心是“AI 不是工具箱，而是活的同事”：每个 Agent 有独立 persona、长期记忆、状态、主动发起对话能力，支持 Agent-to-Agent 私聊、群组讨论、Convene 会议等。用户已有 Chat Bot User 框架（假设是单用户-单Bot 的聊天后端 + 前端），下面给你一个完整、可落地实现方案，从架构到功能拆解、实现路径、技术栈建议。 


cumora.ai



1. 整体架构设计（分层）核心概念映射：Workspace：一个独立空间（类似项目/团队），包含 Humans + Agents。

Agent：有 ID、Persona（角色+系统提示+语音风格）、Memory（私有 + 共享）、Status（available/working/idle）、Private Workspace（文件/笔记）。

Room/Chat：公共群聊、DM、私信、Whisper Room（可观察的 Agent 间对话）、Convene（聚焦会议）。



推荐架构（前后端分离 + 实时 + Agent 引擎）：Frontend（Desktop-first，像 Cumora）：Electron / Tauri（推荐，后者轻量、Rust 安全）实现跨平台 macOS/Windows/Linux。

UI：聊天界面（类似 Slack/Discord） + Agent 列表 + Status + Private Workspace 侧边栏。

实时：WebSocket / Socket.io / Supabase Realtime。


Backend（API + 实时服务器）：Node.js (NestJS) / Python (FastAPI) / Go。

数据库：PostgreSQL (主) + Redis (缓存/队列/在线状态) + Vector DB (记忆检索，如 PGVector / Qdrant / Chroma)。

存储：S3/MinIO（文件）。


Agent Engine（核心新增部分）：每个 Agent 作为一个“自治实体”，有独立循环（循环检查 + 决策）。

使用 LangGraph / CrewAI / AutoGen / 或自研简单 Orchestrator。

LLM：OpenAI / Claude / Grok / 本地 Ollama（支持多模型）。


消息总线：所有消息进 Kafka / RabbitMQ / Redis Pub/Sub → 触发 Agent 决策。


2. 核心功能实现方案（基于你现有框架扩展）已有框架假设：有 User → Bot 单聊、消息存储、LLM 调用。需新增/扩展：A. Agent 管理模块创建/编辑 Agent：表结构：agents (id, workspace_id, name, persona_json, system_prompt, voice_style, status)。

默认 starter team：Atlas（研究员）、Iris（设计师）、Bram（工程师）、Nova（PM）——预置 prompt。


Persona 编辑：UI 表单修改 system prompt + 示例对话 + 性格描述。



B. 长期记忆系统（关键差异化）Per-Agent Memory：短期：对话历史（向量嵌入 + 总结）。

长期：Vector Store + Relational（笔记、文件摘要、关系气候“对某人感觉”）。

实现：每次对话后，用 LLM 提取 key facts → 存 Vector DB；检索时 RAG。


Shared Memory：Workspace 级知识库。



C. 主动性（Agents start things）——最难也最酷的部分Idle Loop：每个 Agent 有独立线程/定时任务（每 5-30min，用户可调）。步骤：读取最近 Room 上下文（总结）。

LLM 决策 Prompt：“你是 [Persona]，当前房间状态是... 你是否要行动？行动类型：post_in_room / dm_someone / convene / nothing”。

如果行动 → 生成消息 → 发送到消息总线。


用 Celery / BullMQ / Temporal 管理定时 + 重试。



D. 多类型聊天公共 Room：多用户 + 多 Agent 群聊（你现有框架扩展支持多 participant）。

DM：1v1（Human-Agent 或 Agent-Agent）。

Whisper Room：只读观察的子聊天（权限控制）。

Convene：临时聚焦房间 + 决策记录（投票/总结功能）。



E. 实时同步WebSocket 推送消息。

Agent 状态更新（online/typing/thinking）。

多设备同步（Electron + Web + Mobile 共用后端）。



F. 其他增强文件附件 + Agent 分析。

人类邀请（email/link）。

权限：Agent 分组到不同公司/项目。

通知/心情指示器（“房间氛围紧张”——通过情感分析）。



3. 实现路线图（分阶段）Phase 1: MVP（2-4 周） — 快速验证扩展现有框架支持多 participant 群聊。

实现 Agent 管理 + 基础 Persona。

简单记忆（对话历史 + 向量检索）。

手动触发 Agent 回复（@Agent

）。

Desktop Electron 壳子 + 聊天 UI。



Phase 2: 主动性 + 记忆（3-5 周）实现 Agent Idle Loop。

完整 Vector Memory + RAG。

Agent-to-Agent DM。

Status & 简单情绪模拟。



Phase 3: 高级功能（4-6 周）Convene、Whisper。

文件/项目管理。

多模型支持 + Tool Use（Agent 可调用外部工具）。

移动/Web 客户端。



Phase 4: 生产化监控（Agent 行为日志、成本）。

安全（隔离 Workspace、Prompt Injection 防护）。

计费（如果要做 SaaS）。



4. 技术栈推荐（平衡成本与性能）Frontend: Tauri (Rust + React/Vue/Svelte) —— 轻量桌面 App。

Backend: FastAPI (Python，便于 AI 集成) 或 NestJS。

AI 框架: LangGraph（状态机强，适合复杂 Agent 流） + LiteLLM（多 LLM 统一调用）。

DB: Postgres + PGVector + Redis。

部署: Docker + Kubernetes（或 Railway/Render 快速上线）。

本地优先：支持 Ollama，让 Agent 在用户机器跑（隐私 + 零成本）。



成本估算（早期）：LLM 调用：Claude 3.5 / Grok / GPT-4o-mini 主力。

向量 DB：本地或 Supabase。

服务器：中小项目每月几百 RMB。



5. 潜在挑战 & 解决方案Agent 幻觉/混乱：强 System Prompt + 宪法式规则（Constitutional AI） + 人类审核机制。

无限循环：Idle Loop 加冷却 + 成本阈值。

上下文爆炸：定期总结历史 + 分层记忆（短期/长期/工作区）。

性能：Agent Loop 用队列异步，不阻塞主聊天。

UI/UX：参考 Cumora 风格——干净、Agent 有头像+状态，像真人团队。



下一步行动建议：先画 ER 图（User/Workspace/Agent/Message/Memory）。

Fork 你现有框架，加一个 “MultiParticipantRoom” 模块。

实现一个最小 Agent Loop Demo（Python 脚本测试主动发言）。

做个 Tauri 原型，把 UI 先跑起来。



如果你把现有 Chat Bot User 框架 的技术栈（语言、是否已支持多用户等）告诉我，我可以给出更精确的代码结构/伪代码，甚至帮你拆具体模块的 Prompt 模板。这个方案完全可落地，从单 Bot 升级到“AI 团队”只是扩展维度的问题。需要哪部分细节（比如 Agent Decision Prompt 示例、数据库 Schema、Tauri 项目模板），随时说！



