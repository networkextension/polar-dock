# Qunliao API — iOS client reference

Server: Polar- dock (`internal/app/dock`). Production endpoint: `https://zen.4950.store`. All paths below are relative to that origin.

Vision + roadmap: see [`doc/qunliao/dev-plan.md`](dev-plan.md). This document is the canonical wire contract for the iOS client; web client uses the same endpoints.

**Status as of 2026-05-19**: Phase **P0** is live (rooms + members + messages + realtime WS broadcast). **P1** (`@mention` multi-bot fanout) and **P3** (idle loop) are planned — sections at the end of this doc are marked 🚧 so iOS can stub UI ahead of the backend ship.

---

## 1. Auth

Two ways to carry the access token; **Bearer wins over cookie**:

```
Authorization: Bearer <access_token>          # preferred for native iOS
Cookie: access_token=<access_token>           # web fallback
```

Token is minted by `POST /api/login`. Server-side token extraction lives at `internal/app/dock/middleware.go:62` (`extractAccessToken`).

### Active workspace

Rooms are workspace-scoped. Pick a workspace via header on every request:

```
X-Workspace-Id: <team_id>
```

If omitted, the server resolves the caller's **personal team** (`resolveActiveWorkspace`, `middleware.go:155`). For iOS this means the workspace switcher must remember the user's last selection and stamp it on every API call + on the WS upgrade.

### Errors

All non-2xx responses are `application/json` with:

```json
{ "error": "human-readable Chinese or English string" }
```

Common codes:

