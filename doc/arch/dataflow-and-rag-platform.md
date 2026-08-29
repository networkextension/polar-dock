# Dataflow + RAG platform for the commercial verticals

Status: design (2026-06-03). Covers where the `research/dataflow` framework lives, how
the three verticals (buildings / lawyer / opera) consume it, and how pgvector/RAG fits.

## TL;DR / decisions

1. **Dataflow does NOT go in `polar-sdk`.** Promote `research/dataflow` to a standalone
   library repo **`polar-dataflow`** that each vertical imports. `polar-sdk` stays the thin
   *control-plane* client (auth, heartbeat, assets, llm-proxy, register); `polar-dataflow`
   is the *data-plane* engine that sits **above** the SDK and bridges to dock through it.
2. **RAG = pgvector inside each vertical's own Postgres** (`polar_lawyer` / `polar_buildings`
   / `polar_opera`), with a shared `polar-dataflow/rag` package owning the canonical schema +
   upsert/search ops. Data stays per-tenant, per-vertical, on-prem (air-gap / 不出域).
3. **Embedding is a pluggable port.** Default routes through dock's LLM proxy to an on-prem
   embedding `llm_config` (reuses P1 billing/audit, model never leaves the box); air-gapped
   sites swap in a direct local-model impl.

## Why not polar-sdk

`polar-sdk` is the minimal HTTP client every module imports to talk to **dock**: `AuthVerifyWS`,
`Heartbeat`, `Asset{Upload,Download,…}`, `LLMConfigGet`, `AgentRegister`, `IssueAgentToken`,
`EnsureWorkspaceAgentPoolProxyToken`. It must stay small and stable — its release cadence and
dependency surface are felt by all ~12 modules. The dataflow framework is a heavy, opinionated
engine (pipeline orchestration, an expr rules VM, EventBus/Store/RAG ports, vector math). Folding
it into the SDK would bloat every non-dataflow module, couple unrelated release cycles, and bury a
big subsystem inside a "client". They are different layers:

| | `polar-sdk` | `polar-dataflow` |
|---|---|---|
| Role | module ↔ **dock** (control plane) | module's internal **data plane** engine |
| Shape | thin HTTP client | framework + ports + adapters |
| Imported by | all modules | only the data/observability verticals |
| Depends on | net/http | expr-lang, pgx, (adapters →) polar-sdk |

`polar-dataflow` *uses* `polar-sdk` (its adapters call dock through it) — it sits on top, it does
not replace it.

## Repo layout — `github.com/networkextension/polar-dataflow`

Promote `research/dataflow` (currently `module dataflow`) as-is, then add `rag/` + `adapters/`:

```
polar-dataflow/
├── core/        envelope + 4 plugin interfaces + infra ports   ← from research, unchanged
├── pipeline/    5-stage orchestrator (deferral, retry, transfer.complete) ← unchanged
├── rules/       config-driven expr engine (JSON rules)         ← unchanged
├── rag/         NEW — canonical RAG contracts + pgvector
│   ├── schema.sql   rag_chunks(... embedding vector(N) ...) + HNSW index
│   ├── store.go     RAGStore: Upsert(chunks,vecs,meta) / Search(kb,vec,k,filter) / Delete
│   ├── pgvector.go  pgx-backed RAGStore (per-vertical DB)
│   └── embed.go     Embedder port: Embed([]string) ([][]float32, error)
└── adapters/    NEW — concrete impls of the core ports, shared by all verticals
    ├── store_pg.go    core.Store     → Postgres (status/audit) [+ Timescale for metrics]
    ├── bus_nats.go     core.EventBus  → NATS  (transfer.complete, service up/down)
    ├── alert_dock.go   core.AlertSink → dock (work-order / notification) via polar-sdk
    ├── embed_proxy.go  rag.Embedder   → dock llm-proxy embed route via polar-sdk
    └── log_slog.go     core.Logger    → slog
```

