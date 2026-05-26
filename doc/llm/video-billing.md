# Video billing — per-shot + per-frame ledger

> **Status:** design, not implemented.
> **Owner:** polar-dock + polar-video.
> **Last touched:** 2026-05-26.

## Problem

Video projects today track *whether* a shot generated successfully
(`video_shots.status`) but record nothing about how much that
generation cost. Operators have no way to:

- attribute a shot's USD cost to the workspace that initiated it,
- aggregate "this project burned $X" across many shots,
- compare providers (Seedance vs Kling vs Runway) at the workspace
  level,
- bill or rate-limit video usage the way text-LLM usage is already
  metered.

The text-LLM side has this solved: every chat / extract / summary
call lands in `dock.llm_proxy_calls` with `workspace_id`,
`prompt_tokens`, `completion_tokens`, `cost_usd`. We want the same
shape for video generation — adapted to its native billing unit.

## Why "per frame" isn't the natural unit

Video models (Seedance, Kling, Runway) don't bill by token count.
They bill by `duration × resolution × audio`. The API doesn't even
return a token field — the underlying inference isn't a discrete
text generation, it's a diffusion / flow-matching pass that has no
token concept.

So the natural billing unit is **per shot** (one generate-task =
one `video_shots` row). Per-frame cost is a *derived* number:

```
cost_per_frame_usd = shot.cost_usd / (shot.duration_sec × shot.fps)
```

Frames-per-second varies by provider (Seedance defaults to 24,
Kling 30), so we store both `frames_total` and `cost_per_frame_usd`
in the DB rather than recomputing in every query.

## Data model

### polar_video side — extend `video_shots`

```sql
ALTER TABLE video_shots
  ADD COLUMN IF NOT EXISTS provider              TEXT,           -- "seedance" / "kling" / ...
  ADD COLUMN IF NOT EXISTS model                 TEXT,           -- provider model id
  ADD COLUMN IF NOT EXISTS resolution            TEXT,           -- "720p" / "1080p" / "4k"
  ADD COLUMN IF NOT EXISTS duration_charged_sec  NUMERIC(6,3),   -- what provider actually charged
  ADD COLUMN IF NOT EXISTS fps                   INT,            -- provider-reported frame rate
  ADD COLUMN IF NOT EXISTS frames_total          INT,            -- duration_charged_sec × fps
  ADD COLUMN IF NOT EXISTS cost_usd              NUMERIC(12,6),  -- shot total
  ADD COLUMN IF NOT EXISTS cost_per_frame_usd    NUMERIC(14,9),  -- pre-computed amortization
  ADD COLUMN IF NOT EXISTS billing_meta          JSONB,          -- raw pricing-table snapshot + formula
  ADD COLUMN IF NOT EXISTS billed_at             TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_video_shots_billed_at
  ON video_shots(billed_at DESC) WHERE billed_at IS NOT NULL;
```

Why store `cost_per_frame_usd` instead of computing on the fly:

- analytics queries (`SUM(cost_per_frame_usd × frames_in_range)`)
  stay simple SQL.
- the `cost_usd / frames_total` math involves provider-specific FPS
  and rounding rules — pinning the answer at write time means an
  fps-table change doesn't retroactively rewrite history.
- enables per-frame indexes for "top 100 most-expensive frames"
  style queries without computed columns.

### dock side — new `video_shot_calls`

Mirrors `llm_proxy_calls` so the existing workspace-wallet plumbing
(`llm_billing_store.go::ensureLLMConfigTeamQuota` etc.) can consume
both ledgers uniformly.

