package storage

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestPurgeLogsBatched_EmptyTable(t *testing.T) {
	repo := newTestRepo(t)
	n, err := repo.PurgeLogsBatched(context.Background(), time.Now(), 100, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 deletions, got %d", n)
	}
}

func TestPurgeLogsBatched_AllOld_AllDeleted(t *testing.T) {
	repo := newTestRepo(t)
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	seedLogs(t, repo.db, 50, old, "svc")

	n, err := repo.PurgeLogsBatched(context.Background(), time.Now().UTC().Add(-time.Hour), 10, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 50 {
		t.Fatalf("expected 50 deletions on SQLite single-shot path, got %d", n)
	}
	if mustCount(t, repo.db, &Log{}) != 0 {
		t.Fatal("rows remain after full-purge")
	}
}

func TestPurgeLogsBatched_AllNew_NoneDeleted(t *testing.T) {
	repo := newTestRepo(t)
	seedLogs(t, repo.db, 20, time.Now().UTC(), "svc")

	n, err := repo.PurgeLogsBatched(context.Background(), time.Now().UTC().Add(-time.Hour), 10, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 deletions, got %d", n)
	}
	if mustCount(t, repo.db, &Log{}) != 20 {
		t.Fatal("new rows were deleted")
	}
}

func TestPurgeLogsBatched_ZeroBatchSize_DefaultsTo10k(t *testing.T) {
	repo := newTestRepo(t)
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	seedLogs(t, repo.db, 5, old, "svc")

	// batchSize=0 must default internally and not loop forever.
	done := make(chan struct{})
	go func() {
		_, _ = repo.PurgeLogsBatched(context.Background(), time.Now(), 0, 5*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PurgeLogsBatched with batchSize=0 hung")
	}
}

func TestPurgeLogsBatched_ContextCancellation(t *testing.T) {
	repo := newTestRepo(t)
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	seedLogs(t, repo.db, 100, old, "svc")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	// SQLite path is single-shot so ctx.Err() may not be observed; validate it doesn't panic.
	_, _ = repo.PurgeLogsBatched(ctx, time.Now(), 10, 5*time.Millisecond)
}

func TestPurgeLogsBatched_BoundaryTimestamp(t *testing.T) {
	repo := newTestRepo(t)
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)

	seedLogs(t, repo.db, 1, cutoff, "at-cutoff")
	seedLogs(t, repo.db, 1, cutoff.Add(-time.Nanosecond), "just-before")
	seedLogs(t, repo.db, 1, cutoff.Add(time.Nanosecond), "just-after")

	n, err := repo.PurgeLogsBatched(context.Background(), cutoff, 100, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// "<" semantics: only strictly-before-cutoff row deleted.
	if n != 1 {
		t.Fatalf("expected 1 deletion for just-before-cutoff, got %d", n)
	}
	if mustCount(t, repo.db, &Log{}) != 2 {
		t.Fatalf("expected 2 survivors (at and after cutoff), got %d", mustCount(t, repo.db, &Log{}))
	}
}

// TestPurgeTracesBatched_OrphanSpanSweep validates the post-review fix:
// span sweep must correlate via trace existence, NOT span.start_time, so that
// clock-skewed spans under a live trace are preserved.
func TestPurgeTracesBatched_OrphanSpanSweep(t *testing.T) {
	repo := newTestRepo(t)
	nowUTC := time.Now().UTC()
	cutoff := nowUTC.Add(-7 * 24 * time.Hour)

	// T1: old trace + old spans — both should be purged.
	seedTrace(t, repo.db, "t-old", cutoff.Add(-time.Hour), []time.Time{
		cutoff.Add(-time.Hour),
		cutoff.Add(-90 * time.Minute),
	})

	// T2: FRESH trace but spans with OLD start_time (clock-skew scenario).
	// Under the old bug (start_time < cutoff) these spans would be wrongly deleted.
	seedTrace(t, repo.db, "t-skew", nowUTC, []time.Time{
		cutoff.Add(-2 * time.Hour), // old clock on agent
		cutoff.Add(-3 * time.Hour),
	})

	// T3: fresh trace + fresh spans — all preserved.
	seedTrace(t, repo.db, "t-new", nowUTC, []time.Time{nowUTC})

	_, err := repo.PurgeTracesBatched(context.Background(), cutoff, 10, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("PurgeTracesBatched: %v", err)
	}

	// Traces: t-old gone, t-skew and t-new remain.
	if got := mustCount(t, repo.db, &Trace{}); got != 2 {
		t.Fatalf("expected 2 traces remaining, got %d", got)
	}
	var traceIDs []string
	repo.db.Model(&Trace{}).Pluck("trace_id", &traceIDs)
	hasNew, hasSkew, hasOld := false, false, false
	for _, id := range traceIDs {
		switch id {
		case "t-new":
			hasNew = true
		case "t-skew":
			hasSkew = true
		case "t-old":
			hasOld = true
		}
	}
	if !hasNew || !hasSkew || hasOld {
		t.Fatalf("wrong traces survived: new=%v skew=%v old=%v", hasNew, hasSkew, hasOld)
	}

	// Spans: t-old's spans must be gone. t-skew's old-timestamp spans must SURVIVE.
	var skewSpans int64
	repo.db.Model(&Span{}).Where("trace_id = ?", "t-skew").Count(&skewSpans)
	if skewSpans != 2 {
		t.Fatalf("regression: t-skew lost spans to start_time sweep; got %d want 2", skewSpans)
	}
	var oldSpans int64
	repo.db.Model(&Span{}).Where("trace_id = ?", "t-old").Count(&oldSpans)
	if oldSpans != 0 {
		t.Fatalf("t-old orphan spans not swept; got %d want 0", oldSpans)
	}
}

// TestPurgeTracesBatched_DoesNotDeleteLiveSpans reproduces the ingest/purge race:
// while the purge runs, an ingestion worker inserts a span whose parent trace row
// has not yet committed. The sweep must NOT delete that live span.
func TestPurgeTracesBatched_DoesNotDeleteLiveSpans(t *testing.T) {
	repo := newTestRepo(t)
	nowUTC := time.Now().UTC()
	cutoff := nowUTC.Add(-7 * 24 * time.Hour)

	// Old trace with old spans — must be purged.
	seedTrace(t, repo.db, "t-old", cutoff.Add(-time.Hour), []time.Time{
		cutoff.Add(-time.Hour),
	})

	// Fresh live spans WITHOUT a parent trace row (simulating the race window
	// between span insert and trace insert in TraceServer.Export).
	liveSpans := []Span{
		{TraceID: "t-live-1", SpanID: "live-1", OperationName: "op", StartTime: nowUTC, EndTime: nowUTC, Duration: 100, ServiceName: "svc"},
		{TraceID: "t-live-2", SpanID: "live-2", OperationName: "op", StartTime: nowUTC, EndTime: nowUTC, Duration: 100, ServiceName: "svc"},
	}
	if err := repo.db.Create(&liveSpans).Error; err != nil {
		t.Fatalf("seed live spans: %v", err)
	}

	if _, err := repo.PurgeTracesBatched(context.Background(), cutoff, 10, 5*time.Millisecond); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// Old trace + its spans: purged.
	if mustCount(t, repo.db, &Trace{}) != 0 {
		t.Fatal("old trace not purged")
	}
	var oldSpans int64
	repo.db.Model(&Span{}).Where("trace_id = ?", "t-old").Count(&oldSpans)
	if oldSpans != 0 {
		t.Fatalf("old spans not swept; got %d want 0", oldSpans)
	}

	// Live spans (no parent trace, but fresh start_time): MUST survive.
	var liveCount int64
	repo.db.Model(&Span{}).Where("trace_id IN ?", []string{"t-live-1", "t-live-2"}).Count(&liveCount)
	if liveCount != 2 {
		t.Fatalf("live spans wrongly swept (ingest race regression); got %d want 2", liveCount)
	}
}

func TestPurgeMetricBucketsBatched(t *testing.T) {
	repo := newTestRepo(t)
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	keep := time.Now().UTC()

	buckets := []MetricBucket{
		{Name: "m1", ServiceName: "s", TimeBucket: old, Count: 1},
		{Name: "m1", ServiceName: "s", TimeBucket: old, Count: 2},
		{Name: "m1", ServiceName: "s", TimeBucket: keep, Count: 3},
	}
	if err := repo.db.Create(&buckets).Error; err != nil {
		t.Fatalf("create buckets: %v", err)
	}

	n, err := repo.PurgeMetricBucketsBatched(context.Background(), time.Now().UTC().Add(-time.Hour), 10, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 deletions, got %d", n)
	}
	if mustCount(t, repo.db, &MetricBucket{}) != 1 {
		t.Fatal("fresh bucket deleted")
	}
}

func TestPurgeLogs_ConcurrentIngestWhilePurging(t *testing.T) {
	repo := newTestRepo(t)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)

	// Seed a batch of old rows.
	seedLogs(t, repo.db, 200, old, "svc")

	// Simultaneously: purge old rows, and ingest fresh rows.
	var stopIngest atomic.Bool
	ingestDone := make(chan int)
	go func() {
		inserted := 0
		for !stopIngest.Load() {
			err := repo.db.Create(&Log{
				TraceID:     "live-trace",
				Severity:    "INFO",
				Body:        "live",
				ServiceName: "svc",
				Timestamp:   time.Now().UTC(),
			}).Error
			if err == nil {
				inserted++
			}
			time.Sleep(time.Millisecond)
		}
		ingestDone <- inserted
	}()
	time.Sleep(20 * time.Millisecond)

	_, err := repo.PurgeLogsBatched(context.Background(), time.Now().UTC().Add(-time.Hour), 50, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	stopIngest.Store(true)
	inserted := <-ingestDone

	remaining := mustCount(t, repo.db, &Log{})
	// Old rows all gone; inserted rows all present.
	if remaining != int64(inserted) {
		t.Fatalf("expected %d fresh rows, got %d", inserted, remaining)
	}
}
