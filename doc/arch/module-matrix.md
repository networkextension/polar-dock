# Polar 项目/模块矩阵

> 数据来源：zen `plugin_modules` 注册表 + per-module Postgres + nginx server_names。生成于 2026-06-21。
> 重新生成：`ssh zen psql -d ideamesh -c "SELECT name, endpoint, version, ui_routes, last_heartbeat FROM plugin_modules ORDER BY name"`。

## 分层架构

```
客户端 (iOS·Mac·Web·Agent)
        │
   nginx 边缘 :2443/:443  (*.4950.store 通配证书)
        │
   dock :8080  ── 核心：身份 · LLM 代理 · 协作 · OAuth/OIDC   [DB=ideamesh + redis]
        │
   ├─ 数据/控制面:  assets:8099(内容寻址)  release:8101(发布登记)  zone:8108(联邦)
   ├─ 网络/基础:    wg:8090  hosts:8095  latch:8098  packtunnel:8093  dns:8107
   └─ 产品垂直:     buildings lawyer screen film music video library
                    event expense projects iosdist stock 3dfactory
        │
   per-module Postgres (polar_*)   ·   字节面 assets 直传客户端（绕过 dock）
```

## 模块矩阵

| 模块 | 端口 | DB | 子域 (`*.4950.store`) | UI | 角色 |
|---|---|---|---|---|---|
| **dock** | 8080 | ideamesh | `zen` | 核心UI | 身份·LLM·协作核心 |
| assets | 8099 | (dock) | `assets-zen` | ✓ | 内容寻址数据面 |
| release | 8101 | polar_release | `release` | ✓ | 发布/分发登记 · ed25519 |
| zone | 8108 | polar_zone | `zone` | ✓(tabs) | 联邦/副本控制面 |
| wg | 8090 | polar_wg | `wg` | ✓ | Mesh VPN |
| hosts | 8095 | polar_hosts | `hosts` | ✓ | 主机/agent 队列 |
| latch | 8098 | polar_latch | `latch` | ✓ | 代理/规则 |
| packtunnel | 8093 | polar_packtunnel | `packtunnel` | — | 隧道 |
| dns | 8107 | polar_dns | `dns` | ✓ | DNS |
| buildings | 8100 | polar_buildings | — | — | 楼宇 |
| lawyer | 8102 | polar_lawyer | `lawyer` | ✓ | 律所 RAG |
| screen | 8105 | polar_screen | `screen` | ✓ | DOOH 广告屏 |
| film | 8106 | polar_film | `film` | ✓ | 影视/台词/人脸 |
| music | 8104 | polar_music | `music` | ✓ | 私有乐库 |
| video | 8092 | polar_video | `video` | ✓ | 视频工作室 |
| library | 8094 | polar_library | `library` | ✓ | 资料库 |
| event | 8110 | polar_event | `event` | ✓ | 日程/ICS |
| expense | 8097 | polar_expense | `expense` | ✓ | 家庭账本 |
| projects | 8096 | polar_projects | `project`(单数) | ✓ | 项目 |
| iosdist | 8091 | polar_iosdist | `iosdist` | ✓ | iOS 分发 |
| stock | 8103 | polar_stock | `stock` | ✓ | 股票 |
| 3dfactory | 8109 | polar_3dfactory | — | ✓ | 3D 打印工厂 |
| photo | (dev) | polar_photo | `photos.dev` | ✓ | 照片库（仅 dev box，未上 zen） |

**第三方 on zen**：`plane`:8000(makeplane 追踪器) · `webmux`:8181(Web 终端)。

**库/repo（非运行服务）**：polar-sdk · polar-sdk-swift · polar-sdk-py · polar-ui-common · polar-dock-ui · polar-dataflow · polar-agent · polar-task(设计稿)。

## 注意
- **跑服务但无公网子域**：`buildings`、`3dfactory`。
- **端口纠偏**：`lawyer=8102`（非 8099），`assets=8099`，`event=8110`/`3dfactory=8109` 为最新两个。
- **photo** 仅在 dev box（10.88.0.5），未部署到 zen。
- 端口分配：8090–8110 连续段，dock 独占 8080。
