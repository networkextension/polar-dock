# 模块平台化计划：自描述 API + 直连模块 + 模块 OTA 自更新

> 状态：2026-05-31 计划稿。起因：运维/调试中反复踩三个病根——① 每次定位 API 都要翻
> Go 源码；② 对模块的操作都绕 dock 一圈；③ 部署老是跑着旧版本（zen 上 hosts-svc 是
> 5/29 的旧码，导致 register 500）。本计划把这三件根治。先做 Track 1（最止痛、最自包含）。

---

## Track 1 — 自描述 API（生成可查的 API 目录 / skill）

**目标**：API 表是**可查询的**，不是靠 grep 源码。AI/运维/前端都能问"有哪些端点、要什么参数"。

**P0（先做）**：dock + 每个模块暴露 `GET /api/_routes`（或 `/_meta/routes`），用 gin
`engine.Routes()` 直接吐出全部 `method + path + handler` 的 JSON。**一个 curl 取代所有 grep。**
- 复用：gin 自带 `Routes()`；handler 里读 `s.router.Routes()`。
- 鉴权：admin-gated（避免泄露内部路由面）。

**P1**：把路由表升级成**带参数/请求体形状**的契约。两条路：
- 注释驱动：在 handler 上加结构化 doc 注释 → 构建期生成 `doc/api/openapi.json`；
- 或类型驱动：从 handler 的 `ShouldBindJSON(&T)` 入参类型反射出字段（dock 里大量用这个模式）。
- 产出一份 OpenAPI，前端 + agent 都吃它。

**P2**：做成 **polar-agent skill**（`api-catalog`）——agent 能"列端点 / 查某端点契约 /
按契约发请求"，彻底告别人肉翻代码。复用已有 skill 框架（mcp-server skill 那套）。

**关联**：`doc/arch/internal-api-v1.md` 是内部 API 的人工版，本 Track 是它的自动化版。

---

## Track 2 — 对模块的 API 直连，不绕 dock

**现状**：dock 持有 `/api/hosts/*`、`/api/assets/*` 等并转发给插件；前端每次操作都
dock → 插件绕一圈。buildings 已经自带 `/api`（在自己子域），是对的方向。

**目标**：每个模块在**自己子域**直接提供 REST，前端/客户端直连模块，dock 只管身份与编排。

**做法**：
- 模块 REST 统一搬到模块自身（像 buildings 那样），鉴权走共享的 dock `/internal/v1/auth/verify`（SDK 已缓存）。
- 前端 API client 指向**模块子域**（`hosts.4950.store/api/…`、`assets-*/api/…`），不再打 dock 的转发层。
- dock 只保留：身份、`/internal/v1/*`、跨模块编排、UI 壳。
- 和"WebSocket 从 dock 独立"（[[observability-design.md]] §3）同一个去中心化主题——`/ws/agent` 也属于该抽出来的。

**取舍**：减少 dock 瓶颈 + 发版不互相牵连；代价是鉴权要在每个模块做好（已有 SDK 中间件，buildings 已验证）。**按模块逐个迁，不一次性大爆改。**

---

## Track 3 — 模块 package 定期发布 + dock UI 看更新 + 点击自更新（OTA）

**现状的痛**：插件是手工 build→rsync→重启的裸进程/launchd；版本漂移（zen 旧 hosts-svc）、
重启就丢（裸进程）。

**目标**：开发期模块按版本发布；dock UI 的"模块"页显示**已装版本 vs 最新版本**；点一下，
模块**自更新**（拉新二进制→换→重启），全程 UI 可见。

**组件**：
1. **包仓库**：直接用 **assets 模块**托管模块二进制（`kind=module-bin`、`name=<svc>`、
   `version=<git-sha/日期>`、附 sha256）。一次性把 Track 3 和 assets 打通。
2. **版本发布**：CI 或 `make release-<svc>` 构建 arm64 → 上传 assets → 写 `module_releases`
   表（svc, version, sha256, asset_id, channel=dev/stable, published_at）。
3. **dock UI 模块页**：每个模块卡显示"运行版本（来自心跳上报的 build ref）vs 最新 release"，
   有更新就亮"更新"按钮。
4. **自更新执行**：点更新 → dock 下发指令（经 agent skill 或模块的 `/v1/self-update`）→
   模块拉 asset（warm-pull）→ 校验 sha → 原子替换二进制 → **launchd 重启**（KeepAlive 接管）。
   复用已有：polar-agent 的 `install.sh` + `/api/agent/binary` 自更新模式，推广到所有插件 svc。

**前置**：assets provider 得是健康的（当前 zen-prov-1 挂了——见下"已知阻塞"）。
**关联**：RBAC R3「模块 marketplace」与本 Track 重叠，可合流。

---

## 执行顺序与理由

| 顺序 | Track | 为什么先它 |
|---|---|---|
| **1** | **T1 P0**（`/api/_routes` 自描述路由） | 最止痛、最自包含、当下就消除"翻代码"；不依赖 provider/部署 |
| 2 | T1 P1/P2（OpenAPI + agent skill） | 在 P0 基础上加契约与 skill |
| 3 | T3（模块 OTA） | 治部署漂移；**需先修好 assets provider**（健康托管二进制） |
| 4 | T2（直连模块） | 最大重构，按模块逐个迁，和 WS 独立合流 |

> 不阻塞：**Track 1 P0 现在就能做**，且正好是我反复 grep 的那个痛点。

---

## 已知阻塞 / 依赖
- **assets provider 挂了**（zen-prov-1 / dev-prov-1 `last_seen=never`，assets-svc 没在跑、
  启动配置丢失）。T3 的"包仓库"依赖它健康。**先不急修**（用户指示），但 T3 开工前要解决。
- 部署到 zen 走 `ssh local@10.88.0.1`（见 memory `project-build-box-agent-register`）。

---

## 本轮执行
**Track 1 P0**：给 dock 加 `GET /api/_routes`（admin-gated），返回 gin 全量路由表 JSON。
提交 + 开 PR（不强求立即部署）。后续 P1/P2、T2、T3 按上表推进。
