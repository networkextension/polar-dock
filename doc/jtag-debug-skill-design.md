# JTAG / LLDB Skill — Design Proposal

**Status**: draft 2026-05-16. Synthesized from `doc/jtag/00..11.md`
(LLM-conversation logs on m1n1 + JTAG + MCP) + the existing MCP
skill (P0a + P0b adapter framework already in tree).

**Why now**: P0b shipped the adapter framework. The next adapter to
land is the one with the highest "AI × hardware" leverage — exactly
what the jtag/ docs argue: give an LLM agent direct read/write
access to ARM system registers + physical memory + boot-stage
inspection, and Apple Silicon kernel-port work that used to take a
week of grep'ing the ARMv8 manual collapses to hours.

**Companion docs**:
- `doc/mcp-skill-design.md` — protocol + dock-side dispatch overview
- `doc/mcp-skill-dev.md` — phase plan; this proposal slots between P0c (sessions) and P2-mcp (lldb-dap)
- `doc/jtag/all_arch.md` — the operator's whole-system vision; this skill is the "JTAG" branch of the §2.3 Skill Layer

## 1. Problem in one paragraph

An Apple Silicon kernel-port developer (m1n1, FreeBSD-on-M1,
asahi-Linux) spends most debug time in a loop: tweak source →
build → flash → set breakpoint → read register → look up what
the register means in a 5000-page ARMv8 manual → guess → repeat.
The JTAG probe + LLDB chain gives a human direct register
+ memory access, but the human is the bottleneck. Wire that chain
behind an MCP adapter and the LLM becomes the loop's executor —
it reads `ESR_EL1`, knows what bit 25 means without grep, walks
the page table itself, and reports "your TTBR0_EL1 is pointing at
unmapped phys; fix here". The jtag/ docs converge on this same
prescription — this design is how it lands in Polar.

## 2. Where it fits

```
polar-agent (one process per host)
├── skills/mcp_server.go  (MCP skill, P0a)
│   ├── MCPAdapter registry (P0b)
│   │   ├── echo      ← P0a — sanity check, always loads
│   │   ├── stub      ← P0b — deterministic test tools
│   │   ├── lldb      ← THIS DESIGN, P-jtag-0a
│   │   ├── jtag    ← THIS DESIGN, P-jtag-0b (when probe present)
│   │   ├── ida       ← future, P-ida-0a
│   │   └── frida     ← future, P-frida-0a
│   └── JSON-RPC dispatch — initialize / tools/list / tools/call
└── (existing skills: shell / vnc / coder / proxy / wireguard / kdp)
```

The `lldb` and `jtag` adapters are siblings of `echo` and `stub`.
Operator opts in per-host via skill config:

```jsonc
// skill_config_json on a Mac with LLDB + an JTAG probe attached:
{ "adapters": ["lldb", "jtag"] }

// On a Linux build box with no probe:
{ "adapters": ["lldb"] }
```

Adapter unavailability is a soft-fail (per P0b's
`errAdapterUnavailable` pattern) — no JTAG probe → `jtag`
adapter logs + skips at Start; the rest of the MCP server runs
normally.

## 3. Architecture

### 3.1 Transport

No new transport. JSON-RPC over the existing `skill.stdin` /
`skill.event(stdout)` byte stream — same shape as P0a/P0b. The
adapter owns its subprocess (lldb or vendor JTAG controller); the MCP server
just dispatches `tools/call` and forwards the result.

### 3.2 LLDB adapter

LLDB ships with Xcode on macOS, ships in dev packages on Linux —
broadly available. No license, no probe needed for software-only
debugging. **Start here**; it's the higher-coverage adapter.

**Transport choice**: spawn `lldb-rpc-server` (LLDB's built-in
GDB-Remote-Protocol port), bind it to a local pipe, drive via the
Python `lldb` bindings. The Python bindings give us a clean Go
side via `os/exec` + JSON-marshaled commands.

Alternative considered + rejected: scrape `lldb` REPL via stdin/
stdout. Too brittle — output format depends on lldb version, color
codes, terminal width. The Python binding has stable
serialization.

**Tools (P-jtag-0a)**:

| Tool | Args | Returns |
|---|---|---|
| `lldb.attach` | `{pid?, executable?, core_dump?}` | `{session_id, target_arch, executable}` |
| `lldb.read_register` | `{session_id, register}` | `{value_hex, value_decimal}` |
| `lldb.read_memory` | `{session_id, address, size, format?}` | `{bytes_hex, decoded?}` |
| `lldb.backtrace` | `{session_id, thread_id?, max_frames?}` | `[{frame_index, pc, function, file, line}]` |
| `lldb.set_breakpoint` | `{session_id, location}` | `{breakpoint_id, resolved_address?}` |
| `lldb.continue` | `{session_id}` | `{stop_reason, stop_pc, signal?}` |
| `lldb.disassemble` | `{session_id, address, count}` | `[{address, mnemonic, operands}]` |
| `lldb.list_sessions` | `{}` | `[{session_id, target, attached_at}]` |
| `lldb.detach` | `{session_id}` | `{ok}` |

