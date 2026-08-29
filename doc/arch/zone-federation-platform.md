# Zone Federation Platform — design

**Status:** design (2026-06-14). Builds on **polar-release** (signed binary registry),
**polar-zone** (cut/publish/bootstrap/drift CLI), and `polar-zone/scripts/fleet/`
(build-from-main → test → deploy pipeline, proven standing up the everest zone).

## 1. One-liner

zen becomes a **control plane** that opens, licenses, and watches a fleet of **zones**
(self-contained Polar replicas). A zone pulls only the modules it is **entitled** to
(a "purchase"), auto-deploys signed binaries built from `main`, publishes its **health**
publicly, and **files issues back to zen** when something breaks. End-user iOS/Android
apps discover zones from a public **config endpoint**, pick an endpoint, and register/login —
reusing the existing multi-site app shell.

```
 developer ──push──▶ polar-release (zen)            ┌────────── zone: everest ──────────┐
                         │  signed binaries          │ polar-zone agent                  │
 zen admin ──open/license──▶ polar-zone (zen) ◀──────┤  • pull license + manifest        │
                         │  zones · licenses · health │  • OTA deploy entitled modules    │
                         │  health board · issue intake│  • enforce license               │
 iOS/Android ──discover──▶ /zones/discover           │  • push health / file issues      │
                                                      │ dock + plugins (entitled only)    │
                                                      │ GET /zone/health  (public)        │
                                                      └───────────────────────────────────┘
```

## 2. Actors & trust

| Actor | Holds | Trust anchor |
|---|---|---|
| **zen control plane** (`polar-zone` module) | zone registry, license authority, health board, issue intake | `zen_license_key` (ed25519) signs licenses; `release_key` signs binaries |
| **zone** (everest, opera, customer site) | dock + entitled plugins, `polar-zone` agent | `zone_key` (ed25519) generated at bootstrap; pubkey approved by zen ("开zone") |
| **developer** | publishes binaries to release | release admin token |
| **end-user app** | endpoint chosen from discovery | zone's dock auth (cookie/Bearer) |

Three independent signature chains, never conflated:
- **Binaries**: signed by `release_key`, verified by the zone agent before install (already in polar-release/sdk).
- **Licenses**: signed by `zen_license_key`, verified by the zone's dock offline + by zen online.
- **Zone identity**: `zone_key` authenticates the zone's calls to zen (health push, issue filing, license/manifest pull). zen approving the pubkey **is** the act of opening a zone.

## 3. Components

### 3.1 zen control plane — `polar-zone` module (on zen dock)
New plugin module (repo `networkextension/polar-zone` already exists; today CLI-only).
Adds a svc + DB + admin UI. Owns:
- **zones** registry + lifecycle (open / suspend / revoke).
- **licenses / entitlements** — manual grant of module set + time window per zone.
- **health board** — aggregates each zone's public health; per-zone × per-module grid.
- **issue intake** — receives zone-filed issues; mirrors to GitHub / internal tickets.
- **manifest service** — computes, per zone, the *resolved* module→version set it should run (channel + entitlement), as a signed bundle the agent pulls.
- **discovery endpoint** — public list of zones/endpoints for apps.

### 3.2 zone agent — `polar-zone` (long-running, on the zone box)
Extend the existing CLI (cut/publish/bootstrap/drift) into a daemon (`polar-zone agent`):
- **enroll** once (token → registers `zone_key` pubkey with zen).
- **reconcile loop** (default 60–300s): pull license + resolved manifest → diff vs running (`drift`) → for entitled+out-of-date modules, pull signed binary from release, verify, deploy via the fleet `deploy.sh` recipe → enforce license (disable expired/unentitled).
- **health push** + serve the public `/zone/health`.
- **watchdog** → file issues to zen on anomalies.
Runs the fleet scripts under the hood; those scripts stay the source of deploy truth.

### 3.3 dock changes (per zone)
- **license gate**: on plugin activation, dock cross-checks the entitlements blob; un-entitled/expired module → `plugin_modules.enabled=false` + nav hides it.
- **public health**: `GET /zone/health` (unauth, rate-limited) aggregating dock + each plugin `/healthz` + license summary + drift.
- **entitlement to UI**: `/api/nav` already self-registers; filter by entitled set so apps only see purchased modules.

