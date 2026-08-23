# Aggregate seven-day gate — 2026-08-23

Rendered from `2026-08-23-aggregate-7day-gate.json`. That JSON is the source of truth; every number below is read straight out of it.

| | |
|---|---|
| Run id | `20260823T115819Z` |
| Schema | `otelcontext.aggregate-7day-gate/v1` |
| Gate version | `1.0.0` |
| Started | 2026-08-23T11:58:19Z |
| Ended | 2026-08-23T15:55:10Z |
| Wall time | 3h56m50s |

## Verdict: PASS

61 assertions, 61 passed, 0 failed.

No failures.

## Provenance

| | |
|---|---|
| Commit | `6b90740ee708ade8d8355015af62601ac927e6a6` |
| Branch | `feat/gate-budget-rebalance` |
| Dirty tree | true |
| Dirty files | `?? docs/gates/2026-08-22-aggregate-7day-gate.json`, `?? docs/gates/2026-08-22-aggregate-7day-gate.md`, `?? docs/gates/2026-08-23-aggregate-7day-gate.json`, `?? docs/gates/2026-08-23-aggregate-7day-gate.md`, `?? gate` |
| Go | `go1.26.2` |
| sha256 `loadsim (loadsim)` | `19de8c0a699c2a511fbbc7a93aef5698037cfb2beaed5828a45eb0b77c16bdb1` |
| sha256 `prefill (aggprefill)` | `f641ae411563ea2be63c278ab85a80d5c21393c72ef8a3cd2eac74b09dc0d65d` |
| sha256 `server (otelcontext)` | `c83374064277123e82b8e4137bc21a4a8aa1e617989d08f93934e6c74621fb8b` |
| Host | `ssh` (linux/amd64, 8 CPU, 31.34 GiB RAM) |
| Kernel | `Linux version 6.8.0-136-generic (buildd@lcy02-amd64-041) (x86_64-linux-gnu-gcc-13 (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0, GNU ld (GNU Binutils for Ubuntu) 2.42) #136-Ubuntu SMP PREEMPT_DYNAMIC Wed Jul  1 21:53:05 UTC 2026` |
| cgroup v2 | true |
| Data dir | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/data` on `/dev/loop0` (ext4), mounted at `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run` |
| Volume size | 8.76 GiB |

### Effective server environment

| Variable | Value |
|---|---|
| `AGGREGATE_DB_PATH` | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/data/aggregate.db` |
| `AGGREGATE_MODE` | `aggregate` |
| `AGGREGATE_SYNCHRONOUS` | `NORMAL` |
| `API_KEY` | `` |
| `APP_ENV` | `development` |
| `DATA_DISK_BUDGET_MB` | `8192` |
| `DATA_DISK_PATH` | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/data` |
| `DB_DRIVER` | `sqlite` |
| `DB_DSN` | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/data/otelcontext.db` |
| `DLQ_PATH` | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/data/dlq` |
| `EXEMPLAR_RETENTION_DAYS` | `2` |
| `GRPC_PORT` | `4317` |
| `HOT_RETENTION_DAYS` | `8` |
| `HTTP_PORT` | `8080` |
| `INGEST_ASYNC_ENABLED` | `true` |
| `LOG_FTS_ENABLED` | `true` |
| `MCP_ENABLED` | `true` |
| `OTELCONTEXT_ALLOW_SQLITE_PROD` | `false` |
| `SAMPLING_RATE` | `1.0` |
| `TLS_AUTO_SELFSIGNED` | `false` |
| `TLS_CACHE_DIR` | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/data/tls` |

## Confinement

| | |
|---|---|
| Mode | `cgroup-scope` |
| Unit | `otelcontext-gate-20260823T115819Z-restarted.scope` |
| Scope path | `/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/app.slice/otelcontext-gate-20260823T115819Z-restarted.scope` |
| `cpu.max` | `200000 100000` (2.00 CPUs) |
| `memory.max` | `4294967296` (4.00 GiB) |

cgroup-v2 transient scope with CPUQuota and MemoryMax enforced by the kernel; cpu.max, memory.max, memory.peak and memory.events read back from the scope's cgroup files. The load generator ran outside the scope.

## Thresholds versus actuals

