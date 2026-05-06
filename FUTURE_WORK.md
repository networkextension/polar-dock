# Future Work

Based on the current source at the time of writing. Organized by theme — security first, then product, then polish.

---

## Security & Correctness

### Rate Limiting on Auth Endpoints
`/api/login`, `/api/register`, and `/api/passkey/login/begin` have no rate limiting. A trivial script can enumerate passwords or flood the WebAuthn ceremony. Add a per-IP sliding-window middleware (e.g. `golang.org/x/time/rate` or a Redis token-bucket) before these handlers.

### Pagination Limit Cap
Every list endpoint accepts `limit` from the caller and passes it directly to the query. There is no upper-bound check. A single request with `limit=1000000` can read the full table. Add a hard cap (`if limit > 200 { limit = 200 }`) in every handler that reads this parameter.

### Chat Access Control
Chat message endpoints fetch messages from a thread ID supplied by the client. The handler should verify the requesting user is a participant of that thread before returning data. The schema has `chat_threads.participant1_id / participant2_id` — this check is cheap and must be done.

### Email Verification Token Expiry
`email_verification_tokens` are stored without a visible TTL or expiry enforcement. A leaked token is valid indefinitely. Add a `created_at` column and reject tokens older than 24 hours.

### Post Visibility Enforcement
Every `Post` row has `is_public`, but the list endpoints appear to return all rows regardless. Feed and profile endpoints should filter `WHERE is_public = TRUE` for unauthenticated callers and honor user-block relationships for authenticated ones.

### WebSocket Origin Check
`CheckOrigin` is set to `func(r *http.Request) bool { return true }`. This accepts WebSocket upgrades from any origin. In production tighten this to the known deployment domain.

### CORS Policy
Gin's default CORS allows all origins. Restrict to the expected frontend origin(s) in the CORS middleware configuration.

### Admin Password Reset Without Confirmation
An admin can reset any user's password immediately. Add an audit log row and, optionally, a confirmation step so the action is traceable.

---

## Reliability & Performance

### Database Index Audit
Several heavily-queried join patterns (messages by thread, entries by user, login records by user) rely on foreign key columns. Verify covering indexes exist for `chat_messages(thread_id, created_at)`, `markdown_entries(user_id, updated_at)`, and `login_records(user_id, logged_in_at)`. Offset-based pagination is already in use; consider keyset pagination for large tables.

### Geolocation Call on Every Login
`GeoLite2` lookup runs synchronously during the login handler. Move it to a goroutine that writes the record after the session is already issued so login latency is unaffected even on slow lookups.

### AI Agent Task Queue Depth
The LLM task queue is a buffered channel of 64 items. Under load new messages silently drop if the queue is full. Either increase the buffer, shed load with an explicit error response to the client, or replace with a persistent job table.

### Site Settings & LLM Config Caching
`/api/site-settings` and `/api/llm-configs` are fetched on every page load from every client. These change rarely. Add a short in-memory TTL cache (or Redis key with a 30-second TTL) to reduce DB round-trips.

### File Garbage Collection
Attachments uploaded to Cloudflare R2 (or local `uploads/`) are never deleted if the parent message is revoked or a chat thread is removed. Add a periodic cleanup job that cross-references stored file paths with live `chat_messages.attachment` records.

---

## Missing UI Pages & Feature Parity

### Login History Page
The backend exposes `GET /api/admin/users/:id/login-history` and `/api/login-history` for the current user. The admin panel shows this inline per-selected-user, but there is no standalone page where a regular user can review their own full login history with filtering by date or device.

### Task System UI
The backend has complete routes for task creation, applications, candidate selection, and result submission. No frontend pages expose this workflow. Either build a `tasks.html` page or integrate task management into the Posts feed for posts that have `task` metadata.

### Profile Recommendations
`POST /api/users/:id/recommendations` stores a recommendation, but there is no algorithm, ranking, or display. Decide whether this is a social-proof badge (show count + recent recommenders on the profile page) or remove the endpoint to reduce dead surface area.

