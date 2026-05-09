# Grafana 集成设计稿

## Context

Polar 现在没有任何观测栈：dock 不暴露 metrics、没有 OTel、`logs_entries` 表
是 dev-only debug 用（[memory:project_logs_service](项目内部记忆) 里说
prod 会换 ES，但还没动）。当业务上线、研究派发跑起来、AI 调用计费上量
之后，**「现在到底怎么样」** 这个问题会越来越难回答 —— 我们需要一个
仪表盘。

Grafana 是合理选择：开源、轻量、能同时绘 Prometheus 指标 + 直接查
Postgres + 后续接 Loki / Tempo。本设计稿讨论怎么把它集成进来，分阶段
落地。

## 目标

**MVP 想看到的**：

1. dock 当前健康（请求量、错误率、p95 延迟、goroutine、DB 连接池）
2. LLM 调用消耗：按 provider / model / endpoint 切，token / 延迟 / 失败率
3. polar-agent fleet：当前 attached 数量、按 bot / tool 切
4. 研究派发活跃度：研究 run / sharpen / retrospect / thread summary 调用频次 + 成功率
5. iosdist build pipeline：sign 成功率、ASC upload 失败模式
6. 项目层活动：task 派单 / 完成节奏、按 workspace 切

**显式 non-goals**：
- 全链路分布式追踪（单 dock 进程，不需要 Tempo）
- 用户行为分析（埋点 / 漏斗 — 那是产品分析师的事，不进 Grafana）
- 替换 `logs_entries` 表（[memory:project_logs_service] 说 prod 会换 ES，
  那是另一刀）

## 架构

**双源策略**：Grafana 同时接两个 datasource。

```
                   ┌──────────────────────────────────┐
                   │              Grafana             │
                   └───────┬─────────────────┬────────┘
                           │ Prometheus      │ Postgres datasource
                           │ datasource      │ (read-only role)
                  ┌────────▼──────────┐   ┌──▼───────────┐
                  │  Prometheus       │   │   dock DB     │
                  │  (scrape /metrics)│   │ (live tables) │
                  └────────┬──────────┘   └──▲────────────┘
                           │ pull /metrics    │ writes
                  ┌────────▼──────────────────┴──────────┐
                  │     dock (gin-auth-app)              │
                  │     /metrics  ← prometheus/client_golang
                  └──────────────────────────────────────┘
```

**为什么双源不是双重工作**：

- 「业务指标」（研究 run 数 / 任务复盘量 / 工作区活跃度）天然就在
  Postgres 里 —— 现有 schema 都带 `created_at`、状态列、外键关系。
  让 Grafana 直接 SQL 出去就完事，**不要为了挂 Prometheus 再
  Inc() 一遍计数器** —— 那是双写、易漂移。
- 「运行时指标」（HTTP 延迟直方图 / goroutine 数 / DB 连接池）不在 DB
  里，必须 instrument。用 `prometheus/client_golang` 暴露 `/metrics`。
- 两个面板风格上对操作员是一致的（都是 Grafana），只在 panel 配置层
  分裂。

**为什么不上 Loki / Tempo / OTel**（暂时）：
- Loki 替代 ES — 在替换 `logs_entries` 那一刀里再决定（可能是
  Loki，可能是 ES，可能继续 PG）。本 PR 不绑死。
- Tempo 跑分布式追踪 — Polar 是单 dock 进程，agent 走 WS 不算独立
  trace edge，没必要。
- OTel 想统一，但目前只需要一个生产者一个消费者，引入 OTel collector
  是负担。等真要切多 backend 时再迁移（OTel 输出 Prometheus 就是一行
  config）。

## 指标清单（详）

### Prometheus 这一路

