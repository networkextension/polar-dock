# Agent: proxy-client, identity, and task dispatch

Status: design (2026-06-02). Supersedes the ad-hoc "plumb raw LLM config into the
research_task envelope" approach.

## Problem

Three coupled issues surfaced while finishing the dock→hosts split:

1. **Agent LLM spend is un-billed and un-audited.** Today dock plumbs the raw upstream
   `{base_url, api_key, model}` of an `llm_config` into the `research_task` WS envelope
   (`agent_hub.go` `agentResearchTask`; consumed in `polar-agent/research.go:51-76` →
   `llm_client.go:141-160`). The agent builds a *direct* OpenAI client and calls the provider.
   Consequences: the upstream API key is copied to every host; none of the existing proxy
   machinery (`llm_proxy_calls` audit, cost ledger, rate-limit, quota) sees agent traffic.

2. **Register is a control-plane call, not "registration".** The agent does not need a bespoke
   register; it needs (a) an identity (bot) and (b) an **API token to call dock**. The existing
   register chain already creates the bot and ensures a workspace proxy token — it just doesn't
   hand a usable token back to the agent or point the agent at the proxy.

3. **Task delivery is pure push and non-durable.** `agentHub.dispatchResearchTask` is a 5s
   fire-and-forget WS send. If the agent is offline at dispatch time the task is effectively
   lost (row stuck `queued`, no redelivery), there is no backpressure, and delivery is bound to
   dock's in-memory hub.

## Planes

| Plane | Owner | Responsibilities |
|-------|-------|------------------|
| **Control** (identity) | **polar-hosts** (front door) → dock `/internal/v1` (authority) | register, mint **per-agent proxy token**, create/bind bot, skill catalog, host UI |
| **Data** (runtime) | **dock**, direct | LLM proxy (`/api/proxy/v1/*`), task WS (`/ws/agent`), result callbacks |

**Decision: the agent's runtime traffic goes direct to dock, not through hosts.** The proxy,
billing, audit, and agentHub all live in dock; the agent authenticates with a bearer token (no
SSO cookie). Routing runtime through hosts would add a hop and a second streaming/WS proxy for no
benefit. hosts stays the *control-plane* front door only. nginx already proxies
`*.4950.store/api/*` → dock, so "direct" needs no new infra.

## 1. LLM proxy: model list + agent-as-client

### 1a. `GET /api/proxy/v1/models` (NEW)

OpenAI-compatible. Auth = the proxy bearer token (same as chat). Returns the models the *calling
token* can reach:

- every `llm_proxy_routes.alias_model` bound to the token, plus
- the token's `default_llm_config_id` model (advertised under its config name) if set.

```json
{ "object": "list",
  "data": [ { "id": "<alias_model>", "object": "model", "owned_by": "polar" }, ... ] }
```

Assembled list cached per `token_id` with a 30s TTL (mirror the `/api/nav` package-level
mutex+timestamp cache). Gating is by token, not user.

Handler: `llm_proxy_chat_handlers.go` (sibling `handleLLMProxyModels`); route in `app.go` next to
the chat route, same bearer middleware.

### 1b. Agent caches the model list

On WS connect (after `hello`) the agent does one `GET /v1/models`, caches it, and refreshes on
TTL or on a `models.invalidate` WS hint. Never blocks a task on discovery.

### 1c. Flip the envelope from raw-config to proxy-pointer

`agentResearchTask.llm` changes from the upstream secret to a proxy pointer:

```
before: { base_url: <upstream>, api_key: <UPSTREAM SECRET>, model: <real model> }
after:  { base_url: https://zen.4950.store:2443/api/proxy/v1,
          api_key: <agent's per-agent proxy token>,
          model: <alias the token is routed for> }
```

The agent's existing `{base_url, api_key, model}` client (`llm_client.go`) needs no structural
change — only the values it receives. Net agent diff ≈ 10 lines (drop the proxy-exempt dock-host
special-case in `buildProxyFunc` since the agent now calls dock for LLM on purpose).

**Result:** every agent LLM call is billed + audited via `llm_proxy_calls`; upstream keys never
leave dock.

## 2. Identity at register

Register flow is unchanged at the edge (agent → `polar-hosts /api/hosts/register` → dock
`/internal/v1/agents/register`). Two additions to the dock register chain
(`registerAgent`, `internal_api_handlers.go`):

