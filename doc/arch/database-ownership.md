# Database Ownership Map

**Status:** Spec (2026-05-21). Drives Phase 1+ extractions.
**Related:** `decision.md`, `internal-api-v1.md`.

This document is the authoritative answer to **"which database owns which table?"** It is the input for every per-plugin migration script in Phase 1 and beyond.

## Topology

- **One Postgres instance** in production (the existing one on `.57`).
- **One database per service**, all inside that instance:
  - `ideamesh` — Dock core. Already exists.
  - `polar_wg` — wg-svc.
  - `polar_iosdist` — iosdist-svc.
  - `polar_video` — video-svc.
  - `polar_hosts` — hosts-svc + agent-svc (share for Phase 1; split later if needed).
  - `polar_latch` — latch-svc.
  - `polar_packtunnel` — packtunnel-svc.
  - `polar_library` — library-svc.
  - `polar_expense` — expense-svc (when extracted).
  - `polar_projects` — projects-svc (if extracted at all; see decision.md "Open questions").

Each plugin's connection string opens **only** its own database. No `SET search_path` games. No cross-database queries. No Postgres FDW. If a plugin needs core data, it goes through `internal-api-v1.md`.

If a single instance becomes a scaling bottleneck (it won't soon), individual databases can move to dedicated instances with `pg_dump` + DSN change — purely operational.

## Table ownership — full inventory

Source: `git grep "CREATE TABLE IF NOT EXISTS" internal/app/dock/store.go` plus the per-domain migration helpers.

### Stays in `ideamesh` (Dock core)

| Table | Domain | Notes |
|---|---|---|
| `users` | auth | Source of truth for every user record. |
| `webauthn_credentials` | auth | Passkey registration. |
| `user_devices` | auth | Push tokens, last-seen-at. |
| `login_records` | auth | Audit. |
| `sessions` | auth | Access/refresh sessions. Redis mirrors hot rows. |
| `teams` | teams | Workspace identity. |
| `team_members` | teams | Membership + role. |
| `team_invitations` | teams | Pending invites. |
| `llm_configs` | llm | Per-user + shared LLM configs. |
| `llm_config_team_quotas` | llm | B5 billing budget. |
| `llm_config_applications` | llm | Marketplace apply log. |
| `llm_config_team_recharges` | llm | Prepaid wallet topups. |
| `llm_proxy_tokens` | llm-proxy | Customer-facing OpenAI-compat tokens. |
| `llm_proxy_calls` | llm-proxy | Cost ledger. Plugin LLM calls log here via `/internal/v1/llm/chat-completion`. |
| `bot_users` | chat | Bot identity + model_override. |
| `chat_threads` | chat | 1:1 DM. |
| `chat_messages` | chat | DM + room messages. `room_id` nullable. |
| `chat_thread_summaries` | chat | Per-thread summary. |
| `chat_member_state` | chat | Read receipts, presence. |
| `chat_rooms` | chat | Qunliao rooms (P0a). |
| `chat_room_members` | chat | Room membership. |
| `chat_room_agent_loops` | chat | P3 idle-loop config. |
| `markdown_*` | markdown | Owned docs, likes, bookmarks. |
| `posts`, `post_*` | social | Public feed. |
| `replies`, `reply_*` | social | Public threads. |
| `likes`, `bookmarks`, `tags`, `task_posts`, `task_tags` | social | Engagement + cross-links. |
| `attachments`, `chat_message_attachments` | core | Shared upload store. Plugins use their own. |
| `push_deliveries`, `push_dedupe_keys` | core | Push tracking. |
| `plugin_modules` | core | Plugin registry. **Dock-owned.** |

### Moves to `polar_wg`

| Table | Owner after extraction |
|---|---|
| `wg_hubs` | wg-svc |
| `wg_tokens` | wg-svc |
| `wg_devices` | wg-svc |
| `wg_sites` | wg-svc |
| `wg_bundles` | wg-svc |
| `wg_*` (any tables prefixed `wg_`) | wg-svc |

### Moves to `polar_iosdist`

| Table | Owner |
|---|---|
| `iosdist_apps` | iosdist-svc |
| `iosdist_versions` | iosdist-svc |
| `iosdist_certificates` | iosdist-svc |
| `iosdist_profiles` | iosdist-svc |
| `iosdist_sign_tasks` | iosdist-svc |
| `iosdist_asc_configs` | iosdist-svc |
| `iosdist_asc_*` (any `iosdist_*` table) | iosdist-svc |

### Moves to `polar_video`

| Table | Owner |
|---|---|
| `video_projects` | video-svc |
| `video_shots` | video-svc |
| `video_assets` | video-svc |
| `video_provider_jobs` | video-svc |
| `video_*` (any `video_*` table) | video-svc |

### Moves to `polar_hosts`

| Table | Owner |
|---|---|
| `hosts` | hosts-svc |
| `host_skills` | hosts-svc |
| `host_skill_credentials` | hosts-svc |
| `host_shell_sessions` | hosts-svc |
| `host_shell_backscroll` | hosts-svc |
| `host_vnc_sessions` | hosts-svc |
| `console_layouts` | hosts-svc |
| `agent_tokens` | hosts-svc (shared with agent-svc) |
| `agent_*` (any `agent_*` table that backs polar-agent presence/dispatch) | hosts-svc |

### Moves to `polar_latch`

| Table | Owner |
|---|---|
| `latch_profiles` | latch-svc |
| `latch_proxies` | latch-svc |
| `latch_rules` | latch-svc |
| `latch_service_nodes` | latch-svc |
| `latch_*` | latch-svc |

### Moves to `polar_packtunnel`

| Table | Owner |
|---|---|
| `packtunnel_profiles` | packtunnel-svc |
| `packtunnel_rule_files` | packtunnel-svc |
| `packtunnel_*` | packtunnel-svc |

### Moves to `polar_library`

| Table | Owner |
|---|---|
| `rev_devices` | library-svc |
| `rev_firmwares` | library-svc |
| `rev_functions` | library-svc |
| `rev_*` | library-svc |

### Moves to `polar_expense`

| Table | Owner |
|---|---|
| `expense_categories` | expense-svc |
| `expenses` | expense-svc |
| `expense_*` | expense-svc |

### Moves to `polar_projects` (only if projects is extracted; see decision.md)

| Table | Owner |
|---|---|
| `projects` | projects-svc |
| `project_tasks` | projects-svc |
| `project_research_runs` | projects-svc |

## Cross-database rules

- **No SELECT, INSERT, UPDATE, DELETE across databases.** Every cross-domain query is replaced with one or more calls to `/internal/v1/*`.
- **No foreign keys across databases.** Plugin tables that referenced `users(id)` (e.g. `wg_tokens.owner_user_id`) lose the FK during extraction. The TEXT field remains; integrity is enforced by application code (insert succeeds whether or not the user still exists in Dock).
- **No Postgres FDW.** Tempting; rejected. It creates a hidden cross-database dependency that breaks plugin process independence.
- **Migrations are per-plugin.** Each plugin's startup runs its own idempotent migrations against its own database. A `plugin_migration_log` table inside each plugin DB records what has been applied.

## Migration playbook (template — copies into `wg-svc-extraction.md`)

For each plugin extraction:

### Step 1: pre-flight on Dock

```sql
-- Backup the soon-to-move tables from ideamesh
\c ideamesh
\copy (SELECT * FROM wg_hubs)    TO '/tmp/wg_hubs.csv'    WITH CSV HEADER;
\copy (SELECT * FROM wg_tokens)  TO '/tmp/wg_tokens.csv'  WITH CSV HEADER;
-- … etc per table in the plugin
```

### Step 2: create plugin database + schema

```bash
psql -U ideamesh -d postgres <<'SQL'
  CREATE DATABASE polar_wg OWNER ideamesh;
SQL

# Apply the plugin's own migrations — these are owned by the plugin, not by Dock.
# Each plugin ships a binary that runs its own migrations on startup; for the
# initial extraction, run it once explicitly:
POLAR_DB_DSN="postgres://ideamesh:test123456@localhost/polar_wg?sslmode=disable" \
  ./wg-svc --migrate-only
```

### Step 3: load data

```bash
psql -U ideamesh -d polar_wg <<'SQL'
  \copy wg_hubs   FROM '/tmp/wg_hubs.csv'   WITH CSV HEADER;
  \copy wg_tokens FROM '/tmp/wg_tokens.csv' WITH CSV HEADER;
SQL
```

### Step 4: cutover

1. Deploy wg-svc pointing at `polar_wg`, `POLAR_WG_REMOTE=false` on Dock. wg-svc heartbeats but doesn't take traffic.
2. Verify counts match between `ideamesh.wg_*` and `polar_wg.wg_*`.
3. Flip Nginx + `POLAR_WG_REMOTE=true` on Dock. Traffic now goes to wg-svc.
4. **Wait one release cycle** with no rollback.
5. Drop the moved tables from `ideamesh` (after final backup).

### Step 5: rollback (if needed before step 4 closes)

- Flip `POLAR_WG_REMOTE=false` on Dock.
- Revert Nginx config.
- Dock resumes serving wg from `ideamesh` (its tables are still there).
- Any rows wg-svc wrote during the cutover window need to be **manually merged back** to `ideamesh` — script template lives in `wg-svc-extraction.md`.

## Verification

After each extraction:

- `\dt` on the plugin DB shows the expected tables and **only** those tables.
- `\dt` on `ideamesh` no longer shows them (after the cleanup at Step 5).
- The plugin's connection string (set via env) does not include a connection string for `ideamesh`.
- A `psql` session opened with the plugin's user can `SELECT` from its own tables and gets `ERROR: permission denied` if it tries to `SELECT` from `ideamesh.users`. (Permissions phase comes in late Phase 1.)

## Notes on shared concerns

- **Push notifications** stay in Dock — plugins call `/internal/v1/notify`. The push_deliveries / push_dedupe_keys tables are not duplicated.
- **Attachments** stay in Dock for chat / markdown / posts. Plugins handle their own storage (e.g. video-svc has its own R2 bucket; iosdist-svc has its own IPA storage). If a plugin wants to attach to a chat message, it uploads to Dock via a future `/internal/v1/storage/sign-url` (added when needed).
- **Audit logs** are local to each plugin. Dock's `login_records` only covers auth events.
- **Schema migrations on Dock's tables** continue to live in `internal/app/dock/store.go` and Dock's startup applies them. Plugin migrations are owned by each plugin's repo.
