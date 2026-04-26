# OtelContext

[![CI](https://github.com/RandomCodeSpace/otelcontext/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/RandomCodeSpace/otelcontext/actions/workflows/ci.yml)
[![Security (OSS-CLI)](https://github.com/RandomCodeSpace/otelcontext/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/RandomCodeSpace/otelcontext/actions/workflows/security.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/RandomCodeSpace/otelcontext/badge)](https://scorecard.dev/viewer/?uri=github.com/RandomCodeSpace/otelcontext)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/12646/badge)](https://www.bestpractices.dev/projects/12646)
[![Release](https://img.shields.io/github/v/release/RandomCodeSpace/otelcontext)](https://github.com/RandomCodeSpace/otelcontext/releases)
[![Beta](https://img.shields.io/github/v/release/RandomCodeSpace/otelcontext?include_prereleases&label=beta)](https://github.com/RandomCodeSpace/otelcontext/releases)
![Go Version](https://img.shields.io/github/go-mod/go-version/RandomCodeSpace/otelcontext)
[![Frontend Version](https://img.shields.io/badge/frontend-none-lightgrey)](https://github.com/RandomCodeSpace/otelcontext)

A self-hosted OTLP observability platform in a single Go binary — OTLP gRPC + HTTP ingest, GraphRAG-powered root-cause analysis, multi-tenant storage, and a built-in MCP server for AI agents.

For teams who want traces, logs, and metrics in one place without standing up a Collector + Prometheus + Loki + Tempo stack.

## Documentation

- **Operators:** [`docs/OPERATIONS.md`](docs/OPERATIONS.md) — first run, production checklist, backup, incident response, upgrades.
- **AI agents / contributors:** [`CLAUDE.md`](CLAUDE.md) — architecture, GraphRAG, MCP tools, conventions.
- **Env reference:** [`.env.example`](.env.example) — every supported environment variable with defaults.

## Quick start

```bash
# 1. Build
go build -o otelcontext .

# 2. Run with an API key (dev-friendly — SQLite, plaintext HTTP)
export API_KEY="$(openssl rand -hex 32)"
./otelcontext
```

The server listens on:
- OTLP gRPC: `:4317`
- HTTP API + OTLP HTTP + UI + MCP: `:8080`
- Prometheus: `:8080/metrics/prometheus`
- Probes: `:8080/live`, `:8080/ready`

Send an OTLP log via HTTP:

```bash
curl -sS -X POST http://localhost:8080/v1/logs \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "resourceLogs": [{
      "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "demo"}}]},
      "scopeLogs": [{"logRecords": [{
        "timeUnixNano": "'$(date +%s)000000000'",
        "severityText": "INFO",
        "body": {"stringValue": "hello otelcontext"}
      }]}]
    }]
  }'
```

Then query it back:

```bash
curl -sS -H "Authorization: Bearer $API_KEY" \
  "http://localhost:8080/api/logs?limit=5" | jq .
```

## Switching databases

Default is SQLite (`otelcontext.db` in the working dir). Override via env vars:

```bash
# PostgreSQL
DB_DRIVER=postgres \
DB_DSN="host=localhost user=otel password=otel dbname=otelcontext port=5432 sslmode=disable" \
./otelcontext

# MySQL
DB_DRIVER=mysql \
DB_DSN="root:password@tcp(localhost:3306)/otelcontext?charset=utf8mb4&parseTime=True&loc=Local" \
./otelcontext
```

See [`.env.example`](.env.example) for SQL Server and Azure Entra (passwordless Postgres) configurations.

## OTLP Integration

OtelContext accepts OTLP gRPC on `:4317` and OTLP HTTP on `:8080/v1/{traces,logs,metrics}`. Point any OpenTelemetry Collector (or SDK) at it:

```yaml
exporters:
  otlp/otelcontext:
    endpoint: "localhost:4317"
    tls:
      insecure: true

service:
  pipelines:
    traces:
      exporters: [otlp/otelcontext]
    logs:
      exporters: [otlp/otelcontext]
    metrics:
      exporters: [otlp/otelcontext]
```

See `docs/otel-collector-example.yaml` for a complete example.

## Features

- **OTLP gRPC + HTTP ingest** — traces, logs, metrics; gzip and protobuf/JSON supported.
- **GraphRAG** — layered in-memory graph with error-chain, impact, and root-cause queries.
- **Drain log clustering** — deterministic template mining, persisted across restarts.
- **MCP server** — 21 tools exposing the platform to AI agents over JSON-RPC 2.0 + SSE.
- **Multi-tenancy** — per-row `tenant_id`, `X-Tenant-ID` header / `x-tenant-id` gRPC metadata.
- **Adaptive sampling** — always-on for errors and slow spans, probabilistic otherwise.
- **DLQ** — durable typed envelopes with disk-bounded replay.
- **Self-instrumentation** — export OtelContext's own spans via `OTEL_EXPORTER_OTLP_ENDPOINT`.

## Security

See [`SECURITY.md`](SECURITY.md) for the vulnerability reporting process. The security posture (OSV-Scanner, Trivy, Semgrep, Gitleaks, jscpd, SBOM, Scorecard) is described in [`CLAUDE.md`](CLAUDE.md) under "Security & Supply Chain".

## License

See [LICENSE.md](LICENSE.md).
