# Zone Federation Platform — design (v2)

**Status:** design (2026-06-14, v2). Builds on **polar-release** (signed binary registry),
**polar-zone** (cut/publish/bootstrap/drift CLI), and `polar-zone/scripts/fleet/`
(build-from-main → test → deploy pipeline, proven standing up the everest zone).

> **v2 changes** (from a design review): a mandatory **stage gate + signed stamp** before
> any version reaches a real zone; **canary rollout + auto-rollback**; **backup-before-migrate**;
> a realistic **license threat model** (client-side is friction, server-side is the lever);
> **federated identity** (zen as IdP) instead of per-zone accounts; explicit **key management**;
> **correctness** health + alerting; a **dock↔plugin compatibility** rule; and a **zone vs
> workspace** tenancy rule. The optimistic-path skeleton was sound; v2 designs the failure paths.

## 1. One-liner

zen is a **control plane** that opens, licenses, and watches a fleet of **zones**
(self-contained Polar replicas). Every version flows through a **promotion pipeline** —
build-from-`main` → unit test → **stage zone full test (incl. migrations)** → **stamp** →
canary → stable — and **nothing reaches a real zone unstamped**. A zone pulls only the
modules it's **entitled** to ("purchase"), auto-deploys signed binaries with **health-gated
rollout + rollback**, exposes health publicly, and **files issues back to zen**. End-user
iOS/Android apps discover zones from a public **config endpoint** and sign in with **one
federated identity** (zen as IdP) — reusing the multi-site app shell.

```
 dev push─▶ build@main ─▶ unit test ─▶ publish dev ─▶ STAGE zone (deploy+migrate+smoke)
                                                          │  result record
                                                          ▼
                                                  STAMP (human or AI, signed) ──┐ gate
                                                          ▼                      │
 zen control plane (polar-zone): release registry · channels · zones · licenses  │
   · promotion attestations · health board · issue intake · IdP · discovery      │
        │ beta→canary zones (health-gated) ───▶ stable→general fleet ◀───────────┘
        ▼
 zone: polar-zone agent (pull license+manifest · drift · OTA · rollback · health push · file issues)
        dock + entitled plugins · GET /zone/health (public)
 iOS/Android ─discover(signed)─▶ pick endpoint ─▶ SSO via zen IdP
```

## 2. Actors & trust

| Actor | Holds | Trust anchor |
|---|---|---|
| **zen control plane** (`polar-zone` module) | release registry, channels, zones, licenses, attestations, health board, issue intake, **IdP** | `release_key`, `license_key`, `promotion_key` (distinct) |
| **stage zone** | a real zone that tracks `dev`, with realistic seed data | same as any zone; its pass is a precondition to promote |
| **approver** (human **or AI reviewer agent**) | stamps promotion attestations | `approver_key` (per signer; AI agents have their own) |
| **zone** (everest, opera, customer site) | dock + entitled plugins, `polar-zone` agent | `zone_key` (ed25519, generated at bootstrap; pubkey approved = "开zone") |
| **developer** | publishes to `dev` | release admin token |
| **end-user app** | endpoint from signed discovery | federated identity at zen IdP |

**Four independent signature chains**, never conflated:
1. **Binaries** — `release_key`; verified by the agent before install (polar-release/sdk).
2. **Promotion stamps** — `promotion_key`/`approver_key`; verified by the registry before a
   version may advance to a fleet-facing channel.
3. **Licenses** — `license_key`; verified by the zone's dock offline + zen online.
4. **Zone identity** — `zone_key`; authenticates the zone's calls to zen.

## 3. Release promotion pipeline — the gate (centerpiece)

**Hard rule: a version (binary + its schema/migrations) cannot be promoted to a
fleet-facing channel until it has passed the stage zone AND carries a valid stamp.**
The registry enforces this; it is not convention.

