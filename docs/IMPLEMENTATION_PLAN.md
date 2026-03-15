# Argus Observability & Performance Improvement Plan

## Context

Argus is a self-hosted OTLP observability platform (Go backend + React frontend) that ingests traces, logs, and metrics via gRPC, stores them in a relational DB (SQLite/MySQL/PostgreSQL/MSSQL), and serves dashboards via HTTP/WebSocket. When connected to active services pushing continuous telemetry, the DB will grow unbounded and performance will degrade. The goal is to:

1. **Capture all metrics, traces, and logs** — comprehensively but smartly
2. **Create an AI-consumable system graph API** — structured JSON that any AI agent can query
3. **Maximize performance** — fix identified bottlenecks, use specialized in-memory DBs as accelerators
4. **Hot/cold storage tiering** — keep 7 days hot, archive older data to compressed cold storage
5. **Data compression** — prevent DB blowup from continuous ingestion
6. **HTTP Streamable MCP Server** — universal AI agent access to Argus tools

### Storage Architecture Principle

Specialized in-memory/embedded databases are used as **processing accelerators** (hot caches/indexes). The relational DB (SQLite/MySQL/PostgreSQL/MSSQL) remains the **single source of truth**. All specialized stores are rebuildable from the relational DB at any time. All internal DBs are **embedded** — compiled into the single binary, no external processes.

| Layer | Purpose | Implementation | Feeds Back To |
|-------|---------|---------------|---------------|
| **Time Series (in-memory)** | Metrics aggregation, real-time dashboards, sliding windows | Enhanced ring buffer replacing current TSDB aggregator with per-metric sliding windows and pre-computed percentiles | `metric_buckets` table |
| **Graph (in-memory)** | Service topology, dependency analysis, impact radius, health propagation | In-memory adjacency graph rebuilt periodically from span parent→child relationships | Derived from `spans` table (not stored separately) |
| **Vector (embedded)** | Semantic log search, AI similarity matching, anomaly clustering | Embedded HNSW index (Go library, e.g. `github.com/viterin/vek`) built from log embeddings | `logs` table (vectors are ephemeral search indexes) |
| **Relational (persistent)** | Source of truth for all data, cold queries, compliance | SQLite/MySQL/PostgreSQL/MSSQL via GORM | — (final destination) |

---

## Phase 1: Performance Fixes ✅ DONE (commit 648b428)

**Changes delivered:**
- `internal/tsdb/aggregator.go` — json.Marshal moved outside lock, flushChan 100→500, 3 persistence workers, `BucketCount()`/`DroppedBatches()` accessors
- `internal/storage/trace_repo.go` — removed `Preload("Spans")` from list view; batch span-summary query; parallel COUNT+SELECT via errgroup
- `internal/storage/log_repo.go` — parallel COUNT+SELECT via errgroup
- `internal/config/config.go` — 20+ new env vars (pool, hot/cold, sampling, cardinality, DLQ, MCP, compression, vector)
- `internal/storage/factory.go` — configurable connection pool via env vars
- `internal/cache/ttl.go` *(new)* — in-memory TTL cache with background eviction
- `internal/api/server.go` — cache field added, `GET /api/system/graph` registered
- `internal/api/graph_handler.go` *(new)* — AI-consumable system graph with health scores + 10s TTL cache

## Phase 1: Performance Fixes (Highest Impact)

### 1.1 TSDB Aggregator — Move json.Marshal outside lock
- **File:** `internal/tsdb/aggregator.go` — `Ingest()` (line 77)
- **Problem:** `json.Marshal(m.Attributes)` runs inside `a.mu.Lock()`, blocking all concurrent ingestion
- **Fix:** Pre-compute `attrJSON` and `key` before acquiring the lock. Lock only protects map read/write

### 1.2 Eliminate eager Preload("Spans") on list views
- **File:** `internal/storage/trace_repo.go` — `GetTracesFiltered()`
- **Problem:** `Preload("Spans")` loads ALL spans for every trace in paginated results
- **Fix:** Use a COUNT subquery for `span_count` and a lightweight join for `operation`. Only use `Preload("Spans")` in single-trace detail view

