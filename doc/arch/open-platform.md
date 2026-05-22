# ADR: Open Platform Architecture — Plugins as Standalone Products

**Status**: Accepted, 2026-05-22
**Supersedes**: the "iframe-microfrontend" assumption in `doc/arch/decision.md` and the v2 plan's Phase 4 sketch in `/Users/apple/.claude/plans/piped-growing-hearth.md`
**Related**: [auth-and-tokens.md](./auth-and-tokens.md), [internal-api-v1.md](./internal-api-v1.md), [database-ownership.md](./database-ownership.md)

## Context

Phase 4 of the extraction roadmap was originally framed as a microfrontend exercise: dock's UI shell embeds plugin pages via iframe; plugins are tightly coupled to dock's bundle. After review (2026-05-22 design conversations), that framing was rejected in favor of a fundamentally more open model.

The repositioning is strategic:

- **Dock = identity + LLM proxy + collaboration core**, open-sourced. Comparable to Clerk/Auth0 + Helicone/Portkey + light Slack.
- **Plugins = vertical business apps** (iosdist, video, wg-mac, hosts, projects, packtunnel, library, expense, latch, agent). Each is its own product, deployable independently, developable by external teams without reading dock source.
- **Plugin 升级 ≠ 重启 dock** is the load-bearing constraint that drove the Phase 1-3 process boundary; this ADR extends the same principle to UI.

Companies that get this right (GitHub Apps, Slack Apps, Backstage plugins, ServiceNow apps): platform owner publishes contracts; business apps are owned by app teams; deployment is independent.

## Non-goals (explicitly out of scope)

- **Cross-dock plugin portability**. A plugin instance binds to exactly one dock URL. HA = L7 load balancer in front of multiple dock backends presenting one URL. "Same plugin serving company A's dock AND company B's dock simultaneously" is deferred indefinitely. Rationale: billing boundary, identity coordination, and CORS/frame-ancestors complexity are all dramatically simpler under single-dock binding.
- **Microfrontend bundle stitching** (Module Federation, single-spa). Defer until iframe UX actually hurts in production with a real plugin ecosystem.
- **Auto-publishing dock-installed plugins to a public marketplace**. Marketplace is Phase 4+ and operator-curated.
- **Plugins running dock's own database queries**. Plugins talk to dock via public REST API only. See `database-ownership.md`.

## Decision summary

1. **Plugin is a standalone web product**. It has its own UI (separate web app, separate URL, separate framework choice) and its own backend service (already established in Phase 2). Dock registers it via the existing `plugin_modules` table.

2. **Plugin manifest declares `display_mode`** — one of:
   - `tab` (**default**): sidebar entry is a link. Click opens plugin URL in new tab. Plugin owns its own chrome.
   - `iframe`: dock chrome (sidebar + topbar) wraps plugin content. Opt-in for first-party tight integration.
   - `standalone`: not on sidebar. Headless or "configure-and-run" plugin; user accesses it via plugin's own URL.

3. **Single-dock binding**. Plugin reads exactly one `POLAR_DOCK_URL` env var at boot. HA is the operator's problem via L7 load balancer.

4. **All plugin auth via dock-issued tokens**. Three token types, all defined in `auth-and-tokens.md`:
   - `polar_plugin_*` (HMAC): service-to-service, plugin → dock `/internal/v1/*`
   - `polar_proxy_*`: LLM workhorse, plugin → dock `/api/proxy/v1/chat/completions`
   - `polar_pat_*`: user-acting, plugin → dock `/api/v1/*` on user's behalf

5. **Plugin's UI is hosted by plugin operator**, not bundled into dock. Operator points it at a URL (any host, any framework, any deployment). Dock's sidebar carries the URL.

6. **Public API surface is the product**. `/api/v1/*` is versioned, OpenAPI-documented, SDK-shipped. Open-source release: API spec + SDK packages (Go/TS/Python) first; dock source code second.

