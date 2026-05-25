# MCP 7-Tool Triage Surface + SQLite Survival Tuning

**Date:** 2026-05-24
**Branch:** `feat/mcp-7tool-sqlite-survival`
**Status:** Implementation
**Authors:** OtelContext platform team

## Problem statement

A production OtelContext deployment with 120 services ingesting OTel data on the
SQLite backend OOMs within 1 hour and grows the on-disk DB at roughly 2 TB/day.
The platform is not survivable on its default-recommended single-binary setup
once service count crosses ~20, well below the documented "small deployment"
guidance of "~5 services".

## Investigation summary

A 7-agent parallel investigation (5 Explore subagents, plus Codex/GPT-5 and
Antigravity/Gemini cross-checks) identified four primary OOM culprits:

1. **In-memory pipeline queue saturation under SQLite WAL contention.** The
   default `INGEST_PIPELINE_QUEUE_SIZE=50000` × per-batch dozens-of-KB
   payloads is sized for a Postgres deployment that can absorb 8 worker
   threads in parallel. SQLite's single-writer lock serializes everything
   into one writer, so the queue fills, retains all batches in heap, and
   the soft-backpressure 90% threshold never relieves pressure fast enough.
2. **GraphRAG permanent stores with no TTL.** `ServiceStore` and `SignalStore`
   are permanent; `AnomalyStore` is 24h. With 120 services × N operations
   × M log clusters × cross-service edges, the in-memory node count grows
   monotonically until heap pressure triggers full GC stalls.
3. **TSDB ring at default cardinality.** `METRIC_MAX_CARDINALITY=10000` is
   per-series, and with 120 services emitting heterogeneous attribute sets
   the in-memory ring buffer plus the series → bucket map dominates heap.
4. **Span AttributesJSON duplicating resource attributes on every row.**
   Compressed-text column is still tens-of-KB per span; resource attrs are
   ~80% of each row's payload and are duplicated unconditionally.

Secondary findings:

- The `vectordb` TF-IDF index is held entirely in memory (`maxSize=100000`
  documents × per-doc TF map + IDF table) and persists on a 5-minute snapshot
  loop. It accounts for ~5-15% of resident heap depending on log volume.
- The `graph_snapshots` table grows by ~67k rows/week at 100 tenants × 15-min
  cadence × N services, contributing meaningfully to the 2 TB/day disk
  growth on SQLite (every row carries a compressed JSON nodes+edges blob).
- 14 of the 21 MCP tools are operationally non-essential during a triage
  workflow — they wrap full-text trace search, dashboard stats, and
  investigation history that an LLM caller almost never reaches for
  inside an active incident response.

## Decision

Three coordinated changes, none of which touch GraphRAG core query logic,
TSDB core, or ingest pipeline core:

1. **Cut the 21-tool MCP surface to 7 triage-essential tools.** No
   deprecation period — production is already failing; the cut is
   immediate. Kept tools cover the full Linear-scan triage workflow
   (anomaly timeline → service map → root cause → impact → trace).
2. **Drop subsystems no longer reachable by any kept tool.** The
   `vectordb` package, the `graph_snapshots` GORM model + scheduler, and
   the `SimilarErrors` function (vectordb-dependent, no production caller)
   are deleted. Removing them reclaims heap on SQLite and stops the
   `graph_snapshots` row growth dead.
3. **Tune SQLite via PRAGMAs + per-driver config defaults.** Apply the
   community-standard WAL + 256 MB page-cache + 1 GB mmap pragmas at
   `gorm.Open`. Override eight config defaults when `DB_DRIVER=sqlite`
   so the rest of the platform stops pushing more load at SQLite than
   it can absorb. Postgres defaults are unchanged.

### 7-tool MCP triage surface (kept)

| Tool | Source | Why kept |
|---|---|---|
| `get_anomaly_timeline` | in-mem GraphRAG | The triage entry point — "what's wrong right now". |
| `get_service_map` | in-mem GraphRAG | Topology + health overlay drives every UI service-graph view. |
| `get_service_health` | in-mem GraphRAG | Per-service drill-down from the service map. |
| `root_cause_analysis` | in-mem GraphRAG | Ranked probable causes — the LLM's primary "why" tool. |
| `impact_analysis` | in-mem GraphRAG | Blast-radius for incident scoping. |
| `trace_graph` | in-mem GraphRAG (+ DB fallback) | Trace tree visualisation — the "show me the bad trace" path. |
| `search_logs` | DB (FTS5 default on SQLite, LIKE fallback) | The "show me the error logs around the incident" path. |

### Tools cut (14)

`get_system_graph`, `tail_logs`, `get_trace`, `search_traces`, `get_metrics`,
`get_dashboard_stats`, `get_storage_status`, `find_similar_logs`,
`get_alerts`, `correlated_signals`, `get_error_chains`, `get_investigations`,
`get_investigation`, `get_graph_snapshot`.

Rationale: each of these either (a) duplicates a kept tool with a slightly
different framing (`get_system_graph` ≈ `get_service_map`,
`get_error_chains` is folded into `root_cause_analysis`), (b) requires
subsystems being dropped (`find_similar_logs` → vectordb,
`get_graph_snapshot` → snapshot table), or (c) belongs to a separate
forensic-analytics workflow (`get_investigations`, `get_investigation`,
`get_dashboard_stats`) that is not part of active triage.

### Subsystem deletions

