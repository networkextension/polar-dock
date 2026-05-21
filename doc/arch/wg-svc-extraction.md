# wg-svc extraction runbook

Phase 1-C ships the skeleton (this PR). Phase 1-D performs the
actual handler migration + traffic cutover. This doc is the
operator + developer reference for both.

## What Phase 1-C ships

| Path | Purpose |
|---|---|
| `cmd/wg-svc/main.go` | Standalone binary; opens polar_wg + dock SDK + serves `/healthz`. |
| `internal/plugins/wg/plugin.go` | `Plugin` struct: DB pool, heartbeat loop, `/healthz` handler. |
| `internal/plugins/sdk/client.go` | In-tree HMAC client (will become `polar-plugin-sdk-go` in Phase 3). |
| `scripts/migrate/wg-schema.sql` | Polar_wg end-state schema (Phase 2 shape). |
| `scripts/migrate/wg-data.sh` | Idempotent dump + load of wg_* rows from `ideamesh` → `polar_wg`. |

Dock is unchanged. Dock still serves `/api/admin/wg-*` and `/v1/{register,peers,heartbeat,leave,bundle,install}`. wg-svc only proves the architecture: it talks to dock, dock recognises it (`/admin-plugins.html` shows a fresh heartbeat), and it owns its own DB.

## What Phase 1-D ships

1. **Move handlers** from `internal/app/dock/wg_*.go` into `internal/plugins/wg/`. They become methods on `Plugin` (which carries DB + Dock SDK) instead of `*Server`.
2. **Replace cross-domain lookups** with SDK calls:
   - `wg_tokens.created_by_user_id` rendered as a username → `dock.UserGet`
   - `wg_bundles.added_by_user_id` → same
   - Admin auth on `/api/admin/wg-*` → `dock.AuthVerify(token)` + role check
3. **Cutover**:
   - `nginx`: add `location /api/wg/ { proxy_pass http://127.0.0.1:8090; }` + `location ~ ^/v1/(register|peers|heartbeat|leave|bundle|install) { proxy_pass http://127.0.0.1:8090; }`
   - Set `POLAR_WG_REMOTE=true` in dock env — dock returns 404 from its wg routes, telling nginx "use the upstream" (the route in nginx is registered *before* the dock-fallback, so the flag is belt-and-braces).
4. **One release of bake time**, then delete `internal/app/dock/wg_*.go` + drop the `wg_*` tables in `ideamesh` (after a `pg_dump` backup).

## Apply Phase 1-C on the deploy box

```bash
# 0. ssh in
ssh -p 5722 local@127.0.0.1

# 1. create the database (one-time, as the cluster superuser)
PSQL='/Applications/Postgres.app/Contents/Versions/latest/bin/psql'
$PSQL -d postgres -c "CREATE DATABASE polar_wg OWNER ideamesh;"

# 2. apply schema
cd ~/github/Polar-
$PSQL "postgres://ideamesh:test123456@127.0.0.1:5432/polar_wg" \
    -f scripts/migrate/wg-schema.sql

# 3. dry-run the data migration (read-only, shows source counts)
./scripts/migrate/wg-data.sh

# 4. perform the migration (truncates target, loads from source)
./scripts/migrate/wg-data.sh --apply

# 5. provision a plugin row via the admin UI
#    https://dev.4950.store/admin-plugins.html → "New plugin"
#    name: "wg"  display: "WireGuard mesh"  endpoint: "http://127.0.0.1:8090"
#    capture the plaintext token (45 chars, polar_plugin_…) — shown once.

# 6. cross-build wg-svc on the dev box, rsync to deploy
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/wg-svc ./cmd/wg-svc
rsync -avz -e 'ssh -p 5722' /tmp/wg-svc local@127.0.0.1:/Users/local/.local/bin/wg-svc

# 7. start wg-svc (env-file, chmod 600)
cat > ~/wg-svc.env <<EOF
POLAR_WG_DB_DSN=postgres://ideamesh:test123456@127.0.0.1:5432/polar_wg?sslmode=disable
POLAR_DOCK_BASE=http://127.0.0.1:8080
POLAR_PLUGIN_NAME=wg
POLAR_PLUGIN_TOKEN=polar_plugin_…  # from step 5
POLAR_WG_LISTEN=127.0.0.1:8090
POLAR_WG_VERSION=$(git rev-parse --short HEAD)
EOF
chmod 600 ~/wg-svc.env
env $(cat ~/wg-svc.env | xargs) /Users/local/.local/bin/wg-svc &

# 8. smoke
curl -s http://127.0.0.1:8090/healthz | jq
# expect: {"plugin":"wg","db_ok":true,...}

# 9. confirm dock sees the heartbeat
#    open /admin-plugins.html — the wg row should show last_heartbeat
#    within the last minute. Expect green/fresh status.
```

If `/healthz` returns `db_ok:false` → wg-svc can reach polar_wg but a query failed. Check `psql polar_wg` works as the same user.

If wg-svc exits at startup with `dock /ping rejected: HTTP 401` → the HMAC key derivation is off. Most likely the operator pasted the wrong column — `PLUGIN_TOKEN` is the *plaintext* shown once at provisioning, not the row's `plugin_key_hash`. Re-rotate via `/admin-plugins.html` → "Rotate key".

## Phase 1-D checklist (do not start yet)

- [ ] PR: move `wg_*.go` from `internal/app/dock/` → `internal/plugins/wg/`
- [ ] Refactor each handler off `*Server`, onto `*Plugin`
- [ ] Replace `s.getUserByID` calls with `p.Dock.UserGet`
- [ ] Replace admin middleware with `p.Dock.AuthVerify` + role check
- [ ] PR: nginx config — add wg-svc upstream + location blocks
- [ ] PR: dock `POLAR_WG_REMOTE` flag; when true, dock returns 404 from its wg routes
- [ ] Deploy + bake one release (≥7d) with traffic on wg-svc, dock idle on wg
- [ ] PR: delete `internal/app/dock/wg_*.go`
- [ ] One-shot SQL: backup + `DROP TABLE wg_heartbeats, wg_devices, wg_bundles, wg_tokens, wg_sites, wg_hubs;` in `ideamesh`

## Rollback (Phase 1-D)

If wg-svc misbehaves after cutover:

1. `launchctl unload` wg-svc + revert nginx to remove the wg-svc upstream (one `nginx -s reload`).
2. dock still has all the wg handlers and still has its own copy of the wg_* tables (we keep them until step 8 of the Phase 1-D checklist). Set `POLAR_WG_REMOTE=false`, kickstart dock. Traffic is back on dock's in-tree path within ~5s.
3. polar_wg may have rows ahead of ideamesh (anything wg-svc wrote in the meantime). Document any divergence; either replay manually or accept the data loss for the cutover window.

This rollback path stays open for one full release. After step 8, the dock-side wg code is gone and rollback requires reverting that PR.
