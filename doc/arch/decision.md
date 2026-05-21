# ADR: Polar- service split — Dock core + independent plugin services

**Status:** Accepted (2026-05-21)
**Supersedes:** `doc/arch/new.md`, `doc/arch/split.md`
**Related:** `doc/arch/internal-api-v1.md`, `doc/arch/database-ownership.md`

## Context

Polar- today is a single Go binary `polar-dock` (~50K LOC, 148 files in one flat `package dock`), with 18 distinct business domains, 337 routes, and 1030 methods on a `*Server` god-object. All domains share one Postgres database and one process lifecycle. Nginx and Redis are already in front, but used only for TLS termination and cache.

This monolith has two pain points that compound:

1. **Every change requires restarting Dock.** A 5-line tweak to the iOS-distribution module forces every active chat WebSocket to drop, every wg-mac client to reconnect, every Headscale embedded process to restart. Release cadence is hostage to the riskiest domain.
2. **Third-party teams cannot extend Polar- without reading the entire source tree.** A hypothetical "datacenter management" plugin would need DB grants, would risk schema drift on the next release, and would have no clear contract to code against.

## Decision

We will split Polar- into:

- **Dock core** (one process, eventually open source): `auth` + `users` + `teams` + `llm` (configs + proxy + billing) + `chat` (1:1 + rooms) + `markdown` + `social` (posts / replies / likes).
- **Plugin services** (one process per plugin, each optionally open source, each with its own database): `wg-mac`, `iosdist`, `video`, `hosts` + `agent`, `latch`, `packtunnel`, `library`, `expense`, `projects`.
- **Nginx** in front, fanning routes to the right process by path prefix.
- **Redis** stays shared for auth cache, chat events, and similar small coordination state.

Plugins reach the shared core through a **stable, narrow, HMAC-authenticated internal API** (`/internal/v1/*`, 9 endpoints — see `internal-api-v1.md`). They never touch Dock's database directly.

This is a **mix of `split.md` and `new.md`** from the same `doc/arch/` directory:
- Topology and routing match `split.md` — Dock is a peer service behind Nginx, not a host process for in-tree plugins.
- Shared-data access matches `new.md` — plugins call Dock's internal API rather than reading its tables.

## v2 corrections vs an earlier draft

An earlier draft of this plan included two intermediate steps. Both are removed:

| Earlier proposal | Why removed |
|---|---|
| **Phase 1 "internal modularization"** — slice domains into `internal/plugins/<name>/` Go subpackages, all still compiled into the `polar-dock` binary, exposing a `Module` interface. | An in-process module still ships and restarts with Dock. Reaching the **"upgrade a plugin without restarting Dock"** goal — which the user identified as the single largest value driver — requires real process boundaries from day one. Internal modularization would be 6–8 weeks of refactor for a milestone that doesn't move the needle on the actual goal. |
| **Shared database for Phase 1–3** — every plugin keeps writing to the main `ideamesh` Postgres, only its own tables. | Once third-party teams write plugins, a shared database becomes a schema battleground: every plugin migration competes for one timeline, every backup is monolithic, and table-name collisions are inevitable. We pay the data-split cost once (per plugin), upfront, and never again. |

## Locked design decisions

### 1. Process boundary: one binary per plugin

Each plugin is an independent Go binary in `cmd/<plugin>-svc/`. They are deployed as separate containers / services. Restarting a plugin restarts that one process; Dock and all other plugins are unaffected.

### 2. Database ownership: one Postgres database per plugin (same instance)

Each plugin owns its tables in a dedicated Postgres **database** (e.g. `polar_wg`, `polar_iosdist`) hosted in the **same Postgres instance** as Dock's `ideamesh` for operational simplicity. The plugin's connection string only opens that one database; it has no grant on Dock's tables.

If a plugin later needs independent scaling, the database moves to its own instance — that decision is operational and reversible.

See `database-ownership.md` for the table-by-table assignment.

### 3. Route dispatch: Nginx fans out by path prefix

```nginx
location /api/wg/        { proxy_pass http://wg-svc:8090; }
location /api/iosdist/   { proxy_pass http://iosdist-svc:8091; }
location /api/           { proxy_pass http://dock:8080; }       # everything else → core
```

Clients see one origin (`polar.example.com`) and zero plugin-specific configuration. Backend swap is invisible to clients.

### 4. Internal-API auth: HMAC-signed header per plugin

Each plugin holds an opaque shared secret (`PLUGIN_INTERNAL_KEY`, ≥ 32 bytes). Outgoing calls to `/internal/v1/*` carry an HMAC-SHA256 signature over `(method, path, timestamp, body_hash)` in a `X-Polar-Plugin-Sig` header. Dock verifies the signature, rejects requests with a timestamp older than 5 minutes (replay window), and looks up the plugin by its name in the `plugin_modules` table.

**Why not JWT?** Polar- has no JWT signing infrastructure today. Sessions live in Redis as opaque tokens. Adding asymmetric-key JWT plumbing for 10 services is significant build-and-rotate overhead with no concrete benefit over HMAC.

**Why not mTLS?** Operationally expensive for a deployment of this size — certificate lifecycle, distribution to plugin binaries, rotation runbooks. Revisit if the plugin count exceeds ~30 or if we move to a service-mesh substrate.

