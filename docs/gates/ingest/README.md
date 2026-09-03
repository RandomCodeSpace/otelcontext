# Ingest baselines and A/B records

This directory holds the committed **ingest baselines** and the **A/B
records** produced by the ingest baseline protocol (`scripts/ingest-baseline.sh`;
issue #293, decision #282). The protocol starts the exact `otelcontext`
binary in `AGGREGATE_MODE=aggregate`, pinned to two CPUs with
`GOMAXPROCS=2`, drives it with `loadsim --direct` (profile
`aggregate-acceptance`, 150 services, ~10,050 points/s offered) and records
what the server acknowledged, how fast, and where its CPU went.

Files:

- `<date>-aggregate-ingest-baseline.json` — schema
  `otelcontext.ingest-baseline.v1`, one per committed baseline.
- `<date>-aggregate-ingest-baseline.cpu.pprof` — the CPU profile that
  baseline's JSON names (`go tool pprof -top <binary> <file>`).
- `<date>-<finding>-ab.json` — schema `otelcontext.ingest-ab.v1`, one per
  attempted change, accepted or not. A negative result is committed too: it
  is the evidence that the change was tried and did not pay.

**Never hand-write or edit one.** The JSON is the source of truth; the
Markdown the workflow posts is rendered from it.

## The protocol

1. Empty data directory. Server env is the seven-day gate's `server_env`
   (`test/gate/gate.config.json`) with `DATA_DISK_BUDGET_MB=1000000` and
   `API_RATE_LIMIT_RPS=0`, so a near-full runner disk or the API limiter
   cannot fail `/ready` for reasons unrelated to ingest. The full env is
   recorded under `server_env`.
2. `taskset -c 0,1` + `GOMAXPROCS=2` for the server; `taskset -c 2,3` for
   loadsim. This is the gate's taskset fallback, not its cgroup quota: it
   validates dedicated-core behaviour, not throttling.
3. `loadsim --direct --profile aggregate-acceptance --settle 30s
   --duration 120s`. Only the `sustained` phase is recorded. The start is
   **aligned to the wall clock** so that exactly one five-minute window
   boundary falls 60 s into the sustained phase (`BOUNDARY_OFFSET_SEC`;
   `protocol.window_boundary_unix` records which one). A boundary opens a
   fresh delta row for every active series in one group commit — the
   largest single ACK stall the steady state has — and whether a 120 s
   window happened to contain one moved p99 by 3x between otherwise
   identical runs. Pinning it makes a pair comparable and keeps the
   rollover inside what p99 measures, as it is in the seven-day gate. The
   alignment wait is at most five minutes.
4. At sustained + 30 s, a 60 s CPU profile and an allocation profile are
   taken from `PPROF_ADDR`. The profile window sits inside the measured
   window on purpose: the numbers include the profiler's overhead, so a
   baseline and a candidate carry the same overhead.
5. `/metrics/prometheus` is dumped before and after; the deltas of the
   aggregate input/commit/admission/lateness counters are recorded so a
   throughput number can be checked against what the engine says it took.

The hosted weekly run (`.github/workflows/ingest-baseline.yml`, Sundays,
plus `workflow_dispatch`) executes the same script and uploads
`ingest-baseline-<sha>` for 90 days. It is never a required check.
`workflow_dispatch` takes an optional `baseline` (a file in this directory;
default the newest) and an optional `pr_number`; with a PR number the
comparison is posted as a comment. Hosted numbers are informational: the
A/B that counts is a same-hardware pair.

## What one baseline contains

- `git_sha`, `go_version`, `binary_sha256`, `loadsim_sha256` — what ran.
- `hardware` — CPU model, CPU count, memory, kernel, the pinned CPU sets,
  `is_gate_box` (always `false` here; the seven-day gate box is a different
  machine) and a free-text `note`.
- `protocol` — profile, services, offered rate, settle/sustained seconds,
  profile offset and length, wall-clock start and end.
- `sustained` — from loadsim's report: `points_sent`, `points_acked`,
  `points_acked_per_sec`, request outcomes, and
  `ack_latency_all_signals` / `ack_latency_by_signal` with p50/p90/p95/p99/
  p99.9/max/mean in milliseconds. ACK latency is the time from Export call
  to durable-ACK response as the client sees it.
- `server_counters_delta` — engine counters over the whole run (settle
  included).
- `artifacts` — the sibling files the run produced.

## The A/B rule (from #282)

`scripts/ingest-baseline.sh compare BASE CAND OUT` writes
`otelcontext.ingest-ab.v1` with:

- `acknowledged_identical` — every offered point acknowledged on both
  sides (`points_acked == points_sent`, zero loss, zero
  `resource_exhausted`) with the offered totals within 0.01 % of each
  other. Required. loadsim's tick-based emitters vary the offered total by
  a few points per run (1,206,002 vs 1,206,011 in the first pairs), and that
  drift belongs to the generator, not the server; a change that fails to
  acknowledge what it was offered is not a performance change.
- `throughput_ratio` = candidate ÷ baseline `points_acked_per_sec`;
  `ack_p99_ratio` = candidate ÷ baseline `ack_latency_all_signals.p99_ms`.
- `verdict`:
  - `keep` — identical totals AND (`ack_p99_ratio <= 0.9` with throughput
    within `tolerance` of equal, OR `throughput_ratio >= 1.1` with p99
    within `tolerance` of equal). `tolerance` is 0.05 unless
    `AB_TOLERANCE` says otherwise, and is recorded.
  - `revert` — anything else on the same hardware.
  - `incomparable` — different `hardware.cpu_model`. Compare a hosted run
    with a hosted run and a workstation run with a workstation run.

Read a `keep` as "this pair met the rule", not as a guarantee: hosted and
workstation numbers move between runs, and p99 moves more than p50. Run
the machine otherwise idle (a concurrent build or test run on the same
cores changed p99 by 2x in the first attempts), alternate baseline and
candidate runs, and when a result is close to the threshold run the pair
again before trusting it. The committed A/B file is one pair; its
`notes` say how many pairs were run and what they showed.

## Findings (2026-09-03, workstation pairs)

| Finding | Owner | Baseline share | ACK p99 pair | Verdict | Record |
|---|---|---:|---|---|---|
| Batched delta-log pre-read in the group commit | `store_sqlite.go` `mergeDeltas` | 35.5 % (`prepareV2` re-parse 21.9 %) | 154.4 → 66.4 ms (0.43) | keep | `2026-09-03-delta-log-batched-read-ab.json`, `-bench-before.txt`, `-bench-after.txt` |
| Skip the per-Export topology prune when the cutoff has not advanced | `topology.go` `fold` | 14.6 % | 154.4 → 260.4 ms (1.69); other pairs 0.73, 1.49, 1.00, 0.82 | revert (not consistent) | `2026-09-03-topology-prune-skip-ab.json` |
| Multi-row `INSERT OR REPLACE` (8 rows per statement), stacked on the first | `store_sqlite.go` `mergeDeltas` | 13.5 % remaining re-parse | 66.4 → 67.2 ms (1.01), p50 worse | revert | `2026-09-03-delta-log-multirow-insert-ab.json` |

What remains above 10 % after the kept change is kernel I/O under the
commit and the network (`Syscall6`, ~17 %), the driver's statement
re-parse spread over every remaining per-row `Exec` (~13 %), and the
exemplar tier's GORM batch writes (`storage.BatchCreateAll`, ~12 %), which
is outside the owners #293 names. The offered load fixes throughput at
10,050 points/s in this harness, so only the p99 arm of the rule can be
met here; a throughput claim needs the burst phase or a higher profile.

## Regenerating

```bash
CGO_ENABLED=0 go build -trimpath -o /dev/shm/ib/otelcontext .
CGO_ENABLED=0 go build -trimpath -tags loadtest -o /dev/shm/ib/loadsim ./test/loadsim

OTELCONTEXT_BIN=/dev/shm/ib/otelcontext LOADSIM_BIN=/dev/shm/ib/loadsim \
OUT_DIR=/dev/shm/ib/run HARDWARE_NOTE="describe the machine honestly" \
  scripts/ingest-baseline.sh run

cp /dev/shm/ib/run/ingest-baseline-v1.json docs/gates/ingest/$(date +%F)-aggregate-ingest-baseline.json
cp /dev/shm/ib/run/cpu.pprof             docs/gates/ingest/$(date +%F)-aggregate-ingest-baseline.cpu.pprof
```

For an A/B, run the protocol on the baseline commit and on the candidate
commit on the same machine, then
`scripts/ingest-baseline.sh compare base.json cand.json docs/gates/ingest/<date>-<finding>-ab.json`.

Ports default to 18080/14317/16060 so a developer's own instance on
8080/4317/6060 is left alone; override with `HTTP_PORT`, `GRPC_PORT`,
`PPROF_PORT`.
