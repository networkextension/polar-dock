# LLM chat API proxy — Design Proposal

**Status**: draft 2026-05-16.

**Goal**: expose Polar as an OpenAI-compatible API endpoint that
external tools (Cursor / Claude Desktop / `curl` / Python notebooks /
internal scripts) can hit instead of the upstream provider. Polar
routes the call to one of the workspace's existing `llm_configs`,
records the call, and returns the response unchanged.

**Related**:
- `internal/app/dock/ai_agent.go` — existing internal LLM dispatch
  funnel (`requestChatCompletion` + `streamOpenAICompatible`)
- `llm_configs` table — per-user/workspace LLM configs (API key,
  base URL, model, system prompt, provider_kind, proxy_url)
- Phase 2 metrics: `polar_llm_request_total` / `_duration_seconds` /
  `_tokens_total` already wrap every internal LLM call; same wrap
  applies to proxied calls

## 1. Why build it

Today operators use the embedded chat / research / decompose UIs in
Polar to talk to LLMs. They also pay for OpenAI / Anthropic / Kimi
elsewhere — Cursor, Raycast, custom scripts. Three pains the proxy
fixes:

1. **Key sprawl** — every teammate needs the upstream API key in
   every tool. One leak across N teammates × M tools = N×M ways to
   leak. With a proxy, only the operator's dock holds the upstream
   key; teammates carry a Polar-issued proxy token that can be
   rotated / revoked in seconds.
2. **No usage visibility** — operator pays the OpenAI bill, can't
   tell who used what. Proxy logs every call with token, model,
   token counts, latency, requesting feature.
3. **No per-feature routing** — Cursor calls `gpt-4o` directly;
   research notebook calls `gpt-4o` directly; both bill against
   the same upstream. Proxy lets the operator alias virtual model
   names (`polar:cheap`, `polar:strong`, `polar:research`) and
   route each to a different upstream — change the routing in the
   UI, no client-side change.

Building this in Polar is the natural extension of the existing
LLM layer — every primitive (provider clients, auth, workspace
ACL, metrics, chat audit) is already there.

## 2. Where it fits

```
external client                Polar dock                  upstream
(Cursor / Claude         ┌──────────────────────┐
 Desktop / scripts)      │  /v1/chat/completions │
─────────POST───────────►│  (proxy handler)      │
  Authorization:         │           │           │
   Bearer polar_proxy_X  │           ▼           │
                         │  resolveProxyToken    │ ── DB ──┐
                         │           │           │         │
                         │           ▼           │         │
                         │  pickLLMConfig        │ ── DB ──┘
                         │  (route by model)     │
                         │           │           │
                         │           ▼           │
                         │  requestChatCompletion├──HTTPS──► OpenAI / Anthropic / ...
                         │  (existing funnel)    │           (uses llm_config.api_key,
                         │           │           │            base_url, proxy_url)
                         │           ▼           │
                         │  logProxyCall         │ ── DB
                         │  (audit + cost)       │
                         └──────────────────────┘
```

Reuses entirely:
- `aiAgent.requestChatCompletion` (non-stream) / `streamOpenAICompatible`
  (stream) — these already handle provider quirks (OpenAI vs Kimi vs
  Gemini / Anthropic), tool-call schema, usage parsing, metrics
- All existing per-provider HTTP clients (`httpClientFor` with proxy
  support, retry, timeouts)
- Phase-2 metrics — every proxied call gets the same
  `polar_llm_*` metric labels with `endpoint="proxy"`

Adds:
- New endpoint group `/api/proxy/v1/...`
- 2 tables: `llm_proxy_tokens` + `llm_proxy_calls`
- 1 optional table for routing: `llm_proxy_routes`
- UI for token management + call-log dashboard

## 3. API surface — OpenAI-compatible

The whole point is that existing tools just work. We implement the
subset of the OpenAI REST API that 95% of clients use:

| Endpoint | Status |
|---|---|
| `POST /v1/chat/completions` | P0 (non-stream) + P1 (stream SSE) |
| `GET /v1/models` | P3 |
| `POST /v1/embeddings` | P4 |
| `POST /v1/completions` (legacy) | not in plan |
| `POST /v1/images/...` | not in plan |
| Anthropic `/v1/messages` shape | not in plan; clients use OpenAI compat |

**Wire-shape contract**: byte-for-byte OpenAI-compatible JSON, both
request + response. SSE streaming uses OpenAI's `data: ...\n\n`
frames. Tool / function calling preserved verbatim.

