package aggregate

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// Startup recovery (#160, #173).
//
// Three things happen, in this order, before the process reports ready:
//
//  1. Windows whose lateness horizon expired while the process was down are
//     finalized transactionally from the delta log. They are history now; they
//     must not come back as mutable state.
//  2. The delta-log rows of still-mutable windows are replayed into the shards,
//     so acknowledged-but-unfinalized aggregates survive the crash.
//  3. Durable cumulative baselines are seeded into the tracker, so the first
//     cumulative point after a restart converts instead of re-seeding.
//
// FINALIZED HISTORY NEVER HYDRATES INTO THE SHARDS. Only the delta log is
// replayed, and only for windows still inside the mutable set.
//
// There is exactly ONE bounded exception, and it stops short of the shards:
// step 4 rebuilds the engine's TOPOLOGY PROJECTION over a configured horizon
// (#194 finding 15). Without it a restart erases the recent service map —
// every node, edge and metric baseline — until enough new telemetry arrives to
// re-derive one, while the numbers those entities describe are sitting in the
// bucket table. The exception is bounded three ways: a horizon, a row cap, and
// the projection's own retention cutoff. It writes to the projection only, so
// no finalized window can re-enter the mutable set through it.
//
// Readiness is held false until this completes. A process that answers /ready
// while its shards are half-populated would serve numbers that are wrong in a
// way no dashboard can detect.

// RecoveryStats is the outcome of one startup recovery.
type RecoveryStats struct {
	// FinalizedWindows counts windows finalized because their lateness
	// horizon expired during downtime.
	FinalizedWindows int
	// ReplayedRows and ReplayedSeries count delta-log rows read back and the
	// distinct (series, window) pairs they folded into.
	ReplayedRows   int
	ReplayedSeries int
	// SeededBaselines counts cumulative baselines restored.
	SeededBaselines int
	// RestoredTopologyRows counts the durable rows the topology restore read:
	// finalized bucket rows inside the horizon plus the replayed mutable rows,
	// which are in the shards but not in the projection.
	RestoredTopologyRows int
	// RestoredTopologyWindows counts the (series, window) pairs that actually
	// LANDED in the projection. It is lower than RestoredTopologyRows whenever
	// the projection's cutoff or one of its caps refused a row.
	RestoredTopologyWindows int
	// RestoredTopologyTruncated reports that the row cap cut the horizon
	// short. The restored topology is real but incomplete at its oldest end.
	RestoredTopologyTruncated bool
	// SkippedSeries counts delta rows whose series id no longer resolves —
	// structurally impossible while registration and first delta share a
	// transaction, so a non-zero value here is a corruption signal.
	SkippedSeries int
	// Duration is the wall time of the whole recovery.
	Duration time.Duration
}

// RecoverOptions configures the parts of recovery that are policy rather than
// correctness. The zero value is the pre-#194 behaviour: replay only.
type RecoverOptions struct {
	// TopologyHorizon is how much FINALIZED history is rebuilt into the
	// engine's topology projection. Zero disables the restore; anything past
	// the projection's own horizon is clamped down to it, because a window the
	// projection would prune on arrival is not worth reading.
	TopologyHorizon time.Duration
	// TopologyMaxRows caps the finalized rows one restore may read. Zero takes
	// DefaultTopologyRestoreMaxRows. It is the second bound on startup cost:
	// the horizon says how far back, this says how much.
	TopologyMaxRows int
}

// DefaultTopologyRestoreMaxRows caps the finalized rows a topology restore
// reads. At the default series budget a 30-minute horizon is six windows of at
// most a few thousand series, so the cap is headroom rather than a routine
// truncation — and when it does bind, it is reported, not hidden.
const DefaultTopologyRestoreMaxRows = 20000

// topologyRestoreSignals are the durable signals the projection is built from.
// Log series are absent on purpose: the projection holds no log entities, so
// reading them would be startup cost with no restored fact to show for it.
var topologyRestoreSignals = []Signal{SignalTraceOp, SignalServiceEdge, SignalMetric}

// RecoveryGate reports whether startup recovery has completed. The readiness
// probe holds /ready at 503 until Done() is true.
type RecoveryGate struct {
	done atomic.Bool
}

// NewRecoveryGate returns a gate that starts closed.
func NewRecoveryGate() *RecoveryGate { return &RecoveryGate{} }

// Done reports whether recovery has completed.
func (g *RecoveryGate) Done() bool {
	if g == nil {
		return true
	}
	return g.done.Load()
}

// Complete opens the gate.
func (g *RecoveryGate) Complete() {
	if g != nil {
		g.done.Store(true)
	}
}