```sql
CREATE TABLE IF NOT EXISTS video_shot_calls (
    id BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    project_id   BIGINT NOT NULL,
    shot_id      BIGINT NOT NULL,
    provider     TEXT NOT NULL,
    model        TEXT,
    resolution   TEXT,
    duration_charged_sec NUMERIC(6,3),
    fps                  INT,
    frames_total         INT,
    cost_usd             NUMERIC(12,6),
    cost_per_frame_usd   NUMERIC(14,9),
    billing_meta         JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_video_shot_calls_workspace_created
  ON video_shot_calls(workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_video_shot_calls_project
  ON video_shot_calls(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_video_shot_calls_provider
  ON video_shot_calls(provider, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_video_shot_calls_shot
  ON video_shot_calls(shot_id);  -- idempotent: re-poll won't double-bill
```

The `UNIQUE(shot_id)` is the key correctness gate. The poll worker
re-fires on every tick; we use `INSERT ... ON CONFLICT (shot_id) DO
NOTHING` so a flaky retry can never bill twice.

## Flow

```
                     polar-video                                 dock
                     ──────────                                  ────
poll worker (every N sec):
  pollVideoTask(shot.task_id)
  if status == succeeded:
    cost, fps, frames, meta = priceShot(provider, model, ratio, duration, audio)
    UPDATE video_shots SET
      provider, model, resolution, duration_charged_sec, fps,
      frames_total, cost_usd, cost_per_frame_usd, billing_meta,
      billed_at = NOW()
    WHERE id = shot.id

    POST /internal/v1/billing/video-shots  ──── HMAC plugin-token ────►
      { workspace_id, project_id, shot_id, provider, model,
        resolution, duration_charged_sec, fps, frames_total,
        cost_usd, cost_per_frame_usd, billing_meta }
                                                                        INSERT INTO video_shot_calls
                                                                          ON CONFLICT (shot_id) DO NOTHING
                                                                        (best-effort: ack 200 even on dup)
```

Failure semantics:

- pricing helper returns `(0, nil)` if the provider isn't in the
  price table → video_shots gets billed_at but cost_usd = 0. The
  /internal/v1 call still fires, dock records cost=0. Operator can
  see "$0 shots, missing price" in audit.
- /internal/v1 POST fails → video_shots is already updated. The
  poll worker is idempotent on the polar-video side (we look at
  `billed_at` before re-posting), so a retry next tick recovers.
  No double-billing because of the UNIQUE(shot_id) gate on dock.

## Pricing table

`pricing_seedance.go` in polar-video:

```go
// Seedance pricing as of 2026-05. Source: <vendor doc URL>.
// duration billing rounds up to whole seconds.
var seedancePricing = map[seedanceVariant]float64{
    {model: "seedance-pro-v1", resolution: "720p",  audio: false}: 0.020, // $/sec
    {model: "seedance-pro-v1", resolution: "720p",  audio: true}:  0.025,
    {model: "seedance-pro-v1", resolution: "1080p", audio: false}: 0.040,
    {model: "seedance-pro-v1", resolution: "1080p", audio: true}:  0.050,
}

func priceSeedanceShot(model, resolution string, durationSec int, audio bool) (cost float64, fps int, meta map[string]any) {
    rate, ok := seedancePricing[seedanceVariant{model, resolution, audio}]
    if !ok {
        return 0, 24, map[string]any{"reason": "no_price"}
    }
    cost = rate * float64(durationSec)
    fps = 24
    meta = map[string]any{
        "rate_usd_per_sec": rate,
        "duration_sec":     durationSec,
        "audio":            audio,
    }
    return
}
```

When we add Kling / Runway, register their own pricing functions and
dispatch on `provider`. Keep the meta JSONB rich so a future audit
can reconstruct exactly which price-table version applied.

## Analytics queries

These are why `cost_per_frame_usd` lives in the row.

**Per-workspace monthly burn:**
```sql
SELECT workspace_id,
       SUM(cost_usd) AS month_usd,
       SUM(frames_total) AS month_frames,
       SUM(cost_usd) / NULLIF(SUM(frames_total), 0) AS avg_per_frame
FROM video_shot_calls
WHERE created_at >= date_trunc('month', NOW())
GROUP BY workspace_id;
```

