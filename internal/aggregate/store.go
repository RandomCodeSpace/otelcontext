package aggregate

import (
	"errors"
	"fmt"
	"time"
)

// The durable aggregate store (#173, decided in #162 / #160 / #166).
//
// This file owns the store's TYPES and CONTRACT; store_sqlite.go owns the only
// implementation. The split is the escape hatch #162 asked for: if the SQLite
// benchmark gate fails, a second implementation satisfies this interface
// without the writer, the engine or the ingest path noticing.
//
// Two rules are structural, not conventional:
//
//  1. No *sql.Tx, *sql.DB or any transaction-scoped primitive escapes the
//     implementation. Write ownership is coarse: a caller hands over a whole
//     GroupBatch and gets back "committed" or "not committed". There is no way
//     for a caller to hold a transaction open across a lock, which is how the
//     single SQLite writer stays available.
//  2. Every read method enforces its own maximum. The worst case is 6,000
//     series x 2,016 windows; a caller is not trusted to bound that itself.

// StoreSchemaVersion is the aggregate schema version written into
// aggregate_meta at creation and verified at every open. There are no
// automatic migrations in v1 (#162): a mismatch fails startup and the operator
// chooses between an older binary and AGGREGATE_ALLOW_REBUILD=true.
//
// v2 re-keyed aggregate_delta_log on (window_start, series_id) and dropped its
// append sequence (#173). A v1 file cannot be read by this binary and is not
// migrated: the rows it holds are unfinalized deltas with a retention horizon
// measured in minutes, so AGGREGATE_ALLOW_REBUILD loses far less than a
// migration would risk getting wrong.
//
// v3 added request_count and error_request_count to both aggregate_delta_log
// and aggregate_buckets (#197 Q5). A v2 file holds only span counts, and there
// is no way to derive a request count from them after the fact — the parent and
// kind of every span it summarised are long gone. Backfilling zeros would make
// every historical window read "0 requests, N spans", which is worse than an
// operator-acknowledged rebuild, so the fail-closed policy stands unchanged.
//
// v4 added the eight hist_* columns that carry an OTLP histogram point's
// population statistics and its accuracy metadata (#199). A v3 file has no
// column to put them in and no way to reconstruct them, so the same
// rebuild-or-downgrade choice applies.
//
// v5 added aggregate_log_template — the durable log-template miner state —
// plus the dict_id/series_id high-watermark meta keys that dictionary GC
// requires (#200). A v4 file has neither, and a binary that started GC against
// a MAX(id)+1 reseed could re-mint an ID a finalized bucket still names, so the
// fail-closed policy stands: run an older binary or accept the rebuild.
const StoreSchemaVersion = 5

// Meta keys that carry monotonically increasing identity high-watermarks
// (#200 Q1). MAX(id)+1 stopped being a safe reseed the moment GC could delete
// the highest ID, so allocation comes from these instead.
const (
	MetaDictWatermark   = "dict_id_high_watermark"
	MetaSeriesWatermark = "series_id_high_watermark"
)

// MaxDictRows and MaxSeriesRows bound the startup identity warm-up. They are
// far above the #158 caps; they exist so a corrupted or hostile file cannot
// make startup allocate without bound. Exceeding one fails startup with a
// *PreloadError rather than truncating the load in silence (#200 Q3).
//
// Declared as var (not const) for the same reason MaxReadRows is: a test has
// to exercise the fail-fast path without seeding two million rows through a
// race-instrumented SQLite. Nothing outside a test may assign them.
var (
	MaxDictRows   = 2_000_000
	MaxSeriesRows = 500_000
)

// Store errors.
var (
	// ErrSaturated reports that the group-commit writer refused admission
	// because one of its three bounds (pending bytes, waiters, deltas) is
	// full. The ingest path maps it to gRPC RESOURCE_EXHAUSTED / HTTP 429,
	// exactly like the raw pipeline's ErrQueueFull. Errors returned by the
	// writer satisfy errors.Is(err, ErrSaturated) and carry which bound
	// tripped via *SaturationError.
	ErrSaturated = errors.New("aggregate: store admission saturated")

	// ErrStoreClosed reports use of a closed store or writer.
	ErrStoreClosed = errors.New("aggregate: store closed")

	// ErrSelectorUnbounded reports a read whose selector is missing a
	// mandatory bound (window range, tenant scope) or asks for more than the
	// store-side row cap.
	ErrSelectorUnbounded = errors.New("aggregate: unbounded selector")
)

