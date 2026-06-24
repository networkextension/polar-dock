# Host Network Topology — design draft (polar-hosts)

Status: **DESIGN DRAFT** (2026-06-18). A web view in polar-hosts that draws the fleet's
network topology from each host's NIC data (MAC + IP per interface), styled like the
wg-tokens Status topology. Goal: see at a glance which boxes share a subnet/LAN, spot
IP conflicts / rogue or multi-homed hosts, and deep-link into host + wg detail.

Not built yet — this draft pins the data model, the gaps to fill, the graph model, the
UI, and the phases. Implementation follows sign-off.

---

## 1. What we already have vs. the gap

Agent `host_info_json` (per [[host-info-3tier-plan]]) today carries:
- `ipv4_by_iface : {iface → "192.168.11.57"}` — **IP only, no netmask/prefix**
- `ipv6_by_iface : {iface → [{addr, private}]}`
- `wifi_mac` — **only the Wi-Fi NIC's MAC**, not per-interface
- `wg_pubkeys`, plus the usual hw/cpu/os fields

**Gaps to close (agent host_info additions — backward-compatible, all `omitempty`):**
| New field | Type | Why | Source (darwin / linux / freebsd) |
|---|---|---|---|
| `mac_by_iface` | `{iface → "aa:bb:.."}` | per-NIC MAC = the topology key | `ifconfig`/`networksetup` · `/sys/class/net/<i>/address` · `ifconfig` |
| `ipv4_cidr_by_iface` | `{iface → "192.168.11.57/24"}` | need the **prefix** to derive the subnet | same `ifconfig`/`ip addr` parse that already yields the IP |
| `default_gw` | `"192.168.11.1"` | draw the gateway node per segment | `route -n get default` · `ip route` · `route -n get default` |
| `iface_flags` | `{iface → "up,broadcast"}` (opt) | dim down/no-carrier NICs | netif flags |

`wifi_mac` stays as a convenience alias (= `mac_by_iface[wifi-iface]`). Loopback,
`utunN` placeholders, `awdl0`, link-local-only ifaces are collected but tagged so the UI
can filter them.

**Per-NIC tuple the UI works from:** `{iface, mac, ipv4_cidr, ipv6[], is_wifi, is_wg, up}`.

---

## 2. Topology model — how hosts relate

We can't see physical switches, but **shared IPv4 subnet ≈ same L2 broadcast domain ≈ same
LAN/switch**, which is the useful grouping. So the graph is **bipartite**:

- **Segment node** = a CIDR (e.g. `192.168.11.0/24` home LAN, `10.88.0.0/24` wg mesh,
  `100.64.0.0/10` mesh, a public /29, …). Derived by masking each iface's `ipv4_cidr`.
- **Host node** = a registered host.
- **Edge** = "host has interface X on segment Y", labeled `iface · ip · mac`.
- A **multi-homed host** (NICs on >1 segment) sits *between* its segments and visually
  bridges them — this is how **zen** naturally renders as the hub (LAN `.57/.65` + wg
  `10.88.0.1` + …).

Segment kinds (color-coded): `lan` (RFC1918 /24-ish, green) · `wg` (10.88.0.0/24, blue,
cross-linked to the wg topology) · `cgnat/mesh` (100.64/10, teal) · `public` (orange) ·
`link-local/loopback` (grey, hidden by default).

---

## 3. Anomaly surfacing ("发现异常" — same spirit as the wg/Grafana work)

The builder flags, the UI highlights:
- **IP conflict** — same IPv4 on ≥2 hosts in one CIDR → **red** segment + offending hosts.
- **MAC seen on a new IP** (a box that changed IP — NIC swap / DHCP drift, a known zen pain) → amber.
- **Multi-homed change** — a host newly bridging two segments vs. last snapshot → amber.
- **Stale host** — no heartbeat in N min → node dimmed.
- **(P2) Rogue device** — a MAC/IP on a segment that belongs to no registered host —
  needs an ARP/neighbor scan (see Phase 2), since today we only see *self-reported* NICs.

---

## 4. API (polar-hosts)

`GET /api/hosts/topology` (workspace-scoped, `AuthVerifyWS`) builds the graph server-side
from every host's `host_info_json`:

```jsonc
{
  "segments": [
    { "id": "192.168.11.0/24", "cidr": "192.168.11.0/24", "kind": "lan", "gateway": "192.168.11.1", "host_count": 8 },
    { "id": "10.88.0.0/24",    "cidr": "10.88.0.0/24",    "kind": "wg",  "host_count": 6 }
  ],
  "hosts": [
    { "id": "h_…", "name": "zen", "online": true,
      "ifaces": [
        { "iface": "en10", "mac": "…", "ipv4": "192.168.11.57", "cidr": "192.168.11.0/24", "is_wifi": false, "up": true },
        { "iface": "wgc0", "mac": "",  "ipv4": "10.88.0.1",     "cidr": "10.88.0.0/24",   "is_wg": true } ] }
  ],
  "edges":     [ { "host": "h_…", "segment": "192.168.11.0/24", "iface": "en10", "ipv4": "192.168.11.57", "mac": "…" } ],
  "anomalies": [ { "type": "ip_conflict", "cidr": "192.168.11.0/24", "ipv4": "192.168.11.80", "hosts": ["h_a","h_b"] } ]
}
```

Pure read over existing data; no new storage (parses `host_info_json`). Cheap → cache 15s.

---

## 5. UI — mirror the wg-tokens Status topology

New tab **🌐 拓扑** in the hosts UI (or `host-topology.html` on `hosts.4950.store`), reusing
the wg page's `hubTopology` SVG-render pattern + shared styles.