### 1.3 Parallel COUNT + SELECT for pagination
- **Files:** `internal/storage/trace_repo.go`, `internal/storage/log_repo.go`
- **Fix:** Run `Count()` and `Find()` concurrently via `errgroup`

### 1.4 TSDB flush channel — visibility + throughput
- **File:** `internal/tsdb/aggregator.go` — `flush()` (line 114)
- **Fix:** Increase channel cap to 500, add `argus_tsdb_batches_dropped` counter, spawn multiple persistence workers (default 3)

### 1.5 In-memory TTL cache for dashboard queries
- **New file:** `internal/cache/ttl.go` — simple `sync.Map` with expiry
- **Files affected:** `internal/api/server.go`, `internal/api/metrics_handlers.go`
- **Cache:** `GetDashboardStats`, `GetServiceMapMetrics`, `GetTrafficMetrics` with 10s TTL

### 1.6 Connection pool config via environment
- **Files:** `internal/storage/factory.go`, `internal/config/config.go`
- **Add:** `DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`, `DB_CONN_MAX_LIFETIME` env vars

---

## Phase 2: Hot/Cold Storage ✅ DONE (commit 711220c)

**Changes delivered:**
- `internal/archive/archiver.go` *(new)* — daily worker; zstd-compressed JSONL per day; SHA-256 manifest; FIFO size enforcement
- `internal/archive/maintenance.go` *(new)* — post-archival DB optimize (SQLite/PostgreSQL/MySQL)
- `internal/storage/archive_repo.go` *(new)* — archive query/delete methods + `HotDBSizeBytes()`
- `main.go` — archive worker wired with context-cancelled goroutine

## Phase 2: Hot/Cold Storage & Data Compression

### 2.1 Hot/Cold storage architecture
- **Hot storage (0-7 days):** Current DB — fast queries, full indexes, uncompressed for speed
- **Cold storage (7+ days):** Compressed archive files on disk (zstd-compressed JSON/JSONL per day)
- **Design:**
  - New package: `internal/archive/`
  - Background worker runs daily (configurable via `ARCHIVE_SCHEDULE_HOUR=2` for 2 AM)
  - Queries data older than `HOT_RETENTION_DAYS=7` from hot DB
  - Writes to `data/cold/{year}/{month}/{day}/{type}.jsonl.zst` (traces, logs, metrics separate)
  - Deletes archived records from hot DB after successful write
  - Runs VACUUM/OPTIMIZE after deletion

### 2.2 Cold storage file format
```
data/cold/
  2026/03/08/
    traces.jsonl.zst    # One JSON object per line, zstd compressed
    logs.jsonl.zst
    metrics.jsonl.zst
    manifest.json       # Record counts, size, checksum, time range
```

### 2.3 Cold storage query API
- **New endpoint:** `GET /api/archive/search?type=logs&start=...&end=...&query=...`
- Decompresses and streams matching records from cold files
- Returns results with a `source: "cold"` marker so consumers know latency expectations
- Optional: lazy index file per archive day for faster filtering

### 2.4 Archive configuration
- **File:** `internal/config/config.go`
- `HOT_RETENTION_DAYS=7` — days to keep in hot DB
- `COLD_STORAGE_PATH=data/cold` — where to write archives
- `COLD_STORAGE_MAX_GB=50` — max cold storage disk usage (FIFO eviction)
- `ARCHIVE_SCHEDULE_HOUR=2` — hour of day to run archival
- `ARCHIVE_BATCH_SIZE=10000` — records per batch during archival

### 2.5 Enhanced compression for hot storage
- **File:** `internal/storage/models.go` — `CompressedText` already uses zstd
- **Improvement:** Add compression level config (`COMPRESSION_LEVEL=default|fast|best`)
- **Improvement:** For list-view queries, skip decompressing `AttributesJSON` and `Body` — use raw/truncated previews

### 2.6 Automatic hot DB maintenance
- **File:** `internal/archive/maintenance.go`
- After archival: run DB-specific optimize commands
  - SQLite: `VACUUM`, `PRAGMA optimize`
  - PostgreSQL: `VACUUM ANALYZE`
  - MySQL: `OPTIMIZE TABLE`
