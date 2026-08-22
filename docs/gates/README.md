# Aggregate seven-day release gate

This directory holds the **reports** produced by the seven-day aggregate release gate (issue #202, map #195, closes #194's final checkbox). It does not hold the gate: that is `test/gate/`, behind the `gate` build tag.

There is no report here yet. A report appears only when someone runs the protocol and commits the output. **Never hand-write one.** The JSON is the source of truth and the Markdown is rendered from the same result struct, so a hand-edited report is a lie that the tooling cannot detect.

Naming: `<date>-aggregate-7day-gate.json` and `<date>-aggregate-7day-gate.md`.

---

## What the gate certifies

One sentence, and no more than one sentence:

> **Crash-durable on a surviving volume**, at 10k points/s sustained on two vCPU, inside the documented disk and memory budgets, answering the full seven-day range.

What it deliberately does not certify: host power-loss durability, Pod-reschedule or node-loss durability, or any behaviour above the burst rate. See the Durability Contract section of [`../OPERATIONS.md`](../OPERATIONS.md).

## The protocol

| Phase | What happens | Roughly |
|---|---|---|
| `prefill` | `test/aggprefill` writes a deterministic seven-day history: 6,000 series x 2,016 five-minute windows, store-level. OTLP replay of week-old data is impossible — the arrival-time lateness bound is about fifteen minutes — so the history is seeded through the store API. | 20-40 min |
| `server_start` | The server starts inside a cgroup-v2 transient scope (`CPUQuota=200%`, `MemoryMax=4G`). The gate reads `cpu.max` and `memory.max` back out of the kernel and refuses to proceed if the boundary is not what it asked for. | seconds |
| `main_load` | One `test/loadsim --direct` run: settle, then **3 h at 10k pts/s across 150 services**, then a **60 s burst at 20k pts/s**. The load generator runs **outside** the scope. | ~3 h |
| `post_burst` | A second load run whose settle window *is* the contract's two-minute recovery allowance. Its graded window starts once the allowance has elapsed, so "back inside sustained bounds within 2 min" is proven rather than averaged over. | 4 min |
| `quiet_gap` | Longer than one aggregate window, so the crash run never shares a window with anything before it. | 6 min |
| `crash_run` | A third load run, persisting a per-window **ACK ledger** to disk every two seconds. Partway through, the gate snapshots the on-disk ledger and sends **SIGKILL** to the server process, restarts it, and times `/ready`. It is long enough that at least two aggregate windows fall entirely inside it, so the per-window comparison has both an exact window and the crash-affected one. | 16 min |
| `measure` | Disk walk, final scrape, query completeness over the full seven-day range, threshold evaluation, report. | 2-5 min |

Total wall time is roughly four to five hours. **It is manual.** CI compiles the gate binary and runs its unit tests; it never runs the protocol on a shared runner.

## Running it

```bash
make gate-build                     # builds otelcontext, loadsim, aggprefill and the gate binary
make gate-run                       # runs the protocol with test/gate/gate.config.json
```

Exit status is 0 only when every assertion passed. The report is written to this directory either way — a failing gate produces a report that says why, which is the entire point.

To vary anything, edit `test/gate/gate.config.json` (unknown keys are rejected, so a typo fails fast rather than silently keeping a default) or pass `-config` yourself:

```bash
go build -tags gate -o bin/gate ./test/gate
./bin/gate -config path/to/gate.json -out docs/gates
./bin/gate -print-config            # dump the effective configuration and exit
```

For a plumbing dry run, copy the config and shorten `load.sustained_sec`, `prefill.windows` and `sampling.steady_start_offset_sec`. A short run is not a gate result and must not be committed here.

### Host requirements

- Linux with the cgroup-v2 unified hierarchy and a usable `systemd-run --user --scope`. Without it the gate falls back to `taskset -c 0,1` plus `GOMAXPROCS=2`, marks the run `taskset-fallback`, and states in the report that it validated dedicated-core behaviour rather than quota throttling — and that no kernel memory bound was applied.
- **A dedicated data volume.** The disk watchdog's used-bytes figure is `statfs` on the whole filesystem (`total - available`), not the size of the data directory. On a shared root filesystem that number is already far above `DATA_DISK_BUDGET_MB`, the watchdog enters `raw_off` at 95% of the budget, and `/ready` answers 503 for the rest of the run. The gate refuses to start in that state and prints the arithmetic rather than timing out on readiness three hours later. Mount `data_dir` on its own volume, or raise `server_env.DATA_DISK_BUDGET_MB` above the volume's existing usage — and say so in the report, because it is then no longer the budget #201 specified.
- Free disk: the seven-day prefill plus the live run needs comfortably more than the 7 GiB the gate asserts against. Budget 20 GiB.
- Nothing else on ports 8080/4317, and no other OtelContext instance pointed at the same data directory.

## Thresholds

Every one of these is encoded in `test/gate/gatecore/thresholds.go` and produces exactly one row in the report's assertion table, pass or fail. Missing evidence is a FAILED row, never a blank cell.

| Area | Threshold |
|---|---|
| Sustained | 3 h at 10k pts/s; ACK p99 <= 250 ms; ACKed/sent >= 99.9%; zero `RESOURCE_EXHAUSTED`; zero silent aggregate drops (`late_points`, `admission_rejected`, `identity_overflow` deltas all zero); no sustained backlog growth |
| Burst | 20k pts/s for 60 s; backpressure permitted; no crash, no OOM; ACK p99 and writer backlog back inside sustained bounds within 2 min of burst end |
| Recovery | `/ready` <= 60 s after restart; `SkippedSeries` = 0; no acknowledged aggregate loss |
| Memory | cgroup `memory.peak` <= 4 GiB, zero `oom_kill`; `VmHWM` recorded as secondary evidence |
| Disk | main tier <= 4.5 GiB **projected**; `aggregate.db` <= 1.5 GiB demonstrated; DLQ <= 0.5 GiB; WAL/temp/TLS <= 0.5 GiB; total data dir <= 7 GiB; free headroom >= 1 GiB |
| Queries | exact window coverage over the seeded range; each surface declares the coverage marker the contract expects **of that surface**; no `truncated=true`; the five aggregate-backed MCP tools answering over the full seven-day range |

### The crash-interval bound

The aggregate write path is **at-least-once** across a crash: a request whose transaction committed but whose ACK never arrived is a legitimate survivor. For the window a crash lands in the gate therefore asserts a range, not an equality:

```
confirmed-ACKed <= post-restart total <= all attempted
```

Demanding equality there would contradict the documented contract and would fail runs that behaved correctly. Windows the crash did not touch carry `attempted == acked` in the ledger, so the identical rule collapses to an exact equality — one code path, two guarantees, no chance of the two disagreeing.

The ledger is keyed on **data time, not call time**. `internal/aggregate` selects a span's window from its START time (`SpanInput.Timestamp`), and the load generator backdates span start by up to a batch interval plus the span duration. A ledger keyed on the Export's own clock would misattribute roughly a second of spans at every window boundary, and the exact-equality assertion above would fail on arithmetic that has nothing to do with durability. Each batch therefore carries a per-window `Contribution` derived from the timestamps in the payload, and one batch legitimately splits across two windows.

The comparison also skips the crash run's first and last windows: a window shared with the quiet gap or with the run's own tail would carry a second contributor, and the arithmetic would be meaningless. That is why the crash run is sized so at least two whole windows fall inside it.

### Coverage is asserted per surface, not globally

Each aggregate-backed surface declares the coverage its own answer earned, and the gate asserts that exact string rather than a blanket `full`. `/api/metrics/traffic`, `/api/metrics/dashboard` and `/api/metrics/service-map` all answer wholly from the engine and must declare `full`. The expectation is `expect_coverage` per surface in `test/gate/gate.config.json`, not a global flag, so a handler that ever downgrades its marker for an honest reason is a one-line config change here instead of a reason to loosen the gate.

### The main-tier projection

The main (raw exemplar) tier is gated on a **projection**, and the report labels it as one with its sample count, observed range and fit quality. The gate samples the tier's physical allocated size — main DB, indexes, FTS shadow tables, WAL/SHM sidecars, free pages — at every tick of the steady portion, fits physical bytes per completed five-minute window, and multiplies by the 576-window two-day horizon. The conservative upper estimate (point estimate plus two slope standard errors) is what the 4.5 GiB threshold is applied to.

If a logical charged-bytes counter is configured, it is used **only** to report the measured amplification factor. It is never multiplied back into the slope: the slope is already physical, so doing that would charge the indexes twice. `TestFitProjectionAmplificationIsReportedNotApplied` exists to keep it that way.

## Metric gaps

The gate records, in every report, the places where the contract asks for something the platform does not expose. As of the tooling landing:

- `RecoveryStats.SkippedSeries` — the corruption signal gated at zero — and `SeededBaselines` have **no Prometheus gauge**. Only `otelcontext_aggregate_recovery_duration_seconds` and `otelcontext_aggregate_recovery_rows{kind}` (four classes: `replayed`, `finalized_windows`, `topology_restored_rows`, `topology_restored_windows`) are published. `promStoreRecorder.RecordRecovery` receives the whole `RecoveryStats` and publishes neither field, so the gate parses the server's own slog line for the rest.
- The aggregate query API carries **no `truncated` field**: the engine pages every store read to completion, so truncation never reaches the wire on `/api/metrics/*`. Completeness there is asserted through the coverage marker and exact window coverage; the literal `truncated=false` check applies where the field exists (exemplar-backed responses).
- `test/aggprefill` reports windows, bucket rows and delta rows, but **not per-window observation totals**, so the prefill tier's exact scalar check is window coverage rather than span-count equality.
- There is **no process-resident-memory collector**; memory evidence is cgroup `memory.peak` / `memory.events`, with `/proc/<pid>/status` `VmHWM` secondary.
- **No metric reports logical charged bytes** for the main tier, so the amplification factor is unmeasured. The projection does not need it.
- **An aggregate drop counter that never fires emits nothing at all.** `otelcontext_aggregate_late_points_total` and `otelcontext_aggregate_admission_rejected_total` are counter *vectors*, and `client_golang` omits a vector with no children entirely — no sample, no `HELP`, no `TYPE`. A correctly-zero drop counter is therefore byte-identical to a deleted one. The gate resolves this with a witness: if `otelcontext_aggregate_input_points_total` is present and the drop counter is not, the family is registered and the counter never fired, and the assertion passes with a **degraded** basis that says so in the report. If the witness is missing too, the gate fails rather than guess.

Closing any of these is a change to `internal/`, which the gate tooling deliberately does not make.

## Closing #194

Issue #194 closes only by referencing a **committed passing report** in this directory. A green CI run proves the gate compiles; it does not prove the platform survives seven days.
