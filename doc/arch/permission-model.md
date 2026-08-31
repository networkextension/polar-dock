# Permission model

**Status:** proposal v3 (post-review). Replaces the implicit two-axis
model that emerged from Sprint 1 (`teams.is_root` tenant gate) sitting
next to the older `users.role='admin'` flag.

**Author:** Claude (drafting on behalf of @yarshure) — 2026-05-27.

---

## 0. Load-bearing principle

> **The site team has bounded, business-only powers — provisioning,
> reconciliation, audit. It NEVER reads user data.**
>
> Real data (chats, projects, expenses, video shots, library entries,
> WG device tokens of tenants, …) lives inside tenant workspaces and
> is reachable only by that tenant's own members through tenant-scoped
> routes. The site team's reach stops at the **metadata of the
> business relationship** — who is provisioned, what they're entitled
> to, what they owe, and an audit trail of who-did-what.

This is the rule that makes the whole model defensible. Every other
decision below derives from it.

### 0.1 What the site team is allowed to do

Three buckets and **only** three buckets:

1. **业务开通管理 / Provisioning** — create / suspend / delete tenant
   workspaces; enable or disable plugins per tenant
   (`workspace_plugin_access`); set per-tenant quotas (LLM token
   budget, video minutes, storage caps); curate the platform-level
   catalog of LLM providers + pricing; configure site-wide things
   (invite codes, site icon, APNs cert, prompt templates,
   observability scrape token).
2. **对账 / Reconciliation** — see *aggregate* billing per tenant
   (totals: tokens spent, USD spent, recharges credited). Recharge
   a tenant's wallet. Generate invoices. **Sums, not line items.**
   "Tenant X spent $42 this month" is fine; "tenant X's specific
   prompts and replies" is not.
3. **审计 / Audit** — read the platform-wide admin audit log
   (`admin_audit_log`, §3.4). Read the impersonation log when that
   feature lands (§2.2). See *that* something happened and who did
   it — never see the content that was acted on.

### 0.2 What the site team is NOT allowed to do