| 指标 | 类型 | 标签 | 来源 |
|---|---|---|---|
| `polar_http_requests_total` | counter | method, path, status | gin middleware |
| `polar_http_request_duration_seconds` | histogram | method, path | gin middleware |
| `polar_db_pool_open` / `_in_use` / `_idle` | gauge | (无) | `db.Stats()` 抽样 |
| `polar_goroutines` | gauge | (无) | `runtime.NumGoroutine` |
| `polar_llm_request_total` | counter | provider, model, endpoint, status | `aiAgent.requestChatCompletion` 收尾 |
| `polar_llm_request_duration_seconds` | histogram | provider, model, endpoint | 同上 |
| `polar_llm_tokens_total` | counter | provider, model, kind=prompt/completion | 解 LLM response 里 usage 字段 |
| `polar_agent_attached` | gauge | bot_kind, tool | `agentHub.lookup` 周期采样 |
| `polar_research_dispatch_total` | counter | status (queued/running/succeeded/failed) | dispatch handler |
| `polar_iosdist_build_total` | counter | sign_method, status | iosdist sign worker + agent build |

**实现位置**：
- `internal/app/dock/metrics.go`（新文件）—— 集中注册所有 Counter / Histogram / Gauge，提供小函数供 handler 调
- `internal/app/dock/app.go`：注册 `/metrics` 路由（公开还是 admin-gated 看下面）
- gin middleware 改一行：包一层 `prometheus.InstrumentHandlerFunc` 风格，记 `polar_http_*`

### Postgres datasource 这一路

直接给 Grafana 一个 read-only DB role，写 SQL 出 panel：

```sql
-- 研究派发 24h 漏斗
SELECT date_trunc('hour', created_at) AS t, status, count(*) FROM project_research_runs
WHERE created_at > now() - interval '24 hours'
GROUP BY t, status ORDER BY t;

-- 各 LLM 这周用量
SELECT llm_name, count(*) AS calls FROM project_task_retrospects
WHERE generated_at > now() - interval '7 days'
GROUP BY llm_name ORDER BY calls DESC;

-- 按 workspace 任务完成速率
SELECT pt.project_id, p.workspace_id,
       count(*) FILTER (WHERE pt.status='done' AND pt.completed_at > now() - interval '24 hours') AS done_24h,
       count(*) FILTER (WHERE pt.status='in_progress') AS in_progress
FROM project_tasks pt JOIN projects p ON p.id = pt.project_id
GROUP BY pt.project_id, p.workspace_id;
```

这些都是「现在 schema 里的事实」直接出图，零代码改动 —— 加 datasource +
画 panel。

### 安全

- `/metrics` 默认**绑 localhost** 或要求 admin token —— Prometheus 必
  须能拉，但 metrics 内容（路径、错误率、用量）不应公开
- Postgres datasource 用**只读 role**：`CREATE ROLE polar_grafana
  WITH LOGIN PASSWORD '...'; GRANT CONNECT ON DATABASE polar TO
  polar_grafana; GRANT USAGE ON SCHEMA public TO polar_grafana; GRANT
  SELECT ON ALL TABLES IN SCHEMA public TO polar_grafana;` —— 文档
  化，不进代码

## 部署形态

三种部署，按 footprint：

**A. 单机 dev / 评估**：docker-compose 起 prometheus + grafana
两个 service，配置文件 mount 到 host。dock 跑 `:8080` 暴露 `/metrics`，
prometheus.yml 加一条 scrape job 指 `host.docker.internal:8080`，
grafana 加 datasource 指 prometheus + 加 datasource 指本机 Postgres。

**B. 单 host 生产**：dock + prometheus + grafana 三个 systemd / rc.d
service。prometheus + grafana 都用本地存储（短期 retention，比如 30
天），不挂外部对象存储。比 docker-compose 多一些手动配置但稳定性更
高，资源占用更小。

**C. 现有 monitoring 集群**：直接接入。dock 暴露 `/metrics`，外部
Prometheus 加 scrape，外部 Grafana 加 dashboard JSON。

文档建议起步用 A，规模化后切 B / C。

## 实施 Phase

| Phase | 内容 | 估时 |
|---|---|---|
| **1** | dock 集成 `prometheus/client_golang` + 5 个核心 metric（http × 3 + goroutine + db 池） + `/metrics` endpoint。docker-compose 配置示例 + Grafana dashboard JSON v1（基础健康面板） | ~1.5 天 |
| **2** | LLM metric 三件套（request_total / duration / tokens）+ provider/model 标签 + dashboard 第二屏「LLM 用量」 | ~1 天 |
| **3** | agent_attached gauge + research_dispatch / retrospect / sharpen / iosdist_build 业务 counter 补全 + dashboard 第三屏「业务活跃度」 | ~1.5 天 |
| **4** | Postgres datasource + 5-8 个 SQL panel（按工作区切活跃度 / 复盘 / 派发漏斗）+ dashboard 第四屏「业务深度」 | ~1 天 |