### 5. End-user auth across the split: session-introspection

When a plugin receives a user-originated request, it forwards the user's `Authorization: Bearer <token>` to Dock at `/internal/v1/auth/verify`. Dock returns `{user_id, role, workspace_id}` or 401. The plugin caches the result in-memory for **30 seconds** to absorb the per-request cost.

Dock remains the single source of truth for session validity. Logout, role change, and workspace switch all take effect within 30 seconds across every plugin without coordination.

**Why not reissue sessions as JWTs?** Same as decision 4 — no upside, more moving parts. Plus JWT revocation is harder than session deletion.

### 6. UI: bundled in Dock through Phase 3

Plugin frontends live in `ui/src/views/<plugin>/` and ship inside the Dock UI bundle. The UI calls `/api/<plugin>/*` — Nginx routes the request to the right process. UI doesn't know or care where the backend lives.

Micro-frontend splitting (separate UI bundles per plugin) is **Phase 4+** territory. ROI is low at our component sizes (each plugin page is < 1000 LOC of UI).

### 7. Rollback: feature flag + temporary in-process stub per extraction

Every plugin extraction ships behind a `POLAR_<NAME>_REMOTE` environment flag. When false, Dock keeps serving the plugin's routes from in-process code; when true, Dock returns 404 on those paths and trusts Nginx to route them to the plugin service.

The in-process stub is kept in Dock for one release cycle after the cutover. If anything goes wrong, flip the flag and redeploy Dock to restore service in minutes. The stub is deleted in the release after the rollback window closes.

## Consequences

**Positive:**
- Plugin upgrades become independent operational events — Dock keeps serving chat / markdown / posts while wg-svc restarts.
- Third-party teams get a stable contract (`/internal/v1/*`) and a single source of authentication. Their plugin can live in any repository, be written by any team, and ship on its own cadence.
- Each plugin's data layer can evolve without Dock review.
- Dock's source tree shrinks to the open-sourceable core.

**Negative:**
- Every plugin → core lookup is now an HTTP hop. Mitigated by HTTP keepalive + 30s auth cache (target: < 5ms overhead at P95).
- Cross-domain queries that used to be one JOIN become two API calls and an in-memory merge. We accept this; the queries that need it are admin pages, not hot paths.
- Operational footprint grows from 1 process to ~10. Mitigated by docker-compose / k8s manifests + a `plugin_modules` heartbeat table that surfaces per-plugin health in Dock's admin UI.

**Neutral:**
- Same Postgres instance, separate databases → operational cost is unchanged. If a plugin's load grows, it can move to its own instance later.

## Plugin registry

Dock owns a `plugin_modules` table to track which plugins exist, where they live, and when they last heartbeat:

```sql
CREATE TABLE plugin_modules (
    name            TEXT PRIMARY KEY,            -- 'wg', 'iosdist', 3rd-party names
    display_name    TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    endpoint        TEXT NOT NULL,               -- 'http://wg-svc:8090'
    last_heartbeat  TIMESTAMPTZ,
    version         TEXT,
    plugin_key_hash TEXT NOT NULL,               -- sha256 of HMAC secret
    config          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

An admin UI at `/admin/plugins.html` shows the list with heartbeat freshness, lets an operator disable a plugin (Dock starts returning 503 on its public routes), and supports creating new plugin rows (generates a fresh HMAC secret to hand to the plugin team).

## Phased execution

This ADR locks the architecture. Execution is phased in `doc/arch/wg-svc-extraction.md` and the project plan file:

| Phase | Scope | Duration |
|---|---|---|
| **0** | These 3 ADR / spec docs | 1–2 weeks (now) |
| **1** | First plugin extraction: wg-mac | 4–6 weeks |
| **2** | Remaining 9 plugins, two per wave | 3–5 months |
| **3** | SDK extraction + third-party plugin onboarding | 4–8 months |
| **4** | Dock trimmed to core + open source | 6–12 months |

Phase 1 starts as soon as decision.md, internal-api-v1.md, and database-ownership.md are merged. Other Phase 0 docs (plugin-registry.md, wg-svc-extraction.md, plugin-developer-guide.md) are written in parallel with Phase 1.

## Out of scope

- Micro-frontend UI splitting (Phase 4+).
- gRPC, service mesh, Consul/Nacos service discovery (over-engineered at our scale).
- Kafka / NATS / heavyweight message bus (Redis pub/sub + HTTP suffice).
- Multi-tenant SaaS deployment topology (business need has not surfaced).

## Open questions deferred to Phase 1 kickoff

- **Projects:** the `projects` domain is tightly intertwined with `chat`, `tasks`, and `markdown`. It may belong in core forever rather than being extracted. Decide during Phase 2 wave W4 kickoff.
- **iOS public install routes:** `/iosdist/install/:token` is high-traffic and cache-friendly. May be split into a Dock-served signed-URL emitter + an iosdist-svc admin/worker surface. Decide during Phase 2 wave W1 kickoff.
- **Per-plugin Postgres instances:** when does a plugin justify its own instance? Likely never for `wg`, possibly yes for `iosdist` or `video` when they grow. Decide per-plugin in Phase 4.
