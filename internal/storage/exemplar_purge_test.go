package storage

import (
	"context"
	"testing"
	"time"
)

// Exemplar-tier retention (#201 Q2). The exemplar tier keeps two days while
// HOT_RETENTION_DAYS keeps seven, so these tests are about the boundary
// between them: two-day-old rows go, six-day-old rows stay, and the FTS index
// goes with the logs rather than accumulating references to rows that no
// longer exist.
//
// Scaffolding (newTestRepo, seedTrace, seedLogs, mustCount) is shared with
// purge_test.go.

// ftsRowCount counts entries in the FTS5 shadow index.
func ftsRowCount(t *testing.T, repo *Repository) int64 {
	t.Helper()
	var n int64
	if err := repo.db.Raw("SELECT count(*) FROM logs_fts").Scan(&n).Error; err != nil {
		t.Fatalf("count logs_fts: %v", err)
	}
	return n
}

func TestPurgeExemplars_DeletesPastExemplarRetentionKeepsHotTier(t *testing.T) {
	repo := newTestRepo(t)
	if err := setupSQLiteFTS5(repo.db); err != nil {
		t.Fatalf("setupSQLiteFTS5: %v", err)
	}
	now := time.Now().UTC()
	exemplarCutoff := now.Add(-2 * 24 * time.Hour)

	// Past the exemplar cutoff but inside HOT_RETENTION_DAYS=7: exactly the
	// rows the exemplar tier is supposed to reclaim and the old purge kept.
	seedTrace(t, repo.db, "t-3d", now.Add(-3*24*time.Hour), []time.Time{
		now.Add(-3 * 24 * time.Hour),
		now.Add(-3 * 24 * time.Hour).Add(time.Second),
	})
	seedLogs(t, repo.db, 5, now.Add(-3*24*time.Hour), "checkout")

	// Inside the exemplar window: must survive untouched.
	seedTrace(t, repo.db, "t-1d", now.Add(-24*time.Hour), []time.Time{now.Add(-24 * time.Hour)})
	seedLogs(t, repo.db, 3, now.Add(-24*time.Hour), "payments")

	ftsBefore := ftsRowCount(t, repo)
	if ftsBefore != 8 {
		t.Fatalf("logs_fts holds %d rows before the purge, want 8", ftsBefore)
	}

	stats, err := repo.PurgeExemplarsBatched(context.Background(), exemplarCutoff, 10, 0)
	if err != nil {
		t.Fatalf("PurgeExemplarsBatched: %v", err)
	}
	if stats.Traces != 1 {
		t.Fatalf("purged %d traces, want 1", stats.Traces)
	}
	if stats.Spans != 2 {
		t.Fatalf("purged %d spans, want 2", stats.Spans)
	}
	if stats.Logs != 5 {
		t.Fatalf("purged %d logs, want 5", stats.Logs)
	}

	if got := mustCount(t, repo.db, &Trace{}); got != 1 {
		t.Fatalf("%d traces remain, want 1 (the one inside the exemplar window)", got)
	}
	if got := mustCount(t, repo.db, &Span{}); got != 1 {
		t.Fatalf("%d spans remain, want 1", got)
	}
	if got := mustCount(t, repo.db, &Log{}); got != 3 {
		t.Fatalf("%d logs remain, want 3", got)
	}

	// FTS is content-linked and trigger-synced: deleting a log must delete its
	// index entry. An index that outlives its rows is disk the budget table
	// never accounted for.
	if got := ftsRowCount(t, repo); got != 3 {
		t.Fatalf("logs_fts holds %d rows after the purge, want 3 — the AFTER DELETE trigger did not fire", got)
	}

	// And the index must still answer for what survived.
	var hits int64
	if err := repo.db.Raw("SELECT count(*) FROM logs_fts WHERE logs_fts MATCH ?", "payments").Scan(&hits).Error; err != nil {
		t.Fatalf("MATCH query: %v", err)
	}
	if hits != 3 {
		t.Fatalf("FTS MATCH returned %d rows for the surviving service, want 3", hits)
	}
}

// TestPurgeExemplars_SweepsExpiredWeakReferences: a span whose trace row is
// already gone is a dangling weak reference, and it is charged to the main
// tier until something deletes it.
func TestPurgeExemplars_SweepsExpiredWeakReferences(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	cutoff := now.Add(-2 * 24 * time.Hour)

	seedTrace(t, repo.db, "t-orphan", now.Add(-3*24*time.Hour), []time.Time{now.Add(-3 * 24 * time.Hour)})
	// Delete the trace row only, leaving the span behind — the state a
	// cancelled or crashed purge pass leaves on disk.
	if err := repo.db.Unscoped().Where("trace_id = ?", "t-orphan").Delete(&Trace{}).Error; err != nil {
		t.Fatalf("detach trace: %v", err)
	}

	// A fresh trace with clock-skewed old spans must NOT be swept: its trace
	// row still exists, so the reference is live, not expired.
	seedTrace(t, repo.db, "t-skew", now, []time.Time{now.Add(-5 * 24 * time.Hour)})

	stats, err := repo.PurgeExemplarsBatched(context.Background(), cutoff, 10, 0)
	if err != nil {
		t.Fatalf("PurgeExemplarsBatched: %v", err)
	}
	if stats.Spans+stats.OrphanSpans != 1 {
		t.Fatalf("swept %d weak references, want 1", stats.Spans+stats.OrphanSpans)
	}
	var skew int64
	repo.db.Model(&Span{}).Where("trace_id = ?", "t-skew").Count(&skew)
	if skew != 1 {
		t.Fatalf("the clock-skewed span under a live trace was swept: %d remain, want 1", skew)
	}
}

