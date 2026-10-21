# Open API tokens (Cloudflare-style "apply for a token")

Status: design (2026-06-07). Lets external developers / scripts / partner
apps (e.g. share.lzhu.cn embedding the music 乐库) call Polar's public
`/api/*` surface with a long-lived, scoped **API token** instead of a
browser session cookie. Self-service create-a-token in the UI, copied once,
least-privilege scopes, revocable, rate-limited, audited — modelled on
Cloudflare API Tokens / Stripe restricted keys.

## Why now

Today the dock has four auth surfaces (see `CLAUDE.md` / `doc/arch/auth-and-tokens.md`):

| surface | auth | who |
|---|---|---|
| `/api/*` | Redis session cookie (Bearer access_token tolerated) | browser users |
| `/internal/v1/*` | HMAC plugin signature | plugin svcs (southbound) |
| `/api/proxy/v1/*` | `llm_proxy_tokens` (Bearer) | OpenAI-compatible LLM callers |
| `/ws/*` | inline | agents, chat |

There is **no general-purpose, user-issued token for the public API**.
External integrations have to scrape a session cookie (brittle, 30-min TTL,
full-account scope). The `llm_proxy_tokens` system already proves the whole
pattern (hashed secret + prefix, per-token RPM, monthly quota, `scopes`
column, `revoked_at`, `last_used_at`, per-call audit + Prometheus, admin UI
`/proxy-tokens.html`). **This design generalizes that one table into a
first-class open-API token** — LLM access becomes just one scope.

## Token model — `api_tokens` (mirrors `llm_proxy_tokens`)

```sql
CREATE TABLE IF NOT EXISTS api_tokens (
    id            BIGSERIAL PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES teams(id)  ON DELETE CASCADE,
    owner_user_id TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    name          TEXT NOT NULL,                 -- operator label
    prefix        TEXT NOT NULL,                 -- shown in UI: polar_live_AbCd…
    token_hash    TEXT NOT NULL UNIQUE,          -- sha256(secret); secret shown once
    scopes        JSONB NOT NULL DEFAULT '[]',   -- ["chat:read","posts:write",…]
    rate_limit_rpm INTEGER NOT NULL DEFAULT 120,
    ip_allowlist  JSONB,                         -- optional ["1.2.3.0/24",…]; null = any
    expires_at    TIMESTAMPTZ,                   -- optional hard expiry
    status        TEXT NOT NULL DEFAULT 'active',-- active | revoked
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_api_tokens_ws    ON api_tokens(workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_api_tokens_hash  ON api_tokens(token_hash) WHERE status = 'active';

-- per-request audit (mirrors llm_proxy_calls); UI aggregates per-token usage
CREATE TABLE IF NOT EXISTS api_token_calls (
    id           BIGSERIAL PRIMARY KEY,
    token_id     BIGINT NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    method       TEXT NOT NULL,
    path         TEXT NOT NULL,
    status_code  INT  NOT NULL,
    scope        TEXT NOT NULL DEFAULT '',  -- the scope the route required
    ip           TEXT NOT NULL DEFAULT '',
    latency_ms   INT  NOT NULL DEFAULT 0,
    at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_api_token_calls_tok ON api_token_calls(token_id, at DESC);
```

**Secret format:** `polar_<env>_<22+ base62>` (e.g. `polar_live_…`, `polar_test_…`)
— self-identifying like `sk_live_`/Cloudflare `v1.0-`. Stored only as
`sha256`; the plaintext is returned **once** at create and never again.
`prefix` = `polar_<env>_` + first 6 chars, for safe display in lists.

## Scopes — derived from the API catalog

Polar already generates `internal/app/dock/api_catalog.gen.json` (297
endpoints, each with method/path/auth) via `cmd/api-catalog-gen` go/ast
analysis. Reuse it as the scope source of truth:

- Add a `scope` annotation per route (a small static map keyed by
  method+path-prefix, or a `// @scope chat:write` comment the generator
  reads). Each endpoint resolves to exactly one scope.
