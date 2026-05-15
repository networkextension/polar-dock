# macmon — Apple Silicon system monitoring for Polar hosts

Adds power / thermal / CPU / GPU / ANE telemetry for each Apple Silicon Mac that runs `polar-agent`. The data is scraped by Prometheus → visualized in Grafana → embedded as an iframe on `/hosts.html` (see PR #209).

`node_exporter` on macOS gives essentially nothing for Apple Silicon: no CPU power, no GPU usage, no ANE, no temperature. macmon ([github.com/vladkens/macmon](https://github.com/vladkens/macmon)) plugs the gap by reading the same IOReport private API that `powermetrics` uses — **no sudo required**.

## Topology

```
Mac host (any darwin/arm64 box running polar-agent)
├── polar-agent (talks to dock over /ws/agent)
└── macmon serve :9092 (launchd)            ← NEW
              ↓ scraped 15s
       Prometheus :9090 (on dock host)      ← scrape target added
              ↓ queried
       Grafana :3000 or :3100               ← macmon-overview dashboard
              ↓ iframe (kiosk=tv)
       polar-ui /hosts.html (host detail)   ← PR #209
```

## What you'll add per Mac box

1. The macmon binary at `~/.local/bin/macmon`.
2. A launchd plist at `~/Library/LaunchAgents/com.macmon.plist` (macmon's `--install` flag creates it).
3. A Prometheus scrape job pointing at the host's `:9092`.
4. (Only on the dock-with-Grafana box) Provisioning yaml for the dashboard + datasource.
5. (Only on the dock-with-Grafana box) `grafana.ini` edits — `[auth.anonymous]` Viewer + `[security] allow_embedding = true`.

## Step-by-step

### 1. Build macmon (do this once on the dev mac with rustc)

GitHub release downloads + `brew install macmon` may both be blocked from inside corporate / restricted networks. Building from source on the dev mac is the reliable path; the resulting darwin/arm64 binary works on any Apple Silicon target.

```sh
git clone https://github.com/vladkens/macmon ~/Codex/macmon  # or wherever
cd ~/Codex/macmon
cargo build --release
ls -lh target/release/macmon                                  # ~1.6 MB
```

### 2. Push the binary

```sh
rsync -avz ~/Codex/macmon/target/release/macmon \
  local@<TARGET_BOX>:/Users/local/.local/bin/macmon
ssh local@<TARGET_BOX> 'chmod +x /Users/local/.local/bin/macmon && /Users/local/.local/bin/macmon --version'
```

### 3. Install as a launchd service

```sh
ssh local@<TARGET_BOX> '/Users/local/.local/bin/macmon serve --port 9092 --install'
```

This creates `~/Library/LaunchAgents/com.macmon.plist`, auto-starts on every login. Verify:

```sh
ssh local@<TARGET_BOX> 'launchctl list | grep macmon; curl -sS http://127.0.0.1:9092/metrics | head -3'
```

Expect `com.macmon` in the listing and a few `macmon_*` Prometheus gauges.

**Port choice**: 9092 sidesteps the conflict with Prometheus on 9090. If you're co-locating macmon on a non-Prometheus box, you can keep the default port, but using 9092 everywhere keeps the scrape config consistent.

### 4. Add the Prometheus scrape target

On the dock host (where Prometheus runs), append to `/opt/homebrew/etc/prometheus.yml`:

```yaml
  - job_name: macmon
    static_configs:
      - targets:
          - <TARGET_BOX_IP>:9092   # or 127.0.0.1:9092 if same host
        labels:
          host: <hostname>
          env: prod                # optional, useful for dashboard filters later
```

Hot-reload (no restart needed):

```sh
ssh local@<DOCK_BOX> 'curl -sS -X POST http://127.0.0.1:9090/-/reload -o /dev/null -w "%{http_code}\n"'
```

Verify the new target is `up`:

```sh
ssh local@<DOCK_BOX> 'curl -sS http://127.0.0.1:9090/api/v1/targets | grep -oE "\"job\":\"[^\"]+\"|\"health\":\"[^\"]+\""'
```

### 5. Provision the macmon dashboard (dock-with-Grafana box only)

Repo paths used in this project:

```
ui/public/macmon-overview.json     # not committed; comes from upstream
                                   # https://github.com/vladkens/macmon/blob/main/example-grafana/grafana-dashboard.json
```

Push the dashboard JSON to the dock:

```sh
scp ~/Codex/macmon/example-grafana/grafana-dashboard.json \
  local@<DOCK_BOX>:/tmp/macmon-overview.json

ssh local@<DOCK_BOX> '
  mkdir -p /opt/homebrew/opt/grafana/share/grafana/dashboards
  mv /tmp/macmon-overview.json /opt/homebrew/opt/grafana/share/grafana/dashboards/macmon-overview.json
'
```

Provisioning yaml at `/opt/homebrew/opt/grafana/share/grafana/conf/provisioning/dashboards/macmon.yaml`:

```yaml
apiVersion: 1

providers:
  - name: macmon
    orgId: 1
    folder: Host
    type: file
    disableDeletion: false
    updateIntervalSeconds: 30
    allowUiUpdates: true
    options:
      path: /opt/homebrew/opt/grafana/share/grafana/dashboards
```

**Datasource**: most polar deploys already provision a Prometheus datasource with `uid: prometheus` (e.g. `polar.yaml`). **Don't add a second one with `isDefault: true`** — Grafana refuses to start on duplicate defaults. If the existing provisioning file doesn't reference `uid: prometheus`, add one; otherwise skip.

### 6. grafana.ini edits

```ini
[server]
# Match the URL the operator types in their browser. If Grafana is
# fronted by nginx on a hostname, use the hostname here. If accessed
# via raw IP+port, use that. Mismatch = "failed to load application
# files" page.
root_url = https://<your-domain>/grafana/
serve_from_sub_path = true

[security]
allow_embedding = true

[auth.anonymous]
enabled = true
org_role = Viewer
```

**Security trade-off**: `auth.anonymous = true` means anyone who can reach `/grafana/` views dashboards (no edits). `/hosts.html` in polar is admin-gated, so the iframe-inside is only loaded after admin auth; the open exposure surface is the nginx vhost itself. Acceptable for internal monitoring; if you need stricter, see the `Authenticated cookie share` follow-up below.

Restart Grafana:

```sh
ssh local@<DOCK_BOX> '/opt/homebrew/bin/brew services restart grafana'
```

### 7. nginx /grafana/ proxy (only if not already wired)

For a fresh nginx vhost, add a `/grafana/` location block proxying to Grafana's listen port (3000 by default; some installs use 3100). The exact snippet (matches the production zen.4950.store box):

```nginx
location /grafana/ {
    # No trailing slash on proxy_pass — Grafana expects the
    # /grafana/ prefix to be preserved.
    proxy_pass         http://127.0.0.1:3000;
    proxy_http_version 1.1;
    proxy_set_header   Host              $http_host;
    proxy_set_header   X-Real-IP         $remote_addr;
    proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header   X-Forwarded-Proto $scheme;
    proxy_read_timeout 600s;
}

location /grafana/api/live/ {
    # Grafana Live uses WebSocket — needs Upgrade headers + long timeout.
    proxy_pass         http://127.0.0.1:3000;
    proxy_http_version 1.1;
    proxy_set_header   Upgrade           $http_upgrade;
    proxy_set_header   Connection        "upgrade";
    proxy_set_header   Host              $http_host;
    proxy_read_timeout 3600s;
}
```

`$http_host` (not `$host`) is important — `$host` strips the port and breaks Grafana's bootstrap when accessed via non-default-port URLs.

### 8. Smoke test

From any machine on the network:

```sh
curl -sS -k -o /dev/null -w "%{http_code}\n" "https://<your-domain>/grafana/d/macmon-overview/macmon-overview"
# 200 → good
```

Then refresh `/hosts.html` → click a darwin host → 系统监控 (macmon) section paints inline.

## Known gotchas

### nginx `proxy_temp` owned by `nobody`

**Symptom**: Grafana JS bundles > 64 KiB truncate at exactly the same byte count (e.g. 131444 bytes per file). Browser shows `failed to load application files`. nginx error log has lines like:

```
[crit] open() "/opt/homebrew/var/run/nginx/proxy_temp/N/00/00000000NN" failed (13: Permission denied)
```

**Cause**: a previous nginx install (often the stock Apple `nobody` install) created the temp dirs owned by `_nobody`. The current nginx runs as `local` and can't write large responses to disk.

**Fix** (needs sudo):

```sh
sudo chown -R local:admin \
  /opt/homebrew/var/run/nginx/proxy_temp \
  /opt/homebrew/var/run/nginx/client_body_temp \
  /opt/homebrew/var/run/nginx/fastcgi_temp \
  /opt/homebrew/var/run/nginx/scgi_temp \
  /opt/homebrew/var/run/nginx/uwsgi_temp
```

No nginx restart needed — next request that spills to disk just works.

### Duplicate `isDefault: true` across datasource yamls

**Symptom**: After provisioning, Grafana refuses to start. Log says `Only one datasource per organization can be marked as default`.

**Cause**: multiple `.yaml` files in `provisioning/datasources/` each declaring a default. Common when grafana-pg-readonly script (auto-generated `polar.yml`) and a hand-written `prometheus.yaml` both set `isDefault: true`.

**Fix**: keep `isDefault: true` on exactly one yaml. Reload provisioning by restarting Grafana.

### `root_url` mismatch with the access URL

**Symptom**: Same `failed to load application files` page as the proxy_temp issue, but nginx error log is clean.

**Cause**: `grafana.ini`'s `root_url` doesn't match how the operator's browser is reaching Grafana (e.g. configured for `https://dev.4950.store/grafana/` but accessed via `http://192.168.x.x:8888/grafana/`).

**Fix**: set `root_url` to the URL the iframe-loading page sees. For dev boxes accessed by IP:port, use the IP:port form. For production behind a hostname, use the hostname. When the IP drifts (link-local boxes), update `root_url` and restart Grafana.

### Hairpin NAT (FreeBSD agent → its own public hostname)

**Symptom**: From inside the LAN, `curl https://<your-domain>/...` times out at the TLS handshake. TCP connect succeeds but no response.

**Cause**: the NAT router doesn't reflect connections to its own public IP back into the LAN.

**Fix**: register via the **internal LAN IP + raw HTTP port** of the dock (e.g. `http://192.168.11.57:8080`) instead of the public hostname. macmon is macOS-only anyway, so this only matters for FreeBSD/Linux agents trying to call `polar-agent register --server=...`.

### Go HTTP client honors `HTTP_PROXY`, curl often doesn't

**Symptom**: `polar-agent register` times out while `curl` to the same URL works fine.

**Cause**: `HTTP_PROXY` / `HTTPS_PROXY` env vars set in the shell. Go's `net/http` routes all requests through them; curl is configured per-distro and often ignores them unless explicitly told. If the proxy is unreachable, Go hangs until its 30 s timeout.

**Fix** for the polar-agent on that host:

```sh
# One-shot
env HTTP_PROXY= HTTPS_PROXY= polar-agent ...

# Persistent (add to ~/.profile or the launchd plist EnvironmentVariables block)
export NO_PROXY=192.168.0.0/16,127.0.0.1,localhost,10.0.0.0/8
```

## Cleanup / rollback

```sh
# On the target box
ssh local@<TARGET_BOX> '
  /Users/local/.local/bin/macmon serve --uninstall
  rm /Users/local/.local/bin/macmon
'

# On the dock box
ssh local@<DOCK_BOX> '
  # Remove the scrape target — easiest is to edit prometheus.yml + reload
  curl -sS -X POST http://127.0.0.1:9090/-/reload

  rm -f /opt/homebrew/opt/grafana/share/grafana/conf/provisioning/dashboards/macmon.yaml
  rm -f /opt/homebrew/opt/grafana/share/grafana/dashboards/macmon-overview.json
  /opt/homebrew/bin/brew services restart grafana
'
```

The UI iframe section auto-hides for non-darwin hosts and degrades cleanly to its "在 Grafana 打开 ↗" link if the iframe is blocked, so the polar UI doesn't break if macmon/Grafana are removed.

## Box-specific cheat sheet (state as of 2026-05-15)

| | `dev.4950.store` | `zen.4950.store` |
|---|---|---|
| Reach | `local@169.254.246.147` (direct LAN) | `local@127.0.0.1:5722` (tunnel via `swift@zen.4950.store:2222`) |
| Grafana listen | `127.0.0.1:3000` | `127.0.0.1:3100` |
| nginx vhost | `/opt/homebrew/etc/nginx/servers/polar` (`:8888` HTTP + `:443` HTTPS) | `/opt/homebrew/etc/nginx/servers/ssl` (`:443` HTTPS) |
| `root_url` | `http://169.254.246.147:8888/grafana/` | `https://zen.4950.store/grafana/` |
| Datasource provisioning | `prometheus.yaml` (hand-written, `uid=prometheus`, default) | `polar.yaml` (existing, `uid=prometheus`, default) — **don't add a duplicate** |
| nginx temp owner | was `nobody`, fixed 2026-05-15 → `local` | already `local` |

## Follow-ups (not in this PR)

- **WS-forwarded host metrics** for non-Mac agents (FreeBSD `dpaa2`, future Linux). polar-agent shells out to a portable metric source (or macOS-only path stubs), forwards JSON over `/ws/agent` as a new `skill.event{kind:"host_metrics"}` envelope; dock stores a rolling window + the polar UI renders inline panels without needing Prometheus reachability.
- **Per-host template variable** on the macmon dashboard so the iframe can pass `?var-host=<id>` and the dashboard filters automatically.
- **Authenticated cookie-share** as a stricter alternative to anon Viewer: have polar-dock issue a short-lived Grafana session cookie tied to the polar admin session.
