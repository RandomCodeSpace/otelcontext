package aggregate

import (
	"path/filepath"
	"testing"
	"time"
)

// benchSeries is the dirty-series count #162's benchmark gate names.
const benchSeries = 5000

// benchStore opens a store outside the testing.T helpers.
func benchStore(b *testing.B) *SQLiteStore {
	b.Helper()
	store, err := OpenSQLiteStore(StoreConfig{Path: filepath.Join(b.TempDir(), "aggregate.db")})
	if err != nil {
		b.Fatalf("OpenSQLiteStore: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

// benchBatch builds one group commit touching n series in one window.
func benchBatch(n int, window int64, withSeries bool) *GroupBatch {
	batch := &GroupBatch{Deltas: make([]DeltaRow, 0, n)}
	for i := 1; i <= n; i++ {
		if withSeries {
			batch.Series = append(batch.Series, SeriesRow{ID: SeriesID(i), Key: storeKey(uint32(i))})
		}
		batch.Deltas = append(batch.Deltas, DeltaRow{
			SeriesID:    SeriesID(i),
			WindowStart: window,
			Delta:       spanDelta(4, float64(100+i%700)),
		})
	}
	return batch
}

// BenchmarkCommitGroup5kSeries measures the group-commit shape the release gate
// cares about: one transaction, 5,000 pre-merged delta rows with sketches.
func BenchmarkCommitGroup5kSeries(b *testing.B) {
	store := benchStore(b)
	if err := store.CommitGroup(&GroupBatch{Series: benchBatch(benchSeries, 0, true).Series}); err != nil {
		b.Fatalf("seed series: %v", err)
	}
	window := WindowStart(time.Unix(3_000_000, 0).UTC())
	batch := benchBatch(benchSeries, window, false)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := store.CommitGroup(batch); err != nil {
			b.Fatalf("CommitGroup: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(benchSeries), "deltas/commit")
}

// BenchmarkFinalizeWindow measures materializing a full window: 5,000 series x
// 4 commits of delta rows collapsed into buckets and deleted.
func BenchmarkFinalizeWindow(b *testing.B) {
	window := WindowStart(time.Unix(3_000_000, 0).UTC())
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		store := benchStore(b)
		if err := store.CommitGroup(&GroupBatch{Series: benchBatch(benchSeries, 0, true).Series}); err != nil {
			b.Fatalf("seed series: %v", err)
		}
		batch := benchBatch(benchSeries, window, false)
		for c := 0; c < 4; c++ {
			if err := store.CommitGroup(batch); err != nil {
				b.Fatalf("CommitGroup: %v", err)
			}
		}
		b.StartTimer()
		stats, err := store.FinalizeWindow(window)
		if err != nil {
			b.Fatalf("FinalizeWindow: %v", err)
		}
		b.StopTimer()
		if stats.Buckets != benchSeries {
			b.Fatalf("finalized %d buckets, want %d", stats.Buckets, benchSeries)
		}
		_ = store.Close()
		b.StartTimer()
	}
}

// BenchmarkWriterApply measures the end-to-end durable ACK path: reduce ->
// admit -> group commit -> shard apply -> release.
func BenchmarkWriterApply(b *testing.B) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	store := benchStore(b)
	eng, err := NewEngine(EngineConfig{Mode: ModeAggregate, Now: clock.Now})
	if err != nil {
		b.Fatalf("NewEngine: %v", err)
	}
	w, err := NewWriter(WriterConfig{Store: store, Engine: eng, Now: clock.Now, FinalizeInterval: -1})
	if err != nil {
		b.Fatalf("NewWriter: %v", err)
	}
	eng.SetApplier(w)
	w.Start()
	defer w.Stop()

	window := WindowStart(clock.Now())
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		n := uint32(0)
		for pb.Next() {
			n++
			m := DeltaMap{
				SeriesWindowKey{Key: storeKey(n % 64), WindowStart: window}: spanDelta(4, 250),
			}
			if _, err := eng.ApplyDeltasErr(m); err != nil {
				b.Fatalf("ApplyDeltasErr: %v", err)
			}
		}
	})
}