// SchemaError reports a database whose aggregate schema this build cannot use:
// a partial schema, missing meta, or a version mismatch. It is fatal at
// startup by design — silently adopting an unknown layout is how a store
// starts returning numbers nobody can explain.
type SchemaError struct {
	// Reason is a short machine-ish description ("missing_meta",
	// "partial_schema", "version_mismatch").
	Reason string
	// Key names the meta key when Reason is "version_mismatch".
	Key string
	// Got and Want are the mismatched values.
	Got, Want string
	// Detail carries anything else worth printing.
	Detail string
}

func (e *SchemaError) Error() string {
	switch e.Reason {
	case "version_mismatch":
		return fmt.Sprintf("aggregate store: %s is %s, this build writes %s "+
			"(no automatic migrations in v1; run an older binary or set AGGREGATE_ALLOW_REBUILD=true to DESTROY and recreate aggregate data)",
			e.Key, e.Got, e.Want)
	default:
		return fmt.Sprintf("aggregate store: %s: %s "+
			"(set AGGREGATE_ALLOW_REBUILD=true to DESTROY and recreate aggregate data)", e.Reason, e.Detail)
	}
}

// SaturationError reports which admission bound refused a CommitGroup.
type SaturationError struct {
	// Bound is "bytes", "waiters" or "deltas".
	Bound string
	// Current is the value at the moment of refusal; Limit is the cap.
	Current, Limit int64
}

func (e *SaturationError) Error() string {
	return fmt.Sprintf("aggregate: commit admission refused: %s %d >= %d", e.Bound, e.Current, e.Limit)
}

// Is makes errors.Is(err, ErrSaturated) true for every saturation refusal.
func (e *SaturationError) Is(target error) bool { return target == ErrSaturated }

// SeriesID is the durable identity of a series. IDs are minted by the database
// (ADR 0001) so a recovered bucket always resolves to an identity that exists.
type SeriesID int64

// DictRow is one dictionary registration awaiting commit. ID is assigned by the
// registrar before the row is written, because the hot path needs the ID
// synchronously; the row itself lands in the same transaction as the first
// delta that references it (#162's first atomicity invariant).
type DictRow struct {
	ID       uint32
	TenantID uint32
	Kind     Kind
	Value    []byte
}

// SeriesRow is one series registration awaiting commit. Identity columns are
// explicit — there is no generic "variant" column (#162).
type SeriesRow struct {
	ID  SeriesID
	Key SeriesKey
}

// DeltaRow is one delta-log row: the accumulated, not-yet-finalized
// contribution to one (series, window).
//
// On the write side it carries one group commit's contribution, which the store
// merges into the row already standing for that (series, window) — the log is
// keyed by identity, not by an append sequence, so a window's row count tracks
// its active series rather than the number of commits that touched it (#173).
type DeltaRow struct {
	// SeriesID and WindowStart identify the bucket the row contributes to.
	SeriesID    SeriesID
	WindowStart int64
	// Delta is the aggregate contribution. Its sketch is encoded with the
	// versioned codec from #157 on write and decoded on read.
	Delta *AggregateDelta
}

// BaselineRow is one durable cumulative baseline (#166). It is upserted inside
// the same group commit as the deltas it justifies, so a restart never has to
// re-seed a counter it already acknowledged points for.
type BaselineRow struct {
	SeriesID SeriesID
	Producer ProducerID
	Baseline Baseline
}

// GroupBatch is one group commit: everything that must become durable together.
//
// The three atomicity invariants of #162 are structural here — they are not a
// convention the implementation is asked to honour, they are the shape of the
// only write entry point:
//
//	Dicts + Series + Deltas in one struct  -> registration and the first
//	                                          referencing delta cannot split.
//	Baselines in the same struct           -> a baseline cannot become durable
//	                                          without the delta it justifies
//	                                          (and vice versa).
//	FinalizeWindow's materialize+delete    -> the third invariant, one method.
type GroupBatch struct {
	Dicts     []DictRow
	Series    []SeriesRow
	Deltas    []DeltaRow
	Baselines []BaselineRow
	// Templates are identity-critical log-template mutations: a new template,
	// a pattern generalization, or an alias change (#200 Q4). They ride the
	// same transaction as the delta that used the resulting identity, because
	// a 15-minute snapshot alone lets acknowledged identity state vanish in a
	// crash — the bucket would survive naming a template ID that the reloaded
	// miner has never heard of.
	Templates []TemplateRow
}

