# 时序观测设计：Host / WireGuard 时序历史 + WebSocket 独立

> 状态：2026-05-31 讨论稿（未提交）。整理 host 模块与 wireguard 模块的时序观测
> 优化诉求，外加 WebSocket 从 dock 解耦的架构线。第 6 节是给决策用的开放问题。

---

## 0. 现状底数（先交代，避免空谈）

| 模块 | 现在有什么 | 缺什么 |
|---|---|---|
| **Host** | WS 心跳更新 `hosts.last_seen_at`（只存**最新**）；CPU/GPU 走 macmon exporter → Prometheus；dock 有 `metrics.go` + dock-ui `dashboard-widgets.ts`，有 grafana embed 底子 | 在线状态**历史**；负载在 dock UI 上的内联展示 |
| **WireGuard** | `hub_local_wgctl.go` 每 30s 解析每 peer 的 **rx/tx + latest_handshake**，POST 到 dock **缓存**（最新快照） | 流量/在线**历史**（不留时序） |

**共性**：3 个诉求本质都是「时序观测」——数据要么已进 Prometheus（CPU/GPU），要么在轮询但只缓存（WG rx/tx、host 在线）。WebSocket 独立是另一条**架构**线，单列。

---

## 1. 核心决策（先定这个，下面全跟着走）：时序数据存哪？

| 方案 | 怎么做 | 优 | 劣 |
|---|---|---|---|
| **A. 复用 Prometheus 当 TSDB** | 心跳/wg 指标进 Prometheus；dock UI 嵌 Grafana 面板或查 Prometheus HTTP API 自画 | 不重造轮子；保留/降采样全包；和现有 5 exporter 一致 | UI 依赖 Prometheus 可用性；要加一层 PromQL 查询；私有部署要把 Prometheus 一起打包 |
| **B. dock 自存 Postgres 样本** | `host_status_events` / `wg_peer_samples` 表，dock 自己出范围查询 | 自包含、产品不依赖 Prometheus；私有/离线部署友好 | 等于自造 TSDB（保留、降采样、聚合都要自己写） |
| **C. 混合（倾向）** | **稀疏事件**（在线/离线翻转）→ Postgres 事件表（便宜、天然是历史）；**高频数值**（cpu/gpu/吞吐）→ Prometheus | 各取所长 | 两套数据源，UI 要拼 |

> 商业化复用：楼宇/施耐德（[[../strategy/commercialization-roadmap.md]] §3）也要"设备/主机在线历史 + 负载监控"，这套时序观测是**可复用底座**，不是一次性投入。

---

## 2. Host 模块

### 2A. 在线状态历史（1 天）
- 现状只有 `last_seen_at`（最新）。两条路：
  - **Prometheus**：心跳暴露 `polar_host_up{host=…}` gauge → Grafana/PromQL 出 1 天在线带状图。
  - **Postgres 事件表**：`host_status_events(host_id, status, at)`，只在**状态翻转**时写一行 → 查窗口即得历史，存储极省。
- **倾向事件表**：在线/离线是稀疏事件，不值得 30s 采样进 TSDB。判定逻辑：心跳超过阈值（如 90s）未到 → 写一条 offline；恢复 → 写 online。后台 goroutine 扫 `last_seen_at` 即可。

### 2B. CPU/GPU 工作负载
- 数据已在 Prometheus（macmon exporter）。surface 两种：
  - **嵌 Grafana 面板**（iframe）：快，但样式割裂 + 鉴权要处理。
  - **dock 查 Prometheus HTTP API**（range query）→ /hosts.html 主机卡画**原生迷你折线**（cpu/gpu/mem），点开深链到完整 Grafana。
- **倾向原生迷你图 + 深链**：卡片一眼看趋势，详细去 Grafana。需要 dock 配 `PROMETHEUS_URL` + 一个 PromQL 查询层（可复用 `metrics.go`）。

---

## 3. WebSocket 从 dock 独立（架构线，单独立项）

