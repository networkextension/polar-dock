# MCP-Server Skill — Design Proposal

**Status**: draft — opened 2026-05-14, awaiting review.

**Source memo**: [/markdown.html?id=260](mcp-server)  ("mcp server" — the
RE-tooling architecture brief).

**Slot in roadmap**: Host module P3 (after `coder` P1 + `shell` P2).
The `KindMCPServer = "mcp-server"` constant is already declared in
`cmd/polar-agent/skills/skill.go:31`.

---

## 1. Problem in one paragraph

We want an LLM agent (running inside dock's `ai_agent.go`) to drive
real reverse-engineering / debugging tools — GDB, LLDB, IDA, Frida,
radare2 — that physically live on a specific operator host (the box
with the IDA license, the box tethered to an iPhone, the kernel-debug
Linux box). Today the agent can only either (a) talk to an LLM, or
(b) run a coder subprocess (claude/codex/kimi) on a polar-agent host.
There is no path for "LLM → structured tool call → debugger session
on host X → structured result". The source memo argues this gap is
the central problem in AI-driven RE work, and prescribes a specific
shape: structured tool schemas, persistent sessions, MCP-style
dispatch, adapter-per-tool. This doc proposes how that shape lands
inside Polar without inventing new transports or new auth.

## 2. Why MCP-the-protocol (not a custom JSON-RPC)

The memo treats "MCP化" as the end state. Two paths:

| Option | What it means | Trade-off |
|---|---|---|
| **(A) Bespoke JSON-RPC over our WS channel** | Reuse the agent WS tunnel; define `mcp.tool.call` / `mcp.tool.result` message kinds | Smaller diff, but we re-invent schemas + tooling already standardized by MCP |
| **(B) Real MCP server inside polar-agent** | Skill spawns an MCP-compliant server (stdio or HTTP-SSE); dock acts as an MCP **client** that just happens to use our WS as the transport carrier | Standard schema, off-the-shelf SDKs (`github.com/modelcontextprotocol/go-sdk`), Claude Code / Claude Desktop / OpenAI can connect to the same server, and our LLM call site (`ai_agent.go`) gets `tools` for free |

Picking **(B)**. The cost — one extra in-process MCP server per host —
is small. The win — interoperability with every MCP-aware client that
ships in the next two years — is large.

## 3. Architecture

```
                     dock (host: 192.168.11.57)
   ┌───────────────────────────────────────────────────────┐
   │  ai_agent.go (LLM dispatch)                           │
   │      │                                                │
   │      ▼                                                │
   │  mcp_client.go  ──── tool_call ────┐  (NEW, P3)       │
   │                                    │                  │
   │  agent_hub.go ─── WS tunnel  ───────────────┐         │
   └─────────────────────────────────────────────│─────────┘
                                                 │
                                                 ▼
       polar-agent (host: e.g. ida-licensed Mac, kernel-debug Linux)
   ┌───────────────────────────────────────────────────────┐
   │  loop.go                                              │
   │      │ skill.advertise ──► "mcp-server" + adapters    │
   │      ▼                                                │
   │  skills/mcp/server.go                                 │
   │      │                                                │
   │      ├── adapter: gdb-mi   ◄── spawns gdb --interpreter=mi2
   │      ├── adapter: lldb-dap ◄── spawns lldb-dap
   │      ├── adapter: ida-rpc  ◄── connects to IDA plugin RPC port
   │      ├── adapter: frida    ◄── frida-tools subprocess
   │      └── adapter: radare2  ◄── r2pipe subprocess
   └───────────────────────────────────────────────────────┘
```

Key decisions:

1. **Skill scope = one MCP server per host, all adapters in-process**.
   Not one skill per adapter. Reason: the operator typically wants
   "AI-driven debugging on this box" as a single switch; configuring
   five separate skills for one machine is friction. The skill's
   `config.adapters` array picks which adapters load.

2. **Transport is reused, not invented**. polar-agent already has a
   live WS to dock with bearer auth + per-host routing. We add two
   new message kinds:
   - `mcp.tool.call` (dock → agent) — `{call_id, tool, args}`
   - `mcp.tool.result` (agent → dock) — `{call_id, ok, result|error}`
   Plus `mcp.list_tools` so dock can fetch the catalogue on demand.
   The skill internally speaks MCP-the-protocol; the WS tunnel just
   carries the JSON-RPC frames.

3. **Session state lives on the agent side, keyed by `session_id`**.
   First memo lesson — without session persistence the LLM forgets
   that the breakpoint is set. Each adapter exposes
   `<name>.attach(session_id, target)` that returns an opaque session
   handle; subsequent tools take the session_id as the first arg.
   Session TTL = 30 min idle → adapter cleanup. Dock surfaces
   active sessions on `/hosts/:id`.

4. **Schema is generated, not hand-written**. Each adapter file
   declares a Go struct per tool with `json:` tags + a description
   comment; `go generate` produces the MCP `tools/list` payload at
   build time. This is how we keep the catalogue honest as adapters
   evolve.

## 4. P0 scope (the smoke test PR)

P0 must prove the wire end-to-end with the smallest possible adapter,
not boil the ocean. Picking **gdb-mi** because:

- GDB ships MI by design (no parsing CLI text → already structured)
- No license / GUI dependency (unlike IDA)
- Available on Linux + macOS + WSL — every dev box has it

P0 tools (exactly two):

| Tool | Args | Returns |
|---|---|---|
| `gdb.run` | `{program, args, breakpoints?}` | `{session_id, stop_reason, frame}` |
| `gdb.read_register` | `{session_id, register}` | `{value_hex}` |

Smoke test:

```
# on the host with polar-agent:
echo 'int main(){ return 42; }' | cc -g -x c - -o /tmp/answer

# dock-side curl (admin auth):
curl -X POST .../api/hosts/h_xxx/skills/mcp/tools/gdb.run \
  -d '{"program":"/tmp/answer","breakpoints":["main"]}'
# → {"session_id":"s_abc","stop_reason":"breakpoint-hit","frame":{...}}

curl -X POST .../api/hosts/h_xxx/skills/mcp/tools/gdb.read_register \
  -d '{"session_id":"s_abc","register":"rip"}'
# → {"value_hex":"0x401110"}
```

When this round-trip works, every later adapter is "just" plug-and-play.

## 5. Tool dispatch on the dock side

Two consumers of the MCP catalogue:

**(a) Direct API for testing / scripting** — REST endpoints under
`/api/hosts/:id/skills/mcp/tools/:tool` (POST body = args). The
handler picks the host's live agent WS, sends `mcp.tool.call`, waits
for `mcp.tool.result` (timeout 30s default, configurable per tool).
This is the smoke-test path in §4.

**(b) LLM agent tools** — `ai_agent.go` already has tool-loop
plumbing for the coder skill. Add an `mcpTools` provider that, when
the active workspace has at least one host with `mcp-server` enabled
+ running, queries `mcp.list_tools` once on each chat thread start
and injects them into the LLM call's `tools` array. Tool-call
fan-out picks the host based on the LLM's tool name (`<host_slug>.<tool>`
or `<tool>` with auto-routing when only one host advertises it).

