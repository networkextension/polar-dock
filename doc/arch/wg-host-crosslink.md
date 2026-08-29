# WG ↔ Host UI Cross-Link

Status: accepted · 2026-06-07
Scope: `polar-wg`, `polar-hosts`, `polar-agent` (data + UI only — **not** networking)

## Problem

A WG device (`polar-wg`) and a fleet host (`polar-hosts`) are very often the
**same physical machine** — it runs `polar-agent`, which registers twice:

- to **dock** → gets a `host_id` (`polar-hosts`),
- to **WG** → gets a `device_id` (`polar-wg`).

Today the two records share **nothing**, so the two module UIs can't link to
each other. Operators want to click a WG peer and jump to that host's detail
page (and back) — pure UI/data cross-navigation, not a protocol tunnel.

## Current identities (today)

| | key | minted by | file |
|---|---|---|---|
| WG device | `device_id` (opaque) + `pubkey` + `hostname` | wg-svc at `/v1/register` | `modules/polar-wg/internal/wg/store.go` (`WGDevice`) |
| Host | `id = sha256(salt+raw_uuid)[:32]` (v4) | dock at agent register | `internal/app/dock/agents_v4_store.go:69` (`hashHostID`) |

WG devices have **no** `host_id`, `machine_uuid`, or `workspace_id`. There is
no usable join key today (hostname is operator-chosen and not unique).

## Decision

Add a single soft-reference field **`host_id`** to `wg_devices`, populated at
WG registration time by the agent (which already knows its own `host_id` from
dock registration). Then both UIs deep-link via that shared key.

- **No cross-DB foreign key** — keep modules decoupled; `host_id` is an opaque
  string the WG side never dereferences itself.
- **Backward compatible** — legacy devices have `host_id = ""` and simply
  render without a link until the agent re-registers.
- **Cross-link convention** = absolute URL to the other module's subdomain +
  query param, matching the existing platform-nav convention (`/api/nav`
  already emits absolute `https://<module>.4950.store:2443/...` URLs).

## Data model

`wg_devices` (migration, additive):

```sql
ALTER TABLE wg_devices ADD COLUMN IF NOT EXISTS host_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS ix_wg_devices_host_id ON wg_devices(host_id) WHERE host_id <> '';
```

`WGDevice` struct gains:

```go
HostID string `json:"host_id,omitempty"`
```

`/v1/register` (and the heartbeat/touch path) accept an optional `host_id` in
the request body and persist it idempotently (update on re-register so a host
that re-keys keeps the link).

## Flow

```
polar-agent (startup):
  1. dock agent register  → receives host_id  (cached in agent.toml)
  2. wg /v1/register      → body includes { host_id, pubkey, hostname, ... }
  3. wg-svc stores host_id on the wg_devices row
UI:
  wg-tokens.html device row  →  https://hosts.4950.store:2443/hosts.html?id=<host_id>
  hosts.html detail panel    →  https://wg.4950.store:2443/wg-tokens.html?host_id=<host_id>
  hosts.html on load         →  parse ?id= → auto-select that host
  wg-tokens.html on load     →  parse ?host_id= → switch to Devices tab + highlight
```

## UI

- **polar-wg** (`ui/src/wg-tokens.ts`, devices table ~`renderDeviceRow`): when
  `dev.host_id` is set, render the hostname cell as a link to the hosts page;
  otherwise plain text. On load, `?host_id=` → open Devices tab + highlight the
  matching row(s).
- **polar-hosts** (`ui/src/hosts.ts`): parse `?id=` on load and auto-select;
  add a "在 WG Mesh 中查看" link on the detail panel pointing back to wg.

Star-topology view (`feat/wg-hub-topology-ui`, PR polar-wg#16): once `host_id`
is present, device nodes also become click-through to the host page — same link.

## Permissions / multi-tenant note

WG is hub/site-namespaced (not workspace-scoped); hosts are workspace-scoped.
`host_id` is globally unique, so the link is always well-formed, but the target
host page only resolves for a viewer who can see that workspace. Root/site-admin
is fine; for a plain tenant the link may 404 — acceptable (link is best-effort).

## Phasing

- **P1** `polar-wg` backend — schema + struct + register/heartbeat accept
  `host_id` + return it in device JSON. Additive, deployable immediately.
- **P2** `polar-agent` — send `host_id` in the WG register body.
- **P3** UI deep-links — wg→hosts, hosts→wg, `?id=` / `?host_id=` parsing, and
  topology node click-through.

## Out of scope

- Protocol-layer jump/SSH-ProxyJump (separate concern; the agent already
  provides a NAT-proof TCP relay via the VNC skill if that's ever wanted).
- Backfilling `host_id` for existing devices via any heuristic — they fill in
  naturally on the next agent re-register.
