# Raw physical capacity and simulated rolling retention

This fixture repeats a bounded observed raw sample across seven days, then
advances the production exemplar purge cutoff hourly while appending new raw
rows. It is synthetic replication. It does not replay OTLP, rerun the sampler,
prove ingestion throughput, or replace the combined constrained acceptance run.

Use a stopped, checkpointed **owned copy** as `--source`. The simulator opens
that copy with `mode=ro&immutable=1` and verifies its SHA256 at completion.
It refuses a nonempty source WAL and an existing output directory. Do not point
it at a live database. It creates exactly one populated output database, with
an explicit default 12 GiB physical allocation bound. The output and evidence
remain available after the process exits.

```sh
rtk proxy go build -buildvcs=false -o /tmp/rawcapacity ./test/production/rawcapacity
rtk proxy env GOMAXPROCS=4 GOMEMLIMIT=3GiB python3 test/production/rawcapacity/simulate.py \
  --helper /tmp/rawcapacity --source /owned/scratch/observed.db \
  --out /owned/scratch/full-7d \
  --source-start 2026-10-18T19:00:00Z --source-hours 2 \
  --end 2026-10-18T21:00:00Z --hours 168 --roll-hours 6
```

The source sample must include a correlated trace with spans and logs. Whole
trace groups are selected by the parent timestamp, including children across
the window boundary. Standalone logs and already-dangling records retain their
observed semantics. Source trace IDs are deterministically hashed with the copy
number across all three tables. Span and parent-span IDs remain unchanged within
the rewritten trace namespace. Only database row IDs, trace IDs, and timestamp
offsets change. The timestamp rewrite preserves nanoseconds. Compressed blobs,
log bodies, service names, severity, duration, and completeness metadata are
copied without decompression or normalization. Zero-span headers are retained.
A separate tiny correlated sentinel has a unique FTS token and lies outside the
initial retention range.

The helper provisions the main schema and FTS with `migrate.Up`, and requires
the versioned ledger and enabled FTS objects to match with `migrate.RequireExact`.
It requires the production optional-FTS provisioning correction. The initial
2026-10-19 physical run began before that correction landed and used the same
canonical `storage.AutoMigrateModels` FTS setup on its empty versioned fixture.
It represents the corrected intended deployment, not the old tested artifact.
The `validate` action runs corrected `migrate.Up` on an existing owned fixture,
then `migrate.RequireExact` with SQLite writes disabled. It checks that
`total_changes`, `schema_version`, and database/WAL SHA256 did not change,
proving that the complete canonical index needs no rebuild. The byte comparison
starts after connection tuning and its checkpoint: production connection setup
can update SQLite header counters even for a read action. This distinction
avoids attributing connection initialization writes to migration. The action
does not generate another fixture.
All indexes and FTS triggers stay enabled
during inserts and deletes. No full VACUUM or index-free bulk-loading step is
used. SQLite cache, synchronous mode, WAL autocheckpoint, and journal-size-limit
settings match the production defaults. The sampling interval for physical
peaks is 50 ms, so sub-interval peaks are not claimed as measured.

Seeding uses direct parameterized inserts into the production schema to
preserve the observed compressed bytes exactly. Each subsequent hour is staged
in a small SQLite input file beside the fixture directory, with a 64 MiB bound.
The Go helper attaches that input read-only and appends in 500-row statements
through the same production database pool as the concurrent purger. These
inserts do not pass through the ingest policy. Purging uses the existing
`Repository.PurgeExemplarsBatched` cutoff API with production defaults of 10,000
rows and 5 ms inter-batch sleep. The production SQLite pool permits one open
connection, so append statements and purge batches take turns using that
connection. The report counts append batches completed before purge finishes.
The cutoff advances in simulated time; the host clock is untouched. This
exercises the production cutoff API and shared pool, not the hourly scheduler
timer or OTLP ingestion throughput. The staged input files are experiment
overhead outside the measured application directory. The allocation bound is
checked during seeding and before/after each rolling hour; it is not an OS quota.

The original physical run's first rolling cycle used independent Python and Go
writer connections and failed with `SQLITE_BUSY_SNAPSHOT`. That topology did
not match the application's pool. Its original report and diagnostics are
preserved separately from the corrected recovery. Recovery verified the first
hour's deterministic IDs and counts, finished its partial purge at the same
cutoff, then ran six further cycles on the same database with the shared pool.
The command deliberately has no automatic resume mode: a failed partial cycle
requires evidence-based reconciliation before further appends.

The output includes:

- `run.json`: source/helper/simulator hashes, generation timestamps, exact row counts, storage
  snapshots, sampled DB/WAL/allocated peaks, each cutoff's purge counts and
  duration, concurrent append duration, and final verification results.
- `source-inspection.json`, `seed-inspection.json`, `final-inspection.json`:
  SQLite schema, per-table/index allocated pages and payload bytes, freelist,
  zero-span header count, and signal row counts.

Checks include exact seeded counts, expired sentinel trace reads and FTS
deletion, retained trace/log repository reads with decoded attributes and
consistent references, repository FTS search before/after rolling expiry,
cancelled purge, absence of expired traces/logs after every tick, FTS
external-content integrity, SQLite `quick_check`, and unchanged source hash.
Do not infer physical shrink from deleted-row counts: compare allocated bytes,
file length, freelist, and peaks. Six hourly ticks measure short rolling reuse;
they do not prove indefinite steady state. The raw file excludes the aggregate
database, DLQ, TLS cache, and accumulated investigation records. Add measured
allowances for those when recommending an application disk budget.

Focused checks:

```sh
rtk proxy go test -buildvcs=false ./test/production/rawcapacity
rtk proxy env RAWCAPACITY_HELPER=/tmp/rawcapacity python3 -m unittest discover \
  -s test/production/rawcapacity -v
```

The second command includes a tiny fully indexed fixture with zero-span
headers, nanosecond timestamps, a trace crossing the sample boundary, and two
rolling production purges. It does not create a large database. No deployment
configuration is changed by this tool.
