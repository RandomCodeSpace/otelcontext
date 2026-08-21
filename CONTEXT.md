# OtelContext

Self-hosted OTLP observability platform: a single Go binary that ingests traces, logs, and metrics, and serves triage-focused APIs. This glossary covers the aggregate engine's domain language.

## Language

### Aggregate engine

**Series**:
One aggregated telemetry stream, identified by a SeriesKey. All accepted telemetry contributes to some series; series counts represent traffic, not sampling.
_Avoid_: time series (ambiguous with the legacy TSDB), stream

**SeriesKey**:
The complete identity of a series — a fixed set of dictionary IDs and small enums. Nothing outside the attribute allowlist may influence it.
_Avoid_: metric key, group key

**Active series**:
A series present in a mutable (current or allowed-late) window. Only active series consume the cardinality budget; historical series do not.

**Dictionary**:
The durable, tenant-scoped mapping from canonical strings (service, operation, metric name, dimension keys/values) to numeric IDs. IDs are owned by the database, never minted independently in memory.
_Avoid_: interner, symbol table

**Dimension tuple**:
The canonical, order-independent encoding of an operator-configured set of dimension key/value pairs, interned as a single dictionary entry.
_Avoid_: attribute map, label set

**Attribute allowlist**:
The fixed list of attributes permitted to affect series identity. Everything else is presentation data or banned outright (IDs, URLs, messages, bodies).

**Route normalization**:
Deterministic replacement of variable URL path segments with placeholders, applied only to genuine URL/path values. Never learned, never inferred.

**Overflow series**:
The per-service catch-all series that absorbs telemetry past a cardinality cap. Totals are preserved; identity detail is collapsed.
_Avoid_: drop bucket (nothing is dropped from totals)

**Window**:
A UTC-aligned five-minute tumbling interval. Finalized windows are immutable; mutable windows are owned by the engine.

**Exemplar**:
A bounded raw sample (trace, span, or log) retained for diagnostics alongside an aggregate bucket. Eligibility is universal for errors; persistence is always capped.

**Delta**:
The compact aggregate contribution of one Export request to one series and window, produced by request-local reduction. Deltas are what gets committed; buckets are what finalization builds from them.

**Group-commit writer**:
The single component that batches deltas from many Export requests into one durable SQLite transaction and then applies them to the in-memory shards. The only shard mutator.

**Delta log**:
The append-only table of committed deltas for mutable windows. Replayed on restart; consumed and deleted atomically by finalization.

**Finalization**:
The transactional step that materializes a window's bucket rows from its delta-log entries and deletes those entries. After finalization a window is immutable and never re-enters memory.

**Arrival time**:
The single timestamp captured per Export request, used to evaluate lateness and future-skew for every point in that request.

**Producer**:
One concrete emitter of a cumulative metric series — an instance, pod, or process. Identified internally by a ProducerID that never affects series identity.
_Avoid_: instance (overloaded), source

**Cumulative baseline**:
The per-(series, producer) record of the last cumulative value, start time, and timestamp, used to convert cumulative points into deltas and to detect resets. Durable in the same commit as the deltas it justifies.
_Avoid_: prior state, counter cache

### Signals

**Log template**:
A Drain-mined pattern identifying a cluster of similar log lines. Template IDs are stable across restarts and double as log-series identity.
_Avoid_: log cluster ID (in aggregate contexts), pattern
