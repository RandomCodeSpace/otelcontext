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

---

## Candidate v0.5.0-rc.2 — host-aware topology and measured performance (epic #285)

### Candidate identity

Every conclusion in this section is copied from `release-candidate-v1.json` in the retained workflow artifact.

| Field | Value |
|---|---|
| Tag | [`v0.5.0-rc.2`](https://github.com/RandomCodeSpace/otelcontext/releases/tag/v0.5.0-rc.2) (annotated, pushed once) |
| Source SHA | `b7a5d48f9a587d89fa437166dfa64ac5b1b9e25c` on protected `main` |
| Release workflow | [run 33825197780](https://github.com/RandomCodeSpace/otelcontext/actions/runs/33825197780) at `refs/tags/v0.5.0-rc.2`, event `push` |
| Evidence artifact | `release-candidate-v0.5.0-rc.2-b7a5d48f9a587d89fa437166dfa64ac5b1b9e25c` (90-day retention) |
| Go toolchain | go1.25.13 |
| Signature identity | `https://github.com/RandomCodeSpace/otelcontext/.github/workflows/release.yml@refs/tags/v0.5.0-rc.2` |
| `checksums.txt` SHA-256 | `462d62f8605032935f49b49b544e6d09573586d25139a55f0c7c389426ea7f38` |
| Linux amd64 binary SHA-256 | `fff8b882b11d7f8129ffefabd408c5cde9e5faa03f853254384a1c701e901b6d` |
| Linux arm64 binary SHA-256 | `1f77fc60fcc6d7eb8e801aa87609e3ab84f998b7dbefd7d89943e43291c50843` |
| Published | 2026-09-04T01:25:23Z, pre-release, by the `publish` job after `limited_production.approved = true` |
| `limited_production.approved` | `true`, no blocking failures; profiles `sqlite-legacy-bounded` and `postgres-16-legacy` approved, `mysql-8.4` preview, `sqlserver-2022` experimental, `aggregate` and `aggregate-shadow` available-unapproved |
| `aggregate_production.status` | `not_run`: no self-hosted runner carries the `otelcontext-aggregate-gate` label (zero runners registered at cut time); aggregate mode stays available and unapproved, and dispatching the gate against this same signed Linux amd64 archive never changes the limited-production conclusion |

Eleven commits on `main` since `v0.4.0-rc.3`: the epic's nine children as squash merges, in dependency order, plus the browser-smoke test fix from the first candidate.

| Child | Pull request | Merge |
|---|---|---|
| #286 bounded resource registry at ingest | #296 | `d11e5ec` |
| #287 host-only telemetry and resource-sourced dims | #297 | `04e1c81` |
| #288 hosts on REST, WebSocket and MCP | #298 | `bf4241e` |
| #289 read-latency proof and baseline | #299 | `552e714` |
| #291 measured percentiles in legacy service-graph paths | #300 | `4eff0d0` |
| #294 UI host grouping and host panel | #301 | `32bb036` |
| #290 read-latency objectives on the wide-range paths | #302 | `60952d4` |
| #293 ingest profiling and A/B rule | #305 | `7e29e20` |
| #292 RSS witness and memory objectives | #304 | `05f3a1c` |
| #295 browser smoke host-chip race (test only) | #306 | `b7a5d48` |

### Profile conclusions

**Bounded SQLite profile (legacy and aggregate on one host).** Every #281 read objective holds at the CI depth of 144 five-minute windows and 120 services, on the hosted runner and locally. The legacy shape's RSS steady p95 is 379 MiB hosted against a 512 MiB objective; the aggregate shape's is 973 MiB against 2 GiB. The main SQLite page cache is now fixed at 64 MB with `mmap_size` 0 by default (#304): the pure-Go driver's allocator holds about twice the configured cache as anonymous RSS outside the Go heap, invisible to `GOMEMLIMIT`, and no smaller change fit the objective on the hosted runner. Operators can restore the old sizing with `SQLITE_CACHE_SIZE_KB` and `SQLITE_MMAP_SIZE_BYTES` at that RSS cost.

**Seven-day read depth is not met and is recorded as such.** At 2,016 windows (`aggregate.db` 2.23 GB) the aggregate dashboard full-range cold read measured 15.5 s, traffic warm p99 2.3 s with 27 of 200 requests in budget, and service-map cold 13.5 s. The cost is linear in the roughly two million sketch-bearing rows a seven-day range holds; #302's optimizations divide it by the read-pool width and no constant-factor change closes a 7× gap. The structural fix, a per-window rollup written by the finalizer (aggregate store schema v6), is #303, outside this epic. The required CI checks enforce the 144-window objective; the seven-day numbers are in the #302 record and are not a pass.

**Ingest.** The committed baseline (`docs/gates/ingest/`) and profile found the group commit's per-row delta-log point read paying the driver's statement parser more than the B-tree. Batching that read into one SELECT per window and chunk of 500 series (#305) measured ACK p99 154 → 66 ms and p50 50 → 31 ms at 10,049 points/s with zero loss, server CPU 118% → 81% of a core, in the #282 harness on this machine (two cores pinned). Two other attempts were reverted with their A/B recorded. The weekly `ingest-baseline.yml` ran once on a hosted runner after merge (run 33817180705, 5 min): 10,049 points/s, zero loss, ACK p50 24 ms, p99 291 ms. That p99 is hosted-runner hardware, not a regression; the committed baseline came from this machine, so weekly comparisons are cross-hardware until a hosted artifact is promoted to the committed baseline.

### Host feature evidence

- **Topology proof** (`test/topologyproof`, all three modes): host entities register from resource attributes, a metrics-only host appears as `host/<name>` on the service map and in `/api/hosts`, per-host dims split `system.cpu.utilization:host.name`, and service nodes carry `kind`, `host_count` and `hosts` additively. Six host assertions per mode.
- **Latency proof** (same package, 14 assertions per mode): legacy service-graph p99 is `measured/ordered_rank` inside the 500k-span bound and `approximate` from the GraphRAG sketch above it, never a bare average multiplier without the label.
- **Browser smoke at the tag ref** (phase `host-group`, protected feature `host-grouping-and-panel`): the by-host toggle, host clusters, the host panel and the `?group=host` deep link render against the release binary.
- **MCP**: `get_service_map` with `group_by: "host"` returns `{services, hosts}`; `get_service_health` carries `hosts`. Covered by `internal/mcp` honesty tests and the topology proof.
- **Read-latency proof at the tag ref** is a required check (`read latency proof · legacy|aggregate`), so the candidate's own commit carries its numbers and the RSS series in the CI artifacts.

### Proof jobs at the tag ref

| Job | Conclusion | Wall time |
|---|---|---|
| source identity | success | under 1 min |
| goreleaser | success | 3 min |
| verify signed assets | success | under 1 min |
| database proof · sqlite | success | under 1 min |
| database proof · postgres-16 | success | 2 min |
| database proof · mysql-8.4 | success | 1 min |
| database proof · sqlserver-2022 | success | 1 min |
| browser smoke · release binary | success | 1 min |
| systemd proof · release archive | success | 2 min |
| linux arm64 smoke | success | under 1 min |
| release candidate manifest | success | under 1 min |
| publish | success | under 1 min |

The read-latency and RSS proofs are required checks on `main`, so they ran on the candidate's source commit rather than at the tag ref; their artifacts for `b7a5d48` are the `read-latency-legacy-*` and `read-latency-aggregate-*` uploads of that CI run.

### Candidates that did not publish

| Candidate | Commit | Outcome |
|---|---|---|
| `v0.5.0-rc.1` | `05f3a1c` | Every proof passed except `browser smoke · release binary`, which failed in the `host-group` phase: the test focused the `node-b` host chip and clicked `document.activeElement` in a second round-trip, and a live snapshot re-rendered the inspector body in between, so the click landed on `<body>`. Same code passed the identical phase on `main`; the DOM snapshot in the run artifact shows the inspector still on `checkout`. Test-only race, fixed by #306 (one evaluation focuses and activates the chip). A `workflow_dispatch` rerun at the tag ref (run 33823917642) could not re-prove the assets because the proof jobs skip transitively when `goreleaser` is skipped; recorded as #307. Tag retained, never reused; draft left unpublished. |

### Rollback record

None required. Every child rolls back per commit; the SQLite sizing change is reverted per deployment by the two environment overrides above.
