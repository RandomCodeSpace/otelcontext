# OtelContext — AI Agent Instructions

## Project Overview

OtelContext is a self-hosted OTLP observability platform. Single Go binary with embedded React frontend.
- **Backend:** Go 1.24, native `net/http` (no frameworks), GORM ORM, gRPC for OTLP ingestion
- **Frontend:** React 19 + TypeScript + Mantine UI v8 + ECharts + ReactFlow
- **Ports:** gRPC `:4317` (OTLP), HTTP `:8080` (API + WebSocket + UI)

## Strict Rules

- NO Express.js/Gin/Echo — use native Go `net/http`
- NO Tailwind CSS — use Mantine UI v8 exclusively
- Single-service architecture (no microservices split)
- All internal DBs must be **embedded** (no external processes)
- Relational DB (SQLite/MySQL/PostgreSQL/MSSQL) is the **single source of truth**
- Prioritize self-hosted, open-source solutions

## Architecture

```
gRPC :4317 (OTLP Ingest) ──► Ingestion Layer ──► Storage (GORM)
                                    │                    │
                                    ▼                    ▼
                              In-Memory DBs         Relational DB
                              (TSDB Ring,           (Source of Truth)
                               Graph, Vector)            │
                                    │                    ▼
HTTP :8080 ◄── REST API ◄──────────┘              Cold Archive
           ◄── WebSocket (real-time)              (zstd JSONL)
           ◄── MCP Server (AI agents)
           ◄── Prometheus /metrics
```

## Storage Architecture

| Layer | Package | Purpose |
|-------|---------|---------|
| Time Series (in-memory) | `internal/tsdb/` | Ring buffer, sliding windows, pre-computed percentiles |
| Graph (in-memory) | `internal/graph/` | Service topology, health scores, impact analysis |
| Vector (embedded) | `internal/vectordb/` | HNSW index for semantic log search (pure Go, no CGO) |
| Relational (persistent) | `internal/storage/` | GORM-based, multi-DB, single source of truth |
| Cold Archive | `internal/archive/` | Zstd-compressed JSONL on local disk (7+ day old data) |

## Implementation Plan

Full implementation plan with 7 phases, 17 prioritized items, and detailed file-level specifications:
**See [`docs/IMPLEMENTATION_PLAN.md`](docs/IMPLEMENTATION_PLAN.md)**

### Summary of Phases
1. **Performance Fixes** — TSDB lock contention, eager Preload, TTL cache, flush channel
2. **Hot/Cold Storage** — 7-day hot retention, zstd cold archive, auto-maintenance
3. **Self-Monitoring Metrics** — 19 new Prometheus metrics, HTTP/gRPC middleware
4. **System Graph API** — `GET /api/system/graph` with health scores for AI agents
5. **Embedded DB Accelerators** — Ring buffer TSDB, in-memory graph, vector index
6. **MCP Server** — HTTP Streamable MCP at `/mcp` for universal AI agent access
7. **Smart Observability** — Adaptive sampling, cardinality controls, rate limiting

## Key Directories

```
internal/
  api/          # HTTP handlers, middleware, rate limiting
  archive/      # Hot/cold storage archival (new)
  cache/        # TTL cache (new)
  compress/     # Zstd compression utilities
  config/       # Environment configuration
  graph/        # In-memory service graph (new)
  ingest/       # OTLP gRPC receivers + sampling
  mcp/          # MCP server (new)
  queue/        # Dead Letter Queue
  realtime/     # WebSocket hub + event streaming
  storage/      # GORM repository, models, migrations
  telemetry/    # Prometheus metrics + health
  tsdb/         # Time series aggregator + ring buffer
  vectordb/     # Embedded vector index (new)
web/            # React frontend (Vite + Mantine)
test/           # Microservice simulation (7 services)
docs/           # Specifications and plans
```

