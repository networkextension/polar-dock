# Polar dock — single-box deployment guide

How to stand up a full Polar stack (dock + 11 plugin svcs + Postgres + Redis +
nginx + UI) on a fresh macOS box, and how to seed it to mirror an existing
environment. Written from the **dev (`10.88.0.5`)** and **stage (`10.88.0.4`)**
deploys; the same playbook reproduces a new tier (`prod`, etc.).

> TL;DR: build the binaries on the dev/build box, **pull** them to the target
> over the LAN, bootstrap brew (postgres/redis/nginx) via the LAN proxy, bring
> up dock under launchd against a fresh Postgres, run two scripts for the 11
> plugins, wire nginx, then (optionally) seed admin/LLM/bots/mail/invite.

---

## 0. Topology & naming

| Tier  | WG IP        | LAN IP          | Hostnames                         |
|-------|--------------|-----------------|-----------------------------------|
| prod  | 10.88.0.1    | 192.168.11.57   | `zen.4950.store`, `*.4950.store`  |
| dev   | 10.88.0.5    | 192.168.11.61   | `dock.dev.4950.store`, `*.dev…`   |
| stage | 10.88.0.4    | 192.168.2.3     | `dock.stage.4950.store`, `*.stage…`|
| build | 10.88.0.2    | 192.168.11.79 / 192.168.2.1 | runs the HTTP proxy on `:10082` |

- Target user is always `local`, `HOME=/Users/local`, arm64.
- DNS: each `<module>.<tier>.4950.store` (+ `dock.<tier>`) A-records → the tier's
  WG IP. Add via `scripts/dns/name-com-records.sh` (bulk mode, no `--wildcard`).
- The "build box" is the dev workstation; it has the toolchain, internet, and an
  HTTP forward-proxy on `:10082` that targets use for package downloads.

### Network gotchas (read first — they cost hours)

1. **The WG tunnel is lossy for bulk transfer.** rsync stalls, `git`-over-proxy
   hits `early EOF`, and TLS *responses* hang while TCP/handshake succeed. Use
   the **LAN** for anything large. Control-plane SSH over WG works but **drops
   mid-command** — keep remote commands short + idempotent and pass
   `-o ServerAliveInterval=4`.
2. **Artifact transfer = target *pulls* over LAN.** Run `python3 -m http.server`
   on the build box and have the target `curl | tar xz`. ~220 MB lands in
   ~10–30 s vs. stalling forever over WG.
   - Pull host = **`192.168.11.79:8899`** (reachable from every tier, routed via
     the build box). NOTE: the build box's `192.168.2.1` interface **firewalls
     `:8899`** (only `:10082` is open there) — so on stage use `192.168.11.79`,
     not `192.168.2.1`.
   - The `http.server` dies when the spawning shell call returns — start it
     **and** run the target pull in the *same* command.
3. **brew downloads via the LAN proxy**, e.g. `https_proxy=http://192.168.11.79:10082`
   (dev) or `http://192.168.2.1:10082` (stage — its LAN side of the build box).

---

## 1. Pre-flight on the target

```bash
ssh local@<WG_IP> '
  xcode-select -p;            # CLT must be present
  sudo -n true && echo SUDO_OK;   # passwordless sudo required (brew + nginx:443)
  uname -m; sw_vers -productVersion;
  https_proxy=http://<PROXY_LAN_IP>:10082 curl -fsS -m12 -o /dev/null -w "%{http_code}\n" \
    https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh'   # want 200
```

---

## 2. Bootstrap Homebrew + postgres/redis/nginx (background, via LAN proxy)

```bash
ssh local@<WG_IP> '
  git config --global http.postBuffer 524288000
  git config --global http.version HTTP/1.1
  export NONINTERACTIVE=1 HOMEBREW_NO_AUTO_UPDATE=1 \
    https_proxy=http://<PROXY_LAN_IP>:10082 http_proxy=http://<PROXY_LAN_IP>:10082 \
    ALL_PROXY=http://<PROXY_LAN_IP>:10082 \
    HOMEBREW_HTTP_PROXY=http://<PROXY_LAN_IP>:10082 HOMEBREW_HTTPS_PROXY=http://<PROXY_LAN_IP>:10082
  { [ -x /opt/homebrew/bin/brew ] || /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)";
    eval "$(/opt/homebrew/bin/brew shellenv)";
    brew install postgresql@16 redis nginx; } > ~/brew-bootstrap.log 2>&1'
```

