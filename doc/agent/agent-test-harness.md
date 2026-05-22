# Agent test harness — standalone skill testing without dock

**Status**: planning + Phase A in progress (2026-05-16).

**Source**: 用户要求 "agent mcp server 可以独立测试，不依赖dock, 设计 1 套
api, 开发 agent client 跑自动化功能测试". 当前调试 VNC / shell 是手动
console.html 点 → 看 log 循环, 每个 iteration 10+ 分钟. 这套 harness 让
任何 skill 可以从 CLI / Go test 一键端到端验证, 不需要浏览器 / dock /
nginx 任何一个.

## 目标

- **standalone**: polar-agent 可以独立跑，不需要 dock 连接
- **同协议**: 复用 dock ↔ agent 已有的 envelope JSON (skill.start /
  skill.stdin / skill.event / skill.stop / skill.resize)——这样测试通过
  的代码上 dock 即可用，无需重复测试
- **scriptable**: 一个 polar-agent-test 二进制 + 命名 scenario, 可手跑
  也可进 `go test` / CI
- **MCP-ready**: 当 mcp-server skill 落地（doc/agent/mcp-skill-dev.md P0-mcp）后,
  天然能用 harness 测 MCP protocol handshake + tools/list + tools/call

## 架构

```
                  ┌────────────────────────────┐
   $ polar-agent-test scenario shell-echo
                  └──────────┬─────────────────┘
                             │ JSON envelopes (line-delimited)
                             │ same wire shape as dock↔agent WS
                  ┌──────────▼─────────────────┐
   $ polar-agent test-serve --addr 127.0.0.1:7077
                  │  TCP listener              │
                  │  ↓ envelope dispatch       │
                  │  ↓ skills registry         │
                  │    (shell, vnc, coder,     │
                  │     kdp, proxy, wg,    │
                  │     mcp-server …)          │
                  └────────────────────────────┘
                  (NO /ws/agent dock connection;
                   NO bot user; NO host_token.
                   Pure skill testbed.)
```

Test-serve 跟 dock 的区别只有 transport: TCP socket 而非 WS upgrade,
hello/list 控制 envelope 替代 dock 的 hello/advertise pair. 业务侧
(skills + envelope routing) 是同一份代码.

## 协议 (test-mode envelopes)

复用 dock 的 5 个 kind:

| kind | dir | 说明 |
|---|---|---|
| `skill.start` | client → agent | `{run_id, skill_kind, config}` |
| `skill.stdin` | client → agent | `{run_id, bytes_b64}` (shell / vnc input) |
| `skill.resize` | client → agent | `{run_id, rows, cols}` (PTY) |
| `skill.stop` | client → agent | `{run_id, reason}` |
| `skill.event` | agent → client | `{run_id, event_kind, data}` |

新增 3 个 test-only kind:

| kind | dir | 说明 |
|---|---|---|
| `ping` | client → agent | liveness probe |
| `pong` | agent → client | reply with version + uptime |
| `list` | client → agent | request registered skills |
| `skills` | agent → client | `{skills: [{kind, version, capabilities}]}` |

Wire format: JSON 一行, 换行分隔 (NDJSON). 没有 length-prefix / framing
overhead — agent_loop 一直用 `json.Decoder`, harness 复用.

## Phase 计划

| Phase | 内容 | 估 LOC | Exit criteria |
|---|---|---|---|
| **A** | test-serve subcommand + JSON envelope server + ping/list scaffold | ~250 Go | `polar-agent-test ping` + `polar-agent-test list` 两条命令 pass |
| **B** | polar-agent-test CLI + 3 scenarios (shell-echo, vnc-handshake, ...) + Go test runner | ~500 Go | `polar-agent-test all` 全绿; `go test ./cmd/polar-agent-test/...` 通过 |
| **C** | mcp-server skill P0 (per doc/agent/mcp-skill-dev.md): scaffold + gdb-mi adapter + 2 tools | ~700 Go | harness 能跑 `mcp-initialize` / `mcp-tools-list` / `mcp-gdb-run` 三个 scenario |
| **D** | CI hook: `make test-harness` 启 test-serve + 跑所有 scenarios + tear down | ~50 Go + Makefile | 单条命令绿/红, PR 阻断在红 |