- Scopes are coarse, `domain:action`: `chat:read|write`, `posts:read|write`,
  `assets:read|write`, `music:read|write`, `hosts:read`, `wg:read`,
  `llm:invoke` (the old proxy), `tokens:manage` (CRUD api_tokens — dangerous,
  default-off), `*` (full, discouraged).
- **admin scopes** (e.g. anything behind `AdminMiddleware`/site-admin) are
  only grantable by a site-admin and never via `*`.
- **Default deny:** a route with no granted scope → 403. A token with `[]`
  scopes can call nothing.

The catalog already knows each route's auth tier, so the generator can also
emit a `scope` column into `catalog.json`, which doubles as the public **API
reference** ("this endpoint needs `posts:write`").

### Module scopes (2026-10-21, POLAROPERA-48)

Besides dock's own `domain:action` scopes, a token may carry scopes declared by a plugin module's
manifest (`opera:sessions:read`, three-part, see `module-manifest.md`) and `<module>:*`. Create-time
validation (`knownScope`) accepts them only once the module has registered its manifest.
`GET /api/api-token-scopes` adds a `modules` array (per module: `display_name`, `groups[{group, scopes[{id,
name, sensitive}]}]`, `presets[{id, name, scopes}]`) next to the unchanged `groups` / `wildcard`.

dock does not enforce module scopes on its own `/api` (they never match a dock route, so a
module-only token gets 403 there). The module enforces them: `/internal/v1/auth/verify` returns
`kind` / `token_id` / `scopes` — the token's scopes filtered to `<calling module>:` + `*`, `null` for an
unscoped token (`internal-api-v1.md` §1).

## Auth middleware — `apiTokenAuth`

Sits alongside the session middleware on `/api/*`. A request whose
`Authorization: Bearer` value starts with `polar_` takes the token path;
everything else stays on the cookie/session path (zero behavior change for
the browser).

1. `sha256(secret)` → lookup `api_tokens WHERE token_hash=$1 AND status='active'`.
   Miss / revoked / `expires_at < now()` → **401**.
2. `ip_allowlist` set and `RemoteIP` not in it → **403**.
3. Rate limit: per-token RPM via Redis (reuse the `llm_proxy` limiter). Over →
   **429** with `Retry-After`.
4. Required scope for this route (from the catalog scope-map); token lacks it
   → **403**.
5. Bind context: `workspace_id = token.workspace_id` (token is workspace-pinned;
   `X-Workspace-Id` is ignored for tokens — no cross-workspace surprise),
   `user_id = owner`, `auth_kind = "api_token"`.
6. Async: bump `last_used_at` (throttled: at most once per token per 60 s, `touchAPITokenThrottled`), insert `api_token_calls` row, Prometheus counters.

Handlers don't change — they already read `workspace_id`/`user_id` from
context, exactly like the session path sets them.

## Self-service UI — `/api-tokens.html`

Reuse the `/proxy-tokens.html` patterns (list + create modal + audit drawer):

- **List**: name, `prefix…`, scopes (chips), workspace, last used, expiry,
  status; revoked hidden by default with a toggle (we already do this for
  proxy tokens).
- **Create wizard**: name → scope tree (checkboxes grouped by domain, read/write
  per group) → workspace picker → optional expiry / RPM / IP allowlist →
  **Create** → show secret once + copy button + ready-to-paste `curl`.
- **Revoke** (soft, `status=revoked`) + **Rotate** (revoke + mint new, same
  scopes).
- **Edit** (2026-10-21, POLAROPERA-48) — `PATCH /api/api-tokens/:id`, body any
  subset of `{name, scopes[], ip_allowlist[], rate_limit_rpm, expires_at}`;
  absent = unchanged, `"expires_at": null` = never expires (an RFC3339 value must
  be in the future), `rate_limit_rpm` 1..100000. The secret, hash and prefix
  are untouched, so the machines already holding the token don't need it
  re-pasted. Returns `200 {token}` (no secret). Errors: 400 `input.invalid` /
  `api.unknown_scope` / `api.invalid_ip`; 403 `api.session_required`,
  `api.admin_scope_forbidden`, `forbidden` (not the token owner nor a workspace
  owner/admin); 404 `not_found` (missing, revoked, other workspace). The UI
  pre-fills the create modal and keeps scopes it no longer recognises: on
  edit, a scope the token **already holds** may stay even if the catalog no
  longer knows it (e.g. a module dropped it); only newly added scopes must be
  known (`api.unknown_scope`) and pass the admin-scope gate.
