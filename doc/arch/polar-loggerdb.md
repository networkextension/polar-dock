# polar-loggerdb — 按需取证的主机日志系统（不是日志收集器）

Status: DESIGN (2026-07-05)
Repo: networkextension/polar-loggerdb (新插件模块)
Deps: dock agent_jobs 队列（已上线）、polar-agent、polar-assets（签名 PUT）、plugin 平台（nav / AuthVerifyWS / HMAC）

## 1. 定位：飞行记录仪，不是流水线

排查问题时需要把主机上的 dmesg、bootloader/panic、系统日志、服务日志**拉上来**，
集中看、集中搜；问题解决之后这些日志就是垃圾，不该继续占存储。

所以这不是 Loki/ELK 那种长期采集管道，而是一个 **case（排查会话）为中心、
按需快照、到期自动焚毁** 的系统：

- **按需**：点一下"拉日志"才采集，平时 agent 不上传任何日志。
- **case 生命周期**：所有日志挂在一个 case 上；case 标记 resolved 后进入宽限期，
  到期连同 blob 一起物理删除。
- **有界**：每次采集有单源/单包字节上限，workspace 有总配额。存储压力从设计上封死。

明确**不做**：流式 tail、长期留存、全 fleet 历史检索、日志指标/告警
（指标归 Prometheus 栈）、Elastic/Loki 引入。

## 2. 总体结构

```
 用户(UI: loggerdb.html / hosts 页深链)
   │  POST /api/loggerdb/cases/:id/collect {host_id, sources, since}
   ▼
 loggerdb-svc ──plugin HMAC──▶ dock POST /internal/v1/agent-tasks/enqueue
   ▲                                 │ kind=log_collect, assigned_agent_id=<该主机 agent>
   │                                 │ payload 含一次性 ingest_token
   │                                 ▼  {"kind":"task.wake"} doorbell
   │                            polar-agent（目标主机）
   │                                 │ 本地跑 collector（dmesg/log show/journalctl/...）
   │                                 │ 打包 tar.zst（逐源截断到上限）
   │  POST /api/loggerdb/ingest ◀────┘ 一次性 token 鉴权
   │  （bundle 元数据 + blob 流）
   ├── blob → polar-assets（签名 PUT，字节不落 loggerdb）
   └── 小源解压出 log_lines 行存 PG（可 grep）
        然后 agent → dock POST /api/agent/tasks/:id/complete {bundle_id}
```

复用关系：

| 需求 | 复用 |
|---|---|
| 派发采集任务到指定主机 | dock `agent_jobs`（`assigned_agent_id` 定点 + doorbell，filmscan 同款） |
| 谁能对哪台主机拉日志 | hosts/agents 归属 + AuthVerifyWS(workspace) |
| bundle 原始字节 | polar-assets 签名 PUT（bytes off dock，release/filmscan 同款） |
| 模块接入 | 插件五件套：独立 svc + 独立 DB + nginx `location ^~ /api/loggerdb/` + plugin-modules 注册 + /api/nav 自注册 |

## 3. 数据模型（DB: polar_loggerdb）

