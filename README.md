# Polar-

Polar- is an AI-assisted product prototyping project built to explore one practical question:

How far can a small but real full-stack application be pushed when design, implementation, iteration, and documentation are continuously developed with AI coding tools in the loop?

The repository started as a compact auth + Markdown prototype and has grown into a multi-module platform. It is still **not** intended as a large-scale production system — it remains an active engineering experience project, optimized for AI-paired iteration speed, complete user journeys, and demonstrable product behavior.

## Project Character

- AI-assisted by design, not a one-off experiment but an active development workflow
- Built as a usable prototype rather than a throwaway mock
- Complete user journeys before excessive abstraction
- Optimized for iteration speed, clarity, and demo-ready behavior

## Capabilities at a Glance

The platform is organized into modules. Each module is workspace-scoped (a workspace = a team; every user has at least a personal workspace).

- **Authentication** — email/password, Passkey/WebAuthn, session cookies (30 min access + 30 day rotating refresh), Redis-backed sessions, login geolocation via GeoLite2.
- **Teams / Workspaces** — every resource (LLM configs, bots, projects, posts, attachments, iOS apps, etc.) is keyed by `workspace_id`. Personal team is auto-created per user; multi-member teams support roles (owner / admin / member), invitations, and audit logs. See [`teams.md`](teams.md).
- **Bots + Chat** — chat threads between users and bots. Bots route to LLMs via OpenAI-compatible / Anthropic / Gemini / xAI Responses / Volces (Doubao) Responses APIs. Multi-attachment chat messages backed by the unified attachments table.
- **AI Agent Workflow (Projects)** — write a PRD → AI decomposes into features → user picks features → plan generated → tasks created → user "picks up" a task to a bot → bot starts working in chat. See [`DEV_PLAN.md`](DEV_PLAN.md).
- **polar-agent CLI** — Go client that pairs a developer's Mac/Linux/Windows machine with a Polar bot. Two modes:
  1. **Tool loop** — platform LLM drives `read_file` / `write_file` / `list_dir` / `run_cmd` calls, the agent executes them in a pinned working directory.
  2. **Passthrough mode (`--kimi`)** — agent shells out to a local coding agent (today: kimi-cli) per chat message; the platform LLM is bypassed and the user's own subscription does the work. Streams stdout/stderr in real time. Multi-tool config (claude / codex / etc.) is on the roadmap.
- **Posts** — text posts + image/video uploads + zip/pdf/doc/docx/ppt/pptx attachments. "Task" posts (gigs) have applications, results, deadlines.
- **Markdown** — public and private Markdown entries with sharing links, summaries, cover images.
- **iOS Distribution (iosdist)** — IPA upload, App Store Connect bridge (ASC profile import), zsign re-sign pipeline, codesign route as escape hatch, public OTA share links. See [`iosapp.md`](iosapp.md).
- **Video Studio (Seedance)** — text-to-video generation via Volcano Engine Seedance, with project containers and asset library.
- **Logs Service** — categorical append-only log table (sign / team_audit / deploy / agent / server) for admin debugging. Production environments will swap to Elasticsearch; the local table is dev-only.
- **Latch** — admin tooling, service node management. See [`doc/latch.md`](doc/latch.md).
- **Visual Themes** — default + monochrome, persisted per browser.

## Architecture

```text
                ┌───────────────────────┐
                │   ui/ (browser SPA)   │
                │   esbuild + express   │
                │   dev proxy at :3000  │
                └────────────┬──────────┘
                             │ /api/*
                             ▼
                ┌───────────────────────┐         ┌──────────────────┐
                │   dock (Go + Gin)     │◀───────▶│  PostgreSQL      │
                │   :8080               │         │  (workspaces,    │
                │   - auth, teams       │         │   bots, posts,   │
                │   - bots, ai_agent    │         │   chat, projects,│
                │   - projects pipeline │         │   iosdist, ...)  │
                │   - iosdist           │         └──────────────────┘
                │   - chat WS hub       │         ┌──────────────────┐
                │   - agent WS hub      │◀───────▶│  Redis           │
                │   - markdown, posts   │         │  (sessions)      │
                │   - logs, latch       │         └──────────────────┘
                └─────┬───────────┬─────┘
                      │           │
            chat WS   │           │  /ws/agent
                      ▼           ▼
              ┌───────────┐  ┌──────────────────────────┐
              │ browser   │  │ polar-agent (Go CLI)     │
              │  client   │  │ - tool loop mode         │
              └───────────┘  │ - kimi passthrough mode  │
                             │ - exec(read_file/...)    │
                             │ - spawn kimi-cli         │
                             │   (or claude / codex     │
                             │    when configured)      │
                             └──────────────────────────┘
                                       │
                                       │  in workdir
                                       ▼
                             ┌──────────────────────────┐
                             │ user's local repo        │
                             │ (any OS — agent is       │
                             │  cross-platform Go)      │
                             └──────────────────────────┘
```