Auth header:
```
Authorization: Bearer polar_proxy_<32 hex chars>
```

Where the token's owning workspace + LLM config is resolved
server-side. Token has no upstream key in it.

## 4. Routing decision tree

For each incoming request:

```
1. Resolve token → (workspace_id, owner_user_id, scopes, rate_limit, default_llm_config_id)
   Token invalid / expired / revoked → 401 unauthorized

2. Parse request body's `model` field
   - If matches an llm_proxy_routes.alias_model for this token:
       use that route's llm_config_id
   - Else if token has a default_llm_config_id:
       use it (and warn in response headers if the requested model
       doesn't match the config's model)
   - Else:
       try to match `model` directly to an llm_configs row in the
       workspace by name
   - Else:
       400 with available models

3. Build aiRuntimeConfig from the chosen llm_configs row
   (api_key / base_url / model / system_prompt / proxy_url)

4. Map incoming request to aiChatCompletionRequest
   - messages: pass through
   - tools / tool_choice: pass through
   - max_tokens / temperature / etc: pass through

5. Call existing aiAgent.requestChatCompletion (or stream variant)
   with endpoint="proxy:<token_name>" so metrics slice by token

6. Marshal response back to OpenAI shape and return

7. logProxyCall (token_id, llm_config_id, model_requested,
   prompt_tokens, completion_tokens, latency_ms, http_status)
```

The routing is **operator-defined virtual models**. Examples:

```jsonc
// llm_proxy_routes for token "cursor-bob":
[
  { alias_model: "gpt-4o-mini",         llm_config_id: 12 },  // cheap
  { alias_model: "gpt-4o",              llm_config_id: 13 },  // expensive
  { alias_model: "polar:research",      llm_config_id: 14 },  // gemini-2.5-pro
  { alias_model: "polar:code-review",   llm_config_id: 15 },  // claude-sonnet
  { alias_model: "*",                   llm_config_id: 12 }   // catch-all
]
```

So Bob can use Cursor against `gpt-4o-mini` (cheap) for autocomplete
and switch to `gpt-4o` (expensive) for hard questions, both billed
on the operator's keys, both logged with Bob's token id.

## 5. Schema

```sql
CREATE TABLE llm_proxy_tokens (
  id BIGSERIAL PRIMARY KEY,
  owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workspace_id TEXT REFERENCES teams(id) ON DELETE CASCADE,
  name TEXT NOT NULL,                        -- "cursor-bob" / "raycast-laptop"
  token_hash TEXT NOT NULL UNIQUE,           -- sha256(plaintext_token)
  token_prefix TEXT NOT NULL,                -- first 12 chars, for display ("polar_proxy_a3b...")
  default_llm_config_id BIGINT REFERENCES llm_configs(id) ON DELETE SET NULL,
  scopes TEXT[] NOT NULL DEFAULT '{"chat.completions"}',
  rate_limit_rpm INTEGER NOT NULL DEFAULT 60,
  monthly_quota_usd NUMERIC(10,2),            -- NULL = unbounded
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ
);
CREATE INDEX ix_llm_proxy_tokens_owner ON llm_proxy_tokens(owner_user_id);
CREATE INDEX ix_llm_proxy_tokens_workspace ON llm_proxy_tokens(workspace_id);

CREATE TABLE llm_proxy_routes (
  id BIGSERIAL PRIMARY KEY,
  token_id BIGINT NOT NULL REFERENCES llm_proxy_tokens(id) ON DELETE CASCADE,
  alias_model TEXT NOT NULL,                 -- "gpt-4o" / "polar:research" / "*"
  llm_config_id BIGINT NOT NULL REFERENCES llm_configs(id) ON DELETE CASCADE,
  upstream_model_override TEXT,              -- optional: override the llm_config's model
  priority INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ux_llm_proxy_routes ON llm_proxy_routes(token_id, alias_model);

CREATE TABLE llm_proxy_calls (
  id BIGSERIAL PRIMARY KEY,
  request_id TEXT NOT NULL UNIQUE,           -- uuid for client correlation
  token_id BIGINT NOT NULL REFERENCES llm_proxy_tokens(id) ON DELETE CASCADE,
  llm_config_id BIGINT REFERENCES llm_configs(id) ON DELETE SET NULL,
  model_requested TEXT NOT NULL,
  model_used TEXT NOT NULL,
  provider_kind TEXT NOT NULL,               -- "openai" / "anthropic" / "gemini" / ...
  endpoint TEXT NOT NULL,                    -- "chat.completions" / "embeddings"
  stream BOOLEAN NOT NULL DEFAULT FALSE,
  tools_count INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER,
  completion_tokens INTEGER,
  total_tokens INTEGER,
  latency_ms INTEGER NOT NULL,
  http_status INTEGER NOT NULL,
  error_class TEXT,                          -- "upstream_error" / "rate_limit" / "timeout" / ...
  error_msg TEXT,
  estimated_cost_usd NUMERIC(10,6),          -- computed from a per-model price table
  called_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ix_llm_proxy_calls_token_time ON llm_proxy_calls(token_id, called_at DESC);
CREATE INDEX ix_llm_proxy_calls_workspace_time ON llm_proxy_calls(token_id, called_at DESC)
  WHERE error_class IS NULL;
```

