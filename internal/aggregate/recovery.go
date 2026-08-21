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
// FINALIZED HISTORY NEVER HYDRATES INTO RAM. Only the delta log is read, and
// only for windows still inside the mutable set — the bucket table is a read
// path, not a startup path.
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
	// SkippedSeries counts delta rows whose series id no longer resolves —
	// structurally impossible while registration and first delta share a
	// transaction, so a non-zero value here is a corruption signal.
	SkippedSeries int
	// Duration is the wall time of the whole recovery.
	Duration time.Duration
}

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
func Recover(store Store, engine *Engine, w *Writer, now time.Time) (RecoveryStats, error) {
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

	stats.Duration = time.Since(start)
	return stats, nil
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
		"unresolved_series", stats.SkippedSeries,
		"duration", stats.Duration,
	)
	if stats.SkippedSeries > 0 {
		slog.Error("aggregate recovery: delta rows referenced unknown series — the store may be corrupt",
			"rows", stats.SkippedSeries)
	}
}