```sql
-- 排查会话：一切日志的生命周期锚点
CREATE TABLE log_cases (
  id            TEXT PRIMARY KEY,            -- lc_<ulid>
  workspace_id  TEXT NOT NULL,
  title         TEXT NOT NULL,               -- "zen 开机后 wg-svc 起不来"
  status        TEXT NOT NULL DEFAULT 'open',-- open|resolved
  opened_by     TEXT NOT NULL,               -- user id
  resolved_at   TIMESTAMPTZ,
  purge_at      TIMESTAMPTZ,                 -- GC 依据，见 §5
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 一次采集产出一个 bundle（一台主机、一组源、一个时间点）
CREATE TABLE log_bundles (
  id            TEXT PRIMARY KEY,            -- lb_<ulid>
  case_id       TEXT REFERENCES log_cases(id) ON DELETE CASCADE, -- NULL=孤儿(boot自动抓)
  workspace_id  TEXT NOT NULL,
  host_id       TEXT NOT NULL,               -- hosts 模块 id，UI 反向深链
  agent_id      TEXT NOT NULL,
  trigger       TEXT NOT NULL,               -- manual|boot_panic
  sources       JSONB NOT NULL,              -- 请求的源 + 每源实采字节/截断标记
  captured_at   TIMESTAMPTZ NOT NULL,        -- 主机侧采集时刻
  asset_id      TEXT NOT NULL,               -- polar-assets 里的 tar.zst
  raw_bytes     BIGINT NOT NULL,
  pinned        BOOLEAN NOT NULL DEFAULT false, -- 归档钉住，GC 跳过（罕用）
  purge_at      TIMESTAMPTZ NOT NULL,        -- 孤儿包自己的 TTL；有 case 时随 case
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 小源的行存，供 UI 直接 grep；大源只留 blob（§4）
CREATE TABLE log_lines (
  bundle_id  TEXT NOT NULL REFERENCES log_bundles(id) ON DELETE CASCADE,
  source     TEXT NOT NULL,                  -- dmesg|boot|system|service:wg-svc|file:...
  line_no    INTEGER NOT NULL,
  ts         TIMESTAMPTZ,                    -- 能解析就填，解析不出为 NULL
  text       TEXT NOT NULL,
  PRIMARY KEY (bundle_id, source, line_no)
);
-- 搜索走 (bundle_id, source) 范围内 ILIKE；数据量被 TTL+上限封死，够用。
-- 真嫌慢再加 pg_trgm，不预建。

-- 一次性上传凭据（enqueue 时铸造，ingest 时核销）
CREATE TABLE ingest_tokens (
  token_hash   TEXT PRIMARY KEY,
  bundle_hint  JSONB NOT NULL,               -- {case_id, host_id, agent_id, workspace_id}
  expires_at   TIMESTAMPTZ NOT NULL,         -- 1h
  used_at      TIMESTAMPTZ
);
```

## 4. 采集：agent 侧 collector

### 4.1 job kind

dock 侧一行注册：`validAgentJobKind` 加 `log_collect`（无 chain，无 required_arch，
永远 `assigned_agent_id` 定点——日志只能从"那台"主机拿）。

payload：

```json
{
  "case_id": "lc_...",
  "sources": ["dmesg", "boot", "service:wg-svc"],
  "since":   "2026-07-05T00:00:00Z",
  "max_bytes_per_source": 8388608,
  "max_bytes_total":      67108864,
  "ingest_url":   "https://zone.../api/loggerdb/ingest",
  "ingest_token": "lgi_..."
}
```

### 4.2 源注册表（按 OS 解析，polar-agent 内置）

| source key | macOS | Linux | FreeBSD |
|---|---|---|---|
| `dmesg` | `log show --last <d> --predicate 'sender=="kernel"'`（dmesg 需 root，绕开） | `dmesg -T` | `dmesg` |
| `boot` | `nvram -p` + 上次启动 `log show --boot`; panic: `/Library/Logs/DiagnosticReports/*.panic` | `journalctl -b -1` | `/var/log/messages` 启动段 |
| `system` | `log show --last <d> --style syslog`（谓词过滤，硬截断） | `journalctl --since` / `/var/log/syslog` | `/var/log/messages` |
| `service:<name>` | launchd Std{Out,Err}Path + `~/Library/Logs/<name>*`；覆盖 nohup 场景（`~/.local/log/*`） | `journalctl -u` | `/var/log/<name>*` |
| `file:<glob>` | 白名单 glob（`/var/log/**`, `~/Library/Logs/**`, `~/.local/log/**`），路径穿越拒绝 | 同 | 同 |

每源独立超时（30s）+ 字节上限，超限**保尾弃头**（排查看最近的）并打 `truncated` 标记。
产物 `bundle.tar.zst`：每源一个 `<source>.log` + `manifest.json`（主机名/arch/uptime/采集命令与退出码——命令失败也是线索，记进 manifest 而不是整包失败）。

### 4.3 ingest（agent → loggerdb 直传）

`POST /api/loggerdb/ingest`，`Authorization: Bearer <ingest_token>`（一次性、1h、
scope 锁死 case+host）。multipart：`meta` + `bundle` 流。svc 收到后：

1. 流式转存 assets（插件自己的 provider 凭据），拿 `asset_id`；
2. 解压 ≤8MB 的源进 `log_lines`（`dmesg`/`boot`/panic 基本都很小；大文件跳过，
   UI 搜索时走"从 assets 拉流 + 服务端 grep"兜底）；