The `git` hardening prevents the `early EOF` pack failures over the proxy.

---

## 3. Build artifacts (on the build box)

Build from a **clean `origin/main` worktree** (the working tree is usually
dirty). `CGO_ENABLED=0` — the project is CGO-free and the build box's CLT SDK
ref may be stale.

```bash
git worktree add --detach /tmp/polar-clean origin/main   # or refresh existing
cd /tmp/polar-clean && mkdir -p /tmp/deploy/plugins
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o /tmp/deploy/polar-dock ./cmd/dock
for d in modules/polar-*; do svc="${d#modules/polar-}-svc"; [ -d "$d/cmd/$svc" ] || continue
  (cd "$d" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o "/tmp/deploy/plugins/$svc" "./cmd/$svc"); done
# if a plugin errors "updates to go.mod needed": run `go mod tidy` in that module then rebuild (hosts-svc)
( cd modules/polar-dock-ui && [ -d node_modules ] || npm install --no-audit --no-fund; node scripts/build.mjs )  # → dist/

cd /tmp/deploy && cp plugins/*-svc . && tar czf bins.tgz polar-dock *-svc
tar czf ui.tgz -C /tmp/polar-clean/modules/polar-dock-ui/dist .
tar czf migrate.tgz -C /tmp/polar-clean/modules $(cd /tmp/polar-clean/modules && echo */scripts/migrate/*.sql)
```

11 plugin svcs + ports: `wg:8090 iosdist:8091 video:8092 packtunnel:8093
library:8094 hosts:8095 projects:8096 expense:8097 latch:8098 lawyer:8099
buildings:8100`. dock listens `127.0.0.1:8080`.

---

## 4. Transfer to the target (LAN pull)

```bash
cd /tmp/deploy
python3 -m http.server 8899 --bind 0.0.0.0 >/tmp/deploy/httpserver.log 2>&1 & SRV=$!; sleep 2
ssh local@<WG_IP> 'H=http://192.168.11.79:8899; mkdir -p ~/.local/bin ~/www/dock-ui /tmp/plugin-migrate
  curl -fsS $H/bins.tgz | tar xzf - -C ~/.local/bin
  curl -fsS $H/ui.tgz   | tar xzf - -C ~/www/dock-ui
  curl -fsS $H/migrate.tgz | tar xzf - -C /tmp/plugin-migrate
  chmod +x ~/.local/bin/polar-dock ~/.local/bin/*-svc'
kill $SRV
```

---

## 5. dock env, Postgres/Redis, launchd boot

**Env** — `~/.config/polar/dock.env` (fresh secrets per box; quote values with
spaces; `set -a; source` it):

```
ADDR=127.0.0.1:8080
POSTGRES_DSN=postgres://polar:<dbpw>@127.0.0.1:5432/polar_dock?sslmode=disable
REDIS_ADDR=127.0.0.1:6379
REDIS_PREFIX=polar
UPLOAD_DIR=/Users/local/polar/data/uploads
MARKDOWN_DIR=/Users/local/polar/data/markdown
PUBLIC_BASE_URL=https://dock.<tier>.4950.store
PASSKEY_ORIGIN=https://dock.<tier>.4950.store
PASSKEY_RP_ID=dock.<tier>.4950.store
PASSKEY_RP_NAME="Polar <Tier>"
POLAR_COOKIE_DOMAIN=dock.<tier>.4950.store
POLAR_CREDENTIAL_KEY=<openssl rand -hex 32>   # 32-byte hex; AES key for host_skill_credentials
POLAR_ALLOW_LOCAL_BOOTSTRAP=true
POLAR_ENFORCE_TEAM_STATUS=false
```

**Postgres + Redis** — `brew services start` is ideal but **flakes over a lossy
WG** mid-bootstrap. The reliable fallback is a detached daemon:

```bash
D=/opt/homebrew/var/postgresql@16
[ -d "$D/base" ] || initdb -D "$D"
pg_ctl -D "$D" -l "$D/server.log" -w -t 30 start      # detached; survives SSH drops
redis-server --daemonize yes
# role + db (idempotent):
psql -h 127.0.0.1 -d postgres -c "CREATE ROLE polar LOGIN PASSWORD '<dbpw>'"
psql -h 127.0.0.1 -d postgres -c "CREATE DATABASE polar_dock OWNER polar"
```