### phase

| Result | Assertion | Threshold | Actual | Basis | Notes |
|---|---|---|---|---|---|
| PASS | `phase.prefill.completed` — phase prefill ran to completion | == true | true | orchestrator |  |
| PASS | `phase.server_start.completed` — phase server_start ran to completion | == true | true | orchestrator |  |
| PASS | `phase.main_load.completed` — phase main_load ran to completion | == true | true | orchestrator |  |
| PASS | `phase.post_burst.completed` — phase post_burst ran to completion | == true | true | orchestrator |  |
| PASS | `phase.quiet_gap.completed` — phase quiet_gap ran to completion | == true | true | orchestrator |  |
| PASS | `phase.crash_run.completed` — phase crash_run ran to completion | == true | true | orchestrator |  |
| PASS | `phase.measure.completed` — phase measure ran to completion | == true | true | orchestrator |  |

### confinement

| Result | Assertion | Threshold | Actual | Basis | Notes |
|---|---|---|---|---|---|
| PASS | `confinement.mode` — the server ran inside a recorded resource boundary | == cgroup-scope (or sanctioned taskset-fallback) | cgroup-scope | orchestrator | cgroup-v2 transient scope with CPUQuota and MemoryMax enforced by the kernel; cpu.max, memory.max, memory.peak and memory.events read back from the scope's cgroup files. The load generator ran outside the scope. |
| PASS | `confinement.scope_path` — the transient scope's cgroup path was located and read | == true | true | cgroup-v2 files | scope path: /sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/app.slice/otelcontext-gate-20260823T115819Z-restarted.scope |
| PASS | `confinement.cpu_max` — effective cpu.max matches the configured CPU quota | == 2.00 CPUs | 2.00 CPUs | cgroup cpu.max | raw: 200000 100000 |
| PASS | `confinement.memory_max` — effective memory.max is at or below the memory threshold | <= 4.00 GiB | 4.00 GiB | cgroup memory.max | raw: 4294967296 |

### sustained

| Result | Assertion | Threshold | Actual | Basis | Notes |
|---|---|---|---|---|---|
| PASS | `sustained.duration` — the sustained phase ran for the contracted time | >= 10692.0 s | 10800.0 s | loadsim ACK ledger and report | contract: 3.0 h |
| PASS | `sustained.offered_rate` — the offered load reached the contracted points/second | >= 9500 pts/s | 10017 pts/s | loadsim ACK ledger and report | acked rate 10017 pts/s; a phase that ran below the contracted load cannot certify it |
| PASS | `sustained.ack_p99` — ACK p99 stayed inside the latency bound | <= 500.0 ms | 334.1 ms | loadsim ACK ledger and report | p50 39.6 ms, p90 175.4 ms, p99.9 506.0 ms, max 1013.2 ms over 19389102 samples |
| PASS | `sustained.ack_ratio` — acknowledged points as a fraction of points sent | >= 99.9000% | 100.0000% | loadsim ACK ledger and report | 108181217 acked of 108181217 sent |
| PASS | `sustained.resource_exhausted` — no RESOURCE_EXHAUSTED refusal at sustained load | == 0 | 0 | loadsim ACK ledger and report |  |
| PASS | `sustained.transport_errors` — no UNAVAILABLE or other transport error at sustained load | == 0 | 0 | loadsim ACK ledger and report |  |
| PASS (degraded basis) | `sustained.late_points` — no silent aggregate drops: otelcontext_aggregate_late_points_total did not move | <= 0.00 | 0.00 | prometheus counter vector with no children | the counter vector had no child series in any sustained-phase scrape, which for a drop counter means it never fired; the sibling otelcontext_aggregate_input_points_total was present, so the metric family is registered. The Prometheus text format cannot distinguish an empty vector from a deleted metric, which is why this basis is marked degraded. |
| PASS (degraded basis) | `sustained.admission_rejected` — no silent aggregate drops: otelcontext_aggregate_admission_rejected_total did not move | <= 0.00 | 0.00 | prometheus counter vector with no children | the counter vector had no child series in any sustained-phase scrape, which for a drop counter means it never fired; the sibling otelcontext_aggregate_input_points_total was present, so the metric family is registered. The Prometheus text format cannot distinguish an empty vector from a deleted metric, which is why this basis is marked degraded. |
| PASS (degraded basis) | `sustained.identity_overflow` — no silent aggregate drops: otelcontext_aggregate_identity_overflow_total did not move | <= 0.00 | 0.00 | prometheus counter vector with no children | the counter vector had no child series in any sustained-phase scrape, which for a drop counter means it never fired; the sibling otelcontext_aggregate_input_points_total was present, so the metric family is registered. The Prometheus text format cannot distinguish an empty vector from a deleted metric, which is why this basis is marked degraded. |
| PASS | `sustained.backlog_flat` — no sustained backlog growth | == true | true | prometheus otelcontext_aggregate_delta_log_rows | fitted growth 310 rows and endpoint growth 2400 rows over 179.8 min, allowance 5000 rows (peak 3600, R2 0.094) |