- Track and expose `argus_archive_last_run`, `argus_archive_records_moved`, `argus_hot_db_size_bytes`

---

## Phase 3: Self-Monitoring Metrics ✅ DONE (commit c78058e)

**Changes delivered:**
- `internal/telemetry/metrics.go` — 19 new Prometheus metrics (gRPC, HTTP, TSDB, WebSocket, DLQ, Archive, Runtime); `StartRuntimeMetrics()` goroutine; enriched `HealthStats` with goroutines/heap/uptime
- `internal/api/middleware.go` *(new)* — `MetricsMiddleware` wraps all HTTP routes with latency + count recording; path cardinality guard collapses UUIDs/IDs to `{id}`
- `main.go` — `metricsUnaryInterceptor` on gRPC server; `MetricsMiddleware` wraps HTTP mux; `StartRuntimeMetrics()` called at startup

## Phase 3: Self-Monitoring Metrics

### 3.1 Expand Prometheus metrics
- **File:** `internal/telemetry/metrics.go`

| Metric | Type | Labels | Purpose |
|--------|------|--------|---------|
| `argus_grpc_requests_total` | CounterVec | method, status | gRPC call counts |
| `argus_grpc_request_duration_seconds` | HistogramVec | method | gRPC latency |
| `argus_grpc_batch_size` | Histogram | — | Spans/logs per Export call |
| `argus_http_requests_total` | CounterVec | method, path, status | API call counts |
| `argus_http_request_duration_seconds` | HistogramVec | method, path | API latency |
| `argus_tsdb_ingest_total` | Counter | — | Raw metric points ingested |
| `argus_tsdb_flush_duration_seconds` | Histogram | — | Flush window time |
| `argus_tsdb_batches_dropped_total` | Counter | — | Dropped batches |
| `argus_ws_messages_sent_total` | CounterVec | type | WS broadcast count |
| `argus_ws_slow_clients_removed_total` | Counter | — | Dropped slow clients |
| `argus_dlq_enqueued_total` | Counter | — | DLQ writes |
| `argus_dlq_replay_success_total` | Counter | — | Successful replays |
| `argus_dlq_replay_failure_total` | Counter | — | Failed replays |
| `argus_dlq_disk_bytes` | Gauge | — | DLQ disk usage |
| `argus_archive_records_moved` | Counter | type | Records archived |
| `argus_hot_db_size_bytes` | Gauge | — | Hot DB file size |
| `argus_cold_storage_bytes` | Gauge | — | Cold archive size |
| `argus_go_goroutines` | Gauge | — | Active goroutines |
| `argus_go_heap_alloc_bytes` | Gauge | — | Heap memory |

### 3.2 HTTP metrics middleware
- **New file:** `internal/api/middleware.go`
- Wrap all routes with latency + count recording
- Wire in `main.go`

### 3.3 gRPC interceptor
- **File:** `main.go` — add `grpc.UnaryInterceptor()` to gRPC server
- Records `argus_grpc_*` metrics per RPC

### 3.4 Runtime metrics goroutine
- **File:** `main.go` — background goroutine sampling `runtime.MemStats` every 15s

### 3.5 Enriched /api/health
- **File:** `internal/telemetry/metrics.go` — expand `HealthStats`
- Add: `goroutines`, `heap_alloc_mb`, `uptime_seconds`, `hot_db_size_mb`, `cold_storage_mb`, `archive_last_run`

---

## Phase 4: AI-Consumable System Graph API

### 4.1 New endpoint: `GET /api/system/graph`
- **New file:** `internal/api/graph_handler.go`
- **Route:** registered in `internal/api/server.go`
- **Cached:** 10s TTL via Phase 1.5 cache