3. 落 `log_bundles` 行，核销 token；
4. agent 再 `POST /api/agent/tasks/:id/complete {"bundle_id":"lb_..."}` 闭环队列。

选直传而非 assets 签名 PUT 给 agent 的原因：一次采集要"blob + 行存 + 元数据"三件套
原子落地，经 loggerdb 一跳最简单；包体 ≤64MB，代理转发无压力。

### 4.4 boot/panic 自动抓（P3，可选开关）

agent 启动时发现上次不干净关机（macOS 有新 .panic / Linux `journalctl -b -1 -p err` 非空）
→ 自动采一个小包（`boot` 源，≤4MB）直报 ingest，`trigger=boot_panic`、无 case（孤儿包，
自带 14d TTL）。这是唯一的非人工触发路径，专治"重启完现场没了"。

## 5. 生命周期与 GC —— 本系统的核心

规则（loggerdb 内置 goroutine，每小时跑）：

| 状态 | purge_at |
|---|---|
| case resolved | `resolved_at + 7d`（宽限期内可 reopen，reopen 清空 purge_at） |
| case open 但闲置 | 最后一个 bundle 的 `created_at + 30d`（滚动） |
| 孤儿包（boot_panic） | `created_at + 14d` |
| `pinned=true` | 永不 GC（UI 上显式"归档"才置位，默认没有） |

GC 动作：删 assets blob（走 assets DELETE）→ 删 `log_bundles`（级联 `log_lines`）→
case 无 bundle 且过期则删 case。**删是物理删**，没有回收站。

配额：workspace 总 `raw_bytes` 上限（默认 2GiB，env 可调）。超限时新 collect 直接 4xx
提示先 resolve 旧 case，**不做静默淘汰**——排查中的数据不能被另一次排查挤掉。

## 6. API（/api/loggerdb/*，AuthVerifyWS + X-Workspace-Id）

```
POST  /cases                          开 case {title, host_ids?}
GET   /cases?status=                  列表（含 bundle 数/字节数/purge 倒计时）
PATCH /cases/:id                      {status: resolved|open} → 置/清 purge_at
POST  /cases/:id/collect              {host_id, sources[], since?} → 铸 token + enqueue → {job_id}
GET   /cases/:id/bundles              该 case 的采集时间线
GET   /bundles/:id/sources/:src/lines?q=&regex=&limit=   行存 grep；无行存则拉 blob 流式 grep
GET   /bundles/:id/download           302 → assets 签名 URL（拿原始 tar.zst）
POST  /bundles/:id/pin                归档钉住 / 取消
POST  /ingest                         （agent，一次性 token）见 §4.3
GET   /healthz                        含 POLAR_LOGGERDB_VERSION
```

UI（loggerdb.html，nav 自注册）：case 列表（带"N 天后焚毁"角标）→ case 详情
（主机×时间 bundle 网格）→ 查看器（源 tab + grep 框 + 时间过滤 + truncated 提示）。
hosts 页每台主机加"拉日志"深链（带 host_id 预填），对齐 wg↔hosts 交叉链模式。

## 7. 部署形态

插件五件套，照 zen playbook：二进制 `~/.local/bin/loggerdb-svc`、端口 **8108**、
DB `polar_loggerdb`（DSN 用 LAN IP，launchd-PG gotcha）、nginx `location ^~ /api/loggerdb/`、
POST /api/admin/plugin-modules 注册 + workspace 授权。零 dock 改动除了
`validAgentJobKind` 加一个 kind（1 行 + 常量）。

## 8. 阶段

| 阶段 | 内容 | 改动面 |
|---|---|---|
| **P0** | schema + cases/bundles CRUD + ingest + assets 转存 + GC/配额 | 新 repo loggerdb-svc |
| **P1** | dock 注册 `log_collect` kind；polar-agent collector（3 OS 源表 + 打包 + 直传） | dock 1 行；polar-agent 一个 skill |
| **P2** | UI：case 流 + 查看器 grep + hosts 深链；blob 流式 grep 兜底 | loggerdb + hosts-ui |
| **P3** | boot/panic 自动抓（默认关，env 开） | polar-agent |
| **P4** | pin 归档 + purge 倒计时提醒（DM bot 通知"case X 明天焚毁"） | loggerdb |

P0+P1 即可用（curl 建 case → collect → psql/API grep），UI 在 P2。