**Per-project cost breakdown:**
```sql
SELECT project_id,
       provider, model, resolution,
       COUNT(*) AS shots,
       SUM(duration_charged_sec) AS total_sec,
       SUM(frames_total) AS total_frames,
       SUM(cost_usd) AS total_usd
FROM video_shot_calls
WHERE project_id = $1
GROUP BY project_id, provider, model, resolution;
```

**Top-100 most expensive single frames** (sanity check / quota
alerts):
```sql
SELECT shot_id, cost_per_frame_usd, provider, resolution
FROM video_shot_calls
ORDER BY cost_per_frame_usd DESC NULLS LAST
LIMIT 100;
```

**Provider mix over time** (decide whether to switch defaults):
```sql
SELECT date_trunc('day', created_at) AS d,
       provider,
       SUM(cost_usd) AS d_usd,
       SUM(frames_total) AS d_frames
FROM video_shot_calls
WHERE created_at >= NOW() - INTERVAL '90 days'
GROUP BY d, provider
ORDER BY d, provider;
```

## Workspace wallet integration

Reuse the existing `llm_config_team_quotas.credit_balance` path:

- video and text-LLM share one wallet per workspace.
- Before submitting a shot, polar-video calls a new dock helper
  `/internal/v1/billing/video-quota-check?workspace_id=…&estimated_usd=…`
  → dock returns `{allow, reason, remaining_usd}`.
- On reject, polar-video aborts the submit and surfaces the same
  402 error envelope as the chat path.
- After billing, dock's existing recharge / monthly-burn dashboards
  pick up `video_shot_calls` rows automatically (joined by
  `workspace_id`).

The quota check is **not** in the P0 scope below — it's a phase 2
add-on once the ledger has data to enforce against.

## Phases

| Phase | Scope | Touches |
|---|---|---|
| **P0** | polar-video schema migration + pricing table + poll-worker write to video_shots | polar-video only |
| **P1** | dock `video_shot_calls` schema + `POST /internal/v1/billing/video-shots` + idempotent insert | dock only |
| **P2** | polar-video wire the /internal/v1 POST after every poll-success | polar-video |
| **P3** | dock list/aggregate endpoints (`GET /api/video-shot-calls?project_id=…`, `GET /api/admin/video-burn?since=…`) | dock |
| **P4** | UI surface — project page total bar + per-shot cost chip (separate PR, out of this initiative) | polar-dock-ui |
| **P5** | quota gate — `POST /internal/v1/billing/video-quota-check` + polar-video pre-submit call | both |

P0+P1+P2+P3 = API work. P4 = UI (deferred). P5 = enforcement (deferred).

## Out of scope

- Subscription / pre-paid bundles for video credits — handled by the
  existing `llm_billing_team_recharge` flow, no schema change here.
- Per-frame *inference* attribution — video models don't decompose
  by frame, only by shot. The amortized number is for accounting,
  not for back-pressure on individual frames.
- Provider-side webhook ingestion (e.g. Seedance pushing usage to
  us) — current Seedance API is poll-only; switch later if they
  ship webhooks.
- Real-time burn-rate alerts — once `video_shot_calls` has rows,
  the existing Prometheus exporter on dock can scrape SUM(cost_usd)
  in a follow-up.

## Open questions

1. **fps source of truth.** Seedance API doesn't return fps in the
   task-result envelope. We hardcode 24 for now. If a future
   provider returns a different fps per-shot, store it in
   `billing_meta` and update `fps` from there.
2. **rounding of duration.** Provider may bill ceil(duration_sec)
   or actual delivered duration. Encode in `priceXxxShot` per
   provider; the unit test asserts the rounding rule.
3. **historical backfill.** Existing succeeded shots have no
   billed_at — leave them; the analytics queries `WHERE
   billed_at IS NOT NULL` skip them naturally. If finance wants
   retroactive numbers, write a one-off migration.