每 phase 一个 PR。Phase 1 推荐先做 —— 不做不知道有没有问题，做了之后
其他 phase 随时机加。

## 关键文件清单（实施时）

```
internal/app/dock/
  metrics.go                   ← 新建，注册 + 暴露 helper
  app.go                       ← 加 /metrics 路由 + middleware 注册
  ai_agent.go                  ← Phase 2 加 LLM counter / histogram
  agent_hub.go                 ← Phase 3 周期采样 attached gauge

deploy/grafana/
  docker-compose.yml           ← 新建：prometheus + grafana 两个 service
  prometheus.yml               ← scrape config
  grafana/dashboards/polar-overview.json   ← 主面板 (Phase 1+)
  grafana/dashboards/polar-llm.json        ← LLM 面板 (Phase 2+)
  grafana/dashboards/polar-business.json   ← 业务面板 (Phase 3+4)
  grafana/datasources.yml      ← Prometheus + Postgres datasource 自动 provision
  README.md                    ← 起 stack + 加 datasource + 导入 dashboard

doc/grafana-integration.md     ← 本文件（Phase 1 落地时一起合）
```

## 决策记录

- **不上 OTel collector** — 一个生产者一个消费者不需要中间层；要切多
  backend 再迁
- **Postgres 直接做 datasource** — 业务事实已经在 DB 里，不再镜像到
  metrics（双写易漂移、维护成本高）
- **`/metrics` 默认 localhost-bind 或 admin token** — metric 内容不对
  外，公开会泄业务信息
- **不替 `logs_entries`** — log 替换是另一刀（ES vs Loki 选型还没定）
- **dashboard JSON 进 repo** — 不靠手画，让恢复 / 多 host 部署可重现

## 已确认决策（2026-05-09）

1. **`/metrics` 鉴权** — Bearer token，从 `METRICS_TOKEN` env 注入。
   `Authorization: Bearer <token>` 必匹配；空 token = endpoint 关闭
   （fail-closed）。Prometheus scrape config 里 `bearer_token` 配同值。
2. **Dev 环境 Grafana** — `localhost:3100` admin/admin（已就绪）。
   Phase 1 dashboard JSON 直接 import 进这个实例验收。
3. **Dashboard scope** — 按面分 4 个 dashboard：health / LLM / agents
   / business。每个能独立链接，操作员一次只盯一面。

## Phase 2 已落地（2026-05-09）

- **Histogram bucket** — 用 `[0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120]`
  秒级 bucket，覆盖快回复到长生成。
- **Token usage 解析** — OpenAI 兼容响应解 `usage.prompt_tokens` /
  `completion_tokens`。Anthropic Messages / Gemini / XAI Responses / 流式
  SSE 暂不计 token（panel 上为 0 是预期）。
- **Endpoint 标签** — `aiRuntimeConfig.Endpoint` caller-supplied 字符串。
  已覆盖：`thread_summary` / `bio_refine` / `task_sharpen` /
  `task_retrospect` / `chat` / `chat_tool_loop` / `project_decompose` /
  `markdown_assist` / `post_assist` / `reply_assist` / `test_connectivity`。
  未设 = 落入 `endpoint="unknown"`。

## 仍开放（后续 phase 决）

- **流式 SSE token 计数** — OpenAI SSE 协议尾部有 `usage` frame，目前
  没解。等真有人盯流式 token 用量再补，否则 panel 显示 0 也行。

## 验证（Phase 1 ship 时）

- `go build ./...` 通过；启动 dock 后 `curl http://localhost:8080/metrics` 返回
  Prometheus 文本格式，含 `polar_http_requests_total` 等
- `cd deploy/grafana && docker-compose up -d` 起 stack；浏览器开 `http://localhost:3000` 登录后看到一个 dashboard，5 个 panel 有数据
- 手动跑几个请求 → counter / histogram 跟着变
- DB 连接池一直饱和的话 panel 高亮（Grafana threshold）