### burst

| Result | Assertion | Threshold | Actual | Basis | Notes |
|---|---|---|---|---|---|
| PASS | `burst.duration` — the burst ran for the contracted time | >= 57.0 s | 60.0 s | loadsim report |  |
| PASS | `burst.offered_rate` — the burst offered the contracted points/second | >= 19000 pts/s | 20099 pts/s | loadsim report | backpressure during the burst is permitted; offering less load than contracted is not |
| PASS | `burst.no_crash_or_oom` — no crash and no OOM kill during the burst | == true | true | orchestrator and cgroup memory.events | the server process survived the burst |
| PASS | `burst.recovery_ack_p99` — ACK p99 back inside the sustained bound within 120 s of burst end | <= 500.0 ms | 313.3 ms | loadsim recovery probe, graded window | graded window is 120-240 s after burst end; the allowance window (0-120 s) measured p99 335.4 ms and is reported, not gated |
| PASS | `burst.recovery_backlog` — writer backlog back inside the sustained peak within 120 s of burst end | <= 3600.00 | 3600.00 | prometheus otelcontext_aggregate_delta_log_rows | sustained-phase peak is the bound |

### recovery

| Result | Assertion | Threshold | Actual | Basis | Notes |
|---|---|---|---|---|---|
| PASS | `recovery.ledger_persisted_pre_kill` — an ACK ledger existed on disk before SIGKILL was sent | == true | true | orchestrator snapshot | snapshot /home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/ack-ledger-prekill.json (2418 bytes) taken at 15:38:33Z |
| PASS | `recovery.kill_delivered` — the server was killed with SIGKILL, not asked to shut down | == true | true | orchestrator | signal SIGKILL to pid 1500230 |
| PASS | `recovery.ready` — the restarted server reported ready inside the readiness bound | <= 60.0 s | 3.8 s | GET /ready | crash interval 3.9 s |
| PASS | `recovery.skipped_series` — startup recovery resolved every delta-log series | <= 0.00 | 0.00 | /home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/server-restarted.log | finalized 0 windows, replayed 2400 rows into 2400 series-windows, seeded 1350 baselines in 0.08 s |
| PASS | `recovery.no_acknowledged_loss` — no acknowledged aggregate loss: every window's post-restart total is at least its ACKed contributions | == true | true | ACK ledger vs spans totals | 0 points below the acknowledged lower bound across 2 compared windows |
| PASS | `recovery.within_attempted_upper_bound` — no window exceeded its attempted contributions | == true | true | ACK ledger vs spans totals | 0 of 2 windows above the attempted upper bound, 0 windows had no point at all |
| PASS | `recovery.exact_outside_crash` — windows the crash did not touch matched exactly | == true | true | ACK ledger vs spans totals | 1 exact windows, 1 crash-affected windows carrying 40719 points of permitted ambiguity |

### memory

| Result | Assertion | Threshold | Actual | Basis | Notes |
|---|---|---|---|---|---|
| PASS | `memory.peak` — peak memory stayed inside the bound | <= 4.00 GiB | 973.73 MiB | /sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/app.slice/otelcontext-gate-20260823T115819Z-restarted.scope/memory.peak | VmHWM secondary evidence: 862.02 MiB |
| PASS | `memory.oom_kills` — no OOM kill occurred | == 0 | 0 | /sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/app.slice/otelcontext-gate-20260823T115819Z-restarted.scope/memory.events |  |

