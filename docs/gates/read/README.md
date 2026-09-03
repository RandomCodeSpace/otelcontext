# Read-latency proofs

This directory holds committed **baselines** produced by the read-latency proof
(`test/readproof`, build tag `readproof`; issue #289, decision #281). The
proof itself starts the exact `otelcontext` binary, seeds one of two shapes,
and measures the read surfaces the dashboard and MCP clients actually call.

Files are named `<date>-<shape>-baseline.json` and carry schema
`otelcontext.read-latency.v1`. **Never hand-write or edit one.** The JSON is
the source of truth; every assertion carries the measured number and the
objective, and missing evidence is a failed assertion with its reason.

## What one file contains

- `binary_sha256`, `go_version`, `server_env` — what was measured.
- `prefill` — the seeded history (windows and series for aggregate; traces,
  spans and logs for legacy) and how long seeding took.
- `measurements[]` — per endpoint: the **cold** first call (recorded on its
  own), then 10 warm-up calls, then up to 200 timed calls inside a 60 s
  budget. `latency` holds exact ordered (nearest-rank) p50/p90/p99/max over
  `samples_ms`; `status`, `coverage` (the `OtelContext-Data-Coverage`
  header), `response_bytes` and `cache_hits` (`X-Cache: HIT`) are the last
  and cumulative values seen.
- MCP tools appear twice: `mcp_<tool>` sends the arguments a real client
  sends, so the server's 5 s result cache serves most warm calls, and that
  is the asserted number; `mcp_<tool>_miss` adds an ignored `_nonce` argument
  that defeats the cache key and is recorded for reference only.
- `rss` — the server's own `otelcontext_process_resident_memory_bytes` and
  `otelcontext_go_heap_inuse_bytes` (#292) scraped from `/metrics/prometheus`
  every 5 s across the run (`samples[]` carry `t_s`, `bytes`,
  `heap_inuse_bytes`), with `peak_bytes`, `heap_inuse_peak_bytes`, and the
  **steady p95** the #283 objective is asserted against: the exact ordered
  p95 over samples taken at or after `steady_from_s`. `settle_s` and
  `steady_rule` record how that window was chosen (below).
- `memory_accounting` — one `/metrics/prometheus` read at the end of the
  measurement phase saying where the memory sits: `rss_bytes` and
  `heap_inuse_bytes` at that instant, the resource registry
  (`registry_pair_entries`, `registry_host_entries`, summed over tenants),
  the GraphRAG census by entity and edge store (`graphrag_entities`,
  `graphrag_edges`; `latency_sketches` is the #291 per-service sketch count
  and `graphrag_latency_sketch_bytes` is that count × `sizeof(aggregate.Sketch)`),
  and the read caches (`read_cache_entries` by `api_ttl` — dashboard,
  service-map and ETag entries — and `mcp_result`).
- `assertions[]` — two per asserted endpoint, `<name>.cold_ms` and
  `<name>.warm_p99_ms`, plus `rss.steady_p95_bytes`.

## Memory objective and the steady window (from #283)

| Shape | RSS steady p95 |
|---|---|
| `legacy` (bounded SQLite, 5 services) | ≤ 512 MiB |
| `aggregate` (120 services) | ≤ 2 GiB |

Seeding is a transient the objective does not describe: the legacy shape
exports two days of spans in under twenty seconds, every seeded span is
backdated past the GraphRAG trace TTL and sits in memory until the next
60 s refresh tick prunes it, and the Go runtime keeps the freed heap until
its next collection — an idle process only gets one from the two-minute
forced GC — and then returns it to the OS several seconds later. The
seven-day gate excludes its own warm-up with `steady_start_offset_sec`; the
read proof does the equivalent from observable runtime state. The steady
window starts at the first GC cycle completed at least one refresh interval
(60 s) after seeding finished, once the runtime's retained idle heap
(`go_memstats_heap_idle_bytes − go_memstats_heap_released_bytes`) is below
the heap in use and the RSS gauge has held within 2% across four readings
5 s apart. A collection after which retained idle stays above in-use for
90 s counts as settled, and the whole wait is bounded at 300 s; either case
is recorded in `steady_rule`. The measurement phase (the reads) runs inside
that window, so the p95 covers the server answering the asserted endpoints.

With about eight samples the nearest-rank p95 is the maximum, which is the
strict reading and the intended one.

Where the memory went at the 144-window depth, measured while sizing the
fix: the pure-Go SQLite page cache is **not** Go heap (modernc's libc
allocates it outside the heap, invisible to `GOMEMLIMIT`) and measured
~277 MiB of anonymous RSS at the former 256 MB ceiling; the former 1 GB
`mmap_size` window kept every page a full-range scan touched resident
(124–155 MiB for the 173 MB legacy file, growing with the file); the Go heap
after the transient holds ~65 MiB live. The fix (#292) is at that owner:
the page-cache ceiling is 128 MB and mmap is opt-in via
`SQLITE_MMAP_SIZE_BYTES` — both re-proven against the latency objectives
above. `OTELCONTEXT_READPROOF_PPROF_ADDR=127.0.0.1:6060` opens the measured
server's pprof listener so a future exceedance can be attributed the same
way.

## Shapes and objectives (from #281)

| Shape | Seeded history | Endpoints | Warm p99 | Cold |
|---|---|---|---|---|
| `aggregate` | `test/aggprefill`, 120 services, 6,000 series, `AGGREGATE_MODE=aggregate`, SQLite | `/api/metrics/dashboard`, `/api/metrics/traffic`, `/api/metrics/service-map`, MCP `get_service_map`, `get_anomaly_timeline`, `root_cause_analysis` | ≤ 500 ms | ≤ 2 s |
| `legacy` | 5 services, 2 days of exemplars over OTLP HTTP (43,200 traces, 4 spans + 1 log each), `AGGREGATE_MODE=legacy`, SQLite | same REST endpoints plus `/api/system/graph`, same MCP tools | ≤ 300 ms | ≤ 1 s |

Each REST endpoint is measured twice. `rest_<name>` uses the default range
(no query parameters), exactly as the embedded UI polls.
`rest_<name>_full_range` passes explicit `start`/`end` (RFC3339) spanning the
whole seeded horizon — every prefilled window in the aggregate shape, the
full two days in the legacy shape — so the wide-range SUM path #219 optimised
is exercised; both variants are asserted against the same objectives. The
`coverage` column is the `OtelContext-Data-Coverage` header (only
`/api/metrics/traffic` sets it); `body_coverage` is the `coverage` field the
dashboard and service-map bodies carry; `requested_start`/`effective_start`
appear only when the aggregate range clamp (#217) shortened the request.

The decision names `/api/graph`; the GraphRAG-backed REST surface in this
codebase is `GET /api/system/graph`, which is what the legacy shape measures.

**Prefill depth.** The decision's aggregate shape is seven days (2,016
five-minute windows). That prefill writes 12M bucket rows and takes about 24
minutes on eight cores, which does not fit a 15-minute hosted job, so CI and
the committed baselines seed **144 windows (12 h)** via
`OTELCONTEXT_READPROOF_PREFILL_WINDOWS`. The artifact records both
`prefill.windows` and `prefill.requested_windows`. Unset the variable on a
machine with the time and disk to run the full seven days.

## Regenerating

```bash
go build -trimpath -o /tmp/rp/otelcontext .
go build -tags prefill -o /tmp/rp/aggprefill ./test/aggprefill

export OTELCONTEXT_READPROOF_BINARY=/tmp/rp/otelcontext
export OTELCONTEXT_AGGPREFILL_BINARY=/tmp/rp/aggprefill
export OTELCONTEXT_READPROOF_PREFILL_WINDOWS=144   # unset for the full seven days

OTELCONTEXT_PROOF_DIR=/tmp/rp/legacy \
  go test -tags=readproof -count=1 -v -timeout=20m -run '^TestReadLatencyLegacy$' ./test/readproof
OTELCONTEXT_PROOF_DIR=/tmp/rp/aggregate \
  go test -tags=readproof -count=1 -v -timeout=20m -run '^TestReadLatencyAggregate$' ./test/readproof

cp /tmp/rp/legacy/read-latency-v1.json    docs/gates/read/$(date +%F)-legacy-baseline.json
cp /tmp/rp/aggregate/read-latency-v1.json docs/gates/read/$(date +%F)-aggregate-baseline.json
```

Each run writes `read-latency-v1.json` into its `OTELCONTEXT_PROOF_DIR`, so
give the two shapes separate directories. State goes under `TMPDIR` when set.
The test fails when any assertion fails and still writes the JSON.

CI runs both shapes on every pull request as `read latency proof · legacy`
and `read latency proof · aggregate` (`.github/workflows/ci.yml`) and uploads
`read-latency-<shape>-<sha>` for 14 days. The untagged helpers (percentiles,
evaluation, JSON, exposition parsing, memory accounting) are covered by the
normal `go test ./...`. Each shape now spends up to five minutes settling
after seeding before it measures, so a run is three to eight minutes.
