# Limited-production readiness — v0.4.0-rc.3

Delivery record for [#254](https://github.com/RandomCodeSpace/otelcontext/issues/254),
the final child of epic [#243](https://github.com/RandomCodeSpace/otelcontext/issues/243).
Every conclusion below is copied from `release-candidate-v1.json` in the retained
workflow artifact; nothing here was decided by hand.

## Candidate identity

| Field | Value |
|---|---|
| Tag | [`v0.4.0-rc.3`](https://github.com/RandomCodeSpace/otelcontext/releases/tag/v0.4.0-rc.3) (annotated, pushed once) |
| Source SHA | `f49d7c84c6204bba78b97b7bb218a1a83815d73c` on protected `main` |
| Release workflow | [run 33752423893](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893) at `refs/tags/v0.4.0-rc.3`, event `push` |
| Evidence artifact | `release-candidate-v0.4.0-rc.3-f49d7c84c6204bba78b97b7bb218a1a83815d73c` (90-day retention) |
| Go toolchain | go1.25.13 |
| Signature identity | `https://github.com/RandomCodeSpace/otelcontext/.github/workflows/release.yml@refs/tags/v0.4.0-rc.3` |
| `checksums.txt` SHA-256 | `84ea01045d6392547438705c202b232750e7fdea64994e49cf266e4bbab7b1de` |
| Linux amd64 binary SHA-256 | `0576ed496e04b3d5453ba3d816409ba85baa61ba7326a841488e8d18dd42ae00` |
| Linux arm64 binary SHA-256 | `bf9731c84c2adadd09fcababca955904d3b6bfd0f29968c1179658417a09da23` |
| Published | 2026-09-03T12:04:46Z, pre-release, by the `publish` job after `limited_production.approved = true` |

## Profile conclusions

| Profile | Conclusion | Basis |
|---|---|---|
| PostgreSQL 16, legacy, unpartitioned, one process | **approved** for limited production | primary-tier lifecycle proof: exact migration state, candidate backup CLI restore to a fresh target, fingerprints equal |
| SQLite, legacy, one process, ≤ 5 services, documented low write rate | **approved** as bounded opt-in | bounded-tier lifecycle proof: exact migration state, candidate backup CLI restore to a fresh target, fingerprints equal |
| MySQL 8.4 | **preview** | adapter proof green; migration `unverified-preview`; native restore to a fresh target, fingerprints equal |
| SQL Server 2022 | **experimental** | adapter proof green; migration `unverified-preview`; native restore to a fresh target, fingerprints equal |
| `aggregate` | available, **unapproved** | aggregate gate `not_run` (see below) |
| `aggregate-shadow` | available, **unapproved** | same |

Blocking failures for limited production: none.

## Proof jobs at the tag ref

| Job | Result | Link |
|---|---|---|
| source identity | success | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100638811379) |
| goreleaser | success | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100638955571) |
| verify signed assets | success | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100640003973) |
| database proof · sqlite | success | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100640243554) |
| database proof · postgres-16 | success | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100640243499) |
| database proof · mysql-8.4 | success | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100640243625) |
| database proof · sqlserver-2022 | success | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100640243581) |
| browser smoke · release binary | success (Chrome 151, desktop and mobile phases through `mobile-map-list-inspector`) | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100640243260) |
| systemd proof · release archive | success (install, migrate, restart, crash, pressure, backup, upgrade from `v0.4.0-beta.2`, rollback to it, cleanup) | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100640243381) |
| linux arm64 smoke | success (6 assertions, `/live` 1015 ms, `/ready` 1024 ms) | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100640243300) |
| release candidate manifest | success | [job](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33752423893/job/100641047957) |

## Aggregate certification

`aggregate-release-gate.yml` needs a self-hosted runner labelled
`otelcontext-aggregate-gate` with enforced cgroup limits. The repository has no
runner registered, so the protocol did not run and the manifest records
`aggregate_production.status = not_run`. Aggregate and shadow modes stay
available and unapproved. Registering the runner and dispatching the gate
against this same signed Linux amd64 archive is the only remaining step; it
never changes the limited-production conclusion above.

## Candidates that did not publish

Tags are never moved or reused. Both earlier candidates remain unpublished
drafts with their evidence attached.

| Tag | Failure | Corrective change |
|---|---|---|
| `v0.4.0-rc.1` | `verify signed assets`: archives carried a single `deploy/systemd` file instead of the unit and env example | #275 (`.goreleaser.yaml` plain file entries) |
| `v0.4.0-rc.2` | `linux arm64 smoke`: disk watchdog `raw_off` on the oversized runner volume; `database proof · mysql-8.4`: batched purge failed (error 1093); `database proof · sqlserver-2022`: empty `CompressedText` bound as nvarchar into `varbinary(max)` (error 257) | #276 |

The MySQL and SQL Server failures were ingestion and retention defects in the
candidate itself. They were found only because the gate runs every retained
adapter against the exact release binary.

## Rollback record

- Previous signed release: `v0.4.0-beta.2`. The systemd proof upgraded from it
  to this candidate and rolled back to it with database state retained;
  fingerprints before and after are in the evidence artifact.
- Rollback path in operation: stop the unit, restore the previous archive's
  binary, keep the data directory, start the unit. Schema migrations added
  since `v0.4.0-beta.2` are recorded in `migrate-status-*` evidence files.
