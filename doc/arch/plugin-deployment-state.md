# Plugin deployment state (2026-05-22)

Snapshot of where the Polar- split-out lives on `.57` today + the path
forward per plugin. Companion to [decision.md](decision.md),
[internal-api-v1.md](internal-api-v1.md),
[database-ownership.md](database-ownership.md),
[open-platform.md](open-platform.md),
[auth-and-tokens.md](auth-and-tokens.md),
and [wg-svc-extraction.md](wg-svc-extraction.md).

## Plugin source-of-truth: separate private repos (extracted 2026-05-22)

All plugin code lives in standalone private GitHub repos under `networkextension/`:

| Plugin | Repo | Status |
|---|---|---|
| (SDK) | https://github.com/networkextension/polar-sdk | v0.1.0 tagged |
| wg | https://github.com/networkextension/polar-wg | build green |
| packtunnel | https://github.com/networkextension/polar-packtunnel | build green |
| library | https://github.com/networkextension/polar-library | build green + tests |
| projects | https://github.com/networkextension/polar-projects | build green, 12 stubs |
| video | https://github.com/networkextension/polar-video | build green, 4 stubs |
| hosts | https://github.com/networkextension/polar-hosts | build green + tests, 18 stubs |
| iosdist | https://github.com/networkextension/polar-iosdist | build green, 5 stubs |
| expense | https://github.com/networkextension/polar-expense | build green, 6 stubs; multimodal LLM via SDK; OCR-then-LLM sync path stubbed |
| latch | https://github.com/networkextension/polar-latch | build green, 0 stubs (self-contained, no LLM coupling) |

Each plugin repo has its own `cmd/<svc>-svc/`, `scripts/{launchd,nginx,migrate}/`, `ui/`, and depends on `github.com/networkextension/polar-sdk v0.1.0`.

This Polar dock repo (`networkextension/Polar`) is the **identity + LLM proxy + collaboration core** — no business-plugin code here anymore.

**Cutover status**: only `wg` has flipped to remote on .57 (`POLAR_WG_REMOTE=true`). The other 9 are deployed (heartbeating to dock) but nginx still routes `/api/<plugin>/*` to dock — per-plugin cutover PRs flip the flag + add the nginx snippet one at a time.

## What's deployed

```
                     +----------+
                     |  nginx   |  :443 ssl  (zen.4950.store)
                     +----+-----+
                          |
        +-----------------+-----------------+
        |                                   |
   /v1/*                              /api/* (everything else)
   /api/admin/wg-*                          |
        |                                   v
        v                            +------------+
   +---------+                       | polar-dock |  :8080
   | wg-svc  |                       +------------+
   +---------+                       launchd, polar.dock.plist
   polar.wg-svc.plist                ENV: POLAR_WG_REMOTE=true
   :8090                             DB:  ideamesh
   DB: polar_wg                      cmd/dock/main.go
   cmd/wg-svc/main.go
```

Plus 6 other plugin processes running on `:8091`-`:8096`, registered
in `plugin_modules`, heartbeating to dock — but **nginx does not
route traffic to them yet**. They serve only `/healthz` + `/metrics`;
real `/api/<plugin>/*` requests still hit dock.

## Per-plugin status

