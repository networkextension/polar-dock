# Polar Assets Module — Design

**Status:** design, pre-implementation
**Date:** 2026-05-28 (rev 2 — multi-tenant + migration)
**Owner:** Kong XiangBo

## Goals

A **single unified file/object store** for the whole platform, used by:

1. **Platform internals** — LLM weights, polar-agent binaries, wg-mac
   installers, library blobs. (Original v1 scope.)
2. **Tenant apps as backing store** — workspaces call our API from
   their own code: PUT/GET/DELETE objects. Closest analog: S3 as a
   product offering.
3. **Tenant share + distribute** — uploader gets a short link, sends
   to recipients (logged-in or anonymous). Closest analog: WeTransfer.
4. **Tenant internal team share** — workspace members upload/browse
   together; folders, paths. Closest analog: Dropbox for teams.
5. **Migration target for existing media** — post images, markdown
   attachments, video sources, video module outputs all move here
   for centralized management instead of ad-hoc per-module storage.

## Non-goals (v1)

- **No money mechanics yet.** Per operator direction: stats only.
  Record `usage_events` rows; defer the "convert stats to invoice"
  path until product is ready to monetize.
- **No real folder semantics.** Use **path-prefix in the asset name**
  (e.g. `name = "projects/q3/draft.mp4"`); UI fakes folder nav via
  prefix grouping. A real `asset_folders` table with mv/mkdir is v2.
- **No versioning.** `(workspace_id, name)` is unique. Re-uploading
  the same name overwrites; we keep a single audit row in
  `asset_usage_events`. (Real version history is v2 when team-share
  users ask.)
- **No auto-fetch from upstream URLs.** Ingestion remains operator-
  or tenant-driven. `source_url` is provenance only. (.57 network
  block still applies.)
- **No resumable uploads >5 GB.** Single PUT in v1; tus.io comes later.

## Architecture

Unchanged from rev 1 — dock = catalog + 302 router, providers serve
bytes:

```
┌─────────────────────────────────────────────────────────────┐
│ polar-dock                                                  │
│  catalog: assets, asset_providers, asset_locations,         │
│           asset_quotas, asset_share_links, usage_events     │
│  router: GET /api/assets/...      → 302 → provider URL      │
│          GET /s/<short_code>      → 302 → provider URL      │
│  upload, ACL, quota enforce, audit                          │
└──────────────┬──────────────────────────────────────────────┘
               │ 302 (signed: sha256+exp+sig)
               ▼
┌──────────────────────┐  warm-pull on miss  ┌──────────────┐
│ polar-assets-svc     │ ◄────────────────► │ peer         │
│  (provider plugin)   │                      │ provider     │
│  GET /blob/<sha256>  │                      │ (other site) │
│  POST /pull?src=...  │                      └──────────────┘
│  POST /receive       │
│  local cache + LRU   │
└──────────────────────┘
       ▲
       │
   plugin / agent / tenant app / anonymous link-share recipient
```

Topology unchanged: **one dedicated provider per region** (zen + dev).

## Data model

All tables in `ideamesh` Postgres.

### Catalog

```sql
CREATE TABLE assets (
  id           BIGSERIAL PRIMARY KEY,
  workspace_id TEXT REFERENCES teams(id) ON DELETE CASCADE,   -- NULL = platform
  kind         TEXT NOT NULL,            -- 'model'|'package'|'lib'|'media'|'app_data'
  name         TEXT NOT NULL,            -- path-like: "videos/q3/clip01.mp4"
  version      TEXT NOT NULL DEFAULT 'v1',
  sha256       TEXT NOT NULL,
  size_bytes   BIGINT NOT NULL,
  mime         TEXT NOT NULL DEFAULT 'application/octet-stream',
  visibility   TEXT NOT NULL DEFAULT 'private',  -- 'private'|'workspace'|'link'|'public'
  source_url   TEXT,                     -- provenance
  uploaded_by  TEXT NOT NULL,            -- user_id (or 'system' for platform)
  pinned       BOOLEAN NOT NULL DEFAULT false,
  metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uniq_asset_ws_kind_name_version
    UNIQUE NULLS NOT DISTINCT (workspace_id, kind, name, version)
);
CREATE INDEX idx_assets_sha256 ON assets(sha256);
CREATE INDEX idx_assets_workspace_kind ON assets(workspace_id, kind, created_at DESC);
CREATE INDEX idx_assets_kind_created ON assets(kind, created_at DESC) WHERE workspace_id IS NULL;
-- prefix queries for folder-fake UI:
CREATE INDEX idx_assets_workspace_name_prefix ON assets(workspace_id, name text_pattern_ops);
```

`visibility` decides who can fetch via the catalog router:
| value | who can read |
|---|---|
| `private` | only the uploader |
| `workspace` | any member of `workspace_id` |
| `link` | anyone holding a valid `asset_share_links` row |
| `public` | anyone (no auth) |