```
┌───────────────────────────────────────────────────────────────────────────┐
│  Host Network Topology                       [⟳ refresh]  [□ show link-local]│
│  8 hosts · 3 segments · 1 multi-homed · ⚠ 1 IP conflict                       │
├───────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│        ╭───────────────╮                       ╭───────────────╮            │
│        │ 192.168.11.0/24│────── en10 ──────────│      zen       │            │
│        │   LAN  · gw .1 │                       │  (multi-homed) │            │
│        │   8 hosts      │                       ╰──────┬────────╯            │
│        ╰──────┬─────────╯                              │ wgc0                │
│          en0  │  .80 devbox   .82 everest        ╭──────┴────────╮           │
│          ╭────┴────╮  ╭────────╮  ╭────────╮     │  10.88.0.0/24 │           │
│          │ .79 build│  │ devbox │  │everest │ ····│   wg mesh     │           │
│          ╰─────────╯  ╰────────╯  ╰────────╯     │   6 peers     │→ wg page  │
│                                                  ╰───────────────╯           │
│  Hover a host → all NICs (iface · mac · ip). Click → host detail.            │
│  Red = IP conflict · dim = stale · blue edge = wg (deep-links wg topology).  │
└───────────────────────────────────────────────────────────────────────────┘
```

- Segments = labeled rounded rects (color by kind); hosts = pill nodes; edges = lines
  labeled with iface/IP.

### 5a. Layout — layered / 分层 (preferred over radial & force-directed)

The network has a natural hierarchy, so render it as a **layered (Sugiyama) graph**, not a
blob. Layering scheme = **segment-as-lane, lanes ordered by network tier** (top→bottom):

```
   ── 公网 / WAN  (gateway, public /29) ───────────────────────────────
   ── LAN  192.168.11.0/24 ───────────────────────────────────────────
        [.79 build] [.80 devbox] [.82 everest] [zen .57] …
   ── overlay  wg 10.88.0.0/24 ────────────────────────────────────────
        [zen .1 hub] [.2] [.4 stage] [.5 devbox] …   → wg topology
   ── mesh  100.64.0.0/10 ─────────────────────────────────────────────
```

- Each **CIDR = a horizontal lane**; hosts sit in their lane. A **multi-homed host spans
  lanes** via vertical connectors (zen visibly bridges LAN ↕ wg ↕ public) — the bridging
  is the most informative part of the picture.
- Don't hand-place: feed nodes/edges to a **layered DAG layout** (`dagre` / `elk` "layered")
  and let it assign ranks (= lanes) + order within rank to minimise edge crossings.
- Deterministic + readable at fleet scale (~10–30 hosts), and anomalies highlight inside the
  relevant lane.

### 5b. Rendering engine

hosts UI is vanilla-TS bundles (no React), so use a framework-agnostic graph lib with a
built-in layered layout:
- **AntV G6** — prettiest defaults, built-in `dagre`/antv-layered layout, good docs. *(lean pick)*
- **Cytoscape.js** + `cytoscape-dagre`/`cytoscape-elk` — most stable, large ecosystem.
- (React Flow / xyflow only if we ever move hosts UI to React.)

**Visual references to mirror** (好看 + 分层): **UniFi Topology** (gateway→switch→AP→client
hierarchical tree, animated links — closest to our self-host/LAN case) · **Cisco Meraki**
(auto-tiered, expand/collapse) · **Dynatrace Smartscape** (the canonical vertical-layer UX) ·
Datadog/Auvik/Kentik (traffic-weighted, pro-grade).
- Hover host → tooltip listing every NIC (`iface · mac · ipv4/cidr · ipv6`); hover edge →
  that iface's detail; click host → deep-link to host detail (reuse the wg↔hosts crosslink).
- Summary chips up top double as anomaly counters (IP conflict / multi-homed / stale).
- wg segment carries a "→ wg topology" link (ties into [[wg-host-crosslink]]).

---

## 6. Phases

- **P0 (MVP, this draft):** agent adds `mac_by_iface` + `ipv4_cidr_by_iface` + `default_gw`;
  polar-hosts `GET /api/hosts/topology` builder; 🌐 拓扑 page with radial SVG, tooltips,
  host deep-links, summary chips, IP-conflict highlight. (Known-fleet only.)
- **P1:** multi-homed + MAC-moved + stale highlighting; wg-segment ↔ wg topology crosslink;
  filter/legend controls.
- **P2 (bigger, opt-in):** agent ARP/neighbor scan (`arp -an` / `ip neigh`) per segment →
  discover **unregistered** devices on the LAN → rogue-device detection. Scope/privacy
  decision required (scans the user's whole LAN).

---

## 7. Open decisions (recommendations in **bold**)

1. **MAC scope** — all NICs vs. only active (has-IP, up) ones? → **active+up by default, toggle to show all.**
2. **Layout** — → **layered/分层 (segment-as-lane, dagre/elk)**, mirroring UniFi/Meraki/Smartscape; not radial/force-directed (see §5a).
3. **ARP rogue-device scan (P2)** — do it, or known-fleet only? → **known-fleet for P0/P1; ARP behind an explicit opt-in later** (it scans the whole LAN).
4. **Home** — a tab inside the hosts UI vs. a dedicated `host-topology.html`? → **tab in hosts UI** (one less page, shares nav).

Related: [[host-info-3tier-plan]] (host_info source), [[wg-host-crosslink]] (wg↔hosts links + wg star topology to mirror), [[project-polar-hosts-split-brain]].