// Recover replays the durable store into the engine. It must run before the
// writer starts accepting Exports and before readiness flips.
func Recover(store Store, engine *Engine, w *Writer, now time.Time, opts RecoverOptions) (RecoveryStats, error) {
	start := time.Now()
	var stats RecoveryStats
	if store == nil || engine == nil {
		return stats, nil
	}

	// 1. Windows whose lateness expired during downtime finalize now, so the
	//    replay below cannot resurrect them as mutable state.
	finalized, err := finalizeExpired(store, now)
	if err != nil {
		return stats, err
	}
	stats.FinalizedWindows = finalized

	// 2. Replay the mutable delta log into the shards.
	rows, err := store.ReplayMutable(MutableSince(now))
	if err != nil {
		return stats, err
	}
	stats.ReplayedRows = len(rows)
	if len(rows) > 0 {
		replayed, skipped, err := replayRows(store, engine, w, rows)
		if err != nil {
			return stats, err
		}
		stats.ReplayedSeries = replayed
		stats.SkippedSeries = skipped
	}

	// 3. Seed the cumulative baselines.
	seeded, err := seedBaselines(store, engine, w)
	if err != nil {
		return stats, err
	}
	stats.SeededBaselines = seeded

	// 4. Rebuild the topology projection over the configured finalized
	//    horizon. It runs LAST because it reads what step 1 finalized, and it
	//    touches no shard.
	if err := restoreTopology(store, engine, w, now, rows, opts, &stats); err != nil {
		return stats, err
	}

	stats.Duration = time.Since(start)
	return stats, nil
}

// restoreTopology rebuilds the engine's topology projection from durable rows.
//
// Two sources, one fold. The finalized buckets inside the horizon are the part
// that #194 finding 15 is about; the mutable delta rows step 2 replayed are
// added because they are back in the SHARDS but not in the PROJECTION, and a
// topology that stopped at the finalized watermark would show a hole between
// the horizon and now.
//
// Nothing here writes to a shard, and the projection's retention cutoff still
// applies — which is why the count reported is what the fold accepted, not
// what the store returned.
func restoreTopology(
	store Store,
	engine *Engine,
	w *Writer,
	now time.Time,
	mutable []DeltaRow,
	opts RecoverOptions,
	stats *RecoveryStats,
) error {
	horizon := opts.TopologyHorizon
	if horizon <= 0 {
		return nil
	}
	if projected := engine.TopologyHorizon(); horizon > projected {
		horizon = projected
	}
	limit := opts.TopologyMaxRows
	if limit <= 0 {
		limit = DefaultTopologyRestoreMaxRows
	}
	page, err := store.ReadFinalizedSince(WindowStart(now.Add(-horizon)), topologyRestoreSignals, limit)
	if err != nil {
		return fmt.Errorf("aggregate recovery: read finalized topology: %w", err)
	}
	rows := make([]DeltaRow, 0, len(page.Buckets)+len(mutable))
	for _, b := range page.Buckets {
		if b.Delta == nil {
			continue
		}
		rows = append(rows, DeltaRow{SeriesID: b.SeriesID, WindowStart: b.WindowStart, Delta: b.Delta})
	}
	rows = append(rows, mutable...)
	stats.RestoredTopologyRows = len(rows)
	stats.RestoredTopologyTruncated = page.Truncated
	if len(rows) == 0 {
		return nil
	}

	keys, err := seriesKeys(store, w, distinctSeriesIDs(rows))
	if err != nil {
		return err
	}
	ids := make(map[SeriesWindowKey]topoIdentity, len(rows))
	deltas := make(DeltaMap, len(rows))
	for _, row := range rows {
		key, ok := keys[row.SeriesID]
		if !ok {
			continue
		}
		id, ok := engine.topoIdentityFor(key)
		if !ok {
			continue
		}
		swk := SeriesWindowKey{Key: key, WindowStart: row.WindowStart}
		if cur, ok := deltas[swk]; ok {
			// Structurally unreachable — both sources are unique per
			// (series, window) and cover disjoint windows — but merging into
			// a FRESH delta rather than in place is what keeps it harmless if
			// it ever happens: the mutable deltas are the same pointers the
			// shards already hold.
			merged := &AggregateDelta{}
			merged.Merge(cur)
			merged.Merge(row.Delta)
			deltas[swk] = merged
			continue
		}
		deltas[swk] = row.Delta
		ids[swk] = id
	}
	stats.RestoredTopologyWindows = engine.RestoreTopology(ids, deltas)
	return nil
}

