# Microsoft Teams Integration — Feasibility Research

**Status**: research only, no implementation. Decision recorded:
**don't replace, do hybrid integration if/when a paying M365 customer asks for it.**

**Date**: 2026-05-07

## Question

Can we offload Polar's chat / workspace / bot infrastructure to
Microsoft Teams (Microsoft Graph + Bot Framework) instead of
maintaining our own?

The Polar-side components in scope:

1. **Chat** — 1-on-1 user/user and user/bot direct messages, with
   WebSocket real-time delivery. Messages can have attachments
   (zip/pdf/doc).
2. **Workspaces (teams)** — multi-user collaboration spaces with
   member roles (owner / admin / member), invitation tokens.
3. **Bots** — workspace bots that respond to user messages. Two
   flavors: classic (platform calls an LLM API and replies) and
   "passthrough" (a polar-agent CLI on the user's local Mac runs
   kimi-cli / claude / codex and pipes the reply back over a
   WebSocket the platform hosts).
4. **File attachments** — workspace-scoped uploads (zip/pdf/doc/
   docx/ppt/pptx, 100 MiB cap, sha256 dedup).
5. **Tasks / project management** — out of scope (platform-specific).

## A. Microsoft Graph for chat + channels + files

**Endpoints in play:**

- 1:1 / group DM: `POST /chats/{id}/messages` ([chat-post-messages][1])
- Channel: `POST /teams/{id}/channels/{id}/messages` ([channel-post-messages][2])
- Files: `POST /drives/{id}/items/{parent}:/{name}:/createUploadSession`
  (chunked, 320 KiB-aligned, up to 250 GB) ([driveItem createUploadSession][3])
- Real-time inbound: Graph change-notification subscriptions on
  `chatMessage` with `includeResourceData=true` ([change notifications][4])

**The "post as platform" problem.** App-only `ChatMessage.Send`
is **not** allowed for live messages — only for migration imports,
which Microsoft has explicitly flagged as a future paid SKU
([permissions reference][5]). The supported route is **Resource-Specific
Consent (RSC)** with `ChatMessage.Send.Chat` /
`ChannelMessage.Send.Group`, but RSC requires the app be
*installed in that chat/team* by a tenant user/admin ([RSC docs][6]).
So "platform posts on behalf of itself" effectively becomes
"platform posts via a Teams app the customer installed."

## B. Bot Framework for Teams

Bots receive activities via inbound HTTPS POST to `/api/messages`
from Azure Bot Service — **the Teams channel is HTTPS-only, no
inbound WebSocket** ([channel-connect-teams][7]). Polar's
polar-agent backend doesn't need to change shape — the Go server
becomes the bot endpoint, then keeps the *existing* WebSocket
between server ↔ polar-agent CLI. The bot piece is just one extra
HTTPS handler that translates Teams Activity → internal job, then
`POST /conversations/{id}/activities` to reply.

**Deployment.** Single-tenant works fine: zip the manifest +
sideload via Teams admin center (Manage apps → Upload) under a
custom-apps policy ([apps-upload][8]). AppSource publication is
**only** required for one-click discovery across tenants we don't
control. Each customer tenant must still install the app — there
is no zero-touch path.

## C. Authentication boundary

Every Teams user needs an Entra ID identity in *some* tenant.
**Free-tier Polar users with no M365** are out — they can join as
B2B guests in a customer's tenant, but they cannot host their own
Teams or be the workspace owner without a license.

Hybrid is feasible (tenant-routing flag per workspace:
`chat_backend = "polar" | "teams"`) and is the only sane path if
any non-M365 users exist.

## D. Engineer-weeks (one mid-senior eng, ballpark)

| Scope | Estimate | Assumes |
|---|---|---|
| Workspace metadata → Teams (teams/channels CRUD) | **2–3 wk** | accept M365-only users; keep own user table joined by Entra `oid` |
| 1:1 + channel chat replacement (send + Graph subscription ingest + attachments via OneDrive) | **5–8 wk** | RSC manifest, webhook validation, lifecycle re-subscribe job, retry/idempotency for the 10s SLA |
| Bot scenario on Bot Framework SDK + keep polar-agent WS | **3–4 wk** | thin Activity↔WS adapter; auth via JWT bot creds |
| Migrate existing chat history | **3–6 wk** | write-only migration path uses app-only import perms with throttling; lossy on reactions/threads |
| **Full replacement total** | **13–21 wk** | + ongoing Azure Bot Service + Graph throttling tax |

