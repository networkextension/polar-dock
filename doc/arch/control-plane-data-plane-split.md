# Control-plane / data-plane split

Status: Phase 1 shipping (2026-06-22). Phases 2–4 designed, not started.

## Context

dock conflates two responsibilities with opposite scaling profiles in one process:

- **Control plane** — login/register + session/token verification. Tiny payloads, sub-ms,
  invoked by *every* request to *every* module (each plugin calls `/internal/v1/auth/verify`).
  Must be always-fast and always-available.
- **Data plane** — the LLM proxy. Long-lived streaming connections (seconds–minutes),
  high memory/IO, bursty.

When LLM load spikes, it starves auth verification → platform-wide login-state latency. These
two should scale (and fail) independently. This ADR is the roadmap for separating them, plus the
keystone that unblocks it.

## Current state (audited 2026-06-22)

- **Sessions are already redis-native.** `getAccessSession` reads `{prefix}:session:access:{token}`
  (24h TTL, JSON `Session`); login writes it (`session_store.go`). So "tokens/cookies in redis"
  is done; the SDK caches `/internal/v1/auth/verify` results in-process for 30s
  (`polar-sdk/client.go`).
- **The other credential types were a Postgres QueryRow per request, uncached:**
  agent tokens (`resolveAgentToken` → `agent_tokens`), llm_proxy tokens
  (`lookupLLMProxyTokenByHash`), plugin-HMAC (`getPluginModuleByName`), api_tokens
  (`resolveAPIToken`).
- **Agent-token verification was dock-only** — neither a session nor a `polar_` key, so no SDK
  method could verify it. This is exactly why agent-authenticated surfaces (e.g. the compute-task
  claim API) had to live inside dock.
- **LLM proxy own tables** (`llm_proxy_tokens` / `llm_proxy_calls` / `llm_proxy_routes`) are
  largely self-contained; the real coupling to dock is `llm_configs` (upstream key + model,
  cross-workspace shareable) and team quotas/recharges (policy).

## Keystone decision

Make token verification **shared + cacheable** rather than dock-process-bound. One change that
(a) removes the hot-path Postgres load and (b) lets *any* service verify *any* credential without
a synchronous in-dock call — the prerequisite for extracting the LLM proxy and beyond.

## Roadmap

### Phase 1 — shared verification layer (this change)

1. **Redis verify-cache** (`verify_cache.go`): a short-TTL cache in front of the per-request token
   lookups. Keys are `{prefix}:verify:{kind}:{key}` where `key` is always a **token hash**, never a
   raw secret. Positive TTL 30s; a one-byte `-` negative entry (TTL 5s) blunts brute-force scans.
   Gated by `POLAR_VERIFY_CACHE` (default **on**; kill switch `=false`). The cache is wired into
   `resolveAgentToken` first (the architecture enabler) with eviction on revoke
   (`revokeAgentToken` now `RETURNING token_hash` → `verifyCacheDel`).
2. **`GET /internal/v1/auth/verify-agent`** (HMAC-plugin-gated, sibling of `/auth/verify`):
   verifies a `polar_agent_…` token and returns `{token_id, user_id, workspace_id}`. This is what
   frees agent-authenticated surfaces from dock.
3. **SDK `AuthVerifyAgent(token)`** (`polar-sdk/client.go`): mirrors `AuthVerify` with the same
   30s in-process cache, so plugins can authenticate agents.

Revocation semantics: eviction-on-revoke makes the common case immediate; the worst case (a redis
miss-then-revoke race, or cache flag toggled) is bounded by the 30s positive TTL. `POLAR_VERIFY_CACHE=false`
disables the layer instantly if that window is ever unacceptable.

**Fast-follow (same helper, next commit):** wire the cache into `lookupLLMProxyTokenByHash`,
`getPluginModuleByName`, and `resolveAPIToken`, each with eviction on its revoke/rotate path.
Deferred from this commit only because the proxy/api revoke paths are billing/security-sensitive
and warrant their own focused review.

### Phase 2 — extract the LLM proxy

Move the proxy into `polar-llm-proxy-svc` (own process, N replicas, own box if needed). It talks
to dock only for token verification (via Phase 1) and async billing writes. `llm_configs` + team
quotas/recharges stay in dock and are reached by RPC or a redis mirror (the proxy already keeps a
60s in-process quota cache, so a short TTL is acceptable). Its own tables move with it.

### Phase 3 — stateless dock + N replicas

Run multiple dock instances behind a load balancer for control-plane HA. Sessions already live in
shared redis, so this is mostly free once any remaining in-process state (rate limiters, etc.) is
either redis-backed or accepted as per-instance.

### Phase 4 — API-key management consolidation

One issuer (dock), one registry trending to `api_tokens` + **scopes** (the open-API-token work,
P1a, already built the scope machinery). Verification served from the Phase-1 redis cache;
revocation via short TTL + a redis denylist for instant kill. Optionally graduate to dock-signed
short-lived tokens that modules verify locally against dock's public key (zero-RTT verification).

## Files (Phase 1)

- dock: `internal/app/dock/verify_cache.go` (new), `agent_store.go` (cache + evict),
  `internal_api_handlers.go` (`handleInternalAuthVerifyAgent`), `app.go` (route + flag wiring),
  `config.go` + `cmd/dock/main.go` (`POLAR_VERIFY_CACHE`), `verify_cache_test.go`.
- polar-sdk: `client.go` (`AuthVerifyAgent` + `AgentVerifyResult`), `client_authagent_test.go`.
