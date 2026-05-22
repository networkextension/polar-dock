# ADR: Auth and Tokens — Identity Sources, SSO, and the Three-Token Model

**Status**: Accepted, 2026-05-22
**Related**: [open-platform.md](./open-platform.md), [decision.md](./decision.md), [internal-api-v1.md](./internal-api-v1.md)

## Context

Open-source positioning (locked in [open-platform.md](./open-platform.md)) puts dock in the role of **identity + LLM proxy + collaboration core**. Plugins never do their own auth UI; they receive user identity from dock. This ADR specifies:

1. What identity sources dock accepts (local + SSO triad)
2. What token types dock issues and what each is for
3. How control granularity stacks (team / user / per-token)
4. How plugins act on behalf of users
5. Cascading revocation on plugin uninstall

The auth model is the single most important interface dock exposes to the world. Get it right at v1; refactoring it post-open-source is painful.

## Decision summary

| # | Decision |
|---|---|
| **1** | Identity sources: local password + Google OAuth + Microsoft Azure (Office 365, multi-tenant) + Sign in with Apple. Local password admin-disable-able for "SSO-only dock." |
| **2** | Three token types: `polar_proxy_*` (LLM workhorse), `polar_pat_*` (user general API), `polar_plugin_*` (HMAC service-to-service). `polar_svc_*` (team service account) **not in v1** — `polar_proxy_*` is the workhorse for plugin LLM calls. |
| **3** | PAT default expiry: 90 days. Email reminder 7 days before. "Never expires" requires admin enable. |
| **4** | Both `polar_proxy_*` and `polar_pat_*` carry optional `bound_plugin_id`. Plugin uninstall → cascade revoke. |
| **5** | 2FA on top of SSO: supported (TOTP + passkey), default off, user opt-in. |
| **6** | Plugin acts on user behalf via PAT (v1) → OAuth "Sign in with Polar" (Phase 4+). |
| **7** | Plugin impersonation (HMAC + `X-Polar-As-User`) gated by `plugin_modules.config.allow_impersonation`, default off. Reserved for admin-installed trusted plugins. |

## Identity sources

### Source matrix

| Source | Provider | Use case | Status |
|---|---|---|---|
| `local` | dock itself | Bootstrap admin, legacy users, air-gapped deploys | Existing |
| `google` | accounts.google.com | Consumer (Gmail / Workspace) | New |
| `azure` | login.microsoftonline.com (multi-tenant) | Enterprise (Office 365, corporate Windows) | New |
| `apple` | appleid.apple.com | iOS users (App Store compliance) | New |

**SSO triad rationale**: Google covers consumer, Azure covers enterprise (~80% of corporate users have Office 365), Apple covers iOS compliance + ecosystem. Beyond these three, marginal coverage gain is small.

### Schema

```sql
CREATE TABLE user_external_identities (
    provider             TEXT NOT NULL,        -- 'local'|'google'|'azure'|'apple'
    provider_user_id     TEXT NOT NULL,        -- OIDC 'sub' claim
    user_id              TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider_email       TEXT,                 -- for account-linking lookup
    provider_tenant_id   TEXT,                 -- azure tenant id; null for others
    raw_claims           JSONB NOT NULL DEFAULT '{}'::jsonb,
    linked_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at        TIMESTAMPTZ,
    PRIMARY KEY (provider, provider_user_id)
);
CREATE INDEX ix_user_ext_user_id ON user_external_identities(user_id);
CREATE INDEX ix_user_ext_provider_email ON user_external_identities(provider, provider_email);
```

`users` table gains:

```sql
ALTER TABLE users
  ADD COLUMN IF NOT EXISTS password_hash    TEXT,   -- nullable; null = SSO-only user
  ADD COLUMN IF NOT EXISTS password_disabled BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS two_factor_enabled BOOLEAN NOT NULL DEFAULT FALSE;
```

A dock-wide setting `auth.local_password_enabled` (false = "SSO-only dock"; admin sets via `/admin-auth-config.html`).

### OIDC flows

All three SSO providers use **OIDC authorization code flow** with PKCE:

```
1. user clicks "Sign in with <provider>" on /login.html
2. dock generates state + PKCE verifier, stores in short-lived (5min) cookie
3. dock redirects to <provider>/authorize?client_id=...&redirect_uri=https://<dock>/auth/<provider>/callback&...
4. user authenticates at provider, consents
5. provider redirects back to /auth/<provider>/callback?code=...&state=...
6. dock verifies state, exchanges code for id_token + access_token at <provider>/token
7. dock validates id_token (signature, aud, exp, nonce)
8. lookup user_external_identities WHERE (provider, provider_user_id) = (provider, id_token.sub)
   - found → sign user in, set session cookie
   - not found, but email matches existing user → "link account?" page
   - not found, no email match → just-in-time create user + external identity row
9. set session cookie, redirect to original destination
```

