# Chat + LLM extraction (control/data-plane split, Phase 2)

Status: DESIGN (2026-06-22). Expands Phase 2 of `control-plane-data-plane-split.md`.
Decisions locked: **two services, `polar-llm` first**; **`llm_configs` + billing tables move to `polar-llm`**.

## Why

dock fuses three planes in one process. Phase 1 (verify-cache + `verify-agent`) made token
verification shareable; this phase carries that through to the heavy subsystems:

- **dock = identity control plane** — users, teams, sessions, push gateway, `agent_hub`. Tiny, always-fast.
- **polar-llm = LLM data plane** — the OpenAI-compat proxy + upstream dispatch + `llm_configs` +
  billing/quota + cost ledger. Long streaming, bursty, scales horizontally. **One gateway for every
  caller** (chat, expense, video, agents, external) → unified cost/quota/audit; streaming load off
  dock's auth hot path.
- **polar-chat = messaging plane** — 1:1 threads, 群聊 rooms, bots, @-mention fanout, idle loop, `/ws/chat`.
  Stateful; a client of polar-llm.

Audit confirmed **two LLM call paths** today, both inside dock: the proxy (`/api/proxy/v1/*`,
`llm_proxy_chat_handlers.go`) and the in-app `aiAgent` (`requestChatCompletion`/streaming, driven by
chat threads + `chat_rooms_agent.go` @-fanout + `chat_rooms_idle_loop.go`). Both converge on
`requestChatCompletion` + the `llm_proxy_calls` ledger + `checkChatBillingGate`
(`llm_config_team_quotas`/`recharges`), and both pivot on `llm_configs` (cross-workspace-shared
upstream key+model). That convergence is exactly why one `polar-llm` gateway is the right cut.

## Target

```
        ┌──────────── dock (identity) ────────────┐
        │ users·teams·sessions·push·agent_hub      │
        └───┬───────────────────────────┬──────────┘
   AuthVerify(cached)            /internal/v1/notify
        ┌───▼──────────────┐    ┌────────▼───────────────────────┐
        │  polar-chat       │──▶│  polar-llm  (LLM gateway)       │
        │ threads·rooms·bots│ HMAC│ /api/proxy/v1/* + dispatch     │
        │ fanout·idle·/ws/chat│   │ llm_configs·billing·ledger·quota│
        └───────────────────┘    └────────────────────────────────┘
              every LLM caller (chat·expense·video·agents·external) → polar-llm
```

---

## Service 1 — `polar-llm` (extract FIRST)

**Owns (moves out of dock):**
- Tables: `llm_configs`, `llm_proxy_tokens`, `llm_proxy_calls`, `llm_proxy_routes`,
  `llm_config_team_quotas`, `llm_config_team_recharges`, `llm_config_applications`. New DB `polar_llm`.
- Code: `llm_proxy_*` (chat/models/handlers/store/rate_limit/quota/pricing/probe), `llm_billing_*`,
  the upstream dispatch core (`requestChatCompletion` / `requestStreamingChatCompletion` from `ai_agent.go`),
  the admin config/billing CRUD.
- Public surface: `/api/proxy/v1/chat/completions` (+ stream), `/api/proxy/v1/models`; admin
  `/api/llm-configs/*`, `/api/llm-billing/*`. nginx routes these → polar-llm.

**Gateway-side billing.** The per-team quota + recharge gate (today `checkChatBillingGate`) moves to
polar-llm and runs **inside** the completion endpoint — so every caller is metered uniformly, and the
ledger (`llm_proxy_calls`) has one writer. Callers stop calling `checkChatBillingGate` directly.

**Auth at the gateway.** polar-llm verifies callers via dock (Phase-1 cache):
- `polar_proxy_` tokens → its own `llm_proxy_tokens` (local DB, redis-cached).
- session / `polar_` / `polar_agent_` callers → `sdk.AuthVerify*` (dock, cached) — closes the
  "only dock can verify agent tokens" gap that motivated Phase 1.

**The in-app path becomes a network call.** Where dock today calls `aiAgent.requestChatCompletion()`
in-process, callers (incl. polar-chat) now POST polar-llm. Add an **internal completion endpoint**
`POST /internal/v1/llm/complete` (HMAC) that takes `{workspace_id, llm_config_id|system, messages,
stream, requester}` → resolves config + gates billing + dispatches + writes the ledger + returns the
reply (SSE for stream). System-agent calls pass `system` (cost absorbed, `is_system` config).

**Coupling that stays a dock RPC:** workspace/team identity + membership (for billing scope) via
`sdk.AuthVerify`/`Team`. `llm_configs` cross-workspace `shared=true` rows move wholesale — sharing is
resolved inside polar-llm now.

**Migration (flag-gated, no big-bang):**
1. Stand up polar-llm + `polar_llm` DB; **copy** the seven tables; polar-llm serves `/api/proxy/v1/*`
   behind a flag while dock still does (double-serve), nginx flips `/api/proxy/v1` → polar-llm.
