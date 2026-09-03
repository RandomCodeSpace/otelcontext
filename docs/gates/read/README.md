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
- `rss` — the server's `VmRSS` sampled every 5 s across the run, with the peak
  and the p95 over samples taken during the measurement phase. Recorded, not
  asserted (#292 owns that objective).
- `assertions[]` — two per asserted endpoint, `<name>.cold_ms` and
  `<name>.warm_p99_ms`.

## Shapes and objectives (from #281)

| Shape | Seeded history | Endpoints | Warm p99 | Cold |
|---|---|---|---|---|
| `aggregate` | `test/aggprefill`, 120 services, 6,000 series, `AGGREGATE_MODE=aggregate`, SQLite | `/api/metrics/dashboard`, `/api/metrics/traffic`, `/api/metrics/service-map`, MCP `get_service_map`, `get_anomaly_timeline`, `root_cause_analysis` | ≤ 500 ms | ≤ 2 s |
| `legacy` | 5 services, 2 days of exemplars over OTLP HTTP (43,200 traces, 4 spans + 1 log each), `AGGREGATE_MODE=legacy`, SQLite | same REST endpoints plus `/api/system/graph`, same MCP tools | ≤ 300 ms | ≤ 1 s |

REST calls use the default range (no query parameters), exactly as the
embedded UI polls. The decision names `/api/graph`; the GraphRAG-backed REST
surface in this codebase is `GET /api/system/graph`, which is what the legacy
shape measures.

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
evaluation, JSON, RSS parsing) are covered by the normal `go test ./...`.
