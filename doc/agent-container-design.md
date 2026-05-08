# polar-agent Container Image — Design Note

**Status**: design recorded, not implemented. Future work item.

**Date**: 2026-05-07

## Goal

Ship a single OCI image (and optionally a VM image for the macOS-
build case) that bundles polar-agent + every coding tool + git +
common toolchains. Operator runs one `docker run` with three env
vars and the box is ready to receive chat-driven tasks.

```bash
docker run -d --name polar-agent \
  -e POLAR_DOCK_URL=wss://polar.example.com \
  -e POLAR_AGENT_TOKEN=polar_agent_xxxxxxxxxx \
  -e POLAR_BOT_IDS=bot_kimi,bot_claude,bot_codex \
  -v ~/work:/work \
  polar/agent:standard
```

Container starts → reads the three vars → for each bot id spawns
a `polar-agent attach` (one tmux window inside the container, or
one process group with concurrent goroutines) → bots immediately
respond to chat.

## Why containerize

Today a teammate joining the platform manually:

1. Downloads release tarball, picks `polar-agent_<os>_<arch>` for
   their machine
2. Installs kimi-cli, claude, codex separately (each with its own
   install path / login flow)
3. Installs git if missing
4. `polar-agent login --server=… --token=…`
5. Edits `~/.polar/start_agents.env` with their bot IDs
6. `./scripts/start_agents.sh start`
7. Logs into kimi / claude / codex separately if they want each

Six steps × every machine = friction. With a container:

```bash
docker run -d ... polar/agent:standard
```

…and the box is operating. Updates: `docker pull` + restart.

## Image variants

| Tag | Contains | Size estimate | Use case |
|---|---|---|---|
| `polar/agent:slim` | polar-agent + git | ~15 MB | tool-loop mode only (no passthrough) |
| `polar/agent:standard` | slim + kimi-cli + claude + codex | ~400 MB | typical multi-tool A/B |
| `polar/agent:full` | standard + Go + Node + Python + common build chain | ~1.2 GB | running build commands inside the agent |
| Per-stack variants | standard + one specific stack (e.g. `polar/agent:rust`, `polar/agent:swift-toolchain`) | varies | dedicated CI-style runners |

Build base: `debian:bookworm-slim` or `alpine` (alpine fights
glibc-linked claude binary, so debian is safer). Multi-arch via
buildx so Apple Silicon hosts get arm64 image without QEMU
emulation.

## Startup contract

The container's entrypoint reads these env vars:

| Variable | Required | Description |
|---|---|---|
| `POLAR_DOCK_URL` | yes | Where polar-agent dials (e.g. `https://polar.example.com` — entrypoint converts to `wss://...` for ws/agent) |
| `POLAR_AGENT_TOKEN` | yes | Long-lived bearer from `POST /api/agent/tokens` |
| `POLAR_BOT_IDS` | yes | Comma-separated bot_user_ids |
| `POLAR_BOT_TOOL_MAP` | optional | JSON mapping `{bot_id: tool}`; default = use bot's `preferred_tool` server-side |
| `POLAR_WORK_ROOT` | optional | Default `/work`; per-bot subdir derived |
| `POLAR_KIMI_TIMEOUT` | optional | Per-tool wall-clock cap, default 1h |
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `KIMI_API_KEY` | optional | If unset and `~/.kimi` etc. are mounted as volumes, OAuth login state from host is reused |

Entrypoint logic:

```bash
#!/bin/sh
set -e
mkdir -p "${POLAR_WORK_ROOT:-/work}"
polar-agent login --server="${POLAR_DOCK_URL}" --token="${POLAR_AGENT_TOKEN}"

IFS=','
for bot in $POLAR_BOT_IDS; do
  tool=$(echo "$POLAR_BOT_TOOL_MAP" | jq -r ".\"$bot\" // \"\"")
  workdir="${POLAR_WORK_ROOT}/$bot"
  mkdir -p "$workdir"
  polar-agent attach \
    --bot="$bot" \
    --workdir="$workdir" \
    ${tool:+--tool=$tool} \
    --verbose &
done
wait
```

## Auth strategy for local coder tools

kimi-cli / claude / codex each have their own login state in
`~/.kimi` / `~/.claude` / `~/.codex` (browser OAuth). Two paths:

### Path A: bind-mount host auth dirs (zero-config)

```bash
docker run -d \
  -v ~/.kimi:/root/.kimi:ro \
  -v ~/.claude:/root/.claude:ro \
  -v ~/.codex:/root/.codex:ro \
  ...
```

Container reuses operator's already-logged-in state. Simple but
ties container to a specific host's login dir.

### Path B: API key env (cloud-friendly)

```bash
docker run -d \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e OPENAI_API_KEY=sk-... \
  ...
```