// TestPurgeExemplars_TraceAndSpanDeletesShareATransaction: a reader must never
// observe spans whose trace row has already gone. The assertion is behavioural
// — the two deletes are issued inside one transaction, so a failure injected
// into the span delete rolls the trace delete back with it.
func TestPurgeExemplars_TraceAndSpanDeletesShareATransaction(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	cutoff := now.Add(-2 * 24 * time.Hour)
	seedTrace(t, repo.db, "t-old", now.Add(-3*24*time.Hour), []time.Time{now.Add(-3 * 24 * time.Hour)})

	// Drop the spans table out from under the pass: the span DELETE fails,
	// and the trace DELETE in the same transaction must not survive it.
	if err := repo.db.Exec("ALTER TABLE spans RENAME TO spans_hidden").Error; err != nil {
		t.Fatalf("hide spans table: %v", err)
	}
	if _, err := repo.PurgeExemplarsBatched(context.Background(), cutoff, 10, 0); err == nil {
		t.Fatal("purge reported success with a broken spans table")
	}
	if err := repo.db.Exec("ALTER TABLE spans_hidden RENAME TO spans").Error; err != nil {
		t.Fatalf("restore spans table: %v", err)
	}

	if got := mustCount(t, repo.db, &Trace{}); got != 1 {
		t.Fatalf("%d traces remain after a rolled-back pass, want 1 — the trace delete was not transactional with the span delete", got)
	}
	if got := mustCount(t, repo.db, &Span{}); got != 1 {
		t.Fatalf("%d spans remain, want 1", got)
	}
}

// TestPurgeExemplars_EmptyAndCancelled: the boring paths, because a purge that
// spins on an empty table is a purge that holds the writer lock for nothing.
func TestPurgeExemplars_EmptyAndCancelled(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()

	stats, err := repo.PurgeExemplarsBatched(context.Background(), now.Add(-2*24*time.Hour), 100, 0)
	if err != nil {
		t.Fatalf("empty purge: %v", err)
	}
	if stats.Total() != 0 {
		t.Fatalf("empty purge deleted %d rows", stats.Total())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.PurgeExemplarsBatched(ctx, now, 100, 0); err == nil {
		t.Fatal("cancelled purge returned nil error")
	}
}

// TestRetentionSchedulerRunsExemplarPurgeAheadOfHotRetention: the wiring, not
// the SQL. Without SetExemplarRetention nothing in the exemplar tier is
// touched — which is exactly what legacy and shadow mode need.
func TestRetentionSchedulerRunsExemplarPurgeAheadOfHotRetention(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()

	// Three days old: past the 2-day exemplar tier, inside 7-day hot retention.
	seedTrace(t, repo.db, "t-3d", now.Add(-3*24*time.Hour), []time.Time{now.Add(-3 * 24 * time.Hour)})
	seedLogs(t, repo.db, 4, now.Add(-3*24*time.Hour), "checkout")

	// No exemplar retention configured: the hot purge alone keeps everything.
	s := NewRetentionScheduler(repo, 7, 100, 0)
	s.runPurge(context.Background())
	if got := mustCount(t, repo.db, &Trace{}); got != 1 {
		t.Fatalf("%d traces after a hot-only purge, want 1", got)
	}

	// With it configured, the same rows go.
	s.SetExemplarRetention(2)
	s.runPurge(context.Background())
	if got := mustCount(t, repo.db, &Trace{}); got != 0 {
		t.Fatalf("%d traces after the exemplar purge, want 0", got)
	}
	if got := mustCount(t, repo.db, &Log{}); got != 0 {
		t.Fatalf("%d logs after the exemplar purge, want 0", got)
	}
}

// TestPurgeExemplarsNow_RunsOffTick covers the watchdog's on-demand trigger.
func TestPurgeExemplarsNow_RunsOffTick(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	seedLogs(t, repo.db, 4, now.Add(-3*24*time.Hour), "checkout")

	s := NewRetentionScheduler(repo, 7, 100, 0)
	s.PurgeExemplarsNow(context.Background())
	if got := mustCount(t, repo.db, &Log{}); got != 4 {
		t.Fatalf("%d logs, want 4 — an unconfigured exemplar tier must purge nothing", got)
	}

	s.SetExemplarRetention(2)
	s.PurgeExemplarsNow(context.Background())
	if got := mustCount(t, repo.db, &Log{}); got != 0 {
		t.Fatalf("%d logs after the on-demand purge, want 0", got)
	}
}