## 6. Session manager (P3 of the skill, ~P5 overall)

P0 sessions are per-tool-call (no reuse). That's fine for "run program
to breakpoint, read one register" but breaks for "set breakpoint →
run → read mem → step → read regs" workflows.

The session manager lives in `cmd/polar-agent/skills/mcp/sessions.go`
and:

- Indexes by `session_id` (UUID, agent-generated on first `<tool>.attach`)
- Holds the live debugger subprocess + adapter state struct
- Idle TTL eviction (default 30 min, per-call extendable)
- Cap = 8 sessions per host (memory pressure on the operator box)
- Exposes a `mcp.list_sessions` tool so the LLM can re-attach after a
  context window flush

Dock-side persistence: optional. We could write session metadata to
`host_skill_runs` so dashboards survive agent restart, but the active
debugger state is on-host volatile by definition; restart = sessions
gone. P3 ships without dock persistence; revisit if real users care.

## 7. Memory + context strategy (matches §13 of the memo)

The memo is right that token budget kills RE agents. Two mitigations
built into the skill, not the LLM prompt:

- **Tool returns are compact by default**. `gdb.disassemble` returns
  `{addr, bytes_hex, mnemonic_count}` not the full instruction stream.
  An LLM that wants the full text calls `gdb.disassemble_text` with
  an explicit `max_lines`.
- **Semantic cache**. Per-session memo of "we already disassembled
  0x401000..0x401100 — here's the digest". `mcp.cache_get` /
  `mcp.cache_put` are MCP tools the LLM can call when it wants to
  avoid re-fetching. Cache backed by the same Redis instance dock
  already uses for thread-state coordination.