// finalizeExpired finalizes every window whose lateness horizon expired.
func finalizeExpired(store Store, now time.Time) (int, error) {
	done := 0
	for {
		windows, err := store.FinalizableWindows(FinalizeCutoff(now), finalizeWindowsPerPass)
		if err != nil {
			return done, fmt.Errorf("aggregate recovery: list expired windows: %w", err)
		}
		if len(windows) == 0 {
			return done, nil
		}
		for _, window := range windows {
			if _, err := store.FinalizeWindow(window); err != nil {
				return done, fmt.Errorf("aggregate recovery: finalize window %d: %w", window, err)
			}
			done++
		}
		if len(windows) < finalizeWindowsPerPass {
			return done, nil
		}
	}
}

// replayRows folds delta-log rows back into the shards. Rows are merged per
// (series, window) first so the engine sees one apply, not one per commit that
// ever touched the window.
func replayRows(store Store, engine *Engine, w *Writer, rows []DeltaRow) (replayed, skipped int, err error) {
	keys, err := seriesKeys(store, w, distinctSeriesIDs(rows))
	if err != nil {
		return 0, 0, err
	}
	m := make(DeltaMap, len(rows))
	for _, row := range rows {
		key, ok := keys[row.SeriesID]
		if !ok {
			skipped++
			continue
		}
		swk := SeriesWindowKey{Key: key, WindowStart: row.WindowStart}
		if cur, ok := m[swk]; ok {
			cur.Merge(row.Delta)
			continue
		}
		m[swk] = row.Delta
	}
	if len(m) > 0 {
		engine.ApplyCommitted(m)
	}
	return len(m), skipped, nil
}

// seedBaselines restores the durable cumulative baselines.
func seedBaselines(store Store, engine *Engine, w *Writer) (int, error) {
	rows, err := store.LoadBaselines(0)
	if err != nil {
		return 0, fmt.Errorf("aggregate recovery: load baselines: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ids := make([]SeriesID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.SeriesID)
	}
	keys, err := seriesKeys(store, w, ids)
	if err != nil {
		return 0, err
	}
	seeded := 0
	for _, r := range rows {
		key, ok := keys[r.SeriesID]
		if !ok {
			continue
		}
		engine.Baselines().Seed(key, r.Producer, r.Baseline)
		seeded++
	}
	return seeded, nil
}

// distinctSeriesIDs collects the series ids referenced by rows.
func distinctSeriesIDs(rows []DeltaRow) []SeriesID {
	seen := make(map[SeriesID]struct{}, len(rows))
	out := make([]SeriesID, 0, len(rows))
	for _, r := range rows {
		if _, ok := seen[r.SeriesID]; ok {
			continue
		}
		seen[r.SeriesID] = struct{}{}
		out = append(out, r.SeriesID)
	}
	return out
}

// seriesKeys resolves ids to identities, preferring the writer's warmed
// in-memory map and falling back to the store in chunks that respect the
// ResolveSeries input cap.
func seriesKeys(store Store, w *Writer, ids []SeriesID) (map[SeriesID]SeriesKey, error) {
	out := make(map[SeriesID]SeriesKey, len(ids))
	var missing []SeriesID
	for _, id := range ids {
		if w != nil {
			if key, ok := w.SeriesKeyByID(id); ok {
				out[id] = key
				continue
			}
		}
		missing = append(missing, id)
	}
	for start := 0; start < len(missing); start += MaxReadRows {
		end := start + MaxReadRows
		if end > len(missing) {
			end = len(missing)
		}
		infos, err := store.ResolveSeries(missing[start:end])
		if err != nil {
			return nil, fmt.Errorf("aggregate recovery: resolve series: %w", err)
		}
		for _, info := range infos {
			out[info.ID] = info.Key
		}
	}
	return out, nil
}

// LogRecovery emits the operator-facing recovery summary.
func LogRecovery(stats RecoveryStats, path string) {
	slog.Info("🔁 Aggregate store recovered",
		"path", path,
		"finalized_windows", stats.FinalizedWindows,
		"replayed_rows", stats.ReplayedRows,
		"replayed_series_windows", stats.ReplayedSeries,
		"seeded_baselines", stats.SeededBaselines,
		"restored_topology_rows", stats.RestoredTopologyRows,
		"restored_topology_windows", stats.RestoredTopologyWindows,
		"restored_topology_truncated", stats.RestoredTopologyTruncated,
		"unresolved_series", stats.SkippedSeries,
		"duration", stats.Duration,
	)
	if stats.RestoredTopologyTruncated {
		slog.Warn("aggregate recovery: topology restore hit its row cap — the oldest part of the horizon was not restored",
			"restored_rows", stats.RestoredTopologyRows,
		)
	}
	if stats.SkippedSeries > 0 {
		slog.Error("aggregate recovery: delta rows referenced unknown series — the store may be corrupt",
			"rows", stats.SkippedSeries)
	}
}