| Code | When |
|---|---|
| 401 | Token missing/expired — re-auth via `/api/login` or `/api/auth/refresh` |
| 403 | Caller is not a member (read endpoints) or not the room owner (mutating endpoints); also covers workspace-mismatch on `X-Workspace-Id` |
| 404 | Room or member doesn't exist (or is archived) |
| 409 | Conflict — currently only relevant on bot create (will land with PR #1, see §6) |
| 500 | Server error — surface as a retry-or-report toast |

---

## 2. HTTP surface (live in P0)

All `/api/rooms/*` routes require `AuthMiddleware` + `X-Workspace-Id`. Member-only and owner-only gates are noted per-row.

### 2.1 Rooms

#### `GET /api/rooms?include_archived=0|1`

List rooms the caller is a member of. Returns archived only when `include_archived=1`.

Response:
```json
{ "rooms": [ Room, ... ] }
```

#### `POST /api/rooms`

Create a room. Creator is automatically added as `owner` in the same DB transaction (`createChatRoom` in `chat_rooms_store.go`).

Request:
```json
{
  "kind":  "room",         // optional, default "room". Allowed: "room" | "dm" | "whisper" | "convene"
  "name":  "产品讨论组",    // optional but recommended
  "topic": "Sprint 0519"   // optional
}
```

Response: `201 Created`
```json
{ "room": Room }
```

#### `GET /api/rooms/:id`

Read a single room + the caller's membership row. **403 if not a member.**

Response:
```json
{ "room": Room, "member": RoomMember }
```

#### `PUT /api/rooms/:id`

Update room name / topic. **Owner only**.

Request (any subset):
```json
{ "name": "新名字", "topic": "新话题" }
```

Response: `{ "ok": true }`

#### `DELETE /api/rooms/:id`

Soft-archive: sets `archived_at = now()`. Row is preserved + still queryable via `?include_archived=1`. **Owner only**.

Response: `{ "ok": true }`

### 2.2 Members

#### `GET /api/rooms/:id/members`

Member-gated. Returns members sorted by `joined_at` ASC.

```json
{ "members": [ RoomMember, ... ] }
```

#### `POST /api/rooms/:id/members`

**Owner only.** Idempotent — adding an existing member is a no-op.

Request:
```json
{ "user_id": "abc123", "role": "member" }   // role optional, default "member"
```

Roles: `owner` | `member` | `observer`.

Response: `201 Created` → `{ "ok": true }`

#### `DELETE /api/rooms/:id/members/:userId`

**Owner only.** Path param is URL-encoded user_id.

Response: `{ "ok": true }`, or 404 if the membership didn't exist.

### 2.3 Messages

#### `GET /api/rooms/:id/messages?limit=&before_id=`

Member-gated. Returns up to `limit` newest-first messages; for backwards pagination pass `before_id` (id of the oldest message you already have).

- `limit`: default 50, max 200
- `before_id`: optional int64 cursor; filter `id < before_id`

```json
{ "messages": [ ChatMessage, ... ] }
```

#### `POST /api/rooms/:id/messages`

Member-gated. Sends a message and publishes a realtime `room_message` event to every member (see §3).

Request:
```json
{
  "content": "你好 @bot",
  "message_type": "text"    // optional, default "text"
}
```

Response: `201 Created`
```json
{
  "message": {
    "id":           1234,
    "room_id":      42,
    "sender_id":    "user_abc",
    "message_type": "text",
    "content":      "你好 @bot",
    "created_at":   "2026-05-19T07:30:00Z"
  }
}
```

The response is intentionally a thin echo (no `username`/`icon`). For the full enriched row, listen for the WS event or re-fetch via `GET .../messages`.

---

## 3. WebSocket realtime

Endpoint: `wss://zen.4950.store/ws/chat` (use `ws://` for non-TLS dev). Same `/ws/chat` used by 1:1 chat — there's no separate room socket.

### Connect

Open the socket and pass the access token + workspace header as you would on HTTP. Native clients should use Bearer; web cookies travel automatically.

### Events

Server pushes JSON objects of shape:

```ts
{
  type:        "message" | "room_message" | "read" | "revoke" | "presence",
  chat_id?:    number,        // 1:1 thread id; present on legacy "message" events
  room_id?:    number,        // qunliao room id; present on "room_message"
  message?:    ChatMessage,   // full enriched row on "message" + "room_message"
  message_id?: number,
  user_id?:    string,
  read_at?:    string,
  deleted_at?: string,
  online?:     boolean,
  device_type?: string,
  last_seen_at?: string
}
```

### `room_message`

Fired whenever any room message lands (human send, bot reply later in P1, idle-loop post later in P3). Every member of the room receives the event on every active client.

Client behavior:

1. Look at `event.room_id`. If it matches the room currently open on screen, append `event.message` to the message list (after dedup by `message.id`).
2. Otherwise, just bump the room's unread badge in the room list.

Server publishes via Redis `chat:events` → all dock instances broadcast (`handleRoomMessageCreatedEvent` in `chat_events.go`). iOS does **not** need to send any subscribe command — membership in `chat_room_members` is the implicit subscription.

### Reconnect / fallback

The web client uses 5s polling as a fallback when the WS drops; iOS should do the same. On reconnect, `GET /api/rooms/:id/messages?limit=N` to catch up.

---

## 4. Canonical types

Pulled from `ui/src/types/room.ts` — these are the wire contract.

```ts
type RoomKind = "room" | "dm" | "whisper" | "convene";
type RoomRole = "owner" | "member" | "observer";

interface Room {
  id:                 number;
  workspace_id:       string;
  kind:               RoomKind;
  name:               string;
  topic:              string;
  created_by_user_id: string;
  created_at:         string;    // RFC3339 / ISO 8601
  archived_at?:       string;
}

interface RoomMember {
  room_id:               number;
  user_id:               string;
  role:                  RoomRole;
  joined_at:             string;
  last_read_message_id?: number;
}

// ChatMessage is shared with 1:1 DMs — both `thread_id` and `room_id`
// are optional; exactly one is set per row.
interface ChatMessage {
  id:                number;
  room_id?:          number;
  thread_id?:        number;
  llm_thread_id?:    number;
  sender_id:         string;
  sender_username:   string;
  sender_icon:       string;
  message_type:      string;   // "text" today; richer kinds added incrementally
  failed:            boolean;
  content:           string;
  created_at:        string;
  llm_config_id?:    number;
  llm_config_name?:  string;
  llm_model?:        string;
}
```

Equivalent Swift skeleton (use `Codable`):

```swift
struct Room: Codable {
    let id: Int64
    let workspaceId: String
    let kind: String          // RoomKind enum value
    let name: String
    let topic: String
    let createdByUserId: String
    let createdAt: Date
    let archivedAt: Date?
    // … set up CodingKeys to map snake_case → camelCase
}
```

---

## 5. Sender attribution

`sender_id` on a ChatMessage maps to `users.id`. Three cases:

| Pattern | What it means | UI hint |
|---|---|---|
| `users.id` of a human | Real teammate | Use their avatar from `sender_icon` |
| `users.id` of a bot (joined via `bot_users.bot_user_id`) | LLM reply (P1) or idle-loop post (P3) | Bot avatar + role badge |
| `users.id` of the system bot (`is_system=TRUE` in `llm_configs`) | Platform AI assistant | Special "system" badge |

iOS should fetch the workspace user list once (cache by `workspace_id`) and join against it locally for avatar/name rendering.

---

## 6. 🚧 Coming: P1 — `@mention` multi-bot fanout

**Status**: planned, not deployed.

When this lands, **no API shape changes** — `POST /api/rooms/:id/messages` continues to accept and return the same payloads. The new behavior is:

1. Server parses the outgoing `content` for `@bot_name` tokens (regex: `@([A-Za-z0-9_\-\p{Han}]{1,32})`).
2. For each token matching a `bot_users.name` row whose `bot_user_id` is also in `chat_room_members` for this room, the server fires an LLM call against the bot's `llm_configs` row.
3. Each bot reply lands as a new `chat_messages` row, then fans out via the same WS `room_message` event with `sender_id = bot.bot_user_id`.

iOS UI prep:

- Tokenize `@name` in message rendering — recommend a blue chip style — and short-circuit any system-keyboard auto-correction inside an `@` token.
- Be ready for **multiple inbound `room_message` events in quick succession** after a single send, attributed to different bot senders.
- Optionally fetch `GET /api/llm-configs` or a new "bots in this room" helper (TBD) to power an `@`-typeahead picker.

Bot name uniqueness rule (PR #1, see plan): names are unique per workspace. Attempting to create a duplicate returns `409 Conflict` with `{"error":"workspace 内已有同名 bot..."}`. iOS bot-management forms should surface this.

---

## 7. 🚧 Coming: P3 — agent idle loop

**Status**: planned, not deployed.

Lets a room owner enable per-bot "speak up on your own" behavior. Each enabled (room, bot) pair has a timer; when it fires, the server asks the bot's LLM "should you post right now? if yes, what?" and inserts the result as a normal room message if so.

Planned routes (owner-only):

```
GET    /api/rooms/:id/agent-loops                       → list per-bot settings
PUT    /api/rooms/:id/agent-loops/:botUserId            → upsert
       body: { enabled, interval_secs, max_per_hour }
DELETE /api/rooms/:id/agent-loops/:botUserId            → disable + clear settings
```

Constraints:
- `interval_secs` ∈ [60, 3600] (1 min – 1 h)
- `max_per_hour` ∈ [1, 30]
- Default: disabled. Server auto-disables a loop when the workspace's billing gate trips.

iOS UI prep:

- Room owner: add a small "🤖 自主性" toggle next to each bot in the member list.
- Non-owners: read-only badge ("🟢 active" / "💤 idle") visible to everyone in the room.
- Bot self-posts arrive via the **same `room_message` WS event** as any other message — no new event type. Distinguishing self-posts from `@mention` replies isn't user-visible; both are attributed to the bot's `sender_id`.

---

## 8. Versioning

This document tracks the wire contract. When the backend changes a shape, the corresponding section is updated in the same PR. iOS clients should pin to a `dev-plan` phase ("we support P0 + P1") rather than to a date; phase changes mean new fields or routes appear, never silent shape changes.

For corrections / additions, ping the backend owner or open an issue on the Polar- repo.
