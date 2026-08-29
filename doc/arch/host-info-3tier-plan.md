# Host Info — 3-Tier Collection Plan

Status: proposed (2026-06-06). Owner: Kong. Spans **polar-agent → polar-dock → polar-hosts → polar-dock-ui**.

> **P1 deploy status (2026-06-06):** node_exporter live on 5 fleet Macs (node-77/stage-78/
> node-81/node-82/dev), bound `0.0.0.0:9100`, scraped by zen over **LAN** via the `fleet-node`
> job — cluster CPU/mem/disk in Prometheus/Grafana. **macmon GPU% DONE** on 4 stable boxes
> (.77/.78/.81/.82) via node_exporter textfile-collector bridge (`macmon_gpu_usage_ratio` etc.
> ride :9100); zen's own macmon scraped separately. **Pending:** dev box macmon (was offline
> at deploy). See [[reference-observability-stack]] "Fleet-wide node metrics".

## Goal

Tier host facts by *how often they change* and *who consumes them*, so the platform can
eventually **schedule tasks onto the right host by live load**:

| Tier | Cadence | Fields | Where it lives | Consumer |
|---|---|---|---|---|
| **1. Register** | once, at enroll | SoC/model, memory, GPU, **wifi MAC** | polar-hosts `host_info_json` | inventory / identity |
| **2. Connect (hello)** | per attach | OS version, **disk capacity, fan-present, battery-present** | polar-hosts `host_info_json` | inventory / capability filter |
| **3. Heartbeat** | scrape (~15–30s) | **CPU% / GPU% / mem% (current)** | **Prometheus** (not hosts DB) | task dispatcher (readiness) |