### 4.2 Response schema
```json
{
  "timestamp": "2026-03-15T10:00:00Z",
  "system": {
    "total_services": 7,
    "healthy": 6,
    "degraded": 1,
    "critical": 0,
    "overall_health_score": 0.92,
    "total_error_rate": 0.015,
    "avg_latency_ms": 52.3,
    "hot_db_size_mb": 450,
    "cold_storage_mb": 2300,
    "uptime_seconds": 86400
  },
  "nodes": [
    {
      "id": "order-service",
      "type": "service",
      "health_score": 0.95,
      "status": "healthy",
      "metrics": {
        "request_rate_rps": 150.5,
        "error_rate": 0.02,
        "avg_latency_ms": 45.2,
        "p99_latency_ms": 230.0,
        "log_error_count_1h": 3,
        "span_count_1h": 12000
      },
      "alerts": []
    }
  ],
  "edges": [
    {
      "source": "order-service",
      "target": "payment-service",
      "call_count": 1200,
      "avg_latency_ms": 23.5,
      "error_rate": 0.01,
      "status": "healthy"
    }
  ]
}
```

### 4.3 Health score computation
- Per service: `health = 1.0 - (error_rate * 5) - (latency_deviation * 0.1)`
- Thresholds: >0.9 = "healthy", >0.7 = "degraded", else "critical"
- Auto-generated alerts: error rate spike >2x baseline, p99 >500ms

### 4.4 Repository method
- **File:** `internal/storage/graph_repo.go` (new)
- `GetSystemGraph(start, end)` — combines topology + per-service + per-edge metrics
- Uses `errgroup` to run sub-queries in parallel

### 4.5 WebSocket variant
- **File:** `internal/realtime/events_ws.go`
- Add system graph to `LiveSnapshot` for real-time AI agent subscriptions

---

## Phase 5: Embedded Specialized DB Accelerators

All internal DBs are **embedded** (compiled into the single binary, no external processes). They act as hot caches that accelerate queries while the relational DB remains the source of truth.

### 5.1 Enhanced Time Series Ring Buffer
- **New file:** `internal/tsdb/ringbuffer.go`
- **Replaces:** Current map-based TSDB aggregator buckets with a proper per-metric ring buffer
- **Design:**
  - Fixed-size circular buffer per metric key (default 1 hour of 30s windows = 120 slots)
  - Pre-computed aggregates: min, max, avg, p50, p95, p99 per window
  - Lock-free reads via atomic snapshot (copy-on-write ring)
  - Configurable retention: `TSDB_RING_BUFFER_DURATION=1h`
  - On flush: write aggregated buckets to relational DB (existing `BatchCreateMetrics`)
- **Benefits:** Dashboard queries for recent data (last 1h) hit ring buffer directly — zero DB queries
- **Rebuild:** On startup, hydrate from `metric_buckets` table for the last `TSDB_RING_BUFFER_DURATION`

### 5.2 In-Memory Service Graph
- **New file:** `internal/graph/service_graph.go`
- **Design:**
  - Adjacency list: `map[string]*ServiceNode` where each node has edges to downstream services
  - Each edge tracks: call count, error count, latency stats (min/max/avg/p99)
  - Rebuilt every 30s from recent spans (last 5 min window) via a background goroutine
  - Source data: `SELECT service_name, parent_span_id FROM spans WHERE timestamp > ?`
  - Resolves parent span -> parent service via span_id lookup
  - Health score computed per node: `1.0 - (error_rate * 5) - (latency_deviation * 0.1)`
  - Impact radius: BFS/DFS from a failing node to find all affected downstream services
- **Benefits:** `GET /api/system/graph` returns instantly from memory, no complex joins
- **Rebuild:** Fully derivable from `spans` table — no separate persistence needed

### 5.3 Embedded Vector Index for Semantic Log Search
- **New file:** `internal/vectordb/index.go`
- **Go library:** Use `github.com/viterin/vek` for vector ops or `github.com/coder/hnsw` for HNSW index (pure Go, no CGO)
- **Design:**
  - Build embeddings from log body text using a simple TF-IDF or BM25 approach (no external AI call needed for indexing)
  - Optional: If Azure OpenAI is configured, use embedding API for richer semantic vectors
  - HNSW index for approximate nearest neighbor search
  - Index only ERROR/WARN logs to keep index small and relevant
  - Max index size: configurable `VECTOR_INDEX_MAX_ENTRIES=100000` (FIFO eviction)
  - On new log ingestion: add to index asynchronously via buffered channel
