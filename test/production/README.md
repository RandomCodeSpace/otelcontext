# Production acceptance fixtures

These commands prepare synthetic history for root's integrated acceptance run.
They do not start the three-hour load. Run from the repository root. No additional
Python packages or Go modules are needed.

## Data and bounds

| Fixture | Default shape | Written data |
| --- | --- | --- |
| Native Prometheus | 200 services, 20 hosts, 60-second cadence, eight days | 706 series, 11,520 samples per series, 8,133,120 samples |
| Seven-day Prometheus | Same identities, `--hours 168` | 706 series, 10,080 samples per series, 7,116,480 samples |
| Selected raw | 200 services, eight retained checkpoints and one expired checkpoint each | 1,800 traces, 3,600 spans, 3,600 logs |
| Live mock hosts | 20 hosts, five gauges every 10 seconds | 100 points/request, 10 points/second |
| Aggregate prefill | 200 service identities, unchanged 6,000 series, 2,016 windows | 12,096,000 aggregate buckets |

The 60-second native metric history is a simulation, not seven days of observed
production traffic. The raw data contains selected complete two-span exemplars
and two correlated logs per trace. It does not represent all raw traffic.

Names match `test/loadsim`: `loadsim-svc-000` through `loadsim-svc-199` and hosts
`mock-host-id-000` / `mock-node-000` through index 019. Service index modulo 20
selects its host. Each mock host represents Linux amd64, eight logical CPUs,
32 GiB RAM, and a 100 GiB filesystem. Those are mock specifications, not the
actual test server's capacity. Root must record the real machine's CPU, RAM,
filesystem size, software versions, and resource limits alongside the load report.

## Native Prometheus history

Use the deployment workstream's patched **3.15.0** `promtool`. Its exact path is
an argument; the generator checks the reported version. The output must be new
or empty. `--stopped-fixture` declares that the dedicated target is stopped.
The generator exclusively creates its fixture marker and holds the TSDB `lock`
while importing. There is no append, resume, overwrite, or live-directory mode.

```bash
rtk proxy python3 -B test/production/fixtures.py prometheus \
  --promtool /absolute/path/to/promtool \
  --out /tmp/otelcontext-acceptance-prometheus \
  --stopped-fixture --hours 192 --services 200 --hosts 20
```

The default exclusive end is the most recent two-hour UTC boundary. To coordinate
with other fixtures, add `--end-time '<PAST_RFC3339_2H_BOUNDARY>'`. Future times
and unaligned boundaries fail before writing. Each file covers exactly one
nonoverlapping two-hour block and is deleted only after a successful import and
metadata count/bounds check. A full run creates 96 blocks, each with 84,720
samples. The current input file survives an import failure. `promtool.log` and
`manifest.json` remain outside the repository with block IDs, counts, input byte
sizes, time bounds, and exact expected queries.

Point a **stopped, isolated** Prometheus at the generated `tsdb/` directory, then
start it with the deployment configuration's `otlp.translation_strategy:
NoTranslation`. For a pre-retention proof, use retention long enough to keep the
whole seed, such as nine days. For the actual retention test use seven days and
check the expired blocks separately. Retention removes whole blocks in the
background. Do not expect a synchronous wall-clock seven-day cutoff at startup.
The eight-day seed deliberately includes older blocks for that test.

```bash
rtk proxy python3 -B test/production/fixtures.py verify \
  --manifest /tmp/otelcontext-acceptance-prometheus/manifest.json \
  --query-url http://127.0.0.1:9090/api/v1/query --before-retention
```

`verify` accepts a full query endpoint, so root can point it at the integrated
app's PromQL proxy. HTTPS verifies the server certificate; add `--ca-file` for a
private CA. Plain HTTP is permitted only for loopback Prometheus verification.
After seven-day retention, omit `--before-retention`; this skips only the seed's
original first sample and full pre-retention sample count. Other checks include
six-days-old history for full seeds. Use the manifest's per-block timestamp bounds
to distinguish expected expiry from missing retained data.

The schema has three series per service: `queue.depth`,
`http.server.request.count`, and `custom.reset.counter`. It preserves dotted
`service.name`, `service.instance.id`, `host.id`, and `host.name` labels.
`fixture.kind="history"` separates seeded series from live load. The counter is
cumulative, increasing by `60 * (service_index + 1)` each minute. The reset
counter restarts at that same increment every hour. No delta receiver or
experimental feature is needed. Backfill stores float samples without OTLP type
metadata; the live workload separately exercises cumulative OTLP ingestion.

For service zero, these queries have exact expected results at the manifest's
specified evaluation times:

| Check | PromQL | Expected |
| --- | --- | --- |
| Gauge and labels | `{"queue.depth","service.name"="loadsim-svc-000","fixture.kind"="history"}` | 10; exact service, instance and host labels |
| Counter rate | `rate({"http.server.request.count","service.name"="loadsim-svc-000","fixture.kind"="history"}[5m])` | 1/second |
| Counter increase | `increase({"http.server.request.count","service.name"="loadsim-svc-000","fixture.kind"="history"}[5m])` | 300 |
| Reset rate/increase | Same functions over `custom.reset.counter` spanning the last hourly reset | 1/second and 300 |
| Reset count | `resets({"custom.reset.counter","service.name"="loadsim-svc-000","fixture.kind"="history"}[5m])` | 1 at the manifest's reset time |
| Histogram p95 | `histogram_quantile(0.95, sum by (le) (rate({"fixture.request.duration_bucket","service.name"="loadsim-svc-000","fixture.kind"="history"}[5m])))` | 0.75 seconds |

One classic histogram fixture adds six series on service zero: four buckets
`0.1`, `0.5`, `1`, `+Inf` with cumulative per-minute counts `50`, `90`, `100`,
`100`, plus `_count` and `_sum`. This tests `histogram_quantile` over classic
buckets, not native histogram ingestion. For earliest/latest proof, `verify`
reads a range vector of one gauge at the seed's last timestamp and checks its
first timestamp, last timestamp, and exact sample count. The manifest records
all evaluation times, including a five-minute window crossing a real reset.

[Prometheus backfill documentation](https://prometheus.io/docs/prometheus/3.15/storage/#backfilling-from-openmetrics-format)
describes the two-hour block format and current-head overlap restriction.
[UTF-8 query syntax](https://prometheus.io/docs/guides/utf8/#querying) requires the
quoted names used here. The tool only creates an empty TSDB, so there is no
existing head to overlap. Do not copy these blocks into a running TSDB.

## Selected raw traces and logs

```bash
rtk proxy go run ./test/production/rawseed \
  -out /tmp/otelcontext-acceptance-raw -services 200 -hosts 20
