# 告警设计:服务不可达 + 指标越阈 → 通知 / 工单

目标:任一(a)服务/主机不可达,或(b)指标越过阈值时,**及时、去重、可追踪**地通知,
并对严重项自动开 Plane 工单。复用现有 Prometheus(zen `:9090`)+ exporter 拉模型,
最小新增组件。现状:`prometheus.yml` 已声明 `rule_files:` 但**无规则文件**、**无 Alertmanager**、
无任何告警 —— 本设计从零补这层。

## 0. 四层结构

```
 采集(已有)        判定               路由/去重/抑制         动作
 exporters  →   Prometheus     →    Alertmanager     →   webhook 接收器
 (/metrics)     告警规则 rules        分组/去重/抑制/静默      → 运维推送(渠道)
 blackbox   →   for: 持续期          repeat/severity 路由     → 严重项开/更新 Plane 工单
 (探测)         severity 标签                                 → 写 alert_log 历史
                                          │
                                   Watchdog 常燃 → 外部心跳(谁看门人)
```

- **判定 = Prometheus 告警规则**(`/opt/homebrew/etc/rules/*.rules.yml`)。它本就在拉数,加 `expr + for + labels`。
- **路由 = Alertmanager**(brew 装 + plist):去重、分组(按 host/alertname)、**抑制**(主机 down 时压掉它名下的子告警)、静默(维护窗口)、重复间隔。
- **动作 = 一个薄 webhook 接收器**:接 Alertmanager 的 JSON,扇出到运维渠道 + Plane + 历史表。
- **看门人 = Watchdog**:一条永远 firing 的告警,经 Alertmanager 打到**外部**心跳;zen/Prometheus 整个挂了时由外部发现(本 lab .64/zen 都会重启,这条必须有)。

## 1. 两类告警

### 1A. 不可达(availability)
- `up == 0`:每个 scrape 目标。按**层级**分阈值(见 §2 打标签)。
- **blackbox 探测**(新增 `blackbox_exporter`):对"对外服务"做端到端探活,不只看进程 —— HTTPS `probe_success==0`(release/dns/lawyer/event/plane…),DNS 模块探 `.64` 解析器能否答 `zen.4950.store`(正好盯住那个会重启的 SPOF)。
- **证书将过期**:blackbox `probe_ssl_earliest_cert_expiry - time() < 14d`(warning)/ `<3d`(critical)—— 和 acme 自动续期互为保险。

### 1B. 越阈(threshold)
| 指标 | warning | critical | for |
|---|---|---|---|
| node CPU% | >90% | — | 10m |
| 内存% | >90% | >96% | 10m |
| 磁盘 fs% | >85% | >95% | 5m |
| 温度(macmon Apple) | >90℃ | >98℃ | 3m |
| 温度(.64 ACPI `node_acpi_thermal_celsius`) | >80℃ | >88℃ | 3m |
| dock 5xx 率 | >2%/5m | >10%/5m | 5m |
| postgres 连接 | >80% max | >92% max | 5m |
| redis 内存 | >90% maxmemory | — | 5m |

阈值/`for` 都可调;`for:` 是防抖关键 —— 2 分钟重启、笔记本休眠不该刷屏。

## 2. 本 lab 的关键裁剪:分层防抖

机器分两层,**靠 scrape 目标打标签** `tier`:
- `tier: infra` —— 必须常在:zen(dock/pg/redis/nginx)、`.64` 解析器、dns-svc。`up==0 for 2m` = **critical**。
- `tier: fleet` —— M3Max 机群,**会休眠/离线是常态**。默认 `up==0 for 15m` = warning,且**只对选定的"该常在"的 fleet 机**告警(其余进 `info`/不告警),避免半夜笔记本睡觉刷屏。
- **抑制规则**:`host` 级 `InstanceDown` firing 时,抑制同 host 的所有子告警(CPU/温度…),避免一台挂了引发一串噪声。