- Read tenant chat history, room messages, projects, expenses
  (including OCR'd receipts), video shots / assets, library
  firmware blobs, WG peer secrets, host VNC sessions, etc.
- Run AI agent calls as a tenant, dispatch host skills against a
  tenant's hosts, sign iOS builds for a tenant.
- Anything the *tenant* itself cannot do for itself through normal
  product flows — site admin shouldn't be a backdoor superpower.

Consequences:

- A site-admin endpoint that reads or mutates user-generated content
  is **wrong by construction**, full stop. If a real support
  situation needs that access, it must go through the explicit
  *impersonation* flow (§2.2) with a tenant-side audit trail, not a
  "scoped to all tenants" query parameter.
- The root team is just a normal workspace from the data-storage
  perspective. Its members store their own chats/projects/etc. in
  rows tagged with the root team's `workspace_id`, exactly like any
  other team. They get no implicit read access to other workspaces.

The cleanest test: **delete the row from every site-admin endpoint
that reads tenant user-generated content.** If the endpoint doesn't
fit "provisioning OR reconciliation-aggregate OR audit", it
shouldn't be a site-admin endpoint.

### 0.3 Every site-admin action is logged

Every mutation a site-team member makes — provisioning, quota
change, plugin enable/disable, wallet recharge, prompt-template
edit, anything — writes an `admin_audit_log` row: **who, when,
which role, what action, against which tenant** (when applicable),
with a stable JSON payload of the before/after delta. This is not
optional and not P5-deferred — it lands in P0 alongside the
middleware change. See §3.4 for the schema and §3.6 for the
helper API.

---

## 1. Current state — why it's confusing

Three overlapping concepts ended up in the schema, each driven by a
different feature wave, and they aren't consistently checked together.

| # | Concept | Where it lives | What it currently gates |
|---|---|---|---|
| 1 | **Platform admin** | `users.role IN ('user','admin')` (`store.go:772`) | Every `AdminMiddleware()` route — site settings, prompts, plugin-modules CRUD, LLM-config admin, LLM-proxy tokens, invite codes, etc. (~35 routes in `app.go`) |
| 2 | **Root tenant** | `teams.is_root BOOLEAN` with partial UNIQUE (`store.go:2827`) | (a) `workspace_plugin_access` gate — root team bypasses the per-plugin allow-list (Sprint 1). (b) `llm_configs.shared` toggle — only root-team users can flip it (B-llm cleanup PR). |
| 3 | **Team admin** | `team_members.role IN ('owner','admin','member','viewer')` (`store.go:1069`) | Intra-team things — invite members, change team name/avatar, set `default_llm_config_id`, etc. |

Today user `a` happens to satisfy all three (he's `users.role='admin'`,
he's the only member of the root team `lzhu` with role `owner`). That
masks the design gap: there's no shared definition of "platform admin",
and the two halves can drift.

**Concrete bug:** if a future operator wants to grant platform-admin
rights to a colleague, they have two unrelated steps to remember —
`UPDATE users SET role='admin'` AND `INSERT INTO team_members
(root_team_id, …, 'admin')`. Forgetting either half silently
half-promotes them. The Sprint-1 root-team gate doesn't look at
`users.role`; the legacy `AdminMiddleware` doesn't look at the root
team. So a "platform admin" by one definition is denied by the other.

---

## 2. Goal: one source of truth

Make **membership in the root team** the single answer to "is this
user a site admin?". `users.role` becomes a legacy column we stop
reading; new code paths use the team-derived check.

Site admin's job is bounded by §0: **platform configuration + tenant
lifecycle**, nothing else. Real user data — chats, projects,
expenses, video shots, library entries — lives in tenant workspaces
and is read/written only through tenant-scoped routes that the
data's owners can see in their own audit logs.

This matches the user's stated intent: *root team = configuration
management + tenant management; real data lives on the tenant side.*

### 2.1 Three roles, clearly separated

After this proposal:

| Role | Defined by | What it can do | What it CAN'T do |
|---|---|---|---|
| **Site admin** | `team_members` row where `team_id = root_team.id` AND `role IN ('owner','admin')` | **Provisioning**: register/enable/disable plugin_modules, curate LLM provider catalog + pricing, create/suspend tenant workspaces, toggle `workspace_plugin_access` per tenant, set per-tenant quotas, edit site_settings + prompts + invite codes + APNs cert, mint observability scrape tokens. **Reconciliation**: aggregate per-tenant billing (totals only), recharge a tenant's wallet, generate invoices. **Audit**: read `admin_audit_log` (everything site admins did), read `impersonation_log` (when §2.2 lands). | **Read any tenant's user-generated content** (chats, rooms, projects, expense receipts, video shots, library blobs, WG peer secrets, host VNC sessions, …). **Read per-tenant line-item billing detail** (line items are tenant content; the *sum* is operator data). For real support situations needing tenant data, see §2.2 impersonation. |
| **Tenant admin** | `team_members` row where `team_id = X` AND `role IN ('owner','admin')` | Manage team X's members. Set team X's `default_llm_config_id`. Recharge team X's billing wallet. Manage team X's WireGuard tokens / projects / expenses / video studio / library / hosts. Approve / configure plugins enabled for team X. Read the slice of `admin_audit_log` where they were the target (`GET /api/teams/:id/admin-actions`). | Touch any other tenant. Touch site config. |
| **Tenant member** | `team_members` row where `team_id = X` AND `role IN ('member','viewer')` | Use bots / plugins assigned to team X. Read/write per-feature data the team has granted them. | Destructive actions (delete projects, remove members, …). |

Anonymous / non-logged-in stays the existing GuestMiddleware path —
unchanged.

### 2.2 Impersonation, for the cases site admin really does need tenant data

When a site admin legitimately has to look at tenant X's data —
debugging a bug a tenant reported, responding to abuse, billing
dispute — the path is **explicit impersonation**, not a magical
read-everything endpoint.

Shape:

1. Site admin hits `POST /api/admin/impersonate` with `{tenant_id,
   reason}`. The reason is required (free-text, audit log).
2. Dock writes a row to `impersonation_log` (new table, see §3.4)
   and mints a short-lived session scoped to that tenant. The
   session carries an "impersonating=true" flag.
3. Site admin browses the tenant's UI exactly as a tenant member
   would — same endpoints, same `workspace_id` filtering, no
   `?workspace_id=*` escape hatches.
4. The tenant sees an "auditor session active" banner in their UI
   while the impersonation is live, and a permanent entry in their
   tenant-side audit log after.

Out of scope for v1 of this proposal — impersonation lands in a
later phase. **The principle still holds today:** site-admin
endpoints that read tenant content are wrong. They get rewritten
into impersonation-only access when impersonation ships, and stay
forbidden until then.

### 2.3 Root team membership semantics

- The root team is just a regular `teams` row with `is_root = TRUE`.
  The partial-UNIQUE index keeps that singleton.
- Adding a user to the root team via `team_members` is *the* way to
  grant site admin rights. Same UI as adding to any other team
  (invite by email + accept).
- Owner vs admin within the root team: owner can demote/remove other
  root-team admins; admin can do everything else.
- Removing your own root-team membership while you're the only owner
  → blocked, same shape as "can't leave a team you own elsewhere".
- **Per §0, root-team membership grants no implicit read access to
  other tenants' data.** Root team members store their own personal
  chats/projects in rows tagged with the root team's `workspace_id`;
  those are theirs, just like every other team's data is its own.

### 2.4 What about a root-team *member* (non-admin)?

`role = 'member'` in the root team grants no site-admin powers — they
just happen to share a workspace. This is intentional: the root team
is a normal workspace that *additionally* confers site admin
when the membership role is owner/admin. No surprise privilege from
being added as a viewer.

---

## 3. Concrete code changes

### 3.1 Middleware

Add to `middleware.go`:

```go
// IsPlatformAdmin returns true when the user is a root-team
// owner or admin. Cached for 30s per (user_id, root_team_id)
// pair to avoid hitting the DB on every request.
func (s *Server) IsPlatformAdmin(userID string) (bool, error) { … }

// PlatformAdminMiddleware — replaces AdminMiddleware. Same shape,
// but the predicate is team-membership-based, not users.role.
func (s *Server) PlatformAdminMiddleware() gin.HandlerFunc { … }
```

Keep `AdminMiddleware()` as a thin alias for `PlatformAdminMiddleware()`
during the transition; mark deprecated. After the cutover lands and
all call sites compile, delete the legacy alias.

### 3.2 Route reclassification

Audit the ~35 routes currently using `AdminMiddleware()` against §0
("site admin = system config only"). Three buckets:

**Stays site admin** — these *are* system configuration and the
data they touch is the platform catalog itself, not user content:

- `/api/admin/plugin-modules*` — plugin registry CRUD
- `/api/admin/llm-configs*` — provider catalog + pricing (the
  *catalog* of providers, not what tenants *did* with them)
- `/api/admin/llm-config-team-quotas` (GET/PUT) — per-tenant
  allowance numbers, set by the platform operator
- `/api/admin/llm-config-team-quotas/recharge` (POST) — operator
  adds credit to a tenant's wallet, balance-only, no usage detail
- `/api/admin/workspace-plugin-access*` — per-tenant plugin
  allow-list (Sprint 1)
- `/api/prompts*` — site-wide prompt templates
- `/api/site-settings*` + `/api/site-settings/invite-codes*` +
  `/api/site-settings/icon` + `/api/site-settings/apple-push-cert`
- `/api/llm-proxy/tokens*` — platform-issued proxy tokens (the
  tokens are the catalog entry; what the holder *spent them on*
  belongs in §3.2 bucket 3 below)
- `/api/llm-proxy/dashboard-stats` — aggregate counters; no per-
  tenant content body, just counts/sums

**Moves to tenant-scoped routes** — these read tenant content and
should be `/api/teams/:id/…` (or per-feature equivalent) with
handler-internal `team_members.role` check, **not** site admin:

- `/api/admin/llm-config-applications` (GET) and
  `/api/admin/llm-config-team-quotas/recharges` (GET) — if these
  surface a specific tenant's history (vs. aggregate counts), audit:
  the *recharge ledger* probably stays site admin (it's how the
  platform operator does its books), but a *per-tenant detail view*
  of "what did tenant X spend it on" must go through tenant-scoped
  routes the tenant itself can see, or through impersonation (§2.2).
  Verify before flipping.

**Needs impersonation when it lands** (today: refuse the request
unless the asker is the data's tenant; site admin gets 403 instead
of a "scoped to all tenants" superpower):

- Any future "list all chat conversations" / "list all projects" /
  "list all video shots" / "see this expense receipt" endpoint a
  support engineer might be tempted to add. Those don't exist
  today (good); the proposal forbids adding them as `AdminMiddleware`
  routes. They must be tenant-scoped routes used via the §2.2
  impersonation flow.

### 3.2a One-time audit checklist (P3)

A concrete pass to run once before P3 declares done — fail any
endpoint that:

1. Currently uses `AdminMiddleware()` AND
2. Reads or writes a row tagged with `workspace_id` that is not the
   *root team's own* workspace_id

These either move to bucket 2 (tenant-scoped) or get deferred to
the impersonation flow in bucket 3. None of them stay site admin.

### 3.3 Bootstrap rule

On dock boot:

1. If `teams.is_root = TRUE` exists, do nothing.
2. Else if exactly one `users.role = 'admin'` exists, auto-promote
   that user's personal team to `is_root = TRUE`, and add that user as
   `team_members.role = 'owner'`. Log loudly.
3. Else require an operator to run `polar-dock init-root --user <id>`
   (new CLI subcommand) and refuse to start the admin endpoints until
   they do. Better than picking arbitrarily.

This makes fresh installs self-configure on the common path and makes
the broken paths visible instead of silent.

### 3.4 Schema changes

**P0 — admin_audit_log (table stakes, lands with the middleware):**

```sql
CREATE TABLE IF NOT EXISTS admin_audit_log (
    id              BIGSERIAL PRIMARY KEY,
    actor_user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    -- Role at the time of the action, denormalized so revoking
    -- privilege later doesn't blank the audit trail:
    actor_role      TEXT NOT NULL,           -- 'site_owner' | 'site_admin'
    -- Bucket per §0.1 so the table is grep-able by category:
    action_category TEXT NOT NULL,           -- 'provisioning' | 'reconciliation' | 'audit_read' | 'config'
    action          TEXT NOT NULL,           -- 'plugin_enable', 'quota_set', 'wallet_recharge', …
    -- The thing acted on, when applicable. Multiple FKs are kept
    -- nullable so a single action targeting "site itself" (e.g.
    -- prompt-template edit) doesn't need a synthetic tenant row.
    target_team_id  TEXT REFERENCES teams(id) ON DELETE SET NULL,
    target_kind     TEXT,                    -- 'team' | 'plugin_module' | 'llm_config' | 'site_setting' | …
    target_id       TEXT,                    -- free-form id of the targeted row
    -- Before/after delta + free-form note. Whatever lets a human
    -- 6 months later reconstruct what happened. Keep tenant
    -- content OUT — only metadata of the business action.
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    note            TEXT NOT NULL DEFAULT '',
    -- Where the action came from, for forensics:
    source_ip       TEXT NOT NULL DEFAULT '',
    user_agent      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_admin_audit_actor    ON admin_audit_log(actor_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_target   ON admin_audit_log(target_team_id, created_at DESC) WHERE target_team_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_admin_audit_category ON admin_audit_log(action_category, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_action   ON admin_audit_log(action, created_at DESC);
```

Retention: indefinite for now. Compliance horizon is bounded by
data-protection rules we haven't written yet; default to keep, add
a TTL knob when legal counsel asks.

Tenants get a *read-only* view of this table, filtered to actions
where `target_team_id = their_workspace`, so they can see who
provisioned what against them and when. Mounted as
`GET /api/teams/:id/admin-actions` and goes through the existing
tenant-admin gate (no new middleware needed).

**P0 — Existing schema changes:**

- `users.role` → keep the column (data preservation), stop *reading*
  it after the cutover, mark for future drop in a separate migration
  once a quarter passes without regressions.
- `team_members.role` → no change.
- `teams.is_root` → no change.

**Adds when impersonation lands (deferred to P5, §2.2):**

```sql
CREATE TABLE IF NOT EXISTS impersonation_log (
    id           BIGSERIAL PRIMARY KEY,
    admin_user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_team_id  TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    reason          TEXT NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at        TIMESTAMPTZ,
    session_token_hash TEXT NOT NULL  -- so the tenant audit log can correlate
);
CREATE INDEX IF NOT EXISTS idx_impersonation_admin ON impersonation_log(admin_user_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_impersonation_target ON impersonation_log(target_team_id, started_at DESC);
```

The tenant-side audit view reads from this table joined with
`users.username` so the tenant sees "yarshure auditor session at
2026-06-01 14:30 — reason: investigating SEV-2 incident".

### 3.5 Audit helper API (Go)

To make "every mutation logs" the cheap default, all site-admin
handlers grow one helper call:

```go
// LogAdminAction writes one admin_audit_log row. Called once per
// mutating handler, AFTER the underlying write succeeds so we
// never log an action that didn't happen.
//
// payload is whatever JSON best captures the before/after delta —
// for create: {"after": <row>}; for update: {"before": <row>,
// "after": <row>}; for delete: {"before": <row>}. Plugin tokens
// + raw secrets are redacted before serialization.
func (s *Server) LogAdminAction(c *gin.Context, in AdminAuditIn) error { … }

type AdminAuditIn struct {
    Category      string         // 'provisioning' | 'reconciliation' | 'audit_read' | 'config'
    Action        string         // 'plugin_enable', 'quota_set', …
    TargetTeamID  string         // "" when action targets the site itself
    TargetKind    string
    TargetID      string
    Payload       map[string]any
    Note          string
}
```

Reads (GETs) don't log by default — too noisy and the platform
isn't paranoid enough to need it yet. The exception: reading
the *audit log itself* logs an `audit_read` row, so the audit
trail of "who looked at the audit trail" is closed. Same goes for
the future impersonation log.

### 3.6 What gets logged at minimum

Non-exhaustive starting list — every existing `AdminMiddleware`
route in §3.2 bucket 1 grows a `LogAdminAction` call. Concretely
on day 1:

- `plugin_module.create / enable / disable / rotate_key / delete`
- `workspace_plugin_access.set` (per-tenant flip)
- `llm_config.create / update / pricing_update / delete`
- `llm_config_team_quota.set / recharge`
- `prompt_template.update`
- `site_settings.update / icon_upload / apns_cert_upload / apns_cert_delete`
- `invite_code.generate / revoke`
- `tenant.create / suspend / unsuspend / delete`  *(when those endpoints land)*

Each row carries enough payload that a follow-up question — "why
does tenant X have plugin video disabled today?" — can be answered
from the log alone, not by spelunking through git history.

### 3.7 UI surfacing

- `/admin-plugins.html` and every other site-admin-only page reads
  `whoami.site_admin: true|false` (new field on `/api/auth/whoami`)
  to decide whether to render the sidebar link at all. Today they just
  try to load and 403.
- A new `/admin-site-team.html` (or a section inside `/teams.html`)
  shows the root team's membership table with invite / promote /
  demote / remove buttons — same shape as any team's member admin
  page, but pinned to the root team and labeled "Site admins".
- A new `/admin-audit-log.html` lists `admin_audit_log` rows with
  filters by actor / target tenant / category / date range. Read-only
  except for the implicit `audit_read` self-log on view (§3.5).
- Tenant side: `/team-settings.html` (or wherever team admins manage
  their workspace) grows an "Admin actions against this team"
  section reading `GET /api/teams/:id/admin-actions`. Tenants can
  see when site admins flipped a plugin on/off, recharged the
  wallet, set a quota, etc. — building trust by being transparent.

---

## 4. Migration path

| Phase | Work | Risk |
|---|---|---|
| P0 | Land `IsSiteAdmin` + bootstrap rule + `whoami.site_admin` flag + **`admin_audit_log` table + `LogAdminAction` helper + all bucket-1 routes wired to call it**. `AdminMiddleware` keeps using `users.role`. Both paths agree because user `a` satisfies both. | Zero — read-only addition + append-only writes |
| P1 | Flip `AdminMiddleware` to call `IsSiteAdmin` internally. Keep `users.role` writes for read-after-write parity in audit logs. | Low — if `IsSiteAdmin` is wrong, every `/api/admin/*` 403s; one rollback flag (`POLAR_LEGACY_ADMIN_ROLE=1`) reverts immediately |
| P2 | UI: root team management page, sidebar gating by `whoami.site_admin`, "you are not a site admin" empty states instead of bare 403s. Tenant-side `/api/teams/:id/admin-actions` viewer ("who changed our setup and when"). | Cosmetic + tenant transparency |
| P3 | Documentation + route audit per §3.2 / §3.2a: every former `AdminMiddleware` call site reviewed against §0; bucket-2 routes move to tenant-scoped, bucket-3 routes get marked TODO-impersonation. | Slow + boring |
| P4 | Drop `users.role` column in a dedicated migration. New `/api/admin/me/legacy-flags` endpoint surfaces the historical value once for archaeology, then the column goes. | Defer; not blocking |
| P5 | Impersonation flow (§2.2): `POST /api/admin/impersonate` + `impersonation_log` table + tenant-side "auditor session active" banner + tenant audit-log entry on every impersonation. | New feature; lands after P3 tells us which endpoints actually need it. |

Backout: every phase reversible via env var (`POLAR_LEGACY_ADMIN_ROLE=1`)
until P4. After P4 the column is gone — needs a real revert PR.

---

## 5. Out of scope

- Multi-tenant SaaS per-workspace admin invites with email flows — the
  existing `team_invitations` table handles this; no new design.
- Per-feature ACLs (e.g. "platform admin but read-only on plugin tokens").
  YAGNI; we have ~3 humans on the platform. Revisit when there are 20.
- Audit log of who promoted/demoted whom — should land in P2 alongside
  the new root-team management UI, but as a small follow-up PR not a
  blocking concern.
- API tokens / service accounts as "platform admin" — out of scope; the
  HMAC plugin-token model already separates service-to-service auth
  from end-user auth.

---

## 6. Naming

- "Platform admin" beats "super admin" / "root admin" / "system admin"
  because it's literal: this person administers the polar **platform**.
- "Root team" stays as the schema name (`teams.is_root`) for continuity
  with Sprint 1; UI labels can show "Platform team" if "root" feels
  too unix-y to end users. Either works.
- "Tenant" = `workspace` = `team` — the codebase already treats them
  interchangeably; no rename proposed.

---

## 7. Open questions for review

1. **Resolved by v2 §0**: Should root-team-`member` (the non-admin
   tier) get *any* extra power? **No.** And site admin gets no
   implicit read of other tenants' data either — §0 is the gate.
2. P3 currently says "leave per-team routes alone." Are there any
   today that should actually require site admin (e.g. "delete any
   team")? Quick audit of `/api/teams/:id/*` handlers needed before
   shipping P3.
3. Should the root team be visible to non-members at all? Currently a
   `team_members.team_id` view returns rows the caller is in — meaning
   non-root users won't even see `lzhu` exists. Probably fine but
   worth deciding explicitly.
4. **New (v2)**: Impersonation gets its own phase number — call it
   **P5**. Lands after P3's audit completes, since P3 tells us which
   endpoints actually need an impersonation path.
5. **New (v2)**: When site admin views aggregate billing (e.g. "total
   tokens spent this month across all tenants"), is per-tenant
   breakdown allowed? Proposal: **yes for counts, no for content** —
   "tenant X spent $42" is operational, "tenant X's top prompts
   contained the phrase 'top secret merger'" is content. The line is
   sometimes fuzzy; default to redacting and let the UI ask for
   impersonation when a human needs detail.

---

## 8. Immediate workaround for "admin-plugins page 加载中"

Independent of this proposal: today's user `a` already has both
`users.role='admin'` AND root-team ownership, so they should pass
`AdminMiddleware()` and the page should load. If it doesn't, the
issue is likely:

- you're logged in as a different user (check `/api/auth/whoami` in
  devtools)
- session is stale (try logout/login)
- the admin-plugins JS bundle has a build-time error swallowed silently
  (devtools console)

Fix the right one before re-launching this proposal — don't conflate
"page doesn't load" with "permission model is wrong"; both are real
but they have different remedies.
