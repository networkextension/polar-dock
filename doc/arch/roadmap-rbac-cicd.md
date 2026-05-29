# Roadmap: Team-Ownership RBAC + dev/stage/prod CI/CD

Status: PLAN (2026-05-29). Two parallel tracks driven by two product goals:

- **Track R — RBAC / ownership:** the root (site-admin) team owns the
  platform. It publishes modules, approves new teams, and enables
  modules per-team on demand. **There are no personal resources and no
  personal teams** — every resource belongs to a real (approved) team,
  and **every team ships with one AI-driven team bot as its minimum
  configuration.**
- **Track C — Environments + CI/CD:** dev → stage → prod with automatic
  build, test, and release/deploy.

> **Decisions locked 2026-05-29:** Plan **B** — eliminate personal
> teams/resources entirely (not the softer "keep a default personal
> team" Plan A). Plus: a team is not usable without an AI team bot, so
> bot provisioning is part of team creation, not an add-on.

This doc inventories what already exists, names the gaps, and sequences
the work into shippable phases. It does not change code; each phase
below becomes its own PR series.

---

## Current state (verified 2026-05-29)

### RBAC — mostly built, approval/marketplace UX missing
- **Site admin = root-team owner/admin.** `IsSiteAdmin()` joins
  `team_members` to the single `teams.is_root = TRUE` team
  (`internal/app/dock/perm_site_admin.go`). `AdminMiddleware` calls it;
  `POLAR_LEGACY_ADMIN_ROLE=1` rolls back to `users.role='admin'`.
  Provisioning mutations write `admin_audit_log`. (perm-model P0/P1 — done.)
- **Per-team module enablement infra EXISTS:** `plugin_modules`
  (global `enabled` + `ui_routes`) + `workspace_plugin_access`
  (`workspace_id, plugin_name, enabled`). Root team bypasses; non-root
  teams need an `enabled=TRUE` grant row. Site admin flips via
  `PUT /api/admin/workspace-plugin-access`.
- **Gaps:** team creation is open self-serve (`POST /api/teams`, no
  gate); no module "publish"/marketplace UX (rows created by raw API);
  no per-team module *request→approve* flow; team
  suspend/delete/transfer endpoints missing; team create/suspend not
  wired to `admin_audit_log`.

### Ownership — migration already in-flight ("T6")
- **Already team-scoped (`workspace_id NOT NULL`):** projects, posts
  (+ post_images/attachments), assets, chat_rooms, expenses,
  llm_proxy_tokens/calls, quota/recharge ledgers, iosdist_agent_builds.
- **Hybrid (nullable `workspace_id`, backfill pending):** llm_configs,
  bot_users, packtunnel_profiles/rule_files, video_projects,
  iosdist_apps/certificates/profiles/asc_configs. Column added; NULL =
  still personal; awaiting backfill + NOT NULL.
- **Personal-only (no/unenforced workspace_id):** markdown_entries
  (nullable workspace_id), agent_tokens (user_id only),
  prompt_templates (platform-global), chat DM threads (user×user).

### CI/CD — per-repo PR builds, manual deploy
- Each repo has a build/test workflow on PR (`go vet` + `go test -race`
  for Go modules; `npm ci && npm run build` for UI). polar-dock has a
  tag→GitHub-Release workflow (cross-builds dock to 5 platforms).
- **No staging.** **No auto-deploy** — cross-build on dev → rsync over
  WG → `launchctl unload/load` on .57, all by hand (every deploy this
  session). Config lives in per-box `~/<svc>.env` sourced by launchd
  wrappers; one shared Postgres/Redis; no env separation in code.
- Topology: **.57** = prod-ish (zen.4950.store), **.69** = dev box,
  **.64** = FreeBSD jumpbox. Boxes are behind NAT, reachable via WG.

---

## Decisions — locked & open

**Locked (2026-05-29):**
1. **Plan B — no personal teams, no personal resources.** The
   `kind='personal'` workspace concept is removed. Every user belongs to
   one or more real (approved) teams; every resource is owned by a team
   (`workspace_id NOT NULL`, no `user_id`-only ownership). A brand-new
   user with no team can do nothing except create/join one (which goes
   through approval, R2).
2. **Every team has one AI-driven "team bot" as minimum config.** Team
   provisioning is not complete until a default team bot exists. The bot
   is bound to an LLM config (platform-default to start) and is the
   baseline the team can extend with more bots. A team without its team
   bot is not "active".

**Locked (2026-05-29, batch 2):**
3. **B. Personal-team migration = in-place conversion.** Flip every
   `kind='personal'` row to `kind='team'` (preserves all data; it becomes
   the user's first real team). Grandfather to `status='active'` (skip
   approval). The user's *home/default* workspace is no longer identified
   by `kind` — it is the team whose `slug == personalTeamSlug(userID)`,
   owned by that user. All default-workspace resolution keys on that slug,
   not on `kind='personal'`, so it survives the conversion.
4. **A. Approval flow = request → pending → approve.** Site admin approves;
   no auto-approve quota for v1. The **requester must hold team-admin role**
   (owner/admin in `team_members`) on the team the request is for — regular
   members cannot request modules.
5. **C. Module enablement = both, per-module gated.** Site admin can push
   enablement directly; tenants can also *request* — but only for modules
   the admin published with `accepts_applications = true`. Each published
   module carries a manifest declaring its full **API-route list**
   (`api_routes`) and **page-route list** (`ui_routes`, drives sidebar
   injection on enable). Requests can be **module-level** (whole module)
   or **api-level** (a specific API route within the module).

**Still open (pick before C3):**
- **D. Where staging lives** — third box vs. second DB + vhost on .69.
  Pull-based deploy assumed (boxes behind NAT).
- **E. Team bot identity** — does the team bot get its own LLM config
  (billable to the team) or share the platform-default Kimi until the
  team adds credit? (Plan: platform-default at provision, team can
  rebind; ties into existing team LLM quotas/billing.)

---

## Track R — RBAC / team ownership

### R1 — Eliminate personal scope ("个人资源移走")  *(foundational, do first)*
Plan B: finish the T6 migration AND retire the personal-team concept so
every resource is owned by a real team.
- **R1a Backfill + enforce hybrids:** for each hybrid table (llm_configs,
  bot_users, packtunnel_*, video_projects, iosdist_*), backfill
  `workspace_id` from the owner's (soon ex-personal) team, then
  `SET NOT NULL` (guarded migration: assert NULLs → 0 before the
  constraint flips).
- **R1b markdown_entries + remaining personal-only:** same backfill +
  NOT NULL; rewrite read/write/list handlers to be workspace-scoped
  (mirror the assets/posts pattern). Pairs with the just-shipped
  markdown editor-link fix.
- **R1c Convert personal teams in place:** flip every `kind='personal'`
  team to `kind='team'` (per locked decision B), so existing users keep
  their data inside what is now their first real team. Grandfather them
  to `status='active'` (skip approval for pre-existing teams). Stop
  auto-creating a personal team on signup.
- **R1d Write-path audit + invariant:** grep every
  `INSERT ... owner_user_id` lacking a workspace_id, route through the
  active workspace (`requireWorkspaceID`), and delete the "NULL =
  personal" branches. Add a startup assertion that fails loudly if any
  covered table has a NULL workspace_id, and remove
  `personalWorkspaceForUser` / personal-team helpers.
- **R1e agent_tokens:** the one intentional exception — device auth is
  per-user and crosses teams. Keep `user_id`-scoped; document why.
- Deliverable: single invariant "ownership = team"; no `kind='personal'`
  rows; new users land in a "create or join a team" gate.

### R2 — Team creation approval + default team-bot provisioning
- Schema: `teams.status` (`pending|active|suspended`). New teams from the
  public endpoint land `pending`; grandfathered (ex-personal) teams are
  `active`.
- Site-admin endpoints: list pending, approve/reject → writes
  `admin_audit_log` (category `provisioning`). Resource access gated on
  `status='active'`; pending owner sees a "waiting for approval" state.
- **Team-bot provisioning (minimum config):** on a team reaching
  `active`, atomically provision its default AI team bot —
  - create a `bot_users` row (`workspace_id = team`, system-owned,
    name e.g. "<team> 助手"),
  - bind it to an LLM config (platform-default Kimi to start; see open
    decision E),
  - seat it in the team's default chat room so it's @mention-able / can
    run the qunliao idle-loop.
  Reuse the existing `ensureAutoRegisteredCoder/ResearchBot` +
  chat-room/bot-fanout machinery rather than inventing a new path. Make
  it idempotent (re-running provisioning is a no-op).
- Invariant: an `active` team always has ≥1 bot; the team bot can't be
  deleted (only replaced/rebound). A backfill provisions the team bot
  for every grandfathered team.
- UI: site-admin "Team approvals" queue on `/admin-teams.html`;
  requester-side pending banner; team settings shows + lets you rebind
  the team bot's model.

### R3 — Module publishing + per-team enablement UX (marketplace)
Reuse the existing `plugin_modules` + `workspace_plugin_access` infra;
add publishing metadata + the UX layer.
- **Schema additions to `plugin_modules`:**
  - `published BOOLEAN NOT NULL DEFAULT FALSE` — 上架 flag (visible in the
    marketplace only when true).
  - `accepts_applications BOOLEAN NOT NULL DEFAULT FALSE` — the per-module
    toggle: can tenants *request* this module, or is it admin-push-only?
  - `api_routes JSONB NOT NULL DEFAULT '[]'` — the module's full API
    surface, mirroring the existing `ui_routes` shape. Pushed by the
    plugin at heartbeat (like `ui_routes`) and/or edited by admin at
    publish. Element: `{"method":"GET","path":"/api/expense/items","label":"列出账目","scope":"read"}`.
    Drives api-level access requests + the per-route access gate.
  - (`ui_routes` already exists — page-route list; enabling a module
    injects these into the dynamic sidebar via `/api/plugin-ui-routes`.)
- **R3a Publish ("上架"):** site-admin module catalog UI (CRUD over
  `plugin_modules`: key rotation, enable/disable, `published` +
  `accepts_applications` toggles, view/edit `ui_routes` + `api_routes`).
  Publishing requires a non-empty manifest (both route lists present).
  Audited.
- **R3b Enable per-team + applications:**
  - Site-admin grid (team × module) toggling `workspace_plugin_access`
    directly (push model). Audit every flip.
  - Tenant request path: a **team-admin** (owner/admin in `team_members`;
    regular members get 403) requests a published, `accepts_applications`
    module. New table `plugin_access_requests`:
    `(id, workspace_id, plugin_name, scope ENUM('module','api'),
    api_route TEXT NULL, requested_by_user_id, status
    ('pending'|'approved'|'denied'), reviewed_by, reason, timestamps)`.
    Site admin approves → writes the `workspace_plugin_access` grant
    (module scope) or a finer api-level grant; audited as `provisioning`.
- **R3c Discovery:** tenant "Modules" page listing published-available vs
  enabled (driven by `/api/plugin-ui-routes` + the new catalog endpoint);
  team-admins see an "Apply" button only on `accepts_applications` modules.

### R4 — Team lifecycle + hardening
- Suspend / un-suspend / delete / transfer-ownership endpoints + UI,
  all audited. Suspended team = read-only or blocked.
- Wire team create/approve/suspend/delete into `admin_audit_log`
  (closes the audit gap).
- Optional (defer): site-admin scoped impersonation for support.

**R dependencies:** R1 first (everything assumes workspace ownership).
R2 + R3 are independent and can run in parallel after R1. R4 last.

---

## Track C — Environments + CI/CD

### C1 — Normalize CI across repos  *(low effort, do early)*
- Ensure every service repo (polar-dock, -ui, -hosts, -wg, -agent,
  -expense, -projects, -library, -iosdist, -latch, -video, -lawyer,
  -assets) has the same PR gate: `go vet` + `go build ./...` +
  `go test ./...` (UI: build). Fill the gaps (agent/lawyer/wg lacked
  workflows).
- Add an **optional integration job**: spin up a Postgres service
  container, run the dock schema migration + a boot smoke (the
  DB-touching tests the in-package convention currently defers to manual
  smoke). Makes "does it actually start + migrate" a CI gate.

### C2 — Release automation  *(low effort)*
- Tag (`vX.Y.Z`) → CI cross-builds **all** artifacts (dock + every
  plugin svc + UI dist) → one versioned GitHub Release with tarballs.
  Extend the existing dock release workflow to the plugin svcs (they
  build ad-hoc today).
- Stamp version via `-ldflags` (git sha/branch already surfaced by
  `/api/system/info`); add the same to each plugin svc so
  `/healthz`/`/stats` report a version.

### C3 — Environment model  *(medium)*
- Define **dev (.69) / stage (new or .69-second-DB) / prod (.57)**.
  Per-env: separate DB (`ideamesh_dev|stage|prod`), separate vhost
  (`*.dev.4950.store` / `stage` / prod), separate provider region.
- Move config from hand-edited per-box `.env` to a **versioned template
  + injected secrets**: commit `deploy/env/<svc>.<env>.env.tmpl`
  (non-secret keys + placeholders); secrets stay out of git, filled at
  deploy time from a vault/age-encrypted file. One source of truth for
  "what env vars each svc needs per env."
- Document the env matrix in `doc/ops/environments.md`.

### C4 — Deploy automation  *(high effort, highest ROI)*
Replace the manual rsync+launchctl loop. Boxes are behind NAT → prefer
a **pull model**.
- **C4a Release channel:** each env tracks a channel (dev = latest main
  build, stage = release candidate, prod = released tag).
- **C4b On-box deploy agent:** a small service on each box polls for the
  channel's target version, downloads the artifact (from GitHub
  Releases or the assets module — dogfood it), verifies checksum,
  atomic-swaps the binary (`.new` → rename, keep `.bak`),
  `launchctl kickstart`, runs a `/healthz` smoke, and **auto-rolls-back
  to `.bak` on smoke failure**. This is exactly the manual sequence from
  this session, automated.
