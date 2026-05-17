# Polar Ops + Demo Playbook

最近一轮迭代后的"怎么用 Polar"参考。按场景翻，不按代码翻。
对应代码 / 设计文档放在每节末尾。

---

## 速查表

| 你想做 | 看哪一节 |
|---|---|
| 给客户开 OpenAI-compat API key | [§1 LLM Proxy](#1-llm-proxy-openai-compat-外部-api) |
| 客户喊"Polar 慢" | [§5.1 排错路径](#51-llm-慢) |
| 看本月烧了多少 LLM 钱 | [§1.3 用量 + 账单](#13-用量--账单) |
| 上线一台新 mac/linux 操作员机 | [§4 新机器 onboard](#4-新机器-onboard-checklist) |
| 想给 LLM 在调试时调用知识库 | [§2 Knowledge base](#2-knowledge-base) |
| 插 iPhone 自动注册 | [§3 iOS recovery 设备](#3-ios-recovery-设备自动捕获) |
| Grafana 没数据 | [§5.4 prometheus / grafana 链路](#54-prometheusgrafana-看不到数据) |

---

## 1. LLM Proxy（OpenAI-compat 外部 API）

`POST /api/proxy/v1/chat/completions` 是站点的 OpenAI 替代品。
任何能配 base_url 的客户端（Cursor / Raycast / Continue.dev /
Python `openai` SDK / curl）一行接入。

### 1.1 给客户开 token

UI：登录后 sidebar → 🔑 API Tokens → `+ 新建 Token` → 配置：

- 名称（用于审计 + 列表识别）
- 默认 LLM 配置（必填）
- RPM (默认 60，0=不限)
- 月度 USD 限额 (默认 0=不限)

提交后弹一次 plaintext bearer，**只显示一次**。复制走。

CLI（用 admin cookie，少见）：

```bash
curl -X POST https://zen.4950.store/api/llm-proxy/tokens \
  -H "Content-Type: application/json" \
  -b "polar_session=...; polar_access=..." \
  -d '{"name":"cursor-bob","default_llm_config_id":3,"rate_limit_rpm":60}'
```

### 1.2 客户怎么用

```bash
# OpenAI Python SDK
from openai import OpenAI
client = OpenAI(api_key="polar_proxy_xxx", base_url="https://zen.4950.store/api/proxy/v1")
client.chat.completions.create(model="any", messages=[{"role":"user","content":"hi"}])
```

```bash
# curl 直接打
curl https://zen.4950.store/api/proxy/v1/chat/completions \
  -H "Authorization: Bearer polar_proxy_xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"any","messages":[{"role":"user","content":"hi"}]}'

# Qwen-3 家族关 thinking（10× 提速 + 100× 减 token）
curl ... -d '{"model":"any","messages":[...],"chat_template_kwargs":{"enable_thinking":false}}'
```

`stream:true` 走 SSE，按 OpenAI chat.completion.chunk 协议输出
（role frame → content×N → finish → 可选 usage → `data: [DONE]`）。

### 1.3 用量 + 账单

- **Dashboard widget**：`/dashboard.html` 的 "LLM Proxy" 卡片
  显示本月 USD + 活跃 token 数 + 上游可用率
- **per-token 审计**：`/proxy-tokens.html` → 任意 token → "审计"
  按钮 → 弹层显示最近 100 次调用 + 按 model 7d 聚合 (调用数 /
  ok / fail / prompt / completion / total tokens / 估算 USD)
- **Grafana**：`polar_llm_proxy_e2e_seconds` / `_ttft_seconds` /
  `_completion_tokens` / `_requests_total` 都是 histogram + counter
- **DB 直查**：`llm_proxy_calls` 每次调用一行，永久存

### 1.4 别名路由（一个 token 跨多个 model）

`/proxy-tokens.html` → 任意 token → "路由" 按钮 → 弹层加：

| 别名 | → 上游 LLM |
|---|---|
| `gpt-4o-mini` | OpenAI · gpt-4o-mini |
| `polar:research` | Gemini 2.5 Pro |
| `claude-haiku` | local Anthropic 镜像 |

客户端发 `"model": "polar:research"` → 路由命中 Gemini；
都没命中 → 走 token 默认 LLM。

### 1.5 速率 + 月度限额

- `rate_limit_rpm` — 60s 滑动窗口；超 → HTTP 429 + `Retry-After`
- `monthly_quota_usd` — UTC 月内 `SUM(cost_usd)` 超 → HTTP 402
- 都打 Prometheus counter `polar_llm_proxy_quota_exceeded_total` /
  `polar_llm_proxy_requests_total{status="429"}`

USD 估算来自 `internal/app/dock/llm_proxy_pricing.go` 的硬编码价
格表（7 家 provider × ~25 model prefix）。**最权威账单仍是 provider
自己的 invoice**——这里只是趋势 + 安全网。

### 1.6 设计 / 代码参考

- `doc/llm-proxy-design.md` — 整体设计
- `internal/app/dock/llm_proxy_*.go` — handler / store / pricing / quota / rate_limit / probe
- `scripts/proxy-bench.py` — 经过 Polar 链路压测
- `scripts/upstream-bench.py` — 直连上游对比测
- `scripts/prometheus/llm-proxy-alerts.yml` — 告警规则
- `scripts/nginx/llm-proxy-snippet.conf` — nginx vhost 片段

---

## 2. Knowledge base

3 张全局表（**不按 workspace 隔离** — 故意，要"操作员之间网络效应"）：

| 表 | 内容 |
|---|---|
| `rev_devices` | 硬件清单 (ECID + cpid/bdid/cprv + chip/board/os) |
| `rev_firmwares` | 固件镜像 (kind + version + compat[] + sha256 + 本地 blob) |
| `rev_functions` | 反编译函数 (firmware_id + address + symbol + prototype + byte_signature) |

### 2.1 录入

**Devices**：
- 自动：插 iOS 设备进 recovery → polar-agent 5s 内自动 upsert（见 §3）
- 手工：`/library.html` → 📱 Devices → `+ 新建`

**Firmwares**：
- `/library.html` → 💾 Firmwares → `⬆ 上传固件`
  → 选文件 + kind + version + 兼容 cpid[]
  → 服务端按 sha256 内容寻址存到 `<uploadDir>/firmwares/<sha[:2]>/<sha>`
  → 重复上传同一 blob 直接 409 + 返回已有 sha
- 已有的元数据 only（blob 单独 rsync）：`POST /api/library/firmwares`

**Functions**：
- 目前只有 REST：`POST /api/library/functions` (admin cookie)
- UI add modal 还没写（操作员实际跑 lldb 时填进来才有意义）

### 2.2 LLM 怎么用（MCP `library` adapter）

操作员的 host_skill config：

```jsonc
{ "adapters": ["echo", "library"] }
```

LLM 在 debug loop 里直接调（9 个工具）：

```
library.devices.list           library.firmwares.list
library.devices.get            library.firmwares.get
library.devices.recent_seen    library.firmwares.matching
                               library.functions.lookup_by_address  ← 最常用
                               library.functions.lookup_by_symbol
                               library.functions.search
```

典型用法：LLM 看 PC=0x80001234 → 调
`library.functions.lookup_by_address(firmware_id=1, address="0x80001234")`
→ 一次 round-trip 拿到 `iboot_main`，不用把整个 firmware binary
灌进 context。

### 2.3 设计 / 代码参考

- `doc/jtag-debug-skill-design.md` §3.5 — 知识库部分设计
- `internal/app/dock/rev_*.go` — store / handlers / blob upload
- `cmd/polar-agent/skills/mcp_adapter_library.go` — 9 个 MCP 工具
- `ui/src/library.ts` + `ui/public/library.html` — 3-tab admin UI

---

## 3. iOS recovery 设备自动捕获

**前提**：操作员机器装了 polar-agent + `irecovery`（macOS:
`brew install libimobiledevice`）。install script 自动装。

**用法**：
1. 操作员机跑着 polar-agent (`polar-agent attach …`)
2. 插 iPhone/iPad 进 DFU / Recovery 模式
3. ≤5s 后 `/library.html` Devices tab 出现这台机器，ECID + iBoot
   版本全有
4. 拔了 → 下次轮询无设备，行不删；`last_seen_at` 自然停止刷新

**关掉**：`POLAR_AGENT_RECOVERY_DISABLED=true` env

**代码**：`cmd/polar-agent/recovery_watcher.go`

---

## 4. 新机器 onboard checklist

### 4.1 操作员 Mac/Linux（跑 polar-agent）

1. admin 在 dock UI 上：`/hosts.html` → 注册 → 拿一次性 enroll token
2. 操作员一行装：
   ```bash
   curl -fsSL https://zen.4950.store/api/agent/install/$ENROLL_TOKEN | sh
   ```
   自动装：polar-agent 二进制 + login + libimobiledevice (macOS) + 选的 coder (kimi/claude/codex)
3. 起 attach（按提示）

### 4.2 新 dock 部署（罕见，单机 mac 部署）

跟 `doc/internal/app/`（参考材料）+ memory 的 deploy-box note：

```bash
# 1. brew install postgresql@18 redis nginx
# 2. start postgres + redis
# 3. 建库:
psql -d postgres -c "CREATE ROLE ideamesh LOGIN PASSWORD 'test123456';"
psql -d postgres -c "CREATE DATABASE ideamesh OWNER ideamesh;"
# 4. 拉 binary + UI dist 过来
rsync polar-dock local@new-box:~/.local/bin/
rsync ui/dist/ local@new-box:~/github/Polar-/ui/dist/
# 5. 配 launchd + nginx vhost (参考 scripts/launchd/ + scripts/nginx/)
# 6. 启动观测栈（prometheus + grafana + 4 exporters）
```

### 4.3 上游 LLM 加新 model

UI: admin → `/bots.html` → LLM Configs → 新建：

| 字段 | 例 |
|---|---|
| name | `kimi-test` |
| base_url | `https://api.moonshot.cn/v1/chat/completions` |
| model | `kimi-k2.5` |
| api_key | `sk-...` |
| provider_kind | `text` |

立刻生效——30s 内 prober 会试它一下，`polar_llm_upstream_up`
gauge 会出现。给 token 加默认/别名 routes 就能用。

---

## 5. 常用排错路径

### 5.1 LLM 慢

**步骤**（约 5 分钟出结论）：

1. **Grafana**：看 `polar_llm_upstream_up{name="..."}` 是不是 1
2. **看 bench**：
   ```bash
   # 经过 Polar
   POLAR_PROXY_TOKEN=… POLAR_PROXY_BASE_URL=… \
     python3 scripts/proxy-bench.py --model X --concurrency-sweep 1,2,4 --requests-per-level 4
   # 直连上游绕过 Polar
   UPSTREAM_URL=… UPSTREAM_API_KEY=… UPSTREAM_MODEL=X \
     python3 scripts/upstream-bench.py --concurrency-sweep 1,2,4 --requests-per-level 4 --disable-thinking
   ```
3. 对比表格：
   | 现象 | 锅在 |
   |---|---|
   | 两个都慢 | 上游 vLLM 配置（`--max-num-seqs` 太小 / `--max-num-batched-tokens` 不够 / 显存吃紧） |
   | proxy 慢、direct 快 | Polar 链路（dock / nginx）。看 dock log + nginx error log |
   | 都 timeout / 502 | 上游进程死了，去那台机看 vLLM/SGLang 是否在跑 |
4. 客户 Qwen-3 系 → 一定确认带 `chat_template_kwargs:{enable_thinking:false}`

### 5.2 上游死

- Grafana 自动 critical alert `LLMUpstreamDown` 应该已经触发（如果配了 alertmanager）
- 手工查：
  ```bash
  curl https://zen.4950.store/metrics | grep llm_upstream_up
  ```
  `= 0` 的就是死的
- 修：去对应 inference 主机重启 vLLM / SGLang process
- 修好后 polar-agent prober 30s 内自然恢复 gauge

### 5.3 Token 滥用 / 跑额度

UI: `/proxy-tokens.html` → token 行 → "审计" → 看是哪个 model 在烧
- 短期：调低 RPM / 设月度 USD 限额
- 长期：✕ 撤销 token 立即生效（客户下一次调用即收 401）

### 5.4 prometheus/grafana 看不到数据

1. `ssh local@.57 'curl -s -H "Authorization: Bearer $METRICS_TOKEN" http://127.0.0.1:8080/metrics | head -5'`
   → dock /metrics 活着没
2. `curl http://127.0.0.1:9090/api/v1/targets | jq '.data.activeTargets[] | {job, health}'`
   → prometheus scrape 健康
3. `/grafana/` 数据源 → Postgres `polar_grafana` 角色（只读）+ Prometheus
4. dashboards 在 `scripts/grafana/dashboards/`（如果还没 import）

---

## 6. 常用 API 速查

| 接口 | auth | 用途 |
|---|---|---|
| `POST /api/proxy/v1/chat/completions` | bearer `polar_proxy_*` | LLM 调用 |
| `GET /api/system/info` | **public** | 部署工具探活（OS / IP / 磁盘 / 内存 / 版本） |
| `GET /api/llm-proxy/tokens` | admin cookie | token 列表 |
| `POST /api/llm-proxy/tokens` | admin cookie | 建 token（一次性返 plaintext） |
| `DELETE /api/llm-proxy/tokens/:id` | admin cookie | 撤销 |
| `GET /api/llm-proxy/tokens/:id/calls/aggregate?days=7` | admin | 7d 按 model 聚合 |
| `GET /api/llm-proxy/dashboard-stats` | admin | dashboard 卡片数据 |
| `GET /api/library/devices` | logged-in | 设备列表 |
| `GET /api/library/firmwares/matching?cpid=X&bdid=Y` | logged-in | 兼容固件 |
| `GET /api/library/functions/lookup-by-address?firmware_id=X&address=0x80001234` | logged-in | LLM 调试最常用 |
| `POST /api/library/firmwares/upload` | admin | multipart 上传固件 |
| `POST /api/agent/library/devices/upsert` | agent bearer | recovery watcher 用 |

---

## 7. Prometheus 关键指标

| metric | 类型 | 标签 | 说明 |
|---|---|---|---|
| `polar_llm_proxy_requests_total` | counter | token, model, mode, status | 计数 |
| `polar_llm_proxy_e2e_seconds` | histogram | token, model, mode, status | 端到端延迟 |
| `polar_llm_proxy_ttft_seconds` | histogram | token, model | 流式首 token 到达 |
| `polar_llm_proxy_completion_tokens` | histogram | token, model | 每次 completion 大小 |
| `polar_llm_proxy_quota_exceeded_total` | counter | token | 月度限额命中 |
| `polar_llm_upstream_up` | gauge | config_id, name, model | 1=活 0=死，prober 每 30s 刷 |
| `polar_db_pool_open/in_use/idle` | gauge | — | DB 连接池 |
| `polar_goroutines` | gauge | — | dock 进程 goroutine 数 |
| `polar_http_requests_total` | counter | method, path, status | 通用 HTTP |

---

## 8. Alertrules（已部署到 .57 prometheus）

来自 `scripts/prometheus/llm-proxy-alerts.yml`：

| Alert | 触发 | 严重度 |
|---|---|---|
| **LLMUpstreamDown** | `upstream_up == 0 for 2m` | critical |
| LLMProxyErrorRateHigh | non-2xx > 10% for 5m | warning |
| LLMProxyP95LatencyHigh | nonstream P95 > 60s for 10m | warning |
| LLMProxyTTFTHigh | stream TTFT P95 > 3s for 10m | warning |

接 alertmanager 还没装（看 PR #222 commit footer）。装好后就推 ntfy/iOS push/钉钉。

---

## 9. 部分故意没做的事

- **alertmanager 实际通知通道**（ntfy / iOS push） — 装好 rules 但没接收端，下次值班自己加
- **firmware binary diff** — 设计有但 P1 没做，需要时再说
- **MCP `lldb` / `jtag` adapter** — 需要 host 装 LLDB + JTAG 探针硬件，等真有 demo 再做
- **token 自动轮转** — 长生命周期 token 是隐患，未来加 90 天过期 + webhook
- **多 box dock cluster** — 当前单 dock 单 box，多 box 没设计

---

## 10. 出问题找谁

- proxy / 知识库 / agent 代码：从 `internal/app/dock/` `cmd/polar-agent/` 入手
- nginx / launchd / postgres：看 `doc/internal/`（git-ignored，本地）
- 部署 / 重启脚本：`~/restart-polar-dock.sh` on .57

---

## 11. 这文档怎么维护

新功能合并 PR 时，对应章节加 2-3 行 + 链一下 PR 号。
**不要让这个文档落后**——它是值班手册的 root。
