package storage

import (
	"context"
	"testing"
	"time"
)

// TestRunPurge_RunsTablesInParallel_SQLiteSerialFallback exercises runPurge
// across logs + traces + metric_buckets in a single call. On SQLite the
// parallelization path is intentionally a no-op (runPurgeSerial), but the
// observable behaviour — all three tables drained past cutoff — must hold.
func TestRunPurge_RunsTablesInParallel_SQLiteSerialFallback(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)

	seedLogs(t, repo.db, 200, old, "svc")
	seedTrace(t, repo.db, "old-trace", old, []time.Time{old})

	// Seed a couple of old metric buckets so the metric_buckets branch isn't
	// exercising a no-op. time_bucket < cutoff -> eligible for purge.
	buckets := []MetricBucket{
		{Name: "m1", ServiceName: "svc", TimeBucket: old, Count: 1},
		{Name: "m2", ServiceName: "svc", TimeBucket: old, Count: 2},
	}
	if err := repo.db.Create(&buckets).Error; err != nil {
		t.Fatalf("seed metric buckets: %v", err)
	}

	sched := NewRetentionScheduler(repo, 1, 100, 0) // batch 100, sleep 0
	sched.runPurge(context.Background())

	if c := mustCount(t, repo.db, &Log{}); c != 0 {
		t.Fatalf("logs not purged: %d rows remain", c)
	}
	if c := mustCount(t, repo.db, &Trace{}); c != 0 {
		t.Fatalf("traces not purged: %d rows remain", c)
	}
	if c := mustCount(t, repo.db, &MetricBucket{}); c != 0 {
		t.Fatalf("metric_buckets not purged: %d rows remain", c)
	}
}