For reboot **persistence**, copy brew's plists into `~/Library/LaunchAgents`
(they load at next login; don't `bootstrap` them while `pg_ctl` holds `:5432`):
```bash
cp /opt/homebrew/opt/postgresql@16/homebrew.mxcl.postgresql@16.plist ~/Library/LaunchAgents/
cp /opt/homebrew/opt/redis/homebrew.mxcl.redis.plist ~/Library/LaunchAgents/
```

**dock launchd** — `~/Library/LaunchAgents/com.polar.dock.plist` runs
`bash -lc 'set -a; source ~/.config/polar/dock.env; exec ~/.local/bin/polar-dock'`
with `RunAtLoad` + `KeepAlive`, logging to `~/polar/dock.log`. Then:
```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.polar.dock.plist
grep -q 'Listening and serving' ~/polar/dock.log   # boots + auto-migrates schema; bootstraps root team
```

dock **auto-migrates** the schema on boot against the empty DB and bootstraps a
root team. `GET / → 302`, `GET /api/me → 401` = healthy.

---

## 6. Plugins (11) — DBs, tokens, registration, launchd, schemas

Each plugin: own DB `polar_<name>`, a minted `POLAR_PLUGIN_TOKEN`
(`sha256(token)` stored in `plugin_modules.plugin_key_hash`), launchd job, and
its schema applied from `scripts/migrate/<name>-schema.sql` (**plugins do NOT
self-migrate**). Two reusable, hostname-independent scripts do it (see
`doc/deploy/scripts/` — `deploy-plugins.sh` mints tokens + creates DBs +
registers `plugin_modules` + writes per-plugin env + launchd; `apply-schema.sh`
loads each schema from `/tmp/plugin-migrate` and restarts the svcs):

```bash
bash deploy-plugins.sh      # → 11 launchd jobs running, registered, heartbeating
bash apply-schema.sh        # → each polar_<name> DB gets its tables
```

Verify: `SELECT count(*) FROM plugin_modules WHERE last_heartbeat IS NOT NULL` = 11.

Per-plugin env lives at `~/.config/polar/<name>-svc.env`: `POLAR_<NAME>_DB_DSN`,
`POLAR_DOCK_BASE` (latch uses `POLAR_DOCK_URL`), `POLAR_PLUGIN_NAME`,
`POLAR_PLUGIN_TOKEN`, `POLAR_<NAME>_LISTEN`.

---

## 7. nginx (TLS + reverse proxy)

nginx serves the **dock-ui static** (`~/www/dock-ui`) and proxies `/api`, `/ws`,
`/uploads`, `/s/`, `/v1/`, `/internal/` to `127.0.0.1:8080`; module subdomains
also route `/v1 /healthz /metrics` to the plugin svc.

macOS specifics:
- Move brew's default server off `:8080` (dock owns it): `listen 8080;` → `8088;`.
- Set `user local admin;` in `nginx.conf` so workers can read the home-dir UI
  (else `/admin.html → 500/403`).
- nginx must run **as root** to bind 80/443: `sudo /opt/homebrew/bin/nginx`.
- dock's mailer needs nothing here, but small `ssl_buffer_size 4k;` avoids TLS
  response stalls on lossy paths.
- Self-signed cert (browser warning; fine for dev/stage):
  ```bash
  openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
    -keyout ~/polar/tls/<tier>.key -out ~/polar/tls/<tier>.crt \
    -subj "/CN=dock.<tier>.4950.store" \
    -addext "subjectAltName=DNS:dock.<tier>.4950.store,DNS:*.<tier>.4950.store"
  ```
  When copying the dev vhost to a new tier, sed **both** the hostname **and** the
  cert path (`tls/dev` → `tls/<tier>`).

Verify on-box: `curl -k --resolve dock.<tier>.4950.store:443:127.0.0.1
https://dock.<tier>.4950.store/login.html` → 200.

---

## 8. Seeding (optional — mirror an existing tier)

All data-plane; run after the stack is up. IDs are per-box (fresh DB) — look them
up first: `SELECT id,email FROM users; SELECT id,slug,is_root,owner_user_id FROM teams;`

1. **Site admin** — there is no "personal" admin; create a dedicated root team
   and add the admin user as owner (also set `users.role='admin'`):
   ```sql
   INSERT INTO teams (id,slug,name,kind,owner_user_id,is_root,status,created_at,updated_at)
   SELECT <rid>,'site-admin','Site Admin','team',<ceo_id>,TRUE,'active',now(),now()
   WHERE NOT EXISTS (SELECT 1 FROM teams WHERE is_root);
   INSERT INTO team_members (team_id,user_id,role,joined_at)
   SELECT id,<ceo_id>,'owner',now() FROM teams WHERE is_root
   ON CONFLICT (team_id,user_id) DO UPDATE SET role='owner';
   UPDATE users SET role='admin' WHERE id=<ceo_id>;
   ```
   `IsSiteAdmin` = owner/admin of the `is_root` team. Restart dock to clear its
   admin cache.

2. **LLM configs** — copy from prod's `ideamesh` DB (api_key is **plaintext**),
   own them in the site-admin team + set `is_platform=TRUE` so every workspace's
   picker sees them (configs are workspace-scoped for *management*, but shared
   for *use* via `shared OR is_platform`). Stream prod→target with `COPY … TO
   STDOUT | … COPY _imp FROM STDIN`. The single `is_system` row (`ali`) carries a
   platform key; insert it separately.

3. **Chat bots** — copy `bot_kind='classic'`, **not** auto-registered, **not**
   `'%research%'` (those are agent bots — skip). Each bot = a `users` row
   (`role='bot'`) + a `bot_users` row; classic bots need a valid `llm_config_id`
   (remap by config name). Put them in the site-admin workspace.

4. **Mail (Resend over SMTP)** — dock's mailer requires **STARTTLS → port 587**
   (NOT 465). Env:
   ```
   SMTP_HOST=smtp.resend.com  SMTP_PORT=587  SMTP_USERNAME=resend
   SMTP_PASSWORD=<resend_api_key>  SMTP_FROM_EMAIL=noreply@4950.store  SMTP_FROM_NAME="Polar <Tier>"
   ```
   The key must be **full-access** to verify a domain; a send-only key returns
   `restricted_api_key`. Verify `4950.store` once (account-level — shared across
   tiers): `POST /v1/domains` → add the SPF/DKIM/bounce-MX records via
   `scripts/dns/resend-mail-records.sh` → `POST /v1/domains/<id>/verify`. Until
   verified, Resend test-mode only delivers to the account owner.

5. **Invite config** — match prod: `UPDATE site_settings SET
   registration_requires_invite=TRUE`. Mint codes:
   `INSERT INTO invite_codes (code,created_by,created_at) SELECT
   upper(substr(replace(gen_random_uuid()::text,'-',''),1,10)),<ceo_id>,now()
   FROM generate_series(1,5);` (use `gen_random_uuid`, not `gen_random_bytes` —
   pgcrypto isn't installed).

---

## 9. Operations & troubleshooting

- **Restart a service:** `launchctl kickstart -k gui/$(id -u)/com.polar.dock`
  (or `com.polar.<name>-svc`).
- **dock logs:** `~/polar/dock.log`; **plugin logs:** `~/polar/<name>-svc.log`.
- **DB:** `PGPASSWORD=… psql -h 127.0.0.1 -U polar -d polar_dock`.
- **`POST /api/markdown` → 500** on a fresh DB: known bug — `workspace_id` is
  `NOT NULL` but the handler nulled it for private/public docs (fixed in
  polar-dock PR #385; rebuild dock if not yet merged).
- **A markdown "isn't shared":** `visibility='workspace'` is team-only; the
  public share link needs `visibility='public'`.
- **Pull fails / `Couldn't connect to :8899`:** the build box's `192.168.2.1`
  interface firewalls it — use `192.168.11.79:8899`; and the server dies between
  shell calls, so start-server + target-pull in one command.
- **Schema FK ordering:** in `store.go`, an `ALTER … REFERENCES <t>` must come
  **after** `<t>`'s `CREATE`, or fresh-DB boot fails (the CI boot-smoke gate
  catches this).

See also: memory note `project-dev-box-deploy` for the live state of the dev +
stage boxes.
