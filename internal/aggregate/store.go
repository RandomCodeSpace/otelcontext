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
const StoreSchemaVersion = 2

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
}

// Empty reports whether the batch would commit nothing.
func (b *GroupBatch) Empty() bool {
	return len(b.Dicts) == 0 && len(b.Series) == 0 && len(b.Deltas) == 0 && len(b.Baselines) == 0
}

// SeriesInfo is a series' durable identity, resolved back from its ID.
type SeriesInfo struct {
	ID  SeriesID
	Key SeriesKey
}

// Bucket is one finalized (window, series) row.
type Bucket struct {
	WindowStart int64
	SeriesID    SeriesID
	Delta       *AggregateDelta
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
}

// MaxReadRows is the store-side cap on rows returned by one ReadBuckets call
// and on IDs accepted by one ResolveSeries call.
const MaxReadRows = 20000

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

	// ReadBuckets returns finalized buckets matching sel. The selector's
	// bounds are mandatory and the row cap is enforced store-side.
	ReadBuckets(sel Selector) ([]Bucket, error)

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
}

// noopStoreMetrics is the default when no metrics are wired.
type noopStoreMetrics struct{}

func (noopStoreMetrics) RecordCommit(time.Duration, int, int64, error) {}
func (noopStoreMetrics) RecordAdmissionRejected(string)                {}
func (noopStoreMetrics) RecordFinalize(FinalizeStats, error)           {}
func (noopStoreMetrics) RecordPurge(PurgeStats, error)                 {}
func (noopStoreMetrics) SetBacklog(int64, float64)                     {}
func (noopStoreMetrics) RecordRecovery(time.Duration, int, int)        {}

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
