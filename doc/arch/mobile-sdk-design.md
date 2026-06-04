# Mobile SDK design — iOS (Swift) & Android (Kotlin)

Status: **design / not yet built.** This is the plan for native client SDKs that
let an app talk to the Polar dock API the way `polar-sdk` (Go) lets a plugin talk
to it. Companion to [`../api/README.md`](../api/README.md) and the machine-readable
[`../api/catalog.json`](../api/catalog.json).

## Goals

1. **Typed, idiomatic** access to the dock API — `await polar.teams.list()`, not
   hand-rolled `URLRequest`s.
2. **Auth handled once**: login, automatic access-token refresh on `401`,
   re-login signal. The app never touches a JWT.
3. **Workspace-aware**: a single "active workspace" the SDK injects as
   `X-Workspace-Id` on every call; switching is one call.
4. **RBAC-aware UI**: expose the caller's effective permissions so the app can
   hide affordances it can't use (server still enforces).
5. **Generated, not hand-maintained**: the request/response layer is
   code-generated from `catalog.json`, so the 297-endpoint surface stays in sync
   with the backend instead of drifting.

Non-goals (v1): offline write/sync, local DB caching, push-notification routing
(the dock already takes `X-Push-Token`; wiring APNs/FCM delivery is a later phase).

## Repository & packaging

Two new repos under `networkextension/` (mirrors the per-module split):

- **`polar-sdk-swift`** — Swift Package (SPM) + CocoaPods podspec. iOS 15+ /
  macOS 12+. Zero third-party deps (URLSession, async/await, Codable).
- **`polar-sdk-kotlin`** — Kotlin Multiplatform-ready but ship Android-first
  (`com.polar.sdk`), published to Maven. Ktor client + kotlinx.serialization, or
  OkHttp + Moshi (decide below). minSdk 24.

A shared **`polar-api-spec`** (the catalog.json + a small normalization layer)
is the single source both generators consume — see *Codegen* below.

## Architecture (same shape on both platforms)

```
 ┌─────────────────────────────────────────────┐
 │ PolarClient(baseURL, config)                 │  ← entry point
 │   .auth      AuthSession                      │  login/refresh/logout, token store
 │   .workspace WorkspaceContext                 │  active id + switch + list
 │   .me        MeService → permissions cache    │  RBAC gating
 │   .teams / .llm / .rooms / .assets / …        │  generated resource services
 └───────────────┬─────────────────────────────┘
                 │ every call →
        ┌────────▼─────────┐
        │ Transport         │  injects auth + X-Workspace-Id;
        │  (URLSession /     │  401 → serialize one refresh → retry once;
        │   OkHttp|Ktor)     │  decodes {error} → typed PolarError
        └───────────────────┘
```

### Transport responsibilities (the hand-written core, ~1 file)

- Build requests: base URL + path + query; JSON or multipart body; inject
  `Authorization`/cookie + `X-Workspace-Id` + device headers.
- **401 interceptor**: on `401`, serialize a single `/api/token/refresh` (a
  mutex/actor so concurrent calls don't stampede), then retry the original once;
  if refresh fails, emit `.sessionExpired` and surface `PolarError.unauthenticated`.
- Map `{ "error": ... }` + status → `PolarError` (`.badRequest`, `.forbidden`,
  `.notFound`, `.conflict`, `.outOfCredit(402)`, `.server`, …).
- Streaming: SSE reader for `/api/proxy/v1/chat/completions` (and any
  `text/event-stream`) → `AsyncThrowingStream<Chunk>` (Swift) / `Flow<Chunk>`
  (Kotlin).

### Auth & token storage — the key decision

The dock's `login`/`refresh` put tokens in **httpOnly cookies** (not the JSON
body) but `AuthMiddleware` also accepts `Authorization: Bearer`. Two viable v1
strategies:

- **A. Cookie-jar (recommended for v1).** Let `URLSession` /
  OkHttp's `CookieJar` persist the `access_token`/`refresh_token` cookies; the
  SDK drives `login`/`refresh`/`logout` and reads `access_expires_in` to schedule
  a proactive refresh (<1h remaining). Pros: zero backend change, works today.
  Cons: cookie storage on iOS is process-global (`HTTPCookieStorage.shared`) —
  use an **ephemeral/owned** session config + an injected cookie store so two
  `PolarClient`s (or the host app) don't collide; persist the cookies to the
  Keychain / EncryptedSharedPreferences ourselves for app-relaunch.
