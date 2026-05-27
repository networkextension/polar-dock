# Agent Identity v4

Date: 2026-05-27
Author: based on user feedback during deploy of the (now obsolete) `machine_uuid` PR series

## TL;DR

Replace the "hardware UUID as host identity" design (shipped 2026-05-27 in
polar-hosts #11, polar-dock #354, polar-sdk #6, polar-agent #12) with a
two-table model:

- **`hosts`** — physical asset. PK = `sha256(salt + raw_machine_uuid)`
  computed server-side. Stores hardware facts + capacity peaks.
- **`agents`** — logical agent instance. PK = `ag_<random32>` issued by
  server on register. FK → hosts. One host : N agents.

The bug fixed: the v3 model wedged "one physical machine = one host row",
forcing 1000 machines × 5 agents = a UNIQUE-violation field day. v4
separates the two concerns.

## Why drop the v3 machine_uuid-as-PK approach

- A physical machine can sensibly run N agents (different bots,
  different workdirs, different LLM proxy tokens). v3's `UNIQUE
  (workspace_id, machine_uuid)` makes this an error.
- Hardware UUID leaks into the DB as identity, which becomes a
  cross-tenant deanonymization risk.
- Hardware UUID changes (logic board swap, VM clone, restore) silently
  break dedup.
- Three platform-specific collectors for a concept that need not exist.

## Two tables

### `hosts` — physical asset

```sql
CREATE TABLE hosts (
  id              TEXT PRIMARY KEY,         -- sha256(salt+raw_uuid)[:32] hex
  workspace_id    TEXT NOT NULL REFERENCES teams(id),
  hw_model        TEXT,                     -- "Mac15,8", "Dell PowerEdge R760xa"
  cpu_brand       TEXT,
  cpu_cores       INT,
  mem_total_bytes BIGINT,
  mem_peak_bytes  BIGINT,                   -- rolling 24h peak from agent samples
  cpu_peak_pct    REAL,                     -- rolling 24h peak
  os              TEXT,                     -- darwin / linux / freebsd
  arch            TEXT,                     -- arm64 / amd64
  first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  -- Reserved for a future phase (this round skips collection):
  --   gpu_components JSONB    -- [{model, serial, slot, status, failed_at}]
  --   faults         JSONB    -- component fault history
  UNIQUE (workspace_id, id)
);
CREATE INDEX idx_hosts_workspace_lastseen ON hosts(workspace_id, last_seen_at DESC);
```

Notes:
- `id` deliberately NOT a sequential int — using sha256 prefix makes the
  identity location-independent (same physical box across reinstalls
  still resolves to same row).
- Salt is a server-side secret (`POLAR_HOST_UUID_SALT` env). Never
  exposed to agents or clients.
- Agent uploads RAW machine_uuid in the register/hello frame; server
  hashes immediately and discards raw. Raw never persists.

### `agents` — logical instance

```sql
CREATE TABLE agents (
  id              TEXT PRIMARY KEY,           -- ag_<random32hex>
  workspace_id    TEXT NOT NULL REFERENCES teams(id),
  host_id         TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  name            TEXT NOT NULL,              -- operator label
  bot_user_id     TEXT REFERENCES bot_users(user_id),
  agent_token_id  TEXT NOT NULL REFERENCES agent_tokens(id),
  os              TEXT,
  arch            TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_hello_at   TIMESTAMPTZ,
  UNIQUE (workspace_id, name)                 -- operator-visible naming;
                                              -- enforce per-workspace
);
CREATE INDEX idx_agents_host        ON agents(host_id);
CREATE INDEX idx_agents_workspace   ON agents(workspace_id, last_hello_at DESC);
CREATE INDEX idx_agents_bot         ON agents(bot_user_id) WHERE bot_user_id IS NOT NULL;
```

Notes:
- `(workspace_id, name)` UNIQUE protects against operator typos creating
  multi-name shadows. Override with `polar-agent register --name=<x>
  --force` if a real rename is intended.
- One host : N agents naturally.

## Register flow

```
agent (cli)                  dock
─────────────────            ─────
register --token=<enroll>
  --name=emei-kimi
                  POST /api/agents/register
                  body: {
                    enroll_token,
                    name: "emei-kimi",
                    machine_uuid_raw,
                    host_info  // hw_model, cpu_brand, mem_total, ...
                  }
                  ────────────────────────▶
                                              ┌─ validate enroll_token
                                              ├─ host_id = sha256(salt+uuid)
                                              ├─ UPSERT hosts (host_id, ...)
                                              │    on new: insert + record first_seen
                                              │    on existing: update last_seen + facts
                                              ├─ agent_id = ag_<rand32>
                                              ├─ proxy_token = ensureWorkspaceAgentPoolProxyToken(ws)
                                              ├─ bot = createBotForAgent(
                                              │    name="bot-<agent.name>-<short(agent_id)>",
                                              │    llm_kind="proxy",
                                              │    proxy_token_id=proxy_token.id)
                                              ├─ agent_token = mintAgentToken(agent_id)
                                              └─ INSERT agents (agent_id, host_id, name,
                                                                bot_user_id, agent_token_id)

                  ◀──────────────────────── response:
                  {
                    agent_id:    "ag_a1b2...",
                    bot_user_id: "bot_xxx",
                    token:       "polar_agent_<raw>",
                    server:      "https://zen.4950.store:2443"
                  }

writes ~/.polar/agent.toml
```

## Agent.toml shape

```toml
server      = "https://zen.4950.store:2443"
agent_id    = "ag_a1b2c3d4e5f6..."
token       = "polar_agent_<raw>"
bot_user_id = "bot_xxx"
# optional: explicit override of the auto-bound bot
# bot_user_id = "bot_some_shared_bot"
```

If user backs up agent.toml + reinstalls OS → same agent_id resolves
to the same row, full history continuous.

## Hello frame

```json
{
  "type": "hello",
  "agent_id": "ag_a1b2c3d4...",       // required (post-migration)
  "machine_uuid_raw": "<raw>",        // hashed to verify host_id consistency
  "host_info": {
    "hw_model": "Mac15,8",
    "cpu_brand": "Apple processor",
    "cpu_cores": 16,
    "mem_total_bytes": 51539607552,
    "mem_peak_bytes": 32212254720,    // agent samples since last hello
    "cpu_peak_pct": 92.4,
    "os_name": "macOS",
    "os_version": "14.6.1",
    ...
  }
}
```

Server:
- Resolve `agents WHERE id = agent_id` (primary identity).
- Recompute `host_id = sha256(salt + machine_uuid_raw)`; if it differs
  from `agents.host_id`, log a warning ("agent migrated to new
  hardware?") but DON'T break — update the FK.
- UPDATE hosts: `last_seen_at = NOW()`, GREATEST of mem_peak / cpu_peak,
  refresh hw fields if changed.
- UPDATE agents: `last_hello_at = NOW()`.

## Bot auto-create via LLM proxy

Every workspace has (or gets on first agent register) one
`llm_proxy_tokens` row named `agent-pool`. All bots auto-created at
register bind to this token.

```go
// On register:
proxyToken := ensureWorkspaceAgentPoolProxyToken(workspaceID)
bot := createBotForAgent(workspaceID, BotForAgent{
    Name:      fmt.Sprintf("bot-%s-%s", slugify(agentName), shortHash(agentID)),
    LLMKind:   "proxy",
    LLMConfig: map[string]any{"proxy_token_id": proxyToken.ID},
    Mode:      "tool-loop",
})
```

Billing: a single SQL groups all auto-bot LLM calls per workspace:

```sql
SELECT workspace_id, SUM(cost_usd), COUNT(*)
FROM llm_proxy_calls
WHERE llm_proxy_token_id = (
  SELECT id FROM llm_proxy_tokens
  WHERE workspace_id = $1 AND name = 'agent-pool'
)
AND created_at >= $2;
```

Operator can override at register: `polar-agent register --bot=bot_existing`
to skip auto-create + bind to a shared bot.

## Migration: 4 existing hosts

The 4 currently-live hosts have:
- `polar_hosts.hosts.id` = `h_<random32>` (NOT sha256, legacy format)
- `polar_hosts.agent_tokens.id` linked to hosts via `hosts.agent_token_id`
- `agent.toml` on each box has `server` + `token` only (no `agent_id`)

Migration (P1, atomic, same PR as schema):

```sql
-- Schema
BEGIN;
CREATE TABLE agents (...);   -- as above
ALTER TABLE hosts ADD COLUMN mem_peak_bytes BIGINT;
ALTER TABLE hosts ADD COLUMN cpu_peak_pct REAL;
ALTER TABLE hosts DROP COLUMN machine_uuid;  -- the v3 mistake
DROP INDEX IF EXISTS uniq_hosts_workspace_machine_uuid;
COMMIT;

-- Backfill 4 agents from existing hosts rows:
INSERT INTO agents (id, workspace_id, host_id, name, agent_token_id,
                    bot_user_id, os, arch, created_at, last_hello_at)
SELECT 'ag_' || encode(gen_random_bytes(16), 'hex'),
       workspace_id, id, name, agent_token_id,
       NULL,                          -- bot_user_id backfill below
       os, arch, created_at, host_info_seen_at
FROM hosts;

-- For each of the 4 existing hosts: ssh box, write agent_id to agent.toml
for host in .57 .69 .64 .2.2; do
  ag_id=$(psql -tA -c "SELECT id FROM agents WHERE host_id=(SELECT id FROM hosts WHERE name='<host_name>')")
  ssh $host "polar-agent set-agent-id $ag_id"
done

-- Existing bots: leave alone for now. The 4 currently-attached bots
-- (kimi/claude/codex/tool-loop on .57; bot_NSTzBPim4VEVxZSn on emei;
-- bot_RlKHvOZXyydGXM1i on jiuhua; bot_pKinHPrTERnYdW9J on dpaa2) are
-- operator-chosen. UPDATE agents SET bot_user_id = <known bot> per host
-- as a one-off, then the agent.toml on each box gets bot_user_id added
-- on next set-agent-id invocation.
```

Existing `hosts.id` keeps its `h_<random>` legacy format. New hosts
created post-migration use the `sha256(salt+uuid)` format. The two
formats coexist; PK is opaque from any caller's perspective.

## CLI additions

```
polar-agent set-agent-id <ag_xxx>
    write agent_id + bot_user_id (if known) into ~/.polar/agent.toml.
    Used for migrating an existing install to the new identity model.

polar-agent register [--server=...] [--token=...] [--name=...]
                     [--bot=bot_xxx]
    --bot is now optional: if omitted, server auto-creates a bot bound
    to the workspace's agent-pool proxy token.

polar-agent attach
    no longer requires --bot. Reads bot_user_id from agent.toml.
    --bot=xxx still overrides.
```

## PR sequence

| # | Repo            | Branch                          | Content                                                                                                                                                                                                |
|---|-----------------|---------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| A | polar-agent     | feat/default-server-zen (#13)   | Already open. Merge + tag v0.3.2. Independent of the rest.                                                                                                                                              |
| B | polar-sdk       | feat/agent-id-protocol          | Add `AgentRegister(req) → resp{agent_id, bot_user_id, token, server}`. Deprecate `IssueHost.MachineUUID`. Tag v0.2.4.                                                                                  |
| C | polar-hosts     | feat/agents-table-v4            | Schema: create agents table, drop hosts.machine_uuid + UNIQUE; add hosts.mem_peak_bytes / cpu_peak_pct. createHost rewritten as UPSERT-host + INSERT-agent + auto-bot + auto-proxy-token. Backfill 4 rows. |
| D | polar-dock      | feat/agents-table-v4            | Same schema additions in ideamesh; rewrite register handler symmetric to polar-hosts; hello handler parses agent_id; agent_hub keyed by agent_id (not bot).                                              |
| E | polar-agent     | feat/persist-agent-id           | agent.toml grows agent_id + bot_user_id fields. set-agent-id CLI. register reads response + writes toml. attach defaults bot from toml. hello frame sends agent_id + machine_uuid_raw.                  |
| F | polar-dock-ui   | feat/hosts-agents-split-ui      | /hosts.html = physical asset view (memory/cpu peak trend, ready for future GPU). New /agents.html = instance view (bot binding, last hello, link to host). Sidebar nav update.                          |
| G | (deploy)        | manual                          | Schema migration + 4-box agent.toml backfill + smoke (new register on a fresh box).                                                                                                                    |

Dependencies:
- B before C/D/E (SDK tag)
- C+D land together (schema must be coherent across both DBs)
- E after D (agent expects server to understand agent_id)
- F after E (UI uses new fields)
- G after F

Estimate: ~15h across all phases.

## What we delete from the v3 ship

| File                                                   | What                                                              |
|--------------------------------------------------------|-------------------------------------------------------------------|
| polar-agent `cmd/polar-agent/hostinfo/hostinfo.go`     | `HostInfo.MachineUUID` field                                      |
| polar-agent `cmd/polar-agent/hostinfo/hostinfo_darwin.go` | `collectMachineUUIDDarwin` + `binIoreg` const                  |
| polar-agent `cmd/polar-agent/hostinfo/hostinfo_linux.go`  | `/etc/machine-id` reader                                       |
| polar-agent `cmd/polar-agent/hostinfo/hostinfo_freebsd.go`| `kenv smbios.system.uuid` reader                               |
| polar-agent `cmd/polar-agent/register.go`              | machine_uuid-in-register-body logic                                |
| polar-sdk `extras.go`                                  | `HostIssueRequest.MachineUUID`, `HostIssueResponse.MachineUUID`   |
| polar-dock `internal/app/dock/internal_api_handlers.go`| machine_uuid parsing in `handleInternalHostIssue`                  |
| polar-hosts `internal/hosts/hosts_store.go`            | createHost dedup-by-machine-uuid SELECT path                       |
| polar-hosts schema                                     | `machine_uuid` column + `uniq_hosts_workspace_machine_uuid`       |
| polar-dock schema                                      | same                                                              |

Replaced by:
- raw machine_uuid in register/hello body (transient, never persisted as raw)
- `hosts.id` = sha256(salt + raw)
- agent.toml-persisted `agent_id`

## Why this isn't just "agent.toml stores host_id"

The simpler alternative — agent persists the existing `hosts.id` — works
for re-installation but doesn't solve N-agents-per-machine: each agent
gets its own row, but how do operators see "all of emei's agents together"?
The two-table split makes that join trivial:

```sql
SELECT h.hw_model, a.name, a.bot_user_id, a.last_hello_at
FROM hosts h JOIN agents a ON a.host_id = h.id
WHERE h.id = (SELECT host_id FROM agents WHERE name = 'emei-kimi')
ORDER BY a.last_hello_at DESC;
```

And future GPU/component fault tracking is a clean extension of `hosts`
without touching agent semantics.

## Open / deferred

- GPU + component collection (nvidia-smi, ipmi for fault state). Deferred
  to a follow-up; this round only adds the mem_peak / cpu_peak fields.
- `hosts.id` legacy `h_<random>` format for the 4 existing rows is left
  alone — coexists with new sha256 format. Could be backfilled later if
  the inconsistency bothers anyone (it shouldn't — the ID is opaque).
- `agents.name` UNIQUE per workspace may be too strict for some
  multi-tenant edge cases. Soft assertion (warning) might be enough; can
  relax to non-UNIQUE if a real case appears.
- Future: agent auto-rotation (if `agent.toml` is lost the agent can't
  recover its identity; a "device claim" flow would let the operator
  recover via web UI). Out of scope here.