### Provider-specific notes

**Google**:
- OIDC discovery: `https://accounts.google.com/.well-known/openid-configuration`
- Scopes: `openid email profile`
- Tenant: N/A (no tenant concept)
- Setup cost: free Google Cloud Console project, OAuth consent screen verification

**Azure (Office 365, multi-tenant)**:
- OIDC discovery: `https://login.microsoftonline.com/common/v2.0/.well-known/openid-configuration`
- `iss` validation: pattern match `https://login.microsoftonline.com/<tenant_id>/v2.0`
- Scopes: `openid email profile User.Read`
- Store `tid` claim as `provider_tenant_id` for audit
- Setup cost: free Azure AD app registration, multi-tenant flag enabled
- Admin can optionally restrict to specific tenants via `auth.azure_allowed_tenants` config

**Apple**:
- OIDC discovery: `https://appleid.apple.com/.well-known/openid-configuration`
- Auth method: client_secret JWT signed with Apple-issued private key
- Tricky: Apple returns email **only on first sign-in**; subsequent sign-ins lack email → must persist on first
- Webhook for revocation: `POST /auth/apple/revocation` (Apple notifies when user revokes consent)
- Setup cost: Apple Developer Program enrollment ($99/year, already paid for iOS), Sign in with Apple capability, key+keyID configured

### Just-in-time user provisioning

First SSO login with no matching `user_external_identities` and no email match:

```
1. INSERT users (id=new, email=<from claims>, name=<from claims>, password_hash=NULL,
                  default_workspace_id=<newly created personal workspace>)
2. INSERT user_external_identities (provider, provider_user_id, user_id=new.id, ...)
3. session.create(user_id=new.id)
4. redirect to /welcome.html for profile setup
```

### Account linking

User already exists (matched by email) but lacks the SSO link → consent page:

> You already have a Polar account with email **alice@example.com**.
> Sign in with your existing password to link your Google account, or proceed
> as a new user with a different email.
>
> [ Sign in with password and link ] [ Use different email ]

Rationale: prevent silent takeover where an attacker controls a Google account using same email as an existing local user.

### 2FA on top of SSO

- User opt-in via `/account/security.html`
- Methods: TOTP (RFC 6238, any authenticator app) + passkey (WebAuthn)
- After SSO succeeds, if `users.two_factor_enabled` → second factor prompt before session created
- Backup codes (10 single-use) generated at enrollment
- Admin can require 2FA dock-wide via `auth.require_2fa` config

## Token model

### Three types

| Prefix | Identity | Allowed endpoints | Billing/limits | Who can issue |
|---|---|---|---|---|
| `polar_proxy_*` | User OR plugin-bound | `/api/proxy/v1/chat/completions` only | RPM + monthly USD cap + model alias map | User self-serve up to team default; admin above |
| `polar_pat_*` | User | `/api/v1/*` subject to scopes | RPM + monthly request count | User self-serve; `admin:*` scopes blocked from PAT |
| `polar_plugin_*` (HMAC) | Plugin (not a user) | `/internal/v1/*` | Per `plugin_modules.config` rate limit | Platform admin only via `/admin-plugins.html` |

### Why segmented (not unified)

Different operational profiles:

- `polar_proxy_*` needs USD cap + cost ledger + model alias. PAT doesn't.
- `polar_plugin_*` HMAC needs replay window + signature verification. User tokens use bearer.
- `polar_proxy_*` and PAT can be self-served; HMAC is admin-issued only.
- Audit log shapes differ: proxy logs token counts + USD cost; PAT logs endpoint + status; HMAC logs plugin_name + endpoint.

A unified table would have half its columns NULL for any given token type. Segmentation aligns columns with semantics.

### Common token schema

Each type has its own table (existing `llm_proxy_tokens`, new `pat_tokens`, new `plugin_tokens`). Shared shape:

```sql
-- Example: pat_tokens (new in this ADR)
CREATE TABLE pat_tokens (
    id              TEXT PRIMARY KEY,                 -- 'pat_<random>'
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,                    -- user-supplied label
    purpose         TEXT,                             -- free-form description; shown in audit
    token_hash      TEXT NOT NULL,                    -- sha256 of plaintext
    token_prefix    TEXT NOT NULL,                    -- 'polar_pat_' + first 4 chars of random
    scopes          TEXT[] NOT NULL DEFAULT '{}',     -- ['read','write']; never 'admin' via PAT
    rate_limit_rpm  INT,
    monthly_quota   INT,
    bound_plugin_id TEXT REFERENCES plugin_modules(name) ON DELETE CASCADE, -- cascade revoke
    expires_at      TIMESTAMPTZ NOT NULL,             -- default now() + 90 days
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at    TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ
);
CREATE INDEX ix_pat_user ON pat_tokens(user_id) WHERE revoked_at IS NULL;
CREATE INDEX ix_pat_plugin ON pat_tokens(bound_plugin_id) WHERE bound_plugin_id IS NOT NULL;
CREATE INDEX ix_pat_hash ON pat_tokens(token_hash);

-- Add bound_plugin_id to existing llm_proxy_tokens
ALTER TABLE llm_proxy_tokens
  ADD COLUMN IF NOT EXISTS bound_plugin_id TEXT REFERENCES plugin_modules(name) ON DELETE CASCADE,
  ADD COLUMN IF NOT EXISTS purpose TEXT;
CREATE INDEX IF NOT EXISTS ix_llm_proxy_plugin ON llm_proxy_tokens(bound_plugin_id) WHERE bound_plugin_id IS NOT NULL;
```

### Plaintext format

- Prefix per type: `polar_proxy_` / `polar_pat_` / `polar_plugin_`
- 32 hex chars (16 random bytes) after prefix
- Total length: 13-prefix + 32 = ~45 chars
- Plaintext shown to user **exactly once** at creation; hash stored

### 90-day expiry policy (PAT)

- Default `expires_at = now() + interval '90 days'`
- User can choose 7d / 30d / 90d at creation; cannot exceed 90d unless `users.can_extend_pat = true` (admin grant)
- Email reminders: 14d, 7d, 1d before expiry; user can self-serve `POST /api/v1/account/tokens/:id/rotate` → new plaintext + 7d grace where old token still works
- Expired tokens return 401 with `WWW-Authenticate: Bearer error="invalid_token", error_description="token expired"`

### LLM proxy token (existing, extended)

Already has: rate limit, monthly USD cap, model alias, audit, cost ledger.
This ADR adds:

- `bound_plugin_id` for cascade revoke
- `purpose` field shown in audit log
- `POST /api/proxy/v1/tokens/:id/rotate` for self-service rotation with 7d grace

### Plugin HMAC (existing, unchanged)

Already specified in [decision.md](./decision.md) Phase 1-A. Canonical signing:
`METHOD\nPATH\nTS\nSHA256_HEX(BODY)`, ±300s replay window.

## Three-level control granularity

### Team level (coarse)

- LLM monthly budget pool (already exists, `team_llm_quota`); any team member's `polar_proxy_*` tokens draw from this pool
- Team admin can revoke any token created by team members
- Team admin can restrict scope whitelist for member-created PATs (e.g., "members in team marketing cannot mint `write:billing`")

### User level (mid)

- User self-serve mint/rotate/revoke their own tokens (proxy + PAT)
- User-level default RPM (anti-abuse floor)
- On user deactivation → cascade revoke all user's tokens

### Per-token level (fine)

- Each token: rate limit, monthly quota, expiry, scopes, `purpose` label
- Each token: separate audit trail
- Each token: optional `bound_plugin_id`

## Approval rubric

| Action | Who | Mechanism |
|---|---|---|
| Mint `polar_proxy_*` cap ≤ team default | User self-serve | UI button |
| Mint `polar_proxy_*` cap > team default | Admin approval | "Request → admin approves in `/admin-tokens.html`" |
| Mint `polar_pat_*` scope=`read` | User self-serve | UI button |
| Mint `polar_pat_*` scope=`write` | User self-serve, audit-highlighted | UI button + audit row tagged `high_privilege:true` |
| Mint `polar_pat_*` scope=`admin:*` | **Blocked** | UI hides scope; API returns 403 |
| Mint `polar_plugin_*` HMAC | Platform admin | `/admin-plugins.html` |
| Extend PAT past 90d | Admin grant per user | `users.can_extend_pat = true` |
| Enable "Never expires" on a token | Admin enable on user | Same flag |
| Disable local password globally | Platform admin | `auth.local_password_enabled = false` |

Approval flow is **not** a ticket system. It's: user submits request → dock notifies admin via webhook (Slack/email) → admin clicks approve in `/admin-tokens.html` → token activates. Async, low-ceremony.

## Plugin acting on user's behalf

Three modes. v1 defaults to mode 1; mode 2 ships in Phase 4+; mode 3 reserved for trusted plugins only.

### Mode 1: PAT forwarding (default, v1)