- **Per-token usage**: aggregate `api_token_calls` (calls, top paths, error
  rate) — same modal UX as the proxy audit view.

## Relationship to existing tokens

- `llm_proxy_tokens` stays for **OpenAI wire-compat** (`/api/proxy/v1/*`), but
  the UI unifies: an api_token with the `llm:invoke` scope is the general way
  in; the proxy path keeps its own table for the OpenAI-specific bits
  (alias routes, cost ledger, `default_llm_config_id`). No migration forced.
- HMAC plugin tokens are unchanged — that's the **southbound/internal** trust
  boundary; api_tokens are the **northbound/public** one.

## Rollout phases

- **P0** — `api_tokens` table + create/list/revoke store+handlers +
  `apiTokenAuth` middleware accepting `polar_` Bearer (full-access only to
  start) + minimal `/api-tokens.html`. Flag-gated (`POLAR_OPEN_API_TOKENS`).
- **P1** — scope map in `cmd/api-catalog-gen` (emit `scope` per route) +
  enforcement in the middleware + scope picker in the create wizard.
- **P2** — per-token RPM + `api_token_calls` audit + Prometheus + usage view
  (lift the proxy infra).
- **P3** — expiry, IP allowlist, rotation.
- **P4** — public **API reference** page from `catalog.json` (scope shown per
  endpoint) + **OpenAPI 3 export** from the catalog → feeds the
  polar-sdk-swift/kotlin codegen track.
- **P5** — in-place edit (`PATCH /api/api-tokens/:id`) + tightened issuance:
  only browser sessions may create / edit tokens, admin scopes need a site
  admin, edit is limited to the owner or a workspace owner/admin, and
  create / update / rotate / revoke write team-audit rows.

## Security notes

- Hash at rest, prefix-only display, show-once secret.
- Default-deny scopes; admin scopes site-admin-grant-only; never in `*`.
- Audit every call; revoke is immediate (active-hash partial index).
- nginx already serves public `/api/*`; `/internal/*` stays blocked there.
- Tokens are workspace-pinned → blast radius = one workspace.
- Editing a token evicts its `verify:api:{sha256}` cache entry (the entry holds
  the whole row), so new scopes / IP allowlist / RPM / expiry apply on the next
  request — on dock's `/api` and on `/internal/v1/auth/verify` — not after the
  positive TTL.
- A `polar_` token cannot create or edit tokens (`auth_kind == "api_token"` →
  403 `api.session_required`), even one holding `*` or `api-tokens:write` — a
  token must never be able to widen itself. (Rotate / revoke by token are
  unchanged.)
- `admin` / `admin:*` scopes can only be granted by a site admin (`IsSiteAdmin`),
  on create and on edit (edit checks only the scopes being newly added) →
  otherwise 403 `api.admin_scope_forbidden`.
- Team audit (`category=team_audit`, subject `api_token`): `api_token.create`,
  `api_token.update` (details `before` / `after` / `changed_fields`),
  `api_token.rotate`, `api_token.revoke`. Details carry name, scopes, IP
  allowlist, RPM, expiry (and prefix on create / rotate) — never the hash or
  secret.

## Open questions

- Personal (user-owned) vs workspace-shared tokens — start workspace-pinned +
  owner-recorded; revisit org-level "account tokens" later.
- Scope granularity: coarse `domain:action` first; per-resource (Cloudflare
  "resources") only if a real need appears.
- Whether to fold `llm_proxy_tokens` fully into `api_tokens` post-P4 (keep the
  proxy-specific columns as a side table keyed by token_id).