7. **First-party plugins follow the same rules**. Polar's own 10 plugins use `tab` mode like everyone else. No special integration paths for "we wrote it." Reason: dogfood the public API; uniform UX; one loading mechanism to maintain.

## Plugin manifest

Lives at `https://<plugin-host>/.well-known/polar-plugin.json` (or operator-pasted into dock's admin UI for plugins that don't expose this URL).

```jsonc
{
  "name": "iosdist",                          // matches plugin_modules.name
  "display_name": "iOS Distribution",
  "version": "1.2.3",
  "icon_url": "https://iosdist.example.com/icon.svg",

  "backend": {
    "endpoint": "https://iosdist.example.com",  // for heartbeat + /internal calls
    "health_path": "/healthz",
    "openapi_url": "/openapi.json"               // optional, for dock to introspect plugin's own API
  },

  "ui": {
    "display_mode": "tab",                       // tab | iframe | standalone
    "url": "https://iosdist.example.com/app",    // landing URL
    "deep_link_param": "path"                    // query param dock uses to deep-link, e.g. ?path=/builds/42
  },

  "auth": {
    "requires_plugin_token": true,               // HMAC token for /internal/v1/*
    "requires_llm_proxy": true,                  // LLM proxy token, auto-minted at install
    "requires_pat_scopes": ["read:projects"]     // optional; if set, install flow asks user to grant
  },

  "permissions": {
    "internal_endpoints": [                      // whitelist of /internal/v1/* it actually uses
      "/internal/v1/users/:id",
      "/internal/v1/teams/:id"
    ]
  },

  "embed": {                                     // only meaningful when display_mode=iframe
    "allowed_dock_origins": ["https://zen.4950.store"],
    "sandbox": "allow-scripts allow-same-origin allow-popups"
  }
}
```

`plugin_modules.config` JSONB column stores this manifest verbatim at install time; dock re-fetches on user-triggered "refresh manifest."

## display_mode contracts

### `tab` (default)

- Sidebar entry: `<a href="<ui.url>?token=<short-lived-jwt>&deep_link=<path>" target="_blank">`
- Plugin handles its own auth landing: read query token, exchange for session, set its own cookie/localStorage
- Plugin can use any framework, any router, any CSS
- Deep link in URL bar reflects plugin's own routes; dock has no responsibility to mirror them
- Browser back button works naturally per browser tab

**Token exchange**: short-lived (5 min) JWT is signed by dock with `aud=<plugin-name>`. Plugin's frontend posts it to its own backend; backend verifies against dock via `/internal/v1/jwt/verify` (new endpoint, see auth-and-tokens.md). This avoids leaking long-lived PATs in URL bars.

### `iframe`

- Sidebar entry stays in dock; clicking renders `<iframe src="<ui.url>?...">` in content area
- `sandbox` attribute applied per manifest
- Dock and plugin communicate via `postMessage`:
  - `init`: dock → plugin, `{theme, locale, jwt}`
  - `path-change`: plugin → dock, `{path}` so dock can mirror to outer URL (deep linking)
  - `navigate`: plugin → dock, `{url}` for cross-plugin navigation
  - `token-refresh`: plugin → dock → response with fresh JWT
- `Content-Security-Policy: frame-ancestors <dock-origin>` MUST be set by plugin's UI server, otherwise dock refuses to render the iframe (defense against clickjacking)
- This mode is opt-in. First-party plugins MAY use it; we recommend `tab` even for first-party for consistency.

### `standalone`

- Not on sidebar
- Plugin's UI is accessed via plugin's own URL only; users bookmark it
- Useful for: CI integrations, dashboards consumed by external monitoring, agent-only plugins (no human UI)
- Plugin still registered in `plugin_modules` for heartbeat + service-to-service auth

## Sidebar integration

Dock fetches `plugin_modules WHERE enabled=true AND last_heartbeat > now()-interval '90 seconds'` → groups by `config->>'category'` → renders sidebar section.

Sidebar entries that are `display_mode=standalone` are omitted (admin can still see them in `/admin-plugins.html`).