The **public "dataflow API" modules consume** is just this library's surface: `core.*` types,
`core.Registry.Register*`, `pipeline.New(...).Run(env)`, `rules.Load/NewEvaluator`, and
`rag.RAGStore`/`rag.Embedder`. No new dock endpoint, no SDK change for the core path.

## How a vertical module uses it

Each vertical is a normal module-service (`cmd/<svc>-svc` + `internal/<svc>`, own `polar_<v>` DB,
UI on `<v>.4950.store`). It writes **only plugins + rules**; the framework gives it state machine,
tracing, retry, deferral, audit for free:

```go
reg := core.NewRegistry()
reg.RegisterClassifier(myClassifier{})
reg.RegisterProcessor(myProcessor{Embed: embedder.Embed})   // doc verticals
reg.RegisterEvaluator(rules.NewEvaluator(engine))           // engine = rules.Load("rules.json")
reg.RegisterDispatcher(adapters.AlertDock{Dock: sdkClient}) // → work order
reg.RegisterDispatcher(rag.Writer{Store: pgStore})          // → RAG

p := pipeline.New(reg, pipeline.DefaultConfig(),
    adapters.StorePG{DB: db}, adapters.AlertDock{Dock: sdk},
    adapters.BusNATS{...}, adapters.LogSlog{})

// ingest endpoint / MQTT consumer / file-intake just builds an envelope and calls:
p.Run(ctx, core.NewEnvelope(traceID, tenantID, source, payload))
```

The module's REST/MQTT/file-intake adapters normalize input → `DataEnvelope`. Large objects
(case PDFs, rehearsal video) go to **dock assets** (signed-PUT, P-assets); the envelope carries a
`PayloadRef{URI}` and the pipeline's `OnTransferDone` path defers parse/chunk/embed until the
asset's `transfer.complete` fires on the EventBus.

### Per-vertical mapping

| Vertical | Source | Trigger | Plugins (business code) | Dispatch |
|---|---|---|---|---|
| **buildings** | BMS/IoT signals (temp, power, alarms) | Immediate | temp/power Classifier+Processor (clean/convert/downsample) | `alert` → **work order** (already built in polar-buildings) |
| **opera** | rehearsal/show signals (EEG, cues, ms-aligned multimodal) | Immediate, ms `DeviceTS` | eeg Classifier + arousal-peak Processor + cross-modal correlate | `capture`/`analyze` clip → observability timeline |
| **lawyer** | case/statute docs (婚姻/不良资产) by object ref | OnTransferDone | legal Classifier + parse→chunk(by_article)→**embed** Processor | `rag_write` → **pgvector KB**, then `analyze` (LLM over retrieved chunks) |

buildings is mostly built — dataflow formalizes its alarm→work-order path. lawyer/opera are the
new builds; lawyer is the RAG-heavy one.

## pgRAG / pgvector

- **Extension**: `CREATE EXTENSION vector;` in each vertical DB. One canonical table owned by
  `polar-dataflow/rag/schema.sql`:
  ```sql
  CREATE TABLE rag_chunks (
    id BIGSERIAL PRIMARY KEY,
    kb TEXT NOT NULL,                 -- knowledge base (e.g. "divorce-precedents")
    tenant_id TEXT NOT NULL,
    source_uri TEXT NOT NULL,         -- the asset object the chunk came from
    data_type TEXT NOT NULL,          -- legal_case / statute / sop / script ...
    chunk_idx INT NOT NULL,
    text TEXT NOT NULL,
    embedding vector(1024) NOT NULL,  -- dim per the chosen on-prem model
    meta JSONB NOT NULL DEFAULT '{}', -- case_no, effective_date, status(in-force/repealed), 溯源
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
  );
  CREATE INDEX ON rag_chunks USING hnsw (embedding vector_cosine_ops);
  CREATE INDEX ON rag_chunks (kb, tenant_id, data_type);
  ```
