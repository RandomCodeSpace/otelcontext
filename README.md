<div align="center">
  <img src="internal/ui/static/favicon.svg" width="88" height="88" alt="OtelContext">

  <h1>OtelContext</h1>

  <p><strong>Turn traces, logs, and metrics into one clear view of your services.</strong></p>
  <p>Self-hosted OpenTelemetry collection, incident triage, and service mapping in one Go binary.</p>

  <p>
    <a href="https://github.com/RandomCodeSpace/otelcontext/releases"><img alt="Latest release" src="https://img.shields.io/github/v/release/RandomCodeSpace/otelcontext?style=for-the-badge&logo=github&label=Release"></a>
    <a href="https://github.com/RandomCodeSpace/otelcontext/actions/workflows/ci.yml"><img alt="CI status" src="https://img.shields.io/github/actions/workflow/status/RandomCodeSpace/otelcontext/ci.yml?branch=main&style=for-the-badge&logo=githubactions&logoColor=white&label=CI"></a>
    <a href="https://github.com/RandomCodeSpace/otelcontext/actions/workflows/security.yml"><img alt="Security status" src="https://img.shields.io/github/actions/workflow/status/RandomCodeSpace/otelcontext/security.yml?branch=main&style=for-the-badge&logo=securityscorecard&logoColor=white&label=Security"></a>
    <a href="LICENSE.md"><img alt="MIT license" src="https://img.shields.io/github/license/RandomCodeSpace/otelcontext?style=for-the-badge&color=818cf8"></a>
  </p>
</div>

![Traces, logs, and metrics flowing through OtelContext into a connected service map](docs/assets/otelcontext-overview.webp)

OtelContext helps you answer the question that starts most incidents: **what is broken, and what does it affect?** Send it standard OpenTelemetry data and it connects service relationships, errors, latency, logs, and traces in a map built for investigation.

It starts with SQLite and no external services. When you need more, the main telemetry database can use PostgreSQL, MySQL, or SQL Server.

> [!IMPORTANT]
> OtelContext is pre-1.0. Read the [changelog](CHANGELOG.md) before upgrading.

## What you get

- **A live service map** that shows dependencies, health, latency, and active anomalies.
- **Incident context in one place** with related traces, logs, metrics, and impact analysis.
- **Seven MCP tools** for investigating your system from an AI client or coding agent.
- **Standard OTLP input** over gRPC and HTTP, so existing SDKs and Collectors can send data directly.
- **A simple first run** with one binary and a local SQLite database.
- **Self-hosted data** with retention controls, health probes, and Prometheus metrics.

## Quick start

### 1. Install a release

Download the archive for your platform from [GitHub Releases](https://github.com/RandomCodeSpace/otelcontext/releases), or install the latest release with Go:

```bash
go install github.com/RandomCodeSpace/otelcontext@latest
```

### 2. Start OtelContext

```bash
otelcontext
```

That is enough for a local trial. OtelContext creates `OtelContext.db`, listens for OTLP gRPC on `4317`, and serves the UI and HTTP endpoints on `8080`.

Open **[http://localhost:8080](http://localhost:8080)**.

Authentication is off by default so the browser UI works immediately. Keep this first run on your machine. Before exposing it to a network, follow [Secure a deployment](#secure-a-deployment).

### 3. Send telemetry

Point an OpenTelemetry SDK or Collector at either endpoint:

| Protocol | Endpoint |
|---|---|
| OTLP gRPC | `localhost:4317` |
| OTLP HTTP | `http://localhost:8080/v1/traces`, `/v1/logs`, or `/v1/metrics` |

To confirm the connection without setting up an application, send one sample error log:

```bash
curl -fsS http://localhost:8080/v1/logs \
  -H "Content-Type: application/json" \
  -d "{
    \"resourceLogs\": [{
      \"resource\": {\"attributes\": [{
        \"key\": \"service.name\",
        \"value\": {\"stringValue\": \"readme-demo\"}
      }]},
      \"scopeLogs\": [{\"logRecords\": [{
        \"timeUnixNano\": \"$(date +%s)000000000\",
        \"severityNumber\": 17,
        \"severityText\": \"ERROR\",
        \"body\": {\"stringValue\": \"OtelContext is receiving data\"}
      }]}]
    }]
  }"
```

ERROR is used because the default storage threshold keeps WARN and ERROR logs.

<details>
<summary><strong>OpenTelemetry Collector example</strong></summary>

Save this as part of your Collector configuration. Replace `otelcontext` with the host or service name running OtelContext.

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch: {}

exporters:
  otlp/otelcontext:
    endpoint: otelcontext:4317
    tls:
      insecure: true

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/otelcontext]
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/otelcontext]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/otelcontext]
```

For an authenticated deployment, add an `authorization` bearer header and, if needed, `x-tenant-id` under the exporter.

</details>

## Investigate with MCP

Connect an MCP client to `http://localhost:8080/mcp` to investigate the same telemetry from an agent. The tools can:

- Build an anomaly timeline.
- Show the service map and service health.
- Analyze likely root causes and downstream impact.
- Reconstruct a trace graph.
- Search logs with bounded results.

Trace-based answers say whether the supporting exemplar is complete, partial, or no longer retained. OtelContext does not present a partial trace as the whole story.

## Upgrade without guessing

The same binary can check and upgrade its database before it starts any receiver or background work:

```bash
./otelcontext migrate status
./otelcontext migrate up
```

If the database came from an older release that predates migration tracking, identify it once before upgrading:

```bash
./otelcontext migrate baseline --from v0.3.1
# or: --from v0.4.0-beta.2
./otelcontext migrate up
```

The baseline command validates the database first; it does not repair or guess. Create a complete OtelContext backup before upgrading, and keep the previous signed binary until `migrate status` reports `result=ready`. Versioned production checks are available for SQLite and unpartitioned PostgreSQL 16. MySQL, SQL Server, and PostgreSQL daily partitioning keep their existing preview AutoMigrate path.

For a one-host Linux deployment, use the shipped [systemd unit and environment example](deploy/systemd/) and follow the [install, upgrade, and rollback runbook](docs/OPERATIONS.md#supported-systemd-deployment). Direct execution remains supported for local use.

## Back up and restore

After stopping OtelContext cleanly, use the same binary and environment as the service:

```bash
otelcontext backup create --out /absolute/path/to/backups
```

The published bundle keeps the main database, any mode-required aggregate database, the dead-letter queue, generated TLS identity, and a hashed manifest together. Restore into new database and sidecar paths; the command will not overwrite the source or an existing target:

```bash
otelcontext backup restore --bundle /absolute/path/to/backups/otelcontext-backup-...
```

Restore starts the same candidate briefly, waits for `/ready`, and shuts it down again. Keep the old binary, configuration, data, and bundle until the restored deployment passes your checks. The [backup and restore runbook](docs/OPERATIONS.md#backup--restore) covers database-specific tools, fresh-target setup, validation, and rollback.

## Secure a deployment

With no authentication or TLS settings configured, production mode starts without either:

```bash
APP_ENV=production ./otelcontext
```

Authentication is optional. Enable it when the deployment boundary requires a credential:

- `API_KEY` for a shared operator credential.
- `AUTH_TRUST_EXTERNAL=true` when a trusted reverse proxy owns authentication and tenant identity.

Authenticated example:

```bash
export API_KEY="$(openssl rand -hex 32)"
./otelcontext
```

Clients then send `Authorization: Bearer <key>`. Authentication and TLS are independently optional at runtime; configure each one when the deployment boundary requires it.

The browser UI does not currently store an API key. For an authenticated browser deployment, put OtelContext behind a same-origin proxy that authenticates the user and injects the credential for REST, MCP, and WebSocket traffic.

See the [operations guide](docs/OPERATIONS.md) for database, TLS, retention, proxy, and health-check setup.

## Common configuration

OtelContext reads environment variables and an optional `.env` file in its working directory.

| Setting | Default | Use it for |
|---|---|---|
| `HTTP_PORT` | `8080` | UI, REST, OTLP HTTP, MCP, WebSockets, and probes |
| `GRPC_PORT` | `4317` | OTLP gRPC |
| `DB_DRIVER` | `sqlite` | `sqlite`, `postgres`, `mysql`, or `sqlserver` |
| `DB_DSN` | `OtelContext.db` | Database connection string |
| `API_KEY` | empty | Shared bearer authentication |
| `DEFAULT_TENANT` | `default` | Scope used when no trusted tenant is supplied |
| `HOT_RETENTION_DAYS` | `7` | Main retention horizon |
| `STORE_MIN_SEVERITY` | `WARN` | Lowest log severity stored in the main database |
| `AGGREGATE_MODE` | `legacy` | `legacy`, `aggregate-shadow`, or `aggregate` |

Start with [`.env.example`](.env.example). The default `legacy` mode is the right choice for a first run. Aggregate modes are for measured high-volume deployments; review the [aggregate gate report](docs/gates/2026-08-23-aggregate-7day-gate.md) before enabling them.

<details>
<summary><strong>Build from source</strong></summary>

Use the Go version in `go.mod`:

```bash
git clone https://github.com/RandomCodeSpace/otelcontext.git
cd otelcontext

CGO_ENABLED=0 go build -o otelcontext .
```

The client-rendered UI is committed as plain HTML, CSS, and JavaScript and is embedded automatically. There is no Node.js install or frontend build step.

Run the core checks with:

```bash
go build ./...
go vet ./...
go test -race -timeout 180s ./...
```

</details>

## Project links

- [Releases](https://github.com/RandomCodeSpace/otelcontext/releases)
- [Changelog](CHANGELOG.md)
- [Operations guide](docs/OPERATIONS.md)
- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)
- [Aggregate design glossary](CONTEXT.md)
- [Release gate protocol](docs/gates/README.md)

## License

OtelContext is available under the [MIT License](LICENSE.md).