Session is the unit of state — created by `lldb.attach`, kept
alive on the agent side until idle timeout (30 min default) or
explicit `lldb.detach`. The MCP P0c work (persistent sessions in
the framework itself) is a prerequisite for this — see §5
ordering.

### 3.3 JTAG adapter

JTAG is Apple-internal. Two real-world constraints from the
source docs:

1. **`vendor JTAG library (libjtag.dylib)` is undocumented** (`doc/jtag/05.md`,
   `06.md`). Headers aren't public. We bind via `cgo` or via
   subprocess to `vendor JTAG controller` — *subprocess is the safer first cut*
   (no symbol-name guessing, no breaking on JTAG updates).
2. **Production boards need the Red-eye / Ticket-based Auth**
   (`doc/jtag/09.md`) — JTAG will refuse to attach to a
   t8101/M1 production board without a server-issued ticket. On
   any board where the operator can't get a ticket, the adapter
   degrades to "absent" and we fall through to LLDB-over-serial
   instead.

So the adapter shape:

```go
type jtagAdapter struct {
    vendor JTAG controller   string       // path; "" if not found
    probeOnline bool         // false if no probe / auth fails
    sessions    map[string]*jtagSession
}

func (a *jtagAdapter) factory() (MCPAdapter, error) {
    p, err := exec.LookPath("vendor JTAG controller")
    if err != nil { return nil, errAdapterUnavailable }
    // ... attempt `vendor JTAG controller status` to detect probe + auth
}
```

**Tools (P-jtag-0b)**:

| Tool | Args | Returns |
|---|---|---|
| `jtag.attach` | `{probe_serial?}` | `{session_id, target_chip}` |
| `jtag.halt` | `{session_id, core?}` | `{halted, pc, exception_level}` |
| `jtag.resume` | `{session_id}` | `{ok}` |
| `jtag.read_reg` | `{session_id, name}` (`SCTLR_EL1`, `TTBR0_EL1`, …) | `{value_hex}` |
| `jtag.write_reg` | `{session_id, name, value}` | `{ok}` ⚠️ destructive |
| `jtag.read_mem` | `{session_id, address, size}` | `{bytes_hex}` |
| `jtag.write_mem` | `{session_id, address, bytes_hex}` | `{ok}` ⚠️ destructive |
| `jtag.step` | `{session_id, count?}` | `{pc, exception_level}` |
| `jtag.parse_pagetable` | `{session_id, ttbr_value}` | `[{va, pa, attrs, perms}]` — high-value helper, AI doesn't have to walk the table by hand |
| `jtag.dump_devicetree` | `{session_id, address}` | `{decoded_json}` |

`jtag.parse_pagetable` and `jtag.dump_devicetree` are the
"give the LLM superpowers" tools — they encode ARMv8 page-table
walks + Apple FDT layouts that no human would want to grep for.

**Safety**: write tools (`write_reg` / `write_mem`) are
HIGH RISK on real hardware. Per `doc/jtag/02.md` "Watchdog 干扰"
and `06.md` "可能烧毁屏幕" warnings, we should:

- Gate `write_*` behind a `safe_mode: false` flag in adapter
  config; default safe_mode=true → write tools return error
- Maintain a per-target denylist of registers known to brick the
  SoC (PMGR clock gates, display controller mode regs, SEP
  registers)
- Log every write to a tamper-evident audit trail

### 3.4 Recovery / DFU adapter — the no-probe fallback

**Critical realization from operator (2026-05-16)**: the board
exposes a SoC-state channel via the USB Recovery / DFU protocol
that does NOT require an JTAG probe AND does not need the
Red-eye Auth ticket from `doc/jtag/09.md`. Two tools available:

- **`irecovery -q`** (from libimobiledevice / libirecovery) —
  query a device in Recovery or DFU mode, returns chip/board
  identity + firmware metadata:
  ```
  CPID: 0x8101    ← chip family (t8101 = M1)
  BDID: 0x14      ← board variant
  ECID: 0x0000XXXXXXXX  ← unique device id
  CPRV: 0x10      ← chip revision
  IBFL: 0x14      ← iBoot flags (security state)
  SRTG: [iBoot-XXXX.XX.X.X.X]  ← stage version
  SRNM: [serial]
  ```

