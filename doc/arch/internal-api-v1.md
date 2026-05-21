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
hmac_sha256_hex(PLUGIN_INTERNAL_KEY, canonical_string)
```

The key is the raw secret bytes (32+) provisioned for the plugin in Dock's `plugin_modules.plugin_key_hash`. The plaintext key is shown **once** at plugin creation; Dock only stores the SHA-256 hash.

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
  "user_id":      "u_abc",
  "username":     "Kong",
  "role":         "user",          // or "admin"
  "workspace_id": "t_root",        // active workspace (from X-Workspace-Id or personal team)
  "expires_at":   "2026-05-28T00:00:00Z"
}
```

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

Sends a push notification fan-out to a user's registered devices.

**Request:**
```json
{
  "user_id": "u_abc",
  "title":   "wg device registered",
  "body":    "iPhone 16 joined the mesh",
  "kind":    "wg.device_registered",
  "data":    { "device_id": "d_xyz" }
}
```

**Response 200:** `{ "delivered": 2, "skipped": 0 }`.

`kind` controls which notification template / icon the client renders. New `kind` values can be added by plugins freely — the client's mapping is permissive.

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
    Key      []byte        // 32+ byte secret
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