- **C4c Promotion flow:** push to main → auto-deploy dev → smoke →
  manual "promote to stage" → smoke → manual "promote to prod". Gated by
  site admin.
- **C4d Schema migrations:** dock runs idempotent `CREATE TABLE IF NOT
  EXISTS` / `ADD COLUMN IF NOT EXISTS` at boot (already the pattern), so
  deploy = binary swap + restart; no separate migration step. Keep that
  invariant; add a pre-deploy "migration dry-run" check in CI.

### C5 — Deploy observability
- Per-env version dashboard (what's running where), deploy events into
  an audit/log stream, alert on smoke-fail/rollback. `/api/system/info`
  already exposes the git ref — surface it per env in a status page.

**C dependencies:** C1 + C2 first (cheap, unblock). C3 (env model) before
C4 (deploy needs env targets + config). C5 after C4.

---

## Suggested sequencing

```
Now ─┬─ R1  eliminate personal scope    (foundational; the "移走个人资源" ask, Plan B)
     ├─ C1  normalize CI                (cheap, parallel)
     └─ C2  release automation          (cheap, parallel)

Then ┬─ R2  team approval + team-bot provisioning  ┐ parallel after R1
     ├─ R3  module marketplace                      ┘
     └─ C3  env model (dev/stage/prod)

Then ┬─ C4  deploy automation (pull agent + promotion)   ← biggest pain-killer
     └─ R4  team lifecycle + audit closure

Last ── C5  deploy observability
```

Rationale: R1 is the user's stated first step ("个人基本部分要先移走") and
unblocks every ownership-dependent feature. C1/C2 are near-free and make
all later work safer. C4 (deploy automation) is the highest-ROI infra
item — it erases the manual rsync/launchctl toil — but needs C3's env
model first. R2/R3 deliver the "root team runs the platform" product
surface and reuse infra that already exists.

## Risks
- **R1 backfill on live data:** every `NOT NULL` flip must be preceded by
  a zero-NULL assertion; do it per-table behind a guarded migration with
  a rollback path, exactly like the permission-model `POLAR_LEGACY_*`
  flag pattern.
- **Plan B blast radius (now the chosen path):** killing personal teams
  touches signup, every resource list, and the default-workspace
  assumption across the whole UI. Mitigate by converting personal teams
  in place (R1c) — data is preserved, only the `kind` + signup flow
  change — and by gating behind a flag so the old personal path can be
  re-enabled if the cutover regresses. Ship R1 to dev/stage first.
- **Team-bot provisioning failure:** if the team bot can't be created
  (LLM config missing, quota), team approval must not half-complete.
  Provision the bot in the same transaction/flow as activation and make
  it idempotent + retryable; surface a clear admin error rather than an
  active-but-botless team.
- **C4 deploy agent is itself prod software** behind NAT — it needs the
  same signed-artifact + checksum discipline the assets module already
  has; dogfood the assets signed-URL flow for artifact delivery.
