# OtelContext — Operations Guide

A self-hosted OTLP observability platform in a single Go binary. This guide covers first-run, production checklist, data layout, backup, incident response, and known limits.

For AI-agent-oriented architecture context see `../CLAUDE.md`. For the canonical env-var reference see `../.env.example`.

---

## First Run

### SQLite (zero-config)

```bash
./otelcontext
```

What happens:
- `.env` is loaded if present; otherwise defaults apply.
- GORM `AutoMigrate` creates tables in `otelcontext.db` in the working directory.
- `DEFAULT_TENANT=default` is assigned to all rows ingested without an explicit tenant header.
- `API_KEY` is empty → auth middleware is a **pass-through** (every request allowed). A warning is logged.
- No TLS is configured → HTTP and gRPC listen in **plaintext**. Dev only.
- Listeners: HTTP `:8080`, gRPC `:4317`, Prometheus `/metrics`, liveness `/live`, readiness `/ready`.

### Postgres

```bash
DB_DRIVER=postgres \
DB_DSN="host=localhost user=otel password=otel dbname=otelcontext port=5432 sslmode=disable TimeZone=UTC" \
DB_AUTOMIGRATE=false \
API_KEY="$(openssl rand -hex 32)" \
./otelcontext
```

Set `DB_AUTOMIGRATE=false` in production. AutoMigrate locks tables and has no rollback; run schema changes out-of-band (Flyway, goose, sqlc migrate, etc.).

### TLS

Two paths:

1. **Explicit cert files** (wins if set):
   ```bash
   TLS_CERT_FILE=/etc/otelcontext/tls/server.crt
   TLS_KEY_FILE=/etc/otelcontext/tls/server.key
   ```
   Both must be set together; both must exist and be readable at startup.

2. **Auto self-signed** (dev/internal only):
   ```bash
   TLS_AUTO_SELFSIGNED=true
   TLS_CACHE_DIR=./data/tls
   ```
   Generates an ECDSA-P256 cert on first start, caches it under `TLS_CACHE_DIR`, regenerates on expiry. Clients must trust the generated material (insecure-skip or CA pin).

When TLS is enabled, both HTTP (`:8080`) and gRPC (`:4317`) serve TLS only.

### Azure Entra (passwordless Postgres)

```bash
DB_DRIVER=postgres
DB_AZURE_AUTH=true
DB_DSN="host=my-server.postgres.database.azure.com user=my-mi@tenant.onmicrosoft.com dbname=otelcontext port=5432 sslmode=require"
```

- DSN must **omit the password**; the `user` field is the Entra principal.
- `sslmode` must be `require`, `verify-ca`, or `verify-full` — weaker modes are rejected at startup.
- Credential resolution: env vars → workload identity → managed identity → Azure CLI → developer credentials.
- Local dev: `az login` is sufficient.
- AKS: use workload identity or pod-managed identity.
- `DB_CONN_MAX_LIFETIME` is internally capped to 30 minutes when Entra auth is active (tokens expire).

---

## Production Checklist

### Must set
- `API_KEY` — long random string. Without it, anyone on the network can query or ingest.
- `DB_DRIVER=postgres` (or another persistent driver). SQLite is fine for small single-node deployments; plan for it accordingly.
- `DB_DSN` — with strict TLS when crossing a network boundary.
- `TLS_CERT_FILE` + `TLS_KEY_FILE` (or `TLS_AUTO_SELFSIGNED=true` for internal-only deployments).
- `HOT_RETENTION_DAYS` — pick a value you can defend. Default 7 is reasonable; range is 1..36500.

### Should set
- `DB_MAX_OPEN_CONNS` — size to match your Postgres pool and expected ingest concurrency.
- `DEFAULT_TENANT` — a non-`default` value if the deployment serves a specific tenant.
- `OTEL_EXPORTER_OTLP_ENDPOINT` — enables self-instrumentation. Set to `localhost:4317` to dogfood into the same instance.
- `DB_AUTOMIGRATE=false` for Postgres in production.

### Trust the defaults (don't tune unless you have a reason)
- `METRIC_MAX_CARDINALITY=10000`
- `DLQ_MAX_DISK_MB=500`, `DLQ_MAX_FILES=1000`, `DLQ_MAX_RETRIES=10`
- `API_RATE_LIMIT_RPS=100`
- `VECTOR_INDEX_MAX_ENTRIES=100000`
- `SAMPLING_*` (defaults keep 100% + always-on errors)
- `GRAPHRAG_WORKER_COUNT=4`, `GRAPHRAG_EVENT_QUEUE_SIZE=10000` — raise worker count at 100+ services if you see `graphrag_events_dropped_total` climbing
- `GRPC_MAX_RECV_MB=16`, `GRPC_MAX_CONCURRENT_STREAMS=1000` — OTLP gRPC server caps
- `RETENTION_BATCH_SIZE=50000`, `RETENTION_BATCH_SLEEP_MS=1` — purge pacing; raise the sleep for busy production DBs