- **Anya** (https://github.com/NyanSatan/Anya) — DFU-mode
  research tool from the iOS bring-up community. Goes beyond
  irecovery's metadata query to expose memory peeks + boot-stage
  hooks via DFU-exploit sequences. Cap of what's reachable
  depends on the SoC's BootROM patch level (which chips are
  `checkm8`-vulnerable etc.).

**Why this matters for the skill design**: a `recovery` adapter
fits BEFORE LLDB and JTAG in the phase plan — no Auth ticket,
no Xcode install, no probe hardware. The bar to attach is just
"USB cable + libimobiledevice installed". On a production board
that JTAG can't touch, recovery is the only channel — exactly
the "Builder's备选方案" the source docs (`doc/jtag/09.md` §方案
调整, `10.md`) prescribe.

**Tools (P-recovery-0a)**:

| Tool | Args | Returns |
|---|---|---|
| `recovery.detect` | `{}` | `[{ecid, chip_id, board_id, mode}]` — list all USB devices in Recovery/DFU |
| `recovery.query` | `{ecid?}` | `{cpid, bdid, ecid, cprv, ibfl, srtg, srnm, pwnd}` — irecovery -q wrapper |
| `recovery.send_command` | `{ecid?, cmd}` | `{output}` — send a command to iBoot Recovery prompt (e.g. `printenv`, `nvram boot-args=...`) |
| `recovery.dfu_send_image` | `{ecid?, image_path}` | `{ok, bytes_sent}` — push a .img4 payload via DFU |
| `recovery.anya_status` | `{ecid?}` | `{checkm8_capable, boot_stage, sep_status}` — Anya-derived deep status (requires Anya on PATH) |

**Adapter shape**: subprocess to `irecovery` for the basic
tools, optional Anya subprocess for the deep tools. Both
absent → adapter unavailable (soft fail per §2 convention).

**Multi-channel arrangement**: an operator with all 3 tools
installed can declare:

```jsonc
{ "adapters": ["recovery", "jtag", "lldb"] }
```

The LLM picks the right channel by tool name. Recovery for the
"is the device present + what is it" pre-flight; JTAG for
deep JTAG-level halt+step (when probe + Auth available); LLDB
for user-space process debugging on the host running the agent.

### 3.5 Knowledge base modules — device / firmware / function libraries

Three first-class libraries the LLM queries DURING the debug
loop (not just at start). Live dock-side (Postgres + optional
blob store for firmware images), exposed both as REST (UI / human
browsing) and as a MCP `library` adapter (in-loop LLM access).

The §4 flow example is what motivates these: when the LLM sees
`PC=0x80001234`, it shouldn't have to load the whole firmware
binary into context to identify the function. It calls
`library.functions.lookup_by_address(firmware_id, 0x80001234)`
and gets back `"iboot_main, void iboot_main(void), Stage 2
iBoot main loop"` — a 100-byte answer that unlocks the next
debug step.

These libraries also produce a **network effect across
operators**: a function symbol reverse-engineered by one operator
(or auto-extracted from an IDA project / Asahi disasm) becomes
searchable for the whole fleet.

#### 3.5.1 Device inventory (`rev_devices`)

Cataloged hardware — chip + board + currently-running OS.

```sql
CREATE TABLE rev_devices (
  id BIGSERIAL PRIMARY KEY,
  ecid BIGINT UNIQUE,             -- 64-bit unique chip id
  cpid INTEGER NOT NULL,          -- chip family (0x8101 = t8101 = M1)
  bdid INTEGER,                   -- board variant
  cprv INTEGER,                   -- chip revision
  chip_name TEXT,                 -- "M1" / "M2 Pro"
  board_name TEXT,                -- "J293" / "M01dev" / "01-dev"
  os_running TEXT,                -- "iOS 17.2" / "macOS 14.3" / "freebsd-port-r1"
  os_kernel_version TEXT,
  notes TEXT,                     -- "iPhone 12 in drawer 3"
  added_at TIMESTAMPTZ DEFAULT now(),
  last_seen_at TIMESTAMPTZ,
  metadata_json JSONB             -- snapshot of last recovery.query
);
CREATE INDEX ix_rev_devices_chip ON rev_devices(cpid, bdid);
```

Populated by:
- Operator manually (one-off `library.devices.upsert`)
- Auto-ingest when `recovery.detect` / `recovery.query` sees a
  previously-unknown ECID — adapter calls `library.devices.upsert`
  with the irecovery metadata; first-seen devices appear in the
  inventory without operator action

Tools (MCP `library.devices.*`):
- `list` — filter by chip / board / os
- `get` — by ecid or id
- `upsert` — operator / agent creates or updates
- `recent_seen` — touched in last N days (the "active fleet")

#### 3.5.2 Firmware library (`rev_firmwares`)

