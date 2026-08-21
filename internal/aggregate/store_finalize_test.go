package aggregate

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The wave-5 acceptance run measured FinalizeWindow holding the writer lock for
// 16.2 s while it drained 1.6M delta rows out of one 5-minute window, parking
// every OTLP emitter for the duration (#173). The cause was an append-only
// delta log: a window's row count grew with the number of group commits that
// touched it, not with the number of series in it. These two tests pin the
// invariant that replaced it and the property it was supposed to buy.

// TestDeltaLogRowsScaleWithSeriesNotCommits is the direct regression: many
// commits into one window must leave one row per series, and the totals must
// survive the merge unchanged.
func TestDeltaLogRowsScaleWithSeriesNotCommits(t *testing.T) {
	const (
		series  = 40
		commits = 250
		window  = 900
	)
	store := newTestStore(t)

	reg := make([]SeriesRow, 0, series)
	for i := 0; i < series; i++ {
		reg = append(reg, SeriesRow{ID: SeriesID(i + 1), Key: storeKey(uint32(i + 1))})
	}
	if err := store.CommitGroup(&GroupBatch{Series: reg}); err != nil {
		t.Fatalf("seed series: %v", err)
	}

	for c := 0; c < commits; c++ {
		batch := &GroupBatch{Deltas: make([]DeltaRow, 0, series)}
		for i := 0; i < series; i++ {
			batch.Deltas = append(batch.Deltas, DeltaRow{
				SeriesID:    SeriesID(i + 1),
				WindowStart: window,
				Delta:       spanDelta(2, float64(100+i)),
			})
		}
		if err := store.CommitGroup(batch); err != nil {
			t.Fatalf("commit %d: %v", c, err)
		}
	}

	// The whole point: 250 commits x 40 series is 10,000 appends and 40 rows.
	assertCount(t, store, "aggregate_delta_log", series)

	stats, err := store.FinalizeWindow(window)
	if err != nil {
		t.Fatalf("FinalizeWindow: %v", err)
	}
	if stats.DeltaRows != series || stats.Buckets != series {
		t.Fatalf("finalize stats = %+v, want %d delta rows / %d buckets", stats, series, series)
	}
	assertCount(t, store, "aggregate_delta_log", 0)

	// Merging is not the same as sampling: every observation has to be in the
	// bucket. spanDelta(2, _) contributes 2 points and 1 error per commit.
	buckets, err := store.ReadBuckets(Selector{TenantID: 1, Start: window, End: window + 300})
	if err != nil {
		t.Fatalf("ReadBuckets: %v", err)
	}
	if len(buckets) != series {
		t.Fatalf("read %d buckets, want %d", len(buckets), series)
	}
	for _, b := range buckets {
		if b.Delta.Count != 2*commits {
			t.Fatalf("series %d count = %d, want %d — the merge lost points", b.SeriesID, b.Delta.Count, 2*commits)
		}
		if b.Delta.ErrorCount != commits {
			t.Fatalf("series %d errors = %d, want %d", b.SeriesID, b.Delta.ErrorCount, commits)
		}
		if b.Delta.Sketch == nil || b.Delta.Sketch.Count() != uint64(2*commits) {
			t.Fatalf("series %d sketch did not survive the merge: %+v", b.SeriesID, b.Delta.Sketch)
		}
	}
}

// TestFinalizeDoesNotStallCommits drives a live commit stream across a
// finalization and bounds how long any single commit was blocked behind it.
// This is the ACK-latency property from #173's acceptance criteria, measured
// against the store rather than against a running server.
func TestFinalizeDoesNotStallCommits(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive")
	}
	const (
		series    = 2000
		commits   = 100
		oldWindow = 1200
		newWindow = 1500
		// Generous next to the 500 ms the ticket asks of a live server: this
		// runs under -race on shared CI. The failure it exists to catch was
		// 16 s, and pre-merge finalization of 2,000 series is milliseconds.
		maxBlocked = 2 * time.Second
	)
	store := newTestStore(t)

	reg := make([]SeriesRow, 0, series)
	for i := 0; i < series; i++ {
		reg = append(reg, SeriesRow{ID: SeriesID(i + 1), Key: storeKey(uint32(i + 1))})
	}
	if err := store.CommitGroup(&GroupBatch{Series: reg}); err != nil {
		t.Fatalf("seed series: %v", err)
	}
	for c := 0; c < commits; c++ {
		batch := &GroupBatch{Deltas: make([]DeltaRow, 0, series)}
		for i := 0; i < series; i++ {
			batch.Deltas = append(batch.Deltas, DeltaRow{
				SeriesID:    SeriesID(i + 1),
				WindowStart: oldWindow,
				Delta:       spanDelta(1, float64(100+i)),
			})
		}
		if err := store.CommitGroup(batch); err != nil {
			t.Fatalf("seed commit %d: %v", c, err)
		}
	}

	// A commit stream shaped like live ingestion: small batches, back to back,
	// into the window that is still mutable.
	var (
		stop      = make(chan struct{})
		wg        sync.WaitGroup
		worst     atomic.Int64
		streamed  atomic.Int64
		commitErr atomic.Value
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			batch := &GroupBatch{Deltas: make([]DeltaRow, 0, 8)}
			for i := 0; i < 8; i++ {
				batch.Deltas = append(batch.Deltas, DeltaRow{
					SeriesID:    SeriesID((n+i)%series + 1),
					WindowStart: newWindow,
					Delta:       spanDelta(1, 250),
				})
			}
			start := time.Now()
			if err := store.CommitGroup(batch); err != nil {
				commitErr.Store(err)
				return
			}
			if d := int64(time.Since(start)); d > worst.Load() {
				worst.Store(d)
			}
			streamed.Add(1)
		}
	}()

	// Let the stream reach steady state so the finalize genuinely collides.
	time.Sleep(200 * time.Millisecond)
	finalizeStart := time.Now()
	stats, err := store.FinalizeWindow(oldWindow)
	finalizeTook := time.Since(finalizeStart)
	close(stop)
	wg.Wait()

	if err != nil {
		t.Fatalf("FinalizeWindow: %v", err)
	}
	if e := commitErr.Load(); e != nil {
		t.Fatalf("concurrent commit failed: %v", e)
	}
	if stats.DeltaRows != series {
		t.Fatalf("finalize drained %d delta rows, want %d — the log is not pre-merged", stats.DeltaRows, series)
	}
	if streamed.Load() == 0 {
		t.Fatal("no commit landed alongside the finalize; the test proves nothing")
	}
	if blocked := time.Duration(worst.Load()); blocked > maxBlocked {
		t.Fatalf("worst commit blocked %v behind a %v finalize of %d series, want <= %v",
			blocked, finalizeTook, series, maxBlocked)
	}
	t.Logf("finalize %v for %d series; %d concurrent commits, worst blocked %v",
		finalizeTook, series, streamed.Load(), time.Duration(worst.Load()))
}
