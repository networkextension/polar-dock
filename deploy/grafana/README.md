# Grafana / Prometheus 接入

Phase 1 的目标是把 dock 的运行时健康指标接到一个本地 Grafana。本目录是
**配置模板**，不是要 docker-compose 起一整套栈 —— 假定你已经本地有
Grafana（dev 默认 `localhost:3100` admin/admin）+ Prometheus。

## 一次性配置

### 1. 给 dock 设 token

dock 的 `/metrics` 端点必须用 `Authorization: Bearer <token>` 拉。token
从 env 注入，未设 = endpoint 直接 404。

```bash
export METRICS_TOKEN="$(openssl rand -hex 32)"
# 启动 dock 时 export 上面这一行；token 也要同时配进 prometheus.yml
```

### 2. 配 Prometheus

复制 `prometheus.yml` 到你的 Prometheus 实例，把 `REPLACE_WITH_METRICS_TOKEN`
换成上一步的真值，重启 Prometheus。

预期 target：

- 同机：`127.0.0.1:8080`
- Prometheus 在 docker、dock 在 host：`host.docker.internal:8080`

### 3. 在 Grafana 加 Prometheus datasource

`localhost:3100` admin/admin → Connections → Data sources → Add → Prometheus
→ URL 填你的 Prometheus（默认 `http://localhost:9090`）→ Save & test。

datasource UID 建议设成 `prometheus`，这样下一步导入 dashboard JSON 不用
手改。

### 4. 导入 dashboard

Grafana → Dashboards → New → Import → Upload JSON file →
选 `dashboards/polar-overview.json` → 选刚加的 Prometheus
datasource → Import。

应该看到 5 个 panel：

- HTTP request rate（按路由）
- HTTP error rate (5xx)
- p50 / p95 / p99 延迟
- Goroutines
- DB pool 三件套（open / in_use / idle）

### 4b. 导入 LLM dashboard（Phase 2）

同样路径再 import `dashboards/polar-llm.json`。6 个 panel：

- 调用 rate（按 endpoint：thread_summary / task_retrospect / chat / 等）
- 错误率（按 provider / model）
- 延迟 p50 / p95 / p99（用秒级 histogram bucket，覆盖 100ms - 2min）
- p95 by endpoint
- token rate（prompt / completion）
- 24h token 累计

**说明**：token 统计依赖上游响应里的 `usage` 块。OpenAI 兼容路径
（Kimi / DeepSeek / OpenAI / Volces 非 Responses）会有；Anthropic
Messages / Gemini / XAI Responses / 流式 SSE 暂不解析 —— 那些 endpoint 的
token panel 显示为 0 是预期。

## 验证

dock 启动后跑：

```bash
curl -fsSL -H "Authorization: Bearer $METRICS_TOKEN" http://localhost:8080/metrics | head -30
```

应看到 `polar_http_requests_total`、`polar_goroutines` 等指标。然后随便
点几下 dashboard，等 ~30s（scrape 周期），曲线应该开始动。

## 常见问题

- **Grafana 看不到数据** —— 先去 Prometheus UI（`http://localhost:9090/targets`）
  确认 `polar-dock` target 是 UP。如果是 DOWN 看 Last Error。
- **`Last Error: server returned HTTP 401`** —— `prometheus.yml` 里的
  bearer token 跟 dock 的 `METRICS_TOKEN` 没对上。
- **`Last Error: server returned HTTP 404`** —— dock 的 `METRICS_TOKEN`
  没设。fail-closed 设计，必须显式 opt-in。
- **dashboard panel 显示 "No data"** —— 最常见是 datasource UID 不叫
  `prometheus`。要么改 datasource UID，要么 import 时手动选你那个
  datasource。

## 后续 phase

见 `doc/grafana-integration.md`。Phase 2/3/4 会在这个目录里加更多
dashboard JSON（LLM / agents / business）。
