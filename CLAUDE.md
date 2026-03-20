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
                               (TSDB Ring,           (Source of Truth)
                                GraphRAG,                  │
                                Vector)                    ▼
                                     │              Cold Archive
HTTP :8080 ◄── REST API ◄───────────┘              (zstd JSONL)
           ◄── WebSocket (real-time)
           ◄── MCP Server (AI agents, 22 tools)
           ◄── Prometheus /metrics
```

## Ingestion Paths

| Path | Endpoint | Content Types | Notes |
|------|----------|---------------|-------|
| gRPC | `:4317` | protobuf | Traces, Logs, Metrics via OTLP gRPC |
| HTTP | `/v1/traces`, `/v1/logs`, `/v1/metrics` | `application/x-protobuf`, `application/json` | OTLP HTTP spec compliant, gzip support, 4MB limit |

Both paths delegate to the same `Export()` methods — zero business logic duplication.

## Storage Architecture

| Layer | Package | Purpose |
|-------|---------|---------|
| GraphRAG (in-memory) | `internal/graphrag/` | Layered graph: 4 typed stores, error chains, root cause analysis, anomaly detection |
| Time Series (in-memory) | `internal/tsdb/` | Ring buffer, sliding windows, pre-computed percentiles |
| Graph (in-memory, legacy) | `internal/graph/` | Simple service topology — **being replaced by GraphRAG** |
| Vector (embedded) | `internal/vectordb/` | TF-IDF index for semantic log search (pure Go, no CGO) |
| Relational (persistent) | `internal/storage/` | GORM-based, multi-DB, single source of truth |
| Cold Archive | `internal/archive/` | Zstd-compressed JSONL on local disk (7+ day old data) |

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

### Ingestion Callbacks
```
TraceServer.Export() → DB persist → spanCallback → GraphRAG.OnSpanIngested()
LogsServer.Export()  → DB persist → logCallback  → GraphRAG.OnLogIngested()
MetricsServer.Export() → TSDB    → metricCallback → GraphRAG.OnMetricIngested()
```

## MCP Server — 22 Tools

The MCP server (`internal/mcp/`) exposes tools via HTTP Streamable MCP (JSON-RPC 2.0 POST + SSE GET).

### Legacy Tools (12)
`get_system_graph`, `get_service_health`, `search_logs`, `tail_logs`, `get_trace`, `search_traces`, `get_metrics`, `get_dashboard_stats`, `get_storage_status`, `find_similar_logs`, `get_alerts`, `search_cold_archive`

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
3. TSDB + Archiver + Graph + GraphRAG — stop processing
4. DLQ — stop replay
5. DB `Close()` — close database last

## Key Directories

```
internal/
  ai/           # AI service integration
  api/          # HTTP handlers, middleware, rate limiting, graph_handler
  archive/      # Hot/cold storage archival
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
    clustering.go   # Log clustering via hash + vectordb similarity
    refresh.go      # Periodic DB rebuild + pruning
  ingest/       # OTLP receivers (gRPC + HTTP), adaptive sampling
    otlp.go         # gRPC TraceServer, LogsServer, MetricsServer
    otlp_http.go    # HTTP OTLP handler (protobuf + JSON, gzip, 4MB limit)
    sampler.go      # Per-service token bucket sampler
  mcp/          # MCP server (22 tools, JSON-RPC 2.0 + SSE)
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
- `HOT_RETENTION_DAYS` (7), `COLD_STORAGE_PATH`, `ARCHIVE_SCHEDULE_HOUR`
- `SAMPLING_RATE` (1.0), `SAMPLING_ALWAYS_ON_ERRORS` (true), `SAMPLING_LATENCY_THRESHOLD_MS` (500)
- `METRIC_MAX_CARDINALITY` (10000), `API_RATE_LIMIT_RPS` (100)
- `MCP_ENABLED` (true), `MCP_PATH` (/mcp)
- `VECTOR_INDEX_MAX_ENTRIES` (100000)
- `DLQ_MAX_FILES` (1000), `DLQ_MAX_DISK_MB` (500), `DLQ_MAX_RETRIES` (10)

## Build & Run

```bash
go build -o otelcontext .        # Build
./otelcontext                     # Run (default: SQLite, ports 4317/8080)
go vet ./...                      # Lint
go test ./...                     # Test
```