`kind` widens to include `media` (post images, markdown attachments,
video sources/outputs) and `app_data` (generic tenant bucket use).

### Providers + locations (unchanged from rev 1)

```sql
CREATE TABLE asset_providers (
  id            BIGSERIAL PRIMARY KEY,
  slug          TEXT NOT NULL UNIQUE,
  base_url      TEXT NOT NULL,
  region        TEXT NOT NULL,
  capacity_gb   INT NOT NULL,
  hmac_token    TEXT NOT NULL,
  last_seen_at  TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE asset_locations (
  asset_id     BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  provider_id  BIGINT NOT NULL REFERENCES asset_providers(id) ON DELETE CASCADE,
  status       TEXT NOT NULL,            -- 'ready'|'fetching'|'missing'
  last_used_at TIMESTAMPTZ,
  bytes_served BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (asset_id, provider_id)
);
```

### Share links (link-share + WeTransfer use case)

```sql
CREATE TABLE asset_share_links (
  id             BIGSERIAL PRIMARY KEY,
  asset_id       BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  short_code     TEXT NOT NULL UNIQUE,   -- 8–12 char base62; URL is /s/<code>
  created_by     TEXT NOT NULL,          -- user_id
  expires_at     TIMESTAMPTZ,            -- NULL = never
  max_downloads  INT,                    -- NULL = unlimited
  download_count INT NOT NULL DEFAULT 0,
  require_login  BOOLEAN NOT NULL DEFAULT false,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_share_links_asset ON asset_share_links(asset_id);
```

Public URL pattern: `https://zen.4950.store/s/<short_code>` →
dock validates (not expired, not over `max_downloads`, login if
required) → 302 to provider with a short-lived signed URL.

### Quotas

