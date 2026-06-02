# Release / distribution system — design

> Status: design proposal (2026-06-02). Goal: offload all artifact bytes off the
> dock, and turn module install/update into a first-class **publish system**.
> Builds on the existing **assets** (data plane) and **OTA** primitives.

## 1. Motivation

Today the dock is on the byte path for uploads (client → dock → provider), which
loads dock I/O, and module install/update is an ad-hoc OTA (heartbeat carries a
`{version, url}`). We want:

1. **Bytes never traverse the dock** — both upload and download go *directly*
   between the client and the storage **provider** (dock only holds metadata).
2. The dock/registry is a **publish system**: it stores module *info* (versions,
   channels, manifests, signatures), not files.
3. **Module install / update** flows through this publish system uniformly
   (agents, plugins, and the marketplace all resolve releases the same way).

This is an evolution of what exists, not a rewrite.

## 2. What already exists (reuse, don't reinvent)

- **Assets = the data plane.** Content-addressed blobs on **providers**
  (`asset_providers`: hmac secret + region), `asset_locations` (which provider
  holds an asset, ready/warm state), and **download already bypasses the dock** —
  the dock 302-redirects to a *signed provider URL* and bytes flow client↔provider
  (Assets P3). Replication / warm-pull on cache miss exist.
- **OTA = a primitive install/update.** Plugin/agent heartbeat → dock offers
  `{version, url}` → `SelfUpdate` sha-verifies + atomically swaps the binary.
- **plugin_modules** = the module registry + HMAC module auth.

The only thing still on the dock byte path is **upload** (client → dock →
provider).

## 3. The missing primitive: signed-URL **upload** (presigned PUT)

Add to the assets provider the symmetric of its existing signed GET: a **scoped
signed PUT**. The dock/assets mints a URL bound to
`{provider, object_key = sha256, max_size, content_type, expiry}`; the client
PUTs bytes **straight to the provider**, which:

1. verifies the signature (same hmac secret as the download signer),
2. enforces `max_size`,
3. streams to the content-addressed key and **recomputes sha256**, rejecting on
   mismatch (so the key is always the true content hash).

With this, **both upload and download bypass the dock entirely** → dock file I/O
≈ 0.

### Signed-PUT token shape

```
PUT https://<provider>/v1/blob/<sha256>?exp=<unix>&max=<bytes>&ct=<mime>&sig=<hmac>
sig = HMAC-SHA256(provider_secret, "PUT\n<sha256>\n<exp>\n<max>\n<ct>")
```

Scoping rules (provider enforces):
- `sig` binds the **exact object key** → a token can't write any other blob.
- `max` caps the body; `exp` bounds the window (minutes).
- The provider **recomputes** sha and rejects if `body_sha != key`, so even a
  valid token can't store mismatched content.

## 4. The release registry (the "publish system")

A thin control plane over assets. New table (owned by the release module — §6):

```sql
releases (
  id            bigserial primary key,
  module        text not null,                 -- e.g. "buildings", "polar-agent"
  version       text not null,                 -- semver
  channel       text not null,                 -- stable | beta | dev
  platform      text not null,                 -- darwin-arm64 | darwin-amd64 | ios-arm64 | …
  artifact_id   bigint not null references assets(id),  -- content-addressed bytes
  sha256        text not null,                 -- = the asset's content hash
  ed25519_sig   text not null,                 -- signature over the manifest (§5)
  manifest      jsonb not null,                -- {module,version,platform,sha256,size,min_host,notes}
  published_at  timestamptz not null,
  yanked_at     timestamptz                    -- soft-pull a bad release
);
-- resolve index: latest non-yanked per (module, channel, platform)
create index on releases (module, channel, platform, published_at desc) where yanked_at is null;
```

The artifact **is an asset** (content-addressed, stored on a provider, deduped
across versions/providers for free). The release row is just metadata + the
signature.

## 5. Flows (bytes never touch dock or release)

### 5.1 Publish (direct upload)

