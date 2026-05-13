# MCP-Server Skill — Development Plan

**Status**: planning. P0-mcp starts after this doc + the design doc land.

**Companion**: [mcp-skill-design.md](./mcp-skill-design.md) — the "why"
and the architecture. This doc is the "how" and "in what order".

**Slots after**: Host module P0 (PR #181, merged) provided the
foundation. Coder P1 and Shell P2 are independent — MCP-Server can
land before, during, or after them; no hard ordering.

## Phase plan

Eight phases. P0-mcp ships the wire end-to-end with one adapter; each
later phase adds one capability and is independently mergeable.

| Phase | Scope | Est LOC | Depends on | Risk |
|---|---|---|---|---|
| P0-mcp | Skill scaffold + gdb-mi adapter + 2 tools + dock direct-call REST + smoke | 700 Go + 0 TS | Host P0 | high (foundation; new transport bridge) |
| P1-mcp | LLM-agent tool injection: dock side calls `mcp.list_tools`, injects into `ai_agent.go` tool array | 250 Go | P0-mcp | medium (LLM tool-loop integration) |
| P2-mcp | LLDB DAP adapter (attach / breakpoint / step / read_mem) | 400 Go | P0-mcp | medium (DAP framing) |
| P3-mcp | Session manager: agent-side, idle eviction, `mcp.list_sessions` tool | 200 Go | P0-mcp | low |
| P4-mcp | IDA-Python RPC adapter — connects to in-IDA plugin port | 500 Go + 200 Py | P0-mcp + P3-mcp | high (operator must install IDA plugin) |
| P5-mcp | Frida + radare2 adapters | 350 Go | P0-mcp + P3-mcp | low |
| P6-mcp | Semantic cache + action-graph (Redis-backed) | 300 Go | P3-mcp | medium (memory model) |
| P7-mcp | Hosts UI: MCP catalogue per host, session list, tool playground | 200 Go + 500 TS | P1-mcp..P3-mcp | low |

P0-mcp + P1-mcp are the gates. Once those merge, P2 through P6 can be
parallelized; P7 (UI) waits on enough catalogue surface to be worth
visualizing.

## Phase 0-mcp — Foundation + first adapter

### Tasks

| # | Task | File |
|---|---|---|
| 0.1 | `Skill` impl for `mcp-server` kind; `init()` registers in registry | `cmd/polar-agent/skills/mcp/skill.go` (new) |
| 0.2 | In-process MCP server: tool dispatch, schema export | `cmd/polar-agent/skills/mcp/server.go` (new) |
| 0.3 | WS transport bridge — pump JSON-RPC frames between MCP server and agent WS | `cmd/polar-agent/skills/mcp/transport_ws.go` (new) |
| 0.4 | Adapter registry: name → adapter; loaded from skill config `adapters: [...]` array | `cmd/polar-agent/skills/mcp/adapters.go` (new) |
| 0.5 | `gdb` adapter: spawn `gdb --interpreter=mi2`, parse MI output | `cmd/polar-agent/skills/mcp/adapters/gdb/gdb.go` (new) |
| 0.6 | `gdb.run` tool — start program with breakpoints, wait for first stop | same |
| 0.7 | `gdb.read_register` tool — read named register from current frame | same |
| 0.8 | New WS message kinds on dock side: `mcp.tool.call` / `mcp.tool.result` / `mcp.list_tools` | `internal/app/dock/agent_hub.go` (extend kind switch) |
| 0.9 | Dock direct-call handler — `POST /api/hosts/:id/skills/mcp/tools/:tool` | `internal/app/dock/host_skill_handlers.go` (new) |
| 0.10 | Route wiring | `internal/app/dock/app.go` (+3 lines) |
| 0.11 | Adapter test: `gdb.gdb_test.go` — run /bin/echo through gdb, assert exit status comes back | `*_test.go` |
| 0.12 | Server test: `server_test.go` — round-trip a fake adapter's tool call through the WS bridge | `*_test.go` |

### Out of scope for P0-mcp

- LLM-agent integration (P1-mcp)
- Persistent sessions beyond a single tool call (P3-mcp)
- Any non-gdb adapter (P2/P4/P5-mcp)
- UI surfaces (P7-mcp)
- Audit logging to `chat_messages` (open question in design §10)

### Acceptance

- On a polar-agent host with `mcp-server` skill enabled and config
  `{"adapters":["gdb"]}`:
  1. `skill.advertise` from agent includes `{"kind":"mcp-server",
     "capabilities":{"adapters":["gdb"],"tools":["gdb.run",
     "gdb.read_register"]}}`
  2. `curl -X POST .../api/hosts/<id>/skills/mcp/tools/gdb.run -d
     '{"program":"/tmp/answer","breakpoints":["main"]}'` returns
     `{"session_id":"...","stop_reason":"breakpoint-hit",...}`
  3. Follow-up `gdb.read_register` returns a hex value
- Old polar-agent + old dock keep working — the new message kinds
  are additive and unknown kinds are already ignored on both sides.
- `go test ./cmd/polar-agent/skills/mcp/...` clean.
- No schema changes — sessions are agent-memory-only in P0-mcp.

## Phase 1-mcp — LLM-agent tool injection

### Tasks

| # | Task | File |
|---|---|---|
| 1.1 | `mcpTools` provider — on chat thread start, query each MCP-enabled host's `mcp.list_tools`, cache in dock memory keyed by `host_id`. Invalidate on `skill.advertise` re-receive or skill restart. | `internal/app/dock/mcp_tools.go` (new) |
| 1.2 | Tool name namespacing: `<host_slug>.<tool>` when ≥2 hosts advertise the same tool; bare `<tool>` when exactly one host advertises it | same |
| 1.3 | Inject into LLM tool array in `ai_agent.go::buildToolsForRequest` | extend |
| 1.4 | Tool call dispatch on LLM response: parse `<host>.<tool>` → look up `agentConn` by host_id → `mcp.tool.call` over WS → wait for result | `ai_agent.go` (extend tool-loop branch) |
| 1.5 | Tool result formatting back into the LLM context: structured JSON in `tool_result` block, no string flattening | same |
| 1.6 | Per-tool timeout enforcement (default 30s, per-tool override via adapter capability) | `mcp_tools.go` |
| 1.7 | Tests: stub MCP server returning canned `tools/list` + `tool/call` results; assert LLM call gets tools[], tool_result fan-out works | `mcp_tools_test.go` |

### Acceptance

- A workspace with one host advertising `gdb.run` shows `gdb.run` in
  the LLM's tool array on the next chat message in that workspace.
- LLM tool_use with `{"name":"gdb.run","input":{...}}` reaches the
  host, runs gdb, returns a `tool_result` block to the LLM.
- Two hosts advertising the same `gdb.run` triggers namespacing:
  `host1.gdb.run` + `host2.gdb.run`.
- Zero hosts → no `mcp.*` tools injected, no errors.

## Phase 2-mcp — LLDB DAP adapter

### Tasks

| # | Task | File |
|---|---|---|
| 2.1 | LLDB DAP framing: spawn `lldb-dap`, speak DAP JSON-RPC over stdio | `cmd/polar-agent/skills/mcp/adapters/lldb/lldb.go` (new) |
| 2.2 | `lldb.attach` tool — `{program, args?}` → DAP `launch` request | same |
| 2.3 | `lldb.set_breakpoint` tool — `{session_id, location}` → DAP `setBreakpoints` | same |
| 2.4 | `lldb.continue` / `lldb.step` / `lldb.next` — DAP `continue`/`stepIn`/`next` | same |
| 2.5 | `lldb.read_memory` tool — DAP `readMemory` | same |
| 2.6 | Stop-event aggregation: DAP emits `stopped` async; adapter packages it into the next tool's result OR an event channel | same |
| 2.7 | Test: launch /bin/echo, single-step through main, assert PC advances | `lldb_test.go` |

### Acceptance

- LLDB adapter listed in `mcp.list_tools` when enabled.
- Mac dev box + iPhone tether → can debug an iOS process and read
  memory via LLM tool calls.

## Phase 3-mcp — Session manager

### Tasks

| # | Task | File |
|---|---|---|
| 3.1 | `sessions.go` — `Manager` struct: map[session_id]Session, idle TTL eviction goroutine | `cmd/polar-agent/skills/mcp/sessions.go` (new) |
| 3.2 | Adapter API change: `Attach()` returns `session_id`; all other tools take `session_id` as first arg | adapter packages |
| 3.3 | `mcp.list_sessions` tool — returns active sessions across all adapters | `server.go` |
| 3.4 | `mcp.kill_session` tool — operator escape hatch | same |
| 3.5 | Cap: 8 sessions/host, configurable via skill `config.max_sessions` | `manager.go` |
| 3.6 | Tests: TTL eviction, cap enforcement, kill_session cleans up adapter state | `sessions_test.go` |

### Acceptance

- LLM can attach → set breakpoint → close chat → re-attach via
  `session_id` in next chat without restart.
- Idle session evicted after 30 min (configurable).
- Adapter subprocess actually killed on eviction (no zombie gdb).

## Phase 4-mcp — IDA-Python RPC adapter

### Tasks

| # | Task | File |
|---|---|---|
| 4.1 | IDA plugin (Python) — listens on TCP localhost:<configurable>, exposes JSON-RPC over the IDA API | `cmd/polar-agent/skills/mcp/adapters/ida/plugin.py` (new) |
| 4.2 | Go adapter — TCP client, marshals tool calls to IDA plugin | `cmd/polar-agent/skills/mcp/adapters/ida/ida.go` (new) |
| 4.3 | Tools: `ida.get_function`, `ida.list_functions`, `ida.disassemble`, `ida.read_memory`, `ida.set_comment`, `ida.rename` | same |
| 4.4 | Install hint: `host_detail.html` shows "Drop plugin.py into ~/.idapro/plugins/" when IDA adapter listed but not reachable | `ui/src/hosts.ts` |
| 4.5 | Test: mock IDA server (Python script that fakes the API) for CI | `ida_test.go` |

### Acceptance

- Operator with IDA Pro can put the plugin file in place, restart IDA,
  enable the IDA adapter in `mcp-server` config, and LLM can call
  `ida.get_function` over chat.

## Phase 5-mcp — Frida + radare2 adapters

### Tasks

| # | Task | File |
|---|---|---|
| 5.1 | Frida adapter: spawn `frida` REPL, script injection over JS pipe | `cmd/polar-agent/skills/mcp/adapters/frida/frida.go` (new) |
| 5.2 | Frida tools: `frida.attach`, `frida.script`, `frida.unload` | same |
| 5.3 | radare2 adapter: `r2pipe` Go binding | `cmd/polar-agent/skills/mcp/adapters/r2/r2.go` (new) |
| 5.4 | r2 tools: `r2.open`, `r2.cmd`, `r2.disasm`, `r2.refs` | same |

### Acceptance

- Both adapters reachable via `mcp.list_tools` when enabled.
- Frida script string-uppercase smoke test: hook a JS function in a
  toy target, return the hooked args.

## Phase 6-mcp — Semantic cache + action graph

### Tasks

| # | Task | File |
|---|---|---|
| 6.1 | `mcp.cache_get` / `mcp.cache_put` MCP tools — Redis backend keyed by `(session_id, namespace, key)` | `cmd/polar-agent/skills/mcp/cache.go` (new) |
| 6.2 | TTL default 24h per cache entry; per-call override | same |
| 6.3 | Action graph: each tool call's `{session_id, tool, args_hash, result_hash, ts}` appended to a Redis list per session; `mcp.action_history(session_id, limit)` tool surfaces it | same |
| 6.4 | LLM-side prompt enhancement: when a session has > N entries, the next prompt's system message includes a 1-line action summary | `internal/app/dock/ai_agent.go` |

### Acceptance

- Re-running `ida.disassemble` over the same range in the same
  session returns the cached digest, not the full bytes.
- LLM can ask `mcp.action_history` and get back a structured list of
  what it has already done.

## Phase 7-mcp — Hosts UI surfaces

### Tasks

| # | Task | File |
|---|---|---|
| 7.1 | `/hosts/:id` detail page: add "MCP Tools" tab — lists tools/list output with schemas | `ui/src/hosts.ts` + `ui/public/hosts.html` |
| 7.2 | Active session list with kill button per row | same |
| 7.3 | "Tool playground": pick a tool, fill schema-derived form, see result JSON. No LLM in the loop — for operator smoke testing | same |
| 7.4 | Config editor for adapters[] (checkboxes per adapter the agent reports as installable) | same |

### Acceptance

- Operator can verify a host's MCP server end-to-end in the UI
  without curl.
- Sessions visible per host with last-activity timestamps.

## Cross-phase concerns

### Schema

P0-mcp through P5-mcp ship **zero** schema changes — all state lives
on the agent side (in-process sessions) or in Redis (cache + action
graph in P6-mcp). P7-mcp may add a small `host_skill_mcp_sessions`
table if we decide to surface sessions across dock restarts; left as
TBD in the design doc §6.

### Backwards compatibility

The new WS message kinds (`mcp.tool.call`, `mcp.tool.result`,
`mcp.list_tools`) are additive. Old agents ignore unknown kinds
(existing behavior); old docks ignore unknown kinds from agents. A
fresh agent talking to an old dock sees its `mcp.list_tools` go
unanswered and falls back to local-only operation. No flag-day
upgrade required.

### Security

P0-mcp ships with **no sandbox**. The skill grants the LLM the
ability to launch any program under the polar-agent uid on the host.
Mitigations in scope:

- `mcp-server` skill is opt-in per host (must be explicitly enabled
  in `host_skills` row)
- Adapter list is allow-list, not deny-list — operator picks which
  adapters load
- Per-tool dock-side rate limit (P1-mcp): 60 calls / chat thread by
  default, configurable per workspace
- Audit (open question — design §10 Q3): whether tool calls land in
  `chat_messages` is unresolved; if yes, that's the audit trail

Out of scope: process sandboxing, filesystem chroot, network
isolation. Operators who need those should run polar-agent in a VM.

### Risk register

| Risk | Phase | Mitigation |
|---|---|---|
| GDB/MI parser brittleness (escaping, async records interleaved) | P0-mcp | Use `github.com/cyrus-and/gdb` or vendor `pygdbmi`'s parser test corpus; full MI-2 grammar is small enough to nail down |
| MCP SDK churn (mark3labs/mcp-go is < 1.0) | P0-mcp | Pin a tag, treat as vendored; protocol surface is small enough to fork if upstream stalls |
| LLDB DAP behavior on iOS-tethered targets | P2-mcp | iOS DAP path has known gaps; if it bites, fall back to `lldb` SB API directly via `lldb -P` Python binding |
| IDA plugin requires operator install on a non-headless GUI app | P4-mcp | Surface "plugin not detected" state in UI with copy-paste install snippet |
| LLM tool-loop infinite recursion (model keeps calling tools) | P1-mcp | Per-thread tool-call budget (default 20); 21st call returns `error: tool_budget_exceeded` to the LLM |
| Session leak from crashed adapter subprocess | P3-mcp | Manager polls subprocess exit; on exit, evicts session + logs `skill.event` to dock |
| MCP-server skill becomes the de-facto "run anything" backdoor | all | Audit log option (P1-mcp) + per-workspace allow-list of adapters |

## Open questions tracked here (not in design doc)

1. **mock GDB for CI** — P0-mcp's test needs a deterministic
   subprocess. Options: build a tiny test C program at test setup
   time (requires `cc` in CI), or vendor a recorded MI session.
   Vendored is more portable; decide before P0-mcp commits.
2. **WS bridge buffering** — a long-running tool (e.g. `gdb.run`
   waiting on a breakpoint that never trips) ties up the WS until
   timeout. Should the bridge support cancelable in-flight calls?
   Likely yes in P1-mcp via an explicit `mcp.tool.cancel` kind, but
   P0-mcp ships without it — first wait-on-breakpoint hang shows
   whether it's needed.
3. **Per-call vs per-session billing** (also in design §10) — defer
   to product call; doesn't block any phase.

## When to merge what

| If user says... | Land |
|---|---|
| "design doc looks right, go" | This dev doc + the design doc as a single PR (no code) |
| "do P0-mcp" | The P0-mcp PR per §0 above |
| "skip gdb, do lldb first" | Re-order: P0-mcp adapter slot is LLDB DAP, gdb moves to P2-mcp |
| "let's pause and validate the LLM-agent integration first" | Land P0-mcp + P1-mcp together as one PR (heavier review, but the agent loop is the value prop) |