// Empty reports whether the batch would commit nothing.
func (b *GroupBatch) Empty() bool {
	return len(b.Dicts) == 0 && len(b.Series) == 0 && len(b.Deltas) == 0 &&
		len(b.Baselines) == 0 && len(b.Templates) == 0
}

// TemplateRow is one durable log-template record (#200 Q4).
//
// It carries exactly what a reload needs to rebuild the prefix tree, resolve a
// historical template ID, and re-enforce the per-partition cap. It carries NO
// raw log sample: the miner keeps one in memory for diagnostics, and persisting
// it would turn the aggregate file into a credential and PII sink for the sake
// of a field exemplars already provide.
type TemplateRow struct {
	// ID is the immutable surrogate template identity — the same uint32 the
	// log_template dictionary minted, and the NameID of every log series that
	// used it.
	ID uint32
	// Tenant and Service name the partition. They are the strings, not
	// dictionary IDs: the miner is keyed by them and a reload must rebuild
	// its partitions before any dictionary lookup is warm.
	Tenant, Service string
	// PatternVersion increments on every generalization of Tokens. It makes a
	// stale periodic stats write unable to overwrite a newer pattern.
	PatternVersion uint32
	// Tokens is the token pattern, NUL-joined. Tokens never contain
	// whitespace (the tokenizer splits on it) and never contain NUL.
	Tokens string
	// Seq is the partition-local creation ordinal. Convergence keeps the
	// lower one, so it has to survive a restart or two restarts could pick
	// different survivors for the same pair.
	Seq uint64
	// IsOther marks the partition's pre-created overflow identity.
	IsOther bool
	// AliasOf is the surviving template this ID forwards to, or 0.
	AliasOf uint32
	// Count, FirstSeen and LastSeen are the non-identity statistics. They are
	// refreshed by the periodic dirty-partition write, not by the identity
	// commit path.
	Count               uint64
	FirstSeen, LastSeen int64
}

// TemplateStatRow is the non-identity half of a template: the counters a
// periodic dirty-partition write refreshes. Losing one costs a count, not an
// identity, so it does not need to ride a group commit.
type TemplateStatRow struct {
	ID                  uint32
	Count               uint64
	FirstSeen, LastSeen int64
}

// SweepStats is the outcome of one identity sweep.
type SweepStats struct {
	// Series, Dict and Templates count deleted rows per table.
	Series, Dict, Templates int64
	// Duration is the wall time of the delete transaction.
	Duration time.Duration
}

// WatermarkStore is the optional Store capability that carries the identity
// high-watermarks. It is separate from Store so an implementation predating
// #200 still satisfies Store; the registrars degrade to MAX(id)+1 without it,
// which is correct exactly as long as nothing collects.
type WatermarkStore interface {
	// Watermarks returns the persisted dictionary and series high-watermarks:
	// the next ID each allocator may mint. Zero means "not yet stamped".
	Watermarks() (uint32, SeriesID, error)
}

// GCSnapshot is one consistent read of every identity table, taken inside a
// single read transaction (#200 Q1).
//
// One transaction, not four queries: a series marked live from the bucket scan
// and a dictionary row read a moment later have to describe the same instant.
// Read them separately and a commit landing in between makes a brand-new
// series invisible to the reference set while its brand-new name is visible to
// the candidate set — and GC deletes the name of a series that exists.
type GCSnapshot struct {
	// Referenced is every series ID named by aggregate_buckets,
	// aggregate_delta_log or aggregate_baseline.
	Referenced map[SeriesID]struct{}
	// Series, Dict and Templates are the identity tables themselves.
	Series    []SeriesRow
	Dict      []DictRow
	Templates []TemplateRow
}