**为什么**
1. dock 发版重启 → 所有 agent/聊天/房间/wg 的 WS 连接全断（agent 会重连，但抖动）。
2. agent/host 规模涨 → 长连接全压在 dock 进程，没法单独横向扩。

**选项**
- **A. 独立 WS 网关**：单独 ws-gateway 服务，鉴权调 dock `/internal/v1/auth`；dock 通过 **Redis pub/sub**（栈里已有 redis）推事件，网关扇出。dock 发版不掉 agent；WS 可独立扩。
- **B. dock 内优雅交接**：发版时 drain/handoff WS 连接。轻量，但不解决"扩"。

**现成可借**：插件已用 `polar-sdk/wsbroker`（如 `/ws/buildings` 自带 WS）；`/ws/agent` 是 dock 独有，正好第一个抽出来。

**取舍**：A 是对方向但有成本（多一跳 + 鉴权复用 + Redis 总线）。建议**按痛点触发**——"发版掉 agent"或连接数真痛了再上 A；现在可先做 B 止血。**和观测分开迭代**。

---

## 4. WireGuard 模块

### 4A. 每 peer 在线历史
- 从 `latest_handshake` 推（握手在 ~3 分钟内=在线），按时间留样 → 在线历史带状图。与 Host 2A 同一套事件表思路。

### 4B. hub 流量历史
- 现有 30s 轮询已拿每 peer rx/tx 计数器 + 握手，但只缓存。留历史两路：
  - 跑 **prometheus-wireguard-exporter** 在 hub → Prometheus → Grafana（最省，和 host 一致）；
  - 或把 30s 样本写 `wg_peer_samples(peer, rx, tx, handshake, at)` → dock 自画。
- 计数器做 `rate()` 出**吞吐曲线**：看谁是流量大户、握手稳不稳、端点漂移。

**价值**：① 给 wg-mac **客户端调优**（握手间隔/重连/漫游/吞吐 per-peer）；② **网络内流量观测**。

**注意边界**：更深的调优指标（丢包、MTU 效应、rekey 频率）`wgctl` 给不了 → 需 **wg-mac 的 NE 自己埋点上报**，是另一块（客户端埋点），别和 hub 侧混。

---

## 5. 推荐起手式（待拍板）

- **时序存储**：C（混合）——在线/离线走 Postgres 事件表，cpu/gpu/吞吐走 Prometheus。
- **UI**：原生迷你图 + 深链 Grafana。
- **WG**：先 hub 侧 prometheus-wireguard-exporter（监控够用），客户端 NE 埋点二期。
- **WS 独立**：单独立项；先 B（优雅交接）止血，痛点到了再上 A（网关 + Redis 总线）。

---

## 6. 开放问题（给决策）

1. **时序存哪**：A（Prometheus 全包）/ B（dock 自存）/ C（混合）？关系到"产品是否硬依赖 Prometheus"——私有部署给客户时 A 要把 Prometheus 一起打包。
2. **UI 风格**：嵌 Grafana（快、糙）vs dock 原生小图 + 深链（慢、统一）？
3. **WS 独立**：现在立项做网关（A），还是先优雅交接（B）、痛了再上？
4. **WG 调优深度**：只 hub 侧 rx/tx/握手（够监控），还是做到客户端 NE 埋点（丢包/MTU/rekey，才能真"调 app"）？
5. **复用楼宇**：是否一开始就设计成"主机/设备通用在线 & 负载历史"，给 #1 楼宇直接用？

---

## 附：维护
- 模块现状以 `modules/polar-hosts/internal/hosts/hosts_store.go`（`last_seen_at`）、
  `modules/polar-wg/internal/wg/hub_local_wgctl.go`（rx/tx/handshake 解析）为准。
- 观测栈现状见 memory `reference-observability-stack`（prometheus + grafana + 5 exporter）。
- 决策确定后回填第 5/6 节，再拆实现 PR。
