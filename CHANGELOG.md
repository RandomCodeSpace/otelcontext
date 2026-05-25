# Changelog

All notable changes to **otelcontext** are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)
once a stable release line exists. While otelcontext remains pre-1.0, every
commit on `main` is the canonical version identifier (`git rev-parse HEAD`).
Per-tag pre-release notes are published on
[GitHub Releases](https://github.com/RandomCodeSpace/otelcontext/releases).
This file's `Unreleased` section tracks what has landed on `main` since the
last published pre-release tag (`v0.0.11-beta.15`).

## [Unreleased]

### Added

- **SQLite survival tuning for 120-service production load** ([#91]):
  - Fail-closed PRAGMA stanza in `internal/storage/factory.go` —
    `journal_mode=WAL`, `synchronous=NORMAL`, 256 MB page cache, 1 GB
    mmap, 64 MB WAL cap, `busy_timeout=5000`.
  - Per-driver config defaults — `config.applyDriverDefaults` overrides
    9 tunables (conn pool, ingest workers/queue, metric cardinality,
    store-min-severity, sampling rate, gRPC stream cap, `LOG_FTS_ENABLED`)
    when `DB_DRIVER=sqlite` and the operator did not set the env var
    explicitly.
  - Design spec at
    [`docs/superpowers/specs/2026-05-24-mcp-7tool-sqlite-survival-design.md`](docs/superpowers/specs/2026-05-24-mcp-7tool-sqlite-survival-design.md).
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

[Unreleased]: https://github.com/RandomCodeSpace/otelcontext/compare/v0.0.11-beta.15...HEAD
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