// GCStore is the Store capability the dictionary/series collector needs. It is
// separate from Store for the same reason WatermarkStore is.
type GCStore interface {
	// GCSnapshot reads the reference set and all three identity tables inside
	// ONE read transaction. It runs on the READ pool, without the writer
	// lock: the full scan is the part of GC that must never become an
	// ACK-latency incident.
	GCSnapshot() (*GCSnapshot, error)

	// LoadTemplates returns every durable log-template row, capped at max.
	LoadTemplates(max int) ([]TemplateRow, error)

	// SweepIdentities deletes the given series, dictionary and template rows
	// in ONE transaction, series first. It runs under the writer lock. A
	// partial sweep is never observable: either every row named here is gone
	// or none of them are.
	SweepIdentities(series []SeriesID, dict []uint32, templates []uint32) (SweepStats, error)

	// SaveTemplateStats refreshes the non-identity template counters. Rows
	// whose template no longer exists are ignored, not created.
	SaveTemplateStats(rows []TemplateStatRow) error
}

// SeriesInfo is a series' durable identity, resolved back from its ID.
type SeriesInfo struct {
	ID  SeriesID
	Key SeriesKey
}

// Bucket is one store-owned (window, series) row.
type Bucket struct {
	WindowStart int64
	SeriesID    SeriesID
	Delta       *AggregateDelta
	// Source says which table the row came from. A window may hold a
	// materialized bucket and a not-yet-finalized delta row for one series;
	// both are real contributions and both must be counted.
	Source BucketSource
}

// BucketPage is one page of a ReadBuckets call.
type BucketPage struct {
	// Buckets are the rows of this page, ordered by (window, series, source).
	Buckets []Bucket
	// Limit is the row limit that was applied.
	Limit int
	// Truncated reports that more rows matched than this page returned. It is
	// result-completeness metadata and is INDEPENDENT of Coverage: a result
	// can be full-coverage and truncated at the same time (#197 Q4).
	Truncated bool
	// Next resumes the read immediately past the last returned row. Only
	// meaningful when Truncated.
	Next BucketCursor
}

// BucketSource says which durable table a row came from. It exists so a paged
// read has a TOTAL order to resume from: (window_start, series_id) is unique
// within each table but a window can legitimately hold a materialized bucket
// AND a not-yet-finalized delta row for the same series.
type BucketSource uint8

// BucketSource values, in scan order.
const (
	// SourceFinalized is a row from aggregate_buckets.
	SourceFinalized BucketSource = 0
	// SourceDelta is a not-yet-finalized row from aggregate_delta_log.
	SourceDelta BucketSource = 1
)

// BucketCursor is the keyset position of a paged bucket read. Treat it as
// opaque: obtain it from BucketPage.Next and hand it back through
// Selector.After.
type BucketCursor struct {
	WindowStart int64
	SeriesID    SeriesID
	Source      BucketSource
}

// zero reports the start-of-range cursor.
func (c BucketCursor) zero() bool {
	return c.WindowStart == 0 && c.SeriesID == 0 && c.Source == SourceFinalized
}

// After reports whether row (window, id, src) sorts strictly after the cursor.
// It is the Go-side twin of the SQL keyset predicate, so an in-memory Store
// implementation pages identically to the SQLite one.
func (c BucketCursor) After(window int64, id SeriesID, src BucketSource) bool {
	if window != c.WindowStart {
		return window > c.WindowStart
	}
	if id != c.SeriesID {
		return id > c.SeriesID
	}
	return src > c.Source
}

// Selector bounds a bucket read. Both window bounds and a tenant are
// mandatory: an unbounded scan of seven days of history is not a query, it is
// an outage (#162).
type Selector struct {
	// TenantID scopes the read. Required.
	TenantID uint32
	// Start and End bound the window range, inclusive of Start and exclusive
	// of End, in UTC-aligned Unix seconds. Required, Start < End.
	Start, End int64
	// Signal, when non-zero, restricts the read to one signal.
	Signal Signal
	// SeriesIDs, when non-empty, restricts the read to these series.
	SeriesIDs []SeriesID
	// Limit caps returned rows. Zero takes MaxReadRows; anything above it is
	// clamped down, never up.
	Limit int
	// After resumes a paged read immediately past this cursor. The zero value
	// starts at the beginning of the range.
	After BucketCursor
	// SketchOnly restricts the read to rows that carry a sketch. It is the
	// percentile path's filter: a row with no sketch cannot move a quantile,
	// so paging past it is wasted work.
	SketchOnly bool
}

// GroupBy selects the grouping of a SumBuckets aggregation. Zero groups
// everything into a single row.
type GroupBy uint8

// GroupBy flags, combinable.
const (
	// GroupByWindow emits one row per window start.
	GroupByWindow GroupBy = 1 << iota
	// GroupByService emits one row per service dictionary ID.
	GroupByService
	// GroupBySignal emits one row per signal.
	GroupBySignal
)