### 3.1 Channels
- **`dev`** — every `main` build lands here automatically (internal/dev zones only).
- **`stage`** — the **stage zone** tracks `dev`; promotion `dev→stage` triggers the full
  stage test (real deploy + real migrations on realistic data + smoke/integration suite).
- **`beta`** — post-stamp; a small set of **canary zones** auto-update, health-gated.
- **`stable`** — after canary health holds for a soak window; the general fleet.

### 3.2 Stage test (must pass)
On the stage zone, for the candidate version, run the real `deploy.sh` path:
1. **pg_dump backup** of each target DB first (failure-safe; ties to backup-before-migrate).
2. Apply migrations; **re-apply once** (idempotency check); assert no error.
3. Bring the module up; run the module's smoke/integration suite + cross-module contract
   checks (e.g. dock `/ping`, `/api/nav`, `/zone/health` green).
4. Hold for a soak window; assert health stays green and no error-log regressions.
Produces a signed-by-stage **result record**: `{module, version, platform,
migrations:[...], idempotent:true, tests:{passed,failed}, health_after, backup_ref,
stage_run_id, finished_at}`.

### 3.3 Stamp (盖章 — auditable, signer may be AI)
An **approver** reviews the result record and **stamps** = signs a **promotion attestation**:
```
attestation = sign(approver_key, canonical({
  module, version, platform, from→to,        -- e.g. stage→beta
  stage_run_id, result_hash, approver, approved_at, notes }))
```
- The signer may be a **human** or an **AI reviewer agent** (its own key + recorded
  reasoning). The stamp must always exist, be signed, and be auditable.
- **AI stamp criteria are real, not a rubber stamp** — it refuses + escalates to a human if
  any fail: all tests green · migration applied + idempotent re-run clean · pg_dump backup
  present · health green across the soak · **no destructive schema change** (`DROP`/`TRUNCATE`/
  type-narrowing) without an explicit human waiver · diff within a risk budget.
- `polar-zone promote --module M --version V --to beta|stable` **verifies the attestation
  signature + that stage passed** before flipping the channel pointer; otherwise it refuses.

### 3.4 Why this shape
The reactive "zone breaks → file issue → dev fixes" loop is a *backstop*, not the safety
net. The stage gate + stamp move the failure left of the customer; canary + rollback (next)
bound the blast radius of anything that slips through.

## 4. Canary rollout & rollback

- **Roles** on `zones`: `stage` (1), `canary` (a few internal/friendly), `general` (rest).
- Promotion to `stable` rolls **canary first**; the agent reports post-deploy health; zen
  **halts the rollout** if any canary goes red within the soak window and **files an issue**.
- **Binary retention**: the agent keeps the **previous N binaries** per module
  (`~/.local/bin/<svc>.vN`). After any OTA it runs a post-deploy health check; **red →
  auto-revert** to the last-good binary + (if a migration ran) flags for human DB review
  (schema rollback is *not* automatic — forward-only discipline, §5).
- A zone may **pin** a known-good version (per-zone pin overrides channel head); customers
  default to pinned/stable, internal zones to channel head.

## 5. Migrations & data safety

- **Backup before migrate, always** ([[feedback-drop-data-ok-backup-first]]): `deploy.sh`
  takes a `pg_dump` per target DB before applying any migration. No backup ⇒ no migrate.
- **Forward-only + idempotent**: migrations use `IF NOT EXISTS`/guards, never blind `DROP`.
  Down-migrations are not auto-run (the stage gate + backup are the safety, not rollback SQL).
- **Schema version tracking** per zone DB (`schema_versions(module, version, applied_at)`),
  surfaced in `/zone/health` so drift between code and schema is visible.
- **Destructive changes require a waiver**: a migration that drops/narrows is blocked by the
  stamp criteria unless a human explicitly waives it (recorded in the attestation).

## 6. Components

