package storage

import (
	"context"
	"errors"
	"testing"
	"time"
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