```sql
CREATE TABLE asset_quotas (
  workspace_id   TEXT PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
  quota_bytes    BIGINT NOT NULL,        -- hard cap
  usage_bytes    BIGINT NOT NULL DEFAULT 0,  -- last computed value
  usage_computed_at TIMESTAMPTZ,
  over_quota     BOOLEAN NOT NULL DEFAULT false, -- set true on shrink-below-usage
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

**Root quota** lives in `site_config` (key/value table or
`POLAR_ASSETS_ROOT_QUOTA_BYTES` env override). Format:

```
asset_root_quota_mode = 'absolute'  | 'partition_pct'
asset_root_quota_value = '419430400000'   (400 GB)  or '80' (=80% of partition)
asset_workspace_default_quota_bytes = '53687091200'  (50 GB)
```

Resolve order on quota check:
1. Read workspace's `asset_quotas.quota_bytes` (use if present).
2. Else fall back to `asset_workspace_default_quota_bytes`.
3. Root quota is checked separately on *every* upload: sum of all
   `asset_quotas.usage_bytes` + size of new asset must be ≤ root
   quota. Otherwise reject with "platform out of capacity, contact
   root admin."

### Usage events (stats-only; future-billing pipe)

```sql
CREATE TABLE asset_usage_events (
  id           BIGSERIAL PRIMARY KEY,
  workspace_id TEXT,                       -- NULL for platform-owned
  asset_id     BIGINT REFERENCES assets(id) ON DELETE SET NULL,
  op           TEXT NOT NULL,              -- 'upload'|'download'|'delete'|'share_create'
  bytes        BIGINT NOT NULL DEFAULT 0,  -- transferred bytes for upload/download
  user_id      TEXT,                       -- NULL for anonymous link-share download
  share_link_id BIGINT REFERENCES asset_share_links(id) ON DELETE SET NULL,
  ip           TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_asset_usage_ws_created ON asset_usage_events(workspace_id, created_at DESC);
CREATE INDEX idx_asset_usage_op_created ON asset_usage_events(op, created_at DESC);
```

Aggregates (storage GB-month, monthly egress GB) computed on read
from these rows + asset.size_bytes. No materialized billing tables
in v1 — we can add `asset_billing_monthly` later if perf requires.

## Quota enforcement

**On upload (workspace asset):**

```
size := len(body)
ws_quota := lookupWorkspaceQuota(ws_id)
if ws_quota.over_quota { reject 413 "over quota; delete files first" }
if ws_quota.usage_bytes + size > ws_quota.quota_bytes { reject 413 }
if sumAllWorkspaceUsage() + size > rootQuotaBytes() { reject 413 "platform full" }
... proceed with upload ...
on success: UPDATE asset_quotas SET usage_bytes = usage_bytes + $size
```

**On root admin shrinks a workspace quota:**

```
admin PATCH /api/admin/asset-quotas/:ws { quota_bytes: 10GB }
if new_quota < current usage_bytes:
   set over_quota = true
   return 200 with warning: "workspace currently uses 14GB; new cap 10GB.
                              uploads disabled. workspace must delete 4GB."
else:
   set over_quota = false
   normal save
```

UI on the tenant side shows a red banner when `over_quota=true`:
"Storage cap reduced. Please delete N GB to re-enable uploads."

**Background recompute (every 6h):**

A sweeper job recomputes `usage_bytes` per workspace from
`SUM(size_bytes) WHERE workspace_id = $1`, refreshes
`usage_computed_at`. Catches drift if any UPDATE path missed an
increment.

## Visibility / ACL matrix

| visibility | catalog list | get blob | who |
|---|---|---|---|
| `private` | uploader sees | uploader only | uploader |
| `workspace` | workspace members | workspace members | members of `workspace_id` |
| `link` | uploader sees | anyone holding short-code | uploader + link recipients |
| `public` | publicly listable via opt-in endpoint | anyone | world |

Platform assets (`workspace_id IS NULL`) are always
visibility=`workspace` but workspace check is "is site admin" or "is
member of root team" depending on `kind`.

## API surface

### Catalog (dock)

```
# tenant + platform
POST   /api/assets                       upload (multipart): meta + blob
GET    /api/assets                       list, filter: kind, name_prefix, workspace
GET    /api/assets/:id                   row
GET    /api/assets/:id/blob              302 → provider (signed, sha256+exp)
GET    /api/assets/by-name/:kind/*name   convenience lookup
PATCH  /api/assets/:id                   rename, visibility, metadata
DELETE /api/assets/:id                   delete (cascade locations + share links)

# folder-fake helpers (path prefix)
GET    /api/assets/folders?workspace=X&prefix=foo/   distinct path-prefix children

# share links
POST   /api/assets/:id/share             { expires_at, max_downloads, require_login }
GET    /api/share-links                  caller's links
DELETE /api/share-links/:id              revoke
GET    /s/:short_code                    public; 302 → provider blob (if valid)

# quotas (admin)
GET    /api/admin/asset-quotas           list workspaces + usage
PATCH  /api/admin/asset-quotas/:ws       set quota_bytes
GET    /api/asset-quota                  caller's own workspace quota

# stats
GET    /api/admin/asset-usage            aggregated; admin-only
GET    /api/asset-usage                  caller's workspace stats
```

### Provider internal (HMAC by dock)

```
GET    /blob/:sha256?token=<signed-url-token>     stream blob
POST   /pull?src=<peer-base-url>&sha256=<hex>     warm-pull from peer
POST   /receive                                   accept upload
GET    /stats                                     used_gb, item_count, top-by-bytes-served
POST   /evict/:sha256                             admin force-evict
```

### SDK (polar-sdk)

```go
client.Assets.Upload(ctx, asset, blobReader) (*Asset, error)
client.Assets.Get(ctx, workspace, kind, name) (*AssetRef, error)
client.Assets.Download(ctx, ref, destPath) error
client.Assets.Share(ctx, id, opts) (*ShareLink, error)
client.Assets.Delete(ctx, id) error
client.Assets.Stat(ctx, sha256) (*AssetMeta, error)
```

## Migration plan — existing media → asset store

Four modules currently store media outside this store; each gets a
small one-shot migration script + a cutover in the relevant handler.

| source | bytes-scale | target kind | path scheme |
|---|---|---|---|
| post images (chat attachments, profile pics) | KB–MB | `media` | `posts/<post_id>/<orig_filename>` |
| markdown attachments (rooms, docs) | KB–MB | `media` | `markdown/<doc_id>/<orig_filename>` |
| video module: uploaded source clips | MB–GB | `media` | `video/sources/<project_id>/<shot_id>/<orig>` |
| video module: generated outputs | MB–GB | `media` | `video/outputs/<shot_id>/<rendered>` |

**Migration sequence per source:**

1. **Code cutover** — the module writes new uploads to the asset
   store via SDK. Old reads still hit legacy storage.
2. **Backfill script** — walks legacy storage, calls `Assets.Upload`
   with the path scheme above, records a `legacy_path → asset_id`
   mapping in the module's own table.
3. **Read flip** — module's read path checks new store first; falls
   back to legacy path while backfill completes.
4. **Verify** — count rows in legacy vs asset store match.
5. **Legacy delete** — only after step 4 passes for ≥7 days.

Each module gets its own cutover PR; this design doc just defines
the destination shape. (Post + markdown are small files → fast
migrations. Video sources/outputs are big → migrate in batches
ordered by `last_accessed_at` so cold content can stream-then-delete.)

## Upload flow (unchanged from rev 1, plus quota check)

```
operator/tenant                  dock                        provider
       │                          │                                │
       │ POST /api/assets         │                                │
       │ ───────────────────────► │                                │
       │                          │ AUTH + visibility check        │
       │                          │ QUOTA check (workspace + root) │
       │                          │ stream → sha256 → temp         │
       │                          │ INSERT assets (pending)        │
       │                          │ pick primary provider          │
       │                          │ POST /receive (stream)         │
       │                          │ ─────────────────────────────► │
       │                          │                                │ write + verify
       │                          │ INSERT asset_locations         │
       │                          │ INSERT asset_usage_events      │
       │                          │ UPDATE asset_quotas.usage      │
       │ 201 {id, sha256, size}   │                                │
       │ ◄─────────────────────── │                                │
```

## Download flow

Three entry points all converge on "dock issues signed URL, provider
serves":

```
A) GET /api/assets/:id/blob   ← cookie/HMAC auth, ACL check by visibility
B) GET /s/:short_code         ← public; validate share link
C) GET /api/proxy/internal/... ← plugin/agent SDK; HMAC auth

         │
         ▼
  dock: pick best provider (same region first)
  dock: emit asset_usage_events (op='download', size, who)
  dock: sign URL  HMAC(secret, sha256+":"+exp_unix)
  302 → https://assets-zen.4950.store/blob/<sha256>?token=<sig>:<exp>

         │
         ▼
  provider: verify token; check local cache
            hit: stream
            miss: POST /pull?src=<peer> ; stream as bytes arrive
  provider: increment asset_locations.bytes_served + last_used_at
```

## Eviction (unchanged from rev 1)

LRU when `used_gb > capacity_gb * 0.85`, deletes oldest-by-
`last_used_at` evictable (`pinned=false`) blobs until
`used_gb < capacity_gb * 0.7`.

LLM models pinned by default. Tenant uploads pinned by default
(can't evict user data) — so eviction in practice only affects
platform `package` and `lib` artifacts that can be re-uploaded.
If a provider runs out of space with everything pinned, it
rejects new `POST /receive` with 507 → dock falls back to peer
provider; root admin gets an alert.

## PR sequence (revised)

| # | PR | Repo | Depends on |
|---|----|------|------------|
| **P0** | schema: assets + providers + locations + quotas + share_links + usage_events | polar-dock | — |
| **P1a** | dock catalog handlers — list/get/upload/delete (no provider yet, blobs to local dir as stub) | polar-dock | P0 |
| **P1b** | dock quota enforce + root-quota config | polar-dock | P1a |
| **P1c** | dock share-link CRUD + `/s/:code` public route | polar-dock | P1a |
| **P2** | new `polar-assets` repo + `polar-assets-svc` skeleton — `/blob`, `/receive`, `/stats` | networkextension/polar-assets | P0 (schema awareness) |
| **P2b** | polar-sdk `Assets` client + signed-URL helper | polar-sdk | P1a API shape |
| **P3** | dock router 302 + signed URLs; provider verifies | polar-dock + polar-assets | P1*, P2, P2b |
| **P4** | warm-pull on cache miss + peer location lookup | polar-assets | P3 |
| **P5** | LRU eviction sweeper + capacity stats | polar-assets | P2 |
| **P6a** | UI: `/assets.html` — workspace browse, upload, delete, share, quota bar | polar-dock-ui | P1a, P1c |
| **P6b** | UI: `/admin/assets.html` — root admin: quota table, provider health, usage | polar-dock-ui | P1b |
| **P7** | ops: register zen-prov-1 + dev-prov-1, deploy provider plugin | ops | P5 |
| **P8** | media migration: post images → asset store (cutover + backfill) | polar-dock | P3 |
| **P9** | media migration: markdown attachments | polar-dock | P3 |
| **P10** | media migration: video sources | polar-video | P3 |
| **P11** | media migration: video outputs | polar-video | P3 |
| **P12 (later)** | billing rollup tables + UI when product is ready to monetize | polar-dock | P0 (usage_events) |

P0 + P2 + P2b can land in parallel. P3 is the integration milestone
that wires everything together. Media migrations (P8–P11) are
independent and can be sequenced based on operator preference (small
modules first usually safer).

## Open questions (deferred to v2)

- **Real folder semantics** — `asset_folders` table with mv/mkdir.
  Path-prefix in `name` handles the read path; mutation UX is
  awkward (rename moves all matching prefix). Likely needed once
  team-share users hit it.
- **Versioning** — keep N historical versions of `(workspace, name)`
  with `is_current` flag and `replaced_by_id` chain.
- **Resumable uploads** (tus.io) for >5 GB blobs.
- **Per-asset ACLs** finer than `private/workspace/link/public` —
  e.g. "share with these 3 users only."
- **CDN sidecar** for public assets going to external consumers
  (not just internal agents) — could be Cloudflare R2 mirror.
- **Cross-region bandwidth control** (token bucket on warm-pull
  between zen and dev over WG mesh).
- **Billing materialization** — `asset_billing_monthly` table +
  invoicing pipeline when product is ready to charge.
