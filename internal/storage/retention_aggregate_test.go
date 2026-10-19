package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

// Retention's share of the durable aggregate store (#173): an hourly purge on
// the same age cutoff as the relational purge, a conservative ANALYZE on the
// daily maintenance tick, and NEVER a VACUUM.

func TestRetentionScheduler_PurgesAggregateStore(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 0)

	var (
		calls  int
		cutoff time.Time
	)
	r.SetAggregateRetention(func(c time.Time) (int64, error) {
		calls++
		cutoff = c
		return 12, nil
	}, nil)

	before := time.Now().UTC().Add(-7 * 24 * time.Hour)
	r.runPurge(context.Background())
	after := time.Now().UTC().Add(-7 * 24 * time.Hour)

	if calls != 1 {
		t.Fatalf("aggregate purge called %d times, want 1", calls)
	}
	if cutoff.Before(before.Add(-time.Minute)) || cutoff.After(after.Add(time.Minute)) {
		t.Fatalf("aggregate purge cutoff %s is not the retention horizon", cutoff)
	}
}

func TestRetentionScheduler_AggregatePurgeFailureDoesNotStopRelationalPurge(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 0)
	r.SetAggregateRetention(func(time.Time) (int64, error) {
		return 0, errors.New("aggregate store unavailable")
	}, nil)

	// runPurge must complete; a failing aggregate store is logged, not fatal.
	r.runPurge(context.Background())
	if r.SkippedRuns() != 0 {
		t.Fatalf("skipped runs = %d, want 0", r.SkippedRuns())
	}
}

func TestRetentionScheduler_MaintenanceAnalyzesAggregateStore(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 0)
	analyzed := 0
	r.SetAggregateRetention(nil, func() error {
		analyzed++
		return nil
	})

	r.runMaintenance(context.Background())

	if analyzed != 1 {
		t.Fatalf("aggregate ANALYZE ran %d times, want 1", analyzed)
	}
}

func TestRetentionScheduler_NoAggregateHooksIsANoop(t *testing.T) {
	repo := newTestRepo(t)
	r := NewRetentionScheduler(repo, 7, 10_000, 0)
	r.runPurge(context.Background())
	r.runMaintenance(context.Background())
}

func TestRetentionScheduler_CancelledExemplarDoesNotStartAggregatePurge(t *testing.T) {
	repo := newFileTestRepo(t)
	seedTrace(t, repo.db, "old", time.Now().UTC().Add(-8*24*time.Hour), nil)
	r := NewRetentionScheduler(repo, 7, 2, 0)
	r.SetExemplarRetention(7)
	calls := 0
	r.SetAggregateRetention(func(time.Time) (int64, error) { calls++; return 0, nil }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const callback = "test:cancel-exemplar"
	if err := repo.db.Callback().Raw().After("gorm:raw").Register(callback, func(*gorm.DB) { cancel() }); err != nil {
		t.Fatal(err)
	}
	r.runPurge(ctx)
	if err := repo.db.Callback().Raw().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("started %d aggregate purges after cancellation", calls)
	}
	if r.running.Load() {
		t.Fatal("cancelled pass retained overlap guard")
	}
	if n := mustCount(t, repo.db, &Trace{}); n != 1 {
		t.Fatalf("cancelled transaction retained %d traces, want 1", n)
	}
	r.runPurge(context.Background())
	if n := mustCount(t, repo.db, &Trace{}); n != 0 {
		t.Fatalf("recovery pass retained %d traces, want 0", n)
	}
	if calls != 1 {
		t.Fatalf("recovery pass called aggregate purge %d times, want 1", calls)
	}
}
