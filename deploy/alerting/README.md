# Polar alerting (P0) — deployment-state-of-truth

Prometheus alert rules + Alertmanager + a webhook receiver that fans out to
**macOS 通知 + Polar 应用内聊天室 + Plane 自动开单**. Design: `doc/arch/alerting-design.md`.
Deployed LIVE on zen 2026-06-21. Everything is loopback + reversible.

## Layout (all on zen unless noted)
| File here | Deploys to | Purpose |
|---|---|---|
| `availability.rules.yml` | `/opt/homebrew/etc/prometheus.rules.d/` | `InstanceDown` (`up==0 for 5m`, critical) |
| `thresholds.rules.yml` | 同上 | CPU/内存/磁盘/温度 (metric 名已核实) |
| `watchdog.rules.yml` | 同上 | 常燃心跳(deadman 用,P0 暂 blackhole) |
| `alertmanager.yml` | `/opt/homebrew/etc/` | 路由/去重/inhibit;webhook→:9095 |
| `alert-receiver.py` | `/Users/local/bin/` | webhook → osascript + 房间 + Plane |
| `polar.alertmanager.plist` | `~/Library/LaunchAgents/` | Alertmanager :9093 (binary from GitHub release) |
| `polar.alert-receiver.plist.tmpl` | 同上(填 `__OPS_TOKEN__`) | 接收器 :9095 |

`prometheus.yml` 另需:`alerting:`→127.0.0.1:9093 + 每 scrape 目标加 `tier`(infra/fleet)。

## Flow
```
exporters → Prometheus 规则(for+severity) → Alertmanager(去重/inhibit)
  → alert-receiver :9095 → osascript 通知 + Polar 房间(ops key)+ Plane(critical,去重,resolved 关单)
```

## Notes / 坑
- Alertmanager 无 brew formula → 从 GitHub release 取 darwin-arm64 + rsync 到 `/opt/homebrew/bin/`.
- 接收器用 `urllib` 必须 `ProxyHandler({})` 绕过死的系统代理(同 plane-api.sh 的 `no_proxy=*`).
- 应用内:ops key(`polar_…`)可直接 `POST /api/rooms/<id>/messages`(zen open-api-token
  中间件已接 /api/*;token 属主即房间创建者/成员)—— **无需改 dock**. 告警房 id=5「告警」.
- Plane 项目 `polar ops` = `6ed4164e-ab38-4342-a19e-8f39751ea227`.
- 回滚:停 `polar.alertmanager`/`polar.alert-receiver` 两 plist + 还原 `prometheus.yml.pre-alerting.bak`.

## 未做(P1/P2)
- deadman 外部心跳(用户:zen 全挂=维护停机,不管).
- blackbox(HTTPS/DNS/证书到期)、LLM 根因归因、Grafana 告警面板、静默自助.