`/api/v1/me/sidebar` endpoint returns the user-personalized sidebar shape (plugin entries the user has permission to use, given their scopes).

Plugins do NOT inject sidebar HTML themselves. They declare their entry in manifest; dock renders it. Defense against XSS via plugin-supplied DOM.

## Public API surface

### Versioning

- All public endpoints under `/api/v1/*`. Future v2 lives at `/api/v2/*` (full duplication, no header-based versioning).
- `Sunset` HTTP header announces removal date for deprecated endpoints.
- Internal-only endpoints stay at `/internal/v1/*` (HMAC-gated, never public).

### OpenAPI

- `/api/v1/openapi.json` served by dock at runtime
- Generated from Go struct tags + handler comments (we'll use go-swagger or oapi-codegen — decision deferred)
- Published to `https://api.polar.example.com/openapi.json` as the canonical spec
- SDK packages (Go/TS/Python) auto-generated from spec; releases tagged to dock release

### Stability commitments (post-1.0)

- v1 endpoints: no breaking changes. Add fields only; never remove.
- New endpoints: experimental for one release before promotion
- Auth/billing semantics: changes require ADR + 90-day notice via Sunset header

## Single-dock binding rationale

| Concern | Why single-dock wins |
|---|---|
| Billing | Plugin uses LLM proxy → cost lands on one team's quota. Multi-dock = which team pays? |
| Identity | User on dock A and dock B are different identities; plugin doing cross-correlation is dangerous |
| CORS / frame-ancestors | Single dock origin → one allowed list. Multi-dock → plugin must accept N origins, harder to audit |
| Heartbeat / health | `plugin_modules.endpoint` is one URL → one heartbeat path. Multi-dock = N heartbeats per plugin instance |
| Webhook auth | Plugin signs webhooks with HMAC keyed to "the" dock. Multi-dock = N keys per plugin |
| Operator clarity | "Plugin failed" investigated on one dock's logs, not multiple |

The only loss is "one plugin serving multiple customer docks simultaneously." That's a SaaS plugin operator's concern, not a v1 requirement. If it ever becomes one, the path is: plugin operator runs N plugin instances, each bound to one dock — same binary, different env vars, different processes.

## Plugin lifecycle

```
operator                dock                       plugin
  │                       │                          │
  │ POST /admin/plugins   │                          │
  │ {manifest_url}        │                          │
  │──────────────────────▶│                          │
  │                       │ GET /.well-known/...     │
  │                       │─────────────────────────▶│
  │                       │◀────────────────────────│
  │                       │   manifest JSON          │
  │                       │                          │
  │                       │ (validate manifest,      │
  │                       │  generate plugin_token,  │
  │                       │  mint LLM proxy token    │
  │                       │  if requires_llm_proxy)  │
  │                       │                          │
  │ {plugin_token,        │                          │
  │  llm_proxy_token,     │                          │
  │  install_id} ◀────────│                          │
  │                       │                          │
  │ (operator configures  │                          │
  │  plugin env vars)     │                          │
  │                       │                          │
  │                       │     POST /admin/install  │
  │                       │     {plugin_token, ...}  │
  │                       │◀─────────────────────────│
  │                       │   plugin acknowledges    │
  │                       │                          │
  │                       │  every 60s heartbeat     │
  │                       │◀─────────────────────────│
  │                       │                          │
  │ DELETE /admin/plugins/iosdist                    │
  │──────────────────────▶│                          │
  │                       │ revoke plugin_token,     │
  │                       │ cascade revoke           │
  │                       │ bound LLM proxy + PATs   │
  │                       │ (see auth-and-tokens.md) │
  │                       │                          │
  │                       │ next heartbeat → 401     │
  │                       │ plugin shuts down        │
```

Uninstall is cascade-driven: removing the `plugin_modules` row revokes all `*_tokens` rows where `bound_plugin_id = <name>`. Plugin's own DB is **not** deleted (operator's data, operator's choice; admin UI offers a "wipe plugin DB" follow-up button as separate destructive action).

## Security model

### iframe mode

- `sandbox` attribute MANDATORY (manifest declares; dock enforces; reject install if missing)
- Plugin UI server MUST set `Content-Security-Policy: frame-ancestors <dock-origin>` — dock checks this header during install validation
- postMessage origin checking on both sides; plugin filters by dock origin, dock filters by plugin origin
- Short-lived JWT (5 min) instead of long-lived PAT in iframe context
- Plugin cannot read dock cookies (different origin)

### tab mode

- Plugin's own session storage is its concern; dock not responsible
- Recommended: plugin trades the short-lived JWT for its own session, sets own cookie scoped to plugin origin
- Plugin's CSRF, session lifetime, logout flow = plugin's responsibility

### Defense in depth

- nginx config: `/internal/` location returns 403 to external clients (defense even if HMAC middleware fails)
- Plugin endpoints rate-limited per plugin_token (dock side)
- Audit log: every plugin → dock call is logged with plugin_name + endpoint + status

## First-party plugins follow same rules

Plugins owned by Polar (iosdist, video, wg-mac, hosts, projects, packtunnel, library, expense, latch, agent) use `display_mode=tab` by default.

**Why**:
- Dogfood the public API → we discover gaps before third parties do
- One loading mechanism in dock → less code, less bug surface
- "Plugin = equal citizen" signal both internally and externally
- Plugin UI improvements ship independently without rebuilding dock

**Exception**: dock-native admin pages (login, /admin-users, /admin-teams, /admin-plugins, /account/tokens, /system, /llm-billing) stay bundled in dock — they're not plugins, they're dock's core.

## Phasing

| Phase | Scope | Trigger |
|---|---|---|
| **P0** (this ADR) | Decisions locked. No code yet. | Approved |
| **P1** | `plugin_modules.config` expanded to manifest schema; manifest validator; install flow handles `requires_llm_proxy` auto-mint | After auth-and-tokens.md P1 lands |
| **P2** | `/api/v1/*` versioned namespace established; sidebar reads from plugin_modules; tab mode end-to-end with one first-party plugin (suggest: library — smallest) | After P1 |
| **P3** | OpenAPI spec generation; Go/TS SDK published to GitHub Packages; iframe mode + postMessage contract with one first-party plugin (suggest: iosdist) | After P2 |
| **P4** | "Sign in with Polar" OAuth IdP flow (dock as IdP for plugins); marketplace UI | After Phase 1-4 plugin extraction done |
| **P5** | Open source: API spec + SDK + dock source; plugin developer guide | After P4 + legal/license audit |

## Open questions (defer until P1 starts)

1. **OpenAPI generator**: go-swagger vs oapi-codegen vs hand-written. Likely oapi-codegen for cleaner Gin integration.
2. **JWT signing**: shared secret vs per-plugin RSA keypair. Likely shared secret for v1 (HS256), upgrade to per-plugin asymmetric in P3+ when marketplace ships.
3. **Manifest discovery**: only `.well-known/` URL, or also support operator-pasted JSON? Likely both — `.well-known/` for self-hosted plugins, paste for air-gapped installs.
4. **Sidebar i18n**: plugin manifest `display_name` is single string today. Should it be a map `{en: ..., zh: ...}`? Probably yes; cheap to add now.
5. **Plugin → plugin direct calls** (not via dock): explicitly out of scope. Force plugin-to-plugin through dock so audit and billing stay correct.

## DoD (this ADR collapses to "decisions documented")

- [x] All 5 design decisions recorded with rationale
- [x] Manifest schema sketched
- [x] display_mode contracts specified
- [x] Single-dock rationale enumerated
- [x] Lifecycle flow documented
- [x] Security model (iframe + tab) specified
- [x] Cross-reference to auth-and-tokens.md
- [x] Phasing tied to extraction roadmap
- [x] Open questions surfaced for P1 kickoff
