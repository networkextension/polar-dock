# Polar 告警系统 — 使用说明

> 同步自 Plane 项目 **`polar alarm`** 的文档页(`polar alarm — 文档`)。LIVE on zen 2026-06-21。
> 告警**自动开单**,你主要"看"和"调",一般不用手动建单。

## 一图看懂
```
exporters → Prometheus 规则 → Alertmanager(去重/抑制) → 接收器(:9095)
   ① macOS 桌面通知(在 zen 前时)
   ② Polar 聊天室「告警」(房间 id 5,随时随地看)
   ③ Plane 项目 polar alarm 自动开单(仅 critical,恢复自动关单)
```

## 我在哪看告警?
- **实时**:Polar 应用的「告警」聊天室 —— 🔴 critical / 🟠 warning / ✅ 已恢复。
- **要跟进的**:`polar alarm` 项目里自动开的工单(只有 critical 才开;恢复后自动置 Done + 评论)。
- **在 zen 机器前**:macOS 桌面通知。

## 告警怎么判定的
- 规则在 zen `/opt/homebrew/etc/prometheus.rules.d/`:`availability`(不可达 `up==0`)、`thresholds`(CPU/内存/磁盘/温度)、`watchdog`。
- 必须持续超过 `for` 时长才触发(防抖,避免瞬断刷屏)。
- 去重/分组/抑制由 Alertmanager(`:9093`)做 —— 一台整机 down 会自动压掉它名下的子告警(CPU/温度…)。

## 当前阈值(都可调)
| 项 | warning | critical | for |
|---|---|---|---|
| 不可达 `up==0` | — | ✓ | 5m |
| CPU | >90% | — | 10m |
| 内存 | >90% | >96% | 10m |
| 磁盘 | >85% | >95% | 5m |
| Apple 芯片温度 | >90℃ | — | 3m |
| .64 ACPI 温度 | >80℃ | — | 3m |

## 怎么调
- 改阈值/加规则:编辑 `rules.d/*.yml` → `curl -X POST http://127.0.0.1:9090/-/reload`。
- 太吵(M3Max 笔记本休眠会误报):当前策略是"**所有机 down 都 critical**"。想把 fleet 调松,用已埋好的 `tier=fleet` 标签把规则改成 warning / 更长 `for`。
- 渠道逻辑:接收器 `/Users/local/bin/alert-receiver.py`(macOS + 房间 + Plane)。

## 临时静默(维护某机前)
```sh
/opt/homebrew/bin/amtool --alertmanager.url=http://127.0.0.1:9093 \
  silence add host=node-XX --duration=2h --comment="维护"
```
或临时整体停:`launchctl stop polar.alert-receiver`(恢复 `launchctl start`)。自助静默 UI 留 P2。

## 运维 / 回滚
- 组件(两个 launchd):`polar.alertmanager`(:9093)、`polar.alert-receiver`(:9095)。
- 看 AM 里的告警:`curl http://127.0.0.1:9093/api/v2/alerts`;接收器日志 `/opt/homebrew/var/log/alert-receiver.*.log`。
- 回滚:停两个 plist + 还原 `/opt/homebrew/etc/prometheus.yml.pre-alerting.bak` + reload。

## 已知 / 待办
- 当前 **node-35 离线**(和 .93 同机);"全部 critical" 下笔记本休眠会误报 —— 按需调 tier。
- deadman 外部心跳:**跳过**(zen 全挂 = 关机维护,不算告警)。
- P1/P2:blackbox(HTTPS/DNS/证书到期探活)、LLM 根因归因、Grafana 告警面板、自助静默。

## 产物 / 设计
- 设计稿:`doc/arch/alerting-design.md`。
- 部署产物(state of truth):`deploy/alerting/`(rules + alertmanager.yml + alert-receiver.py + plist 模板 + README)。
- Plane 项目 `polar alarm`(identifier `ALARM`,id `f3e14dad-a103-4d4c-9b46-3fafb95077ee`)。