| Subsystem | Files / artifacts | Reason |
|---|---|---|
| `vectordb` package | `internal/vectordb/` (index.go, snapshot.go, replay.go + tests) | No surviving MCP tool consumes it; ~5-15% of heap; snapshot+replay loops are dead weight under triage workload. |
| Snapshot scheduler | `internal/graphrag/snapshot.go`; `GraphSnapshot` GORM model; snapshot loop in builder.go; `get_graph_snapshot` MCP tool already cut | `graph_snapshots` table is the second-largest disk-growth contributor after raw spans/logs. No kept tool reads it. |
| `SimilarErrors` | `internal/graphrag/clustering.go::SimilarErrors` | Vectordb-dependent, has no production caller, only used by the cut `find_similar_logs` tool path historically. |
| `/api/logs/similar` | `internal/api/similar_handler.go` + test | Same vectordb dependency; same triage non-essential. |
| `tools.go` cuts | 14 handler funcs deleted | One-line follow-on per dropped tool. |

### SQLite tuning

`internal/storage/factory.go` applies an 8-PRAGMA stanza (WAL mode, sync
NORMAL, 256 MB page cache, MEMORY temp store, 1 GB mmap, 10k-page
autocheckpoint, 64 MB WAL cap, 5s busy_timeout) immediately after
`gorm.Open` when the driver is SQLite. Any PRAGMA failure aborts startup
— these are not optional, and silent fallback to defaults defeats the
survivability goal. CLAUDE.md "SQLite PRAGMA stanza" enumerates each
PRAGMA with its rationale.

### Per-driver config defaults

When `DB_DRIVER=sqlite`, `config.Load()` overrides nine defaults that are
otherwise Postgres-tuned. The override applies only when the operator did
not set the env var explicitly (detected via `os.LookupEnv` presence, not
value comparison). The authoritative table — env var, SQLite default,
Postgres default, and per-row rationale — lives in `CLAUDE.md` under
"SQLite per-driver defaults". The implementation in
`internal/config/config.go::applyDriverDefaults` and its tests in
`internal/config/driver_defaults_test.go` are the runtime source of
truth.

### `search_logs` backend swap

The kept `search_logs` MCP tool drops the vectordb dispatch branch entirely
(the dispatch was previously vectordb-first for free-form text queries on
SQLite). On SQLite the path is FTS5-when-enabled-else-LIKE; both honour the
existing 24h time-window clamp.

## Migration notes for existing DBs

- **`graph_snapshots` table is left in place.** AutoMigrate stops *creating*
  it on fresh deploys (the model is deleted) but existing tables are not
  dropped. Operators on populated SQLite DBs can reclaim disk with
  `DROP TABLE graph_snapshots; VACUUM;` after upgrade.
- **`vectordb.snapshot` file is left in place.** The hydration code that
  reads it at boot is deleted, so it becomes a stale file in `data/`. Safe
  to delete by hand.
- **No schema changes to traces, spans, logs, metric_buckets, investigations,
  drain_templates.** All historical data remains queryable via the kept
  MCP surface.
- **MCP clients calling cut tools will receive an `unknown tool` RPC error.**
  No graceful degradation; the cut is intentional and immediate.

## Risk + mitigation table

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Cut tool was actually load-bearing for some user's workflow | Low | Medium | The kept 7 cover all triage paths; forensic workflows can use the SQL DB directly or wait for re-introduction with a clearer scope. |
| FTS5 default-on bumps SQLite disk by 30-40% | Medium | Low | Documented opt-out (`LOG_FTS_ENABLED=false`) + `POST /api/admin/drop_fts` reclaim path already exists. |
| SQLite `synchronous=NORMAL` + `mmap_size=1GB` is more sensitive to host OOM-kill | Low | Medium | These are the SQLite community's standard "make it survive write-heavy workloads" pragmas; the alternative (silent throughput collapse) is strictly worse. |
| `STORE_MIN_SEVERITY=WARN` default surprises an operator who needs INFO logs persisted | Medium | Low | Documented in `.env.example` + `CLAUDE.md`; setting `STORE_MIN_SEVERITY=INFO` explicitly restores legacy behaviour. |
| `SAMPLING_RATE=0.05` default loses too many spans for some debugging | Medium | Low | Always-on errors + slow spans are preserved (existing config); 5% normal-path sampling still gives enough signal for triage. Operator can set `SAMPLING_RATE=1.0` to revert. |
| Deleted `graph_snapshots` causes existing UI views to break | Low | Medium | No UI view consumes the table — verified by grep before cut. |

## Acceptance criterion

Survives 120 services on SQLite for 7-day continuous load without OOM and
without disk growth exceeding the documented hot retention (7d × ~50 GB/d
after sampling and STORE_MIN_SEVERITY = ~350 GB steady-state, down from
~14 TB unbounded growth).

## Commit structure

Five logical commits on `feat/mcp-7tool-sqlite-survival`:

1. `refactor(mcp): drop 14 non-triage tools, keep 7-tool triage surface`
2. `refactor(vectordb): drop package; FTS5 + recent-N-in-cluster replace semantic similarity`
3. `refactor(graphrag): drop graph_snapshots table + scheduler`
4. `feat(sqlite): PRAGMA tuning + per-driver config defaults for 120-service survival`
5. `docs: 7-tool MCP surface + SQLite operator notes`

## Verification

`gofmt -l .`, `go vet ./...`, `go build .`, `go test ./...`, and a UI
`npm install && npm run build && npm test -- --run` pass before each
commit lands.