1. **Mint a per-agent proxy token** named `agent:<agent_id>`, scoped to the agent's workspace,
   with a default route to the workspace's standard agent model. Per-agent (not the shared
   `agent-pool`) so audit answers "which agent spent what" and a single agent can be revoked
   without killing the fleet.
   - Implementation note: stored proxy tokens are hash-only, so the token must be **minted fresh
     here** (we hold the raw exactly once) and returned in the register response.
2. Return `{ proxy_token, proxy_base_url, default_model }` to polar-hosts, which relays it to the
   agent. The agent persists it alongside its `agent_token` in `~/.polar/agent.toml`.

The bot creation + workspace proxy-token ensure already exist in the chain; this adds the
per-agent token + response plumbing.

## 3. Task dispatch: durable queue + WS-hint + pull

Replace fire-and-forget push with **notify-then-pull**:

1. **Enqueue (durable).** Tasks live in the existing `project_research_runs` queue
   (`status=queued`). projects is the *producer*: when an operator dispatches, projects calls a
   dock internal endpoint `POST /internal/v1/agent-tasks/enqueue` (or dock enqueues directly for
   research). No payload is pushed to the agent at this point.
2. **Hint (low-latency).** If the target agent is online, dock sends a tiny WS frame
   `{kind:"task.wake"}` — a doorbell, not the payload. Offline agents get nothing and the row
   stays `queued`.
3. **Claim (pull).** Agent calls `POST /api/agent/tasks/claim`. Atomic:
   ```sql
   UPDATE project_research_runs SET status='running', claimed_at=now()
    WHERE id = (SELECT id FROM project_research_runs
                 WHERE status='queued' AND assigned_bot_user_id = $1
                 ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
   RETURNING ...;
   ```
   `FOR UPDATE SKIP LOCKED` makes concurrent claims safe. The claim response carries the full
   payload (now with the proxy pointer from §1c, not raw config).
4. **Report.** Unchanged — the existing `/api/projects/research/runs/<id>/{start,result}`
   callbacks finalize the row.
5. **Reconnect drain.** On WS connect the agent issues one `claim` to pick up any backlog that
   accumulated while it was offline; loops until `claim` returns empty.

Gains: durable (survives agent restart), low-latency (WS doorbell, not poll interval),
backpressure (agent claims only when free), reuses the live WS channel + HTTP callbacks.
The queue stays in dock (owns the agent WS, token, and proxy); projects is producer + status
reader.

### Compatibility

- `tool_call` and `chat_message` stay synchronous WS request-reply (correlation-id, blocking) —
  they are interactive, not queued work, and don't need the durable path.
- Only `research_task` moves from push-payload to hint+claim.

## Phasing

- **P1 — proxy model list + agent-as-client (no register change):** add `/v1/models`; flip the
  `research_task` envelope to the proxy pointer using the existing workspace `agent-pool` token.
  Immediately closes the billing/audit + key-leak hole. *(register still mints only the
  pool token; per-agent token deferred to P2.)*
- **P2 — per-agent proxy token at register:** mint `agent:<agent_id>` token, return through
  hosts, agent persists + uses it. Now audit/billing/revoke are per-agent.
- **P3 — durable queue + WS-hint + pull:** add `claim` endpoint + `task.wake` hint + reconnect
  drain; switch dispatch off fire-and-forget.

P1 is the highest-value, lowest-risk step and unblocks the rest.

## Touch list

- dock: `llm_proxy_chat_handlers.go` (+`handleLLMProxyModels`), `app.go` (route),
  `agent_hub.go` (envelope `llm` → proxy pointer; `task.wake`), `internal_api_handlers.go`
  (per-agent token mint + response), `research_handlers.go` / new `agent_tasks_handlers.go`
  (`claim`), `store.go` (`claimed_at` column on `project_research_runs`).
- polar-sdk: register response carries `proxy_token`/`proxy_base_url`/`default_model`.
- polar-hosts: relay the proxy token in the register response.
- polar-agent: fetch+cache `/v1/models`; point client at proxy pointer; `claim` loop + reconnect
  drain; persist proxy token.
- polar-projects: enqueue via dock internal endpoint (P3).