A → D 串行做. A 是地基, B 是开发体验, C 是 MCP 第一块业务, D 是制度化.

## 文件清单

```
cmd/polar-agent/
  test_server.go       NEW (Phase A) — runTestServer + envelope dispatch
  main.go              MODIFIED — subcommand routing 增 "test-serve"

cmd/polar-agent-test/  NEW (Phase B)
  main.go              arg 解析 + 子命令
  client.go            envelope reader/writer over TCP
  scenarios/
    shell.go           shell-echo: spawn shell, echo "hi", expect "hi" back
    vnc.go             vnc-handshake: spawn vnc, expect "RFB " banner
    mcp.go             (Phase C) mcp-initialize / mcp-tools-list / mcp-gdb-run

cmd/polar-agent/skills/mcp/  NEW (Phase C)
  skill.go             Skill interface impl
  server.go            in-process MCP server (JSON-RPC over stdin/stdout pair)
  adapters/gdb/gdb.go  gdb --interpreter=mi2 adapter

Makefile               NEW (Phase D) — `make test-harness` target

doc/agent/agent-test-harness.md    this file
```

## 安全 / 注意点

- test-serve **只绑 loopback** (127.0.0.1). 不能从 LAN 触达, 防止有人
  通过 test mode 绕过 dock auth 起 shell.
- 端口默认 7077, env `POLAR_AGENT_TEST_ADDR` 可覆盖.
- 无认证, 设计前提是单用户 dev box. CI 容器内启停, 隔离.
- 启动时 stderr 一条 banner: `[test-serve] WARNING — NO AUTH on
  127.0.0.1:7077. dev use only.`
- prod build 应该 `-tags noagenttestserve` 编出来不带这个 subcommand.
  (Phase D 可选.)

## 与 dock 路径的对应关系

```
dock 路径                          test-serve 路径                       一致性
─────────────────────────────────  ──────────────────────────────────   ────
POST /api/.../shell/open     →     {kind:"skill.start", skill_kind:     ✓ 同一份
                                     "shell", config:{...}}              skill.Start()
/ws/host/:id/shell/:run_id   →     {kind:"skill.stdin"} +               ✓ 同一份
                                   {kind:"skill.event", event_kind:      Run interface
                                     "stdout"}
DELETE /sessions/:run_id     →     {kind:"skill.stop"}                  ✓ 同一份
                                                                          run.Stop()
```

因此 Phase A/B 没碰任何 skill 代码 — 纯 plumbing. 通过 harness 验证
过的 skill 行为, 上 dock 时不需要重新跑 e2e.

## 验收

- A 完成时: `polar-agent test-serve` 起, `polar-agent-test ping` 出
  `pong{version, uptime_ms}`, `polar-agent-test list` 出 JSON 里有
  shell + vnc + coder 等
- B 完成时: `polar-agent-test scenario shell-echo` 自动跑通 (启 shell →
  写 `echo hi\n` → 拿到含 "hi" 的 stdout → 发 skill.stop → 拿到 exit
  event → 退出 0). VNC scenario 同理 (但只 assert 收到 RFB banner,
  不做真正的 auth, 因为 macOS Screen Sharing 是外部资源).
- C 完成时: 同样 scenario shape 但 dispatch MCP JSON-RPC, 验证
  `initialize → result, tools/list → [gdb.run, gdb.read_register]`.
- D 完成时: `make test-harness` 单命令绿/红.

## 不在范围内

- dock 端的任何改动 (test-serve 跟 dock 平行)
- 跨语言的 MCP client (用户可以用任何 MCP-aware 客户端测 Phase C —
  Claude Desktop 等).
- UI 自动化测试 (Playwright 是另一刀).