### Push Notification Delivery
Device tokens are stored, APNs certificates can be uploaded, and the data model is complete. The actual `apns2` send call is not wired to any event. Connect the token delivery to at least one trigger: new chat message when the recipient is offline.

---

## Developer Experience & Maintainability

### Inconsistent API Error Shapes
Some handlers return `{"error": "..."}`, others return `{"message": "..."}` or custom field names. Define a single envelope type in `response.go` and use it everywhere. This makes frontend error handling simpler and predictable.

### Centralize Constants
Several values are duplicated across files:
- Default Markdown directory (`"data/markdown"`) — read from config, not hardcoded
- Video attachment limit (`100 << 20`) — one constant used in all upload handlers
- LLM task queue depth (`64`) — one constant in the agent package

### Access Token TTL Policy
Current `AccessTokenTTL = 30 * time.Minute` was picked when the web UI was
the only client. 30 min is tight for native clients that wake up rarely
(e.g. Latch iOS fetches `/api/latch/profiles` then goes idle): every cold
launch after ~half an hour hits a 401 → refresh round-trip before the
first useful request. Likely target is ~24h access with refresh still at
30d, or a per-client-type TTL driven by the `X-Device-Type` header
(browser stays short, native gets long). Revisit once more than profile
reads are in play on mobile, and make sure it's covered by an explicit
threat-model note rather than a single constant.

### API Versioning
All routes live at `/api/...` with no version prefix. Adding `/api/v1/` now, before any external consumers exist, avoids a forced migration later.

### OpenAPI Specification
There is no machine-readable API description. Add a `docs/openapi.yaml` (or generate one from annotations) so that clients, tests, and future contributors have a reliable contract. Swagger UI can be served at `/api/docs` during development.

### Backend i18n Completeness
Several Go handler error strings are Chinese-only or English-only with no translation lookup. Run a grep for hardcoded string literals in handlers and route them through the existing `i18n.go` catalog.

### Frontend i18n Key Coverage
The `i18n.ts` catalog has 500+ keys but a few pages (notably `latch.html` labels and some dynamic status strings in `dashboard.ts`) still contain hardcoded English or Chinese strings. A build-time check that all `t("...")` calls have corresponding catalog entries would catch regressions.

---

## Product Enhancements

### Markdown Search
`/api/markdown` returns entries ordered by `updated_at` with no search parameter. Add a `q` query parameter that does a `ILIKE` match on `title` and proxies a simple content grep on disk. Full-text search with `pg_trgm` is a natural upgrade path once entry volume grows.

### Suspicious Login Alerts
Login records already capture IP and geolocation. Compare each new login's country against the last N successful logins for the same user. If the country changes, send an email alert (the SMTP infrastructure is already in place) and surface a warning in the user's login history view.

### Passkey Device Naming
Passkey credentials are stored with a raw `credential_id`. Allow users to set a display name (e.g. "MacBook Touch ID", "iPhone Face ID") so the credential list in Settings is readable and users know which key to revoke.

### Markdown Versioning
Entries are stored as single files. Add an opt-in version history by keeping prior content snapshots under a `versions/` subdirectory. Expose a diff view in the editor so authors can see what changed between saves.

### Chat Message Search
Full-text search within a chat thread is a common expectation. A `tsvector` column on `chat_messages.content` with a GIN index would support this without a separate search service.

### Bot Usage Statistics
Each time the AI agent responds, record the token count and latency in a lightweight table. Expose aggregate usage per LLM config on the Bots page so operators can track spend and identify slow configs.

---

## Infrastructure

### Structured Logging
Log output is unstructured. Replace `log.Println` calls with a structured logger (`slog` from stdlib or `zerolog`) emitting JSON lines. This makes log aggregation (Datadog, Loki, etc.) and filtering in production practical.