// SumRow is one grouped aggregation over the store's rows. Fields outside the
// requested grouping are zero.
//
// The counters are the SUMmable subset of AggregateDelta: everything a scalar
// dashboard total is built from. Sketches are deliberately absent — a quantile
// sketch cannot be merged by SQL, and pretending otherwise is how a p99 turns
// into an average (#197 Q1).
type SumRow struct {
	WindowStart int64
	ServiceID   uint32
	Signal      Signal

	Count             uint64
	ErrorCount        uint64
	RequestCount      uint64
	ErrorRequestCount uint64
	DurationCount     uint64
	DurationSum       float64
	LogCount          uint64
}

// MaxReadRows is the store-side cap on rows returned by one ReadBuckets call
// and on IDs accepted by one ResolveSeries call.
//
// Declared as var (not const) so tests can temporarily shrink it and exercise
// the truncation and paging paths without seeding tens of thousands of rows
// through a race-instrumented SQLite — same reason storage.sqliteP99RowCap is
// a var. Nothing outside a test may assign it.
var MaxReadRows = 20000

// MaxReadWindowSpan bounds the window range one ReadBuckets call may cover: the
// 168 h retention horizon plus one window of slack, expressed in seconds.
const MaxReadWindowSpan = int64(168*time.Hour/time.Second) + int64(WindowSize/time.Second)

// Validate applies the mandatory bounds and returns the effective row limit.
func (s Selector) Validate() (int, error) {
	if s.TenantID == 0 && s.Start == 0 && s.End == 0 {
		return 0, fmt.Errorf("%w: selector is empty", ErrSelectorUnbounded)
	}
	if s.Start <= 0 || s.End <= 0 || s.End <= s.Start {
		return 0, fmt.Errorf("%w: window range [%d,%d) is not a bounded forward range", ErrSelectorUnbounded, s.Start, s.End)
	}
	if s.End-s.Start > MaxReadWindowSpan {
		return 0, fmt.Errorf("%w: window range spans %ds, cap is %ds", ErrSelectorUnbounded, s.End-s.Start, MaxReadWindowSpan)
	}
	if len(s.SeriesIDs) > MaxReadRows {
		return 0, fmt.Errorf("%w: %d series ids, cap is %d", ErrSelectorUnbounded, len(s.SeriesIDs), MaxReadRows)
	}
	limit := s.Limit
	if limit <= 0 || limit > MaxReadRows {
		limit = MaxReadRows
	}
	return limit, nil
}

// FinalizeStats is the outcome of finalizing one window.
type FinalizeStats struct {
	// WindowStart is the finalized window.
	WindowStart int64
	// Buckets is the number of bucket rows written or merged.
	Buckets int
	// DeltaRows is the number of delta-log rows incorporated and deleted.
	DeltaRows int
	// Duration is the wall time of the transaction.
	Duration time.Duration
}

// PurgeStats is the outcome of one retention purge.
type PurgeStats struct {
	// Buckets and Deltas count deleted rows.
	Buckets int64
	Deltas  int64
	// Baselines counts baselines dropped because their series has no
	// remaining data.
	Baselines int64
	// Duration is the wall time of the whole purge.
	Duration time.Duration
}

// BacklogStats describes the delta-log backlog — the health bound #160 requires
// an operator to be able to alarm on.
type BacklogStats struct {
	// Rows is the number of delta-log rows currently awaiting finalization.
	Rows int64
	// OldestWindow is the oldest un-finalized window start, 0 when empty.
	OldestWindow int64
	// Bytes is the approximate delta-log payload size.
	Bytes int64
}