Each tool reads its own env var (claude uses `ANTHROPIC_API_KEY`,
codex uses `OPENAI_API_KEY`, kimi uses its login or
`KIMI_API_KEY`). Friendlier for cloud / ephemeral runners but
requires the operator to manage keys.

Image supports both — entrypoint picks whichever the env shows.

## Workdir model

`/work` is a volume mount. Per-bot subdirectory comes from the
existing `workdir_subpath` plumbing (PR #60), so:

- bot_kimi   → `/work/bot_kimi/<project_id>/`
- bot_claude → `/work/bot_claude/<project_id>/`
- bot_codex  → `/work/bot_codex/<project_id>/`

Existing project-level isolation continues to work; the container
just provides the host-level volume.

## Multi-bot in one container vs one-bot-per-container

The entrypoint above runs **N polar-agent processes inside one
container** (mirrors `scripts/start_agents.sh` minus tmux). Pros:
single docker-compose service, shared volume, single pull.

Alternative: docker-compose with N services (one bot each). Pros:
restart isolation (one bot's polar-agent crashing doesn't take
its siblings). Cons: N times the image instances.

**Recommend**: ship the multi-bot entrypoint as default. If a user
wants strict isolation they trivially run N containers with
different `POLAR_BOT_IDS`.

## Limits

1. **macOS / iOS native builds — not in Docker.** Xcode + the
   iOS Simulator only run on macOS hosts. For iOS coding tasks
   the container approach doesn't help; we still need polar-agent
   running on a real Mac. See "macOS runtime variants" below for
   the Mac-specific story (VM and bare-metal options).
2. **Browser OAuth flows in headless container.** First-time
   login for kimi / claude / codex needs a browser. In the
   container path, operator does login once on host, mounts auth
   dir into container. Or uses API keys (Path B above).
3. **Mounted volume permissions.** Container writes to
   `/work/<bot>/<project>` as a uid that must match the host
   user's. Use `--user $(id -u):$(id -g)` to avoid root-owned
   files on the host.
4. **kimi-cli is Python via uv.** Image gets the uv tool-install
   directory baked in (`/root/.local/share/uv/...`); ~150 MB
   of standard image size goes to this.
5. **Update cadence.** kimi-cli / claude / codex release fast.
   Image needs CI to rebuild on tool releases or auto-update
   inside the container at startup. CI rebuild is simpler.

## Network / proxy

Existing `tools.json` `env` field (PR #65) lets the operator set
per-tool `https_proxy`. The container picks up these via the
same JSON file (mounted from host or baked in). Fits naturally —
no container-specific network config needed.

## Build topology

```text
.
├── docker/
│   ├── slim/Dockerfile           # debian-slim + polar-agent + git
│   ├── standard/Dockerfile       # FROM slim + uv install kimi-cli + npm install claude + brew tap codex
│   ├── full/Dockerfile           # FROM standard + Go + Node + Python + ...
│   └── entrypoint.sh             # the for-loop above
└── .github/workflows/
    └── docker-image.yml           # buildx for amd64 + arm64; tag on release
```

CI: GitHub Actions on tag push → buildx → push to ghcr.io and/or
Docker Hub.

## Security note

The image runs **arbitrary code from the platform's chat** —
that's the whole point of the passthrough mode. Treat it like any
build runner:

- One container per workspace ideally, never share across
  untrusted tenants
- Rotate `POLAR_AGENT_TOKEN` periodically (server-side
  invalidation supported via `DELETE /api/agent/tokens/:id`)
- The volume mount is the trust boundary — anything inside `/work`
  is reachable; don't mount the host's home directory naively
- Network egress: container can call any URL by default. If
  needed, apply a NetworkPolicy or run behind an egress proxy

## Estimate

| Scope | Estimate | Assumes |
|---|---|---|
| `slim` + `standard` images + entrypoint + CI | **1 wk** | release tarball already builds polar-agent for linux/amd64 + linux/arm64 (PR #76) |
| `full` + per-stack variants | **+0.5 wk** each | Dockerfile-only work |
| docker-compose example + docs | **+0.5 wk** | including volume mount semantics + auth strategy doc |
| Multi-bot entrypoint + auto-update + health check endpoint | **+1 wk** | extra polish for production-ish use |

**Total realistic v1**: 1.5 - 2.5 weeks.

## macOS runtime variants

Linux containers cover ~90% of coding tasks (CLI, web, Python,
Go, Rust, Node). For iOS / macOS native builds (SwiftUI / UIKit
apps, Xcode projects), polar-agent has to run on a real macOS
host. Apple Silicon (M-series) makes two paths workable:

### Path 1: macOS VM on Apple Silicon host

Apple's Virtualization.framework on M-series Macs natively runs
macOS guests with near-bare-metal performance. Tools that wrap it:

- **tart** ([cirruslabs/tart](https://github.com/cirruslabs/tart))
  — single-binary CLI, designed for CI runners. Pull a pre-built
  macOS image, run it, mount a host directory, ssh in.
- **macOS Containers** (Tahoe and later) — Apple's own
  container runtime for macOS guests. Newer; smaller ecosystem.
- **VMware Fusion** / **Parallels** — heavier, GUI-oriented;
  not the right fit for fleet operations.

**Apple's licensing constraint**: macOS guests are allowed only
on Apple hardware, and historically only **2 VMs per host**.
Plenty for a 1-2 person team running their own farm; insufficient
for a SaaS where 100 customers each want a Mac VM.

**Image contents** (parallel to the Linux `standard` image):
- macOS guest with Xcode + Command Line Tools + Homebrew
- polar-agent (darwin/arm64 binary)
- kimi-cli, claude, codex installed via brew + uv
- git
- A first-boot script that reads the same three env vars
  (`POLAR_DOCK_URL`, `POLAR_AGENT_TOKEN`, `POLAR_BOT_IDS`)
  injected via tart's metadata or a mounted plist

```bash
tart pull polar/agent-macos:standard
tart set agent --memory 8192 --cpu 4 \
  --metadata POLAR_DOCK_URL=wss://polar.example.com \
  --metadata POLAR_AGENT_TOKEN=polar_agent_xxx \
  --metadata POLAR_BOT_IDS=bot_swift,bot_macos_app
tart run agent
```

Trade-offs:

- ✅ Snapshots / rollback
- ✅ Multiple concurrent VMs (up to Apple's per-host cap) for
  parallel iOS builds
- ✅ Same image can be cloned across host machines
- ❌ ~30 GB per image (Xcode is huge)
- ❌ Apple licensing puts a ceiling on fleet size
- ❌ First-boot Xcode `xcrun --first-launch` provisioning takes
  minutes; image must be pre-warmed

### Path 2: Bare-metal Mac mini / Studio

Single Mac runs polar-agent directly via `start_agents.sh`. No
VM overhead. Best if you only have one workspace's worth of
iOS work happening at a time and no isolation requirement.

Use case: a Mac mini in a closet (or Mac Studio in a rack)
dedicated to platform builds. Single user account, ssh-managed.
Same install path as a developer laptop:

```bash
# on the Mac (one-time)
brew install kimi-cli
# claude / codex per their official install
go build -o ~/.local/bin/polar-agent ./cmd/polar-agent
~/.local/bin/polar-agent login --server=... --token=...
launchctl load ~/Library/LaunchAgents/polar-agent.plist
```

A `LaunchAgents` plist makes polar-agent auto-start on login —
the Mac becomes a passive worker. No Docker / no VM, just a Mac
host. Cheap on resources but no isolation between bots; if one
kimi run trashes the workdir it affects all bots on this host.

Trade-offs:

- ✅ Lowest-friction setup if you already own a Mac
- ✅ Maximum performance — no virtualization overhead
- ✅ Zero licensing concern (it IS the host)
- ❌ Single failure domain — kimi crashes / fills disk → all
  bots affected
- ❌ Hard to script "fresh Mac" provisioning (App Store login,
  Xcode install, etc.)
- ❌ One workspace can read another's `/work` dir if they share
  a Mac account

### Recommendation

For Polar's current scale (1-3 person team):

| Stage | Recommended runtime |
|---|---|
| Now | bare-metal Mac (the developer's own machine), one polar-agent attach with `--workdir=$HOME/work_<tool>` |
| Next | bare-metal Mac mini in a closet running launchd, dedicated to platform iOS builds |
| Future scale | tart-based macOS VMs on a Mac Studio host, when parallel iOS builds become bottleneck |

Linux Docker (`polar/agent:standard`) covers the Linux/web/CLI
side at any stage.

Sequence to ship:

1. Linux container (already designed above) — 1.5-2.5 wk
2. macOS bare-metal `LaunchAgents` plist + setup doc — 0.5 wk
3. tart-based macOS VM image — 1.5-2 wk (only when needed)

Mac VM is the most niche and most expensive (Xcode footprint,
Apple licensing); ship last.

## Trigger condition

Ship when one of:

- A teammate asks to run polar-agent on a Linux server (no Mac to
  install on)
- Cloud-runner story comes up ("CI runs polar-agent in a
  container against task queue") — this is also when self-hosted
  enterprise customers would want it
- Operator burnout from "set up everything again on this 4th
  machine" hits

Until then, the existing `release.sh` multi-platform tarball + the
manual `start_agents.sh` flow is good enough for the team's
2-3 personal machines.
