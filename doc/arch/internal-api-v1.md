# Polar- Internal API — v1

**Status:** Spec (2026-05-21). Implementation lands in Phase 1.
**Audience:** authors of any plugin service that needs to call Dock for shared-core data.
**Related:** `decision.md`, `database-ownership.md`, `plugin-developer-guide.md`.

This document is the **complete and exhaustive contract** between Dock and any plugin service (first-party or third-party). Anything not listed here is not available; expanding the surface requires a new ADR and a versioned API bump (v2).

## Transport

- **Protocol:** HTTPS (production) or HTTP (intra-host development). All endpoints accept `application/json` request bodies and return `application/json` responses.
- **Base URL:** `http://dock:8080/internal/v1/` inside the Polar- deployment. Nginx must **block external traffic** to `/internal/` — e.g. `location /internal/ { deny all; }` on the public listener, or a separate internal listener.
- **Versioning:** additive only within `v1`. Breaking changes get `v2` and run in parallel for one release.
- **Keepalive:** clients MUST reuse HTTP connections (`http.Transport` with `MaxIdleConnsPerHost ≥ 10`).

## Authentication: HMAC-SHA256 signed headers

Every internal-API request from a plugin to Dock MUST carry these three headers:

```
X-Polar-Plugin-Name: wg
X-Polar-Plugin-Timestamp: 1716297600         # Unix seconds, server clock skew window ±300s
X-Polar-Plugin-Sig: 7c3a91d0f8…              # hex-encoded HMAC-SHA256, 64 chars
```

### Canonical string

The HMAC input is a single string built from the request, joined by `\n`:

```
<HTTP_METHOD_UPPERCASE>\n
<REQUEST_PATH_WITH_QUERY>\n
<UNIX_TIMESTAMP_DECIMAL_STRING>\n
<HEX_SHA256_OF_BODY_OR_EMPTY_HASH>
```

Examples:

- `GET /internal/v1/users/u_abc\n1716297600\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` (empty body → SHA-256 of the empty string).
- `POST /internal/v1/notify\n1716297600\n<sha256 of marshaled JSON body>`.

### Signature

```
hmac_sha256_hex(PLUGIN_HMAC_KEY, canonical_string)
```

**Important — `PLUGIN_HMAC_KEY` is *not* the plaintext token.** It is the lowercase SHA-256 hex digest of the plaintext.

Why: Dock never persists the plaintext (only `sha256(plaintext)` in `plugin_modules.plugin_key_hash`), so both sides must derive the HMAC key the same way. The plaintext is shown **once** at plugin creation; the plugin operator computes the hash locally and stores *that* as the HMAC key. The plaintext can then be discarded.

```go
// Plugin-side (one-time setup, run after admin shows plaintext):
import "crypto/sha256"; "encoding/hex"
sum := sha256.Sum256([]byte(plaintextFromAdmin))
hmacKey := hex.EncodeToString(sum[:])  // 64 hex chars — store in plugin secrets
```

Dock-side, the middleware uses `plugin_modules.plugin_key_hash` directly as the HMAC key — no extra derivation. The two key derivations meet at the same 64-hex string. See `internal/app/dock/internal_api_middleware.go::computePluginSig`.

### Replay protection

Dock rejects any request whose `X-Polar-Plugin-Timestamp` differs from server time by more than 300 seconds. Plugins SHOULD use NTP. There is no nonce cache — the 5-minute window is the only replay guard.

### Error responses

| HTTP | When | Body |
|---|---|---|
| 401 | Missing / invalid signature, unknown plugin name | `{"error":"plugin auth failed","detail":"…"}` |
| 403 | Plugin row is `enabled=false` | `{"error":"plugin disabled"}` |
| 408 | Timestamp outside ±300s window | `{"error":"timestamp skew"}` |
| 404 | Path not in the v1 surface | `{"error":"not found"}` |
| 500 | Server error | `{"error":"server error","request_id":"…"}` |

## The 9 endpoints

### 1. `GET /internal/v1/auth/verify`

Validates an end-user session token (whichever the user presented to the plugin in `Authorization: Bearer …`).

**Query:** `token=<session_token>` (URL-encoded).

**Response 200:**
```json
{
  "user_id":       "u_abc",
  "username":      "Kong",
  "role":          "researcher",    // module role when granted, else the platform role
  "platform_role": "user",          // users.role — "user" or "admin"
  "module":        "opera",         // the calling plugin (X-Polar-Plugin-Name)
  "module_role":   "researcher",    // "" when this user has no grant in this module
  "workspace_id":  "t_root",        // active workspace (from X-Workspace-Id or personal team)
  "expires_at":    "2026-05-28T00:00:00Z"
}
```

