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

- CI: replaced the deleted central-ops reusable workflow with a local
  `ci.yml` so otelcontext owns its quality gates without relying on an
  external repo. ([#26])
- Post-robustness follow-ups consolidated as a single chore pass over the
  100–200-service work. ([#25])

### Fixed

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
