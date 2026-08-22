package aggregate

import (
	"path/filepath"
	"testing"
	"time"
)

// Startup restore of the finalized topology horizon (#194 finding 15).
//
// The property under test is narrow and load-bearing: a restart must not erase
// the recent service map, and it must not pay for that by hydrating finalized
// history back into the mutable shards.

// restoreFixture ingests one span plus one caller/callee edge through the
// reducer, so the durable rows carry real identities, and returns the cold
// stack that a restart leaves behind after downtime.
func restoreFixture(t *testing.T, downtime time.Duration) *lifecycleFixture {
	t.Helper()
	f := openLifecycleFixture(t, filepath.Join(t.TempDir(), "aggregate.db"), Bounds{}, nil, nil)
	now := f.clock.Now()

	r := f.eng.NewReducer(now)
	reduceSpanAt(r, "acme", "checkout", "/pay", 0, 5000, now)
	reduceSpanAt(r, "acme", "gateway", "/checkout", 0, 7000, now)
	r.ReduceEdge(EdgeInput{
		Tenant: "acme", Caller: "gateway", Callee: "checkout",
		SpanName: "/pay", Timestamp: now, DurationMicros: 5000,
	})
	if _, err := f.eng.ApplyReducerErr(r); err != nil {
		t.Fatalf("ApplyReducerErr: %v", err)
	}
	if snap := f.eng.TopologySnapshot("acme"); len(snap.Edges) != 1 {
		t.Fatalf("pre-restart snapshot has no edge: %+v", snap)
	}

	f.clock.Advance(downtime)
	cold := f.restart(Bounds{})
	if snap := cold.eng.TopologySnapshot("acme"); !snap.Empty() {
		t.Fatalf("a cold engine already holds topology: %+v", snap)
	}
	return cold
}

func TestTopologyRestoreRebuildsFinalizedHorizon(t *testing.T) {
	// Down long enough for the window's lateness to expire — it is finalized
	// history by the time recovery runs — but well inside a 30-minute horizon.
	cold := restoreFixture(t, WindowSize+AllowedLateness+time.Minute)

	stats, err := Recover(cold.store, cold.eng, cold.writer, cold.clock.Now(),
		RecoverOptions{TopologyHorizon: 30 * time.Minute})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if stats.FinalizedWindows != 1 {
		t.Fatalf("finalized %d windows, want 1", stats.FinalizedWindows)
	}
	if stats.ReplayedRows != 0 {
		t.Fatalf("replayed %d mutable rows, want 0 — the window is history", stats.ReplayedRows)
	}
	if stats.RestoredTopologyRows == 0 || stats.RestoredTopologyWindows == 0 {
		t.Fatalf("nothing restored into the projection: %+v", stats)
	}
	if stats.RestoredTopologyTruncated {
		t.Fatalf("restore reported truncation on three series: %+v", stats)
	}

	snap := cold.eng.TopologySnapshot("acme")
	svc := findService(t, snap, "checkout")
	if count, _ := totalCount(svc.Windows); count != 1 {
		t.Errorf("restored checkout count = %d, want 1", count)
	}
	if len(snap.Edges) != 1 || snap.Edges[0].Caller != "gateway" || snap.Edges[0].Callee != "checkout" {
		t.Fatalf("restored edges = %+v, want one gateway->checkout entry", snap.Edges)
	}
	if edgeCount, _ := totalCount(snap.Edges[0].Windows); edgeCount != 1 {
		t.Errorf("restored edge count = %d, want 1", edgeCount)
	}

	// The whole point of the exception is that it stops at the projection.
	// Finalized history must not have re-entered the mutable shards.
	engSnap := cold.eng.Snapshot()
	if count, _ := engSnap.Totals(SignalTraceOp); count != 0 {
		t.Fatalf("shards hold %d trace points after restore; finalized history must not hydrate", count)
	}
	if count, _ := engSnap.Totals(SignalServiceEdge); count != 0 {
		t.Fatalf("shards hold %d edge points after restore; finalized history must not hydrate", count)
	}
}

func TestTopologyRestoreStopsAtTheHorizon(t *testing.T) {
	// The window is 45 minutes old: outside the projection's own 30-minute
	// horizon, so no configured value may drag it back into memory.
	cold := restoreFixture(t, 45*time.Minute)

	stats, err := Recover(cold.store, cold.eng, cold.writer, cold.clock.Now(),
		RecoverOptions{TopologyHorizon: time.Hour})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if stats.RestoredTopologyRows != 0 || stats.RestoredTopologyWindows != 0 {
		t.Fatalf("restored %+v past the horizon", stats)
	}
	if snap := cold.eng.TopologySnapshot("acme"); !snap.Empty() {
		t.Fatalf("topology restored from beyond the horizon: %+v", snap)
	}
}

func TestTopologyRestoreDisabledByZeroHorizon(t *testing.T) {
	cold := restoreFixture(t, WindowSize+AllowedLateness+time.Minute)

	stats, err := Recover(cold.store, cold.eng, cold.writer, cold.clock.Now(), RecoverOptions{})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if stats.RestoredTopologyRows != 0 || stats.RestoredTopologyWindows != 0 {
		t.Fatalf("zero horizon restored %+v; it must disable the read entirely", stats)
	}
	if snap := cold.eng.TopologySnapshot("acme"); !snap.Empty() {
		t.Fatalf("topology restored with the restore disabled: %+v", snap)
	}
}

// TestTopologyRestoreReportsItsRowCap pins the honesty half of the bound: when
// the cap cuts the horizon the caller is told rather than handed a partial
// service map that looks complete.
func TestTopologyRestoreReportsItsRowCap(t *testing.T) {
	cold := restoreFixture(t, WindowSize+AllowedLateness+time.Minute)

	stats, err := Recover(cold.store, cold.eng, cold.writer, cold.clock.Now(),
		RecoverOptions{TopologyHorizon: 30 * time.Minute, TopologyMaxRows: 1})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !stats.RestoredTopologyTruncated {
		t.Fatalf("a one-row cap over three series reported no truncation: %+v", stats)
	}
	if stats.RestoredTopologyRows != 1 {
		t.Fatalf("restored %d rows under a cap of 1", stats.RestoredTopologyRows)
	}
}