Categorized by firmware KIND (lcd / touch / wifi / sep / iboot /
ane / pmu / smc / …) + version + chip/board compatibility. Image
blob stored on object storage (or local FS); DB row holds
metadata + sha256 + path.

```sql
CREATE TABLE rev_firmwares (
  id BIGSERIAL PRIMARY KEY,
  kind TEXT NOT NULL,             -- "lcd" / "touch" / "wifi" / "sep" / "iboot" / "ane" / "pmu" / ...
  vendor TEXT,                    -- "broadcom" / "synaptics" / "apple"
  version TEXT NOT NULL,          -- "21.0.4-rev3" / "iBoot-7459.40.10"
  chip_id_compat INTEGER[],       -- which CPIDs this fits (NULL = any)
  board_id_compat INTEGER[],
  blob_uri TEXT NOT NULL,         -- file:///var/polar/fw/<sha>.img4 | s3://...
  blob_sha256 TEXT NOT NULL,
  size_bytes BIGINT,
  format TEXT,                    -- "img4" / "raw" / "elf" / "ipsw-extracted"
  extracted_from TEXT,            -- which IPSW / OTA / artifact
  added_at TIMESTAMPTZ DEFAULT now(),
  added_by TEXT,
  metadata_json JSONB
);
CREATE INDEX ix_rev_firmwares_kind_version ON rev_firmwares(kind, version);
CREATE UNIQUE INDEX ux_rev_firmwares_sha ON rev_firmwares(blob_sha256);
```

Tools (`library.firmwares.*`):
- `list` — filter by kind / version / chip
- `get` — by id, returns metadata + signed URL to the blob
- `matching` — `{cpid, bdid}` → all known firmwares that fit
  (the "what blobs apply to this device" pre-flight)