### disk

| Result | Assertion | Threshold | Actual | Basis | Notes |
|---|---|---|---|---|---|
| PASS | `disk.main_projected` — projected two-day main-tier footprint (conservative upper estimate) | <= 4.00 GiB | 3.76 GiB | projection from filesystem samples | point estimate 3.71 GiB from 660 samples over 33.0 windows; measured main tier at report time 333.83 MiB |
| PASS | `disk.aggregate` — demonstrated aggregate.db tier | <= 2.25 GiB | 2.08 GiB | filesystem walk of the data directory | 1 files |
| PASS | `disk.dlq` — demonstrated DLQ tier | <= 512.00 MiB | 0 B | filesystem walk of the data directory | 0 files |
| PASS | `disk.wal_temp_tls` — demonstrated WAL, temp and TLS tier | <= 256.00 MiB | 55.62 MiB | filesystem walk of the data directory | 4 files |
| PASS | `disk.total` — total allocated data-directory usage | <= 7.00 GiB | 2.46 GiB | filesystem walk of the data directory | 0 B unclassified across 0 files |
| PASS | `disk.free_headroom` — free headroom on the data volume | >= 1.00 GiB | 5.83 GiB | statfs on the data volume | minimum observed during the run: 5.83 GiB |

### projection

| Result | Assertion | Threshold | Actual | Basis | Notes |
|---|---|---|---|---|---|
| PASS | `projection.sample_count` — the projection was fitted over enough steady-portion samples | >= 6.00 | 660.00 | orchestrator sampler |  |
| PASS | `projection.window_span` — the steady samples span enough completed windows for the slope to mean anything | >= 6.0 windows | 33.0 windows | orchestrator sampler | a slope fitted across less than this is a startup transient extrapolated across two days |
| PASS | `projection.single_application` — the projection is the physical slope times the horizon, with no amplification re-applied | == true | true | projection arithmetic | 6.69 MiB/window upper x 576 windows = 3.76 GiB; amplification measured: false (0.00x) |
| PASS | `projection.labelled` — the projection is labelled as a projection with its sample count and observed range | == true | true | report renderer | observed 70.29 MiB..297.47 MiB over 33.0 windows, R2 0.972 |

### query

