# Security Policy

Thanks for helping keep otelcontext and its users safe.

## Supported versions

otelcontext is pre-1.0 and ships from `main`. The `main` branch is the only branch that receives security fixes; tagged releases are advisory snapshots.

| Version | Supported |
|---|---|
| `main` (HEAD) | ✅ |
| Tagged pre-releases | Best-effort — please upgrade to `main` HEAD |

If you are running an older snapshot, please pull `main` and reproduce against HEAD before reporting — the issue may already be fixed.

## Reporting a vulnerability

Please **do not open a public GitHub issue** for security problems.

Use one of:

- **GitHub private vulnerability report** — preferred. Open `https://github.com/RandomCodeSpace/otelcontext/security/advisories/new` (you must be signed in to GitHub). The advisory channel is monitored by the maintainer.
- **Email** — `ak.nitrr13@gmail.com`. Put `[otelcontext security]` in the subject so the report is triaged ahead of normal mail.

Please include:

- The otelcontext commit SHA or release tag (`./otelcontext --version` if available, otherwise `git rev-parse HEAD`).
- The shortest reproducer you can produce — a curl command, OTLP payload, or test case is ideal.
- Your assessment of impact (e.g., RCE, auth bypass, tenant isolation breakage, info-disclosure, DoS).
- Whether the issue is in a transitive dependency (please name the dependency + advisory ID if known).

## What you can expect

- Acknowledgement within **5 business days** of report receipt.
- A fix triage decision within **10 business days** for High/Critical issues.
- A coordinated disclosure timeline negotiated with you. The default embargo is **90 days** from acknowledgement, or until a patched release is published, whichever is sooner.
- Credit in the release notes (or anonymously, at your preference).

## Scope

In scope:

- The otelcontext binary (single Go process serving OTLP gRPC `:4317`, HTTP API + OTLP HTTP + UI + MCP `:8080`).
- All packages under `internal/` (ingestion, storage, GraphRAG, MCP, API, telemetry).
- The embedded React frontend under `ui/`.
- The OTLP/MCP wire protocol surface as exposed by this binary.
- The DLQ (Dead Letter Queue) on-disk format.

Out of scope (please report upstream):

- Vulnerabilities in third-party OTLP clients or SDKs that send to otelcontext — report to the OpenTelemetry project.
- Vulnerabilities in supported relational databases (SQLite, PostgreSQL, MySQL, MSSQL) themselves — report to the database vendor.
- Misconfiguration that exposes the platform without `API_KEY` set in production — that is operator error, not a vulnerability. The README is explicit about TLS + `API_KEY` for any non-dev deployment.

## Hardening references

- [`CLAUDE.md`](CLAUDE.md) — architecture, multi-tenancy model, configuration surface.
- [`.bestpractices.json`](.bestpractices.json) — OpenSSF Best Practices self-assessment evidence map.
- [`.github/workflows/scorecard.yml`](.github/workflows/scorecard.yml) — OpenSSF Scorecard supply-chain analysis (push + weekly).
- [`.github/workflows/security.yml`](.github/workflows/security.yml) — OSS-CLI security stack (OSV-Scanner, Trivy, Semgrep, Gitleaks, jscpd, SBOM).
- [`.github/dependabot.yml`](.github/dependabot.yml) — dependency update + security update channels.
- [`scripts/setup-git-signed.sh`](scripts/setup-git-signed.sh) — repo-local config for signed commits on `main`.

## Changelog

| Date | Change |
|---|---|
| 2026-04-26 | Initial policy under [RAN-53](/RAN/issues/RAN-53). |
