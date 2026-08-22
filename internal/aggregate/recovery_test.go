package aggregate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Environment contract for the kill -9 child process.
const (
	crashChildEnv = "OTELCONTEXT_AGGREGATE_CRASH_CHILD"
	crashPathEnv  = "OTELCONTEXT_AGGREGATE_CRASH_DB"
)

func TestRecoveryReplaysMutableWindowsOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	key := storeKey(1)

	store := newTestStoreAt(t, path, StoreConfig{})
	mutable := WindowStart(clock.Now())
	stale := mutable - 4*int64(WindowSize/time.Second)
	if err := store.CommitGroup(&GroupBatch{
		Series: []SeriesRow{{ID: 1, Key: key}},
		Deltas: []DeltaRow{
			{SeriesID: 1, WindowStart: mutable, Delta: spanDelta(4, 100)},
			{SeriesID: 1, WindowStart: stale, Delta: spanDelta(9, 100)},
		},
	}); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	eng := newTestEngine(t, clock, nil)
	stats, err := Recover(store, eng, nil, clock.Now(), RecoverOptions{})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if stats.FinalizedWindows != 1 {
		t.Fatalf("finalized %d windows, want 1 (the downtime-expired one)", stats.FinalizedWindows)
	}
	if stats.ReplayedRows != 1 {
		t.Fatalf("replayed %d rows, want 1 (only the mutable window)", stats.ReplayedRows)
	}
	snap := eng.Snapshot()
	count, _ := snap.Totals(SignalTraceOp)
	if count != 4 {
		t.Fatalf("engine holds %d points after replay, want 4 — finalized history must not hydrate", count)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Start.Unix() != mutable {
		t.Fatalf("mutable window set = %+v, want only %d", snap.Windows, mutable)
	}
	assertCount(t, store, "aggregate_buckets", 1)
}

func TestRecoverySeedsBaselines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	key := SeriesKey{TenantID: 1, ServiceID: 1, NameID: 5, Signal: SignalMetric}
	store := newTestStoreAt(t, path, StoreConfig{})
	if err := store.CommitGroup(&GroupBatch{
		Series: []SeriesRow{{ID: 1, Key: key}},
		Baselines: []BaselineRow{{
			SeriesID: 1,
			Producer: 42,
			Baseline: Baseline{
				StartTime:     clock.Now().Add(-time.Hour),
				LastTimestamp: clock.Now().Add(-time.Minute),
				Value:         500,
			},
		}},
	}); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	eng := newTestEngine(t, clock, nil)
	stats, err := Recover(store, eng, nil, clock.Now(), RecoverOptions{})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if stats.SeededBaselines != 1 {
		t.Fatalf("seeded %d baselines, want 1", stats.SeededBaselines)
	}
	if eng.Baselines().DirtyCount() != 0 {
		t.Fatal("recovered baselines must not be marked dirty")
	}
	// The next cumulative point converts against the recovered baseline instead
	// of re-seeding — the restart gap #166 exists to close.
	out := eng.Baselines().ObserveCumulative(key, 42, clock.Now().Add(-time.Hour), clock.Now(), 505)
	if out.Seeded {
		t.Fatal("point re-seeded the baseline; recovery did not restore it")
	}
	if out.Delta != 5 {
		t.Fatalf("delta = %v, want 5", out.Delta)
	}
}

func TestRecoveryFinalizesDowntimeExpiredWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	store := newTestStoreAt(t, path, StoreConfig{})
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, store, eng, clock, WriterConfig{})
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 7)); err != nil {
		t.Fatalf("ApplyDeltasErr: %v", err)
	}
	w.Stop()

	// The process was down long enough for the window's lateness to expire.
	clock.Advance(2 * (WindowSize + AllowedLateness))
	eng2 := newTestEngine(t, clock, nil)
	stats, err := Recover(store, eng2, nil, clock.Now(), RecoverOptions{})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if stats.FinalizedWindows != 1 || stats.ReplayedRows != 0 {
		t.Fatalf("recovery stats = %+v, want 1 finalized / 0 replayed", stats)
	}
	page, err := store.ReadBuckets(Selector{
		TenantID: 1,
		Start:    WindowStart(clock.Now()) - int64(4*(WindowSize+AllowedLateness)/time.Second),
		End:      WindowStart(clock.Now()) + 1,
	})
	if err != nil {
		t.Fatalf("ReadBuckets: %v", err)
	}
	if page.Truncated {
		t.Fatalf("read reported truncation at limit %d for a single-bucket store", page.Limit)
	}
	if len(page.Buckets) != 1 || page.Buckets[0].Delta.Count != 7 {
		t.Fatalf("finalized buckets = %+v, want one bucket with count 7", page.Buckets)
	}
	if count, _ := eng2.Snapshot().Totals(SignalTraceOp); count != 0 {
		t.Fatalf("engine hydrated %d points of finalized history, want 0", count)
	}
}