### SQLite in production
SQLite is rejected at startup when `APP_ENV=production` unless you explicitly opt in with `OTELCONTEXT_ALLOW_SQLITE_PROD=true`. The guard exists because SQLite uses a single writer lock — fine for < ~10 services at low QPS, miserable at scale. Prefer Postgres for anything resembling production.

---

## Data Layout

| Location | What lives here |
|---|---|
| `DB_DSN` (relational) | Logs, traces, spans, metric buckets, investigations, graph snapshots, Drain templates. **Single source of truth.** |
| `DLQ_PATH` (`./data/dlq` default) | Failed-ingest envelopes awaiting replay. Bounded by `DLQ_MAX_DISK_MB`. |
| `TLS_CACHE_DIR` (`./data/tls` default) | Auto-self-signed cert + key material. |
| Working directory (SQLite only) | `otelcontext.db` when `DB_DRIVER=sqlite`. |

**Retention.** `RetentionScheduler` runs hourly. It batches `PurgeLogsBatched`, `PurgeTracesBatched`, and `PurgeMetricBucketsBatched` against rows older than `HOT_RETENTION_DAYS`, plus a daily `VACUUM`/`ANALYZE` pass. Purge is cross-tenant (it does not scope by `tenant_id`).

**Multi-tenancy.** Every row carries a `tenant_id` column. The write path reads `X-Tenant-ID` (HTTP) or `x-tenant-id` (gRPC metadata) and populates the column. The read path attaches the tenant from the request context to every repository query (`Where("tenant_id = ?", ...)`).

---

## Backup & Restore

### SQLite

Online backup (does not block writers):

```bash
sqlite3 otelcontext.db ".backup /backups/otelcontext-$(date +%F).db"
# or
sqlite3 otelcontext.db "VACUUM INTO '/backups/otelcontext-$(date +%F).db'"
```

Restore:

```bash
sqlite3 /backups/otelcontext-YYYY-MM-DD.db "VACUUM INTO './otelcontext.db'"
```

### Postgres

Operator-owned:

```bash
pg_dump -Fc -d otelcontext -f /backups/otelcontext-$(date +%F).dump
```

Restore:

```bash
pg_restore -d otelcontext --clean --if-exists /backups/otelcontext-YYYY-MM-DD.dump
```

### Cadence

- Hourly purge removes rows outside the retention window. If you care about data from within the last hour, back up **before the top of the hour**.
- Daily is fine for the platform use case (platform state is not the same as user application data).
- Test restore quarterly against a scratch instance.

---

## Incident Response

### `/ready` returns 503

Diagnostic tree:

1. **DB unreachable.** Check the `OtelContext_db_up` gauge. If 0, the repository lost its connection. Inspect DB logs, network, credentials (especially Entra token refresh).
2. **GraphRAG wedged.** Symptom: `/ready` passes DB check but latency spikes on MCP tool calls. Restart the process; graph is rebuilt from the DB on boot.
3. **DLQ backlog.** Compare `OtelContext_dlq_disk_bytes` against `DLQ_MAX_DISK_MB`. If near the cap, downstream replay is failing — check ingestion target and `OtelContext_dlq_replay_failure_total`.

### OTLP ingest rejections

Check `OtelContext_otlp_payload_rejected_total` (labeled by reason: `too_large`, `invalid_content_type`, `decode_error`, etc.). Review recent logs for the specific reject reason.

### Retention not running

Alert on both:
- `OtelContext_retention_consecutive_failures` > 3
- `now() - OtelContext_retention_last_success_timestamp` > 2h

Typical causes: DB lock contention, disk full, permissions on the DB file (SQLite).

### Entra token failures

Grep structured logs for `acquire entra token`. Common causes: expired managed-identity binding, misconfigured `AZURE_CLIENT_ID`, `az login` expired (dev).

---

## Observability

- **Prometheus:** `/metrics/prometheus` — public by design (no secrets). Scrape from your existing Prometheus / VictoriaMetrics / etc.
- **Health probes:**
  - `/live` — process is alive (always 200 unless the HTTP server is down).
  - `/ready` — dependencies are healthy (DB reachable, core subsystems running). Use for load-balancer health checks and Kubernetes readiness.
