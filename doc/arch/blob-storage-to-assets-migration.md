# Migrating module blob storage → polar-assets

**Status:** in progress (2026-06-03). wg = pilot.

## Goal

Several plugin modules still store binary blobs on their own service's
local disk (`<UploadDir>/<kind>/<sha2>/<sha>`, recorded as a `file://`
URI or relative path in their Postgres tables). Move them all to the
central **polar-assets** module (content-addressed, signed-PUT upload +
signed download-URL, multi-region cache, quota/billing) via the
**polar-sdk Asset client** — the same path `polar-release` already uses.

## Scope (surveyed 2026-06-03)

| Module | Blobs | Local path | DB ref | Owner |
|---|---|---|---|---|
| **polar-wg** (pilot) | wg-mac bundles | `wg-svc-data/wg-bundles/<sha2>/<sha>` | `wg_bundles.blob_uri` (file://) | platform |
| polar-library | firmware (iboot/wifi/sep) | `<UploadDir>/firmwares/<sha2>/<sha>` | `rev_firmwares.blob_uri` | platform |
| polar-expense | receipt images | `<UploadDir>/expense-images/<sha2>/<sha>` | `expenses.raw_image_id` (rel path) | **tenant** |
| polar-iosdist | IPA + .p12 + .mobileprovision + icons | `AttachmentStorage` (local or R2) | `iosdist_versions.ipa_url` etc. | platform |
| polar-video | shot mp4 + poster + render output + audio | `<BlobDir>/*.mp4` (chatStorage) | `video_shots.video_url` etc. | tenant |

**No blobs (skip):** packtunnel, projects, hosts, latch — they declare a
`BlobDir` for parity but have zero blob I/O.

## SDK API (polar-sdk/extras.go)

- `AssetUpload(input AssetUploadInput, body io.Reader) (*AssetMeta, error)` —
  spools+hashes the reader, POSTs `/internal/v1/assets/upload-grant`,
  PUTs bytes direct to the provider, POSTs `/finalize`. Returns
  `AssetMeta{ID int64, SHA256, SizeBytes, ...}`. `WorkspaceID=nil` →
  platform-owned (no tenant quota); `&ws` → tenant-owned.
- `AssetDownload(ref *AssetMeta) (*http.Response, error)` — needs
  `ref.ID`; tries `/assets/<id>/download-url` (signed provider GET) then
  falls back to `/assets/<id>/blob` (dock-proxied). Caller streams +
  closes the body.

## Per-module pattern (single-write to assets)

Decision (user, 2026-06-03): **single-write — all bytes go to the asset
platform; no local copy is kept.** The local disk is only a transitional
*read* source for old rows until they're migrated, then it's gone.

1. **Schema** — add `asset_id BIGINT` (nullable) next to the existing
   `blob_uri`/url column. Idempotent boot `ALTER TABLE … ADD COLUMN IF
   NOT EXISTS asset_id BIGINT`.
2. **Write = single-write to assets** — on upload, stream the body
   straight to `AssetUpload(...)` (no local file). Use the returned
   `AssetMeta.{ID, SHA256, SizeBytes}` for the row; set `asset_id` and a
   marker `blob_uri = "asset://<id>"`. The catalog `Name` is unique per
   item (e.g. the version); bytes dedup by sha inside assets.
3. **Read** — `asset_id > 0` → `AssetDownload(&AssetMeta{ID})` and stream.
   Legacy rows (`asset_id IS NULL`, old `file://` blob_uri) read from
   local disk **only until backfilled** — the migration bridge, removed
   at cutover.
4. **Backfill = the data migration** — admin endpoint `POST
   …/backfill-assets`: for every row with `asset_id IS NULL` and a
   resolvable local blob, `AssetUpload` the file, set `asset_id`.
   Idempotent. Run once per module right after deploy.
5. **Cutover (final, per module)** — after backfill verified, every row
   has `asset_id`; the local-read fallback is removed and the local blob
   directory deleted. Local storage no longer exists.

Kind/visibility per module: platform blobs (wg/library/iosdist) →
`Kind:"package"`, `WorkspaceID:nil`, `Visibility:"public"` for
public-download ones (wg bundles, IPAs) else `"private"`. Tenant blobs
(expense/video) → `Kind:"media"`, `WorkspaceID:&ws`, `Visibility:"private"`.

## Order

wg (pilot, proves pattern) → library (near-identical to wg) → expense
(tenant + relative-path schema tweak) → iosdist (AttachmentStorage
swap) → video (chatStorage swap, largest). One PR per module; each
deployed + backfilled + verified before the next.

## Rollback

Each step is additive (new column, dual-write, dual-read). Revert = stop
reading `asset_id` (local blobs untouched until the explicit cutover
PR). The backfill never deletes anything.
