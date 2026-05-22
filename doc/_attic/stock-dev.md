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

**Founder-first principle**: before any of the source spec's §7
public-product slice, ship **P00-stock** — a private personal
pipeline that ingests the founder's actual brokerage account, computes
real performance vs QQQ, annotates the buy/sell points on charts,
and runs a basic quantitative fit against the trade history. This
is the "first user is myself" gate: the public product only makes
sense once the founder's own track record is computable +
visualizable end-to-end from real account data.

P00 has **two data-ingestion paths** to defeat the Schwab developer-app
approval lead time (the registration is known-painful):

- **Path A — CSV export (zero-wait)**. Schwab lets account holders
  download transaction + holdings history as CSV from the website.
  P00 starts here so the analytics pipeline can be built + validated
  this week, regardless of API approval status.
- **Path B — Schwab Trader API (drop-in upgrade)**. Same internal
  data model; the API just replaces the CSV importer with a live
  HTTP fetcher. Land whenever the developer app is approved.

Once P00 is running locally and the numbers / charts pass the gate,
fast-follow with the source spec's §7 *Week 1-4 slice* — public
track record + Stripe subscription + email newsletter — as a
working beta. Defer webhook / API-key / Quant tier (§7 Weeks 5-9) to
a follow-up sprint once early-paying-user feedback validates demand.

This compresses the source spec's 9-week solo plan into a sprint by:

- Reusing Polar's existing auth (no new `users` schema, no own JWT) →
  saves the Week 1 "user model + Stripe customer plumbing" half.
- Reusing Polar's existing observability stack (Prometheus + Grafana
  + 5 exporters) → saves the Week 8 monitoring half.
- Reusing existing `markdown_entries` + `posts` as the methodology /
  changelog surface (no new editor needed) → saves Week 3 frontend half.

## Phase plan

Eight phases. **P00-stock is the gating personal-dogfood phase**;
P0-stock + P1-stock are the public-product gates; everything else is
parallelizable. **MVP for the sprint = P00 + P0 + P1 + P2.** P3-P5
are the quant-tier upgrade; P6 is compliance polish.

| Phase | Scope | Est LOC | Depends on | Risk |
|---|---|---|---|---|
| **P00-stock** | **Personal pipeline: CSV-or-API ingest of Schwab account + vs-QQQ analysis + B/S annotation + basic quant fit** | **800 Go** | **—** | **high (founder must validate numbers vs Schwab statements)** |
| P0-stock | Schema (strategies, strategy_nav, signals, positions) + public track record API + chart UI | 500 Go + 400 TS | P00 acceptance gate | medium (TimescaleDB decision) |
| P1-stock | Subscriptions table + Stripe checkout + Stripe webhook + tier middleware | 600 Go + 250 TS | P0 | high (payments + tax) |
| P2-stock | Member dashboard + signal API with tier-gated `publish_at` + email digest | 400 Go + 350 TS | P0 + P1 | medium (email deliverability) |
| P3-stock | Daily strategy worker (cmd/stock-worker) + strategy interface + NAV recompute | 500 Go | P0 | medium (cron correctness, market holidays) |
| P4-stock | Webhook delivery (HMAC sign, exponential retry) + Webhook config UI | 350 Go + 200 TS | P1 + P3 | low |
| P5-stock | API-key issuance + read-only Quant-tier API + per-key rate-limit | 250 Go + 150 TS | P1 | low |
| P6-stock | Compliance: ToS / privacy / risk disclosure pages + audit_logs + GDPR export | 200 Go + 300 TS | P0 + P1 + P2 | medium (legal review gate) |

## Phase 00-stock — Personal pipeline (founder dogfood)

**Audience: one user — the founder. No public surface, no Stripe, no
workspace plumbing. Validates that the data + math + chart pipeline
produces numbers the founder trusts on his own real account.**

### Why this phase exists