### 6.1 zen control plane — `polar-zone` module (svc + DB + admin UI on zen)
Owns: release registry + **channels & promotion attestations**; **zones** registry +
lifecycle; **licenses/entitlements**; **health board**; **issue intake**; **manifest
service** (per-zone resolved module→version from channel+entitlement+pin); **discovery**
endpoint; **IdP** (see §10).

### 6.2 zone agent — `polar-zone agent` (daemon on the zone box)
enroll → reconcile loop (pull license+manifest → `drift` diff → OTA entitled+out-of-date →
**post-deploy health check + rollback** → enforce license) → serve `/zone/health` + push
health → watchdog files issues. Runs the fleet `deploy.sh` under the hood.

### 6.3 dock changes (per zone)
license gate on plugin activation (+ nav filter); public `/api/zone/health` (P1a, shipped);
schema-version reporting; SSO client to zen IdP.

### 6.4 release registry — `polar-release` (exists)
Add **channel pointers** + **promotion attestation** storage + the **refuse-unstamped-promotion**
check. A zone box needs a polar-assets provider for local round-trips; until then the fleet
pipeline rsyncs binaries directly.

## 7. Data model (zen `polar_zone` DB) — deltas over v1

```sql
zones( id, name, base_url, region, visibility, channel,
       role TEXT,                 -- stage|canary|general   (NEW: drives rollout order)
       pinned_versions JSONB,     -- module→version pin override (NEW)
       zone_pubkey, status, last_seen_at, last_health JSONB, created_at )

promotion_attestations(           -- the "stamp" (NEW)
  id, module, version, platform, from_channel, to_channel,
  stage_run_id, result_hash, approver, approver_kind,  -- human|ai
  signature, approved_at, notes )

stage_runs(                       -- stage test results (NEW)
  id, module, version, platform, migrations JSONB, idempotent BOOL,
  tests JSONB, health_after JSONB, backup_ref, status, finished_at )

zone_entitlements( ... )          -- as v1: module, valid_from/until, seats, quota, status
zone_health( zone_id, module, healthy, version, db_ok, schema_version, detail, observed_at )
zone_issues( ... )                -- as v1
```

## 8. Health & public exposure — correctness, not just liveness

`GET /api/zone/health` (public, secrets-free, cached ~10s). Beyond heartbeat freshness
(P1a) it reports **correctness signals**: `db_ok`, `degraded:[...]` (e.g. `no-ollama`),
`schema_version` vs expected, **cert expiry**, disk headroom, and **version match** vs the
zone's manifest (drift). zen **polls + zones push** (both). The admin **fleet grid** (zones ×
modules) colors green/amber(degraded)/red(down|expired|drift) with version + license
countdown, and an **alert path** (not just a board): red/expiring → notify (DM/webhook),
deduped.

## 9. Licensing / entitlement — with a real threat model

- A purchase = `zone_entitlements` lines (module + window [+ seats/quota]); manual now.
- **Client-side enforcement is friction, not a wall.** A customer-hosted zone runs on the
  customer's box with root: they can flip `enabled`, set the clock back, or block revocation
  polls. dock + plugin self-checks raise the bar (honor system + audit) but don't stop a
  determined operator.
- **The real lever is server-side**: gate the *valuable, centralized* capabilities at zen —
  **the LLM proxy**, **the update/release source**, **cross-zone features**, **the IdP**.
  Stop re-issuing the license ⇒ short-TTL license expires ⇒ updates stop, proxy stops,
  federated login stops. That bites even on hardware you don't control.
- **Two threat models, stated explicitly**: *hosted* zones (we run them — full control,
  client checks suffice) vs *customer-hosted* zones (adversarial — rely on server-side
  levers + contracts, treat client checks as tamper-evident only).

## 10. Identity federation (not per-zone accounts)

Reuse the dock **OAuth2/OIDC provider** ([[project-oauth-provider]]): **zen is the IdP**,
each zone's dock is an **SSO relying party**. One identity across all zones; the app picks an
endpoint but logs in once against zen. This matches the multi-site app expectation and avoids
fragmented per-zone accounts. **Discovery is signed** (`sign(zen, zone_list)`) and lists only
vetted, `visibility=public` zones; the app verifies the signature + pins TLS to mitigate
phishing/MITM. Private/internal zones are unlisted (direct URL).

