# OtelContext — AI Agent Instructions

## Project Overview

OtelContext is a self-hosted OTLP observability platform. Single Go binary with embedded React frontend.
- **Backend:** Go 1.25, native `net/http` (no frameworks), GORM ORM, gRPC + HTTP for OTLP ingestion
- **Frontend:** React 19 + TypeScript + TanStack Query/Virtual + wouter + Radix primitives + cmdk palette + hand-rolled token CSS (`ui/src/styles/tokens.css`) with CSS Modules. The **service map renders via React Flow (`@xyflow/react`, MIT)** over a deterministic phyllotaxis ("sunflower") layout (`ui/src/lib/radialLayout.ts` — most-critical service near the disc centre, golden-angle spiral) computed synchronously. Unlike a layered DAG, the sunflower fills a disc evenly and never collapses sparse/disconnected graphs into a single column, so the map stays navigable from 7 to 120+ services. React Flow owns pan/zoom/fit/minimap/grid/a11y; nodes are token-themed React components — draggable, and they collapse to status **dots** below a zoom threshold (level-of-detail) so they stop occluding edges at scale. Edges are straight chords pinned to node borders (centre-to-centre at dot zoom) carrying a direction **arrowhead** (`markerEnd`) so flow is legible statically — at any zoom and under `prefers-reduced-motion`; selecting a node accents the active path (both nodes **and** edges) and dims the rest. No UI framework: `@ossrandom/design-system`, cytoscape, uplot, **and `@dagrejs/dagre`** remain removed (cytoscape's physics hairball → React Flow; the prior hand-rolled-SVG map was replaced by React Flow on 2026-06-18; the dagre layered layout it briefly used was itself replaced by the sunflower layout on 2026-06-18 because a layered DAG stacked sparse 120-service graphs into an unreadable single column)
- **Ports:** gRPC `:4317` (OTLP), HTTP `:8080` (API + HTTP OTLP + WebSocket + UI)

## Strict Rules

- NO Express.js/Gin/Echo — use native Go `net/http`
- NO Tailwind CSS, NO Mantine, NO general-purpose component frameworks — UI styling is the hand-rolled token sheet (`ui/src/styles/tokens.css`) + per-component CSS Modules. Sanctioned third-party UI: Radix primitives (unstyled) for the a11y-hard parts (dialog/tabs/tooltip/dropdown), and **React Flow (`@xyflow/react`) for the service map only** — themed to the token sheet, nodes/edges are our own components. Token values only — no raw hex outside tokens.css.
- Single-service architecture (no microservices split)
- All internal DBs must be **embedded** (no external processes)
- Relational DB (SQLite/MySQL/PostgreSQL/MSSQL) is the **single source of truth**
- Prioritize self-hosted, open-source solutions
- The `internal/graph/` package is **legacy** — use `internal/graphrag/` for all new graph work

## Architecture

```
gRPC :4317 (OTLP Ingest) ──► Ingestion Layer ──► Storage (GORM)
HTTP :8080/v1/* (OTLP HTTP)─┘       │                    │
                                     ▼                    ▼
                               In-Memory Accel.      Relational DB
                               (TSDB Ring,           (Source of Truth,
                                GraphRAG)             7-15 day retention)
                                     │
HTTP :8080 ◄── REST API ◄───────────┘
           ◄── WebSocket (real-time)
           ◄── MCP Server (AI agents, 7-tool triage surface)
           ◄── Prometheus /metrics
```

## Ingestion Paths

| Path | Endpoint | Content Types | Notes |
|------|----------|---------------|-------|
| gRPC | `:4317` | protobuf | Traces, Logs, Metrics via OTLP gRPC |
| HTTP | `/v1/traces`, `/v1/logs`, `/v1/metrics` | `application/x-protobuf`, `application/json` | OTLP HTTP spec compliant, gzip support, 4MB limit. Returns `429 Too Many Requests` + `Retry-After: 1` when the async pipeline queue is full (parity with gRPC `RESOURCE_EXHAUSTED`). |

Both paths delegate to the same `Export()` methods — zero business logic duplication. By default `Export()` parses the OTLP request and hands a `Batch` to the async ingest `Pipeline` (`internal/ingest/pipeline.go`); a worker pool persists Trace→Span→Log in order. With `INGEST_ASYNC_ENABLED=false` the pipeline is bypassed and `Export()` writes inline (legacy path).

### Multi-tenancy

Tenant identity flows into the request context on every write and read. An
**authenticated tenant key outranks every one of these carriers** — see the
Authentication section:
- **Authenticated identity (highest):** a bearer key from `API_TENANT_KEYS_FILE`, or a proxy-injected `X-OtelContext-Tenant` under `AUTH_TRUST_EXTERNAL`. Binding is absolute; contradicting client assertions are ignored and counted on `OtelContext_auth_tenant_conflicts_total{surface,reason}`.
- **HTTP:** `X-Tenant-ID` header (see `internal/api/tenant_middleware.go`).
- **gRPC:** `x-tenant-id` metadata key (see `internal/ingest/otlp.go`).
- **OTLP resource attribute:** `tenant.id` on the resource overrides the header/metadata (only when `OTLP_TRUST_RESOURCE_TENANT=true`).
- **WebSocket:** one tenant per connection, fixed at the handshake — never a protocol message.

When none are present, `DEFAULT_TENANT` (default `"default"`) is assigned. The
resolved tenant is stamped on every async-pipeline `Batch`, so
`INGEST_PIPELINE_PER_TENANT_CAP` charges each tenant its own admission slot.
When `OTLP_TRUST_RESOURCE_TENANT=true` and one Export carries resources for
several tenants, the Export is split into one batch per tenant. Every row in the relational DB carries a `tenant_id` column; every read method in `internal/storage/` scopes by the tenant in the request context (`Where("tenant_id = ?", tenant)`). Retention (`RetentionScheduler`) is **cross-tenant** — it purges by age, not by tenant.

### OTLP Metric Points in Aggregate Mode

`MetricsServer.Export` (`internal/ingest/otlp.go`) accounts four OTLP point
types. Folding lives in `internal/aggregate/histogram.go`; the platform sketch
is positive-only at scale 4.

| Point type | Aggregate handling |
|---|---|
| `NumberDataPoint` (Gauge, Sum) | Gauge-like or baseline-converted per the temporality model. In legacy and shadow it also feeds the TSDB ring and the real-time bypass; in `AGGREGATE_MODE=aggregate` neither exists and no `tsdb.RawMetric` is built at all. |
| `HistogramDataPoint` (delta) | Bucket counts folded as **weighted** synthetic observations at each finite positive bucket's geometric midpoint — one sketch add per bucket, never one per count. |
| `ExponentialHistogramDataPoint` (delta) | Exact index transfer. Scale > 4 downscales by shifting indexes; scale 0–3 downscales the accumulating sketch to the source scale; `zero_count` folds into the zero bucket. |
| `SummaryDataPoint` | **Wholly unsupported.** Producer-chosen quantiles cannot be merged across series or windows. |

**Delta temporality only.** GA aggregates delta-temporality Histogram and
ExponentialHistogram points. A cumulative (or unspecified-temporality)
histogram point is refused **completely** — no count, no scalar, no bin — and
counted on `otelcontext_ingest_metrics_unsupported_total{type,reason}`.
Convert upstream with the OpenTelemetry Collector's
[`cumulativetodeltaprocessor`](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/cumulativetodeltaprocessor),
which handles cumulative Sum, Histogram and ExponentialHistogram streams.
⚠️ That processor is **stateful**: it keeps the previous cumulative value per
series in memory, so the collector fleet must route each series to **one**
instance (a single collector, or a `loadbalancing` exporter keyed by the
resource/attribute set). Round-robin across replicas produces wrong deltas and
spurious resets, not an error.

**Percentile honesty.** Scalars (count/sum/min/max) are kept even when the
distribution cannot enter the sketch; percentiles are then marked unavailable
and `otelcontext_ingest_metrics_sketch_dropped_total{reason}` is incremented:
- `negative_observations` — negative exponential buckets held counts, or an
  explicit bucket admits negative values and no reported `min >= 0` proves the
  population non-negative. Publishing the positive side's p99 as the global p99
  is a lie, so no quantile is published at all.
- `scale_out_of_range` — an ExponentialHistogram below scale 0. Valid OTLP,
  not representable by the unsigned sketch scale.
- `no_finite_boundaries` — an explicit-bounds Histogram with observations and
  no boundary.

The `+Inf` bucket is tracked separately (`HistogramTailCount` /
`HistogramTailBound`) and never folded. `AggregateDelta.HistogramQuantile`
returns `(value, lowerBound, ok)`: a quantile landing in the tail is answered
as `p99 >= last_finite_boundary`, never as an ordinary estimate.
`AccuracyMetadata` carries `source_bucket_error` and `unbounded_tail`
alongside the sketch's own bound — a bare `degraded=true` does not describe an
unbounded bucket.

**Dimensions.** Configured `AGGREGATE_METRIC_DIMS` keys are extracted from each
point's attributes into the SeriesKey `DimsID`, identically for all three point
types. Missing any configured key yields `DimsID=0` (all-or-nothing). String,
int, bool and double attribute values are supported; array and kvlist values
are refused from identity and counted on
`otelcontext_ingest_metrics_dims_rejected_total{reason="unsupported_value_type"}`.
Extraction runs against a request-local scratch — no per-point map allocation.

**Response honesty.** Every refused point lands in
`ExportMetricsPartialSuccess.rejected_data_points` with an exact count and a
bounded message naming the types and reasons. The Export still returns success
for the accepted points, and **clients must not retry a populated
partial-success response**. Late and future points are *not* counted there:
they are excluded from aggregates and reported on the lateness counters, but a
retry would not change their fate. Legacy mode (`AGGREGATE_MODE=legacy`) has no
aggregate accounting to be honest about and leaves the response unchanged.

Aggregate store schema is **v5**: v4 added the eight `hist_*` columns carrying
population statistics and accuracy metadata; v5 added `aggregate_log_template`
(durable log-template miner state) plus the `dict_id_high_watermark` /
`series_id_high_watermark` meta keys. An older file cannot be migrated — start
an older binary or set `AGGREGATE_ALLOW_REBUILD=true`.

### Legacy TSDB retirement in aggregate mode (#194 finding 10)

`AGGREGATE_MODE=aggregate` constructs **no** `tsdb.Aggregator`, **no**
`tsdb.RingBuffer` and **no** metric callback. `main.go` gates all three on
`legacyMetricPath(cfg.AggregateMode)`, which is false only for pure aggregate
mode — legacy has no other metric store, and shadow's entire job is running both
paths side by side.

Consequences, all deliberate:

- `MetricsServer.exportNumberPoints` skips the `tsdb.RawMetric` build (and its
  per-point attribute map) when neither a TSDB aggregator nor a metric callback
  is wired. The aggregate reducer still sees every point.
- Nothing writes the `metric_buckets` table. The rows are not read in aggregate
  mode either; `RetentionScheduler` keeps purging whatever an earlier legacy run
  left behind.
- `GraphRAG.OnMetricIngested` is not called. Metric nodes were already replaced
  per topology revision in aggregate mode, so the callback was paying for a
  channel hop and an allocation to have `processMetric` discard the event.
- `EventHub.BroadcastMetric` is not called. The aggregate publisher already
  short-circuited it; the coalesced revision-driven snapshot is the live feed.
- The TSDB-specific Prometheus collectors are **unregistered** at startup via
  `Metrics.DisableTSDBCollectors()`: `OtelContext_tsdb_ingest_total`,
  `_tsdb_flush_duration_seconds`, `_tsdb_batches_dropped_total`,
  `_tsdb_cardinality_overflow_total`,
  `otelcontext_tsdb_cardinality_overflow_by_tenant_total`,
  `otelcontext_tsdb_ring_series_active`, `otelcontext_tsdb_ring_series_rejected_total`.
  A cardinality counter frozen at 0 reads as "no overflow" rather than "no
  TSDB". The aggregate engine publishes its own admission and cardinality caps
  (#200/#201) and must not be shadowed by dead series. `METRIC_MAX_CARDINALITY`
  therefore has no effect in aggregate mode.

**Metric read endpoints in aggregate mode.** Both are served from the engine's
topology projection (`Engine.TopologySnapshot`), not from `metric_buckets`:

| Endpoint | Aggregate-mode source | What changes |
|---|---|---|
| `GET /api/metrics?name=…` | Topology projection metric windows | Same bare-array shape and `time_bucket ASC` ordering. Bucket **width** is the engine's 5-minute window, not 30s. `id` is `0` and `attributes_json` is empty — an aggregate window has no row identity and the projection keys metrics by `(service, name)` only. |
| `GET /api/metadata/metrics` | Distinct names in the projection | Same sorted bare array. |

Both stamp the `OtelContext-Data-Coverage` header. `/api/metrics` is `full` when
the requested range sits inside the retained horizon and no projection cap fired,
`sampled` otherwise. `/api/metadata/metrics` is **always** `sampled`: the
projection retains a bounded recent horizon (the mutable windows plus
`TopologySnapshot.Horizon`, 30m by default) while the legacy answer spanned
`HOT_RETENTION_DAYS`, so a metric that stopped reporting an hour ago is simply
absent. History older than that horizon is unavailable in aggregate mode — it is
reported as reduced coverage, never as a flat line.

### Engine-sourced topology and finalized-horizon restore (#194 finding 15)

`Engine.QueryTopology` returns **nodes and edges from one query**: one tenant,
one range, one ownership snapshot. The store half is SQL `GROUP BY` on both
sides — one row per service for the nodes, one row per `(caller, callee)` for
the edges (`GroupByService|GroupByName` under a `SignalServiceEdge` selector) —
so neither half can be truncated by a row cap. A `Services` filter selects a
**subgraph**: an edge survives only when both ends do, so the result never
carries an edge hanging off a node the response omits.

The GraphRAG edge side-channel is **gone** from aggregate-mode responses.
`GET /api/metrics/service-map` and the WebSocket `live_snapshot` used to take
their nodes from the engine and their edges from the GraphRAG service store — a
different range, a different retention rule, exemplar-fed counts — and marked
the whole response `sampled` to cover for it. Both now carry the engine's own
coverage (`full`) and its edges. GraphRAG itself is untouched: `/api/graph`
still reads it, and in aggregate mode its topology is already replaced per
revision from `TopologySnapshot`.

**Startup restore.** `aggregate.Recover` takes a `RecoverOptions` and, when
`TopologyHorizon > 0`, rebuilds the topology projection from **finalized bucket
rows** inside that horizon (`Store.ReadFinalizedSince`, newest window first)
plus the mutable delta rows it just replayed. Without it a restart erased the
recent service map — every node, edge and metric baseline — while the numbers
sat in the bucket table.

This is the **only** exception to "finalized history never hydrates", and it
stops short of the shards: it writes to the projection only, so no finalized
window can re-enter the mutable set through it. It is bounded three ways — the
configured horizon, a row cap (`RecoverOptions.TopologyMaxRows`, default
20,000, clamped to `MaxReadRows`), and the projection's own retention cutoff,
which is why recovery reports what the fold **accepted**, not what the store
returned.

- `AGGREGATE_TOPOLOGY_RESTORE_HORIZON` (Go duration, default `30m`, `0`
  disables, validated `0..24h`) — clamped internally to
  `TopologySnapshot.Horizon`; reading a window the projection would prune on
  arrival is startup cost with nothing to show for it.
- Recovery reports `restored_topology_rows`, `restored_topology_windows` and
  `restored_topology_truncated` on the recovery log line, and
  `otelcontext_aggregate_recovery_rows{kind="topology_restored_rows"|"topology_restored_windows"}`.
  A truncated restore logs a warning: the topology is real but incomplete at
  its oldest end.

### Aggregate identity lifecycle (#200)

The dictionary and series tables were append-only. They are not any more.

**Dictionary/series GC.** A mark-and-sweep pass runs on the **daily**
maintenance tick (`RetentionScheduler`, wired from `main.go` next to
`aggStore.Analyze`; `AGGREGATE_GC_ENABLED=false` disables it). The **mark**
phase reads the reference set (`aggregate_buckets`, `aggregate_delta_log`,
`aggregate_baseline`) and all three identity tables inside **one deferred read
transaction** on the read pool — no writer lock, so a daily full scan can never
become an ACK-latency incident. Marking is transitive: surviving series mark
their tenant/service/name/dim-tuple IDs, surviving dim tuples mark the dim-key
and dim-value IDs encoded inside them, and miner templates plus persisted alias
records mark **both ends** of every alias, followed transitively through
retired chains. The **sweep** phase runs through the writer's
identity-maintenance barrier (`Writer.RunBarrier`, executed on the commit
goroutine): revalidate candidates against durable, active, pending and staged
references → fence survivors from new lookup → delete series, then dict, then
template rows in **one** transaction → remove forward and reverse map entries
only after the commit. A failed delete releases the fence and changes nothing
in memory. `internal/aggregate/gc.go`.

**High-watermarks, not MAX(id)+1.** `aggregate_meta` carries
`dict_id_high_watermark` and `series_id_high_watermark`, bumped inside the same
transaction as the rows they cover and never decreasing. ID allocation reseeds
from them: once GC can delete the highest ID, `MAX(id)+1` would re-mint a number
a finalized bucket or an alias row still names.

**Miner persistence.** `aggregate_log_template` holds tenant/service partition,
stable template ID (which IS the `log_template` dictionary ID and IS the log
series `NameID`), versioned token pattern, alias target, partition sequence and
the overflow flag. Identity-critical mutations — new templates, pattern
generalizations, alias changes — are staged into `GroupBatch.Templates` and
commit **atomically with the delta that used the identity**; a periodic
snapshot alone lets acknowledged identity state vanish in a crash. Counters
(`hit_count`, `first_ts`, `last_ts`) take the cheap periodic dirty write plus a
best-effort save at shutdown. **Raw log samples are never persisted** —
credential/PII sink; exemplars already carry the raw line. Reload
(`aggregate.RestoreMiner`) rebuilds partitions and prefix trees in `main.go`
**before** recovery and before ingest starts.

**Identity bounds.** Every non-tenant dictionary value carries an encoded-length
cap (`AGGREGATE_MAX_VALUE_BYTES`, default 512); over-length identities route to
`__other__` and are **never truncated**. Service, dim-key, dim-value and
dim-tuple namespaces now carry both per-tenant and instance-wide count caps.
The tenant namespace is different: an over-length, empty, or over-cap tenant is
**REJECTED** — the point is refused and counted on
`otelcontext_aggregate_tenant_rejected_total`, never collapsed into a shared
`__other__` tenant, because a shared overflow tenant is exactly the cross-tenant
merge the cap exists to prevent. Startup identity preloads ask for `limit+1` and
fail with a `*PreloadError` when the table exceeds the supported bound, instead
of truncating the load in silence.

Observability: `otelcontext_aggregate_gc_runs_total{result}`,
`otelcontext_aggregate_gc_duration_seconds{phase=mark|barrier}`,
`otelcontext_aggregate_gc_swept_total{table}`,
`otelcontext_aggregate_gc_retained{table}`,
`otelcontext_aggregate_identity_overflow_total{kind,bound}`,
`otelcontext_aggregate_tenant_rejected_total{signal}`.

## Storage Architecture

| Layer | Package | Purpose |
|-------|---------|---------|
| GraphRAG (in-memory) | `internal/graphrag/` | Layered graph: 4 typed stores, error chains, root cause analysis, anomaly detection |
| Time Series (in-memory) | `internal/tsdb/` | Ring buffer, sliding windows, pre-computed percentiles. **Not constructed in `AGGREGATE_MODE=aggregate`** — the aggregate engine owns metric storage and reads there (#194 finding 10). |
| Graph (in-memory, legacy) | `internal/graph/` | Simple service topology — **being replaced by GraphRAG** |
| Relational (persistent) | `internal/storage/` | GORM-based, multi-DB, single source of truth. Driven by `RetentionScheduler` (hourly batched purge + daily VACUUM/ANALYZE). `logs.body` is plain TEXT. **Log search**: SQLite FTS5 (`logs_fts`, porter+unicode61, ordered by `bm25()`, AFTER INSERT/DELETE/UPDATE triggers) is the default path — `LOG_FTS_ENABLED` defaults to `true` when `DB_DRIVER=sqlite` and `false` otherwise. Operators who want the ~30% disk savings can set `LOG_FTS_ENABLED=false` and reclaim the FTS table + indexes via `POST /api/admin/drop_fts`. Postgres uses `pg_trgm` GIN on `logs.body` and `logs.service_name`. `AttributesJSON` and `AIInsight` remain `CompressedText`. The `search_logs` MCP tool and the API `/api/logs?q=…` filter are clamped to the **last 24 hours** to bound the LIKE-fallback worst case. The `vectordb` package (TF-IDF semantic search) was removed on 2026-05-24 alongside the `find_similar_logs` MCP tool — `data/vectordb.snapshot` is left on disk for operators to delete by hand. |

## GraphRAG Architecture

The `internal/graphrag/` package is the core intelligence layer. It replaces the simple `internal/graph/` for advanced observability queries.

### Layered Stores (each with own `sync.RWMutex`)

| Store | Nodes | Edges | TTL |
|-------|-------|-------|-----|
| `ServiceStore` | ServiceNode, OperationNode | CALLS, EXPOSES | Permanent |
| `TraceStore` | TraceNode, SpanNode | CONTAINS, CHILD_OF | Configurable (default 1h) |
| `SignalStore` | LogClusterNode, MetricNode | EMITTED_BY, MEASURED_BY, LOGGED_DURING | 24h TTL + per-tenant caps |
| `AnomalyStore` | AnomalyNode | PRECEDED_BY, TRIGGERED_BY | 24h |

### Node Types (7)
`ServiceNode`, `OperationNode`, `TraceNode`, `SpanNode`, `LogClusterNode`, `MetricNode`, `AnomalyNode`

### Edge Types (9)
`CALLS`, `EXPOSES`, `CONTAINS`, `CHILD_OF`, `EMITTED_BY`, `LOGGED_DURING`, `MEASURED_BY`, `PRECEDED_BY`, `TRIGGERED_BY`

### Query Functions
| Function | Algorithm | Purpose |
|----------|-----------|---------|
| `ErrorChain(service, timeRange)` | BFS upstream via CHILD_OF + CALLS | Trace error to responsible service |
| `ImpactAnalysis(service, depth)` | BFS downstream via CALLS | Blast radius |
| `RootCauseAnalysis(service, timeRange)` | ErrorChain + anomaly correlation | Ranked probable causes with evidence |
| `DependencyChain(traceID)` | Tree from CONTAINS + CHILD_OF | Full trace visualization |
| `CorrelatedSignals(service, timeRange)` | Gather all edges | Related logs/metrics/traces |
| `ShortestPath(from, to)` | Dijkstra weighted by inverse call freq | Service communication path |
| `AnomalyTimeline(since)` | Time-sorted anomalies + PRECEDED_BY | Recent anomaly overview |
| `ServiceMap(depth)` | Full topology dump | Service topology + health |

### Background Processes
- **4 event workers** consume from a 10,000-capacity buffered channel (best-effort; DB is source of truth)
- **Refresh loop** (60s) — rebuilds from DB, prunes expired TraceStore nodes, cleans old anomalies
- **Snapshot loop** (15min) — persists Drain templates so cluster IDs survive restart (the `graph_snapshots` write side was removed on 2026-05-24; the loop name is retained for wiring stability)
- **Anomaly loop** (10s) — detects error spikes, latency degradation, metric z-score anomalies

### Persistence Models (GORM)
- `Investigation` — automated error analysis records (trigger, root cause, causal chain, evidence)
- `DrainTemplateRow` — persisted Drain log templates (table `drain_templates`), loaded on startup to warm the miner

> Note: `GraphSnapshot` (table `graph_snapshots`) was removed on 2026-05-24. AutoMigrate no longer creates the table on fresh deploys; existing populated tables are left in place — operators can `DROP TABLE graph_snapshots; VACUUM;` to reclaim disk.

### Log Clustering (Drain)

Log clustering uses **Drain** template mining (`internal/graphrag/drain.go`) — a deterministic fixed-depth prefix tree with O(1) LRU via `container/list`. Templates are persisted to the `drain_templates` table and reloaded on startup so cluster IDs stay stable across restarts.

### Ingestion Callbacks
```
TraceServer.Export() → DB persist → spanCallback → GraphRAG.OnSpanIngested()
LogsServer.Export()  → DB persist → logCallback  → GraphRAG.OnLogIngested()
MetricsServer.Export() → TSDB    → metricCallback → GraphRAG.OnMetricIngested()
```

In `AGGREGATE_MODE=aggregate` the metric line does not exist: no TSDB, no
`metricCallback`, no `tsdb.RawMetric`. Metric state reaches GraphRAG through the
engine's topology snapshot instead.

**Pre-sample topology observer** (`internal/graphrag/topology_observer.go`): `TraceServer.Export()` also calls `GraphRAG.ObserveSpanTopology()` for **every** received span *before* the sampler's keep/drop decision, so cross-service CALLS edges are recorded independent of `SAMPLING_RATE`. Without it, an edge formed only when both the caller and callee spans survived sampling (~`rate²` joint probability), so at low sampling the service map showed nodes but almost no edges/flow. The observer is existence-only (`ServiceStore.EnsureService`/`EnsureCallEdge` create with zeroed aggregates, no-op if present) so the sampled path stays the **sole** source of CallCount/latency/error-rate; it's bounded by a per-tenant spanID→service LRU (cap 100k) + per-pair dedup (memory ≈ #service-pairs) and never touches TraceStore/eventCh, so the SQLite OOM-survival bounds are unaffected.

## MCP Server — 7-Tool Triage Surface

The MCP server (`internal/mcp/`) exposes a focused 7-tool triage surface via
HTTP Streamable MCP (JSON-RPC 2.0 POST + SSE GET). The surface was reduced
from 21 → 7 on 2026-05-24 so the platform survives 120 services on SQLite —
see `docs/superpowers/specs/2026-05-24-mcp-7tool-sqlite-survival-design.md`
for the full rationale.

| Tool | Input | Source |
|------|-------|--------|
| `get_anomaly_timeline` | `{since?, service?}` | In-memory (instant) — triage entry point |
| `get_service_map` | `{depth?, service?}` | In-memory (instant) — topology + health overlay |
| `get_service_health` | `{service_name}` | In-memory (instant) — per-service drill-down |
| `root_cause_analysis` | `{service, time_range?}` | In-memory (instant) — ranked probable causes |
| `impact_analysis` | `{service, depth?}` | In-memory (instant) — blast radius |
| `trace_graph` | `{trace_id}` | In-memory + DB fallback — trace tree visualisation |
| `search_logs` | `{query?, severity?, service?, trace_id?, start?, end?, limit?, page?}` | DB (FTS5 default on SQLite, LIKE fallback, 24h-clamped) |

Cut tools (clients now receive an `unknown tool` RPC error): `get_system_graph`,
`tail_logs`, `get_trace`, `search_traces`, `get_metrics`, `get_dashboard_stats`,
`get_storage_status`, `find_similar_logs`, `get_alerts`, `correlated_signals`,
`get_error_chains`, `get_investigations`, `get_investigation`, `get_graph_snapshot`.

Cacheable surface (5s TTL via `MCP_CACHE_TTL_MS`): `get_anomaly_timeline`,
`get_service_map`, `get_service_health`, `root_cause_analysis`, `impact_analysis`.

Every error-identifying tool returns a `root_cause` block:
```json
{"root_cause": {"service": "...", "operation": "...", "error_message": "...", "span_id": "...", "trace_id": "..."}}
```

## DLQ (Dead Letter Queue)

Uses typed envelopes for all data types:
```json
{"type": "logs|spans|traces|metrics", "data": [...]}
```
Legacy format (raw `[]storage.Log` JSON) is supported for backward compatibility.

A fifth envelope type carries a whole failed async-pipeline batch, whose `data`
is an object rather than an array so the three signal slices replay through a
single `BatchCreateAll` transaction (preserving Trace→Span→Log FK ordering):
```json
{"type": "batch", "data": {"tenant": "...", "signal": "traces|logs", "traces": [...], "spans": [...], "logs": [...]}}
```
`Pipeline.SetDLQ()` wires the sink; without it a `BatchCreateAll` failure still
drops the batch (pre-existing behaviour). Outcomes are counted on
`otelcontext_ingest_pipeline_dlq_total{signal,result}` —
`result=enqueued` (durable, awaiting replay), `enqueue_failed` or `no_sink`
(batch lost). Replay is **at-least-once**: traces and spans collapse duplicates
on their composite unique indexes, logs have no stable OTLP identifier and can
duplicate on replay.

## Shutdown Order

Proper LIFO ordering to prevent data loss:
1. gRPC `GracefulStop()` + HTTP `Shutdown()` — stop ingestion
2. WebSocket Hub + Event Hub + AI Service — stop real-time
3. TSDB + Graph + GraphRAG — stop processing (TSDB is nil and skipped in aggregate mode)
4. DLQ — stop replay
5. RetentionScheduler `Stop()` — halt purge/maintenance ticks
6. DB `Close()` — close database last

## Key Directories

```
internal/
  ai/           # AI service integration
  api/          # HTTP handlers, middleware, rate limiting, graph_handler
  cache/        # TTL cache with synchronized Stop()
  compress/     # Zstd compression utilities
  config/       # Environment configuration (40+ fields)
  graph/        # LEGACY in-memory service graph — use graphrag/ for new work
  graphrag/     # GraphRAG: layered graph, error chains, anomaly detection, investigations
    schema.go       # 7 node types, 9 edge types, query result types
    store.go        # 4 typed stores (Service, Trace, Signal, Anomaly)
    builder.go      # Event workers, ingestion callbacks, GraphRAG coordinator
    queries.go      # ErrorChain, ImpactAnalysis, RootCause, ShortestPath, etc.
    investigation.go # GORM Investigation model + persistence
    anomaly.go      # Z-score, error spike, latency degradation detection
    drain.go        # Log clustering via Drain template mining — pure-Go, stdlib-only, deterministic fixed-depth prefix tree
    refresh.go      # Periodic DB rebuild + pruning + Drain template persistence
  ingest/       # OTLP receivers (gRPC + HTTP), adaptive sampling
    otlp.go         # gRPC TraceServer, LogsServer, MetricsServer
    otlp_http.go    # HTTP OTLP handler (protobuf + JSON, gzip, 4MB limit)
    sampler.go      # Per-service token bucket sampler
  mcp/          # MCP server (7-tool triage surface, JSON-RPC 2.0 + SSE)
  queue/        # Dead Letter Queue (typed envelopes, bounded disk, exp backoff)
  realtime/     # WebSocket hub + event streaming
  storage/      # GORM repository, models, migrations, Close() method, SQLite PRAGMA stanza
  telemetry/    # Prometheus metrics + health (19 metrics)
  tsdb/         # Time series aggregator + ring buffer (lock-free Windows()) — legacy/shadow only
  ui/           # Embedded React frontend
ui/             # React frontend (Vite + token CSS Modules, no UI framework)
test/           # Microservice simulation (7 services)
docs/           # Specifications and plans
```

## Configuration (Environment Variables)

Key settings in `internal/config/config.go`:
- `HTTP_PORT` (8080), `GRPC_PORT` (4317), `DB_DRIVER` (sqlite), `DB_DSN`
- `DB_AUTOMIGRATE` (true), `DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`, `DB_CONN_MAX_LIFETIME` (internally capped to 30m when `DB_AZURE_AUTH=true`)
- `DB_AZURE_AUTH` (false) — see Authentication below
- `TLS_CERT_FILE`, `TLS_KEY_FILE` — explicit TLS (both or neither)
- `TLS_AUTO_SELFSIGNED` (false), `TLS_CACHE_DIR` (`./data/tls`) — self-signed bootstrap, ignored if cert files set
- `API_KEY` — operator bearer token gating `/api/*`, `/v1/*`, `/mcp`, `/ws*`, and OTLP gRPC. Empty = auth disabled
- `API_TENANT_KEYS_FILE` — JSON or YAML (by extension) mapping bearer key → tenant; several keys per tenant allowed. Startup-only load, digest-only in memory, constant-time compare. File must not be group/other readable or writable (>0600 refuses startup, quoting the actual mode). A tenant key BINDS the request: `X-Tenant-ID`, `x-tenant-id` metadata, and `tenant.id` resource attributes are ignored and counted
- `AUTH_TRUST_EXTERNAL` (false), `AUTH_EXTERNAL_TENANT_HEADER` (`X-OtelContext-Tenant`) — proxy-injected identity; read the mandatory deployment contract in Authentication before enabling
- `WS_ALLOWED_ORIGINS` (`""` = same-host only) — comma-separated origins (`https://app.example.com`) or bare hosts, enforced on `/ws*` whenever authentication is enabled or `APP_ENV=production`
- `WS_MAX_CLIENTS` (0 = unlimited) — admission cap on BOTH WebSocket hubs; past the cap the handshake gets 503 before the upgrade
- `GRPC_REFLECTION` — defaults to true outside production, false in production; set explicitly to override
- `OTELCONTEXT_ALLOW_INSECURE_GRPC` (false) — waives the production TLS+auth requirement on the OTLP gRPC listener
- `OTEL_EXPORTER_OTLP_ENDPOINT` — enables self-instrumentation (empty = off)
- `DEFAULT_TENANT` (`default`) — assigned to rows ingested without explicit tenant
- `HOT_RETENTION_DAYS` (7) — drives `RetentionScheduler`; range 1..36500
- `EXEMPLAR_RETENTION_DAYS` (2) — separate, shorter retention for the raw exemplar tier in aggregate mode; validated 1..`HOT_RETENTION_DAYS`. See the Data Disk Budget section
- `EXEMPLAR_BYTES_GLOBAL_WINDOW` (**3 MiB**, was 8 MiB) / `EXEMPLAR_BYTES_PER_SERVICE_WINDOW` (512 KiB) — instance-wide and per-service byte budget per 5-minute window
- `EXEMPLAR_SYNTH_LOGS_PER_SPAN` (8), `EXEMPLAR_SYNTH_LOGS_PER_TRACE` (64) — count caps on logs synthesized from span events and span status
- `DATA_DISK_BUDGET_MB` (8192), `DATA_DISK_PATH` (`./data`) — disk watchdog ceiling and the volume it `statfs`-es
- `SAMPLING_RATE` (1.0), `SAMPLING_ALWAYS_ON_ERRORS` (true), `SAMPLING_LATENCY_THRESHOLD_MS` (500)
- `METRIC_MAX_CARDINALITY` (10000), `METRIC_MAX_CARDINALITY_PER_TENANT` (0 = unlimited), `API_RATE_LIMIT_RPS` (100). The per-tenant cap is checked first; when set, a noisy tenant cannot exhaust the global pool. Overflow is labeled by tenant via `otelcontext_tsdb_cardinality_overflow_by_tenant_total{tenant_id}` (`__global__` sentinel when the global cap was the trigger). **Both caps are TSDB-only and inert in `AGGREGATE_MODE=aggregate`**, where the collectors are unregistered and the aggregate engine's own caps apply.
- `MCP_ENABLED` (true), `MCP_PATH` (/mcp)
- `MCP_MAX_CONCURRENT` (32), `MCP_CALL_TIMEOUT_MS` (30000), `MCP_CACHE_TTL_MS` (5000) — MCP HTTP streamable robustness. Counting semaphore gates concurrent `tools/call` (JSON-RPC `-32000` past the cap), per-call deadlines abort runaway handlers (JSON-RPC `-32001`), and a 5s TTL cache memoizes the cheap in-memory GraphRAG tools (`get_service_map`, `impact_analysis`, `root_cause_analysis`, `get_anomaly_timeline`, `get_service_health`). SSE GET sends a `: keep-alive\n\n` comment every 25s to keep the stream alive across reverse-proxy idle timeouts. Set any to 0 to disable.
- `LOG_FTS_ENABLED` — when truthy (`true`/`yes`/`on`/`1`), provisions the SQLite FTS5 `logs_fts` virtual table + sync triggers at startup; when false, log-search uses a 24h-clamped LIKE fallback. **Defaults to `true` when `DB_DRIVER=sqlite`** (BM25 is dramatically faster than LIKE on the kept `search_logs` MCP tool) and `false` otherwise. Toggle off and reclaim the ~30% disk overhead via `POST /api/admin/drop_fts` (refused while the flag is on). The vectordb-backed semantic-search path was removed on 2026-05-24.
- `DLQ_MAX_FILES` (1000), `DLQ_MAX_DISK_MB` (500), `DLQ_MAX_RETRIES` (10)
- `GRAPHRAG_WORKER_COUNT` (16), `GRAPHRAG_EVENT_QUEUE_SIZE` (100000; **10000 on SQLite**) — sized for 100–200 services; raise further if `otelcontext_graphrag_events_dropped_total` climbs
- `GRAPHRAG_TRACE_TTL` (`1h`; **`30m` on SQLite**), `GRAPHRAG_MAX_SPANS_PER_TENANT` (500000), `GRAPHRAG_TENANT_IDLE_TTL` (`24h`) — in-memory GraphRAG memory bounds. Spans past the per-tenant cap are skipped from the graph only (DB unaffected; metered as `otelcontext_graphrag_events_dropped_total{signal="span_capacity"}`); tenant store slices idle past the TTL are evicted (default tenant immune, self-healing via the 60s rebuild). SignalStore is bounded per-tenant by a 24h TTL + a cap: metrics 2000/tenant, log clusters 10000/tenant (constants; the log-cluster cap was added because clusters key on service×Drain-template-ID and would otherwise grow unbounded as template IDs churn).
- `PPROF_ADDR` (`127.0.0.1:6060`) — `net/http/pprof` on a dedicated loopback listener (never the public mux); empty disables. Startup also sets a soft `GOMEMLIMIT` (honors the env var, else 75% of the cgroup/host budget via `internal/membudget`).
- `INGEST_MIN_SEVERITY` (`INFO`), `STORE_MIN_SEVERITY` (**defaults to `"WARN"` for all drivers**; `""` falls back to same-as-ingest) — two-tier log severity gate. The ingest gate runs at the OTLP receiver and **drops the log entirely** below the threshold (no in-memory enrichment either). The store gate runs at the persist boundary inside the async pipeline (`internal/ingest/pipeline.go:process`) and **only skips the DB row write** — the log still flows through `LogCallback` so GraphRAG Drain template mining and span/trace correlation see it. By default (`STORE_MIN_SEVERITY=WARN`, `INGEST_MIN_SEVERITY=INFO`) INFO logs reach GraphRAG/Drain in-memory but are **not** persisted — the DB only grows with WARN+. To also analyse DEBUG in-memory (not just INFO), set `INGEST_MIN_SEVERITY=DEBUG` (raises in-memory event volume). Setting `STORE_MIN_SEVERITY` ≤ `INGEST_MIN_SEVERITY` is a no-op (logged as a warning at startup). Drops surface via `Pipeline.Stats().StoreFiltered`.
- `INGEST_ASYNC_ENABLED` (true), `INGEST_PIPELINE_QUEUE_SIZE` (50000), `INGEST_PIPELINE_WORKERS` (8), `INGEST_PIPELINE_MAX_BYTES` (536870912 = 512 MB; **128 MB on SQLite**) — async ingest pipeline (`internal/ingest/pipeline.go`). Hybrid backpressure: <90% accept all, 90–100% drop healthy batches (errors/slow always pass), 100% return gRPC `RESOURCE_EXHAUSTED`. The byte cap bounds queue memory regardless of item count — at the cap even priority batches get `RESOURCE_EXHAUSTED`/429 (a 429 is recoverable, an OOM kill is not); watch `otelcontext_ingest_pipeline_queue_bytes` and reason `bytes_full`. Set `INGEST_ASYNC_ENABLED=false` to revert to synchronous DB writes inside `Export()`. Drops surface as `otelcontext_ingest_pipeline_dropped_total{signal,reason}`.
- `GRPC_MAX_RECV_MB` (16), `GRPC_MAX_CONCURRENT_STREAMS` (1000) — OTLP gRPC server caps, validated to 1..256 and 1..1_000_000
- `RETENTION_BATCH_SIZE` (50000), `RETENTION_BATCH_SLEEP_MS` (1) — purge pacing; raise the sleep on busy production DBs
- `DB_POSTGRES_PARTITIONING` (`""`), `DB_PARTITION_LOOKAHEAD_DAYS` (3) — opt-in Postgres declarative range partitioning of the `logs` table by day. When `daily`, `logs` is provisioned as a partitioned parent (greenfield only — refuses to start if `logs` already exists unpartitioned), the `PartitionScheduler` maintains lookahead partitions and drops expired ones via `DROP TABLE`, and `RetentionScheduler` skips the row-level DELETE for `logs`. Watch `otelcontext_partitions_dropped_total` and `otelcontext_partitions_active`.
- `APP_ENV` (`"development"`), `OTELCONTEXT_ALLOW_SQLITE_PROD` (false) — SQLite is refused when `APP_ENV=production` unless the allow flag is set

### SQLite per-driver defaults (auto-flipped when DB_DRIVER=sqlite)

So a 100+ service deployment on SQLite survives without OOM, `config.Load()` overrides the defaults listed below at the end of the Load() pass — but **only when the operator did not explicitly set the env var** (detected via `os.LookupEnv` presence, not value comparison). Postgres/MSSQL/MySQL paths are untouched.

| Env var | SQLite default | Postgres default | Rationale |
|---|---|---|---|
| `DB_MAX_OPEN_CONNS` | 1 | 50 | SQLite is single-writer; extra conns are wasted slots. |
| `DB_MAX_IDLE_CONNS` | 1 | 10 | Match open conns. |
| `INGEST_PIPELINE_WORKERS` | 2 | 8 | Workers all serialise through the SQLite writer lock; 2 is enough to keep the queue non-empty. |
| `INGEST_PIPELINE_QUEUE_SIZE` | 10000 | 50000 | Lower heap watermark; backpressure kicks in earlier so OTLP clients back off. |
| `INGEST_PIPELINE_MAX_BYTES` | 128 MB | 512 MB | Item count alone cannot bound queue memory; one batch may carry MBs of spans/logs. |
| `GRAPHRAG_EVENT_QUEUE_SIZE` | 10000 | 100000 | Each queued event embeds a Span/Log by value (~0.5–2 KB); buffer less, drop sooner (metered). |
| `GRAPHRAG_TRACE_TTL` | 30m | 1h | The in-memory span window is the largest legitimate GraphRAG consumer; anomaly/investigation lookbacks are ≤5min. |
| `METRIC_MAX_CARDINALITY` | 3000 | 10000 | Bound the in-memory TSDB series map (no effect in aggregate mode). |
| `SAMPLING_RATE` | 0.05 | 1.0 | Errors and slow spans are always kept by `SAMPLING_ALWAYS_ON_ERRORS`. |
| `GRPC_MAX_CONCURRENT_STREAMS` | 240 | 1000 | ~2 streams per service at 120 services with headroom. |
| `LOG_FTS_ENABLED` | `true` | n/a | FTS5 BM25 is dramatically faster than LIKE on the kept `search_logs` path. |

Also at SQLite startup, `internal/storage/factory.go` applies a fail-closed PRAGMA stanza: `journal_mode=WAL`, `synchronous=NORMAL`, `temp_store=MEMORY`, `wal_autocheckpoint=10000`, `journal_size_limit=67108864` (64 MB WAL cap), `busy_timeout=5000`, plus **budget-scaled** memory knobs: page cache = budget/32 clamped to [64 MB, 256 MB] and mmap = budget/8 clamped to [256 MB, 1 GB], where the budget comes from `internal/membudget` (cgroup v2 → v1 → /proc/meminfo; a 4 GB host gets 128 MB cache + 512 MB mmap, detection failure falls back to the 256 MB/1 GB ceilings). Operators override with `SQLITE_CACHE_SIZE_KB` / `SQLITE_MMAP_SIZE_BYTES`. With the pure-Go driver the page cache is Go-heap memory and competes with `GOMEMLIMIT`. `PRAGMA auto_vacuum=INCREMENTAL` is attempted best-effort **before** the WAL switch (the WAL header freezes the stored mode; only affects newly created DB files). Any pragma failure in the fail-closed stanza aborts startup with a wrapped error — these are not optional. See `docs/superpowers/specs/2026-05-24-mcp-7tool-sqlite-survival-design.md` for per-default reasoning.

### Authentication

Two credential classes, one authenticator (`internal/authn/`), three transports.

**Operator key (`API_KEY`).** Gates `/api/*`, OTLP HTTP (`/v1/*`), the MCP endpoint, `/ws*`, and OTLP gRPC via `Authorization: Bearer <API_KEY>`. It authenticates and nothing more: tenant selection keeps its historical precedence (`X-Tenant-ID` / `x-tenant-id` → trusted `tenant.id` resource attribute → `DEFAULT_TENANT`), so a single-tenant install behaves exactly as before. When empty the middleware is a pass-through (dev only). Always-unprotected paths: `/live`, `/ready`, `/health*`, `/metrics*`, and the UI bundle.

**Tenant keys (`API_TENANT_KEYS_FILE`).** JSON or YAML chosen by extension, mapping bearer key → tenant ID, several keys per tenant (rotation, per-agent keys):

```json
{"3f7c…": "acme", "9b21…": "acme", "c40d…": "beta"}
```

Loaded once at startup — a key-file swap is an explicit restart, never a live reload. The process keeps only SHA-256 digests; lookup digests the presented key and compares with `subtle.ConstantTimeCompare` against every entry without exiting early. Startup refuses: unreadable files, files with any group/other permission bit (the refusal quotes the actual mode), unknown extensions, empty files, empty keys, keys containing whitespace or control characters, tenant IDs the storage sanitizer rejects, and duplicate keys. Credentials never appear in a log line or an error message at any level.

A tenant key **binds** the request or connection. Client-asserted tenancy — `X-Tenant-ID`, the `?tenant=` WebSocket parameter, `x-tenant-id` gRPC metadata, and the `tenant.id` OTLP resource attribute — is ignored and counted on `OtelContext_auth_tenant_conflicts_total{surface,reason}`. A non-zero rate means a client is asserting a tenancy it does not hold.

**WebSocket (`/ws*`).** The handshake authenticates (`internal/api/ws_auth.go`) once any credential source is configured. Carriers: `Authorization: Bearer <key>` for non-browser clients, or a `Sec-WebSocket-Protocol` entry `otelcontext.v1, auth.<base64url-token>` for browsers, which cannot set headers on a WebSocket. The server validates the token and echoes **only** `otelcontext.v1`; the protocol header value is never logged. Tokens are never accepted in query strings. One tenant scope per connection: a tenant-key socket is bound, an operator socket selects exactly one tenant via `?tenant=` or `X-Tenant-ID` and otherwise gets `DEFAULT_TENANT`. There is no merged all-tenant stream. Event delivery in both hubs is filtered by that scope, and an event carrying no tenant is invisible to a scoped socket (fail closed). `WS_ALLOWED_ORIGINS` is enforced whenever authentication is enabled or `APP_ENV=production` (empty list = same-host only); a request with no `Origin` header is a non-browser client and still has to authenticate. Both hubs cap admission at `WS_MAX_CLIENTS`, and the event hub now writes through one bounded queue (256 messages) and one writer goroutine per client — **overflow disconnects the client** rather than dropping messages, because the log/metric batches are incremental and a silent gap would misreport as "nothing happened". A reconnect re-seeds with a full snapshot.

**OTLP gRPC.** Unary and stream interceptors (`internal/ingest/grpc_auth.go`) read `authorization` metadata against the same key store. Refusals are `codes.Unauthenticated` with a non-specific message (a prober learns nothing) and are counted on `OtelContext_grpc_auth_failures_total{reason}`. Reflection is disabled when `APP_ENV=production` unless `GRPC_REFLECTION=true`.

**Production fail-closed startup.** With `APP_ENV=production`, startup refuses unless the OTLP gRPC listener has **both** transport protection (`TLS_CERT_FILE`/`TLS_KEY_FILE` or `TLS_AUTO_SELFSIGNED=true`) and authentication (`API_KEY` or `API_TENANT_KEYS_FILE`). Two waivers, each named in the refusal: `AUTH_TRUST_EXTERNAL=true` (the proxy terminates TLS and authenticates) or `OTELCONTEXT_ALLOW_INSECURE_GRPC=true` (explicit acknowledgement that telemetry crosses the network unprotected).

**`AUTH_TRUST_EXTERNAL` — mandatory proxy contract.** The flag trusts a tenant identity injected by a front proxy in `AUTH_EXTERNAL_TENANT_HEADER` (default `X-OtelContext-Tenant`; the gRPC metadata key is its lower-cased form). It is never blind trust of `X-Tenant-ID`, which stays client-controlled and untrusted. All of the following are mandatory deployment conditions:

1. The proxy authenticates every caller (mTLS, OIDC, or its own credential check) before forwarding.
2. The proxy **strips inbound copies** of the identity header from client requests and injects its own verified value. A configuration that merely appends is a bypass.
3. The application ports (HTTP and gRPC) are unreachable except through the proxy — network policy, listener binding, or both.
4. TLS is terminated at the proxy, and the proxy-to-application hop is on a trusted network or separately protected.
5. The proxy forwards the WebSocket upgrade (`Connection`/`Upgrade`, and the `Sec-WebSocket-Protocol` entries) with the identity header intact.
6. The proxy forwards gRPC metadata, injecting the identity key on the gRPC path too.

**Without every one of those conditions, `AUTH_TRUST_EXTERNAL=true` is an authentication bypass wearing infrastructure terminology.** Anything that can reach the port becomes any tenant it names.

**Database auth (Azure Entra).** Setting `DB_AZURE_AUTH=true` enables Azure Entra ID (AAD) authentication for PostgreSQL. The driver uses `DefaultAzureCredential`, which resolves identity via the standard probe order (env vars → workload identity → managed identity → Azure CLI → developer credentials). When Azure auth is enabled, strict TLS (`sslmode=require`, `verify-ca`, or `verify-full`) is mandatory; weaker modes are rejected at startup. `DB_CONN_MAX_LIFETIME` is internally capped to 30 minutes to stay inside the token TTL.

### Retention & Maintenance

The `RetentionScheduler` in `internal/storage/` runs an hourly batched purge of data older than `HOT_RETENTION_DAYS` via `PurgeLogsBatched`, `PurgeTracesBatched`, and `PurgeMetricBucketsBatched`, plus a daily maintenance pass: `PRAGMA optimize` and `PRAGMA incremental_vacuum(10000)` on SQLite (the historical full `VACUUM` held an exclusive whole-DB lock for 10–60 min on multi-GB files, starving ingest into a 429 storm; restore it with `RETENTION_FULL_VACUUM=true` or run `POST /api/admin/vacuum` on demand — note pre-existing DB files keep their `auto_vacuum` mode until a manual full VACUUM rewrites them, so `incremental_vacuum` no-ops harmlessly there), `ANALYZE`-equivalent maintenance on other drivers as before. Purge is **cross-tenant** — it scopes by age, not `tenant_id`. Valid `HOT_RETENTION_DAYS` is clamped to the range 1..36500.

Failure-mode gauges (prefix `OtelContext_`):
- `retention_consecutive_failures` — reset to 0 on success; alert when > 3
- `retention_last_success_timestamp` — Unix seconds; alert when stale relative to the hourly tick
- `retention_rows_purged_total`, `retention_purge_duration_seconds`, `retention_vacuum_duration_seconds` — throughput and latency

### Data Disk Budget — 8 GiB (#201)

The platform targets a single 8 GiB data volume. The budget is a promise about
that volume, not about a table, so **enforcement reads `statfs` on
`DATA_DISK_PATH`** — the only figure that also counts WAL frames, SQLite temp
files, free pages the file has not handed back, and anything else sharing the
volume. Per-component file sizes are attribution, never enforcement: a budget
enforced against summed file sizes reports 60% while `write()` returns ENOSPC.

| Tier | Allocation | Covers |
|---|---|---|
| Main relational tier | **4.5 GiB** | Raw trace/span/log exemplars, synthesized logs, investigations and other main-DB metadata, indexes, FTS5, database free pages |
| `aggregate.db` | **1.5 GiB** | Aggregate buckets, delta log, baselines, identity tables, and their indexes |
| DLQ | **0.5 GiB** | Existing `DLQ_MAX_DISK_MB` cap |
| WAL/SHM + temp | **0.5 GiB** | `-wal`/`-shm` sidecars of both databases, SQLite temp files, TLS material, transient maintenance overhead |
| Headroom | **1 GiB** | Mandatory and unused |

**Unused allocation in one tier does not authorize another tier to consume the
final 1 GiB.** The seven-day gate (#202) validates these numbers against
measured high-water marks; it does not quietly reallocate them after a failure.

Gauges: `otelcontext_disk_budget_bytes` (the effective ceiling — min of
`DATA_DISK_BUDGET_MB` and the usable volume capacity),
`otelcontext_disk_used_bytes`, `otelcontext_disk_used_ratio`,
`otelcontext_disk_component_bytes{component}` and
`otelcontext_disk_component_high_water_bytes{component}` for
`main_db|aggregate_db|dlq|wal`, `otelcontext_disk_shedding_state`,
`otelcontext_disk_shedding_transitions_total{from,to}`.

#### Exemplar-tier retention

`EXEMPLAR_RETENTION_DAYS=2` runs a **separate transactional purge** of exemplar
traces, spans, logs, their FTS rows and expired weak references (spans whose
trace row is gone), ahead of the `HOT_RETENTION_DAYS` purge on the same hourly
tick. Trace and span deletes share one transaction per batch, so a reader never
sees spans whose trace row has already gone. `logs_fts` is content-linked and
trigger-synced, so index entries die with their rows. Aggregate retention stays
seven days. Wired only in `AGGREGATE_MODE=aggregate`: in legacy and shadow the
raw rows ARE the dataset and a two-day purge would be data loss.

Arithmetic behind the 3 MiB default: two days = 576 five-minute windows;
576 × 3 MiB = 1.69 GiB of charged payload; at the provisional 2× DB/index/FTS
amplification ≈ 3.38 GiB, leaving ≈ 1.12 GiB of margin inside the 4.5 GiB main
tier. 4 MiB/window consumes the whole tier under the same optimistic assumption
— it stays configurable, it is not the default until #202 proves it fits.
Throughput: `otelcontext_exemplar_rows_purged_total{table}`,
`otelcontext_exemplar_purge_duration_seconds`.

#### Synthesized-log metering

Every log synthesized from a span event or span status reserves
`len(body) + len(attributesJSON) + logRowFixedBytes` against its trace's
per-trace budget AND the shared per-service/global window budgets, under
`EXEMPLAR_SYNTH_LOGS_PER_SPAN` and `EXEMPLAR_SYNTH_LOGS_PER_TRACE`. They do
**not** consume the ordinary log-exemplar quota — that budget is for logs a
client actually sent — but they are not weightless either: a span carrying two
hundred exception events used to write two hundred rows no budget had ever
seen. Refusals drop the log, count
`otelcontext_exemplar_dropped_total{signal="logs",reason}` with
`synth_per_span|synth_per_trace|budget_bytes`, and stamp the trace `truncated`.

#### Reservation lifecycle (no unconditional refunds)

Bytes are **reserved** before a row is constructed, **committed** when the
primary queue or the DLQ accepts the batch, and **released** only when the row
is dropped before submission or permanently lost because both destinations
refused it. Reserved bytes bind the cap exactly like committed ones. Once a
submission is accepted the charge is **monotonic for that window**: displacing
the trace later releases the count slot (a slot is a seat, not a byte) and
never the bytes — refunding bytes already on disk is how a window writes past
its cap. `Batch.Reservation` carries the charge to the submit boundary.

#### Staged shedding, hysteresis, and the one ENOSPC exception

| Volume usage | State | Behaviour |
|---|---|---|
| ≥ 90% | `errors_only` | Only error trace/log exemplars are admitted; healthy/slow/WARN raw retention off |
| ≥ 95% | `raw_off` | ALL new raw exemplar admission off, exemplar DLQ fallback closed, immediate expired-exemplar purge + `wal_checkpoint(TRUNCATE)`, `/ready` → 503 |

Hysteresis: recover from `raw_off` only below 90%, from `errors_only` only
below 85%. A failed `statfs` HOLDS the current state — shedding because a
syscall failed would be an outage caused by the safety mechanism.

Raw shedding **never** turns a successful aggregate Export into a retryable
failure. One exception: if the **authoritative** aggregate commit fails with
`ENOSPC` or `SQLITE_FULL` (`aggregate.IsDiskFull`), the Export MUST fail with
`RESOURCE_EXHAUSTED`/429 — under the durable-ACK contract a success response
asserts the deltas are committed, and acknowledging data that was not stored is
data loss with better branding. Shadow mode is unaffected: there the legacy raw
path is still the source of truth.

### Health probes — `/live` and `/ready`

`/live` is **process-only** and has no dependencies: it answers 200 as long as
the process is up. Nothing below can make it fail. Killing a process because
its store went unreachable throws away the in-memory shards and buys a delta-log
replay; that is a worse outage than the one it was reacting to.

`/ready` is the dependency probe. It answers 200 only when every named check
below passes and 503 with the full per-check breakdown otherwise. **Check names
are an operator contract** (asserted by the #202 gate): they are stable, and
each runtime probe carries its measured number in the payload so an alert can
be built on a figure rather than on a word.

| Check | Source | 503 when |
|---|---|---|
| `database` | main relational DB `PingContext` (2s) | ping fails |
| `graphrag` | coordinator | not running (`skipped` when unconfigured) |
| `dlq_disk` | DLQ bytes ÷ `DLQ_MAX_DISK_MB` | ≥ 0.95 |
| `pipeline` | ingest queue depth ÷ capacity | ≥ 0.95 |
| `aggregate_store` | `RecoveryGate` | delta-log replay not finished (#173) |
| `disk` | disk watchdog state (#201 Q5) | `raw_off` |
| `aggregate_db` | aggregate store **read pool** ping (2s) | ping fails |
| `aggregate_commit` | consecutive group-commit failures | streak ≥ `READY_MAX_COMMIT_FAILURE_STREAK` |
| `aggregate_finalizer` | consecutive window-finalize failures | streak ≥ `READY_MAX_FINALIZE_FAILURE_STREAK` |
| `aggregate_admission` | fullest of pending bytes / pending deltas / waiters ÷ their bounds | ≥ `READY_MAX_ADMISSION_RATIO` |
| `aggregate_delta_log` | age of the oldest un-finalized window | ≥ `READY_MAX_DELTA_LOG_AGE_S` seconds |
| `aggregate_disk` | `aggregate.db` bytes ÷ `READY_AGGREGATE_DISK_BUDGET_MB` | ≥ `READY_MAX_AGGREGATE_DISK_RATIO` |

The six `aggregate_*` runtime probes (#194 finding 18) are **degraded-not-dead**:
each flips `/ready` to 503, none touches `/live`, none stops the process, and
each recovers on its own the moment its signal does. `aggregate_store` gates
startup; the rest cover the ways a process that finished recovery later stops
being able to serve. Every check reports `skipped` when its source is not
configured (`AGGREGATE_MODE=legacy`, no DLQ cap, no watchdog), so a legacy
deployment's readiness is unchanged.

Runtime signals are read from counters the group-commit writer already keeps —
including the delta-log backlog sample the finalize loop publishes — so a
readiness request never queries the aggregate store and never queues behind the
single SQLite writer. The one exception is `aggregate_db`, which pings the
**read** pool with a 2s deadline for the same reason: the writer pool is
`MaxOpenConns(1)` behind the group commit, and a ping issued there would report
"unreachable" for a database that is merely busy. The delta-log age carries the
staleness of its sample (age + time since the sample was taken), so a wedged
finalize loop cannot look healthy by simply never refreshing the number.

Thresholds (`0` disables that probe):

| Env var | Default | Rationale |
|---|---|---|
| `READY_MAX_COMMIT_FAILURE_STREAK` | 3 | One failed commit is a retry; three in a row is a store that stopped accepting writes. |
| `READY_MAX_FINALIZE_FAILURE_STREAK` | 3 | Same shape, on the finalizer. |
| `READY_MAX_ADMISSION_RATIO` | 0.9 | Below the 0.95 the DLQ/pipeline probes use: the writer's admission bound is what turns an Export into `RESOURCE_EXHAUSTED`, so readiness says "stop sending" before clients are refused, not while they are. |
| `READY_MAX_DELTA_LOG_AGE_S` | 1800 | 2× (`WindowSize` 5m + `AllowedLateness` 10m). A window is finalizable 900s after it opens, so a healthy oldest entry tops out just past 900s plus one finalize tick. |
| `READY_AGGREGATE_DISK_BUDGET_MB` | 1536 | `aggregate.db`'s share of the 8 GiB data budget (#201 Q1). The disk watchdog enforces the **volume**; this enforces the **tier**, so a runaway aggregate file is visible before it eats another tier's allocation. |
| `READY_MAX_AGGREGATE_DISK_RATIO` | 0.9 | Warn inside the tier before the volume-level ladder starts shedding. |

## Security & Supply Chain

OtelContext targets the OpenSSF Best Practices `passing` badge (project [12646](https://www.bestpractices.dev/en/projects/12646)) and ships a six-job OSS-CLI security stack, supplemented by **SonarCloud SAST as a required gate** (board reversal 2026-04-28). No CodeQL, no NVD-direct tooling. Cost: $0 for the OSS-CLI tier; SonarCloud is free for public repos.

### OSS-CLI security stack (`.github/workflows/security.yml`)

| Concern | Tool | Gate |
|---|---|---|
| SCA (Go modules + npm) | OSV-Scanner against `go.mod` + `ui/package-lock.json` (OSV.dev / GHSA / ecosystem feeds; **not NVD**) | Block merge on High/Critical |
| SCA (filesystem + OS) + container scan | Trivy filesystem scan; Dependabot surfaces advisories on the Security tab | Block merge on `severity: HIGH,CRITICAL`, `exit-code: 1`, `ignore-unfixed: true` |
| SAST | Semgrep (`p/security-audit` + `p/owasp-top-ten` + `p/golang`) | Block merge on `--severity ERROR` |
| Secret scan | Gitleaks (full git history) | Block merge on any finding |
| Duplication | jscpd, threshold 3%, `--min-tokens 100`, scoped to `internal/` + `ui/src/`, excludes tests, vendor, build artifacts, and the legacy `internal/graph/` package | Block merge above threshold |
| SBOM | `anchore/sbom-action` (SPDX + CycloneDX) | Surface as 90-day artifact; do **not** gate merge |
| Lint (Go) | `golangci-lint` (existing `.golangci.yml`) | Wired into `ci.yml`, not security.yml |

All actions are SHA-pinned per Scorecard `Pinned-Dependencies`. Top-level `permissions: read-all`; jobs scope up only when needed (gitleaks needs full history; sbom uploads).

**Required external gate:** SonarCloud Code Analysis. Runs as the SonarCloud GitHub App (no in-repo workflow); listed in `main` branch protection's `required_status_checks` since 2026-04-28. Reinstated by board reversal — earlier docs that said "do not re-introduce" are superseded.

**Not used (do not re-introduce without an explicit board reversal):** CodeQL (GHAS-paid for non-public repos), OWASP Dependency-Check (or any NVD-direct tool — NVD has analysis-backlog and rate-limit reliability problems).

### OpenSSF Scorecard (`.github/workflows/scorecard.yml`)

- **Schedule:** push to `main` + Mondays 06:00 UTC + manual `workflow_dispatch`.
- **Output:** SARIF → Security tab; results published to public Scorecard dashboard.
- **Hardening:** `step-security/harden-runner` (egress: audit), `actions/checkout` with `persist-credentials: false`.
- **Baseline:** to be measured after first push to `main`. Track via the Scorecard dashboard linked from the README badge.
- **Stretch target:** ≥ 8.0/10. Best-effort — Scorecard does **not** gate merge per the board ruling. The `passing` Best Practices badge is the only hard supply-chain gate.

### Release artifacts & signing (`.github/workflows/release.yml` + `.goreleaser.yaml`)

Triggered by a `v*` tag push (the tag is cut by `scripts/release.sh`). GoReleaser OSS v2 builds cross-platform binaries (linux/darwin × amd64/arm64, `CGO_ENABLED=0` for the pure-Go SQLite driver, `-trimpath -s -w`), emits `tar.gz` archives (incl. LICENSE.md + README.md), a sha256 `checksums.txt`, and per-archive SBOMs via syft. **cosign keyless** (Sigstore OIDC; the release job grants `id-token: write`) signs the checksums file, attaching `checksums.txt.sig` + `checksums.txt.pem` to the release. This is what makes Scorecard's **Packaging** (a publishing workflow is detected) and **Signed-Releases** checks score.

Division of labour: `scripts/release.sh` builds the UI + pushes the tag (and, with `--release`, creates the GitHub release shell with notes but **no artifacts**); `release.yml` then runs GoReleaser with `release.mode: append`, which adds the signed artifacts to that same release without racing on "release already exists". All third-party actions are SHA-pinned per Scorecard `Pinned-Dependencies` (pins reused from `security.yml`/`scorecard.yml` where they overlap). $0/OSS-only: GoReleaser OSS, cosign (Apache-2.0), syft (Apache-2.0) — no Pro-only config keys.

> Caveat: Packaging/Signed-Releases only **score** after the next `v*` tag-push actually runs `release.yml` and produces a signed release.

### Vulnerability reporting

See [`SECURITY.md`](SECURITY.md). Preferred channel: GitHub Security Advisories at `https://github.com/RandomCodeSpace/otelcontext/security/advisories/new`. Email fallback: `ak.nitrr13@gmail.com` with subject prefix `[otelcontext security]`.

### Signed commits & branch protection

- Repo-local config helper: [`scripts/setup-git-signed.sh`](scripts/setup-git-signed.sh) — supports ssh, openpgp, and x509 signing; honours the contributor's existing global git identity.
- Branch protection on `main` requiring signed commits is configured at the GitHub repo level (board-admin action; not file-driven). When toggled on, every commit landing on `main` must verify.

### Self-assessment evidence

- [`.bestpractices.json`](.bestpractices.json) — OpenSSF Best Practices evidence map (project 12646, level `passing`, six categories self-assessed). The badge level transition from `in_progress` → `passing` requires a board admin to log into bestpractices.dev with the OSS-Random identity.

## Build & Run

```bash
go build -o otelcontext .        # Build
./otelcontext                     # Run (default: SQLite, ports 4317/8080)
go vet ./...                      # Lint
go test ./...                     # Test
```