- **B. Body-token Bearer (cleaner, needs a tiny backend add).** Add a
  `X-Token-Response: 1` opt-in (or `/api/mobile/login`) that ALSO returns
  `{ access_token, refresh_token, access_expires_in }` in the body. SDK stores
  them in the Keychain/EncryptedSharedPreferences and sends `Bearer`. No cookie
  semantics to fight. **Recommended target state**; tracked as the first backend
  task for the SDK.

Decision: **build the SDK against B's interface** (`TokenStore` abstraction +
`Bearer` transport), and land the small dock change to return tokens in-body for
mobile; keep A as a fallback adapter. Token store is pluggable
(`Keychain`/`EncryptedSharedPreferences` default, in-memory for tests).

### WorkspaceContext

`workspace.setActive(id)` persists the id (UserDefaults / DataStore) and the
transport reads it for `X-Workspace-Id`. `workspace.list()` = `GET /api/teams`.
On `setActive`, invalidate the permission cache (below).

### RBAC gating (`me.can`)

`me.refreshPermissions()` calls `GET /api/me/permissions` and caches
`Set<String>` per workspace + the `site_admin` bool. The app gates UI with
`polar.me.can("llm:admin")` / `polar.me.isSiteAdmin`. Re-fetched on login,
workspace switch, and a manual refresh. Mirrors the dock-ui pattern.

## Codegen from `catalog.json`

A generator (Node or Go script in `polar-api-spec`) reads `catalog.json` and emits
per-family service files:

- Group endpoints by path prefix → a service (`TeamsService`, `LLMService`, …).
- For each endpoint emit a typed method: name from method+path, params from
  `path_params` + `query`, a `Codable`/`@Serializable` request struct from `body`,
  return `Data`/`JSONValue` where the response isn't yet modeled (responses aren't
  in the catalog — see *Gaps*).
- Mark `auth:"admin"` methods (doc comment / `@SiteAdmin`), skip `plugin-hmac`.

This keeps ~90% of the surface generated. Hand-written: transport, auth,
workspace, the SSE chat stream, and **response models** for the ~20 endpoints an
app actually renders (teams, me, llm-configs, rooms, posts, assets) — written by
hand for ergonomics and evolved as needed.

### Gaps to close in the spec (so codegen is fuller)

`catalog.json` today captures **requests**, not **responses**, and params read
indirectly (helpers/loops) are missed. To improve mobile DX:
1. Extend `api-catalog-gen` to also recover the dominant response shape
   (`c.JSON(... gin.H{...})` / named struct) — even best-effort response field
   names materially help.
2. Add a hand-maintained `doc/api/overrides.json` for the ~20 app-critical
   endpoints (exact response models, pagination style) that the generator merges.

## Phasing

- **M0 — spec & scaffold.** Land the body-token login on the dock; extend
  `api-catalog-gen` for responses; scaffold both repos + the codegen pipeline.
- **M1 — Swift core.** Transport + AuthSession + WorkspaceContext + MeService +
  generated services; ship `teams/me/llm-configs/rooms` typed responses; SSE chat.
  Example iOS app: login → pick workspace → list rooms → chat.
- **M2 — Kotlin core.** Same surface, Android example app. (Decide Ktor vs OkHttp
  at M2 start — lean **Ktor + kotlinx.serialization** for KMP-readiness and to
  reuse the SSE/coroutines model; OkHttp if we want the most battle-tested HTTP.)
- **M3 — push + uploads.** APNs/FCM token registration end-to-end; multipart
  asset/document upload with progress; image helpers.
- **M4 — polish.** Retry/backoff config, request logging hooks, offline-read
  cache (optional), publish to SPM/CocoaPods/Maven, versioning tied to dock API.

## Versioning & compatibility

- SDK semver; a `PolarClient` advertises a `minDockApi`. The dock is additive
  (new fields/endpoints don't break old clients); the SDK tolerates unknown JSON
  fields (non-strict decoding).
- Pin the `catalog.json` snapshot the SDK was generated against in the repo; CI
  regenerates and flags drift.

## Open decisions (pick before M0)

1. **Token strategy** — confirm B (body-token Bearer) + land the dock change. *(recommended)*
2. **Kotlin HTTP stack** — Ktor (KMP path) vs OkHttp. *(lean Ktor)*
3. **Codegen host language** — Go (reuse `api-catalog-gen` AST + same repo) vs a
   Node script in `polar-api-spec`. *(lean Go, one toolchain)*
4. **Response modeling depth** — generate best-effort from an extended catalog,
   or hand-write all app-facing responses. *(hybrid: generate + overrides.json)*