### 3.4 release registry — `polar-release` (exists)
No change to the core. A zone box needs a **polar-assets provider** to hold artifact
bytes before `publish`/`cut` round-trips work *on that zone*; the seed (zen) already has
assets, so for now zones consume from zen's release/assets, and the fleet pipeline
rsyncs binaries directly until per-zone assets is stood up.

### 3.5 apps — discovery + endpoint picker
Public `GET https://zen.4950.store/zones/discover` → JSON of selectable endpoints
(name, base_url, region, visibility, modules, login methods). iOS/Android (polar-sdk-swift /
the ShangDynasty multi-site shell) render a picker → user selects → register/login against
that zone's dock. One app binary, many endpoints — the multi-site work pays off here.

## 4. Data model (zen `polar_zone` DB)

```sql
zones(
  id TEXT PK,                  -- zn_<rand>
  name TEXT,                   -- "everest"
  base_url TEXT,               -- https://dock.everest.4950.store  (or :2443)
  region TEXT, visibility TEXT,-- public|private|internal  (drives discovery)
  channel TEXT,                -- dev|beta|stable  (which release channel to track)
  zone_pubkey TEXT,            -- ed25519, approved at open
  status TEXT,                 -- pending|active|suspended|revoked
  last_seen_at TIMESTAMPTZ, last_health JSONB,
  created_at TIMESTAMPTZ)

zone_entitlements(            -- the "purchase" / license line items
  id BIGSERIAL PK,
  zone_id TEXT REFERENCES zones,
  module TEXT,                 -- 'lawyer'  (or '*' = whole fleet)
  valid_from TIMESTAMPTZ, valid_until TIMESTAMPTZ,
  seats INT NULL, quota JSONB, -- optional limits
  status TEXT,                 -- active|expired|revoked
  granted_by TEXT, note TEXT, created_at TIMESTAMPTZ)

zone_health(                  -- time series of pushed/polled health
  zone_id TEXT, module TEXT, healthy BOOL, version TEXT,
  db_ok BOOL, detail JSONB, observed_at TIMESTAMPTZ)

zone_issues(
  id BIGSERIAL PK, zone_id TEXT, module TEXT, kind TEXT,  -- crashloop|migrate|health|drift|license
  signature TEXT,             -- dedupe key
  status TEXT,                -- open|ack|fixed|closed
  github_url TEXT, detail JSONB, first_seen TIMESTAMPTZ, last_seen TIMESTAMPTZ)
```

A **license** delivered to a zone is the signed projection of its `zone_entitlements`:
```
license = ed25519_sign(zen_license_key, canonical({
  zone_id, issued_at, not_after,            -- short TTL (e.g. 24–72h) → forces re-pull, enables revoke
  modules: [{name, valid_until, seats}],
}))
```
Short-TTL online license + each line's own `valid_until` = revocation works (stop re-issuing)
while a zone tolerates being offline up to its line `valid_until`.

## 5. Lifecycles

### 5.1 Open a zone (zen admin, manual)
1. Admin creates a `zones` row (name, base_url, channel, visibility) → gets an **enrollment token**.
2. Admin grants `zone_entitlements` (modules + time window) — this is the "购买/授权".
3. Hands operator the one-line bootstrap: `polar-zone enroll --token <T> --server zen`.

### 5.2 Bootstrap (on the bare box)
`polar-zone enroll` → generates `zone_key`, registers pubkey (token-authed) → zen flips
zone `pending→active` → agent pulls license + resolved manifest → pulls+verifies signed
binaries → runs `deploy.sh` for entitled modules → reports first health.

### 5.3 Steady state (agent reconcile loop)
pull license (enforce) → pull manifest → `drift` diff → OTA update changed modules
(respect a maintenance window) → push health → watchdog files issues.

### 5.4 Developer fix loop
dev finds/receives an issue → fixes on `main` → CI builds + `publish` to channel →
channel manifest bumps → zones on that channel auto-update next reconcile. **All deploys
are prebuilt-from-main binaries** ([[feedback-full-deploy-prebuilt-from-main]]).

### 5.5 App access
app → `/zones/discover` → picker → register/login at chosen zone's dock → sees only
entitled modules (nav filtered) → uses them.

## 6. Health & public exposure

