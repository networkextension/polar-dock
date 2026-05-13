# Stock Module — Development Plan

**Status**: planning. Queued for next sprint (after the MCP-Skill P0).

**Source spec**: [stock.md](./stock.md) — the architecture brief
(strategy track record + tiered subscription + signal delivery,
positioned as "research content subscription" to dodge advisory-license
risk in JP).

**Module shape**: a new top-level product surface inside Polar, sharing
existing `users` + workspace + JWT plumbing, adding its own tables +
worker binary + UI pages. Mirrors how `iosdist` slotted in.

## Sprint goal

Ship the source spec's §7 *Week 1-4 slice* — public track record + 
Stripe subscription + email newsletter — as a working beta. Defer
webhook / API-key / Quant tier (§7 Weeks 5-9) to a follow-up sprint
once early-paying-user feedback validates demand.

This compresses the source spec's 9-week solo plan into a sprint by:

- Reusing Polar's existing auth (no new `users` schema, no own JWT) →
  saves the Week 1 "user model + Stripe customer plumbing" half.
- Reusing Polar's existing observability stack (Prometheus + Grafana
  + 5 exporters) → saves the Week 8 monitoring half.
- Reusing existing `markdown_entries` + `posts` as the methodology /
  changelog surface (no new editor needed) → saves Week 3 frontend half.

## Phase plan

Seven phases. P0-stock + P1-stock are the gates; everything else is
parallelizable. **MVP for the sprint = P0 + P1 + P2.** P3-P5 are the
quant-tier upgrade; P6 is compliance polish.

| Phase | Scope | Est LOC | Depends on | Risk |
|---|---|---|---|---|
| P0-stock | Schema (strategies, strategy_nav, signals, positions) + public track record API + chart UI | 500 Go + 400 TS | — | medium (TimescaleDB decision) |
| P1-stock | Subscriptions table + Stripe checkout + Stripe webhook + tier middleware | 600 Go + 250 TS | P0 | high (payments + tax) |
| P2-stock | Member dashboard + signal API with tier-gated `publish_at` + email digest | 400 Go + 350 TS | P0 + P1 | medium (email deliverability) |
| P3-stock | Daily strategy worker (cmd/stock-worker) + strategy interface + NAV recompute | 500 Go | P0 | medium (cron correctness, market holidays) |
| P4-stock | Webhook delivery (HMAC sign, exponential retry) + Webhook config UI | 350 Go + 200 TS | P1 + P3 | low |
| P5-stock | API-key issuance + read-only Quant-tier API + per-key rate-limit | 250 Go + 150 TS | P1 | low |
| P6-stock | Compliance: ToS / privacy / risk disclosure pages + audit_logs + GDPR export | 200 Go + 300 TS | P0 + P1 + P2 | medium (legal review gate) |

## Phase 0-stock — Public track record (foundation)

### Tasks