// Store is the durable aggregate store. Implementations must be safe for
// concurrent use; CommitGroup and FinalizeWindow serialize internally on the
// single writer.
type Store interface {
	// CommitGroup writes one group batch inside exactly one transaction.
	// Either everything in the batch is durable when it returns nil, or
	// nothing in it is.
	CommitGroup(b *GroupBatch) error

	// FinalizeWindow materializes the window's buckets from the delta log and
	// deletes exactly the delta rows it incorporated, in one transaction.
	FinalizeWindow(windowStart int64) (FinalizeStats, error)

	// FinalizableWindows returns the un-finalized window starts at or below
	// cutoff, oldest first, capped at limit.
	FinalizableWindows(cutoff int64, limit int) ([]int64, error)

	// PurgeBefore deletes finalized history older than cutoff.
	PurgeBefore(cutoff int64) (PurgeStats, error)

	// ReadBuckets returns the store-owned rows matching sel: materialized
	// buckets AND any not-yet-finalized delta rows for the same range. Both
	// are store-owned data once the engine has handed the window over, and
	// omitting either is exactly the silent omission #194 blocker 4 is about.
	//
	// The selector's bounds are mandatory and the row cap is enforced
	// store-side. The read asks the database for limit+1 rows and reports
	// Truncated rather than trimming in silence; a caller that needs every row
	// pages with Selector.After until Truncated is false.
	ReadBuckets(sel Selector) (BucketPage, error)

	// SumBuckets aggregates the same row set as ReadBuckets in SQL, grouped by
	// by. NO row cap applies: the result is bounded by the grouping — windows,
	// services, signals — not by the number of rows scanned, which is what
	// makes a scalar dashboard total structurally impossible to truncate.
	SumBuckets(sel Selector, by GroupBy) ([]SumRow, error)

	// ReplayMutable returns the delta-log rows for windows at or after since —
	// the mutable set only. Finalized history never hydrates into RAM (#160).
	ReplayMutable(since int64) ([]DeltaRow, error)

	// LoadBaselines returns every durable cumulative baseline, capped at max.
	LoadBaselines(max int) ([]BaselineRow, error)

	// ResolveSeries resolves series IDs back to their identity. The input
	// count is capped at MaxReadRows.
	ResolveSeries(ids []SeriesID) ([]SeriesInfo, error)

	// LoadDict returns every dictionary row, capped at max. It is how the
	// durable registrar warms its cache at startup so IDs stay stable.
	LoadDict(max int) ([]DictRow, error)

	// LoadSeries returns every series row, capped at max.
	LoadSeries(max int) ([]SeriesRow, error)

	// Backlog reports the delta-log backlog health bounds.
	Backlog() (BacklogStats, error)

	// Close releases the store's connections.
	Close() error
}

// StoreMetrics is the durable path's metric surface. It mirrors the
// MetricsRecorder pattern: an interface so tests need no live registry, and a
// nil-safe no-op default.
type StoreMetrics interface {
	// RecordCommit publishes one group commit: its wall time, how many
	// deltas it carried and how many bytes it wrote.
	RecordCommit(d time.Duration, deltas int, bytes int64, err error)
	// RecordAdmissionRejected counts one ErrSaturated refusal by bound.
	RecordAdmissionRejected(bound string)
	// RecordFinalize publishes one window finalization.
	RecordFinalize(stats FinalizeStats, err error)
	// RecordPurge publishes one retention purge.
	RecordPurge(stats PurgeStats, err error)
	// SetBacklog publishes the delta-log backlog health bounds.
	SetBacklog(rows int64, ageSeconds float64)
	// RecordRecovery publishes the startup recovery duration and how many
	// delta rows were replayed.
	RecordRecovery(d time.Duration, replayed int, finalized int)
	// RecordGC publishes one identity garbage-collection pass.
	RecordGC(stats GCStats, err error)
}

// noopStoreMetrics is the default when no metrics are wired.
type noopStoreMetrics struct{}

func (noopStoreMetrics) RecordCommit(time.Duration, int, int64, error) {}
func (noopStoreMetrics) RecordAdmissionRejected(string)                {}
func (noopStoreMetrics) RecordFinalize(FinalizeStats, error)           {}
func (noopStoreMetrics) RecordPurge(PurgeStats, error)                 {}
func (noopStoreMetrics) SetBacklog(int64, float64)                     {}
func (noopStoreMetrics) RecordRecovery(time.Duration, int, int)        {}
func (noopStoreMetrics) RecordGC(GCStats, error)                       {}

// MutableSince returns the oldest window start still inside the mutable set at
// now: the current window minus the lateness horizon. Everything strictly older
// is finalized history and never re-enters memory.
func MutableSince(now time.Time) int64 {
	return WindowStart(now) - int64(AllowedLateness/time.Second)
}

// FinalizeCutoff returns the newest window start whose lateness horizon has
// expired at now — windows at or below it are ready to finalize.
func FinalizeCutoff(now time.Time) int64 {
	return now.Unix() - int64(WindowSize/time.Second) - int64(AllowedLateness/time.Second)
}
