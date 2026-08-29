# Bots list at scale — server pagination + search + cache (design)

Status: **DESIGN** (2026-06-18). Product-grade, not MVP. Driven by loading 744 Cherry
assistant bots into a workspace → `bots.html` choked. Root cause is the list path, not the
data; fix it properly so a workspace can hold a large bot library and the page stays fast.

## What's wrong today (measured)
`GET /api/bots` returns **every** bot in the workspace, **including the full
`system_prompt`** of each → 774 bots = **1.7 MB** JSON (565 K chars of prompts). Server
serves it in 40 ms; the **client** dies:
1. `renderBotUserList` does one `innerHTML` of all 774 cards and **markdown-renders every
   full prompt** → DOM/CPU blowup.
2. After render, `await Promise.all(bots.map(fetch /status))` fires **one status request
   per bot — 774 concurrent**; the browser caps ~6/host so 768 queue → page appears hung.

Neither scales. The list must be paginated + searchable + light; status must be batched.

## API redesign — `GET /api/bots`
Backward-compatible (no params ⇒ legacy behavior during rollout, then flip the UI).

| param | meaning |
|---|---|
| `q` | full-text-ish search over name + description (ILIKE / trigram) |
| `group` | filter by category/tag |
| `cursor` / `limit` | keyset pagination (default `limit=30`, max 100) |
| `summary=1` | **omit `system_prompt`** from list items (the 1.7 MB → ~80 KB) |

Response:
```jsonc
{
  "bots": [ { "id", "name", "description", "llm_config_id", "config_name",
              "bot_kind", "group", "has_agent" } ],   // NO system_prompt in list
  "next_cursor": "…",          // null at end
  "total": 774,
  "groups": [ {"name":"工具","count":276}, … ]         // facets for filter chips
}
```
- **Full prompt only via detail**: `GET /api/bots/:id` returns `system_prompt`; the UI
  lazy-loads it when a card is expanded.
- **Batch status**: replace per-bot `/status` with `POST /api/agent/bots/status {ids:[…]}`
  → `{id: {connected, ip, uptime}}`; UI calls it once for the **visible** agent/passthrough
  bots only (not classic, not off-screen).

## Schema — category/tags (P1)
Cherry bots have a `group` ("工具/教育/职业/…") that we dropped on import. For product-grade
filtering, add to `bot_users`:
```sql
ALTER TABLE bot_users ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS idx_bot_users_ws_name_trgm
  ON bot_users USING gin (name gin_trgm_ops);   -- fast ILIKE search (pg_trgm)
```
Backfill the 744 from `bots-optimized.json` (`groups[]`). `group` facet = unnest(tags).

## Server-side cache (hot lists)
Bots change rarely; the list is read constantly. Cache the **hot** queries:
- Key: `(workspace_id, q, group, cursor, limit, summary)`. Value: the rendered JSON page.
- Store: in-process LRU (a few hundred entries) **or** Redis (`bots:list:<ws>:<hash>`),
  TTL ~60 s. The common case — `q="" group="" cursor=first` — is the one to keep warm.
- **Invalidate** on any bot create/update/delete in that workspace (bump a per-workspace
  `bots_version` counter that's part of the cache key → O(1) invalidation, no key scan).
- Facet counts (`groups`) cached per workspace, same invalidation.

## UI (`bots.html` / `src/bots.ts`)
- **Search box** (debounced 250 ms) → `q`; **group chips** (from `groups` facet, with counts).
- **Paged / infinite-scroll** render: render `limit` cards, "load more" on scroll via
  `next_cursor`. Never one big `innerHTML` of everything.
- **Lazy prompt**: card shows name/description/config; expand → `GET /api/bots/:id` →
  render markdown once, cache in memory.
- **Status**: batch endpoint, only for visible agent/passthrough bots.
- Virtualize if a single page ever gets large (not needed at limit=30).

## Phases (慢慢做, each its own PR → stage → deploy)
- **P0 — API relief** (polar-dock): add `q`/`cursor`/`limit`/`summary` to `GET /api/bots`
  (keyset pagination + `summary` drops `system_prompt`) + `GET /api/bots/:id` returns full +
  `POST /api/agent/bots/status` batch. Biggest single win.
- **P1 — search/facets/cache** (polar-dock): `tags` column + pg_trgm index + backfill 744
  groups + `group` facet + per-workspace cache w/ version-bump invalidation.
- **P2 — UI** (polar-dock-ui): search + group chips + infinite scroll + lazy prompt + batched
  status. Flip the UI to `summary=1` paginated calls.
- **P3 (later, optional)** — the site-wide "shared/system bot" model is a *separate* design
  ([[project-llm-architecture]] note); explicitly out of scope here per owner.

## Interim
Until P2 ships, `bots.html` is overloaded at 774. Options if you need it usable sooner:
park the full 744 in `~/cherry-bots/bots-optimized.json` and keep a curated ~120 in the
workspace; re-load the rest once pagination lands. (Owner chose to build it properly instead
of trimming — full set stays.)

Related: [[reference-llm-architecture]], [[project-qunliao-state]] (bot/rooms), the import
loaders in `zen:~/cherry-bots/`.