The migration line item is the riskiest — Microsoft has
telegraphed import will become billable.

## E. Dealbreakers

1. **Free / non-M365 users** — hard wall.
2. **App-only "post as platform" without RSC install** — not
   supported; every customer tenant must install our manifest.
3. **WebSocket inbound to bot** — not available; we'd inject an
   HTTPS shim. (Tolerable, not fatal.)
4. **Legacy Incoming Webhooks** — being retired April 30 2026
   ([retirement notice][9]). Don't build on them; use Workflows
   or a real bot.
5. **Latency** — Graph notifications have a 10s response SLA and
   missing-notification issues for `chatMessage` are a known
   long-standing complaint; current in-process WebSocket is faster.
6. **Custom UI** (passthrough job status, PRD-decompose flow,
   attachment dedup UX) — Adaptive Cards cover ~70% but the
   bespoke feel is gone.
7. **100 MiB / sha256 dedup** — OneDrive supports the size easily
   but does not expose pre-upload sha256 dedup; we'd dedup
   client-side before `createUploadSession`.

## F. Minimum viable hybrid (recommended)

Skip replacement. Ship a **Teams connector app** in ~2–3 weeks
that gives 80% of the value:

1. **SSO** — Entra ID OIDC as an auth provider alongside the
   existing one. Maps `oid` → Polar user.
2. **Outbound notifications** — when a Polar bot completes a task
   or a workspace event fires, `POST` an Adaptive Card via a
   **channel Workflow webhook** ([replacement for connectors][10]).
   No manifest, no AppSource.
3. **Optional bot** — a single sideloadable manifest letting
   `@polar decompose <PRD>` from Teams; routes through the
   existing API and the existing polar-agent WS unchanged.
4. **Deep-link back to Polar** — Adaptive Card "Open in Polar"
   buttons.

This keeps polar-agent's WebSocket flow, supports free-tier users
(they stay on Polar's chat), and gives M365 customers a "we
integrate with Teams" checkbox.

## Recommendation

**Don't replace.** The app-only-posting restriction + free-tier
blocker + Graph notification SLA each independently kill full
replacement; together it's a clear no.

**Do build the hybrid (F)** — but not yet. Trigger condition:
> An identifiable enterprise customer asks for it AND has an
> active M365 tenant.

Until that trigger fires, this stays in `FUTURE_WORK.md`. The
research above is the basis for that future scope; we won't have
to re-research from scratch.

If a single enterprise customer ever demands "all chat in Teams,"
ship the per-workspace `chat_backend` flag described in C — but
make them pull, don't push.

## Source links

- [chat-post-messages][1]
- [channel-post-messages][2]
- [createUploadSession][3]
- [Change notifications with resource data][4]
- [Graph permissions reference][5]
- [Resource-Specific Consent (RSC)][6]
- [Connect Bot Framework bot to Teams][7]
- [Upload custom Teams app (sideload)][8]
- [O365 Connectors retirement notice][9]
- [Workflows replacement for incoming webhooks][10]

[1]: https://learn.microsoft.com/en-us/graph/api/chat-post-messages?view=graph-rest-1.0
[2]: https://learn.microsoft.com/en-us/graph/api/channel-post-messages?view=graph-rest-1.0
[3]: https://learn.microsoft.com/en-us/graph/api/driveitem-createuploadsession?view=graph-rest-1.0
[4]: https://learn.microsoft.com/en-us/graph/change-notifications-with-resource-data
[5]: https://learn.microsoft.com/en-us/graph/permissions-reference
[6]: https://learn.microsoft.com/en-us/microsoftteams/platform/graph-api/rsc/resource-specific-consent
[7]: https://learn.microsoft.com/en-us/azure/bot-service/channel-connect-teams?view=azure-bot-service-4.0
[8]: https://learn.microsoft.com/en-us/microsoftteams/platform/concepts/deploy-and-publish/apps-upload
[9]: https://devblogs.microsoft.com/microsoft365dev/retirement-of-office-365-connectors-within-microsoft-teams/
[10]: https://techcommunity.microsoft.com/discussions/teamsdeveloper/simple-workflow-to-replace-teams-incoming-webhooks/4225270