2. Add `/internal/v1/llm/complete`; switch dock's in-app `aiAgent` dispatch to call it behind
   `POLAR_LLM_REMOTE` (default off). Verify ledger/quota parity.
3. Flip the flag on zen; delete dock-side llm code once baked. dock-ui llm-config/billing pages
   re-point to polar-llm origin (same platform-nav pattern).

---

## Service 2 — `polar-chat` (extract SECOND, client of polar-llm)

**Owns (moves out of dock):**
- Tables: `chat_threads`, `chat_member_state`, `chat_reads`, `chat_thread_summaries`,
  `chat_rooms`, `chat_room_members`, `chat_room_agent_loops`, `chat_messages` (thread_id + room_id),
  `chat_message_attachments`, **`bot_users`** (see below). New DB `polar_chat`.
- Code: `chat_rooms_*` (handlers/store/agent/idle_loop), `chat_thread_*`, `chat_events.go` (Redis
  pub/sub), `ws_hub.go` + `ws_handlers.go` (the `/ws/chat` hub), `llm_bot_handlers.go`.
- Surface: `/api/rooms/*`, `/api/chat/*` (threads), `/api/bots/*`, `/ws/chat`. nginx → polar-chat.

**WS topology.** polar-chat runs **its own** `/ws/chat` hub; the browser connects cross-subdomain
(`chat.4950.store`) on the shared `.4950.store` session cookie, and the WS handshake verifies via
`sdk.AuthVerify`. The Redis `polar:chat:events` channel stays shared (publisher = polar-chat).
**`agent_hub` (`/ws/agent`) stays in dock** — agents attach to dock; bot replies are produced inside
polar-chat (it owns the fanout/idle loop), not via agent_hub.

**Bots call polar-llm.** `fanoutOneBotReply` + `idleLoopDecideOne` replace the in-process
`requestChatCompletion` with a `POST /internal/v1/llm/complete` to polar-llm, then persist + broadcast
(`insertAndBroadcastBotReplyWithMetrics`). The ledger lives in polar-llm; polar-chat just keeps the
per-message metric snapshot it already stores on `chat_messages`.

**Push via the dock gateway.** Implement the real `POST /internal/v1/notify` in dock (today a **stub**
— the one true gap). polar-chat, on a new DM/mention, calls it with `{recipient_user_ids, title, body,
ref}`; dock owns `push_tokens` + APNs + dedup + the `push_deliveries` worker. (Rooms have no push
today — add it here as a natural follow-on.)

**`bot_users` placement (recommendation).** A bot *is* a user (`bot_users.bot_user_id` → `users.id`),
but its persona (`system_prompt`, `model_override`, `bot_kind`, `tags`) and its `llm_config_id` binding
are chat/LLM concerns. Cut: **the bot's user identity row stays in dock** (username, icon, role=bot —
so @mentions and member lists resolve via `sdk.User`); **`bot_users` (persona + bindings) moves to
polar-chat**, with `bot_user_id` a soft-ref to the dock user and `llm_config_id` a ref into polar-llm.
Auto-register-by-agent-token stays a dock concern (dock mints the user + agent token, then calls
polar-chat to create the bot persona).

**Dock seams polar-chat needs (add SDK wrappers):** `AuthVerify*` (exists), `User(id)`, `Team(id)`,
`TeamMember(team,user)` (endpoints exist, no SDK wrapper yet), `NotifyUser(...)` (new, once
`/internal/v1/notify` is real).

---

## Sequenced plan

- **P2a — polar-llm extract** (flag-gated double-serve → cutover → cleanup). Delivers the load-isolation
  win + unified LLM gateway. Biggest, do first.
- **P2b — `/internal/v1/notify`**: implement real push dispatch in dock (unblocks polar-chat + a long-
  standing stub).
- **P2c — polar-chat extract**: messages/rooms/bots/WS move; bots → polar-llm `/internal/v1/llm/complete`;
  notifications → dock `/internal/v1/notify`. Cutover behind `POLAR_CHAT_REMOTE`.
- **P2d — SDK wrappers** (`User`/`Team`/`TeamMember`/`NotifyUser`) + dock-ui re-point of chat + llm-config
  + billing pages to the new subdomains.

## Open items / risks

- **Streaming over `/internal/v1/llm/complete`** must support SSE pass-through; the in-app chat UX
  depends on token streaming — verify the HMAC-signed internal call streams cleanly.
- **`llm_configs` admin UI move** (llm-config/billing pages) is real surface area — sequence after the
  proxy cutover is stable.
- **Two new DBs** (`polar_llm`, `polar_chat`) + dual-write windows during copy; back up before each
  cutover (rapid-iter discipline).
- **Cross-service cost attribution**: ledger is single-writer in polar-llm, but the *requester* tag
  (chat vs expense vs agent) must be threaded through `/internal/v1/llm/complete` for per-feature analytics.
