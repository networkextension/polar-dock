# LLM Proxy 并发吞吐基准

`scripts/proxy-bench.py` — Python 3.10+ 并发压测脚本，跑出 boss 能读的吞吐 / 延迟 / TTFT 报告。

## 安装

只依赖 `httpx`。**Python 3.12+ 在 macOS 上是 PEP 668 锁的**，所以走 venv：

```bash
python3 -m venv /tmp/bench-venv
/tmp/bench-venv/bin/pip install -q httpx
```

## 跑起来

需要 3 样：

| 参数 | env var | 说明 |
|---|---|---|
| `--token` | `POLAR_PROXY_TOKEN` | plaintext bearer（`polar_proxy_…`，admin 在 `/proxy-tokens.html` 建） |
| `--base-url` | `POLAR_PROXY_BASE_URL` | `https://zen.4950.store/api/proxy/v1` 这种 |
| `--model` | `POLAR_PROXY_MODEL` | 客户端发的 `model` 字段。命中 token 的 alias route 或走 default LLM config |

最快起手：

```bash
export POLAR_PROXY_TOKEN=polar_proxy_xxxxxxxx...
export POLAR_PROXY_BASE_URL=https://zen.4950.store/api/proxy/v1

/tmp/bench-venv/bin/python scripts/proxy-bench.py \
  --model Qwen3_Standard \
  --concurrency-sweep 1,2,4,8,16 \
  --requests-per-level 8
```

报告默认写到 `/tmp/proxy-bench-report.md`（markdown）+ `/tmp/proxy-bench-raw.json`（原始数据）。

## 常用 flag

| flag | 默认 | 说明 |
|---|---|---|
| `--concurrency-sweep` | `1,2,4,8` | 逗号分隔，每个值跑一次 |
| `--requests-per-level` | `10` | 每个并发档样本数。boss 报告用 8-20 比较稳 |
| `--mode` | `both` | `stream` / `nonstream` / `both` |
| `--report` | `/tmp/proxy-bench-report.md` | markdown 输出路径 |
| `--raw` | `/tmp/proxy-bench-raw.json` | 原始 result 数组（per-request 数据） |

## 跑之前先检查什么

| 检查 | 怎么查 |
|---|---|
| **上游活着没** | 看 Grafana `polar_llm_upstream_up{name="…"}`，0 = 死，别跑 |
| **token RPM 够大** | 默认 60。要跑 16 并发 × 8 req = 128 req/min，先把 token 的 `rate_limit_rpm` 改大或设 0（不限）。不然结果里全是 429 |
| **dock 健康** | `curl https://…/metrics` 200 |

## 报告里的指标

| 指标 | 含义 |
|---|---|
| **总吞吐 (completion tokens/s)** | 该批窗口 `Σ completion_tokens / wall_seconds`。**服务总产能**——boss 关心这个 |
| **单请求 P50 (tok/s)** | 每次请求自己的 `completion / e2e_seconds` 中位数。**单流体感**——cursor 用户感受 |
| **E2E P50 / P95 / P99** | 客户端发出到完整响应到达的端到端延迟分位 |
| **TTFT P50 / P95** (仅 stream) | Time-To-First-Token，决定打字机起手是否流畅 |
| **错误分布** | 429 = token RPM 撞墙；502/504 = 上游/nginx 死；inline = stream 200 但内含 error 帧 |

## 怎么读结果

- **总吞吐随并发先增后平** —— 平台位即服务上限。继续加压只会拉高延迟，吞吐不涨
- **P95/P99 ≫ P50** —— 有排队/重试或上游瞬时拥塞。看上游自己的 metrics
- **TTFT P95 > 3s** —— 客户感觉"按了 enter 没反应"。上游 prompt 处理慢，或 first-token batching 配置不对

## 报告样例（节选）

```
## 流式 SSE (stream: true)

| 并发 | 总耗时 | 总吞吐    | OK/Err | E2E P50 | E2E P95 | TTFT P50 | TTFT P95 |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1  |  4.5s | 24.0 tok/s | 8/0  | 560ms  | 720ms  | 180ms | 240ms |
| 2  |  4.6s | 47.2 tok/s | 8/0  | 590ms  | 780ms  | 190ms | 290ms |
| 4  |  4.7s | 91.5 tok/s | 8/0  | 620ms  | 950ms  | 210ms | 380ms |
| 8  |  5.9s | 145 tok/s  | 8/0  | 980ms  | 1.4s   | 260ms | 510ms |
| 16 | 10.2s | 152 tok/s  | 7/1  | 2.1s   | 4.3s   | 380ms | 920ms |
```

平台位在 8-16 之间（吞吐 145→152，延迟从 980ms → 2.1s），同时开始出现 1/8 错。**这台机器舒适并发就在 8 左右**，再上去就是延迟换吞吐了。

## 排错

- **`HTTP 502 + EngineCore encountered an issue`** —— 上游 vLLM/SGLang 崩了。常见 OOM。重启 engine
- **`HTTP 502 + dial tcp ... connection refused`** —— 上游进程没起
- **`empty stream (no content + no usage)`** —— 流式 200 但 body 全空。dock 自己 OK，但上游回了垃圾。一般和上述同源
- **`HTTP 401 invalid_api_key`** —— token 被 revoke 了，或拼写错
- **`HTTP 429 rate_limit_exceeded`** —— 撞 token 的 RPM。看 `Retry-After` header
- **CONNECT tunnel failed**（本机 dev box 跑时）—— 系统 http proxy 不识别 zen.4950.store。脚本已用 `trust_env=False` 绕开，不需要额外配置；若仍出现，确认 venv 里跑的是这个文件

## 跟 dock 的实时指标的关系

`/api/proxy/v1` 的每次调用都会同时打到 dock 的 Prometheus：

- `polar_llm_proxy_e2e_seconds`
- `polar_llm_proxy_ttft_seconds`
- `polar_llm_proxy_completion_tokens`
- `polar_llm_proxy_requests_total{status,token,model,mode}`

bench 是"主动一次性压测出曲线"，Prometheus 是"持续被动观察生产流量"。**两者数据应该对得上**——如果对不上说明 dock 自己有问题。

## 引用文件

- `scripts/proxy-bench.py` — 压测脚本
- `internal/app/dock/llm_proxy_chat_handlers.go` — 服务端实现
- `internal/app/dock/llm_proxy_probe.go` — 上游健康探针（不要跟 bench 混了）
- `scripts/prometheus/llm-proxy-alerts.yml` — 告警规则
- `doc/llm/llm-proxy-design.md` — 整体设计