```
1. User logs into dock, mints PAT at /account/tokens
2. User pastes PAT into plugin's configuration UI
3. Plugin stores PAT (encrypted at rest is plugin's responsibility)
4. Plugin calls dock /api/v1/* with Authorization: Bearer <PAT>
5. Dock authenticates as user; audit shows `principal=user_id via plugin=<plugin_name>`
   (plugin name detected via plugin_token in a separate header X-Polar-Via-Plugin)
```

Pros: simple, no consent flow. Cons: PAT lives in plugin's storage; user trusts plugin to handle securely.

### Mode 2: OAuth "Sign in with Polar" (Phase 4+)

Dock acts as OIDC provider for plugins:

```
1. User clicks "Sign in with Polar" on plugin
2. Plugin redirects to dock /oauth/authorize?client_id=<plugin>&scope=read:projects&...
3. Dock shows consent screen: "Plugin iosdist requests read access to your projects"
4. User approves
5. Dock redirects back with code; plugin exchanges for access_token + refresh_token
6. Plugin calls dock with Bearer <oauth_token>
```

Pros: scoped consent, revokable by user without re-mint, refresh tokens. Cons: more code, OAuth standard subtleties (PKCE, refresh rotation, token introspection).

Schema sketch (deferred):

```sql
CREATE TABLE oauth_clients (
    client_id    TEXT PRIMARY KEY,
    plugin_id    TEXT NOT NULL REFERENCES plugin_modules(name) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    redirect_uris TEXT[] NOT NULL,
    allowed_scopes TEXT[] NOT NULL,
    client_secret_hash TEXT NOT NULL
);

CREATE TABLE oauth_grants (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_id       TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    scopes          TEXT[] NOT NULL,
    access_token_hash TEXT NOT NULL,
    refresh_token_hash TEXT NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    refresh_expires_at TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ
);
```

### Mode 3: Plugin impersonation (trusted plugins only)

```
1. Plugin's manifest has permissions.allow_impersonation = true
2. Platform admin approves at install time (UI: "This plugin will be able to act as any user. Approve?")
3. plugin_modules.config stores allow_impersonation: true
4. Plugin calls /api/v1/* with HMAC signature + X-Polar-As-User: <user_id>
5. Dock validates HMAC, checks allow_impersonation, sets ctx.user_id = X-Polar-As-User header
6. Audit shows `principal=user_id impersonated_by=<plugin_name>`
```

Use case: internal admin plugins that need to make calls on user's behalf without user interaction (e.g., scheduled report generator). **Never** for third-party plugins. UI shows scary red warning at install time.

## Cascading revocation

```
admin → DELETE /api/admin/plugins/iosdist
              │
              ▼
plugin_modules row deleted (ON DELETE CASCADE on FK)
              │
              ├─→ pat_tokens WHERE bound_plugin_id='iosdist'      → revoked_at = now()
              ├─→ llm_proxy_tokens WHERE bound_plugin_id='iosdist' → revoked_at = now()
              ├─→ oauth_grants WHERE client_id IN (oauth_clients WHERE plugin_id='iosdist')
              └─→ plugin_tokens (the HMAC one)                     → row deleted
              │
              ▼
WebSocket broadcast "plugin_uninstalled" → connected plugin instances log out
              │
              ▼
next plugin → dock call returns 401, plugin shuts down gracefully
```

Plugin's own DB is **not** wiped. Separate destructive action via admin UI.

## Audit log

Single `auth_audit_log` table for all token-acting operations:

```sql
CREATE TABLE auth_audit_log (
    id            BIGSERIAL PRIMARY KEY,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_type    TEXT NOT NULL,        -- 'login'|'logout'|'token_mint'|'token_use'|'token_revoke'|'impersonate'|'sso_link'
    actor_user_id TEXT,                 -- nullable for HMAC-only
    actor_token_id TEXT,                -- which token authenticated
    actor_plugin  TEXT,                 -- if via plugin
    target_user_id TEXT,                -- e.g., admin acted on another user
    ip_address    INET,
    user_agent    TEXT,
    endpoint      TEXT,
    status_code   INT,
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX ix_audit_actor_time ON auth_audit_log(actor_user_id, occurred_at DESC);
CREATE INDEX ix_audit_event_time ON auth_audit_log(event_type, occurred_at DESC);
CREATE INDEX ix_audit_plugin_time ON auth_audit_log(actor_plugin, occurred_at DESC) WHERE actor_plugin IS NOT NULL;
```

Retention: 90 days hot in Postgres + archive to long-term storage on day 91. Admin search UI at `/admin-audit.html`.