## 8. Non-goals (explicit)

- **Not a sandbox.** If the LLM asks the GDB adapter to load malware
  into the debugger, the adapter does it. Sandboxing belongs to the
  host's OS (run polar-agent in a VM if you don't trust the LLM).
- **Not a replacement for `coder`.** Coder writes code; mcp-server
  inspects/manipulates running binaries. Some overlap (both might
  edit files), but the abstractions stay separate.
- **Not Lua.** Per memo §5 + §11, Lua is fine as a per-adapter
  DSL (radare2's r2 shell is Lua-adjacent) but not the agent
  orchestrator. Polar's orchestrator stays Go / TS.
- **No web UI for tools in P0.** UI surfaces will land in P4+ once
  we know which tools operators actually invoke; over-designing UI
  ahead of usage data is the trap §13 warns about.

## 9. Phase plan (post-design)

| Phase | Scope | Estimate |
|---|---|---|
| **P0-mcp** | Skill scaffold + gdb-mi adapter + 2 tools + dock direct-call REST + smoke | 3–4 days |
| **P1-mcp** | LLM-agent tool injection (§5b) + WS message kinds + `mcp.list_tools` | 2 days |
| **P2-mcp** | LLDB DAP adapter (3–4 tools — attach, breakpoint, step, read_mem) | 3 days |
| **P3-mcp** | Session manager + idle eviction + `mcp.list_sessions` | 2 days |
| **P4-mcp** | IDA-Python RPC adapter (requires IDA running on host) | 4 days |
| **P5-mcp** | Frida + radare2 adapters | 3 days |
| **P6-mcp** | Semantic cache + action-graph (§7) | 3 days |
| **P7-mcp** | Hosts UI: per-host MCP catalogue, session list, tool playground | 4 days |

Each phase = one PR, mergeable independently.

## 10. Open questions (need user input before P0 starts)

1. **MCP SDK choice** — `github.com/mark3labs/mcp-go` (community,
   active) vs hand-rolling JSON-RPC. Recommend mark3labs; it has the
   schema generators and saves us a week. Locked-in dependency, but
   the protocol surface is small enough to vendor if upstream stalls.
2. **First non-GDB adapter** — after P0 lands, the natural next pick
   is LLDB DAP (Mac-native, matches the user's dev box). If the
   primary use-case is iOS reversing on the nanokdp box, IDA-Python
   may jump the queue. Confirm before P2 starts.
3. **Per-call vs per-session billing model** — the LLM-agent path
   counts tokens; we don't currently count *tool* invocations.
   Whether `mcp.tool.call` events should land in `chat_messages` for
   audit / billing is a product call, not a tech call.

## 11. What lands first if approved

One PR titled `feat(agent): mcp-server skill scaffold + gdb-mi P0`.
Diff shape, by file:

- `cmd/polar-agent/skills/mcp/server.go` — new, ~250 LOC. MCP server
  init, adapter registry, message dispatch.
- `cmd/polar-agent/skills/mcp/transport_ws.go` — new, ~120 LOC.
  Bridge MCP JSON-RPC frames over the existing agent WS.
- `cmd/polar-agent/skills/mcp/adapters/gdb/gdb.go` — new, ~200 LOC.
  GDB/MI subprocess wrapper + 2 tool implementations.
- `cmd/polar-agent/skills/mcp/adapters/gdb/gdb_test.go` — new. Smoke
  test against `/bin/echo` (no debug info needed for `gdb.run` exit
  case).
- `cmd/polar-agent/skills/mcp/skill.go` — new, ~80 LOC. Implements
  the `Skill` interface; `init()` calls `skills.Register(&mcpSkill{})`.
- `internal/app/dock/agent_handlers.go` — extend kind switch with
  `mcp.tool.result` (~30 LOC).
- `internal/app/dock/host_skill_handlers.go` — new, ~150 LOC.
  POST `/api/hosts/:id/skills/mcp/tools/:tool` direct-call.
- `internal/app/dock/app.go` — wire the new route (~3 LOC).
- `doc/mcp-skill-dev.md` — new. Per-phase implementation notes,
  mirrors `doc/host-module-dev.md`.

Smoke: bring up `polar-agent` with `mcp-server` skill enabled +
`adapters: ["gdb"]` config, run the two `curl` commands from §4, get
structured JSON back. No UI changes in this PR.