- `diff` — `{a_id, b_id}` → byte-level diff regions between
  two firmwares of the same kind (powers the "what changed
  between iBoot 7459 and 7460" workflow)
- `ingest` — operator uploads a new firmware blob; system
  computes sha + auto-fills format/size; operator confirms
  kind/version/compat

#### 3.5.3 Reverse-engineered function library (`rev_functions`)

Per-firmware function catalog. Each row: address, symbol,
prototype, short purpose. Plus byte/IDA signature for matching
the same function across firmware versions.

```sql
CREATE TABLE rev_functions (
  id BIGSERIAL PRIMARY KEY,
  firmware_id BIGINT NOT NULL REFERENCES rev_firmwares(id) ON DELETE CASCADE,
  address BIGINT NOT NULL,        -- abs address in the loaded image
  symbol TEXT,                    -- "iboot_main" / "verify_signature"
  prototype TEXT,                 -- "int verify_signature(const void *img, size_t len)"
  purpose TEXT,                   -- one-line description for the LLM
  calling_convention TEXT,        -- "aarch64-aapcs64"
  byte_signature BYTEA,           -- first ~32 bytes (cross-version sig match)
  ida_signature TEXT,             -- IDA-style "?? ?? ?? ??" pattern
  confidence TEXT,                -- "verified" / "heuristic" / "speculative"
  source TEXT,                    -- "asahi-Linux disasm" / "operator manual" / "auto-extracted"
  added_at TIMESTAMPTZ DEFAULT now(),
  added_by TEXT,
  metadata_json JSONB             -- xrefs, decompiled snippet, etc.
);
CREATE INDEX ix_rev_functions_fw ON rev_functions(firmware_id);
CREATE INDEX ix_rev_functions_symbol ON rev_functions(symbol);
CREATE INDEX ix_rev_functions_addr ON rev_functions(firmware_id, address);
```

Tools (`library.functions.*`):
- `lookup_by_address` — `{firmware_id, address}` → symbol +
  prototype + purpose. **MOST-USED during debug loops** —
  see §4 flow
- `lookup_by_symbol` — `{firmware_id, name}` → address + the
  rest
- `search` — fuzzy by name / purpose (text search)
- `match_signature` — `{bytes_hex}` → potential matches in
  OTHER firmwares (signature-based reuse across versions —
  "find this function in 7460 given I named it in 7459")
- `add` — operator/agent adds a new function from a debug
  session (the network-effect entry point)

#### 3.5.4 Implementation notes

- All three tables live dock-side; schema migrations in
  `internal/app/dock/store.go` (existing pattern — see
  `host_skills` migration for reference)
- REST under `/api/library/{devices,firmwares,functions}/...`
  (admin-only PUT/POST, workspace-member read)
- MCP `library` adapter wraps the REST — operator config
  `{adapters: ["library", "lldb", "jtag", "recovery"]}`. The
  adapter dials the dock loopback (`http://127.0.0.1:8080/...`)
  + forwards each tool call as a REST request. No new wire
  protocol; reuses the dock auth + DB
- Firmware blob storage: v1 is local FS under
  `~/polar/firmwares/`, indexed by sha256. v2 swap to S3 / R2 /
  whatever object store the operator wires
- For Phase 1: skip the `diff` tool (binary diff infrastructure
  is its own design); ship lookup + ingest + match-signature
  only

### 3.6 Sandbox + state recovery

From `doc/jtag/all.md` §🛠 avoidance:

> Agent 是破坏狂，尤其是挂载了内核调试和逆向工具后。整个 VNC/
> Shell 环境必须实现秒级快照恢复 (Snapshot & Restore)。

For the LLDB adapter the blast radius is "kill the subprocess +
the debugged target" — bounded. For JTAG the blast radius is
"brick the SoC" or "force a Kernel Panic that requires DFU
re-flash". For v1 we MUST NOT auto-recover SoC bricks; instead:

- Hard cap on write tools (per above)
- Surface every JTAG adapter run with a state event flagging it
  as "destructive-capable" so the operator UI can warn

Auto-DFU + reflash is a P-jtag-0c follow-up; depends on a
separate per-host launcher script the operator wires.

## 4. Concrete tool flow — example

Operator picks "AI 排查 m1n1 卡死" task. Coder agent receives
the task + a hostid pointing at a Mac with JTAG probe + an M1
dev board attached:

```
1. agent → MCP: tools/call jtag.attach {}
              ← {session_id: "s_7k3", target_chip: "t8101"}

2. agent → MCP: tools/call jtag.halt {session_id: "s_7k3"}
              ← {halted: true, pc: "0x8000124c", exception_level: 1}

3. agent → MCP: tools/call jtag.read_reg {session_id, name: "ESR_EL1"}
              ← {value_hex: "0x96000050"}

   LLM internal: ESR_EL1 bits 31-26 = 0x25 = "Data Abort from
   lower EL", bit 6 = WnR=1 (write), bits 5..0 = 0x10 = "level 0
   translation fault". So: write to an unmapped page.

4. agent → MCP: tools/call jtag.read_reg {session_id, name: "FAR_EL1"}
              ← {value_hex: "0xfffffe0010083000"}

5. agent → MCP: tools/call jtag.read_reg {session_id, name: "TTBR0_EL1"}
              ← {value_hex: "0x80808000"}

6. agent → MCP: tools/call jtag.parse_pagetable
              {session_id, ttbr_value: "0x80808000"}
              ← [{va: 0xff...000, pa: 0x800..., perms: "rw"},
                 {va: 0xff...100, perms: "RESERVED — NOT MAPPED"}]

7. agent surfaces to operator:
   "m1n1 crashed at 0x8000124c trying to write 0xfffffe0010083000.
   The page is not mapped in TTBR0_EL1. Looking at m1n1/src/mmu.c
   line 142, the mapping for the framebuffer region appears to
   start at 0xfffffe0010000000 with size 0x80000 — your access
   at +0x83000 falls past the mapped range. Suggested fix:
   change FB_REGION_SIZE in mmu.c from 0x80000 to 0x100000."
```

This is exactly the loop the source docs envision. Without the
parse_pagetable helper the LLM would burn 50× tokens dumping +
parsing raw page-table entries.

## 5. Phase plan + ordering

Depends on MCP framework state we already shipped (P0a + P0b)
plus one prerequisite that hasn't landed.

| Phase | Scope | Depends on | Est LOC | Risk |
|---|---|---|---|---|
| **MCP P0c** (prereq) | Per-session state in the MCP server itself — `session_id` threading, per-session adapter state, 30-min idle evict, `mcp.list_sessions` tool | P0b ✓ | ~250 Go | low |
| **P-library-0a** | dock schema migrations + REST endpoints for `rev_devices` / `rev_firmwares` / `rev_functions`. CRUD + lookup tools | none (pure dock) | ~600 Go + ~150 SQL | low |
| **P-library-0b** | `library` MCP adapter — wraps the REST so the agent can query in-loop. Tools: devices.{list,get,upsert,recent_seen} + firmwares.{list,get,matching,ingest} + functions.{lookup_by_address,lookup_by_symbol,search,match_signature,add} | P0c + P-library-0a | ~400 Go | low |
| **P-recovery-0a** | `recovery` adapter — wraps `irecovery` (mandatory) + Anya (optional). 5 tools (detect / query / send_command / dfu_send_image / anya_status). No probe required, no Auth ticket. On detect / query, auto-upserts new ECIDs into `rev_devices` so the device inventory is self-populating | P0c + P-library-0a | ~400 Go + mock | low |
| **P-jtag-0a** | `lldb` adapter — 5 core tools (attach / read_register / read_memory / backtrace / continue) over LLDB Python bindings via subprocess | P0c | ~600 Go + ~200 Python helper | medium |
| **P-jtag-0b** | `jtag` adapter — wrap `vendor JTAG controller (interactive mode)`, 6 read tools + `parse_pagetable` helper. NO write tools by default. Detect probe + auth at Start | P-jtag-0a | ~700 Go + ~150 Tcl | high (env-specific) |
| **P-jtag-0c** | Write tools (`write_reg` / `write_mem`) behind `safe_mode: false`. Per-target register denylist | P-jtag-0b | ~150 Go | high (destructive) |
| **P-jtag-0d** | Auto-recovery: DFU + reflash hook per host. Sandbox snapshot/rollback (where the host is itself a sacrificial VM) | P-jtag-0c + P-recovery-0a | env-specific | high (operator setup) |
| **P-jtag-0e** | LLDB-over-serial fallback for boards JTAG can't attach to (per `doc/jtag/10.md`) | P-jtag-0a | ~400 Go | medium |

**Ordering rationale**:

1. **P0c first** — sessions are required by every stateful
   adapter (lldb / jtag attach state); pure framework code,
   no hardware
2. **P-library-0a in parallel** — pure dock CRUD, no adapter
   work yet. Schema + REST can land alongside P0c on a sibling
   branch
3. **P-library-0b right after** — once both 0c (session
   framework) and library-0a (dock REST) merge, the library
   MCP adapter wraps them. Self-contained: no hardware, no
   external binaries, fully testable on dev box
4. **P-recovery-0a next** — lowest-bar hardware adapter. Auto-
   populates the device inventory built in -0a. Validates the
   multi-adapter end-to-end on hardware the operator has today
5. **P-jtag-0a (lldb)** then **0b (jtag)** — heavier
   adapters; lean on the library adapter for symbol resolution
6. **0c / 0d / 0e** — destructive writes / auto-recovery /
   serial fallback. All operator-environment-specific

P0c lands first (we already need session_id for LLDB; better to
build it into the MCP framework than per-adapter). Then P-jtag
phases serial.

## 6. Open questions to settle before P0c

1. **Session identity** — agent-generated UUID, or echo a
   client-provided id from `tools/call params.session_id`? Server-
   generated is easier; client-provided lets the LLM pass session
   across sub-tasks without storing state. *Recommend server-
   generated + return in result; LLM threads it explicitly.*

2. **Adapter Close ordering vs sessions** — when MCP run exits,
   do we close all sessions first then close adapters, or vice
   versa? Sessions hold subprocess state owned by the adapter, so
   adapters know how. Recommend `adapter.Close()` is responsible
   for tearing down its own sessions.

3. **Session-aware tools/list** — should `lldb.read_register`
   appear in the catalogue when no session exists yet? The MCP
   spec doesn't forbid it; tools surface unconditionally and the
   `session_id` arg is documented as required. *Yes — always list,
   error at call time if no session.*

4. **Audit log for destructive tools** — write to per-host
   `polar-agent.audit.log` file, or push as `skill.event` of a
   new `audit` kind for dock to ingest? Dock-side gives operators
   the visible trail, file gives a permanent record. *Recommend
   both.*

## 7. Testing strategy — harness-only, no web clicks

**Hard rule for this skill: every adapter ships with a mock + a
scenario set in `cmd/polar-agent-test/`. No "open /console.html
and click around" allowed — that loop is too slow + non-
reproducible for a domain (low-level debug) where bugs are
race conditions / state-machine corners. The harness is the
only validation surface.**

This is enforced by convention, not tooling: PR review for a new
adapter rejects code without:
- a `mock_<adapter>.go` (mocked subprocess matching the real
  binary's wire shape closely enough that the adapter doesn't
  notice the difference)
- ≥4 scenarios under `scenarios.go`'s adapter section that
  cover happy path + error paths + at least one multi-step flow

### 7.1 Mock subprocess pattern

LLDB and JTAG are external binaries we can't assume are
installed. The pattern we shipped for VNC (`mock_vnc.go` — TCP
listener that speaks the wire shape) generalizes:

```go
// cmd/polar-agent-test/mock_lldb.go
type mockLLDBSubprocess struct {
    // Speaks just enough lldb-rpc-server wire to satisfy the 5
    // P-jtag-0a tools. Returns canned fixtures keyed by the
    // exact GDB-Remote-Protocol request bytes — adapter doesn't
    // know it's mocked.
    Pid  int
    Sock string  // unix socket the adapter dials into
}

// cmd/polar-agent-test/mock_jtag.go
type mockJTAG controller struct {
    // Pretends to be `vendor JTAG controller (interactive mode)` over a pipe. Has a
    // built-in fixture table for halt / read_reg / read_mem so
    // scenarios can pin expected values:
    //   m.SetRegFixture("ESR_EL1", "0x96000050")
    //   m.SetMemFixture(0xfffffe0010000000, []byte{...})
}
```

The adapter doesn't have a `--mock-mode` flag — it dials a
subprocess and reads bytes. The mock binary path is supplied
via env (`POLAR_LLDB_BIN=/path/to/mock`) at scenario startup,
which the adapter's factory honors as an override.

### 7.2 Scenario inventory

**P0c (MCP framework sessions)**:
- `mcp-session-create` — adapter that supports sessions returns
  a session_id; subsequent calls accept it
- `mcp-session-list` — `mcp.list_sessions` enumerates live
- `mcp-session-bad-id` — calling a session_id from another run
  returns -32000 with "unknown session"
- `mcp-session-idle-evict` — set idle=2s, wait 3s, verify
  session is gone via list

**P-library-0a (dock REST, no adapter yet)**:
Go integration tests under `internal/app/dock/rev_library_test.go`
— uses the existing `httptest` pattern. Not harness scenarios
(harness drives the agent, not dock directly).
- `library-devices-crud` — create / get / list / update
- `library-firmwares-ingest` — POST blob → sha computed →
  retrievable by id
- `library-firmwares-matching` — `{cpid, bdid}` returns only
  compat-matching firmwares
- `library-functions-lookup-by-addr` — fixture firmware with
  3 known functions → address-bracket queries return correct
  symbol
- `library-functions-match-signature` — same function bytes
  exist in firmware A + B → match returns both

**P-library-0b (library MCP adapter, mock-backed)**:
- `mcp-library-devices-list` — adapter dials test dock loopback,
  returns the list verbatim
- `mcp-library-functions-lookup` — round-trips a known address →
  symbol via the adapter
- `mcp-library-no-dock` — when dock loopback unreachable, the
  adapter loads as unavailable (soft fail; same convention)
- `mcp-library-cross-tool-flow` — **multi-step**: call
  `recovery.detect` (mock returns ECID 0x123) → call
  `library.devices.recent_seen` → verify the auto-upsert
  triggered by recovery actually landed a row visible to library

**P-recovery-0a (recovery adapter, mock-backed)**:
- `mcp-recovery-detect-empty` — no device on USB → empty list,
  not an error
- `mcp-recovery-detect-fixture` — mock irecovery returns 1
  device with known ECID/CPID/BDID, parsed into the result
- `mcp-recovery-query-shape` — `recovery.query` fixture returns
  full metadata block, all fields populated
- `mcp-recovery-send-command` — `nvram boot-args=...` request
  reaches mock subprocess in expected wire form
- `mcp-recovery-anya-unavailable` — when Anya not on PATH,
  `anya_status` returns "unavailable" instead of erroring
- `mcp-recovery-no-irecovery` — when even irecovery is missing,
  adapter loads as unavailable (soft fail per framework
  convention)

**P-jtag-0a (lldb adapter, mock-backed)**:
- `mcp-lldb-attach-mock` — happy path, session_id returned
- `mcp-lldb-read-register` — value flows from mock fixture
- `mcp-lldb-backtrace` — multi-frame list parses correctly
- `mcp-lldb-bad-session` — error shape
- `mcp-lldb-debug-flow` — **multi-step**: attach → set
  breakpoint at `main` → continue → assert stop_reason=
  breakpoint-hit → read pc → match expected fixture address.
  This is the scenario that proves the WHOLE LOOP works the way
  §4's example expects, not just individual tools.

**P-jtag-0b (jtag adapter, mock-backed)**:
- `mcp-jtag-no-probe` — adapter loads + immediately reports
  unavailable when `vendor JTAG controller` returns "no probe"
- `mcp-jtag-halt-reg` — halt, read PC + ESR_EL1, values match
  fixture
- `mcp-jtag-parse-pagetable` — given a fixture TTBR0 dump,
  decoded VA→PA list matches expected
- `mcp-jtag-mmu-fault-flow` — **multi-step, mirrors §4**:
  attach → halt → read ESR (fixture = 0x96000050) → read FAR →
  read TTBR0 → parse_pagetable → assert that the fault VA falls
  outside the mapped ranges in the decoded output. Proves the
  end-to-end "AI walks the page table fault" path works in
  isolation.

**P-jtag-0c (destructive writes, safe_mode gating)**:
- `mcp-jtag-write-rejected` — safe_mode=true (default),
  attempt write_reg → error "writes disabled"
- `mcp-jtag-write-allowed` — safe_mode=false, write_reg
  succeeds, audit event emitted
- `mcp-jtag-write-denylist` — safe_mode=false but PMGR_CLK_X
  on the denylist → still rejected

### 7.3 Dev loop with the harness

The intended iteration when working on these adapters:

```
1. Write/modify adapter code in skills/mcp_adapter_<name>.go
2. Add/modify mock in cmd/polar-agent-test/mock_<name>.go
3. Add/modify scenarios in cmd/polar-agent-test/scenarios.go
4. go run ./cmd/polar-agent-test orchestrate --only mcp-lldb-debug-flow
5. Read agent [agent] log lines interleaved with [PASS]/[FAIL]
6. Fix; goto 4. Iteration: ~3-8s per cycle.
```

NOT in the loop: building polar-dock, deploying, opening
console.html, picking host, picking skill, copy-pasting JSON-RPC
into anything. The harness orchestrates a fresh isolated
test-serve subprocess every time + tears it down on exit, so
each iteration is hermetic.

For real-hardware testing (LLDB attached to a real process,
JTAG attached to a real probe), the SAME scenarios run with
real subprocesses by setting `POLAR_LLDB_BIN=/usr/bin/lldb` etc.
— no separate test code path. Gated behind a tag like
`POLAR_TEST_REAL_LLDB=1` so CI doesn't need the hardware.

### 7.4 Anti-patterns (rejected at review)

- **"Test only the happy path; manual-verify error paths"** —
  the whole point of the harness is automation; error paths
  are the most error-prone code and MUST be covered
- **"Test the adapter in isolation with Go unit tests; skip
  scenarios"** — Go unit tests are fine + welcome for parsing
  logic, but the integration scenario is what proves the wire
  works
- **"Skip mock; only run scenarios when the binary is on PATH"**
  — defeats the goal. Every adapter needs a mock so the harness
  passes on a vanilla dev box / CI

## 8. What this design does NOT cover

- The MiroFish marketplace / billing / Surge+kcptun parts of
  `doc/jtag/all_arch.md` — that's the surrounding business
  layer. This is one specific Skill inside it.
- IDA Pro / Ghidra / Frida / radare2 adapters — same pattern, each
  is its own future P-* design.
- The dock-side LLM integration (`ai_agent.go` learning to call
  MCP tools) — covered separately by `doc/mcp-skill-dev.md`'s
  P1-mcp.
- A web UI for the MCP catalogue + session list (`doc/mcp-skill-
  dev.md` P7-mcp).
- Hardware autoflash recovery (P-jtag-0d above is a stub —
  needs its own design doc when we get there).

## 9. Recommended next action

Start **P0c (sessions in the MCP framework)** — it's the
prerequisite for both LLDB and JTAG adapters, and is dock/
hardware-independent. ~250 Go LOC, 4 harness scenarios (per §7.2).
Lands behind the same `go run ./cmd/polar-agent-test orchestrate`
loop we already use; can ship before anyone needs to touch LLDB
or get an JTAG probe.

**Concrete first session of work** (~3 hours, end-to-end):

1. Branch `feat/mcp-p0c-sessions` off main
2. Extend the MCP `MCPAdapter` interface with optional
   `SessionAware` sub-interface: `OpenSession() (id string, err
   error)`, `CloseSession(id string) error`, plus an
   in-MCP-server session map (id → adapter)
3. New built-in `mcp.list_sessions` / `mcp.close_session` tools
   (live in `mcp_server.go`, not in any adapter)
4. Update `stub` adapter to implement SessionAware → use it to
   write the 4 harness scenarios above
5. `go run ./cmd/polar-agent-test orchestrate` → 30+4 scenarios
   PASS in <30s
6. Commit + PR. P-jtag-0a can start immediately after merge.

After P0c lands, **P-library-0a + 0b are the next two steps in
parallel with P-recovery-0a**. The library work is dock-pure
(no hardware) so it can ship on a separate branch alongside
recovery; both feed each other (recovery auto-populates the
device inventory, library exposes lookup tools the LLM uses
during the recovery flow).

P-recovery-0a still wants to land too. Reasons:
- No Auth ticket, no probe, no Xcode — works on any board the
  operator can plug into USB
- Validates the multi-adapter framework end-to-end with real
  hardware (the operator has irecovery + a board today, per
  the 2026-05-16 update that triggered this section)
- Provides the universal "what is this device + which channel
  is available" pre-flight that LLDB and JTAG adapters
  later reference

Same dev pattern: write `mock_recovery.go` first (mock returns
canned `irecovery -q` output), write the 6 scenarios, then write
the adapter until they pass. Mock-first keeps the dev loop fast
and the API clean. Real-hardware run via
`POLAR_IRECOVERY_BIN=/opt/homebrew/bin/irecovery` env override.