**Module roles.** A plugin can have its own cast — 风洞 (`opera`) has `admin` / `researcher` /
`audience` — which is *not* the platform role: `users.role='admin'` means dock **site** admin, so
minting one for a module admin would escalate across the whole platform. Module grants therefore
live in `module_user_roles(module, user_id, role)` and are resolved here against the
HMAC-authenticated `X-Polar-Plugin-Name`, so a plugin can neither read nor claim another module's
roles.

`role` is overridden by the grant so an existing plugin picks it up with no code change;
`platform_role` keeps the unmasked `users.role`. A user with no grant is unchanged
(`module_role: ""`), so a plugin that has never issued grants sees exactly what it saw before.

Grants are created by redeeming a role-bearing invite code at registration
(`POST /api/register` with `invitation_code`) or by a site admin via
`POST /api/site-settings/module-roles`. Codes are minted with
`POST /api/site-settings/invite-codes {count, module, role, expires_in_days?, note?}`; a code's
human-readable prefix (`WT-CREATOR-…`) is cosmetic — the row is the authority, so a fabricated code
grants nothing, and an unrecognized code is **rejected** at registration rather than downgraded to a
plain signup. `GET /api/me` returns the same data user-side as `module_roles: {"opera":"researcher"}`.

Open-API (`polar_…`) tokens verify through this endpoint too but are not module-role aware — they
act as their owner's platform role.

**Response 401:** `{"error":"invalid session"}`.

**Caching:** plugins SHOULD cache by token for 30 seconds. Cache TTL must not exceed 60 seconds.

### 2. `GET /internal/v1/users/:id`

Public profile snapshot. Plugins use this to render usernames / icons next to data they own.

**Response 200:**
```json
{
  "id":         "u_abc",
  "username":   "Kong",
  "icon_url":   "/uploads/avatars/u_abc.png",
  "bio":        "…",
  "is_bot":     false,
  "created_at": "2025-09-01T00:00:00Z"
}
```

Email, password hash, sessions, and any other sensitive field are **not** in this response and never will be in v1.

### 3. `GET /internal/v1/users/:id/workspace`

Returns the user's personal team (always exists per Dock invariant).

**Response 200:**
```json
{ "workspace_id": "t_personal_u_abc" }
```

### 4. `GET /internal/v1/teams/:id`

**Response 200:**
```json
{
  "id":         "t_root",
  "slug":       "root",
  "name":       "Root Team",
  "owner_user_id": "u_admin",
  "created_at": "2025-01-01T00:00:00Z"
}
```

### 5. `GET /internal/v1/teams/:id/members/:user`

**Response 200:**
```json
{
  "team_id":   "t_root",
  "user_id":   "u_abc",
  "role":      "member",            // "owner" | "admin" | "member" | "viewer"
  "joined_at": "2025-09-01T00:00:00Z"
}
```

**Response 404:** user is not a member of the team.

### 6. `GET /internal/v1/llm-configs/:id?workspace_id=<wid>`

Returns the resolved LLM config a plugin may use to make an LLM call (with the API key plaintext). Dock enforces workspace scoping — if the config is not owned by, shared with, or marketplace-applied for the workspace, returns 403.

**Response 200:**
```json
{
  "id":          5,
  "name":        "ali qwen-plus",
  "base_url":    "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions",
  "model":       "qwen-plus",
  "api_key":     "sk-…",
  "proxy_url":   "",
  "is_system":   false,
  "is_shared":   true
}
```

**Response 403:** `{"error":"llm config not accessible to workspace"}`.

### 7. `POST /internal/v1/llm/chat-completion`

Convenience endpoint. Plugin sends OpenAI-compatible chat-completion request **with `workspace_id` + `llm_config_id`**; Dock runs it through the same `checkChatBillingGate` → `requestChatCompletion` → `recordAgentLLMCall` pipeline used by chat. Plugin doesn't need to hold any LLM credentials.

**Request:**
```json
{
  "workspace_id":    "t_root",
  "llm_config_id":   5,
  "model_override":  "qwen-max",         // optional
  "endpoint_tag":    "wg.install_advisor", // free-form, lands in llm_proxy_calls.endpoint
  "messages": [
    { "role": "system", "content": "…" },
    { "role": "user",   "content": "…" }
  ],
  "max_tokens": 512
}
```

**Response 200:**
```json
{
  "content":           "…",
  "model":             "qwen-max",
  "prompt_tokens":     120,
  "completion_tokens": 86,
  "total_tokens":      206,
  "latency_ms":        1180,
  "cost_usd":          0.0021
}
```