| Result | Assertion | Threshold | Actual | Basis | Notes |
|---|---|---|---|---|---|
| PASS | `query.api.traffic_seven_day` — query surface traffic_seven_day answered over the requested range without a truncation flag | == true | true | http://127.0.0.1:8080/api/metrics/traffic?end=2026-08-23T12%3A00%3A00Z&start=2026-08-16T12%3A00%3A00Z | 10.47 s, 298370 bytes, coverage "full" (response header OtelContext-Data-Coverage), truncated flag present: false |
| PASS | `query.api.traffic_seven_day.windows` — query surface traffic_seven_day returned every seeded window | == 2016 | 2016 | http://127.0.0.1:8080/api/metrics/traffic?end=2026-08-23T12%3A00%3A00Z&start=2026-08-16T12%3A00%3A00Z | 0 windows missing |
| PASS | `query.api.traffic_seven_day.windows_extra` — query surface traffic_seven_day returned no windows outside the seeded interval | == 0 | 0 | http://127.0.0.1:8080/api/metrics/traffic?end=2026-08-23T12%3A00%3A00Z&start=2026-08-16T12%3A00%3A00Z | 0 extra windows |
| PASS | `query.api.traffic_seven_day.coverage` — query surface traffic_seven_day declared the aggregate coverage the contract expects of it | == full | full | response header OtelContext-Data-Coverage |  |
| PASS | `query.api.dashboard_seven_day` — query surface dashboard_seven_day answered over the requested range without a truncation flag | == true | true | http://127.0.0.1:8080/api/metrics/dashboard?end=2026-08-23T12%3A00%3A00Z&start=2026-08-16T12%3A00%3A00Z | 521.04 s, 1103 bytes, coverage "full" (response body field), truncated flag present: false |
| PASS | `query.api.dashboard_seven_day.coverage` — query surface dashboard_seven_day declared the aggregate coverage the contract expects of it | == full | full | response body field |  |
| PASS | `query.api.service_map_seven_day` — query surface service_map_seven_day answered over the requested range without a truncation flag | == true | true | http://127.0.0.1:8080/api/metrics/service-map?end=2026-08-23T12%3A00%3A00Z&start=2026-08-16T12%3A00%3A00Z | 13.65 s, 102637 bytes, coverage "full" (response body field), truncated flag present: false |
| PASS | `query.api.service_map_seven_day.coverage` — query surface service_map_seven_day declared the aggregate coverage the contract expects of it | == full | full | response body field |  |
| PASS | `query.api.stats` — query surface stats answered over the requested range without a truncation flag | == true | true | http://127.0.0.1:8080/api/stats | 1.00 s, 139 bytes, coverage "" (), truncated flag present: false |
| PASS | `query.api.ready` — query surface ready answered over the requested range without a truncation flag | == true | true | http://127.0.0.1:8080/ready | 0.00 s, 361 bytes, coverage "" (), truncated flag present: false |
| PASS | `query.api.live` — query surface live answered over the requested range without a truncation flag | == true | true | http://127.0.0.1:8080/live | 0.00 s, 19 bytes, coverage "" (), truncated flag present: false |
| PASS | `query.mcp.get_anomaly_timeline` — MCP tool get_anomaly_timeline answered over the full seven-day range | == true | true | POST /mcp | 0.00 s, 7615 result bytes, args {"since":"2026-08-16T12:00:00Z"} |
| PASS | `query.mcp.get_service_map` — MCP tool get_service_map answered over the full seven-day range | == true | true | POST /mcp | 0.00 s, 375284 result bytes, args {"depth":3} |
| PASS | `query.mcp.get_service_health` — MCP tool get_service_health answered over the full seven-day range | == true | true | POST /mcp | 0.00 s, 2820 result bytes, args {"service_name":"loadsim-svc-000"} |
| PASS | `query.mcp.root_cause_analysis` — MCP tool root_cause_analysis answered over the full seven-day range | == true | true | POST /mcp | 0.00 s, 2107 result bytes, args {"service":"loadsim-svc-000","time_range":"7d"} |
| PASS | `query.mcp.impact_analysis` — MCP tool impact_analysis answered over the full seven-day range | == true | true | POST /mcp | 0.00 s, 362 result bytes, args {"depth":3,"service":"loadsim-svc-000"} |

## Phases

| Phase | Started | Duration | Completed | Detail |
|---|---|---|---|---|
| `prefill` | 2026-08-23T11:58:20Z | 1114.9 s | true |  |
| `server_start` | 2026-08-23T12:16:55Z | 0.7 s | true |  |
| `main_load` | 2026-08-23T12:16:55Z | 10987.3 s | true |  |
| `post_burst` | 2026-08-23T15:20:03Z | 240.2 s | true |  |
| `quiet_gap` | 2026-08-23T15:24:03Z | 360.0 s | true |  |
| `crash_run` | 2026-08-23T15:30:03Z | 960.6 s | true |  |
| `measure` | 2026-08-23T15:46:03Z | 546.2 s | true |  |

## Load phases

| Phase | Duration | Offered | ACKed | ACK ratio | p50 | p99 | p99.9 | max | RESOURCE_EXHAUSTED | UNAVAILABLE | other |
|---|---|---|---|---|---|---|---|---|---|---|---|
| sustained | 10800.0 s | 10017 pts/s | 10017 pts/s | 100.0000% | 39.6 ms | 334.1 ms | 506.0 ms | 1013.2 ms | 0 | 0 | 0 |
| burst | 60.0 s | 20099 pts/s | 20099 pts/s | 100.0000% | 43.8 ms | 348.8 ms | 390.0 ms | 550.1 ms | 0 | 0 | 0 |
| post-burst allowance (0-120s, evidence only) | 120.0 s | 8752 pts/s | 8752 pts/s | 100.0000% | 38.5 ms | 335.4 ms | 483.6 ms | 716.4 ms | 0 | 0 | 0 |
| post-burst proof (120-240s, gated) | 120.0 s | 10049 pts/s | 10049 pts/s | 100.0000% | 38.3 ms | 313.3 ms | 371.8 ms | 686.7 ms | 0 | 0 | 0 |
| crash run (ledger source) | 900.0 s | 10033 pts/s | 9973 pts/s | 99.3960% | 37.5 ms | 309.7 ms | 480.8 ms | 725.0 ms | 0 | 9761 | 0 |