| # | Task | File |
|---|---|---|
| 0.1 | Schema bootstrap: `strategies`, `strategy_nav`, `signals`, `positions` tables with FK ordering + idempotent guards (follow PR #182 pattern) | `internal/app/dock/store.go::applySchema` |
| 0.2 | `strategies_store.go` — CRUD + `ListActive` + `GetByCode` | `internal/app/dock/strategies_store.go` (new) |
| 0.3 | `signals_store.go` — `Create`, `ListVisible(strategyID, beforePublishAt, limit)`, `UpdateExit` | `internal/app/dock/signals_store.go` (new) |
| 0.4 | `nav_store.go` — `Upsert`, `Series(strategyID, from, to)` | `internal/app/dock/nav_store.go` (new) |
| 0.5 | Public handlers (no auth required) | `internal/app/dock/strategy_handlers.go` (new) |
| 0.6 | Routes: `GET /api/public/strategies`, `/api/public/strategies/:code/nav`, `/metrics`, `/methodology`, `/signals?older_than=7d` | `internal/app/dock/app.go` |
| 0.7 | Risk metrics helper: Sharpe, MDD, annualized return, win rate — computed on demand (small dataset, no precompute yet) | `internal/app/dock/strategy_metrics.go` (new) |
| 0.8 | UI page: `/strategies.html` (list) + `/strategies/:code.html` (detail w/ NAV chart, monthly heatmap, recent-signals list) | `ui/public/strategies*.html` + `ui/src/strategies.ts` (new) |
| 0.9 | NAV chart component using Chart.js (already vendored in `ui/`) — line chart for strategy vs SPY benchmark | `ui/src/components/nav-chart.ts` (new) |
| 0.10 | Methodology page reads from existing `markdown_entries` by slug `methodology-<strategy_code>` — no new editor | `ui/public/methodology.html` (new) |
| 0.11 | Sidebar nav entry: 策略 (between 主机 and Editor) | the 18 sibling `.html` pages |
| 0.12 | Seed data CLI: `cmd/dock/seed-strategy --code=momentum_v1 --csv=track.csv` | `cmd/dock/seed_strategy.go` (new) |
| 0.13 | Tests: store CRUD, metrics math, public API response shape | `*_test.go` |

### Out of scope for P0-stock

- No auth changes — public pages only; member-gated content lands in P2.
- No Stripe — pricing page is static placeholder until P1.
- TimescaleDB **deferred**: the source spec recommends it for
  `strategy_nav`, but for ≤ 5 strategies × 10y daily = 18k rows per
  strategy, plain Postgres is fine. Revisit at 100+ strategies or
  intraday NAV. See open question §1.

### Acceptance

- `dropdb && createdb && go run ./cmd/dock` clean.
- Seed one strategy with a 12-month NAV history → `/strategies/momentum_v1.html`
  renders chart + Sharpe / MDD / annualized / win rate cards.
- Recent signals list shows the 10 most recent signals with rationale
  text. No login required.
- All public endpoints respond on the deploy box.

## Phase 1-stock — Stripe subscription + tier middleware

### Tasks

| # | Task | File |
|---|---|---|
| 1.1 | Schema: `subscriptions` table (FK → users), `audit_logs` table; FK guards if needed | `store.go::applySchema` |
| 1.2 | Users table extension: `stripe_customer_id TEXT` (nullable) | `store.go` ALTER block |
| 1.3 | Stripe client wrapper (one file, no separate package — keep deps shallow) | `internal/app/dock/billing.go` (new) |
| 1.4 | `POST /api/billing/checkout` — body: `{tier}`, returns Stripe checkout URL with trial-period-days=14 | `internal/app/dock/billing_handlers.go` (new) |
| 1.5 | `POST /api/billing/portal` — returns Stripe customer portal URL (manage / cancel) | same |
| 1.6 | `POST /stripe/webhook` — verify signature, handle `customer.subscription.{created,updated,deleted}` + `invoice.payment_failed` | same |
| 1.7 | `tier_gate.go` middleware — reads user's active subscription, gates route by min-tier | `internal/app/dock/tier_gate.go` (new) |
| 1.8 | Pricing page UI: 3-tier card layout + FAQ + 14-day trial badge + JP-friendly tax line | `ui/public/pricing.html` (new) + `ui/src/pricing.ts` (new) |
| 1.9 | User dashboard surface: subscription status card + "Manage Billing" button → portal URL | `ui/src/dashboard.ts` (extend) |
| 1.10 | Stripe price IDs in `.env`: `STRIPE_PRICE_PRO`, `STRIPE_PRICE_QUANT`; webhook secret too | `.env.example` |
| 1.11 | Tests: webhook signature verification, tier_gate denies on inactive sub, subscription sync after webhook | `*_test.go` |

### Out of scope for P1-stock

- Tax: Stripe Tax auto-collects JP consumption tax 10%. Manual tax
  config UI is out — let Stripe handle it from the dashboard.
- Yearly billing tiers — start with monthly only, add yearly when a
  user actually asks.

### Acceptance

- A test Stripe key in `.env`, click "Subscribe → Pro" on `/pricing`,
  complete checkout → user lands back on dashboard, subscription card
  reads "Pro · trial ends in 14 days".
- Webhook signature check rejects forged payloads (returns 400).
- A user on `tier=free` calling a route gated by `RequireTier("pro")`
  gets 403 `{"error":"upgrade_required","min_tier":"pro"}`.
- Cancel from portal → next request sees status `canceled`, tier_gate
  treats them as `free` after `current_period_end`.

## Phase 2-stock — Member dashboard + tier-gated signals + email digest

### Tasks

| # | Task | File |
|---|---|---|
| 2.1 | `GET /api/member/signals?strategy_id=` — applies `publish_at` filter by tier (free: -7d, pro: -1h, quant: now); writes `audit_logs` | `signal_handlers.go` (new) |
| 2.2 | `GET /api/member/positions/current?strategy_id=` — current open positions, pro+ only | same |
| 2.3 | Email queue: simple worker that batches new-signal events per subscriber (digest once per day for free, real-time-ish for pro) | `cmd/stock-worker/email.go` (lands as part of P3 worker) |
| 2.4 | Email template (HTML + plaintext) — strategy name, signal action, rationale excerpt, link to dashboard | `internal/app/dock/email_templates/signal_digest.html` (new) |
| 2.5 | SendGrid (or Resend; pick one) integration — minimal wrapper around their HTTP API, retry on 429/5xx | `internal/app/dock/email.go` (new) |
| 2.6 | Unsubscribe link with HMAC token → `GET /api/member/unsubscribe?token=` flips `users.email_signals_opt_out` | same |
| 2.7 | Member dashboard: signal stream component (filter by strategy, sort by date) | `ui/src/dashboard-signals.ts` (new) |
| 2.8 | Strategy detail page upgrade: signed-in pro users see realtime signals; free still sees -7d delay | `ui/src/strategies.ts` (extend) |
| 2.9 | Tests: tier-gated visibility (free vs pro vs quant); audit log written on every view; unsubscribe HMAC verification | `*_test.go` |

### Acceptance

- Logged-in free user opens `/strategies/momentum_v1.html` → sees
  signals older than 7 days, "Upgrade to Pro for real-time" CTA on
  newer ones.
- Logged-in pro user sees signals up to 1h delay; quant up to now.
- Daily 16:30 ET digest email goes to subscribed users containing
  the day's new signals (or "No new signals today" silent skip).

## Phase 3-stock — Daily worker + strategy interface

### Tasks

| # | Task | File |
|---|---|---|
| 3.1 | `cmd/stock-worker/main.go` — robfig/cron driver, 16:30 ET schedule, market-holiday-aware (NYSE calendar) | `cmd/stock-worker/main.go` (new) |
| 3.2 | `Strategy` interface — `Code()`, `Run(ctx, asOf time.Time) ([]Signal, error)`, `Methodology() string` | `internal/strategies/strategy.go` (new package) |
| 3.3 | First strategy: `momentum_v1` — reads price history from `prices` table (P3.7), generates buy/sell signals based on configurable rules | `internal/strategies/momentum_v1/momentum.go` (new) |
| 3.4 | Strategy registry — `init()` registers each strategy, mirrors `cmd/polar-agent/skills` pattern | `internal/strategies/registry.go` (new) |
| 3.5 | NAV recompute: take previous NAV + today's signals + positions → new NAV row | `internal/strategies/nav.go` (new) |
| 3.6 | Price data ingestion: pluggable source interface; first impl = Polygon free tier (delayed EOD prices, no realtime needed for daily strategies) | `internal/strategies/prices/polygon.go` (new) |
| 3.7 | `prices` table — `(symbol, date, open, high, low, close, volume)` | `store.go::applySchema` |
| 3.8 | Backfill CLI: `cmd/stock-worker --backfill=momentum_v1 --from=2024-01-01 --to=2026-05-01` | `cmd/stock-worker/backfill.go` (new) |
| 3.9 | Failure handling: per-strategy error → alert via `alerting.Notify`, skip strategy, continue others (don't fail-stop the cron) | `cmd/stock-worker/main.go` |
| 3.10 | Tests: cron schedule fires on a business day, skips on a holiday; strategy run idempotent (re-running same `asOf` is safe) | `*_test.go` |

### Acceptance

- Worker started on the deploy box via launchd plist, runs at 16:30 ET
  on NYSE trading days, writes new signals + NAV rows.
- Backfilling 1 year of `momentum_v1` from CLI populates a clean track
  record visible on the public page.
- Manual trigger (`cmd/stock-worker --run-now`) succeeds.

## Phase 4-stock — Webhook delivery (Quant tier)

### Tasks

| # | Task | File |
|---|---|---|
| 4.1 | Schema: `webhooks` table with FK to users, plus `webhook_deliveries` for the retry-state ledger | `store.go::applySchema` |
| 4.2 | `POST /api/member/webhooks` (Quant tier only) — body: `{url, events[]}`, returns secret one-time | `webhook_handlers.go` (new) |
| 4.3 | `DELETE /api/member/webhooks/:id` + `GET /api/member/webhooks` | same |
| 4.4 | Webhook dispatcher: when signal_handler creates a signal that any user with a quant tier + matching event subscription expects, fan-out a HTTP POST with HMAC-SHA256 sig in `X-Signature` header | `internal/app/dock/webhook_dispatcher.go` (new) |
| 4.5 | Retry with exponential backoff: 0 / 5 / 30 / 300 / 1800 sec attempts; on each failure increment `failure_count`, on success reset | same |
| 4.6 | Auto-disable webhook after 10 consecutive failures; email the user | same |
| 4.7 | Test-webhook button in UI: synthesizes a fake `signal.test` event → user can verify their endpoint is reachable | `ui/src/dashboard.ts` (extend) |
| 4.8 | Tests: HMAC sig verification matches stripe-style format; retry schedule respected; failure increment + auto-disable | `*_test.go` |

### Acceptance

- Quant user adds a webhook URL → test fires, returns 200 → sees green
  status on dashboard.
- Next live signal fans out within 5s of being written.
- Endpoint that returns 500 → 4 retries with growing delay → 5th
  success clears the failure count.

## Phase 5-stock — API key + read-only Quant API

### Tasks

| # | Task | File |
|---|---|---|
| 5.1 | Schema: `api_keys` table — `(id, user_id, name, key_hash, scope, rate_limit_per_min, last_used_at, created_at, revoked_at)` | `store.go::applySchema` |
| 5.2 | `POST /api/member/api-keys` (Quant tier) — generates one-time raw key (returned once, then hash-stored) | `apikey_handlers.go` (new) |
| 5.3 | Bearer auth middleware variant — checks `Authorization: Bearer pk_…` against `api_keys.key_hash`, sets user + scope | `internal/app/dock/apikey_middleware.go` (new) |
| 5.4 | Read-only `/api/v1/strategies/:code/signals` + `/strategies/:code/nav` + `/strategies/:code/positions` — same data as member endpoints but key-authed | `apikey_handlers.go` |
| 5.5 | Per-key rate limit (token bucket) — store buckets in Redis (already deployed) | `internal/app/dock/apikey_ratelimit.go` (new) |
| 5.6 | UI: API-key list, rotate, revoke; one-time raw display on create | `ui/src/dashboard-apikeys.ts` (new) |
| 5.7 | Tests: key auth happy + revoked path; rate-limit enforcement returns 429 with `Retry-After` | `*_test.go` |

### Acceptance

- Quant user clicks "Generate API key" → copies `pk_xxxx` → uses it
  with curl to fetch their strategies' signal history.
- 601st request in a minute on a 600 rpm key returns 429.

## Phase 6-stock — Compliance polish

### Tasks

| # | Task | File |
|---|---|---|
| 6.1 | Static pages: `/terms.html`, `/privacy.html`, `/risk-disclosure.html` — render from `markdown_entries` for easy editing | new HTML stubs |
| 6.2 | Footer disclaimer auto-inserted on every signal-bearing page: "For research purposes only, not investment advice." | `ui/src/lib/footer.ts` (new) |
| 6.3 | Audit log retention: prune `audit_logs` older than 2y via worker; archive to S3 monthly | `cmd/stock-worker/audit_prune.go` (new) |
| 6.4 | GDPR endpoints: `POST /api/member/data-export` (zip + email link), `POST /api/member/account-delete` (soft-delete user + revoke subs + drop webhook config) | `compliance_handlers.go` (new) |
| 6.5 | Cookie banner / consent toggle (minimal — JP / EU enough) | `ui/src/lib/cookie-banner.ts` (new) |
| 6.6 | Sentry-equivalent error capture wired to existing Prometheus alerts (no new vendor — alert on `polar_error_rate` spike) | `internal/app/dock/error_middleware.go` (extend) |
| 6.7 | Tests: data-export zip integrity; audit-log prune leaves recent entries intact | `*_test.go` |

### Acceptance

- A user can export their data + delete their account from the
  dashboard, both flows complete within 5 min.
- Every page that shows a signal has the disclaimer in the footer.

## Cross-phase concerns

### Schema

P0-stock through P6-stock add the following tables — all forward-compatible
with PR #182's FK-ordering pattern; any cross-table FKs that resolve
later in `applySchema` use the late-bound DO block guard:

- `strategies`, `strategy_nav`, `signals`, `positions`, `prices`
- `subscriptions`, `audit_logs`
- `webhooks`, `webhook_deliveries`
- `api_keys`

ALTER on existing `users` for `stripe_customer_id` is non-breaking
(nullable column).

### Workspace scoping

Strategies are **workspace-scoped** (`strategies.workspace_id` FK to
teams). A workspace = one strategy owner / publisher. Subscriptions
are **per-user** (a user subscribes to view a workspace's strategies).
This mirrors the existing iosdist module's pattern.

For the MVP single-strategy-owner deployment, the deploy box's
`local` user's personal workspace is the publisher; subscribers join
no workspace (they just have a user account + a subscription record).

### Compliance + legal (gate before P1 ships)

The source spec §6.1 is non-negotiable. Before P1-stock ships and
real money flows:

- [ ] Consult a JP lawyer on the "research content subscription"
      positioning — confirm it stays outside 投資助言業 license
      requirements
- [ ] Get ToS / privacy / risk disclosure drafts reviewed
- [ ] Confirm Stripe Tax setup for JP consumption tax (10%)
- [ ] Decide on EU exposure — if blocking EU, geoblock at edge

This gate is in P1-stock's PR description but NOT in the code
acceptance — it's an operator-confirmable checkbox the PR reviewer
must explicitly approve before merging.

### Security

- Passwords reuse existing `users.password_hash` (bcrypt, already
  hardened in Polar).
- Stripe customer ID is not secret per Stripe's docs, but never log
  it.
- API key raw value shown **exactly once** on creation; DB stores
  bcrypt hash only.
- Webhook secret returned exactly once; DB stores bcrypt hash.
- `audit_logs` retains IP + user_id + action for 2y → compliance
  safety net. Don't expose to non-admin endpoints.

### Risk register

| Risk | Phase | Mitigation |
|---|---|---|
| Stripe webhook delivery flaky → subscription state drifts | P1 | Daily reconciler job: pull all active subs from Stripe API, diff against local table, repair |
| Email deliverability poor in JP | P2 | SendGrid w/ SPF + DKIM on a sending domain; pilot to friends first |
| Strategy run hangs (Polygon API timeout) | P3 | Per-strategy timeout 5 min; alert + skip on exceed |
| Market holiday calendar drift | P3 | Use NYSE calendar package, verify each Jan against next year's holiday list |
| Webhook receiver eats secret in error log | P4 | Document HMAC pattern + never include secret in any payload; only signature |
| Source spec §13 reminder: build all features → users want something else | all | Hold P3-P5 until P0-P2 has 5+ paying users for 30+ days |

## Open questions

1. **TimescaleDB now or later?** Source spec recommends; plain
   Postgres is enough for MVP. Defer to P3 when intraday data shows
   up, or to a later phase if we ever query NAV across thousands of
   strategies. **Recommend: defer.**
2. **Pricing tiers** — exact prices not set. Source spec implies
   free / pro / quant. Suggest pro = $29/mo, quant = $99/mo, both
   yearly options at -20% — confirm with user before P1 PR opens.
3. **Polygon vs Schwab data source** — source spec mentions both.
   Polygon is simpler to integrate (HTTP/JSON, free tier). Schwab
   requires OAuth + brokerage account; production-grade but heavy.
   **Recommend: Polygon for P3, Schwab as P3.5 swap if order
   execution lands (not in scope).**
4. **Single strategy vs many at launch** — sprint MVP can ship with
   exactly one strategy populated (`momentum_v1`). Confirm the
   strategy logic + a 1-year backfill before P0 PR opens.
5. **Methodology storage** — proposed: existing `markdown_entries`
   with slug convention. Alternative: dedicated `strategy_methodology`
   column on `strategies`. The markdown_entries route is freer (HTML,
   images, versioning); the column route is simpler. **Recommend:
   markdown_entries.**

## When to merge what

| If user says... | Land |
|---|---|
| "design looks right, queue it" | This dev doc as a single PR (no code), tagged `next-sprint` |
| "do P0-stock" | The P0-stock PR per §0 above |
| "skip the worker, manual signals only" | P0 + P1 + P2 with signals admin-inserted via SQL; P3-stock deferred indefinitely |
| "ship the whole spring slice" | P0 + P1 + P2 as a single mega-PR (heavy review, but smaller surface area than 3 round trips) |
| "let's prove demand first — defer everything" | This doc waits in `next-sprint` with no PR; revisit after MCP-Skill P3 lands |

## Sprint sequencing recap (against current backlog)

Current work in flight:
1. PR #184 — MCP-Skill design + dev doc (review pending)
2. Sprint after that — **THIS doc's P0-stock + P1-stock + P2-stock**
3. After validation — P3-stock through P6-stock as parallel branches

If the user's intent is "stop adding to Polar core, focus on Stock
for a month", we can branch Stock onto its own subfolder (`internal/stock/`,
`cmd/stock-worker/`) so it doesn't pollute dock's main lineage.