- **Key metrics to alert on:**
  - `OtelContext_db_up == 0`
  - `OtelContext_dlq_disk_bytes / (DLQ_MAX_DISK_MB * 1024 * 1024) > 0.8`
  - `OtelContext_retention_consecutive_failures > 3`
  - `rate(OtelContext_otlp_payload_rejected_total[5m]) > 0`
  - `rate(OtelContext_dlq_replay_failure_total[5m]) > rate(OtelContext_dlq_replay_success_total[5m])`
  - `rate(otelcontext_graphrag_events_dropped_total[5m]) > 0` — ingestion channel saturated; bump `GRAPHRAG_WORKER_COUNT` or `GRAPHRAG_EVENT_QUEUE_SIZE`
  - `otelcontext_retention_rows_behind > 1_000_000` — purge is falling behind; tune `RETENTION_BATCH_SIZE` / `RETENTION_BATCH_SLEEP_MS`
  - `otelcontext_db_pool_in_use / otelcontext_db_pool_max_open > 0.9` — pool exhausted; raise `DB_MAX_OPEN_CONNS`
  - `rate(otelcontext_dlq_evicted_total[5m]) > 0` — DLQ is actively dropping entries at cap; replay target is down or slow
  - `rate(otelcontext_dashboard_p99_row_cap_hits_total[1h]) > 0` on SQLite — dataset exceeds the 200k in-memory cap; migrate to Postgres for accurate p99
- **Log levels:** `LOG_LEVEL=DEBUG` for deep diagnostics, default `INFO`. `WARN` or `ERROR` is too quiet for a running system; avoid in prod.

---

## Known Limitations

- **Single-instance only.** No leader election. Running two replicas against the same DB will double-purge (retention runs on both) and double-snapshot (GraphRAG snapshot loop runs on both). Use a single replica behind your LB, or shard by tenant.
- **Tenant isolation is API-layer.** A shared `API_KEY` grants blanket access to every tenant. There is no per-tenant-key file in the current codebase; isolate tenants at the network/auth layer if that matters.
- **No built-in TLS cert rotation** beyond `TLS_AUTO_SELFSIGNED` regenerating on expiry. For managed certs, re-mount and restart on rotation.
- **GraphRAG is in-memory.** The topology is rebuilt from the DB on boot. Very large corpora (millions of services/operations) will extend boot time.
- **Cold archive is not part of the current build.** Historical data beyond `HOT_RETENTION_DAYS` is deleted, not archived. If you need long-term retention, extend `HOT_RETENTION_DAYS` or export via a downstream pipeline.

---

## Scale & Load Testing

The backend is sized for **100–200 services** emitting OTLP at commodity rates. A programmatic load simulator ships with the repo to verify this.

### Running the simulator

```bash
make loadtest-build       # produces bin/loadsim
./bin/loadsim             # 200 producers × 50 spans/sec × 60s against localhost:4317
./bin/loadsim --help      # flags: --endpoint, --services, --rate, --duration, --tenant-id, --warmup
```

The binary is under the `loadtest` build tag — `go build ./...` and `go test ./...` ignore it. `make loadtest` runs a full 60s sweep against `localhost:4317`.

### What healthy looks like

During a 60s / 200-service run against a warm instance on Postgres:

- Ingestion: no `otlp_payload_rejected_total` samples, no `graphrag_events_dropped_total` samples.
- DB pool: `db_pool_in_use / db_pool_max_open` stays below ~0.8.
- Retention: `retention_rows_behind` stays within one hourly tick of steady state.
- DLQ: zero activity (`dlq_evicted_total`, `dlq_replay_failure_total` unchanged).
- The dashboard p99 gauge updates without hitting the SQLite row cap.

If any of those trip, use the corresponding metric alert from the Observability section above as the entry point.

### When to re-run

- Before cutting a release that touches the ingestion path or GraphRAG.
- After tuning any of: `GRAPHRAG_WORKER_COUNT`, `GRPC_MAX_CONCURRENT_STREAMS`, `RETENTION_BATCH_SIZE`, `DB_MAX_OPEN_CONNS`.
- When scaling the deployment past the current-tested envelope (e.g., 500+ services) — expand the simulator's `--services` flag to match.

---

## Upgrade Path

1. **Back up the DB** (see Backup & Restore above).
2. **Read the CHANGELOG** for breaking changes between your current and target versions.
3. **SQLite:** `DB_AUTOMIGRATE=true` (default) handles schema upgrades in place.
4. **Postgres in production:** keep `DB_AUTOMIGRATE=false` and apply migrations out-of-band before starting the new binary.
5. Roll the new binary. Watch `/ready`, `OtelContext_db_up`, and `retention_*` metrics for the first hour.

If the new version fails to start:
- For SQLite, restore the pre-upgrade backup with `VACUUM INTO`.
- For Postgres, restore via `pg_restore --clean --if-exists`.
