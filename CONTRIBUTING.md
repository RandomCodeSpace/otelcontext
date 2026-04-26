# Contributing to otelcontext

Thanks for considering a contribution. otelcontext is a single-binary OTLP observability platform written in Go (with an embedded React UI). Contributions are welcome via GitHub pull requests.

## Reporting bugs

- Functional bugs: open a [GitHub Issue](https://github.com/RandomCodeSpace/otelcontext/issues/new). Include a reproducer (config + minimal OTLP payload or HTTP/gRPC steps) and the version/SHA.
- Security issues: **do not** open a public issue. Follow [`SECURITY.md`](SECURITY.md) — preferred channel is a [private GitHub Security Advisory](https://github.com/RandomCodeSpace/otelcontext/security/advisories/new).

## Development workflow

1. Fork and create a topic branch off `main` (e.g. `fix/postgres-tls-mode`).
2. Make focused, atomic commits in [Conventional Commits](https://www.conventionalcommits.org/) style (`feat:`, `fix:`, `refactor:`, `chore:`, `docs:`, `test:`, `ci:`, `perf:`).
3. Open a PR against `main`.

`main` is the only protected branch. Direct pushes are blocked; signed commits are required.

## What every PR must pass

CI gates every PR on the following — please run them locally before requesting review:

| Check | Command | Workflow |
|---|---|---|
| Build | `go build ./...` | `.github/workflows/ci.yml` |
| Vet | `go vet ./...` | `.github/workflows/ci.yml` |
| Tests (race-enabled) | `go test -race -timeout 180s ./...` | `.github/workflows/ci.yml` |
| Lint | `golangci-lint run` (config in [`.golangci.yml`](.golangci.yml)) | `.github/workflows/ci.yml` |
| SCA — Go modules + npm | `osv-scanner --lockfile=go.mod --lockfile=ui/package-lock.json` | `.github/workflows/security.yml` |
| SCA + OS — filesystem | `trivy fs . --severity HIGH,CRITICAL --exit-code 1 --ignore-unfixed` | `.github/workflows/security.yml` |
| SAST | `semgrep scan --error --config p/security-audit --config p/owasp-top-ten --config p/golang` | `.github/workflows/security.yml` |
| Secret scan | `gitleaks detect --source . --redact --no-banner --exit-code 1` | `.github/workflows/security.yml` |
| Duplication | `jscpd --threshold 3 --min-tokens 100` (`internal/`, `ui/src/`) | `.github/workflows/security.yml` |

Merge is blocked on **any** High/Critical finding from OSV-Scanner, Trivy, Semgrep `ERROR`, or Gitleaks. See [`SECURITY.md`](SECURITY.md) and [`CLAUDE.md`](CLAUDE.md) "Security & Supply Chain" for the full policy.

## Tests

- **New behaviour requires a test.** Unit tests live next to the code they exercise (`*_test.go`). Examples: `internal/graphrag/drain_test.go`, `internal/ingest/otlp_e2e_test.go`, `internal/storage/...`. Run them with `go test -race ./...`.
- **Bug fixes require a regression test** that fails on the prior `main` and passes on the fix.
- The `loadtest` build tag covers the synthetic ingestion harness under `test/loadsim/`; CI verifies it compiles via `go build -tags loadtest ./test/loadsim/...`.

## Project layout

See [`CLAUDE.md`](CLAUDE.md) for the architecture overview, key directory map, ingestion flow, GraphRAG layered stores, MCP tool catalogue, retention behaviour, and configuration surface (40+ env vars). Read it before making non-trivial changes.

Hard rules from `CLAUDE.md` worth repeating here:

- Use native Go `net/http` (no Express/Gin/Echo).
- Use Mantine UI v8 for the React frontend (no Tailwind).
- Single-service architecture; embedded internal DBs only.
- Relational DB (SQLite/Postgres/MySQL/MSSQL) is the source of truth.
- New graph work goes in `internal/graphrag/`, not the legacy `internal/graph/`.

## Signed commits

Commits to `main` must be cryptographically signed. Configure your local git identity once:

```bash
./scripts/setup-git-signed.sh
```

The script supports SSH, OpenPGP, and X.509 signing keys.

## License

By submitting a contribution you agree it is licensed under the [MIT License](LICENSE.md), consistent with the rest of the repository.