The source spec assumes a strategy already has a track record to
publish. The founder's actual track record lives in Schwab, not in
Polar. Until that data flows end-to-end into the same shape P0-stock
will publish (`strategy_nav` + `signals` + `positions`), there's no
ground truth to validate against. P00-stock builds the ETL +
analytics that turns Schwab account history into the same shape, so
P0-stock is just "expose it publicly" instead of "build it from
scratch".

### Two ingestion paths (CSV first, API later)

| Path | Trigger | Source | When |
|---|---|---|---|
| A | always available | Schwab website → "Account History → Export" → CSV files for transactions + positions | start **today** — zero gating |
| B | upgrades A | Schwab Trader API via developer app | land whenever the dev-app approval clears |

Internal data model is identical for both paths. The `ingest`
subcommand has two flavors (`ingest csv <path>` / `ingest api`) that
write the same `trades` + `positions` tables. Path A unlocks the
analytics work immediately; Path B is a swap-in once approved, with
no schema churn.

### Tasks

| # | Task | File | Path |
|---|---|---|---|
| 00.1 | CSV importer: parse Schwab "Transactions" CSV export (handles current 2026 format + the 2024 pre-migration format if encountered) → normalize into `trades` rows: `(id, account, symbol, side, qty, price, commission, executed_at, kind)` | `internal/stock/ingest/csv_transactions.go` (new) | A |
| 00.2 | CSV importer: parse Schwab "Positions" CSV export → snapshot rows: `(account, symbol, qty, avg_cost, market_value, as_of_at)` | `internal/stock/ingest/csv_positions.go` (new) | A |
| 00.3 | Idempotent dedup: CSV imports keyed by `(account, symbol, executed_at, qty, price)` so re-running the same file is a no-op | both importers | A |
| 00.4 | Market data ingest from a CSV-friendly source first (Yahoo Finance unofficial endpoint via `query1.finance.yahoo.com`, or Stooq.com daily CSV) — pull EOD OHLC for each held ticker + QQQ benchmark | `internal/stock/ingest/yahoo_prices.go` (new) | A |
| 00.5 | Schwab developer app registration (manual, one-time): submit form on developer.schwab.com, callback URL via `https://zen.4950.store:2443/auth/schwab/callback`, track approval state in a checklist | external + `.env.example` | B |
| 00.6 | Schwab OAuth flow (3-legged): `polar-stock auth login` opens browser → user logs into Schwab → returns to callback → exchanges code for tokens → saves to `~/.config/polar-stock/schwab-tokens.json` (chmod 600) | `internal/stock/schwab/oauth.go` (new) | B |
| 00.7 | Refresh-token rotation: Schwab access token TTL 30 min, refresh token TTL 7 days; daemonize a tiny refresher that re-mints before expiry, fails loudly if refresh expired | same | B |
| 00.8 | API transaction pull: `GET /accounts/{acct}/transactions` paged by date range; same `trades` normalization as Path A | `internal/stock/schwab/accounts.go` (new) | B |
| 00.9 | API market-data pull: `GET /marketdata/v1/pricehistory` → same daily-OHLC schema as Path A's Yahoo fetcher | `internal/stock/schwab/marketdata.go` (new) | B |
| 00.10 | Local store: SQLite at `~/.config/polar-stock/personal.db` (NOT Postgres — keep this phase fully separable from Polar's main DB; founder may run it on a different machine than the dock) | `internal/stock/store/sqlite.go` (new) | both |
| 00.11 | NAV reconstruction: from trades + EOD prices → daily portfolio NAV, normalized to 1.0 at start of window; cost-basis FIFO (specific-lot later if FIFO diverges from Schwab statements) | `internal/stock/analysis/nav.go` (new) | both |
| 00.12 | vs-QQQ analytics: time-weighted return, alpha vs QQQ buy-and-hold over same window, beta, Sharpe (vs risk-free + vs QQQ excess), max drawdown, win rate | `internal/stock/analysis/perf.go` (new) | both |
| 00.13 | Per-ticker B/S annotation: for each ticker traded, generate a price chart with buy/sell markers + holding-period shading + cumulative-PnL ribbon | `internal/stock/charts/ticker_bs.go` (new) | both |
| 00.14 | Portfolio NAV chart vs QQQ: dual-line, monthly grid, drawdown shading | `internal/stock/charts/nav_qqq.go` (new) | both |
| 00.15 | Quant fit (basic): for each closed trade, features = `{entry_rsi_14, entry_momentum_20, sector, day_of_week, holding_days}`; target = `pnl_pct`; fit ordinary least squares + report coefficient table + R² | `internal/stock/analysis/fit.go` (new) | both |
| 00.16 | Static HTML report: `polar-stock report --since=YYYY-MM-DD --to=YYYY-MM-DD` → writes `~/Desktop/polar-stock-report-<date>.html` with all charts inline (base64 PNG) + numbers table — single self-contained file, openable offline | `cmd/stock-personal/report.go` (new) | both |
| 00.17 | CLI surface: one binary `polar-stock` with subcommands `ingest csv ...`, `ingest api`, `auth`, `accounts`, `sync`, `report`, `fit`, `serve` (optional local web preview on :7777) | `cmd/stock-personal/main.go` (new) | both |
| 00.18 | Tests: trade-replay correctness (synthetic trades → known positions), NAV math (golden-output fixtures), perf metrics (synthetic returns w/ known Sharpe), fit math (synthetic regression w/ known coefficients), CSV-parser fixtures (one fixture per Schwab export schema variant) | `*_test.go` | both |

### Day-1 critical path (no Schwab API needed)

Tasks 00.1, 00.2, 00.3, 00.4, 00.10–00.18 can all start immediately
using Schwab CSV exports + Yahoo prices. The API tasks (00.5–00.9)
land as a separate follow-up commit once the developer app is
approved. Founder's data is in the SQLite the same day — no waiting
on Schwab paperwork.

### Out of scope for P00-stock

- No multi-user, no Stripe, no auth beyond Schwab OAuth (Path B only).
- No realtime quotes — daily EOD only.
- No options / futures / crypto handling — equities + ETFs only in
  this phase. Add when the founder has options trades worth modeling.
- No Polar dock integration — the personal binary writes its own
  SQLite + HTML. Promotion to Polar tables happens in P0-stock.
- No quant fit beyond OLS. ML / non-linear fits are a follow-up after
  the founder confirms the basic linear features explain something.

### Acceptance

End-to-end smoke (must pass before P0-stock starts):

1. **Path A available immediately**: founder exports CSV from
   Schwab → `polar-stock ingest csv ~/Downloads/transactions.csv`
   succeeds → `polar-stock ingest csv ~/Downloads/positions.csv`
   succeeds → re-running both is idempotent.
2. `polar-stock report --since=2025-05-14 --to=2026-05-14` generates
   a self-contained HTML file. Opened in the browser:
   - Portfolio NAV line tracks QQQ benchmark on the same axes;
     drawdown shading aligns with the founder's memory of bad months.
   - Per-ticker charts have B/S markers placed at the correct dates +
     prices (spot-check 3 trades against the Schwab statement PDF).
   - Numbers table: time-weighted return, alpha vs QQQ, Sharpe, MDD,
     win rate, total trades — all populate, all in expected magnitudes.
3. `polar-stock fit --since=...` outputs an OLS coefficient table.
   Founder eyeballs whether any feature looks signal-bearing.
4. **Path B optional**: once dev-app approved, `polar-stock auth login`
   succeeds and `polar-stock ingest api` produces identical SQLite
   state to a fresh CSV import for the same date range (golden diff
   test).
5. The founder uses the report at least once for a real decision.
   **This is the validation gate**: if the founder doesn't trust the
   numbers or doesn't find them useful, stop and refine P00-stock
   instead of moving to P0-stock.

### Promotion path → P0-stock

After P00-stock validates:

- The SQLite schema becomes the source for P0-stock's Postgres schema
  (`trades`, `positions`, `nav` tables get migrated 1:1; `signals`
  is the same shape).
- The trade-replay + NAV recompute code moves from `internal/stock/`
  to the dock module under `internal/app/dock/strategies/` and
  becomes the `personal_v1` strategy.
- The chart components move from static-PNG to Chart.js in `ui/src/`.
- The OLS fit becomes the methodology page's "what the data says"
  section.

This is why P00-stock's data model is intentionally a superset of
what P0-stock will need: zero rework on promotion.

### Risks (this phase only)

| Risk | Mitigation |
|---|---|
| Schwab developer app approval lead time (founder confirmed: registration is painful) | **Path A is the answer** — CSV exports unblock the entire analytics pipeline. Path B is purely an ergonomics upgrade for "I don't want to download CSVs every week" |
| Schwab CSV export schema changes between releases | Version-tag the parser; CSV fixtures committed under `testdata/`; on schema drift, fail loudly + ask founder to commit a new fixture |
| Yahoo Finance unofficial endpoint may rate-limit or break | Cache prices locally by `(symbol, date)`; on failure, fall back to Stooq.com daily CSV (also unofficial but stable for years) |
| Cost-basis FIFO vs Schwab's actual specific-lot accounting | First version uses FIFO; if positions or PnL diverge from Schwab statements, add specific-lot tagging from the CSV "Lot" column (Schwab includes it on some export variants) |
| Quant fit produces meaningless results on small N | Document that <50 closed trades = anecdote, not signal; show CI bands on coefficients |

### Open questions for P00-stock

- **Quant-fit language** — pure Go (`gonum/stat` + small helper) or a
  Python sidecar (scikit-learn, statsmodels)? Recommend Go for
  zero-deps; revisit if non-linear fits become useful.
- **Report output target** — static HTML on disk (current proposal)
  or auto-host on `zen.4950.store/personal/` behind admin auth so
  the founder can view from any device? Static HTML is simpler; the
  hosted option is one extra route in dock if wanted.

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
3. **Polygon vs Schwab data source** — **decided: Schwab is the
   eventual primary source** (founder's real account lives there).
   But Schwab developer-app approval is the painful gate; P00 starts
   with the CSV-export path + Yahoo daily prices. No Polygon
   integration unless Schwab market-data quotas become a real bottleneck.
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
| "start P00-stock — CSV path only" | P00 Path-A tasks (00.1-00.4, 00.10-00.18) — analytics pipeline against CSV exports, no Schwab API |
| "P00 CSV works, add the API" | P00 Path-B tasks (00.5-00.9) as a follow-up PR once developer app approved |
| "P00 numbers check out, go P0" | P0-stock PR after the founder confirms the personal report passed §00 acceptance gate |
| "skip the worker, manual signals only" | P0 + P1 + P2 with signals admin-inserted via SQL; P3-stock deferred indefinitely |
| "ship the whole sprint slice" | P00 (Path A) first, then P0 + P1 + P2 as a single mega-PR |
| "let's prove demand first — defer everything" | This doc waits in `next-sprint` with no PR; revisit after MCP-Skill P3 lands |

## Sprint sequencing recap (against current backlog)

Current work in flight:
1. PR #184 — MCP-Skill design + dev doc (review pending)
2. PR #185 — Stock dev plan (this doc) — merged 2026-05-14
3. **Next code work — P00-stock Path A** (CSV ingest + analytics +
   charts + fit). No Schwab API dependency; can start today.
4. Parallel / async — submit the Schwab developer-app form; whenever
   it clears, land P00-stock Path B as a small follow-up PR.
5. After P00 acceptance gate passes — P0-stock + P1-stock + P2-stock
   as the public-MVP slice.
6. After ≥30 days of paying users — P3-stock through P6-stock as
   parallel branches.

**No critical-path blocker on Schwab API anymore**: P00 Path A
sidesteps the registration. The CSV-export path is good enough to
validate every analytics + chart decision in the doc.

If the user's intent is "stop adding to Polar core, focus on Stock
for a month", branch Stock onto its own subfolder (`internal/stock/`,
`cmd/stock-personal/` for P00, `cmd/stock-worker/` for P3) so it
doesn't pollute dock's main lineage.