```

The output directory must **not exist**, even if empty. The parent directory
must exist. The command creates `raw.db`, applies the repository's versioned
migrations, requires an exact schema, and inserts with `BatchCreateAll`. It never
opens an existing application database. Root should configure the stopped test
app to use this new database rather than importing rows into a live database.

The optional `-end-time` is an exclusive RFC3339 timestamp in the past; the
default is the current UTC minute. Retained checkpoints are end minus seven days
plus one minute, end minus six through one days, and end minus one minute. Each
service also has an expiry sentinel at end minus eight days. `manifest.json`
records each checkpoint and an example trace ID for service zero. Spans and logs
carry host attributes and `fixture.kind=history` or `expired`. Every trace has
complete two-span metadata, and each log identifies its trace and span.

These are repository-seeded historical rows. They do not prove that OTLP accepts
old timestamps, nor do they create aggregate buckets or resource-registry rows.
Live load and the host sender must populate the current resource registry.
Retention expected counts are relative to `retention_cutoff_at_seed`; recompute
from checkpoint times as real time advances. In particular, the oldest retained
checkpoint will expire after its one-minute margin passes. Use the six-days-old
checkpoint for stable long-run retained-history evidence.

## Live host metrics over HTTPS

```bash
rtk proxy python3 -B test/production/fixtures.py hosts \
  --url https://otelcontext.test --ca-file /absolute/path/to/test-ca.pem \
  --hosts 20 --duration 10800 --interval 10 \
  --report /tmp/otelcontext-hosts.ndjson
```

Root starts this beside its load run. The sender uses standard-library OTLP JSON
at `/v1/metrics`, requires HTTPS, verifies the CA and hostname, and refuses
redirects. It needs no authentication. The report path must be new. Each resource
omits `service.name`, includes `host.id` and `host.name`, and uses the host ID as
`service.instance.id`, exercising the application's implicit host identity.

The five gauges match the UI: `system.cpu.utilization`,
`system.memory.utilization`, `system.filesystem.utilization`,
`system.memory.usage`, and `system.filesystem.usage`. Values remain deterministic
per host, and timestamps use the current clock. Requests fail on non-200 replies,
OTLP partial rejection, warning messages, malformed JSON, or TLS errors. The
NDJSON report records accepted samples and request latency, then a final summary
with attempted/accepted request and point counts, p95/max latency, and any error.
An accepted response is transport evidence; root still needs application query,
MCP and UI proof with the integrated binary.

## Aggregate prefill options

Existing defaults remain 120 `checkout-svc-*` identities, 6,000 series and 2,016
windows including the current window. Explicit end-time excludes that boundary's
window and must align to five minutes. The new service count changes identities,
not series count. The existing aggregate command itself does not guard a live
path; root must give it a new database under stopped fixture storage.

```bash
rtk proxy go run -tags prefill ./test/aggprefill \
  -db /tmp/otelcontext-acceptance-aggregate.db \
  -services 200 -service-prefix loadsim-svc- \
  -end-time '<PAST_RFC3339_5M_BOUNDARY>' -windows 2016
```

Do not mount or mutate `/tmp/otelcontext-200-rolling-71rjl795/volume.img` from this
workstream. It belongs to root.

## Focused checks

```bash
rtk proxy go test -tags prefill ./test/aggprefill
rtk proxy go test ./test/production/rawseed
rtk proxy python3 -B -m unittest discover -s test/production -p 'test_*.py' -v
```

For a real Prometheus smoke fixture use `--hours 2 --services 2 --hosts 1`, giving
17 series and 2,040 samples. Start the patched binary on a free loopback port
with that isolated TSDB and run `verify --before-retention`. The rawseed tests
exercise repository reads, decompressed host attributes, correlation and
batched retention. The Python tests check bounded counts, exclusive timestamps,
path guards, host resource identity and partial-error accounting.