### Writer backlog trend (sustained phase)

| | |
|---|---|
| Metric | `otelcontext_aggregate_delta_log_rows` |
| Samples | 720 over 179.8 min |
| First / last | 1200 / 3600 rows |
| Min / max | 1200 / 3600 rows |
| Slope | 1.72 rows/min (R2 0.094) |
| Fitted growth / allowance | 310 / 5000 rows |
| Flat | true |

## Recovery — kill -9 on a surviving volume

| | |
|---|---|
| Killed | pid 1500230 with SIGKILL at 2026-08-23T15:38:33Z |
| Restarted | 2026-08-23T15:38:33Z |
| Ready | 2026-08-23T15:38:37Z (3.8 s after restart) |
| Crash interval | 3.9 s |
| Recovery stats source | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/server-restarted.log` |
| Finalized windows | 0 |
| Replayed rows / series-windows | 2400 / 2400 |
| Seeded baselines | 1350 |
| Skipped (unresolved) series | 0 |
| Recovery duration | 0.078 s |

### ACK ledger

| | |
|---|---|
| Path | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/ack-ledger.json` |
| Pre-kill snapshot | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/ack-ledger-prekill.json` (2.36 KiB, taken 2026-08-23T15:38:33Z) |
| Flush interval | 2.0 s |
| Windows | 4 (1787499000..1787499900) |
| Attempted / ACKed points | 9542346 / 9487805 |

### Crash-interval bound

The aggregate write path is at-least-once across a crash, so a crash-affected window is bounded rather than fixed: post-restart totals must be at least the confirmed-ACKed contributions and at most all attempted contributions. Windows the crash did not touch carry attempted == ACKed, so the same rule is an exact equality there.

| | |
|---|---|
| Signal compared | `spans` |
| Comparison range | 1787499300..1787499900 |
| Windows compared | 2 (1 exact, 1 crash-affected) |
| Attempted / ACKed / observed | 4487519 / 4446800 / 4447226 |
| Permitted ambiguity | 40719 points |
| Acknowledged loss | 0 points |
| Windows outside bounds | 0 below, 0 above, 0 missing |

| Window | Crash-affected | ACKed (lower) | Observed | Attempted (upper) | Result |
|---|---|---|---|---|---|
| 1787499300 | true | 2196809 | 2197235 | 2237528 | PASS  |
| 1787499600 | false | 2249991 | 2249991 | 2249991 | PASS  |

## Memory

| | |
|---|---|
| Basis | cgroup-scope |
| Peak | 973.73 MiB (from `/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/app.slice/otelcontext-gate-20260823T115819Z-restarted.scope/memory.peak`) |
| Limit | 4.00 GiB |
| oom_kill | 0 (from `/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/app.slice/otelcontext-gate-20260823T115819Z-restarted.scope/memory.events`, observed: true) |
| VmHWM (secondary) | 862.02 MiB |

| Server incarnation | PID | Peak | VmHWM | oom_kill | Scope |
|---|---|---|---|---|---|
| initial | 1500230 | 973.73 MiB | 862.02 MiB | 0 | `/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/app.slice/otelcontext-gate-20260823T115819Z-initial.scope` |
| restarted | 1739087 | 638.10 MiB | 718.62 MiB | 0 | `/sys/fs/cgroup/user.slice/user-1001.slice/user@1001.service/app.slice/otelcontext-gate-20260823T115819Z-restarted.scope` |

## Disk — every partition

Filesystem walk of `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/data` at 2026-08-23T15:46:03Z. The server's own attribution gauges are shown alongside; the walk is what the assertions read.

| Tier | Measured | Budget | Basis | Server gauge | Gauge high-water |
|---|---|---|---|---|---|
| `main` | 333.83 MiB | 4.00 GiB | projected | 334.09 MiB | 334.09 MiB |
| `aggregate` | 2.08 GiB | 2.25 GiB | demonstrated | 2.08 GiB | 2.08 GiB |
| `dlq` | 0 B | 512.00 MiB | demonstrated | 0 B | 0 B |
| `wal_temp_tls` | 55.62 MiB | 256.00 MiB | demonstrated | 55.62 MiB | 55.62 MiB |
| **total data dir** | 2.46 GiB | 7.00 GiB | demonstrated | — | — |
| free headroom | 5.83 GiB | >= 1.00 GiB | statfs | — | — |

## Main-tier projection

PROJECTION — not a measurement. Physical bytes/window fitted over the steady portion and multiplied by the 576-window (two-day) horizon.

| | |
|---|---|
| Samples | 660, from 2026-08-23T12:34:05Z to 2026-08-23T15:18:50Z |
| Observed range | 70.29 MiB .. 297.47 MiB over 33.0 completed windows |
| Physical growth | 6.60 MiB per completed 5-minute window |
| Fit quality | R2 0.9723, slope std err 44.45 KiB/window |
| Conservative slope | 6.69 MiB per window (point estimate + 2.0 std err) |
| Projected 576-window footprint | 3.71 GiB (point) / **3.76 GiB (gated upper estimate)** |
| Amplification | not measured — no logical charged-bytes counter was configured for the main tier, so the physical/charged amplification factor is not measured. The projection does not need it. |

The slope is already physical: it is the difference between two filesystem measurements of the same files, so it contains the indexes, the FTS shadow tables, the WAL/SHM sidecars and the free pages. The amplification factor is reported and never multiplied back in, because that would charge the indexes twice.

## Query completeness

Seven-day range: 2026-08-16T12:00:00Z .. 2026-08-23T12:00:00Z (2016 seeded windows expected).

| Surface | Status | Time | Coverage | Windows | truncated flag | Scalars |
|---|---|---|---|---|---|---|
| `traffic_seven_day` `http://127.0.0.1:8080/api/metrics/traffic?end=2026-08-23T12%3A00%3A00Z&start=2026-08-16T12%3A00%3A00Z` | 200 | 10.47 s | full | 2016/2016 (0 missing) | absent | — |
| `dashboard_seven_day` `http://127.0.0.1:8080/api/metrics/dashboard?end=2026-08-23T12%3A00%3A00Z&start=2026-08-16T12%3A00%3A00Z` | 200 | 521.04 s | full | — | absent | active_services=120, p99_latency_ms=1881, request_errors=55655502, requests=1934344516, span_errors=55655502, spans=1934344516, total_errors=55655502, total_logs=110305445, total_traces=1934344516 |
| `service_map_seven_day` `http://127.0.0.1:8080/api/metrics/service-map?end=2026-08-23T12%3A00%3A00Z&start=2026-08-16T12%3A00%3A00Z` | 200 | 13.65 s | full | — | absent | — |
| `stats` `http://127.0.0.1:8080/api/stats` | 200 | 1.00 s | — | — | absent | — |
| `ready` `http://127.0.0.1:8080/ready` | 200 | 0.00 s | — | — | absent | — |
| `live` `http://127.0.0.1:8080/live` | 200 | 0.00 s | — | — | absent | — |