| svc | port | plugin_modules row | nginx routed? | dock-side handlers? | polar_<svc> DB |
|---|---|---|---|---|---|
| wg | 8090 | ✓ phase1d2 | **✓ cutover** (PR #283) | removed via flag (`POLAR_WG_REMOTE=true`) | 9 rows, source-of-truth |
| iosdist | 8091 | ✓ phase2-skel | ✗ (skeleton, will 404) | still serving, source-of-truth | 24 rows, stale snapshot |
| video | 8092 | ✓ phase2-skel | ✗ | still serving, source-of-truth | 17 rows, stale snapshot |
| packtunnel | 8093 | ✓ phase2-skel | ✗ | still serving, source-of-truth | 2 rows, stale snapshot |
| library | 8094 | ✓ phase2-skel | ✗ | still serving, source-of-truth | 0 rows (source empty) |
| hosts | 8095 | ✓ phase2-skel | ✗ | still serving, source-of-truth | 86 rows, stale snapshot |
| projects | 8096 | ✓ phase2-skel | ✗ | still serving, source-of-truth | 182 rows, stale snapshot |

"stale snapshot" = polar_<svc> got a one-time `--apply` copy of
ideamesh data; dock keeps writing to ideamesh until the plugin's
cutover happens. Re-run `scripts/migrate/<svc>-data.sh --apply` just
before the per-plugin cutover to refresh.

## launchd state

All 7 plugin services run under launchd (`~/Library/LaunchAgents/polar.<svc>-svc.plist`).
RunAtLoad + KeepAlive (SuccessfulExit:false) + ThrottleInterval:10s
mirror `polar.dock.plist`. Stop / start:

```bash
launchctl unload  ~/Library/LaunchAgents/polar.<svc>-svc.plist
launchctl load -w ~/Library/LaunchAgents/polar.<svc>-svc.plist
```

The wrapper at `~/.local/bin/polar-<svc>-svc-launch.sh` sources
`~/<svc>-svc.env` (chmod 600 — holds the plaintext PLUGIN_TOKEN
captured at row creation).

## Per-plugin remaining work

For each plugin in W1-W4 the recipe mirrors wg:

| Phase | wg | iosdist | video | packtunnel | library | hosts | projects |
|---|---|---|---|---|---|---|---|
| Schema in store.go | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `internal/plugins/<svc>/plugin.go` skeleton | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `cmd/<svc>-svc/main.go` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `scripts/migrate/<svc>-schema.sql` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `scripts/migrate/<svc>-data.sh` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `scripts/nginx/<svc>-svc-snippet.conf` | ✓ | ✓ (#290) | ✓ (#290) | ✓ (#290) | ✓ (#290) | ✓ (#290) | ✓ (#290) |
| **Handler migration PR** (move dock → plugin) | ✓ (#282) | — | — | — | — | — | — |
| **Cutover PR** (POLAR_<NAME>_REMOTE flag + nginx) | ✓ (#283) | — | — | — | — | — | — |
| launchd plist installed | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| .57 process running | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| nginx route flipped | ✓ | ✗ | ✗ | ✗ | ✗ | ✗ | ✗ |

**Remaining work** for each of 6 = handler migration PR + cutover PR.
Each handler PR is comparable in size to wg's PR #282 (~3.5K LOC for
small plugins, up to ~7K for iosdist). Each cutover PR is small
(flag wire + nginx include + plist edit).

Suggested order (by handler size, smallest first):

1. **packtunnel** (~440 LOC, 2 files) — smallest, easy first run
2. **library** (~1.4K LOC, 4 files) — workspace-agnostic data
3. **video** (~3K LOC, 9 files) — Seedance + ffmpeg
4. **projects** (~1.9K LOC, 2 files) — but heavy cross-domain (chat_threads, llm_threads)
5. **hosts** (~6K LOC, 15+ files) — WS hub for polar-agent + PTY + VNC
6. **iosdist** (~7K LOC, 11 files) — ASC + zsign + R2 + receiver-naming wrinkle

## Cutover playbook (generalized from wg, PR #283)

After the handler PR for plugin X merges:

```bash
# 1. Refresh polar_X with latest from ideamesh
ssh -p 5722 local@127.0.0.1 'cd ~ && SRC_DSN=... DST_DSN=... ./X-data.sh --apply'

# 2. Install nginx snippet
sudo install -m 0644 ~/github/Polar-/scripts/nginx/X-svc-snippet.conf \
    /opt/homebrew/etc/nginx/snippets/X.conf

# 3. Add `include /opt/homebrew/etc/nginx/snippets/X.conf;` to vhost
#    /opt/homebrew/etc/nginx/servers/ssl ABOVE the /api block.
#    Validate + reload:
sudo /opt/homebrew/bin/nginx -t && sudo /opt/homebrew/bin/nginx -s reload

# 4. Flip dock's POLAR_X_REMOTE flag
/usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:POLAR_X_REMOTE string true" \
    ~/Library/LaunchAgents/polar.dock.plist
launchctl unload ~/Library/LaunchAgents/polar.dock.plist
launchctl load -w ~/Library/LaunchAgents/polar.dock.plist

# 5. Smoke
curl -sk -H "Host: zen.4950.store" https://127.0.0.1:443/api/X/...   # → plugin
curl -s http://127.0.0.1:8080/api/X/...                              # → dock 404 (flag honored)
```

Rollback (~10s): remove nginx include + `PlistBuddy -c "Delete ..."` +
kickstart dock.

## Known infra notes

- **Postgres.app gate**: each new binary historically prompted a
  per-binary Allow popup. As of 2026-05-22 the user disabled the
  "Require permission" setting in Postgres.app preferences, so
  future plugin redeploys no longer prompt.
- **nginx vhost backup hygiene**: `.bak` / `.pre-*` files in
  `/opt/homebrew/etc/nginx/servers/` get loaded by nginx and cause
  duplicate-default-server errors. Keep backups OUTSIDE that
  directory (e.g. `~/nginx-ssl.pre-d2`).
- **dock plist env**: real source of truth is `EnvironmentVariables`
  in `~/Library/LaunchAgents/polar.dock.plist` (PlistBuddy-managed).
  `~/polar-dock.env` is **not** sourced by the current dock wrapper.
- **Plugin token discipline**: each plugin's `POLAR_PLUGIN_TOKEN`
  is the plaintext printed ONCE at `/admin-plugins.html` row
  creation. Lost it → "Rotate key" in the UI; copy new plaintext
  into `~/<svc>-svc.env`; bounce the plist.

## Open follow-ups

- [ ] 6 plugin handler-migration PRs (one per plugin)
- [ ] 6 plugin cutover PRs (one per plugin)
- [ ] After wg's 1-week bake passes uneventfully → **Phase 1-D-3**:
      delete `internal/app/dock/wg_*.go` + drop `wg_*` tables in
      `ideamesh` (after backup) + add `/internal/v1/headscale/mint-authkey`
      to dock so wg-svc's `/api/admin/wg-tokens` can re-issue the
      Tailscale PreAuthKey (Phase 1-D-2 dropped that feature
      temporarily).
- [ ] Per-plugin GC + retention policies in launchd plists if any
      plugin grows long-running goroutines that leak (wg-svc's
      stale-device GC is the model).
- [ ] Per-plugin Prometheus alert rules — currently only wg has
      dashboards/alerts from Phase 1.