### Design notes

1. **Complete flows first.** A feature is shipped when register → login → use → produce-output works end-to-end. Speculative system design is deferred until the flow demands it.
2. **Workspace model uniformly applied.** Every domain table carries `workspace_id`; routes pick it up via the `X-Workspace-Id` header (defaulted to the caller's personal team if absent). The platform's "system" workspace owns genuinely platform-wide resources (e.g., shared LLM configs, packtunnel).
3. **Sessions, not JWTs.** Browser-first flows benefit from atomic Redis-backed revocation and a global `/api/token/refresh` interceptor. See [`doc/auth.md`](doc/auth.md), [`doc/auth-refresh.md`](doc/auth-refresh.md).
4. **Lightweight frontend.** TypeScript + esbuild produces small bundles per page. No heavy framework — easy to inspect, modify, and refactor with AI assistance.
5. **AI as an agent, not a code generator.** When the bot has a polar-agent attached, the chat itself becomes the runtime — content flows from chat → WS → local kimi-cli → stdout → chat. The platform routes bytes; the local tool does the work.
6. **Attachments unified.** A single `attachments` table backs posts and chat (junction tables `post_attachments` / `chat_message_attachments`). Content-addressed by sha256 within each workspace, with a 100 MiB single-file cap. See [PR #54](https://github.com/networkextension/Polar/pull/54) for the refactor.
7. **GeoLite + R2 + SMTP optional.** Missing GeoLite2 → login still works without geolocation. Missing R2 → uploads use local filesystem. Missing SMTP → email verification feature degrades but auth core stays alive.

## Technology Stack

### Backend

- Go (Gin)
- PostgreSQL — embedded schema in `internal/app/dock/store.go::openDB()` with `IF NOT EXISTS` + ALTER TABLE migrations applied at boot
- Redis — sessions
- WebSocket (gorilla/websocket) — chat + polar-agent transport
- WebAuthn — Passkey
- GeoLite2 — login IP geolocation
- AES-256-GCM at-rest encryption — iOS distribution resources via `IOSDIST_RESOURCE_KEY`

### Frontend

Lives in its own repo: **[networkextension/polar-dock-ui](https://github.com/networkextension/polar-dock-ui)** (private).

- TypeScript + esbuild
- Vanilla JavaScript modules (no framework)
- `marked` for Markdown rendering
- `express` for local dev hosting and API proxying

### Local Coding Agents (optional, paired via polar-agent)

- kimi-cli (Moonshot) — passthrough mode supported today
- claude (Anthropic) — multi-tool config on roadmap
- codex (OpenAI) — multi-tool config on roadmap

## Repository Structure

```text
.
├── cmd/dock                  # Backend entrypoint (Go)
├── cmd/polar-agent           # Local-side CLI: pairs user's machine with a bot
├── internal/app/dock         # Core backend logic
│   ├── store.go              # Embedded SQL schema + most data access
│   ├── ai_agent*.go          # AI agent dispatch (LLM call, tool loop, kimi passthrough)
│   ├── attachments_*.go      # Workspace-scoped uploads + junctions
│   ├── projects_*.go         # PRD decompose, plan generate, task pickup
│   ├── teams_*.go            # Workspace + members + invitations
│   ├── iosdist_*.go          # iOS distribution + ASC + signing
│   └── ...
├── doc                       # Module-level documentation
├── scripts                   # Setup, migrations, release helpers
└── data                      # Runtime data (uploads, GeoLite db, ...)
```

## Companion Repositories

The dock is the API + agent core. Everything else lives in its own repo
under [networkextension/](https://github.com/networkextension):

| Repo | Visibility | What it is |
|---|---|---|
| **[polar-dock-ui](https://github.com/networkextension/polar-dock-ui)** | private | The web UI — HTML pages + TypeScript bundles served in front of this dock. Built with esbuild → `dist/`. CI on every push. |
| [polar-sdk](https://github.com/networkextension/polar-sdk) | public | Go SDK + HMAC plugin-token helpers shared by every plugin service |
| [polar-agent](https://github.com/networkextension/polar-agent) | public | Local-side CLI: pairs a user's machine with a bot, runs skills (shell, VNC, MCP, library, …) |
| [polar-hosts](https://github.com/networkextension/polar-hosts) | public | Hosts + skill catalog + workspace-scoped skill installs |
| [polar-wg](https://github.com/networkextension/polar-wg) | public | wg-mac mesh control plane + Headscale-compat self-hosted coordinator |
| [polar-library](https://github.com/networkextension/polar-library) | public | Reverse-engineering library: devices, firmwares, functions |
| [polar-projects](https://github.com/networkextension/polar-projects) | public | PRD decompose → plan → task pickup |
| [polar-video](https://github.com/networkextension/polar-video) | public | Video studio: provider dispatch, polling, render |
| [polar-expense](https://github.com/networkextension/polar-expense) | public | Family expense ledger + OCR/multimodal extract |
| [polar-latch](https://github.com/networkextension/polar-latch) | public | Latch profiles + rules + proxy |
| [polar-packtunnel](https://github.com/networkextension/polar-packtunnel) | private | Pack-tunnel rules + proxy (split-out, not yet open) |
| [polar-iosdist](https://github.com/networkextension/polar-iosdist) | private | iOS distribution + ASC + signing |
| [polar-ui-common](https://github.com/networkextension/polar-ui-common) | private | Shared UI library used by plugin pages |

Plugins talk back to this dock via the `/internal/v1/*` API (HMAC plugin
tokens). See `doc/arch/internal-api-v1.md` for the contract and
`doc/arch/open-platform.md` for the overall architecture.

## Local Development

For a full setup walkthrough (PostgreSQL/Redis bootstrap, env vars, AI agent, R2, troubleshooting) see [`doc/deploy-local.md`](doc/deploy-local.md). The migration script for upgrading from earlier database names lives at `scripts/migrate_db_to_ideamesh.sh`.

### Start the backend

```bash
cd /path/to/Polar-
env GOCACHE=/tmp/polar-go-cache go run ./cmd/dock
```

Default backend address: `http://localhost:8080`.

### Start the UI

The static UI ships from its own repo:
**[networkextension/polar-dock-ui](https://github.com/networkextension/polar-dock-ui)** (private).

```bash
git clone git@github.com:networkextension/polar-dock-ui.git ../polar-dock-ui
cd ../polar-dock-ui
npm ci          # first time only
npm run build   # tsc + esbuild → dist/
node server.js  # express dev preview, proxies /api → :8080
```

Default UI address: `http://localhost:3000`.

For production, point any static server (nginx, Caddy, etc.) at
`polar-dock-ui/dist/` and proxy `/api` to the dock binary on `:8080`.

### Optionally pair a coding agent

```bash
# build polar-agent for the local OS
go build -o ~/.local/bin/polar-agent ./cmd/polar-agent

# log in once
polar-agent login --server=http://localhost:8080 --token=<raw token from /api/agent/tokens>

# attach a bot to a working directory
polar-agent attach --bot=<bot_user_id> --workdir=/path/to/repo --kimi --verbose

# longer kimi-cli timeout (default 30 min)
POLAR_KIMI_TIMEOUT=1h polar-agent attach --bot=... --workdir=... --kimi --verbose
```

The bot then becomes the chat-facing front-end of your local kimi-cli session. Every message you send the bot is piped to `kimi-cli --quiet --yolo --continue --prompt=<message>`; stdout streams back to chat.

## Evaluation Bundle (Sales / Trial)

Release tarballs produced by `./release.sh` ship a self-contained evaluation kit:

- `QUICKSTART.md` (top-level eval guide focused on the LLM experience)
- `doc/eval-quickstart.md`, `doc/deploy-local.md`
- `scripts/eval_start.sh` — auto-detects PostgreSQL/Redis, creates the `ideamesh` DB on first run, starts backend + UI in one command
- `scripts/db_init.sql`, `scripts/migrate_db_to_ideamesh.sh`
- `ui/` (built static assets + `server.js` so the UI can be served without re-building)

Recipient flow: extract → install Postgres/Redis/Node → `./scripts/eval_start.sh` → open `http://localhost:3000`.

## Environment Variables

### Core

- `POSTGRES_DSN` — PostgreSQL connection string
- `REDIS_ADDR` — Redis server address
- `REDIS_PASSWORD` — Redis password (if required)
- `REDIS_DB` — Redis logical database index
- `REDIS_PREFIX` — key prefix for session records
- `MARKDOWN_DIR` — directory for Markdown source files
- `GEOLITE_DB_PATH` — GeoLite2 City database path (login geolocation, optional)
- `PUBLIC_BASE_URL` — base URL used in email links and OTA share URLs (e.g. `https://polar.example.com`)

### Passkey / WebAuthn

- `PASSKEY_ORIGIN` — Passkey validation origin
- `PASSKEY_RP_ID` — relying party ID
- `PASSKEY_RP_NAME` — relying party display name

### iOS Distribution (iosdist)

- `IOSDIST_RESOURCE_KEY` — 32-byte hex key for AES-256-GCM at-rest encryption of certificates / profiles / keys

### Apple Push (APNs)

- `APPLE_PUSH_TOPIC`, `APPLE_PUSH_TOPIC_DEV`, `APPLE_PUSH_TOPIC_PROD`
- `APPLE_PUSH_KEY_ID`, `APPLE_PUSH_KEY_ID_DEV`, `APPLE_PUSH_KEY_ID_PROD`
- `APPLE_PUSH_TEAM_ID`, `APPLE_PUSH_TEAM_ID_DEV`, `APPLE_PUSH_TEAM_ID_PROD`

### SMTP (email verification)

- `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`
- `SMTP_FROM_EMAIL`, `SMTP_FROM_NAME`

### iCloud SMTP example

```bash
export PUBLIC_BASE_URL="https://polar.example.com"
export SMTP_HOST="smtp.mail.me.com"
export SMTP_PORT="587"
export SMTP_USERNAME="yourname@icloud.com"
export SMTP_PASSWORD="xxxx-xxxx-xxxx-xxxx"
export SMTP_FROM_EMAIL="yourname@icloud.com"
export SMTP_FROM_NAME="Polar-"
```

After configuration, authenticated users can call `POST /api/email-verification/send`; the verification email link points to `GET /api/email-verification/verify?token=...`.

### polar-agent (client side)

- `POLAR_KIMI_TIMEOUT` — Go duration string (`30m`, `1h`, `90m`) overriding the default 30-minute kimi-cli timeout.

## GeoLite Setup

To enable login geolocation, provide a `GeoLite2-City.mmdb` database file.

Default lookup path:

```text
data/GeoLite2-City.mmdb
```

Or set it explicitly:

```bash
GEOLITE_DB_PATH=/your/path/GeoLite2-City.mmdb
```

If the file is missing, login still works — geolocation simply becomes unavailable.

## Reference Documentation

### Architecture and design

- [`teams.md`](teams.md) — Workspace model, T6 migration, X-Workspace-Id header
- [`iosapp.md`](iosapp.md) — iOS distribution module architecture
- [`oauth.md`](oauth.md) — OAuth design (planned future work)
- [`DEV_PLAN.md`](DEV_PLAN.md) — Roadmap and milestones for the AI agent workflow
- [`FUTURE_WORK.md`](FUTURE_WORK.md) — Open backlog

### Per-module guides

- [`doc/auth.md`](doc/auth.md) — Authentication contracts
- [`doc/auth-refresh.md`](doc/auth-refresh.md) — Cookie keep-alive + 401 interceptor
- [`doc/passkey.md`](doc/passkey.md) — WebAuthn flows
- [`doc/api.md`](doc/api.md) — API reference
- [`doc/chat.md`](doc/chat.md) — Chat thread + LLM topic model
- [`doc/llm-bot.md`](doc/llm-bot.md), [`doc/llm-bot-guide.md`](doc/llm-bot-guide.md) — Bot creation + LLM provider routing
- [`doc/posts.md`](doc/posts.md) — Posts + tasks (gigs)
- [`doc/tasks.md`](doc/tasks.md) — Task post applications + results
- [`doc/profile.md`](doc/profile.md) — Profile model
- [`doc/video-studio.md`](doc/video-studio.md) — Seedance integration
- [`doc/latch.md`](doc/latch.md) — Latch admin module
- [`doc/i18n.md`](doc/i18n.md) — UI translation keys
- [`doc/deploy-local.md`](doc/deploy-local.md), [`doc/eval-quickstart.md`](doc/eval-quickstart.md) — Local + eval setup

## Closing Note

Polar- is best understood as a living prototype.

It is a place where product thinking, engineering pragmatism, interface experimentation, and AI-assisted development meet in one repository. The codebase is intentionally compact enough to move quickly, but complete enough to surface real architectural and UX decisions.

## License

[BSD 3-Clause](LICENSE) — copy, fork, and ship freely; just keep the copyright notice and don't use the project's name to endorse derivative work without permission.
