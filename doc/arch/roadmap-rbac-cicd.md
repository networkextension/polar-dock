# Roadmap: Team-Ownership RBAC + dev/stage/prod CI/CD

Status: PLAN (2026-05-29). Two parallel tracks driven by two product goals:

- **Track R — RBAC / ownership:** the root (site-admin) team owns the
  platform. It publishes modules, approves new teams, and enables
  modules per-team on demand. Personal resources go away — every
  resource belongs to a team.
- **Track C — Environments + CI/CD:** dev → stage → prod with automatic
  build, test, and release/deploy.

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

## Open product decisions (resolve before R1/C3)

1. **Fate of the personal team.** "没有个人资源,资源必须进入 team."
   Two readings:
   - **(A) Recommended — keep a per-user *default* team, drop user_id-only ownership.** Every resource gets `workspace_id NOT NULL`; the personal team (`kind='personal'`) stays as the user's private default workspace so a solo user can still own things. "No personal resources" = no resource is owned by a bare `user_id`; they're all workspace-scoped. Lowest disruption, finishes the T6 migration already underway.
   - **(B) Stricter — eliminate personal teams entirely.** New users own nothing until they belong to an approved real team. Requires the approval flow (R2) to gate *all* resource creation and a story for "user with no team". Bigger UX change.
   The plan below assumes **(A)** unless you choose (B).
2. **Team-creation approval granularity.** Per-team manual approval by
   site admin, or auto-approve with quota + post-hoc review? (Plan: request→pending→approve.)
3. **Module enablement model.** Tenant *requests* a module (admin
   approves), or site admin *pushes* enablement with no request? (Plan: support both — request queue + direct grant.)
4. **Where staging lives.** A third box, or a second DB + vhost on an
   existing box (.69)? Pull-based deploy assumed (boxes behind NAT).

---

## Track R — RBAC / team ownership

### R1 — Finish team-scoping ("个人资源移走")  *(foundational, do first)*
Complete the in-flight T6 migration so every user resource is
workspace-scoped.
- **R1a Backfill + enforce hybrids:** for each hybrid table (llm_configs,
  bot_users, packtunnel_*, video_projects, iosdist_*), backfill
  `workspace_id` from owner's personal team, then `SET NOT NULL`
  (guarded migration: count NULLs → 0 before the constraint flips).
- **R1b markdown_entries:** add the same backfill (user → personal team)
  + NOT NULL; update read/write/list handlers to be workspace-scoped
  (mirror the assets/posts pattern). Pairs with the just-shipped
  markdown editor-link fix.
- **R1c Write-path audit:** grep every `INSERT ... owner_user_id` that
  lacks a workspace_id and route it through the active workspace
  (`requireWorkspaceID` / `personalWorkspaceForUser`). Remove the "NULL =
  personal" branches once backfilled.
- **R1d agent_tokens decision:** keep per-user (device auth crosses
  teams) OR add `workspace_id`. Recommend keep per-user but document why
  it's the one intentional exception.
- Deliverable: a single "ownership = workspace" invariant; a startup
  assertion that fails loudly if any covered table has a NULL
  workspace_id.

### R2 — Team-creation approval
- Schema: `teams.status` (`pending|active|suspended`) or a
  `team_creation_requests` table. New teams created via the public
  endpoint land `pending`.
- Site-admin endpoints: list pending, approve/reject (writes
  `admin_audit_log`, category `provisioning`). Approved → `active`.
- Gate resource access on `status='active'`. Pending team owner sees a
  "waiting for approval" state.
- UI: site-admin "Team approvals" queue on a new `/admin-teams.html`
  (or extend an existing admin page); requester-side pending banner.

### R3 — Module publishing + per-team enablement UX (marketplace)
Reuse the existing `plugin_modules` + `workspace_plugin_access` infra;
add the UX layer.
- **R3a Publish:** site-admin module catalog UI (CRUD over
  `plugin_modules`, key rotation, enable/disable, edit `ui_routes`).
  "上架" = create + enable a module row. (API exists; this is the UI +
  audit wiring.)
- **R3b Enable per-team:** site-admin grid (team × module) toggling
  `workspace_plugin_access`. Optional tenant-side "request this module"
  → request queue → admin approve (writes the grant). Audit every flip.
- **R3c Discovery:** tenant "Modules" page listing available vs enabled,
  driven by the existing `/api/plugin-ui-routes` filter.

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
Now ─┬─ R1  finish team-scoping        (foundational; also the "移走个人资源" ask)
     ├─ C1  normalize CI               (cheap, parallel)
     └─ C2  release automation         (cheap, parallel)

Then ┬─ R2  team approval  ┐ parallel after R1
     ├─ R3  module marketplace ┘
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
- **Decision (B) blast radius:** killing personal teams touches signup,
  every resource list, and the default-workspace assumption across the
  whole UI. Prefer (A) unless there's a hard product reason.
- **C4 deploy agent is itself prod software** behind NAT — it needs the
  same signed-artifact + checksum discipline the assets module already
  has; dogfood the assets signed-URL flow for artifact delivery.