- **Search** is ANN + metadata filter: `Search(kb, queryVec, k, filter{tenant, data_type,
  status:'in-force', effective_date<=now})`. The legal effectivity/version filter the README
  calls out lives in `meta` + the WHERE clause.
- **Provenance** is mandatory for legal: every chunk keeps `source_uri`, `case_no`,
  `effective_date`, `status`, `tenant_id` — already modeled in the legal RAGDispatcher.
- **Embedding stays on-prem**: `rag.Embedder` default = `adapters.EmbedProxy` → dock llm-proxy →
  an embedding `llm_config` whose `base_url` points at a **local** embedding server (BGE/M3E/Qwen-
  embed via Ollama/llama.cpp/vLLM on the box). Reuses P1: spend is billed + audited, model never
  egresses. Air-gap installs register a direct-local `rag.Embedder` and skip the proxy.

## Integration with the existing platform (no new heavy service for the core path)

- **Large objects** → dock **assets** (P-assets signed-PUT); envelope holds the `PayloadRef`.
- **transfer.complete** → emitted onto the EventBus when the asset finalize handler fires
  (assets already has a finalize step; bridge it to NATS, or to a dock `/internal/v1` webhook the
  module subscribes to).
- **Alerts / tasks** → `adapters.AlertDock` raises a buildings work-order / a notification / an
  LLM analysis task through dock (reusing the existing AI-dispatch path) via `polar-sdk`.
- **LLM analysis** (RAG answer, alarm triage) → dock **llm-proxy** (P1, billed/audited).
- **Identity / heartbeat / nav / release** → `polar-sdk` as every module already does.

So the only genuinely new shared pieces are the **`polar-dataflow` library** (framework + rag +
adapters) and the **pgvector tables** per vertical DB. No central data-plane service is required.

## The one fork worth your call: RAG store topology

- **(A) Per-vertical pgvector** (recommended) — `rag_chunks` lives in each `polar_<v>` DB, the
  shared `rag` package gives identical ops. Best isolation (一户一库 / air-gap), simplest, no extra
  service, no cross-tenant blast radius. Downside: no single cross-vertical "all knowledge bases"
  admin view (rarely wanted; verticals are sold separately).
- **(B) Central `polar-rag-svc`** — one multi-tenant vector service all verticals call over REST.
  One KB admin surface, one place to tune ANN/recall. Downside: a shared failure domain + network
  hop on the hot ingest/search path, weaker data isolation, and it fights the "数据不出域 per
  tenant" story. Only worth it if you want to *sell RAG itself* as a standalone product.

Recommendation: **(A)**, with the shared `rag` package so adopting (B) later is just swapping the
`RAGStore` impl behind the same interface.

## Phasing

- **P0** — extract `research/dataflow` → `polar-dataflow` repo (core/pipeline/rules + tests, as-is).
- **P1** — add `rag/` (pgvector schema + store + Embedder port) and `adapters/` (StorePG, AlertDock,
  EmbedProxy, LogSlog; BusNATS optional — can start with an in-proc bus).
- **P2** — wire **polar-lawyer** end-to-end (it's the RAG showcase + fast-cash vertical): intake →
  asset upload → OnTransferDone → parse/chunk/embed → pgvector → LLM-over-retrieval answer UI.
- **P3** — refactor **polar-buildings** alarm→work-order onto the dataflow pipeline (formalize the
  existing ad-hoc path; immediate-trigger reference impl).
- **P4** — **polar-opera** (ms-aligned multimodal observability) once a real signal source exists.

## Risks / notes (from the framework's own README, carried forward)

- Alert convergence/dedup in the `AlertSink` (avoid alarm storms).
- `SweepExpired` must be driven by a ticker (clean up transfer-timeout parks).
- Audit/state log must be tamper-evident for 政企/等保 deployments.
- Legal embeddings MUST be local; chunks carry version/effective-date/status + full provenance.
- Backpressure: Dispatcher rate-limit + EventBus must tolerate backlog.