```
publisher                release                 assets/provider
   │  POST /release/publish {module,version,channel,platform,sha,size}
   │ ───────────────────────►│
   │                         │ auth publisher (dock /internal/v1)
   │                         │ create content-addressed asset(sha)
   │                         │ ask assets → signed PUT URL  ───────────►│
   │  ◄─ {asset_id, put_url} ─│◄──────────── put_url ────────────────────│
   │  PUT bytes ───────────────────────────────────────────────────────►│  (provider verifies sig+size, recomputes sha)
   │  POST /release/finalize {asset_id}
   │ ───────────────────────►│ mark asset ready; ed25519-sign manifest;
   │                         │ insert releases row (published)
   │  ◄─ {release} ──────────│
```

### 5.2 Install / update (direct download)

```
client (agent / plugin / marketplace)         release                provider
   │ GET /release/resolve?module=X&channel=stable&platform=darwin-arm64
   │ ──────────────────────────────────────────►│
   │ ◄─ {version, sha256, ed25519_sig, dl_url} ──│   (dl_url = signed provider GET)
   │ GET bytes ─────────────────────────────────────────────────────────►│
   │ verify sha256 == key  AND  ed25519_sig over manifest (trusted key)
   │ install / atomic-swap
```

The existing **OTA self-update loop** and the **module marketplace** both become
consumers of `/release/resolve` — they stop carrying the artifact URL themselves.

## 6. Should this be a separate module? — **Yes: `polar-release`**

Three concerns, three owners:

| layer | owner | responsibility |
|---|---|---|
| **bytes** | **assets** | dumb content-addressed store; signed **PUT + GET**; replication / warm-pull |
| **registry** | **polar-release** *(new)* | versions / channels / platforms → artifact; publish + resolve; ed25519 manifest signing |
| **identity** | **dock** | authenticate publishers + clients; nothing else |

`polar-release` calls **assets** for bytes (create asset, mint signed PUT/GET) and
**dock `/internal/v1`** for identity. This matches the standing direction
("dock = identity + LLM + collab core only") — release/distribution is exactly
the non-core concern to split out. It also unifies three things that are ad-hoc
today: plugin OTA, agent self-update, and the module marketplace.

Boundary discipline:
- assets must stay "dumb" (no notion of versions/modules) — it just stores and
  serves bytes with up/down signed URLs.
- release never streams bytes — it only mints grants + holds metadata + signs.

## 7. Security

- **Signed PUT is object-scoped** (binds the sha key + size + expiry) — a leaked
  token can't overwrite arbitrary blobs, and content-hash recompute on the
  provider makes mismatched uploads impossible.
- **ed25519-signed manifest.** sha256 alone gives *integrity*; the signature over
  `(module, version, platform, sha256)` with a release key the client trusts
  (baked into agent/plugin, rotated via dock) gives *authenticity* — a
  compromised provider then **cannot** serve a malicious binary even with a
  valid-looking hash. (Same principle as Apple img4 signing.)
- **Yank** sets `yanked_at`; `resolve` skips yanked releases so a bad build stops
  propagating immediately.

## 8. Migration phases

1. **Assets: signed-PUT.** Add presigned upload to the provider + the
   mint-upload-grant endpoint; flip new uploads to direct PUT (dock byte path
   retired). Flag-gated; back-compat with the through-dock upload until proven.
2. **Scaffold `polar-release`.** `releases` schema + `publish`/`finalize`/`resolve`
   + ed25519 manifest signing; reuse assets for bytes, dock for identity.
3. **Point OTA at `resolve`.** Agent/plugin update-check calls `release resolve`
   instead of reading the heartbeat-embedded URL — flag-gated, parallel to the
   current path, then default-on.
4. **Marketplace onto release.** The module marketplace (RBAC R3) publishes +
   installs via release.
5. **Retire** the dock-embedded OTA URL + the through-dock upload code.

## 9. Open questions

- Channel promotion model (promote a `beta` build to `stable` = new row vs
  re-tag?). Leaning new row pointing at the same `artifact_id` (dedup).
- Per-release access control (private modules / per-workspace visibility) — reuse
  the workspace-plugin-access grant?
- Release key rotation + the trust-on-first-use bootstrap for agents.
- Multi-provider/region resolve for `dl_url` (reuse assets' region-aware picker +
  warm-pull so the first install in a region seeds a local cache).