## Endpoint inventory

New / extended endpoints this ADR introduces:

```
POST   /auth/google/start            # OAuth start (each provider has start+callback)
GET    /auth/google/callback
POST   /auth/azure/start
GET    /auth/azure/callback
POST   /auth/apple/start
GET    /auth/apple/callback
POST   /auth/apple/revocation        # Apple webhook

POST   /api/v1/account/link/<provider>   # link additional provider to current user
DELETE /api/v1/account/link/<provider>   # unlink

GET    /api/v1/account/tokens             # list user's PATs + proxy tokens (unified view)
POST   /api/v1/account/tokens             # mint new PAT
DELETE /api/v1/account/tokens/:id         # revoke
POST   /api/v1/account/tokens/:id/rotate  # rotate with 7d grace

GET    /api/v1/account/2fa                # enrollment status
POST   /api/v1/account/2fa/enroll         # start TOTP enrollment
POST   /api/v1/account/2fa/verify         # verify TOTP/passkey

POST   /admin/tokens/:id/approve          # admin approves token requests
POST   /admin/users/:id/can-extend-pat    # toggle extend-pat flag
POST   /admin/auth/config                  # toggle local_password_enabled, require_2fa, etc.
POST   /internal/v1/jwt/verify             # plugin backend verifies short-lived JWT from iframe/tab handoff
```

## Phasing

| Phase | Scope | Trigger |
|---|---|---|
| **P0** (this ADR) | Decisions locked | Approved |
| **P1** | `pat_tokens` table; PAT mint/list/revoke/rotate endpoints; `/account/tokens` UI; `bound_plugin_id` cascade on `llm_proxy_tokens` and new `pat_tokens`; PAT-aware audit log | After ADR approved |
| **P2** | Google OAuth: discovery, callback, account-link UI, JIT provisioning | After P1 |
| **P3** | Azure (multi-tenant) + Apple Sign in; admin auth-config UI; `local_password_enabled` toggle | After P2 |
| **P4** | 2FA (TOTP + passkey) | After P3, parallel-OK |
| **P5** | OAuth "Sign in with Polar" — dock as IdP for plugins (Mode 2 from above) | After open-platform.md P3 |
| **P6** | Plugin impersonation (Mode 3) with admin install gates | After P5 |

## Cross-references to open-platform.md

| open-platform.md item | This ADR section |
|---|---|
| Plugin manifest `auth.requires_plugin_token` | Plugin HMAC token, minted at install |
| Plugin manifest `auth.requires_llm_proxy` | `polar_proxy_*` auto-mint with `bound_plugin_id` |
| Plugin manifest `auth.requires_pat_scopes` | OAuth consent (Mode 2 in this ADR) |
| display_mode=iframe short-lived JWT | `/internal/v1/jwt/verify` endpoint |
| display_mode=tab token handoff via query string | Same — 5-min JWT, plugin trades for session |
| Plugin uninstall cascade | Cascading revocation section above |
| First-party plugins follow same rules | All token types apply uniformly |

## Open questions (defer until P1 kickoff)

1. **OAuth library**: roll our own (Go stdlib + manual OIDC) vs `golang.org/x/oauth2` + `github.com/coreos/go-oidc`. Likely the latter — battle-tested, less rope.
2. **Passkey credential storage**: Postgres `webauthn_credentials` table — schema deferred to P4.
3. **Apple Sign in webhook reliability**: Apple's revocation webhook is unreliable. Add a daily cron that re-validates Apple tokens. Defer mechanism details to P3.
4. **`pat_tokens` vs unify into `llm_proxy_tokens`**: open-platform.md says "segmentation wins"; this ADR follows that. Reviewer may push back — keep open.
5. **Rate limit storage**: Redis (low latency, separate concern) vs Postgres rolling-window query. Likely Redis given LLM proxy already uses it.
6. **Hot/cold audit log split**: 90d Postgres + S3 cold storage. Schema for cold tier deferred to ops decision.

## DoD (this ADR collapses to "decisions documented")

- [x] 7 decisions recorded with rationale
- [x] Identity source matrix + schema + per-provider notes
- [x] Just-in-time provisioning + account linking flow
- [x] Three token types with schema sketch
- [x] 90-day PAT expiry policy with rotation grace
- [x] Three-level control granularity
- [x] Approval rubric
- [x] Three modes of plugin-acts-on-user
- [x] Cascading revocation flow
- [x] Audit log shape
- [x] Endpoint inventory
- [x] Phasing tied to extraction roadmap + open-platform.md
- [x] Cross-references to companion ADR
- [x] 6 open questions surfaced for P1 kickoff
