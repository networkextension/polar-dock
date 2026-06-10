# OAuth2 / OIDC Provider ("Login with Polar")

## Why

Dock had no way to act as an identity provider — `oauth.md` listed "be an IdP"
as an explicit non-goal, so external apps couldn't authenticate users against
Polar. This adds the missing piece: a minimal OAuth2 **authorization-code**
provider so third-party apps (first target: [Plane](https://plane.so)) can offer
"Login with Polar".

Plane's "custom OIDC" needs only an authorize URL, a token URL, and a userinfo
URL that returns `email` — i.e. plain OAuth2 + userinfo, not full signed-id_token
OIDC. So this ships that subset; a signed `id_token` + JWKS can be added later
for stricter OIDC clients without changing the flow.

## Flag

`POLAR_OAUTH_PROVIDER=true` (default off). When off, every endpoint 404s and the
feature is inert — the `oauth_clients` table is still created so the flag is a
pure runtime toggle.

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/.well-known/openid-configuration` | none | discovery |
| GET | `/oauth/authorize` | user session (cookie/Bearer) | issue auth code |
| POST | `/oauth/token` | client_secret (Basic or form) / PKCE | code → access token |
| GET | `/oauth/userinfo` | Bearer access token | `sub,email,name,picture` |
| POST | `/api/oauth/clients` | site-admin | register a relying party |
| GET | `/api/oauth/clients` | site-admin | list clients (no secrets) |

## Design

- **Clients** (`oauth_clients`, Postgres): `client_id` (`oc_…`, public),
  sha256(`client_secret`), exact-match `redirect_uris`, allowed `scopes`.
- **Authorization codes**: Redis, 5-min TTL, **single-use** (`GETDEL`). Payload
  binds client_id + user_id + redirect_uri + scope + optional PKCE challenge.
- **Access tokens**: reuse the access-session Redis records (`mintOAuthAccessToken`),
  so the same Bearer works on `/oauth/userinfo`, `/api/me`, and the whole
  `AuthMiddleware` surface — no new token type. 1-hour TTL.
- **No open redirect**: an unregistered `redirect_uri` is rejected with a 400
  rendered in place (never redirected to). Errors that *do* redirect only go to a
  validated URI.
- **PKCE** (S256/plain) supported for public clients; confidential clients use
  `client_secret`.

Code: `oauth_provider.go` (store) + `oauth_handlers.go` (HTTP). Schema in
`store.go`. Flag in `config.go` + `cmd/dock/main.go`. Routes in `app.go`.

## Wiring an external app (Plane example)

1. Operator: `POST /api/oauth/clients` with Plane's callback
   `https://<plane>/auth/oidc/callback/` → returns `client_id` + `client_secret`
   (shown once).
2. In Plane `/god-mode/authentication/oidc/`: set CLIENT_ID, CLIENT_SECRET,
   Authorize URL `https://<dock>/oauth/authorize`, Token URL
   `https://<dock>/oauth/token`, User-info URL `https://<dock>/oauth/userinfo`.
3. Plane's free Community edition gates custom OIDC behind Pro ($8/seat); on the
   free tier you fork the AGPL CE to re-enable the generic OIDC adapter. Not a
   dock concern.

## Deployment note (nginx)

The provider lives outside `/api`, so any reverse proxy fronting dock must
forward `/oauth/` and `/.well-known/openid-configuration` to the dock upstream
(they otherwise fall into a static `location /`). See `opera-dock.conf` for the
two added proxy blocks.

## Follow-ups (not in P0)

- Signed `id_token` (RS256) + `jwks_uri` for OIDC clients that require it.
- `refresh_token` grant.
- Admin UI for client management + per-client revoke (CRUD store fn exists).
- A consent screen (today an authenticated user is auto-consented).
- Login page honoring `?next=` so the browser bounce returns to `/authorize`.