**Response 402:** workspace billing gate tripped. Body includes user-facing message (already localized in the same style as Dock's chat error placeholders).

**Response 502:** upstream LLM provider failure. Body includes classified error kind (`out_of_credit`, `network`, `auth`, `content_policy`, `unknown`).

### 8. `POST /internal/v1/notify`

Plugin → user notification fan-out (real since 2026-08-17; before that a stub that only counted devices).

**Request:**
```json
{
  "user_id":      "u_abc",                 // OR
  "workspace_id": "ws_123",                // all members of the workspace…
  "roles":        ["owner","admin"],       // …optionally filtered by team role (default: everyone)
  "title":        "云主机余额不足",
  "body":         "余额 0.5 币，24h 后停机 …",
  "kind":         "cloud.billing.grace",   // free-form tag; rendered as a trailing #hashtag
  "url":          "https://cloud.4950.store:2443/#billing",
  "data":         { "tenant_id": "ws_123" }
}
```
`user_id` alone → the notice lands in that user's personal workspace. `workspace_id` + `user_id` → only that member.

**Channels (all best-effort):**
- **room** — a per-workspace system room named `系统通知` (created on demand by the `system` user) receives the notice as a text message from `system`; recipients are added as members if missing, so it shows up live (chat:events) + unread like any room message. This channel always works.
- **email** — each recipient with an email, only if the dock has `SMTP_*` configured (zen currently does not → 0).
- **push** — not wired (no APNs pipeline for non-chat payloads); the count of pushable devices is reported as `push_skipped`.

**Response 200:** `{ "delivered": {"room": 2, "email": 0, "push": 0}, "push_skipped": 1, "recipients": 2, "workspace_id": "ws_123", "room_id": 42, "kind": "cloud.billing.grace" }`.
**400** if neither `user_id` nor `workspace_id`, or neither `title` nor `body`; **404** unknown user/workspace.

### 9. `POST /internal/v1/plugin-registry/heartbeat`

Plugin announces it is alive. Called **every 30 seconds** by each plugin process. Dock updates `plugin_modules.last_heartbeat` and the version column.

**Request:**
```json
{
  "name":     "wg",                       // must match the auth header X-Polar-Plugin-Name
  "version":  "1.2.3",
  "endpoint": "http://wg-svc:8090",       // self-reported, must match nginx config
  "uptime_seconds": 86400,
  "metrics_url":    "http://wg-svc:8090/metrics"  // optional, Prometheus scrape target
}
```

**Response 200:** `{ "ack": true, "config_version": 7 }` — `config_version` lets the plugin know when to re-pull its config.

The first time a name appears, the plugin is auto-rejected unless an admin has provisioned the row in `plugin_modules` (with the HMAC key). This prevents an attacker who learns one key from creating new plugin identities.

---

## v1.1 additions (Phase 2 handler-migration prereqs)

These 5 endpoints unblock the Phase 2 batch migration of packtunnel / library / video / projects / hosts / iosdist handlers. Same HMAC gate as the v1 surface.

### 10. `GET /internal/v1/bot-users/:id`

`:id` is the bot's `user_id` (the `"u_..."` string), NOT the bigserial PK. Used by projects and hosts handlers to render bot metadata + look up the bot's LLM binding.

**Response 200:**
```json
{
  "id": 7,
  "bot_user_id": "u_helper",
  "workspace_id": "t_root",
  "owner_user_id": "u_kong",
  "name": "Helper Bot",
  "description": "…",
  "llm_config_id": 42,
  "bot_kind": "classic",
  "preferred_tool": "kimi",
  "host_skill_id": null,
  "model_override": ""
}
```

**404** if no such bot.

### 11. `GET /internal/v1/chat-threads/:id`

Used by projects' `project_research_runs` UI to dereference the `chat_thread_id` pointer for display.

**Response 200:**
```json
{
  "id":              42,
  "user_low":        "u_kong",
  "user_high":       "u_helper",
  "last_message":    "First 200 chars of last message…",
  "last_message_at": "2026-05-22T01:23:45Z",
  "created_at":      "2026-05-01T00:00:00Z"
}
```

`last_message` is server-truncated to ≤200 chars. Full thread history is read by the end-user's token via `/api/chat/*` (NOT via this internal endpoint).

**404** if no such thread.

### 12. `GET /internal/v1/agent-presence/:bot_id`

`:bot_id` is the bot's `user_id`. Returns the live `agentHub` attach state. Plugins use this to decide whether to enqueue an agent task vs. surface "agent offline" in the UI.

**Response 200 (agent attached):**
```json
{
  "bot_user_id":  "u_helper",
  "attached":     true,
  "host_id":      "h_zen",
  "tool":         "kimi",
  "last_seen_at": "2026-05-22T01:23:45Z"
}
```

**Response 200 (no agent attached):**
```json
{ "bot_user_id": "u_helper", "attached": false }
```

Always 200 — empty state is not an error. If dock's `agentHub` is uninitialized (e.g. during boot), behaves the same as "no agent".

### 13. `POST /internal/v1/agent/dispatch`

Asks dock to enqueue an `aiAgentTask` for an attached agent. Returns immediately; the LLM round-trip happens asynchronously inside dock's `aiAgent` loop, and the result lands on the chat_thread as a new message (caller poll-or-WS-watches it).

**Request body:**
```json
{
  "thread_id":         42,
  "llm_thread_id":     7,        // optional
  "user_id":           "u_kong",
  "responder_user_id": "u_helper",
  "responder_name":    "Helper Bot",  // optional
  "content":           "Please review …"
}
```

**Response 200:** `{ "queued": true }`.

**503** if dock's `aiAgent` is nil (e.g. during shutdown).

### 14. `POST /internal/v1/agent/llm-call-record`

Cost-ledger insert for plugins that run their own LLM dispatch (anything not going through `AgentDispatch`). Keeps `/llm-billing.html` totals consistent across all caller paths.

**Request body:**
```json
{
  "workspace_id":       "t_root",
  "llm_config_id":      42,
  "model_requested":    "gpt-4",
  "model_resolved":     "gpt-4-turbo",
  "prompt_tokens":      120,
  "completion_tokens":  60,
  "total_tokens":       180,
  "latency_ms":         2300,
  "status_code":        200,
  "error_text":         "",
  "cost_override_usd":  null     // optional; dock estimates via llm_proxy_pricing if absent
}
```

**Response 200:** `{ "recorded": true }`.

Empty `workspace_id` or `llm_config_id<=0` is treated as a silent no-op (the dock helper has the same semantics; plugins don't need to pre-validate).

## Go client snippet

A reference HMAC client a plugin author can copy. The full SDK (`github.com/<org>/polar-plugin-sdk-go`) ships in Phase 3 — for now, copy this into `internal/plugins/<name>/client.go`.

```go
package plugin

import (
    "bytes"
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "strconv"
    "time"
)

type Client struct {
    DockBase string        // "http://dock:8080"
    Name     string        // plugin name as registered
    // Key is the *HMAC key*, NOT the plaintext token Dock printed at
    // registration. Derive once with:
    //     sum := sha256.Sum256([]byte(plaintextFromAdmin))
    //     key := []byte(hex.EncodeToString(sum[:]))  // 64-byte hex
    // and store that — discard the plaintext. See "Signature" above.
    Key      []byte
    HTTP     *http.Client
}

func (c *Client) Do(method, path string, body any) (*http.Response, error) {
    var bodyBytes []byte
    if body != nil {
        b, err := json.Marshal(body)
        if err != nil {
            return nil, err
        }
        bodyBytes = b
    }
    ts := strconv.FormatInt(time.Now().Unix(), 10)
    bodySum := sha256.Sum256(bodyBytes)
    canonical := fmt.Sprintf("%s\n%s\n%s\n%s", method, path, ts, hex.EncodeToString(bodySum[:]))
    mac := hmac.New(sha256.New, c.Key)
    mac.Write([]byte(canonical))
    sig := hex.EncodeToString(mac.Sum(nil))

    req, err := http.NewRequest(method, c.DockBase+path, bytes.NewReader(bodyBytes))
    if err != nil {
        return nil, err
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("X-Polar-Plugin-Name", c.Name)
    req.Header.Set("X-Polar-Plugin-Timestamp", ts)
    req.Header.Set("X-Polar-Plugin-Sig", sig)
    return c.HTTP.Do(req)
}

// AuthVerify wraps GET /internal/v1/auth/verify with a 30s in-memory cache.
// (Cache implementation omitted for brevity — sync.Map keyed on token, TTL 30s.)
```

## Verification

Phase 1 ships a Go test in `internal/dock/internal_api_test.go` that:

1. Provisions a test plugin row with a known key.
2. Calls each of the 9 endpoints with valid + invalid signatures + skewed timestamps + disabled plugin.
3. Asserts the documented response shapes.
4. Asserts that an empty / unknown plugin name → 401.
5. Asserts the `/llm/chat-completion` path correctly increments `llm_proxy_calls` with `endpoint='<endpoint_tag from request>'` and the workspace's billing quota.

## Out of scope for v1

- Any write to Dock's user / team / llm_config / chat tables. Plugins observe only.
- Streaming responses (SSE / chunked). `/llm/chat-completion` is non-streaming. Add `v1` `/llm/chat-completion-stream` later if needed.
- File upload to Dock's storage. Plugins handle their own storage; if cross-plugin file sharing becomes needed, an `/internal/v1/storage/sign-url` endpoint will be added in v1.1.
- gRPC, protobuf, OpenAPI auto-generation. Hand-written contract suffices at this scope.