## 11. Key management

Four distinct keys (`release_key`, `license_key`, `promotion_key`/per-approver, `zone_key`),
rotated independently. The high-value ones (`release_key`, `license_key`) are the fleet's
crown jewels — **compromise = supply-chain takeover or forged licenses** — so keep them
**off the always-online zen path** (offline/HSM signing; zen serves already-signed
artifacts). AI approver keys are scoped to stamping and revocable. `/api/zone/health` and
discovery leak no secrets by construction.

## 12. dock ↔ plugin compatibility

Each plugin binary declares `min_dock` (and the sdk version it built against); the manifest
service **won't offer a plugin update a zone's dock can't satisfy**, and dock advertises its
version so a plugin refuses to run against an incompatible core. Promotion is **coordinated**:
a breaking dock change + dependent plugin updates promote together (a bundle), not
independently — preventing the "plugin ran ahead of dock" contract break.

## 13. Zone vs workspace — tenancy rule

Polar already has **workspaces/teams** for multi-tenancy *inside* a deployment. A **zone** is
a *separate deployment* (own box/Postgres/ops). Rule of thumb:
- **Workspace** = default tenancy: many customers share a hosted zone, isolated by team. Use
  for SaaS/self-serve.
- **Zone** = data residency, air-gap, dedicated SLA, or customer-hosted requirements: one
  tenant gets their own deployment. Use for enterprise/on-prem/regulated.
Don't nest the two as a product knob; pick per customer at sale time.

## 14. Lifecycles (updated)

Open (admin grants entitlements → enrollment token) → bootstrap (`polar-zone enroll`) →
**steady state**: agent reconciles against its channel, but only ever sees **stamped**
versions; OTA is health-gated with rollback. Dev loop: fix on `main` → publish `dev` →
**stage test → stamp** → canary → stable. App access: signed discovery → endpoint pick →
**SSO via zen** → entitled modules only.

## 15. Phasing (revised — gate before any auto-OTA)

- **P0 (done)** — release registry; polar-zone CLI; `scripts/fleet/`; everest by hand.
- **P1a (done, PR #447)** — public `/api/zone/health`.
- **P1b** — zone registry + health board (poll+push) + fleet grid. Read-only visibility.
- **P2 — promotion gate FIRST**: channels + `stage_runs` + `promotion_attestations` +
  registry refuse-unstamped + a stage zone + the AI/human stamp flow. *No auto-OTA ships
  before this exists* — it's the safety precondition.
- **P3 — agent reconcile + canary rollout + rollback** (consumes stamped channels only).
- **P4 — entitlement + signed license + dock gate + server-side levers.**
- **P5 — auto issue filing → internal board (+ GitHub mirror opt-in).**
- **P6 — federated identity (zen IdP) + signed discovery + app endpoint picker.**
- **P7 — per-zone assets + metered/self-serve billing.**

## 16. Open decisions
1. **Stage data**: synthetic seed vs sanitized prod snapshot. Recommend **synthetic seed**
   (no PII leaves a zone) + a per-module migration fixture.
2. **Soak windows**: how long canary holds before `stable`. Recommend **start 30–60 min,
   tune per module risk.**
3. **AI stamp autonomy**: auto-promote on green, or always queue for human ack on
   `stable`. Recommend **AI auto to `beta`; human (or AI-with-higher-bar) for `stable`.**

## See also
[[project-zone-federation-platform]] · [[project-zone-release-system]] · [[project-everest-zone]]
· [[project-oauth-provider]] · [[feedback-full-deploy-prebuilt-from-main]] ·
[[feedback-drop-data-ok-backup-first]] · `polar-zone/scripts/fleet/`