// TestCrashAfterACKSurvivesKill9 is the release-gate durability test: a child
// process acknowledges an Export and is then SIGKILLed with no chance to close
// the database. The acknowledged deltas must be present after reopening, and
// deltas that were never acknowledged must not be.
func TestCrashAfterACKSurvivesKill9(t *testing.T) {
	if os.Getenv(crashChildEnv) == "1" {
		crashChild(t)
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("kill -9 crash test requires linux")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "aggregate.db")

	cmd := exec.Command(os.Args[0], "-test.run=TestCrashAfterACKSurvivesKill9", "-test.v")
	cmd.Env = append(os.Environ(), crashChildEnv+"=1", crashPathEnv+"="+path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child exited cleanly; it was supposed to be killed. output:\n%s", out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("child failed with %v, want a signal death. output:\n%s", err, out)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("child exit status = %v (signaled=%v), want SIGKILL. output:\n%s", exitErr, ok, out)
	}
	if !strings.Contains(string(out), "ACKED") {
		t.Fatalf("child was killed before the ACK; the test proves nothing. output:\n%s", out)
	}

	clock := newClock(time.Unix(3_000_000, 0).UTC())
	store := newTestStoreAt(t, path, StoreConfig{})
	eng := newTestEngine(t, clock, nil)
	stats, err := Recover(store, eng, nil, clock.Now(), RecoverOptions{})
	if err != nil {
		t.Fatalf("Recover after kill -9: %v", err)
	}
	if stats.ReplayedRows == 0 {
		t.Fatalf("no acknowledged deltas survived the kill. child output:\n%s", out)
	}
	count, _ := eng.Snapshot().Totals(SignalTraceOp)
	if count != 11 {
		t.Fatalf("recovered %d acknowledged points, want 11. child output:\n%s", count, out)
	}
}

// crashChild is the child half of TestCrashAfterACKSurvivesKill9: acknowledge
// an Export, then die without closing anything.
func crashChild(t *testing.T) {
	path := os.Getenv(crashPathEnv)
	if path == "" {
		t.Fatalf("%s is unset", crashPathEnv)
	}
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	store, err := OpenSQLiteStore(StoreConfig{Path: path})
	if err != nil {
		t.Fatalf("child: OpenSQLiteStore: %v", err)
	}
	eng, err := NewEngine(EngineConfig{Mode: ModeAggregate, Now: clock.Now})
	if err != nil {
		t.Fatalf("child: NewEngine: %v", err)
	}
	w, err := NewWriter(WriterConfig{
		Store: store, Engine: eng, Now: clock.Now, FinalizeInterval: -1,
	})
	if err != nil {
		t.Fatalf("child: NewWriter: %v", err)
	}
	eng.SetApplier(w)
	w.Start()

	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 11)); err != nil {
		t.Fatalf("child: ApplyDeltasErr: %v", err)
	}
	// Printed only after the durable ACK returned. The parent refuses to draw
	// any conclusion without it.
	t.Log("ACKED")
	os.Stdout.Sync()

	// No Stop(), no Close(), no checkpoint: exactly what a container OOM-kill
	// or kill -9 does.
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatalf("child: kill self: %v", err)
	}
	select {} // unreachable; keeps the signal from racing the return
}

// asExitError is errors.As specialised for *exec.ExitError without pulling the
// generic helper into this file's imports twice.
func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// TestReopenWithoutCheckpointKeepsAcknowledgedDeltas is the portable sibling of
// the kill -9 test: it closes the pools without a WAL checkpoint and reopens.
// It exercises WAL recovery but NOT process death, so it is a weaker claim —
// the kill -9 test above is the one that proves the ACK contract.
func TestReopenWithoutCheckpointKeepsAcknowledgedDeltas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	store, err := OpenSQLiteStore(StoreConfig{Path: path})
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, store, eng, clock, WriterConfig{})
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 6)); err != nil {
		t.Fatalf("ApplyDeltasErr: %v", err)
	}
	w.Stop()
	_ = store.Close()

	reopened := newTestStoreAt(t, path, StoreConfig{})
	eng2 := newTestEngine(t, clock, nil)
	if _, err := Recover(reopened, eng2, nil, clock.Now(), RecoverOptions{}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if count, _ := eng2.Snapshot().Totals(SignalTraceOp); count != 6 {
		t.Fatalf("recovered %d points, want 6", count)
	}
}

func TestRecoveryGateHoldsReadiness(t *testing.T) {
	gate := NewRecoveryGate()
	if gate.Done() {
		t.Fatal("a fresh gate must report not-ready")
	}
	gate.Complete()
	if !gate.Done() {
		t.Fatal("gate did not open")
	}
	var nilGate *RecoveryGate
	if !nilGate.Done() {
		t.Fatal("a nil gate must be transparent for configurations without a store")
	}
}