`GET /zone/health` (public, cached ~10s):
```json
{ "zone":"everest","dock":{"version":"b9b9f2f","healthy":true},
  "modules":[{"name":"lawyer","version":"10b6ee7","healthy":true,"db_ok":true,
              "entitled_until":"2026-12-31T00:00:00Z","degraded":["no-ollama"]}],
  "license":{"not_after":"...","modules":12}, "drift":[], "ts":"..." }
```
- zen **polls** every zone + zones also **push** (belt & suspenders) → `zone_health`.
- Admin **fleet board**: zones (rows) × modules (cols) grid, green/amber/red, version, drift,
  license countdown. Amber = degraded (e.g. RAG without ollama); red = down/crashloop/expired.

## 7. Auto issue filing

Agent watchdog detects, dedupes by `signature`, and `POST /zone/issues`:
- module down/crashloop ≥N, migration error, health red > T, **version drift** (running ≠
  manifest), **license near-expiry**. zen creates a GitHub issue (labels `zone:<name>`,
  `module:<m>`, `kind:<k>`) or internal ticket, links it on the health board, and
  auto-closes when the zone reports recovery. Rate-limited + deduped so one flapping
  module ≠ 100 issues.

## 8. Licensing / entitlement enforcement (the "购买" model)

- A purchase = one or more `zone_entitlements` lines (module + window [+ seats/quota]).
  Manual now (admin grant/extend/revoke); billing/self-serve later.
- **Enforced in two places** (defense in depth):
  1. **dock** disables `plugin_modules.enabled` for un-entitled/expired modules and hides
     them from `/api/nav`.
  2. **plugin svc** on startup asks dock "am I entitled?" → if no, refuses to serve / runs
     read-only. (Mirrors the existing plugin `/ping` gate.)
- **Offline tolerance**: zone trusts the signed license until each line's `valid_until`,
  re-validating opportunistically. Revocation = zen stops re-issuing + publishes a
  revocation list the agent polls.

## 9. Security notes
- Enrollment token is single-use, short-TTL; the durable trust is `zone_key` pubkey
  approval. Health-push, issue-filing, license/manifest-pull all signed by `zone_key`.
- Public `/zone/health` must leak **no secrets** — versions + booleans + degraded tags only.
- License key + release key are distinct and rotate independently.
- Discovery only lists `visibility=public` zones; internal/private zones are unlisted
  (apps reach them by direct URL).

## 10. Phasing

- **P0 (done)** — polar-release registry; polar-zone CLI (cut/publish/bootstrap/drift);
  `scripts/fleet/` build-from-main→test→deploy; everest = 15-plugin zone stood up by hand.
- **P1 — zone registry + health board (read-only).** zones table; zones expose
  `/zone/health`; zen polls + admin grid UI. No entitlement yet (all-modules). Highest
  value: visibility across everest/opera/zen.
- **P2 — entitlement + license.** zone_entitlements + signed license; dock license gate +
  nav filter; admin grant/revoke UI. Turns zones into licensed products.
- **P3 — agent reconcile + OTA.** `polar-zone agent` daemon: pull manifest, drift, auto-
  deploy from release on a channel; maintenance windows. Removes manual deploy.
- **P4 — auto issue filing.** watchdog → zen issue intake → GitHub mirror + board linkage.
- **P5 — app discovery.** `/zones/discover` + iOS/Android endpoint picker on the multi-site
  shell; entitled-module-aware UI.
- **P6 — per-zone assets + self-serve billing.** assets provider on each zone (full release
  round-trip locally); purchase flow replaces manual grants.

## 11. Open decisions
1. **License delivery**: pull-only (agent fetches) vs. also embed a signed blob in the zone
   bundle. Recommend **pull-only** with short TTL (simplest revoke).
2. **Issue sink**: GitHub issues vs. internal `zone_issues` only vs. both. Recommend
   **internal first, GitHub mirror opt-in per zone**.
3. **Manifest authority**: does zen pin exact versions per zone, or does the zone track a
   channel head? Recommend **channel head by default, per-zone pin override** for customers.
4. **Health transport**: poll vs push vs both. Recommend **both** (poll for liveness even if
   a zone can't reach out; push for timeliness).

## See also
[[project-zone-release-system]] · [[project-everest-zone]] · [[project-zone-federation-platform]]
· `polar-zone/scripts/fleet/` · [[feedback-full-deploy-prebuilt-from-main]]