- **API:** `GET /api/logs/similar?log_id=123&limit=10` — find logs semantically similar to a given log
- **MCP tool:** `find_similar_logs` — AI agent can find patterns and recurring issues
- **Benefits:** AI agents can cluster errors, find root causes across services
- **Rebuild:** On startup, rebuild from recent ERROR/WARN logs in `logs` table

### 5.4 Shared lifecycle management
- **File:** `main.go`
- All embedded DBs follow the same lifecycle:
  1. **Init:** Create empty structures
  2. **Hydrate:** Rebuild from relational DB (background, non-blocking startup)
  3. **Run:** Background goroutines keep them updated from new ingestion
  4. **Flush:** Periodic write-back to relational DB (TSDB only — graph and vector are derived)
  5. **Shutdown:** Graceful drain, final flush

---

## Phase 6: HTTP Streamable MCP Server

Expose Argus as an MCP (Model Context Protocol) server over HTTP with SSE streaming, so any AI agent (Claude, GPT, Cursor, etc.) can discover and call Argus tools natively — no custom API integration needed.

### 6.1 MCP server package
- **New package:** `internal/mcp/`
- **Files:**
  - `internal/mcp/server.go` — HTTP Streamable MCP server (JSON-RPC 2.0 over HTTP + SSE)
  - `internal/mcp/tools.go` — Tool definitions and handlers
  - `internal/mcp/types.go` — MCP protocol types (InitializeRequest, ToolCallRequest, etc.)
- **Transport:** HTTP Streamable MCP (POST for requests, GET with SSE for streaming responses)
- **Endpoint:** `POST /mcp` (JSON-RPC), `GET /mcp` (SSE stream)
- **Config:** `MCP_ENABLED=true`, `MCP_PATH=/mcp`

### 6.2 MCP Tools to expose

| Tool Name | Description | Parameters |
|-----------|-------------|------------|
| `get_system_graph` | Full system topology with health scores, error rates, latencies | `time_range` (optional) |
| `get_service_health` | Health details for a specific service | `service_name` |
| `search_logs` | Search logs by severity, service, body text, time range | `query`, `severity`, `service`, `start`, `end`, `limit` |
| `get_trace` | Get full trace detail with all spans | `trace_id` |
| `search_traces` | Search traces by service, status, duration | `service`, `status`, `min_duration_ms`, `start`, `end`, `limit` |
| `get_metrics` | Query metric time series | `name`, `service`, `start`, `end` |
| `get_dashboard_stats` | Dashboard summary (error rates, throughput, latencies) | `start`, `end`, `services` |
| `get_alerts` | Active alerts and anomalies | — |
| `get_storage_status` | Hot/cold storage sizes, last archival, DB health | — |
| `search_cold_archive` | Search archived data beyond hot retention | `type`, `start`, `end`, `query` |
| `find_similar_logs` | Semantic similarity search across logs | `log_id`, `limit` |

### 6.3 MCP protocol implementation
- **Initialize:** Return server info, capabilities, and tool list
- **tools/list:** Return all tool definitions with JSON Schema parameters
- **tools/call:** Route to handler, return results as structured JSON content
- **Streaming:** For large result sets (log search, trace list), stream results via SSE with progress notifications
- **Error handling:** Map internal errors to MCP error codes

### 6.4 MCP response format
Each tool returns structured content that AI agents can reason over:
```json
{
  "content": [
    {
      "type": "text",
      "text": "Found 3 degraded services in the last hour..."
    },
    {
      "type": "resource",
      "resource": {
        "uri": "argus://system/graph",
        "mimeType": "application/json",
        "text": "{...system graph JSON...}"
      }
    }
  ]
}
```

### 6.5 Integration with existing code
- **File:** `main.go` — conditionally start MCP server on same HTTP mux
- **File:** `internal/api/server.go` — register `/mcp` route
- **Reuse:** All MCP tool handlers delegate to existing `storage.Repository` methods and `telemetry.Metrics` — no duplication
- **Cache:** MCP tools use the same TTL cache from Phase 1.5

### 6.6 MCP Resources (read-only data subscriptions)
- `argus://system/graph` — live system graph (subscribable)
- `argus://services/{name}/health` — per-service health
- `argus://metrics/prometheus` — current Prometheus metrics dump
- AI agents can subscribe to resources for real-time updates via SSE