### Aggregate-backed MCP tools, named explicitly

| Tool | Arguments | Status | Time | Result bytes | truncated flag | Error |
|---|---|---|---|---|---|---|
| `get_anomaly_timeline` | `{"since":"2026-08-16T12:00:00Z"}` | 200 | 0.00 s | 7615 | absent | — |
| `get_service_map` | `{"depth":3}` | 200 | 0.00 s | 375284 | absent | — |
| `get_service_health` | `{"service_name":"loadsim-svc-000"}` | 200 | 0.00 s | 2820 | absent | — |
| `root_cause_analysis` | `{"service":"loadsim-svc-000","time_range":"7d"}` | 200 | 0.00 s | 2107 | absent | — |
| `impact_analysis` | `{"depth":3,"service":"loadsim-svc-000"}` | 200 | 0.00 s | 362 | absent | — |

## Durability claim demonstrated

> Crash-durable on a surviving volume: committed aggregate data survives a process or container kill -9 while the underlying volume persists. This is not a host-power-loss claim and not a Pod-reschedule or node-loss claim.

`AGGREGATE_SYNCHRONOUS=NORMAL` for this run. This gate demonstrates committed-data recovery after a process kill -9 while the underlying volume survives. It does not claim host power-loss durability, and it does not claim Pod-reschedule or node-loss durability — see the durability section of `docs/OPERATIONS.md` for what the deployment contract carries.

