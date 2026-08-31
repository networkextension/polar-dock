# Session handoff (2026-06-01)

Context for continuing the Polar work on another box. **No secrets here** — tokens / DSNs / admin tokens live only in the local `~/.claude` memory and are NOT copied (by design). Re-paste a fresh session token when needed.

## Boxes
- **zen / prod** = `locals-Mac-3` — `10.88.0.1` (WG) / `192.168.11.57` (LAN). Runs dock + hosts-svc + plugins. dock DB `ideamesh`, module DBs `polar_*`. psql at `/Applications/Postgres.app/Contents/Versions/17/bin/psql`. dock plist `~/Library/LaunchAgents/polar.dock.plist` (env via plist + `~/.polar/dock.env`).
- **dev** = `locals-Mac-2` — `10.88.0.5` (WG) / `192.168.11.61` (LAN). Full stack; dock DB `polar_dock`; psql `/opt/homebrew/bin/psql`; nginx `/opt/homebrew/etc/nginx/servers/plugins-dev.conf` (reload needs `sudo nginx -s reload`).
- **.35** = `locals-Mac.local` — `192.168.11.35`. Now runs `polar-agent-1135` as a launchd service.

## Shipped this session (merged + deployed)
- **Module OTA (Track 3):** sdk v0.5.1, dock #391, buildings #3/#4, dock-ui #19. Pull-model self-update; live on dev + prod.
- **WG register fix (polar-wg #10):** `wg_devices` uniqueness made **partial on `removed_at IS NULL`** (soft-removed devices no longer block IP/index reuse). Migrated on zen + dev.
- **dock→hosts migration Phase 1 + 3:** polar-hosts #19 (scope to selected `X-Workspace-Id`, not personal default) + polar-dock #393 (dock sheds `/api/agents` read handlers). Deployed to **dev AND zen**. Read/management plane now owned by hosts-svc; nginx routes `/api/{hosts,agents,host-skills,console,skill-catalog,agent}` → `:8095`.
- **Regression smoke:** `modules/polar-hosts/scripts/test/host-agent-smoke.sh` — hits every host/agent endpoint, fails on any 5xx/route-404. **9/0 green** on dev.
- **Register server-URL bug:** config-fixed on zen (`POLAR_PUBLIC_BASE_URL=https://hosts.4950.store:2443` in dock plist). Code fix pending.

## Key facts / gotchas
- Real agent-backed hosts (with `host_info`) live in the **Root** team; the user's default `我的工作区` is empty. Select **Root** to see the fleet (works now that the scoping fix is on zen).
- **Server-side register = empty placeholder** — `host_info` is only collected by a LIVE polar-agent's `hello`. To onboard a box for real: copy the binary + register + run the agent (launchd `polar.agent.plist`, `attach --workdir=…`, KeepAlive).
- zen's dock execs the binary directly and only reads SPECIFIC keys from `~/.polar/dock.env` — arbitrary env there is ignored; put it in the dock **plist EnvironmentVariables**.
- zen dock boot is slow (~50-70s, re-runs migrations) — poll ≥120s, don't read slow-boot as a crash.

## Next: dock→hosts migration Phase 4 (the big one)
Move the live agent connection off dock into hosts-svc — see the detailed plan in `~/.claude/plans/snazzy-wobbling-stonebraker.md`:
- **4a (additive, safe):** real `agentHub` + `/ws/agent` attach handler + agent-token verify in hosts-svc (replace `internal/hosts/stubs.go::agentHubStub`).
- **4b (atomic cutover):** dock→hosts dispatch RPC (`/internal/v1/agents/dispatch/{tool-call,chat-message,skill-start}`) for the LLM loop; repoint polar-agent + nginx `/ws/agent` → `:8095`.
- **4c:** register the shell/vnc browser bridges in hosts-svc + wire supervisors to the real hub.
- **Phase 5:** drop dock's host/agent tables + delete dead dock Go files.

## TODO backlog
- dock-ui **token auto-refresh** (no refresh-on-401; sessions die at 30min — `refresh_token` + `/api/token/refresh` exist, client side not wired; `modules/polar-dock-ui/src/api/http.ts`).
- Register server-URL **code** fix (derive from request Host; don't overwrite operator `--server`).