## 3. 通知渠道(已决策 2026-06-20:Polar 应用内 + macOS 通知)
Alertmanager → webhook 接收器后,扇出到:
1. **Polar 应用内(主)**:**发到一个专用告警聊天室**。关键 —— dock `/internal/v1/notify` 是
   **stub**(无真实 APNs/FCM 推送),但聊天系统是**活的**:`POST /api/rooms/:id/messages`
   今天就能用。所以接收器以「告警 bot」身份把 firing/resolved 消息发进
   `#alerts` 室,运维在 Polar 应用里看(对齐「简单胜过过度设计/贴进 DM」偏好,绕开 stub)。
   认证用 [[reference-zen-ops-api-key]] 的 `polar_` key(dock `/api/*` 认)。
2. **macOS 本地通知(辅)**:zen 上 `osascript -e 'display notification …'`(在机器前时即时弹),
   远程时靠渠道 1。
3. **Plane 工单**(critical 自动开,默认做):按 alert fingerprint 去重,复用 zone-federation
   「auto issue filing」;`resolved` 时回写评论/关单。
4. **alert_log 表**:历史可查(对齐「session state 服务端」「AI 驱动日志召回」偏好,日后 AI 归因)。

> 备选(没选,记一笔):飞书/企业微信群机器人 webhook(最快但要外部群)、Telegram、邮件(已配 SES `send.4950.store`)。
> 若日后要真·push(锁屏推送),需补 dock `/internal/v1/notify` 的 APNs/FCM 派发(Phase 1-C+)。

升级策略:warning → 仅渠道 1;critical → 渠道 1 + Plane 工单;critical 持续 N 分钟未恢复 → 重复/升级。恢复 → 发"已恢复" + 关单。

## 4. 谁看门人(dead-man's switch)
Alertmanager 配一条 `Watchdog`(规则 `expr: vector(1)` 恒真),走**独立路由**以固定周期打到**外部**心跳(healthchecks.io 之类,或另一台机的 cron)。**心跳停 = zen/Prometheus/Alertmanager 本身挂了** → 外部告警。鉴于 zen 和 .64 本会重启,这层不可省。

## 5. 选型理由
- **Alertmanager(选)** vs Grafana 告警:Alertmanager 是 Prometheus 原生,纯配置文件 + plist,契合本机 brew/plist/script 运维风;去重/抑制/静默/外部 deadman 更强。Grafana 告警(已有 Grafana)是更轻的备选,但状态在 Grafana DB、抑制弱。→ 取 Alertmanager。
- **blackbox_exporter**:进程 `up` 不等于"对外能用"(nginx 活着但后端 502)。端到端探活 + 证书到期一并解决。

## 6. 分期
- **P0(MVP)**:装 Alertmanager(brew + plist);写 `rules/availability.rules.yml`(`up==0` 分层)+ `rules/thresholds.rules.yml`(CPU/内存/磁盘/温度);目标打 `tier` 标签;接 **1 个**通知渠道(待选);加 Watchdog + 外部心跳。→ 不可达 + 基本越阈即时通知。
- **P1**:`blackbox_exporter`(HTTPS/DNS/证书探测)+ dock webhook 接收器(扇出 Plane 自动开单/关单 + alert_log)+ 抑制规则成套。
- **P2**:LLM 归因(告警风暴 → polar LLM proxy 出根因假设/处置建议)+ Grafana 告警面板 + 静默自助(维护窗口)。

## 7. 落地清单(P0,zen)
- `brew install alertmanager`(若 ghcr/网络受限 → 仿 exporter 那样从 GitHub release 直取 + rsync,见 [[reference-observability-stack]] 套路)。
- `/opt/homebrew/etc/alertmanager.yml`(路由 + 接收器 + inhibit)+ plist `polar.alertmanager.plist`(`:9093`,loopback)。
- `/opt/homebrew/etc/prometheus.yml`:`alerting.alertmanagers` 指 `127.0.0.1:9093`;`rule_files` 指 `rules/*.yml`;给 scrape 目标加 `labels: {tier: infra|fleet}`;`-/reload`。
- `rules/*.rules.yml` 两份。
- 接收器(渠道定后):最薄可先用 Alertmanager 内置(飞书/TG/邮件 webhook 直发);Plane/alert_log 留 P1 的 dock 接收器。
- Watchdog 规则 + 外部心跳注册。
