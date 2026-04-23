package storage

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRetentionScheduler_StopBeforeStart_NoDeadlock(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)
	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop() without Start() deadlocked")
	}
}

func TestRetentionScheduler_DoubleStop(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)
	r.Start(context.Background())
	r.Stop()
	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second Stop() deadlocked")
	}
}

func TestRetentionScheduler_DoubleStart_Idempotent(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)
	r.Start(context.Background())
	r.Start(context.Background())
	r.Stop()
	if !r.started.Load() {
		t.Fatal("started flag should remain true after Stop")
	}
}

func TestRetentionScheduler_InitialPurgeRunsImmediately(t *testing.T) {
	repo := newTestRepo(t)

	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	seedLogs(t, repo.db, 100, old, "old-service")

	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)
	r.purgeInterval = time.Hour
	r.vacuumInterval = time.Hour
	r.Start(context.Background())
	defer r.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mustCount(t, repo.db, &Log{}) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("initial purge did not delete old logs in time; remaining=%d", mustCount(t, repo.db, &Log{}))
}

func TestRetentionScheduler_ContextCancellationStopsLoop(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	cancel()

	select {
	case <-r.done:
	case <-time.After(1 * time.Second):
		t.Fatal("loop did not exit after parent ctx cancel")
	}
	r.Stop()
}

func TestRetentionScheduler_MaintenanceVacuumsSQLite(t *testing.T) {
	repo := newTestRepo(t)
	seedLogs(t, repo.db, 1000, time.Now().UTC(), "svc")

	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)
	r.runMaintenance(context.Background())
	// If runMaintenance crashes on "cannot run inside a transaction", this test fails.
	if mustCount(t, repo.db, &Log{}) != 1000 {
		t.Fatal("VACUUM must not delete data")
	}
}

func TestRetentionScheduler_NoDataNoError(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)
	// Must not panic/error against empty tables.
	r.runPurge(context.Background())
	r.runMaintenance(context.Background())
}

func TestRetentionScheduler_ConcurrentStartStop(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)

	// Race detector target: 20 goroutines hammering Start/Stop simultaneously.
	// atomic.Bool CAS means at most one goroutine transitions the flag, and
	// Stop is safe to call even if Start loses the CAS race.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				r.Start(context.Background())
			} else {
				r.Stop()
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Start/Stop deadlocked")
	}
	// Terminal Stop to leave scheduler quiesced for the next test.
	r.Stop()
}

// TestRetentionScheduler_OverlapGuard verifies that if two runPurge calls
// execute concurrently, only one performs the purge and the other returns
// immediately (incrementing skippedRuns). We coordinate the races by holding
// the SQLite write lock on the first caller via a gating insert in a
// transaction — cheap and deterministic without relying on timing.
func TestRetentionScheduler_OverlapGuard(t *testing.T) {
	repo := newTestRepo(t)

	// Seed enough rows that a purge does measurable work. Not strictly required
	// for correctness of the guard — running atomic.Bool flips before any SQL —
	// but this exercises the intended "second tick fires while first still
	// running" path.
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	seedLogs(t, repo.db, 500, old, "svc")

	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)

	// Pin the "running" flag as if a purge were already executing. Because
	// CompareAndSwap(false, true) fails, the concurrent call must skip.
	if !r.running.CompareAndSwap(false, true) {
		t.Fatal("running flag should start false")
	}

	beforeSkipped := r.SkippedRuns()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runPurge(context.Background()) // must observe running=true and skip
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runPurge did not return (overlap guard not honored)")
	}

	if got := r.SkippedRuns(); got != beforeSkipped+1 {
		t.Fatalf("expected skippedRuns to increment by 1; got before=%d after=%d", beforeSkipped, got)
	}

	// Simulate the first purge completing.
	r.running.Store(false)

	// Now a fresh call must run to completion and actually delete the old rows.
	r.runPurge(context.Background())
	if n := mustCount(t, repo.db, &Log{}); n != 0 {
		t.Fatalf("expected purge to delete all old logs, remaining=%d", n)
	}
}

// TestRetentionScheduler_MaintenanceRespectsOverlapGuard checks the same
// guard covers runMaintenance — a long VACUUM must not race with a hourly
// purge tick.
func TestRetentionScheduler_MaintenanceRespectsOverlapGuard(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)

	r.running.Store(true) // pretend purge is in flight
	before := r.SkippedRuns()
	r.runMaintenance(context.Background())
	if got := r.SkippedRuns(); got != before+1 {
		t.Fatalf("runMaintenance should have skipped; before=%d after=%d", before, got)
	}
	r.running.Store(false)
}

func TestRetentionScheduler_ConcurrentStopCallers(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)
	r.Start(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Stop()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("concurrent Stop() callers deadlocked")
	}
}
