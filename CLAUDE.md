# OtelContext — AI Agent Instructions

## Project Overview

OtelContext is a self-hosted OTLP observability platform. Single Go binary with embedded React frontend.
- **Backend:** Go 1.25, native `net/http` (no frameworks), GORM ORM, gRPC + HTTP for OTLP ingestion
- **Frontend:** React 19 + TypeScript + Mantine UI v8 + ECharts + ReactFlow
- **Ports:** gRPC `:4317` (OTLP), HTTP `:8080` (API + HTTP OTLP + WebSocket + UI)

## Strict Rules

- NO Express.js/Gin/Echo — use native Go `net/http`
- NO Tailwind CSS — use Mantine UI v8 exclusively
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
                                GraphRAG,             7-15 day retention)
                                Vector)
                                     │
HTTP :8080 ◄── REST API ◄───────────┘
           ◄── WebSocket (real-time)
           ◄── MCP Server (AI agents, 21 tools)
           ◄── Prometheus /metrics
```

## Ingestion Paths

| Path | Endpoint | Content Types | Notes |
|------|----------|---------------|-------|
| gRPC | `:4317` | protobuf | Traces, Logs, Metrics via OTLP gRPC |
| HTTP | `/v1/traces`, `/v1/logs`, `/v1/metrics` | `application/x-protobuf`, `application/json` | OTLP HTTP spec compliant, gzip support, 4MB limit. Returns `429 Too Many Requests` + `Retry-After: 1` when the async pipeline queue is full (parity with gRPC `RESOURCE_EXHAUSTED`). |

Both paths delegate to the same `Export()` methods — zero business logic duplication. By default `Export()` parses the OTLP request and hands a `Batch` to the async ingest `Pipeline` (`internal/ingest/pipeline.go`); a worker pool persists Trace→Span→Log in order. With `INGEST_ASYNC_ENABLED=false` the pipeline is bypassed and `Export()` writes inline (legacy path).

### Multi-tenancy

Tenant identity flows into the request context on every write and read:
- **HTTP:** `X-Tenant-ID` header (see `internal/api/tenant_middleware.go`).
- **gRPC:** `x-tenant-id` metadata key (see `internal/ingest/otlp.go`).
- **OTLP resource attribute:** `tenant.id` on the resource overrides the header/metadata.

When none are present, `DEFAULT_TENANT` (default `"default"`) is assigned. Every row in the relational DB carries a `tenant_id` column; every read method in `internal/storage/` scopes by the tenant in the request context (`Where("tenant_id = ?", tenant)`). Retention (`RetentionScheduler`) is **cross-tenant** — it purges by age, not by tenant.

## Storage Architecture

| Layer | Package | Purpose |
|-------|---------|---------|
| GraphRAG (in-memory) | `internal/graphrag/` | Layered graph: 4 typed stores, error chains, root cause analysis, anomaly detection |
| Time Series (in-memory) | `internal/tsdb/` | Ring buffer, sliding windows, pre-computed percentiles |
| Graph (in-memory, legacy) | `internal/graph/` | Simple service topology — **being replaced by GraphRAG** |
| Vector (embedded) | `internal/vectordb/` | TF-IDF index for semantic log search (pure Go, no CGO). Retained as a fallback similarity index for SQLite mode and for `SimilarErrors` ranking within a Drain template cluster. |
| Relational (persistent) | `internal/storage/` | GORM-based, multi-DB, single source of truth. Driven by `RetentionScheduler` (hourly batched purge + daily VACUUM/ANALYZE). `logs.body` is plain TEXT. **Log search**: SQLite uses FTS5 virtual table `logs_fts` (porter+unicode61 tokenizer) ordered by `bm25()`, kept in sync via AFTER INSERT/DELETE/UPDATE triggers; Postgres uses `pg_trgm` GIN on `logs.body` and `logs.service_name`. `AttributesJSON` and `AIInsight` remain `CompressedText`. |

## GraphRAG Architecture

The `internal/graphrag/` package is the core intelligence layer. It replaces the simple `internal/graph/` for advanced observability queries.

### Layered Stores (each with own `sync.RWMutex`)

| Store | Nodes | Edges | TTL |
|-------|-------|-------|-----|
| `ServiceStore` | ServiceNode, OperationNode | CALLS, EXPOSES | Permanent |
| `TraceStore` | TraceNode, SpanNode | CONTAINS, CHILD_OF | Configurable (default 1h) |
| `SignalStore` | LogClusterNode, MetricNode | EMITTED_BY, MEASURED_BY, LOGGED_DURING | Permanent |
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
| `SimilarErrors(clusterID, k)` | k-NN cosine similarity via vectordb | Related error patterns |
| `ServiceMap(depth)` | Full topology dump | Service topology + health |

### Background Processes
- **4 event workers** consume from a 10,000-capacity buffered channel (best-effort; DB is source of truth)
- **Refresh loop** (60s) — rebuilds from DB, prunes expired TraceStore nodes, cleans old anomalies
- **Snapshot loop** (15min) — persists topology snapshot to DB, prunes snapshots > 7 days
- **Anomaly loop** (10s) — detects error spikes, latency degradation, metric z-score anomalies

### Persistence Models (GORM)
- `Investigation` — automated error analysis records (trigger, root cause, causal chain, evidence)
- `GraphSnapshot` — periodic topology snapshots (nodes, edges, health scores)
- `DrainTemplateRow` — persisted Drain log templates (table `drain_templates`), loaded on startup to warm the miner

### Log Clustering (Drain)

Log clustering uses **Drain** template mining (`internal/graphrag/drain.go`) — a deterministic fixed-depth prefix tree with O(1) LRU via `container/list`. It replaces the older hash-based clustering. Templates are persisted to the `drain_templates` table and reloaded on startup so cluster IDs stay stable across restarts. The TF-IDF `vectordb` is retained as a fallback similarity ranker inside a template bucket (`SimilarErrors`).

### Ingestion Callbacks
```
TraceServer.Export() → DB persist → spanCallback → GraphRAG.OnSpanIngested()
LogsServer.Export()  → DB persist → logCallback  → GraphRAG.OnLogIngested()
MetricsServer.Export() → TSDB    → metricCallback → GraphRAG.OnMetricIngested()
```

## MCP Server — 21 Tools

The MCP server (`internal/mcp/`) exposes tools via HTTP Streamable MCP (JSON-RPC 2.0 POST + SSE GET).

### Legacy Tools (11)
`get_system_graph`, `get_service_health`, `search_logs`, `tail_logs`, `get_trace`, `search_traces`, `get_metrics`, `get_dashboard_stats`, `get_storage_status`, `find_similar_logs`, `get_alerts`

### GraphRAG Tools (10)
| Tool | Input | Source |
|------|-------|--------|
| `get_service_map` | `{depth?, service?}` | In-memory (instant) |
| `get_error_chains` | `{service, time_range?, limit?}` | In-memory + DB fallback |
| `trace_graph` | `{trace_id}` | In-memory + DB fallback |
| `impact_analysis` | `{service, depth?}` | In-memory (instant) |
| `root_cause_analysis` | `{service, time_range?}` | In-memory (instant) |
| `correlated_signals` | `{service, time_range?}` | In-memory + DB |
| `get_investigations` | `{service?, severity?, status?, limit?}` | DB query |
| `get_investigation` | `{investigation_id}` | DB query |
| `get_graph_snapshot` | `{time}` | DB query |
| `get_anomaly_timeline` | `{since?, service?}` | In-memory (instant) |

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

## Shutdown Order

Proper LIFO ordering to prevent data loss:
1. gRPC `GracefulStop()` + HTTP `Shutdown()` — stop ingestion
2. WebSocket Hub + Event Hub + AI Service — stop real-time
3. TSDB + Graph + GraphRAG — stop processing
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
    snapshot.go     # GORM GraphSnapshot model + scheduler
    anomaly.go      # Z-score, error spike, latency degradation detection
    drain.go        # Log clustering via Drain template mining — pure-Go, stdlib-only, deterministic fixed-depth prefix tree
    refresh.go      # Periodic DB rebuild + pruning
  ingest/       # OTLP receivers (gRPC + HTTP), adaptive sampling
    otlp.go         # gRPC TraceServer, LogsServer, MetricsServer
    otlp_http.go    # HTTP OTLP handler (protobuf + JSON, gzip, 4MB limit)
    sampler.go      # Per-service token bucket sampler
  mcp/          # MCP server (21 tools, JSON-RPC 2.0 + SSE)
  queue/        # Dead Letter Queue (typed envelopes, bounded disk, exp backoff)
  realtime/     # WebSocket hub + event streaming
  storage/      # GORM repository, models, migrations, Close() method
  telemetry/    # Prometheus metrics + health (19 metrics)
  tsdb/         # Time series aggregator + ring buffer (lock-free Windows())
  vectordb/     # Embedded TF-IDF vector index (FIFO eviction with copy, clean IDF rebuild)
  ui/           # Embedded React frontend
ui/             # React frontend (Vite + Mantine)
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
- `API_KEY` — Bearer token gate for `/api/*`, `/v1/*`, `/mcp`. Empty = auth disabled
- `OTEL_EXPORTER_OTLP_ENDPOINT` — enables self-instrumentation (empty = off)
- `DEFAULT_TENANT` (`default`) — assigned to rows ingested without explicit tenant
- `HOT_RETENTION_DAYS` (7) — drives `RetentionScheduler`; range 1..36500
- `SAMPLING_RATE` (1.0), `SAMPLING_ALWAYS_ON_ERRORS` (true), `SAMPLING_LATENCY_THRESHOLD_MS` (500)
- `METRIC_MAX_CARDINALITY` (10000), `METRIC_MAX_CARDINALITY_PER_TENANT` (0 = unlimited), `API_RATE_LIMIT_RPS` (100). The per-tenant cap is checked first; when set, a noisy tenant cannot exhaust the global pool. Overflow is labeled by tenant via `otelcontext_tsdb_cardinality_overflow_by_tenant_total{tenant_id}` (`__global__` sentinel when the global cap was the trigger).
- `MCP_ENABLED` (true), `MCP_PATH` (/mcp)
- `MCP_MAX_CONCURRENT` (32), `MCP_CALL_TIMEOUT_MS` (30000), `MCP_CACHE_TTL_MS` (5000) — MCP HTTP streamable robustness. Counting semaphore gates concurrent `tools/call` (JSON-RPC `-32000` past the cap), per-call deadlines abort runaway handlers (JSON-RPC `-32001`), and a 5s TTL cache memoizes the cheap in-memory GraphRAG tools (`get_service_map`, `impact_analysis`, `root_cause_analysis`, `get_anomaly_timeline`, `get_service_health`). SSE GET sends a `: keep-alive\n\n` comment every 25s to keep the stream alive across reverse-proxy idle timeouts. Set any to 0 to disable.
- `VECTOR_INDEX_MAX_ENTRIES` (100000)
- `DLQ_MAX_FILES` (1000), `DLQ_MAX_DISK_MB` (500), `DLQ_MAX_RETRIES` (10)
- `GRAPHRAG_WORKER_COUNT` (16), `GRAPHRAG_EVENT_QUEUE_SIZE` (100000) — sized for 100–200 services; raise further if `otelcontext_graphrag_events_dropped_total` climbs
- `INGEST_ASYNC_ENABLED` (true), `INGEST_PIPELINE_QUEUE_SIZE` (50000), `INGEST_PIPELINE_WORKERS` (8) — async ingest pipeline (`internal/ingest/pipeline.go`). Hybrid backpressure: <90% accept all, 90–100% drop healthy batches (errors/slow always pass), 100% return gRPC `RESOURCE_EXHAUSTED`. Set `INGEST_ASYNC_ENABLED=false` to revert to synchronous DB writes inside `Export()`. Drops surface as `otelcontext_ingest_pipeline_dropped_total{signal,reason}`.
- `GRPC_MAX_RECV_MB` (16), `GRPC_MAX_CONCURRENT_STREAMS` (1000) — OTLP gRPC server caps, validated to 1..256 and 1..1_000_000
- `RETENTION_BATCH_SIZE` (50000), `RETENTION_BATCH_SLEEP_MS` (1) — purge pacing; raise the sleep on busy production DBs
- `DB_POSTGRES_PARTITIONING` (`""`), `DB_PARTITION_LOOKAHEAD_DAYS` (3) — opt-in Postgres declarative range partitioning of the `logs` table by day. When `daily`, `logs` is provisioned as a partitioned parent (greenfield only — refuses to start if `logs` already exists unpartitioned), the `PartitionScheduler` maintains lookahead partitions and drops expired ones via `DROP TABLE`, and `RetentionScheduler` skips the row-level DELETE for `logs`. Watch `otelcontext_partitions_dropped_total` and `otelcontext_partitions_active`.
- `APP_ENV` (`"development"`), `OTELCONTEXT_ALLOW_SQLITE_PROD` (false) — SQLite is refused when `APP_ENV=production` unless the allow flag is set

### Authentication

**API auth (platform).** `API_KEY` gates `/api/*`, OTLP HTTP (`/v1/*`), and the MCP endpoint via `Authorization: Bearer <API_KEY>`. When empty, the middleware is a pass-through (dev only). Unprotected paths: `/live`, `/ready`, `/metrics*`, `/ws*`. A shared `API_KEY` grants access to every tenant — there is no per-tenant-key file in the current code; isolate tenants at the network/auth layer if that matters. (If an `API_TENANT_KEYS_FILE` override lands later, re-check `internal/api/auth.go` for the flag name.)

**Database auth (Azure Entra).** Setting `DB_AZURE_AUTH=true` enables Azure Entra ID (AAD) authentication for PostgreSQL. The driver uses `DefaultAzureCredential`, which resolves identity via the standard probe order (env vars → workload identity → managed identity → Azure CLI → developer credentials). When Azure auth is enabled, strict TLS (`sslmode=require`, `verify-ca`, or `verify-full`) is mandatory; weaker modes are rejected at startup. `DB_CONN_MAX_LIFETIME` is internally capped to 30 minutes to stay inside the token TTL.

### Retention & Maintenance

The `RetentionScheduler` in `internal/storage/` runs an hourly batched purge of data older than `HOT_RETENTION_DAYS` via `PurgeLogsBatched`, `PurgeTracesBatched`, and `PurgeMetricBucketsBatched`, plus a daily `VACUUM`/`ANALYZE` pass to reclaim space and refresh planner statistics. Purge is **cross-tenant** — it scopes by age, not `tenant_id`. Valid `HOT_RETENTION_DAYS` is clamped to the range 1..36500.

Failure-mode gauges (prefix `OtelContext_`):
- `retention_consecutive_failures` — reset to 0 on success; alert when > 3
- `retention_last_success_timestamp` — Unix seconds; alert when stale relative to the hourly tick
- `retention_rows_purged_total`, `retention_purge_duration_seconds`, `retention_vacuum_duration_seconds` — throughput and latency

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
