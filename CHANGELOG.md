# Changelog

All notable changes to **otelcontext** are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)
once a stable release line exists. While otelcontext remains pre-1.0, every
commit on `main` is the canonical version identifier (`git rev-parse HEAD`).
Per-tag pre-release notes are published on
[GitHub Releases](https://github.com/RandomCodeSpace/otelcontext/releases).
This file's `Unreleased` section tracks what has landed on `main` since the
last published pre-release tag (`v0.2.0-beta.6`).

## [Unreleased]

### Fixed — production OOM restarts (memory-survival series)

- **AnomalyStore memory blowup** (merged from `fix/sqlite-survival-hardening`): stable
  per-(service,type) anomaly IDs replace one-node-per-10s-tick; the O(N²)
  PRECEDED_BY edge mesh that heap profiling attributed 84% of live heap to is
  gone (AnomalyStore 272 MB → 2.6 MB in soak).
- **GOMEMLIMIT safety net**: startup sets a soft limit (env honored, else 75%
  of the cgroup/host budget via new `internal/membudget`).
- **Byte-bounded ingest queue**: `INGEST_PIPELINE_MAX_BYTES` (512 MB; 128 MB on
  SQLite) — the item-count queue could hold GBs; at the cap even error/slow
  batches get 429/`RESOURCE_EXHAUSTED` (reason `bytes_full`).
- **GraphRAG bounds**: per-tenant span cap (`GRAPHRAG_MAX_SPANS_PER_TENANT`,
  500k), SQLite trace TTL 1h→30m (`GRAPHRAG_TRACE_TTL`), idle-tenant store
  eviction (`GRAPHRAG_TENANT_IDLE_TTL`, 24h; default tenant immune),
  SignalStore metric nodes bounded (2000/tenant + 24h TTL), anomaly
  correlation walk capped at 1000.
- **TSDB ring buffers**: keys are now tenant-scoped (`tenant|service|metric`
  — fixes a cross-tenant data-isolation breach) and ring creation is capped at
  `METRIC_MAX_CARDINALITY` (previously bypassed the cardinality check).
- **SQLite maintenance**: the automatic daily full `VACUUM` (10–60 min
  exclusive lock → 429 storm → queue/RAM spiral) is replaced by
  `PRAGMA optimize` + `incremental_vacuum(10000)`; restore via
  `RETENTION_FULL_VACUUM=true` or `POST /api/admin/vacuum`. New DB files are
  created `auto_vacuum=INCREMENTAL`.
- **Budget-scaled SQLite PRAGMAs**: page cache = budget/32 ∈ [64 MB, 256 MB],
  mmap = budget/8 ∈ [256 MB, 1 GB] (4 GB host → 128 MB + 512 MB); overrides
  `SQLITE_CACHE_SIZE_KB` / `SQLITE_MMAP_SIZE_BYTES`; fail-closed stanza kept.
- Cherry-picked security/correctness quick-wins: cross-tenant read escape via
  `X-Tenant-ID` closed, token-bucket sampler math fixed (rate < 1.0 dropped
  ~100% of healthy spans — SQLite's 0.05 default persisted almost nothing),
  negative limit/offset clamps on `/api/logs` + `/api/traces`, MCP error
  results no longer cached and `trace_graph` response capped.

### Added — observability

- `PPROF_ADDR` (default `127.0.0.1:6060`): `net/http/pprof` on a dedicated
  loopback listener.
- Store census gauges: `otelcontext_graphrag_store_entities{entity}`,
  `otelcontext_graphrag_store_edges{store}`, `otelcontext_tsdb_ring_series_active`,
  `otelcontext_drain_templates_active`, `otelcontext_ingest_pipeline_queue_bytes`,
  `otelcontext_graphrag_tenants_evicted_total`, `otelcontext_tsdb_ring_series_rejected_total`.

### Changed — performance

- `GetServiceMapMetrics`: node stats aggregate in SQL and the edge pass scans
  a narrow projection — the per-row zstd decompression of span attributes is
  gone (benchmark: 22 ms/5 MB vs 37 ms/15 MB on 5k spans; scales with row
  count). Also fixes node `error_count` being permanently 0. The
  `/api/metrics/service-map` response is cached 30s per tenant+window.
- GraphRAG DB rebuild is incremental via per-tenant high-water-mark (was a
  full 1h-window re-read every 60s); the 10s anomaly scan skips tenants with
  no new ingest events.
- HTTP serving: UI assets ship brotli/gzip precompressed with
  `Cache-Control: immutable` + content hashing; `index.html` gets `no-cache`
  + ETag; SPA fallback for client-side routes; GET `/api/*` responses are
  gzipped; `/api/system/graph`, `/api/metrics/dashboard`, `/api/stats` honor
  `If-None-Match` → 304 with a shared 10s render cache.

### Changed — frontend rewrite complete (phases C3–C7)

- New Triage home at `/`: anomaly strip (MCP `get_anomaly_timeline`) + ranked
  worst-first service feed. The cytoscape physics graph is replaced by a
  deterministic layered SVG flow map (own ~300-line layout; −500 KB raw of
  graph-lib payload; map route now ~5 KB gz), keyboard-walkable, with a
  blast-radius overlay (`/map?impact=svc`).
- Service Inspector (docked panel / bottom sheet) with Overview, Dependencies,
  **Why** (`root_cause_analysis`) and **Impact** (`impact_analysis`) tabs —
  the MCP triage verbs as human-clickable actions. Investigation Trail:
  URL-encoded drill-down breadcrumbs (`?trail=`), shareable and reload-safe.
- `/traces` (virtualized table + real time-positioned SVG waterfall) and
  `/logs` (live tail over the bounded WS ring buffer, virtualized,
  severity pills, context/trace cross-links).
- ⌘K command palette (navigate / services / triage actions / utilities),
  `g m/t/l/h` chords, `?` shortcut sheet.
- `@ossrandom/design-system`, the legacy Dashboard and the MCP Console are
  removed (uplot/clsx/fontsource deps too); nav is exactly
  Triage / Flow Map / Traces / Logs. Bundle budgets tightened to actuals+10%:
  initial JS ≤118 KB gz, initial CSS ≤6 KB gz, every lazy chunk ≤10 KB gz.

### Changed — frontend foundation (rewrite phases C1–C2)

- New data layer: TanStack Query (visibility-aware polling — hidden tabs stop
  hitting SQLite), single WebSocket manager with jittered backoff and a
  bounded 5k log ring buffer, `apiFetch` with AbortSignal, percent-formatting
  bugs fixed centrally.
- New responsive shell: System Pulse bar (health/err/p99/DB size, 3-state
  live indicator), bottom tab bar (<768px) / icon rail / labeled rail
  (≥1440px), Connect popover, token CSS (`tokens.css`) with dark/light themes,
  reduced-motion + contrast support. Routing via wouter; deep links served by
  the SPA fallback. CI-able bundle budget gate (`npm run check-budgets`).

## [v0.2.0-beta.6] — 2026-06-05

This is the first release cut with the **source-only + build-on-tag** flow: the
built UI is embedded into the tagged commit (not committed to `main`), so
`go install github.com/RandomCodeSpace/otelcontext@v0.2.0-beta.6` yields a
UI-complete binary. (Supersedes the premature `v0.2.0-beta.5` tag, which had
committed the built UI to `main`.)

### Added

- **Frontend rebuild** ([#98]) — a new default **Dashboard** (system-health
  gauge, traffic/errors, top failing services, recent anomalies, platform
  health); a **scalable service map** (cytoscape / cose-bilkent) that keeps
  every node on screen from 1→200 services, sizes nodes by edge degree, and
  reveals a node's edges + stats on hover/click; and an **MCP Trial console**
  (list-detail over the 7-tool surface with dynamic tool forms, result views,
  history, and a live `/mcp` SSE stream).
- **Source-only `main` + build-on-tag release flow** ([#99]) —
  `scripts/release.sh` / `make release VERSION=…` build the UI and embed it
  into a detached release commit that the tag points to, so
  `go install …@<tag>` is UI-complete while `main` carries no build artifacts
  (`internal/ui/dist` is gitignored except `.gitkeep`; `//go:embed all:dist`).
- **SQLite survival tuning for 120-service production load** ([#91]):
  - Fail-closed PRAGMA stanza in `internal/storage/factory.go` —
    `journal_mode=WAL`, `synchronous=NORMAL`, 256 MB page cache, 1 GB
    mmap, 64 MB WAL cap, `busy_timeout=5000`.
  - Per-driver config defaults — `config.applyDriverDefaults` overrides
    9 tunables (conn pool, ingest workers/queue, metric cardinality,
    store-min-severity, sampling rate, gRPC stream cap, `LOG_FTS_ENABLED`)
    when `DB_DRIVER=sqlite` and the operator did not set the env var
    explicitly. See [`CLAUDE.md`](CLAUDE.md) "SQLite per-driver defaults"
    for the full table.
- **Multi-tenancy across the stack** — tenant context plumbed end-to-end:
  - GraphRAG: in-memory stores partitioned per tenant + query context
    propagation. ([#27], RAN-37)
  - GraphRAG: `tenant_id` column on persisted entities/relationships,
    scoped reads, online backfill of legacy rows. ([#30], RAN-38)
  - Storage: tenant-scoped uniqueness constraint on `trace_id` so two
    tenants can ingest the same trace id without collisions. ([#29], RAN-21)
  - MCP + vector DB: tenant context on every MCP tool call, vector index
    isolation per tenant. ([#31], RAN-39, RAN-20)
- **Entra ID authentication, retention, and pre-UI hardening** —
  multi-tenant Entra integration, configurable per-tenant retention, plus
  the pre-UI request/response hardening pass. (`65bc069`)
- **Backend robustness for 100–200 services** — capacity, batching, and
  back-pressure work to support medium-sized OTLP fan-in without head-of-
  line blocking. ([#24])
- **OpenSSF Best Practices + Scorecard scaffolding** ([#34], RAN-53)
  - `.github/workflows/scorecard.yml` — supply-chain analysis on push to
    `main` + weekly cron + workflow_dispatch, SARIF → Security tab, all
    actions SHA-pinned per Scorecard `Pinned-Dependencies`.
  - `.github/workflows/security.yml` — consolidated OSS-CLI security stack
    (Semgrep, OSV-Scanner, Trivy, Gitleaks, jscpd, anchore/sbom-action),
    PR + push + weekly cron.
  - `.bestpractices.json` — canonical autofill schema for project
    [12646](https://www.bestpractices.dev/projects/12646), `level: passing`,
    per-criterion `*_status` + `*_justification` fields. ([#47], RAN-58)
  - `SECURITY.md` private-disclosure policy, `CLAUDE.md` operator/agent
    SSoT, README badge row.

### Changed

- **UI**: removed the Logs and Traces views; the Dashboard is the new default
  landing view. ([#98])
- **`internal/ui/dist` is no longer committed** ([#99]). The built UI is
  generated at release time, not stored on `main` — a plain `go build .` now
  serves no SPA at `/`; use `make build` or install a release tag.
- **MCP surface reduced from 21 tools to 7 triage-essential tools** ([#91]).
  Kept: `get_anomaly_timeline`, `get_service_map`, `get_service_health`,
  `root_cause_analysis`, `impact_analysis`, `trace_graph`, `search_logs`.
  Removed clients now receive `unknown tool` RPC errors — see CLAUDE.md
  "MCP Server" for the full keep/cut list and rationale.
- **`LOG_FTS_ENABLED` defaults to `true` on SQLite** ([#91]). FTS5 BM25
  ranking became the default log-search backend on the SQLite path,
  replacing the vectordb TF-IDF dispatch. Operators who need the ~30%
  disk savings can opt out via `LOG_FTS_ENABLED=false` + `POST /api/admin/drop_fts`.
- CI: replaced the deleted central-ops reusable workflow with a local
  `ci.yml` so otelcontext owns its quality gates without relying on an
  external repo. ([#26])
- Post-robustness follow-ups consolidated as a single chore pass over the
  100–200-service work. ([#25])

### Removed

- **`internal/vectordb/` package + `find_similar_logs` MCP tool** ([#91]).
  The TF-IDF semantic-search index added 163 MB steady-state RAM with a
  290 MB peak during FIFO eviction, plus a snapshot loop and DB
  tail-replay goroutine. Log similarity now comes from Drain template
  clustering plus FTS5 BM25 ranking. `data/vectordb.snapshot` is left
  on disk for operators to delete by hand.
- **`graph_snapshots` table + scheduler** ([#91]). The 15-minute
  topology snapshot path (`internal/graphrag/snapshot.go`) wrote ~480
  MB/day to disk for a `get_graph_snapshot` MCP tool nothing in
  production ever called. AutoMigrate no longer creates the table on
  fresh deploys; existing populated tables are left in place
  (`DROP TABLE graph_snapshots; VACUUM;` to reclaim disk).
- **`github.com/RandomCodeSpace/central-ops` Go module dependency** ([#91]).
  Replaced two tiny helpers (`pkg/version.Detect`, `pkg/httputil.CORSMiddleware`)
  with inline equivalents in `main.go` and `internal/mcp/server.go`.
  Build now succeeds against a 100% public module graph.

### Fixed

- **MCP SSE returned HTTP 500** ([#98]). `GET /mcp` failed with
  `SSE not supported` because the metrics middleware's `responseWriter`
  captured the status code and forwarded `Hijack` (for WebSocket) but dropped
  `Flush`, so the SSE handler's `w.(http.Flusher)` assertion failed. Forwarding
  `Flush` restores the `200 text/event-stream` stream and the UI live stream.
- **Dashboard p99 latency shown 1000× too large** ([#98]). `p99_latency` was
  microseconds rendered under an "ms" label (e.g. 4,430,763); converted µs→ms
  at the API view boundary and renamed the field to `p99_latency_ms` to match
  `avg_latency_ms`.
- **MCP console** ([#98]): format the `/mcp` SSE stream into concise lines
  (`graph · N svc · M edges · healthy/degraded/critical`) instead of raw
  JSON-RPC envelopes, and add `15m`/`1h`/`24h` quick presets to the
  `time_range` field (it was a bare input while the datetime fields had them).
- **OOM at ~120 services on SQLite under continuous load** ([#91]). At
  default config the binary did not survive an hour: ingest pipeline
  queue saturation under SQLite WAL contention pinned 0.5–5 GB of
  pending batches, GraphRAG permanent stores grew without TTL, TSDB
  ring buffer multi-GB at default cardinality. The 7-tool surface
  reduction + SQLite-tuned defaults bring steady-state RSS to ~1.8 GB
  with bounded bursts at the 120-service target.
- **MCP**: propagate `cfg.DefaultTenant` to the MCP fallback path so
  tools invoked without an explicit tenant resolve to the configured
  default rather than failing. ([#33], RAN-22)
- **Storage**: disable foreign-key creation during `AutoMigrate` to
  unblock Postgres boot — the schema's relational integrity is enforced
  in application code; gorm's `ALTER TABLE … ADD CONSTRAINT` was racing
  against the multi-tenant tenant_id backfill on first boot. ([#32], RAN-49)
- **UI**: switch the service map to a force-directed layout so nodes
  stop stacking on top of each other in dense graphs. (`adb6c76`)

### Security

- Go stdlib `go.mod` directive `1.25.10` → `1.25.11` ([#98]) — clears two
  stdlib advisories flagged by OSV-Scanner (GO-2026-5037, GO-2026-5039).
- **OSV-Scanner advisory clean-up** ([#91]):
  - `golang.org/x/crypto` v0.50.0 → v0.52.0 (12 advisories: GO-2026-5005..5023, 5033).
  - `golang.org/x/net` v0.53.0 → v0.55.0 (6 advisories: GO-2026-5025..5030).
  - `golang.org/x/sys` v0.43.0 → v0.45.0 (1 advisory: GO-2026-5024).
  - Go stdlib `go.mod` directive 1.25.9 → 1.25.10 (8 stdlib advisories).
  - `brace-expansion` 5.0.5 → 5.0.6 via npm overrides
    (GHSA-jxxr-4gwj-5jf2, CVSS 6.5).
- Adopted the OSS-CLI security stack as the project's continuous
  supply-chain observability surface (Semgrep + OSV-Scanner + Trivy +
  Gitleaks + jscpd + anchore SBOM). High/Critical findings are merge
  gates per `CLAUDE.md`. SARIF results land in the GitHub Security tab
  where supported and are uploaded as workflow artifacts otherwise.
- bestpractices.dev project [12646](https://www.bestpractices.dev/projects/12646)
  declared at `level: passing` via canonical autofill schema. ([#47])

[Unreleased]: https://github.com/RandomCodeSpace/otelcontext/compare/v0.2.0-beta.6...HEAD
[v0.2.0-beta.6]: https://github.com/RandomCodeSpace/otelcontext/compare/v0.0.11-beta.15...v0.2.0-beta.6
[#24]: https://github.com/RandomCodeSpace/otelcontext/pull/24
[#25]: https://github.com/RandomCodeSpace/otelcontext/pull/25
[#26]: https://github.com/RandomCodeSpace/otelcontext/pull/26
[#27]: https://github.com/RandomCodeSpace/otelcontext/pull/27
[#29]: https://github.com/RandomCodeSpace/otelcontext/pull/29
[#30]: https://github.com/RandomCodeSpace/otelcontext/pull/30
[#31]: https://github.com/RandomCodeSpace/otelcontext/pull/31
[#32]: https://github.com/RandomCodeSpace/otelcontext/pull/32
[#33]: https://github.com/RandomCodeSpace/otelcontext/pull/33
[#34]: https://github.com/RandomCodeSpace/otelcontext/pull/34
[#47]: https://github.com/RandomCodeSpace/otelcontext/pull/47
[#91]: https://github.com/RandomCodeSpace/otelcontext/pull/91
[#98]: https://github.com/RandomCodeSpace/otelcontext/pull/98
[#99]: https://github.com/RandomCodeSpace/otelcontext/pull/99