> **Decision (2026-06-06):** Tier 3 utilization is time-series and we already run
> Prometheus + Grafana + a **macmon** exporter (Apple-Silicon CPU/GPU/ANE/mem/power). Do
> **not** duplicate it into polar-hosts columns. Utilization flows agent-host → exporters →
> Prometheus; the dispatcher queries **Prometheus `/api/v1/query`** directly (Grafana stays the
> human dashboard layer). polar-hosts stores only the **`host_id ↔ Prometheus label` mapping**
> so query results correlate back to a host. polar-hosts owns Tier 1+2 (inventory facts that
> don't belong in a TSDB) + online-state.

## Current implementation (baseline)

- **Agent** (`modules/polar-agent/cmd/polar-agent/hostinfo/`): `HostInfo` struct collected once
  (`sync.Once`), sent in `register` body **and** the WS `hello` frame. Already has
  `hw_model, cpu_*, memory_bytes, gpu{vendor,model,cores}, os_*, kernel, machine_uuid,
  ipv4_by_iface`. Darwin via `sysctl`/`sw_vers`/`system_profiler`/`ioreg`.
- **Heartbeat** = `skill.advertise` every 60s (`loop.go:269`) — **no metrics**. Pong → `last_seen_at`.
- **Dock**: `hello` handler (`agent_handlers.go:391`) forwards opaque `host_info` to
  polar-hosts `POST /internal/v1/hosts/hello` (`polar_hosts_hello.go`, loopback, SHA-deduped).
  `skill.advertise` → `/internal/v1/hosts/touch`.
- **polar-hosts**: `host_info_json` JSONB (opaque, agent-owned), `mem_peak_bytes`/`cpu_peak_pct`
  (ratcheted peaks, **unused**), `last_seen_at`. Read via `GET /api/hosts`, `/api/hosts/:id`,
  `/api/agents` (joins `host_info_json->>'hw_model'`).

**Key leverage:** `host_info_json` is opaque → Tier 1 & 2 new facts need **no migration**, only
agent collection + UI rendering. Tier 3 needs new "now" columns + a metrics frame.

---

## Phase P0 — Tier 1 + Tier 2 static facts (cheap, no schema change)

All ride the existing `HostInfo` blob (register + hello). Pure agent-side additions + UI.

**Agent** (`hostinfo/hostinfo.go` + per-OS files):
- `WifiMAC string json:"wifi_mac"` — darwin: `networksetup -listallhardwareports` → find "Wi-Fi"
  device → `ifconfig <dev> ether`; linux: `/sys/class/net/*/address` where wireless;
  freebsd: best-effort.
- `DiskTotalBytes uint64 json:"disk_total_bytes"` + `DiskRoot string` — `syscall.Statfs("/")`
  (`Blocks*Bsize`), stdlib, no shell.
- `HasBattery bool json:"has_battery"` — darwin: `pmset -g batt` / `ioreg -c AppleSmartBattery`
  presence; linux: `/sys/class/power_supply/BAT*` exists.
- `HasFan bool json:"has_fan"` + `FanCount int json:"fan_count"` — darwin: SMC `FNum` key
  (reuse the no-sudo SMC read from `~/github/macosInc`, see reference-macos-smc-tooling) or
  `powermetrics`-free heuristic; linux: `/sys/class/hwmon/*/fan*_input` count. Defensive: if
  unreadable, omit (don't block).
- OS version already present (`os_version`/`os_build`) — no change.

> Note: disk *capacity*, fan/battery *presence* are static, so collecting at register is fine;
> they reach polar-hosts on the very first hello. The "connect" tier semantics are satisfied
> because hello fires on every attach.

**Dock**: no change (passes `host_info` opaque).

**polar-hosts**: no schema change. Optionally add partial indexes later if we filter on them
(mirror the existing `host_info_json->>'virt'` / `->>'memory_bytes'` indexes).

**polar-dock-ui** (`hosts` detail page): render the new keys (wifi MAC, disk GB, 🔋/🌀 chips).

**Ship:** one agent PR (new fields + 3 OS collectors), one ui PR. Build agent, redeploy to
fleet (.57/.61/.78/.81/.82/.83 + build box) via the existing OTA/rsync path.

> **P0 CODE-COMPLETE (2026-06-06) — 2 PRs:**
> - **polar-agent #18** (agent collectors, 2 commits): `HostInfo` gained `wifi_mac`,
>   `disk_total_bytes`, `has_battery` (*bool), `has_fan` (*bool — darwin model heuristic /
>   linux hwmon tachometer), `model_name`. **All 3 platforms:** darwin
>   (networksetup/statfs/pmset/system_profiler), linux (pure sysfs:
>   /sys/class/net/*/wireless, power_supply type=Battery, hwmon/fan*_input), freebsd
>   (statfs + hw.acpi.battery.units; wifi/fan skipped — headless server). Pure parsers
>   unit-tested; cross-compiles linux/amd64+arm64, freebsd/arm64, darwin; real-Mac smoke green.
>   Rides the existing host_info blob (no dock/hosts schema change).
> - **polar-hosts #24** (UI): host **list** basic-info line shows model + agent count; host
>   **detail** gains a 硬件信息 grid (model/cpu/mem/disk/gpu/os/wifi-mac/virt + 🔋/🌀 chips) and
>   a 配 Agent section (agents bound to the host via GET /api/hosts/:id/agents). tsc+vite green.
>
> **Remaining P0 (deploy, after zen battery swap):** merge both PRs → redeploy agent to the
> fleet (OTA/rsync) → rebuild+ship hosts-svc UI to zen → verify the new fields render.

---

## Phase P1 — Tier 3 utilization via Prometheus (no hosts metrics columns, no metrics frame)

**Baseline (verified 2026-06-06):** zen's `prometheus.yml` scrapes **only localhost** —
node_exporter(9100), postgres, redis, nginx, dock(8080/metrics), and **macmon(9092)**
(Apple-Silicon CPU/GPU/ANE/mem/power). The rest of the fleet (.61/.78/.81/.82/.83 + build box)
runs the agent but **no exporters and is not scraped**. So the work is: get the fleet into
Prometheus, then point the dispatcher at Prometheus.

### P1a — Exporters on every fleet host
Deploy **macmon** (GPU/CPU/mem/power — the part node_exporter can't do on Apple Silicon) +
**node_exporter** (disk/net/fs) to each box. Two options:
- (simple) rsync the two binaries + launchd plists, mirror the zen setup.
- (better, later) an agent **`metrics` skill** that ensures both exporters' launchd are loaded
  and healthy — self-healing, rides the OTA path. Defer to a follow-up; P1 ships the simple way.

### P1b — Bind for remote scrape (multi-interface)
A single exporter process binds **one** `--web.listen-address` (one socket) — it cannot bind two
specific addresses. To be reachable on more than one interface (LAN **and** WG), bind
`0.0.0.0:9100` (all interfaces) and restrict with the macOS **pf** firewall to the trusted
scrape sources (zen's LAN IP `192.168.11.57` + WG IP `10.88.0.1`). macmon already binds all
ifaces (`*:9092`) — it's multi-interface as-is. Don't leave node_exporter on `127.0.0.1` only.

> Binding `0.0.0.0` is what makes the host scrapeable over *whichever* path Prometheus chooses;
> pf (not the bind address) is the access control.

### P1c — Prometheus scrape config (zen): prefer LAN, WG as fallback
"Which interface the scrape uses" is decided by the **target address**, not the exporter. Each
host has multiple known addresses — LAN (`hosts.lan_addrs_json`, e.g. `192.168.11.81`) and WG
(`wg_devices.device_ip`, e.g. `10.88.0.8`). **The current fleet is on the same /24 as zen, and
WG is a lossy overlay** ([[project-home-lab-topology]]) — so the generator picks the **LAN IP for
same-subnet hosts** and the **WG IP only for off-LAN / remote** ones (e.g. the .64 FreeBSD box,
future external customer hosts). One best address per host, labelled with the polar `host_id`:
```yaml
  - job_name: fleet-node
    static_configs:
      - targets: ["192.168.11.81:9100"]   # LAN — same subnet as zen, no WG overhead
        labels: { host_id: "dev_7236f810bfaccfc0", hostname: "1181" }
      - targets: ["10.88.0.x:9100"]        # WG — only for off-LAN/remote hosts
        labels: { host_id: "…", hostname: "…" }
  - job_name: fleet-macmon
    static_configs:
      - targets: ["192.168.11.81:9092"]
        labels: { host_id: "dev_7236f810bfaccfc0", hostname: "1181" }
```
> Keep this target list **generated**, not hand-maintained — a small job (or the hosts module)
> emits the scrape file from `hosts.lan_addrs_json` ⊕ `wg_devices.device_ip`, choosing the
> preferred reachable address per host (LAN first, WG fallback). New boxes auto-appear; a box
> that drops off the LAN automatically fails over to its WG address.

### P1d — polar-hosts: just the mapping (one column, no metrics)
Migration adds a single nullable column so the dispatcher/UI can correlate a host to its series:
```sql
ALTER TABLE hosts ADD COLUMN metrics_label text;  -- = the host_id label used in Prometheus
```
(Or skip even this and reuse `hosts.id` directly as the label, if we set the scrape label =
`host_id`. Then **zero schema change** — `metrics_label` only earns its keep if the Prom label
must differ from `hosts.id`.) **No `*_now` columns, no `/internal/v1/hosts/metrics` endpoint,
no `host.metrics` frame.** All of that is deleted from the plan.

### P1e — Dispatcher + UI read from Prometheus
- A small dock-side helper `promQuery(ctx, query) ` → `GET http://127.0.0.1:9090/api/v1/query`
  (instant PromQL). Example readiness query per host:
  `100 - (avg by (host_id)(rate(node_cpu_seconds_total{mode="idle"}[1m])) * 100)` for CPU%,
  macmon series for GPU%, `node_memory_*` for mem%.
- Host detail page (polar-dock-ui): embed the existing **Grafana** panels (the grafana-embed
  work, task #9) filtered by `host_id` — humans get the rich view; the dispatcher gets the raw
  instant query.

**Ship:** ops change (exporters + scrape config on zen, no app build) + optional 1-line
hosts migration + dock `promQuery` helper + UI Grafana embed. Far smaller than the original
"4 PRs + migration + new frame/columns/endpoint."

---

## Phase P2 — Dispatch readiness (only when a task-dispatcher consumes it)

- "Eligible host" = skill match (polar-hosts `host_skills`) ∧ live load under threshold
  (Prometheus instant query) ∧ series **fresh** (Prometheus `up{host_id=…}==1` and last scrape
  recent — staleness = host unreachable/offline → not dispatchable).
- Surface in UI: green/amber/red readiness chip (skill+load+freshness composite).
- Hook into the agent task path ([[project-agent-proxy-and-tasks]] P3 durable pull-queue): the
  scheduler picks a host by Prometheus load, then dispatches via the existing WS channel.

Deferred — no new schema; consumer-side logic that joins polar-hosts (capability/identity) with
Prometheus (live load).

---

## Cross-cutting decisions

1. **Tier 1+2 = JSONB blob in polar-hosts, no migration.** Inventory facts (HW, capacity,
   presence) live where inventory lives.
2. **Tier 3 = Prometheus, not the hosts DB.** Utilization is time-series; reuse macmon +
   node_exporter + Grafana. polar-hosts holds at most a label mapping. Dispatcher queries
   **Prometheus `/api/v1/query`** directly; Grafana stays the human panel layer.
3. **Pull-scrape needs reachability; prefer LAN, WG as fallback.** The current fleet shares
   zen's /24, so scrape over **LAN** (WG is a lossy overlay — don't route metrics through it
   when a direct LAN path exists). WG IP is the fallback for off-LAN/remote hosts. Exporters
   bind `0.0.0.0` + pf so either path works. **Caveat:** future *outbound-only* customer agents
   (no LAN, no WG, just the WS to dock) aren't scrapeable at all — they'd need WG, a Prometheus
   `remote_write`/pushgateway from the agent, or a thin heartbeat-metric fallback. Not built
   now; **noted** so we don't design ourselves into a corner.
4. **Generated scrape targets.** Emit the fleet target list from `wg_devices`⊕`hosts` rather
   than hand-editing prometheus.yml, so onboarding a box auto-adds it to monitoring.
5. **Defensive collectors (Tier 1/2).** Any probe needing a tool/permission we lack → omit the
   field, never block register/hello. All probes time-boxed (≤2s, GPU ≤5s).
6. **Darwin-first.** Fleet is all darwin/arm64; linux/freebsd collectors are best-effort stubs.

## Rollout order

**P0** (static HW + capacity facts, agent + UI, no migration — shippable in one sitting) →
**P1** (exporters on fleet + WG-scoped scrape config + generated targets + dock `promQuery` +
Grafana embed — mostly ops, small code) → **P2** (dispatcher joins hosts capability with
Prometheus load, when a task-queue consumer exists).