### Health & Readiness Endpoints
Add `GET /healthz` (liveness) and `GET /readyz` (readiness — checks DB ping + Redis ping) for container orchestration probes.

### Configuration Validation on Startup
The app starts silently with missing environment variables and fails at runtime. Add a startup check that reads all required env vars and exits with a clear error listing every missing variable before binding the HTTP server.

### Graceful Shutdown
Ensure the HTTP server and WebSocket hub drain cleanly on `SIGTERM` (use `http.Server.Shutdown` with a deadline and close the hub's broadcast channel). This matters for zero-downtime deployments.

### Attachment Storage Abstraction Test
The `StorageBackend` interface wraps local FS and R2, but there are no integration tests covering the R2 path. Add a test against a local MinIO instance to verify upload, URL generation, and delete work correctly before deploying to a new environment.

### Microsoft Teams Hybrid Integration
Researched and rejected for full replacement (free-tier users + app-only-posting restriction + Graph notification SLA each independently kill it). Recommended path: **lightweight hybrid** — Entra ID SSO, outbound Adaptive Card notifications via channel Workflow webhooks, optional sideloadable bot manifest, deep-link "Open in Polar" buttons. Estimate: 2–3 engineer-weeks. Trigger condition: an identifiable enterprise customer with an active M365 tenant asks for it. Full design: [`doc/teams-integration-research.md`](doc/teams-integration-research.md).

### polar-agent Container Image (`polar/agent:standard`)
Package polar-agent + kimi-cli + claude + codex + git into a single OCI image. Operator runs `docker run -e POLAR_DOCK_URL -e POLAR_AGENT_TOKEN -e POLAR_BOT_IDS polar/agent:standard` and the multi-bot polar-agent farm is up. Three image variants planned (`slim` / `standard` / `full`); auth via either bind-mounted host `~/.kimi` / `~/.claude` / `~/.codex` directories or `*_API_KEY` env vars. Volume mount `/work` provides per-bot/per-project subdirs (reusing existing `workdir_subpath` plumbing). Limits: macOS/iOS native builds still need a real Mac (Xcode can't run in Linux containers); first-time browser OAuth logins for the coder tools need either host bind-mount or API-key env. Estimate: 1.5–2.5 engineer-weeks for `slim`+`standard`+CI+docs. Trigger: first time someone wants to run polar-agent on a Linux server or a 4th machine. Full design: [`doc/agent-container-design.md`](doc/agent-container-design.md).

### macOS Runtime Variants (Apple Silicon)
For iOS/macOS native builds (Xcode-required) where Linux containers can't help. Two paths recorded: (1) **bare-metal Mac mini** running polar-agent under `launchd` — simplest, lowest friction, single failure domain. (2) **tart-based macOS VMs** on Apple Silicon hosts — snapshots, multiple concurrent VMs (Apple licensing caps at 2 per host), pre-baked Xcode + tools. Image contents and first-boot script parallel the Linux `standard` design. Recommended ship order: Linux container first (1.5–2.5 wk), then bare-metal Mac mini plist + setup doc (0.5 wk), then tart VM image when parallel iOS builds become a bottleneck (1.5–2 wk). Detail: [`doc/agent-container-design.md`](doc/agent-container-design.md) §"macOS runtime variants".

---

## Project Pipeline (end-to-end)

### Project End-to-End Pipeline (idea → review → split → agent → code → push → CI → publish)
A 7-stage pipeline that takes a project from a written idea to a deployed build artifact without manual hand-offs. Today stages 2 (AI 拆解 features + sprint iteration) and 4 (task pickup → polar-agent → coder tool) work end-to-end. The remaining stages have varied gaps:

- **Stage 1 — Review** (idea + LLM prompt sanity check): partial. Add `GET /api/projects/:id/decompose-preview` to surface the exact prompt before the run + edit textarea. Optional reviewer role + draft table later. **0.5 wk** for the preview endpoint, **1.5 wk** with full reviewer flow.
- **Stage 3 — Agent auto-start** (per-project pool, wake-on-dispatch): partial. Builds on the existing auto-provision FUTURE_WORK entry; adds per-project preferred host + workspace pre-warm. **1–2 wk** on top of base auto-provision.
- **Stage 5 — Push code**: not started. Coder output sits in workdir. Three options discussed (A polar-agent does git / B coder tool does git / C hybrid with polar-agent safety net). Recommendation: **option C** — coder commits, polar-agent post-task safety net for any unstaged changes, project-scoped deploy keys. **1.5 wk**.
- **Stage 6 — CI/CD**: not started. Per-project `ci_provider` + `ci_repo_url`; status poll/webhook attached to each task. GitHub Actions only for v1. **1 wk** for v1, **2 wk** for multi-provider.
- **Stage 7 — Publish build result**: not started. Per-project artifact convention (iOS .ipa via existing iosdist OTA, web `dist/` to `/projects/<id>/preview/`, CLI tarball, library publish). iOS path leans on iosdist module. **1 wk** for iOS, **2 wk** with web + tarball.

Cross-cutting: project-scoped credential lifecycle (deploy keys, signing identities, CI tokens), per-task structured log (tokens × price, CI minutes × price), per-stage retry semantics with idempotency keys.

Suggested ship order: Stage 5 first (~1.5 wk), then 1 → 6 → 7 → 3, ~5 engineer-weeks total to "end-to-end usable for one iOS project." Each stage gated by its own trigger condition; premature stages become dead code. Out of scope: multi-tenant project isolation, auto-merge / PR automation, cost guardrails.

Full design: [`doc/project-pipeline-design.md`](doc/project-pipeline-design.md).

### polar-agent Auto-Provision + Cross-Machine Deploy
Today the operator has to: download the right release tarball, copy `agents/polar-agent_<os>_<arch>` into PATH, generate an agent token in the bots page, run `polar-agent login`, edit `~/.polar/start_agents.env` with the right bot IDs, install kimi-cli/claude/codex separately, log each of those in. Six manual steps × every machine the operator wants to attach from. Targets to automate:

1. **Per-user install script** — a one-liner like `curl https://polar.example.com/install/agent | sh` that detects `uname -sm`, pulls the matching `polar-agent_<os>_<arch>` from the latest release, writes `~/.polar/agent.toml` with a one-time-use enrollment token (claimed via `POST /api/agent/enroll` server-side, returns the long-lived agent token), drops a sane default `tools.json`, and prints next steps.
2. **Bot ID propagation** — server-side keep a list of "this user's agent-eligible bots." Install script (or an in-app QR code) emits `start_agents.env` pre-filled. New bot created → push update to all attached agents over the existing WS so they refresh their bot bindings without restart.
3. **Config sync** — when the user edits `tools.json` on one machine (or in a future hosted UI), pubsub the update to every attached `polar-agent` for that user. Today they'd have to scp the file or re-run the install.
4. **Multi-machine attach manifest** — single command `polar-agent attach --all` that reads remembered bot/workdir mappings from server and starts the right tmux farm. Replaces today's per-machine `start_agents.env` editing.
5. **Optional: SSH-based fan-out** — `polar-agent deploy <ssh-target>` ships the binary + config to a remote host and starts the attach loop. Useful when the operator wants `--workdir=/path/on/that/machine` rather than copying a repo locally.

Cross-cuts with bigger stories: per-tool API key rotation (claude/codex tokens move with the user, not the machine), enrollment tokens with TTL + scope limits, and security review (the install script downloads + executes a binary — needs HTTPS + sha256 verification + maybe minisign).

Estimate: 1 week for #1 alone (install script + enrollment endpoint), 2-3 weeks for #1+#2+#3 once we know the pubsub story. Trigger: first time we hit "I just want to try Polar from my work laptop without setting up everything again" — that's when the story stops being theoretical.