---

## Phase 7: Smart Observability

### 7.1 Adaptive trace sampling
- **New file:** `internal/ingest/sampler.go`
- Token-bucket per service. Always keep: errors, slow traces (>p99), rare services
- Sample healthy traces at configurable rate (default 10%)
- **Config:** `SAMPLING_RATE=0.1`, `SAMPLING_ALWAYS_ON_ERRORS=true`, `SAMPLING_LATENCY_THRESHOLD_MS=500`

### 7.2 Metric cardinality controls
- **File:** `internal/tsdb/aggregator.go`
- Attribute allowlist: `METRIC_ATTRIBUTE_KEYS=service,method,status_code`
- Max cardinality guard: if `len(buckets)` > 10,000, route to overflow bucket
- Metric: `argus_tsdb_cardinality_overflow_total`

### 7.3 DLQ bounded disk + backoff
- **File:** `internal/queue/dlq.go`
- Add `DLQ_MAX_FILES`, `DLQ_MAX_DISK_MB`, `DLQ_MAX_RETRIES` config
- Exponential backoff on replay failures

### 7.4 API rate limiting
- **New file:** `internal/api/ratelimit.go`
- Token-bucket per IP, configurable via `API_RATE_LIMIT_RPS=100`

---

## Implementation Priority

| # | Item | Files | Impact |
|---|------|-------|--------|
| 1 | Move Marshal outside lock | `internal/tsdb/aggregator.go` | High — eliminates contention |
| 2 | Remove eager Preload | `internal/storage/trace_repo.go` | High — massive memory savings |
| 3 | Hot/cold storage + archival | `internal/archive/` (new), `internal/config/config.go` | Critical — prevents DB blowup |
| 4 | Core metrics + middleware | `internal/telemetry/metrics.go`, `internal/api/middleware.go`, `main.go` | High — foundation |
| 5 | TTL cache for dashboard | `internal/cache/ttl.go` (new), `internal/api/metrics_handlers.go` | Medium — reduces DB load |
| 6 | In-memory service graph | `internal/graph/service_graph.go` (new) | High — instant topology queries |
| 7 | System graph API (uses #6) | `internal/api/graph_handler.go` (new) | High — AI enablement |
| 8 | Enhanced TSDB ring buffer | `internal/tsdb/ringbuffer.go` (new) | High — zero-DB dashboard queries |
| 9 | MCP Server (HTTP Streamable) | `internal/mcp/` (new), `main.go`, `internal/api/server.go` | High — universal AI agent access |
| 10 | Flush channel + workers | `internal/tsdb/aggregator.go` | Medium — throughput |
| 11 | Adaptive sampling | `internal/ingest/sampler.go` (new) | High — controls volume |
| 12 | Embedded vector index | `internal/vectordb/index.go` (new) | Medium — semantic log search |
| 13 | Cardinality controls | `internal/tsdb/aggregator.go` | Medium — prevents explosion |
| 14 | Parallel COUNT+SELECT | `internal/storage/trace_repo.go`, `log_repo.go` | Medium |
| 15 | DLQ bounded + backoff | `internal/queue/dlq.go` | Medium — safety |
| 16 | Rate limiting | `internal/api/ratelimit.go` (new) | Medium — protection |
| 17 | Connection pool config | `internal/storage/factory.go`, `internal/config/config.go` | Low |

## Verification

1. **Performance:** Run test simulation (`test/run_simulation.ps1`) before/after, compare `/api/health` latency metrics
2. **Metrics:** Verify all new Prometheus metrics appear at `GET /metrics`
3. **Graph API:** Hit `GET /api/system/graph` during simulation, validate JSON schema
4. **Archival:** Set `HOT_RETENTION_DAYS=0`, trigger archival, verify cold files created and hot DB shrunk
5. **Sampling:** Enable sampling, verify reduced ingestion rate via `argus_ingestion_rate` while errors still captured
6. **MCP Server:** `POST /mcp` with `{"jsonrpc":"2.0","method":"initialize",...}` returns tool list; call `tools/call` with `get_system_graph` returns valid graph; connect from Claude Desktop or any MCP client
