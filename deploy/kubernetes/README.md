# Kubernetes and Istio production profile

This profile runs one OtelContext SQLite owner and one Prometheus companion for
200 services. It requires the application Prometheus forwarding/query integration
and retained-name bounds from the production-readiness changes. The manifests are
an initial capacity baseline, not a seven-day production certification.

## Operator values and build

Use Kubernetes 1.29 or later with a CSI driver supporting `ReadWriteOncePod` and
`fsGroup`. The local schema check uses Kubernetes 1.35.0 and Istio 1.31.2 CRDs;
validate against the versions installed in your cluster before applying. Use
POSIX block storage, such as ext4 or XFS, for both PVCs. Prometheus does not support
NFS/EFS storage. See the [Prometheus storage contract](https://prometheus.io/docs/prometheus/latest/storage/).

Copy this directory into your operator configuration repository. Supply:

- Your application and companion registry image names and immutable digests in
  `kustomization.yaml`. Replace `newTag` with `digest: sha256:...` after publishing.
- A real hostname in both `Gateway` and `VirtualService` in `istio.yaml`.
- The installed ingress workload selector, including its label key, in `Gateway`.
- An existing TLS Secret name in `credentialName`. The Secret belongs in the
  ingress gateway workload's namespace and contains `tls.crt` and `tls.key`.
- Your CSI storage class in both PVCs. If changing the namespace, change
  `kustomization.yaml` and `namespace.yaml` together.

The `.invalid` names are deliberate placeholders. No public URL, certificate,
registry access, or storage class is provisioned by these files. Gateway selection
across namespaces must be enabled by the installed Istio configuration; if
`PILOT_SCOPE_GATEWAY_TO_NAMESPACE=true`, put the Gateway beside its ingress
workload and reference it as `namespace/otelcontext` from the VirtualService.
See [Istio Gateway](https://istio.io/latest/docs/reference/config/networking/gateway/).

Build from the integrated source checkout, using operator-owned image names:

```sh
rtk proxy docker build -f Containerfile --build-arg VERSION="$RELEASE_VERSION" --build-arg REVISION="$SOURCE_SHA" -t "$APP_IMAGE" .
rtk proxy docker build -f deploy/prometheus/Containerfile -t "$PROMETHEUS_IMAGE" deploy/prometheus
rtk proxy docker run --rm --entrypoint /bin/promtool -v "$PWD/deploy/kubernetes/prometheus.yml:/etc/prometheus/prometheus.yml:ro" "$PROMETHEUS_IMAGE" check config /etc/prometheus/prometheus.yml
rtk proxy trivy image --scanners vuln --severity HIGH,CRITICAL --exit-code 1 "$APP_IMAGE"
rtk proxy trivy image --scanners vuln --severity HIGH,CRITICAL --exit-code 1 "$PROMETHEUS_IMAGE"
rtk proxy kubectl kustomize deploy/kubernetes
```

Both images run as UID/GID 65532 with a read-only root filesystem. Their only
writable runtime locations are the persistent data mount and bounded `/tmp`.
The app build context allows only Go sources, module files, and `internal/`.
Local databases, `.env` files, Git metadata, and scratch reports are excluded.
The image records the supplied source SHA as an OCI label; its Go binary has
`vcs.revision=unknown` because the build context excludes `.git`. Backups still
bind to the exact binary SHA-256. Retain the image digest and source SHA together.
The current application initializes its version from Go module build information,
so a source-built container reports `local`; the supplied release version lives
in the OCI label. A linker override cannot change that initializer.

The builder is Go **1.26.9**, pinned by its multi-platform image digest. It stays
below the operator's Go 1.27.1 ceiling. Go 1.25.13 and Go 1.27.1 are not acceptable
build toolchains for this profile. `go.mod` supplies the same exact version to CI,
and the release script refuses a different local version. `x/net` is 0.60.0;
its required dependency closure updates `x/crypto` to 0.57.0, `x/sys` to 0.48.0,
`x/text` to 0.42.0, and `x/sync` to 0.23.0. These are required security updates,
not a general dependency refresh. See [GO-2026-6603](https://pkg.go.dev/vuln/GO-2026-6603).

The companion uses maintained, Apache-2.0 licensed
[Prometheus 3.15.0](https://github.com/prometheus/prometheus/releases/tag/v3.15.0),
source commit `5241a27fe3c6983549fccc32f6e65917408c63cd`. The source archive checksum
and Go builder digest are pinned in `deploy/prometheus/Containerfile`. The
official image at index digest
`sha256:efd719c99d83b060d9daefdcf00360461adf279f45ef5391f8d111892118753e`
contains Go 1.27.1 and `x/net` 0.58.0. Its October 18, 2026 Trivy scan reports
HIGH CVE-2026-78669 in `x/net` and HIGH CVE-2026-78667, CVE-2026-78669,
CVE-2026-97031 in the standard library. Do not substitute that image for the
patched build. The companion applies the same security versions as the app,
also raising its `x/term` to the required 0.46.0. It includes `promtool` and the
upstream LICENSE/NOTICE. The frontend console is omitted; OTLP, PromQL, and
management health endpoints are available. No experimental feature is enabled.
Review the [upstream advisories](https://github.com/prometheus/prometheus/security/advisories)
when refreshing these pins. Re-scan the published image digests before rollout.

Local checks on October 18, 2026 found zero HIGH/CRITICAL vulnerabilities in the
patched runtime images with Trivy 0.71.0. `govulncheck` 1.6.0 package scans found
no vulnerable imported packages in the app or the exact patched Prometheus and
promtool sources. Module scans still flag unmaintained OpenPGP,
[GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932), and the companion's AWS SDK v1
dependency also flags [GO-2022-0635](https://pkg.go.dev/vuln/GO-2022-0635) and
[GO-2022-0646](https://pkg.go.dev/vuln/GO-2022-0646). None has a fixed version in
that module line. The exact companion dependency lists contain neither
`golang.org/x/crypto/openpgp` nor `github.com/aws/aws-sdk-go/service/s3/s3crypto`.
Binary-mode govulncheck nevertheless reports those findings; this disagrees with
the source package scan and is retained as a scanner limitation, not silently
suppressed. Reassess if enabling integrations or changing build inputs.

## Storage, capacity, and ownership

The configured stack requests and limits OtelContext to **3 CPU and 6 GiB RAM**
and Prometheus to **1 CPU and 2 GiB RAM**. Together they request and limit **4 CPU
and 8 GiB RAM**, with **48 GiB provisioned storage**. Shared Istio ingress,
pod/runtime and node overhead are additional; this is not a total-node capacity
claim. Both pods keep sidecar injection disabled.

The migration init container requests 1 CPU/1 GiB and is limited to 3 CPU/4 GiB.
For each resource, Kubernetes uses the larger of the regular-container sum and
the largest init-container request or limit. The app pod's effective request and
limit therefore remain 3 CPU/6 GiB, including initialization. See
[init-container resource accounting](https://kubernetes.io/docs/concepts/workloads/pods/init-containers/#resource-sharing-within-containers).

`GOMEMLIMIT=3GiB` and `GOMAXPROCS=4` remain unchanged. `GOMAXPROCS` controls
Go parallelism; it does not raise the 3 CPU quota. The October 20 r4 local run
enforced app 3 CPU/6 GiB and Prometheus 1 CPU/2 GiB beneath a shared 4 CPU/8 GiB
parent, including test TLS in that parent. Its recorded measurements meet the
[current production-200 targets](../../test/loadsim/README.md#current-production-200-acceptance-targets)
after the sustained ACK p99 target changed to 600 ms. The historical 500 ms
verdict remains FAIL; this reassessment adds no new performance measurement.
The run used aged history with gaps. See the
[release evidence](../../docs/release-notes/v0.6.3-readiness.md) for its limits.
Actual Istio, CSI/PVC and node-overhead acceptance remains separate.

The 16 GiB application PVC stores the main DB, aggregate DB, DLQ, and shutdown
receipts under `/var/lib/otelcontext`. The measured sizing candidates are
`READY_AGGREGATE_DISK_BUDGET_MB=6144` (6 GiB, aggregate DB only) and
`DATA_DISK_BUDGET_MB=14336` (14 GiB, total application data). The bounded r4
local run exercised these budgets; fresh dense combined history acceptance
remains unverified. These values are not a certified minimum.

The October 19 seven-day fixture contains 29,232,000 aggregate buckets across
14,500 series. All 169 eligible hourly rollups completed. The checkpointed
aggregate DB is 4,584,816,640 bytes (4.27 GiB); its sampled DB/WAL/SHM allocation
peaked at 4,845,137,920 bytes (4.51 GiB). The separate physical raw-retention
fixture, including FTS and six hourly purge/append cycles, peaked at
5,946,449,920 bytes (5.54 GiB). Their component peaks sum to 10,791,587,840 bytes
(10.05 GiB); they were not measured simultaneously. The raw fixture replicates
the observed earlier sampler mixture, not a new final-profile sampler run.

The former 8 GiB application budget and 4 GiB DB-only aggregate readiness budget
both fail these measurements. A 12 GiB application budget leaves only about
0.75 GiB before the 90% errors-only threshold. The 14 GiB candidate leaves about
2.55 GiB before that threshold and keeps the 95% raw-off threshold at 13.3 GiB,
below the 16 GiB PVC. The aggregate 6 GiB candidate exceeds the observed joint
aggregate peak by over 20%, although readiness measures only its main DB file.
Growth, concurrent ingest/query memory, WAL peaks, and backup workspace still
require runtime validation. Alert on filesystem use, WAL growth, readiness
failures, and raw shedding before these thresholds are reached.
The disk watchdog measures the application's data directory plus configured
SQLite databases, their WAL/SHM files, and the DLQ outside that directory without
double-counting overlapping paths. Unrelated host files do not consume the app
budget. Physical free-space pressure on `DATA_DISK_PATH` is checked separately;
a failed directory scan cannot suppress a full-filesystem reading. Keep all data
on the configured PVC so this free-space check covers every local store.

Startup adds a covering scalar index to existing aggregate databases after
verifying their schema. UUID, schema version and rows remain intact. Allow space
for the index, its build sort and WAL; creation failure refuses startup. The
isolated 2.016-million-row fixture added 40,144,896 bytes, but production values
and cardinality determine the actual cost.

Main schema version 4 adds `idx_spans_start_time` for age-ordered span retention.
Run the new image's `migrate up` with application writers stopped before startup;
production startup checks the schema without creating the index. Migration 4
preserves existing rows, FTS objects, and migration 1–3 checksums. Its index-build
time, temporary space, and retained size on the full raw fixture remain unmeasured
and are not included in the capacity figures above. An older version-3 binary
refuses the upgraded ledger; retain the pre-migration backup for rollback.

`HOT_RETENTION_DAYS=7` and `EXEMPLAR_RETENTION_DAYS=7` retain aggregate evidence
and **selected raw examples**, respectively. Raw payload is capped at 512 KiB per
global five-minute window and 128 KiB per service window. Seven days contains
2,016 windows, or **1,008 MiB of selected raw payload** at the global cap.
Payload arithmetic does not bound physical storage: retained trace headers,
including headers with zero retained spans, ordinary indexes, FTS, and WAL
contribute substantially. The physical raw fixture above supersedes the earlier
2x payload amplification estimate. The sampler retains selected raw traces and
logs. Raising retention without lowering the former 3 MiB/window cap would allow
6,048 MiB of payload before storage overhead.

The retained-name limits have separate headroom from active series limits:

```text
AGGREGATE_MAX_OPERATION_NAMES_PER_TENANT=10000
AGGREGATE_MAX_OPERATION_NAMES=100000
AGGREGATE_MAX_METRIC_NAMES_PER_TENANT=5000
AGGREGATE_MAX_METRIC_NAMES=50000
AGGREGATE_MAX_LOG_TEMPLATES_PER_TENANT=5000
AGGREGATE_MAX_LOG_TEMPLATES=50000
```

These bounds cap retained dictionaries even when active series expire. They do
not increase series admission limits or promise retention of every distinct name.

The `production-200` live log profile uses `AGGREGATE_MAX_SERIES_LOGS=12000`,
`AGGREGATE_MAX_LOG_TEMPLATES_PER_SERVICE=32`, and `AGGREGATE_MAX_SERIES=17500`.
The actual generator/reducer/miner/limiter census is 20 templates and 45
materialized log series per service: 4,000 templates and 9,000 series across
200 services. Those series comprise 4,000 INFO, 2,000 DEBUG, 2,000 WARN, and
1,000 ERROR identities. The caps provide 33% series and 60% per-service template
headroom. Metric/trace/edge/system caps remain 2,400/2,400/500/200. The retained
log-template dictionary holds 4,000 live names plus the dense fixture's 500
historical names, leaving 500 slots under its independent 5,000-tenant cap.
Finalized historical series do not spend active slots.

The miner cap, active per-service name cap, active signal cap, and retained
name cap each need coverage checks. A zero limiter overflow counter alone does
not rule out miner or dictionary identity loss. Run the focused identity checks
and optional footprint measurements without starting a server:

```sh
rtk proxy go test -tags loadtest ./test/loadsim -run '^TestProductionLogIdentity' -count=1 -v
rtk proxy go test -tags loadtest ./test/loadsim -run '^$' -bench '^BenchmarkProductionLogIdentity' -benchtime=1x -count=1
```

At 9,000 log series per five-minute window, seven days adds 18,144,000 log
buckets. Logs allocate no latency sketches. A small 12-window SQLite sample
measured about 87 bytes per added log bucket, projecting 1.47 GiB for these log
buckets alone, before raw records, other signals, WAL, and future rollups. The
one-window diagnostic retained-heap increase over the old profile was about
6.3 MiB. Neither measurement proves the 4 GiB cgroup or disk budget under load.
Repeat combined capacity acceptance with this identity-preserving profile;
keep zero overflow as the acceptance requirement.

Prometheus gets its own 32 GiB PVC with seven-day time retention and a 24 GiB size
cap. **The first limit reached wins.** Seven days is therefore a target that
requires measured disk headroom; the size cap can delete older samples earlier.
Monitor `prometheus_tsdb_*`, PVC bytes, and the oldest queryable sample. Expand
storage and adjust the size cap before it threatens the target. Neither PVC is
a replicated database or a backup. See [retention and compaction](https://prometheus.io/docs/prometheus/latest/storage/).

Both Deployments use one replica, `Recreate`, and `ReadWriteOncePod`. Never scale
these owners above one, force-delete an owner, or run a maintenance pod on its
live PVC. Upgrades have downtime. The app init container runs the existing
`otelcontext migrate up` command with the same image, environment, and PVC as
the server; `DB_AUTOMIGRATE=false` and `AGGREGATE_ALLOW_REBUILD=false` remain set.
Existing unversioned data needs an operator-reviewed `migrate status` and, when
applicable, `migrate baseline --from RELEASE` before `migrate up`. A failed or
incompatible migration stops the rollout. Never repair it by rebuilding the DB.

## OTLP, PromQL, and tenant contract

OtelContext uses `PROMETHEUS_URL=http://prometheus:9090`,
`PROMETHEUS_REQUIRED=true`, and `PROMETHEUS_TENANT_ID=default`. The companion is
dedicated to that tenant. It has no public ingress. The application must enforce
the configured tenant for both forwarding and query proxying; arbitrary tenant
headers must not grant another tenant access to this shared TSDB. Existing
legacy/shadow/aggregate modes and their tenant behavior remain application
contracts. This packaging selects aggregate mode; it does not implement auth.

The write proxy admits at most 16 active requests and 256 waiters by default,
with a shared 128 MiB protobuf payload budget covering both groups. Excess
requests receive retryable backpressure. This is not an RSS limit: decoded Go
objects, network decoding, and active encoded copies need additional memory.
Readiness probes use separate bounded admission from query and ingest traffic.

MCP `trace_graph` returns bounded retained span and log evidence. Correlated logs
have a separate 100-row/256 KiB budget, so noisy logs do not hide the span graph.
Truncated responses report their limits and direct callers to paginated
`search_logs`. Retained trace spans are capped at 1,000 rows and 256 KiB payload;
missing span payload is reported as partial coverage.

Native OTLP HTTP writes use `/api/v1/otlp/v1/metrics`. The app must preserve OTLP
types and timestamps, forward failures to clients, and make required backend
failure visible in `/ready`. Gauge and **cumulative** sum/histogram exports are
the supported production input. Set SDK metric temporality to cumulative, for
example `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=cumulative` where the SDK
supports it. Delta-to-cumulative conversion is experimental in Prometheus and is
disabled here. Delta metrics must not be acknowledged as durable data if the
backend rejects or drops them. An exporter setting or empty OTLP response alone
is not proof that delta semantics are supported. See the
[official OTLP guide](https://prometheus.io/docs/guides/opentelemetry/).

The local patched-image check returned HTTP 500 for a delta monotonic sum and
stored no corresponding series. Gauge, cumulative counter/rate, and explicit
histogram ingestion/query checks passed. WAL replay and an offline full-TSDB
restore into a fresh volume preserved the test gauge. Those small fixture checks
do not establish seven-day capacity or full signal semantics.

`NoTranslation` preserves dotted metric and label names for existing host UI
metrics. Examples of valid PromQL selectors are:

```promql
{__name__="system.cpu.utilization","host.name"="host-a"}
rate({__name__="http.server.requests","service.name"="checkout"}[5m])
```

Bare dotted identifiers are invalid PromQL. The application must construct quoted
selectors and quoted label matchers. Do not rewrite names by replacing dots with
underscores. Producers must not reuse the same name with different units or
metric types because this strategy adds no disambiguating unit or counter suffix.
Resource promotion explicitly includes service, instance, host, cloud, and
Kubernetes identity. New promoted labels increase cardinality; no wildcard
promotion is enabled. A 30-minute out-of-order window allows bounded delivery
reordering, not arbitrarily old backfill. See the
[OTLP configuration schema](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#configuration-file).

Query execution is capped at 20 seconds, five million loaded samples, and eight
concurrent queries. The frontend/proxy deadline must leave room for the backend
response. These bounds can reject expensive PromQL without restricting the
language to a local subset. No snapshot/admin, lifecycle, remote-write receiver,
or experimental PromQL flag is enabled.

## HTTPS, probes, and shutdown

The existing Istio ingress accepts TLS on 443 using the operator's certificate.
Port 80 only redirects to HTTPS. SDKs must start with HTTPS, including gRPC over
TLS on 443; they should not rely on a redirect for telemetry writes. The
VirtualService sends OTLP gRPC method paths to 4317, OTLP HTTP `/v1/*` to Service
port 4318, and MCP, WebSocket, UI, and read APIs to 8080. Port 4318 maps to the
same application HTTP listener. Istio performs no ingest retries here.

Services are ClusterIP only. Plain HTTP/h2c from the ingress to application pods
is the intended TLS termination contract; sidecar injection is disabled in this
profile. Do not expose those Services through another public LoadBalancer or
NodePort. This profile adds no authentication or authorization policies.

The app uses `/live` for startup/liveness and `/ready` for readiness. Saturation,
disk pressure, aggregate replay, or a required Prometheus outage must remove the
app from routing without causing a liveness restart loop. Prometheus uses
`/-/healthy` and `/-/ready`. Probes are pod-local HTTP; never probe through the
public gateway. Startup allows ten minutes for initialization/WAL replay.

On SIGTERM, the app's shutdown path withdraws readiness and drains with its
30-second application deadline; the pod grants 60 seconds. Prometheus receives
120 seconds. No pre-stop shell or sleep consumes those budgets. Confirm clean
shutdown receipts and WAL replay after an upgrade. If rollout fails, keep the
PVCs, image digests, rendered config, and backup bundle; restore a compatible
backup into fresh storage instead of attempting an unsupported schema downgrade.

## Backup and restore

Back up the application and Prometheus as a pair. The existing application
`backup create` contract requires a stopped process, a successful shutdown
receipt, and the exact binary that wrote it. The bundle covers main SQLite,
aggregate SQLite, DLQ, and configured generated TLS material. It does **not**
include the companion. Gateway certificates remain operator-managed.

1. Pause producers or buffer exports upstream. Scale the app to zero and wait
   until its pod has terminated cleanly. Do not force termination. Preserve
   `/var/lib/otelcontext/.otelcontext-shutdown-success.json`.
2. While the app remains stopped, mount its PVC in a temporary maintenance pod
   with UID/GID/fsGroup 65532, the exact app image digest, and the same resolved
   ConfigMap environment. Do not include the migration init container. Mount a
   separate backup PVC at `/backup` and run `/otelcontext backup create --out
   /backup`. Capture the CLI report and completed bundle name. A `.partial`
   directory is not a successful backup. Delete only the maintenance pod after
   completion, preserving both PVCs.
3. Scale Prometheus to zero and wait for clean termination. Snapshot its stopped
   PVC using the CSI driver, or copy the **entire** TSDB directory to separate
   backup storage while no writer is mounted. Preserve blocks, `wal/`, `wbl/`,
   and `chunks_head/` if present. Never copy a live TSDB or omit the WAL and then
   claim a complete backup. The admin snapshot API is deliberately disabled.
4. Record the shared write-pause interval, both image digests, app bundle hash,
   companion snapshot/archive hash, source SHA, configuration, and schema
   versions. Upload them off-volume. Restore-test the pair before calling it a
   recoverable backup. Restart Prometheus before OtelContext and resume producers
   after both readiness probes pass.

Restore into **fresh PVCs and different application paths**, retaining the old
volumes. Application source-identity checks include paths, so a fresh volume
mounted at the old path is insufficient. For example, mount the new app volume
at `/var/lib/otelcontext-restored` and set `DB_DSN`, `AGGREGATE_DB_PATH`, `DLQ_PATH`,
`DATA_DISK_PATH`, and `TLS_CACHE_DIR` consistently under that directory. Preserve
mode and driver. Mount the completed backup bundle read-only at `/backup`.

Restore the whole Prometheus TSDB to its new stopped PVC with ownership 65532,
then start the exact compatible companion image and verify readiness and an
older time-range query. Run `/otelcontext backup restore --bundle
/backup/COMPLETED_BUNDLE` in a single maintenance pod using the fresh application
paths and exact app image. The CLI checks artifacts, schema and readiness; the
required companion must be reachable during that proof. Only after it succeeds
should the normal Deployment mount the new PVC with those same paths and run its
migration/status checks. Verify seven-day range queries, tenant isolation, raw
examples, DLQ state, and ingest before switching traffic. A backup of one store
cannot reconstruct the other store or guarantee an atomic cross-store commit.