`api_key` of the token NEVER stored plaintext — only `token_hash`
(sha256 + per-server pepper). On creation we return the plaintext
ONCE so the operator can copy it; subsequent UI shows
`polar_proxy_a3b...` (the prefix only).

## 6. Auth + tenancy

The proxy token IS the auth — no session cookie, no CSRF concern
(it's an API). Token resolution:

```go
func resolveProxyToken(c *gin.Context) (*ProxyTokenCtx, error) {
    // 1. Pull Authorization: Bearer <token>
    raw := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
    if !strings.HasPrefix(raw, "polar_proxy_") {
        return nil, errInvalidToken
    }
    hash := sha256sum(serverPepper + raw)
    // 2. DB lookup
    var t ProxyTokenCtx
    err := db.QueryRow(`SELECT id, workspace_id, owner_user_id,
        default_llm_config_id, scopes, rate_limit_rpm, revoked_at
        FROM llm_proxy_tokens WHERE token_hash = $1`, hash).Scan(&t...)
    if t.RevokedAt != nil { return nil, errRevoked }
    // 3. Touch last_used_at async
    go touchTokenAsync(t.ID)
    return &t, nil
}
```

The token's workspace_id binds every call to that workspace's
ACL. A token from workspace A can never reach workspace B's
llm_configs even if the proxy route mistakenly references one
(FK check in the resolver).

## 7. Streaming

OpenAI SSE format:
```
data: {"id":"chatcmpl-X","choices":[{"delta":{"content":"Hello"}}]}\n\n
data: {"id":"chatcmpl-X","choices":[{"delta":{"content":" world"}}]}\n\n
data: [DONE]\n\n
```

Our existing `streamOpenAICompatible` already speaks this — it
parses each frame upstream and re-emits to internal callbacks. For
the proxy we need to pump bytes through verbatim (do NOT re-parse
+ re-marshal; that breaks streaming latency). Two implementations:

- **A (simple but coupled)**: subclass the existing stream
  function to skip the JSON-parse step and pipe the raw upstream
  body lines straight to the proxy response. Pros: reuses connection
  setup, retry logic, proxy-URL handling. Cons: bypasses the
  streamOpenAICompatible's stream_options.include_usage path; we
  re-add token accounting at the end.
- **B (separate stream proxy)**: write a thin streamRawProxy that
  dials upstream + io.Copy the body. Pros: maximum throughput,
  minimum CPU. Cons: duplicates connection/proxy setup.

Recommend **A** + add a `bypassParse` flag to the stream func.
~50 LOC delta.

For the final usage accounting: rely on OpenAI's
`stream_options: {include_usage: true}` — every compatible
provider sends a final frame with `usage`. We sniff that one frame
to populate `llm_proxy_calls.{prompt,completion,total}_tokens`.
Non-compliant providers (Anthropic native, some old Gemini) → log
tokens as NULL and warn in the audit table.

## 8. Phase plan

| Phase | Scope | Est LOC | Risk |
|---|---|---|---|
| **P0** | Schema + token CRUD API + `POST /v1/chat/completions` non-stream + audit log | ~600 Go | low |
| **P1** | Streaming SSE (option A above) | ~150 Go | medium (race conditions on cancel) |
| **P2** | `llm_proxy_routes` table + routing decision tree + alias models | ~250 Go | low |
| **P3** | `GET /v1/models` enumeration (returns the token's routes + the workspace's available llm_configs) | ~120 Go | low |
| **P4** | `POST /v1/embeddings` (needs embedding-aware llm_configs schema first) | ~300 Go | medium (new provider surface) |
| **P5** | Per-token rate limit (token bucket in Redis) + monthly USD quota check | ~250 Go | medium |
| **P6** | Cost ledger: per-model price table + filled `estimated_cost_usd` + dashboard "spending by token / model / workspace" | ~400 Go + UI | medium |
| **P7** | UI: token management page (`/proxy-tokens.html`), routing editor, audit log viewer | ~600 TS | low |

P0–P3 are the MVP. P4–P7 are independently mergeable.

## 9. Testing

Lives in the existing harness pattern but on the **dock** side, not
agent. New file `internal/app/dock/llm_proxy_test.go` using
`httptest`:

- **Mock upstream** — an `httptest.NewServer` returning canned
  OpenAI-shaped responses. The token's `llm_config.base_url`
  points at it; the proxy code under test calls through.
- Scenarios:
  - `proxy-bad-token` → 401
  - `proxy-revoked-token` → 401
  - `proxy-chat-completions-basic` → request flows, response
    shape preserved, audit row written
  - `proxy-route-alias` — `model=polar:research` resolves to
    configured llm_config; mock upstream sees the OVERRIDE model name
  - `proxy-cross-workspace-denied` — token bound to workspace A
    can't reach workspace B's llm_config
  - `proxy-tools-passthrough` — tools array preserved on both
    directions
  - `proxy-stream-sse` — chunked frames flow through; final
    `usage` frame parsed; audit row populated
  - `proxy-upstream-5xx` — error wrapped + logged + 502 returned
  - `proxy-rate-limit` — N+1 request inside 1s window → 429
- Real-API smoke: gated by `POLAR_TEST_REAL_OPENAI_KEY=...` env
  (only the operator runs it; CI doesn't need a real key)

The agent test-serve harness is unrelated — proxy is dock-side,
client side is curl / OpenAI SDK.

## 10. Operator workflow

Adding a new tool that needs API access:

1. UI: `Settings → API Tokens → New token`
2. Pick a name ("cursor-bob"), pick a default llm_config or set
   routing rules, optional rate limit + monthly quota
3. Copy the plaintext token (shown ONCE)
4. In Cursor / Claude Desktop / whatever:
   - Base URL: `https://your-polar.example.com/api/proxy/v1`
   - API key: `polar_proxy_a3b...`
   - Model: whatever the operator's routing covers (or the
     llm_config's default model)
5. Use as normal — every call lands in `llm_proxy_calls`

To revoke: hit the X next to the token in the UI; subsequent calls
return 401.

To see "what is Bob using":
- `SELECT model_used, sum(total_tokens), sum(estimated_cost_usd)
   FROM llm_proxy_calls
   WHERE token_id = (SELECT id FROM llm_proxy_tokens
                     WHERE name = 'cursor-bob')
   AND called_at > now() - interval '7 days'
   GROUP BY model_used;`
  Eventually surfaced in UI (P6/P7).

## 11. Security

- Token is bearer-style — anyone with the plaintext can call. So:
  - HTTPS only (existing nginx already enforces)
  - Token prefix shown in UI (`polar_proxy_a3b...`) so leaks are
    diagnosable; plaintext only shown ONCE at creation
  - Token rotation = create-new + revoke-old
- Per-token rate limit (P5) caps damage of a leaked token
- Per-token monthly USD quota (P6) caps cost damage
- Audit log is append-only — operator can grep for unusual
  patterns (model_requested changing suddenly, high tokens etc)
- Cross-workspace isolation enforced at the resolver — even a
  misconfigured route can't escape the token's workspace

## 12. What this design does NOT cover

- **OAuth or per-user-on-demand keys** — token is operator-issued.
  Self-service token creation could come later
- **Anthropic-native API shape** (`/v1/messages`) — clients
  using Anthropic SDK directly should switch to OpenAI-compat mode.
  Could add as a thin shim later
- **Function-calling parallel execution** — we pass through the
  tools array but don't add any agent-side execution. The client
  still runs the tools and submits results back, exactly as with
  raw OpenAI
- **Caching** — semantic cache (a la GPTCache) is its own design;
  out of scope here. Could plug in later as a middleware
- **Multi-region failover** — fallback to a secondary llm_config
  on upstream timeout is P8+
- **Webhook delivery** for long-running completions — chat
  completions API is synchronous; if we add background
  long-running jobs, separate design

## 13. Recommended next action

Start P0: schema migration + non-stream chat completions handler +
4 harness tests (bad-token / basic / cross-workspace / upstream-5xx).
~600 LOC, ~1 day. Lands as a single PR; subsequent phases stack
or branch off main once P0 merges.

After P0, the UI work (P7) and the streaming work (P1) can land in
parallel.