## Metric gaps found by this gate

- internal/aggregate publishes only recovery duration and four row classes (otelcontext_aggregate_recovery_duration_seconds, otelcontext_aggregate_recovery_rows{kind} for replayed, finalized_windows, topology_restored_rows, topology_restored_windows). promStoreRecorder.RecordRecovery receives the whole RecoveryStats but publishes none of SkippedSeries — the corruption signal this gate asserts at zero — or SeededBaselines, so the gate parses the server's own slog line.
- The aggregate query API carries no `truncated` field: internal/aggregate pages every store read to completion, so truncation never reaches the wire on /api/metrics/*. Completeness there is asserted via the coverage marker and exact window coverage; the literal truncated=false check applies only where the field exists (exemplar-backed responses).
- test/aggprefill reports windows, bucket rows and delta rows but not per-window observation totals, so the prefill tier's exact scalar check is window coverage (every seeded window answered) rather than span-count equality.
- There is no process-resident-memory collector in the Prometheus surface; memory evidence comes from cgroup memory.peak / memory.events with /proc VmHWM as secondary.
- No metric reports logical charged bytes for the main (exemplar) tier, so the physical/charged amplification factor is unmeasured. The projection does not need it.
- required metric otelcontext_aggregate_late_points_total was absent from 835 scrapes
- required metric otelcontext_aggregate_admission_rejected_total was absent from 835 scrapes

## Commands invoked

| Phase | Command | Started | Duration | Exit | Log |
|---|---|---|---|---|---|
| `prefill` | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/bin/aggprefill -db /home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/data/aggregate.db -workers 8 -windows 2016` | 2026-08-23T11:58:20Z | 1114.9 s | 0 | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/prefill.log` |
| `server-initial` | `/usr/bin/systemd-run --user --scope --collect --quiet --unit=otelcontext-gate-20260823T115819Z-initial.scope -p CPUQuota=200% -p MemoryMax=4G -- /home/dev/projects/otelcontext/.claude/worktrees/gaterun/bin/otelcontext` | 2026-08-23T12:16:55Z | 0.0 s | -1 | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/server-initial.log` |
| `main_load` | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/bin/loadsim --direct --endpoint 127.0.0.1:4317 --settle 2m0s --duration 3h0m0s --batch-interval 250ms --call-timeout 30s --report /home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/loadsim-main.json --profile aggregate-acceptance --burst 2x60s` | 2026-08-23T12:16:55Z | 10987.3 s | 0 | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/loadsim-main.log` |
| `post_burst` | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/bin/loadsim --direct --endpoint 127.0.0.1:4317 --settle 2m0s --duration 2m0s --batch-interval 250ms --call-timeout 30s --report /home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/loadsim-postburst.json --profile aggregate-acceptance` | 2026-08-23T15:20:03Z | 240.2 s | 0 | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/loadsim-postburst.log` |
| `server-restarted` | `/usr/bin/systemd-run --user --scope --collect --quiet --unit=otelcontext-gate-20260823T115819Z-restarted.scope -p CPUQuota=200% -p MemoryMax=4G -- /home/dev/projects/otelcontext/.claude/worktrees/gaterun/bin/otelcontext` | 2026-08-23T15:38:33Z | 0.0 s | -1 | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/server-restarted.log` |
| `crash_run` | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/bin/loadsim --direct --endpoint 127.0.0.1:4317 --settle 1m0s --duration 15m0s --batch-interval 250ms --call-timeout 30s --report /home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/loadsim-crash.json --profile aggregate-acceptance --ack-ledger /home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/ack-ledger.json --ack-ledger-flush 2s` | 2026-08-23T15:30:03Z | 960.6 s | 0 | `/home/dev/projects/otelcontext/.claude/worktrees/gaterun/data/gate-run/loadsim-crash.log` |

