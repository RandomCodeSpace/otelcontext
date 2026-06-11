package storage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
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

// newFileTestRepo builds a Repository backed by a temp-file SQLite DB. The
// vacuum-mode tests below assert on PRAGMA freelist_count, which needs a real
// database file rather than :memory:.
func newFileTestRepo(t *testing.T) *Repository {
	t.Helper()
	t.Setenv("LOG_FTS_ENABLED", "false")
	db, err := NewDatabase("sqlite", filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := &Repository{db: db, driver: "sqlite"}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func freelistCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Raw("PRAGMA freelist_count").Scan(&n).Error; err != nil {
		t.Fatalf("PRAGMA freelist_count: %v", err)
	}
	return n
}

// forceAutoVacuumNone rewrites the database file with auto_vacuum=NONE so
// PRAGMA incremental_vacuum becomes a guaranteed no-op — the file shape every
// pre-2026-06 deployment has. VACUUM must run on the raw *sql.DB (GORM's Exec
// wraps statements in an implicit tx, and VACUUM refuses to run inside one).
func forceAutoVacuumNone(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("raw sql.DB: %v", err)
	}
	if _, err := sqlDB.Exec("PRAGMA auto_vacuum=NONE"); err != nil {
		t.Fatalf("PRAGMA auto_vacuum=NONE: %v", err)
	}
	if _, err := sqlDB.Exec("VACUUM"); err != nil {
		t.Fatalf("VACUUM (rebuild to NONE): %v", err)
	}
	var av int64
	if err := db.Raw("PRAGMA auto_vacuum").Scan(&av).Error; err != nil || av != 0 {
		t.Fatalf("auto_vacuum=%d err=%v, want 0 (NONE)", av, err)
	}
}

// seedAndDeleteLogs fills the freelist: insert rows, then bulk-delete them so
// the freed pages sit on the freelist awaiting a vacuum of either flavor.
func seedAndDeleteLogs(t *testing.T, db *gorm.DB) {
	t.Helper()
	seedLogs(t, db, 2000, time.Now().UTC(), "vacuum-svc")
	if err := db.Exec("DELETE FROM logs").Error; err != nil {
		t.Fatalf("delete logs: %v", err)
	}
	if freelistCount(t, db) == 0 {
		t.Fatal("test setup broken: bulk delete left an empty freelist")
	}
}

func TestSQLiteVacuumStatement(t *testing.T) {
	if got := sqliteVacuumStatement(false); got != "PRAGMA incremental_vacuum(10000)" {
		t.Fatalf("default statement = %q, want incremental_vacuum", got)
	}
	if got := sqliteVacuumStatement(true); got != "VACUUM" {
		t.Fatalf("full-vacuum statement = %q, want VACUUM", got)
	}
}

// TestRetentionScheduler_MaintenanceDefaultRunsIncrementalVacuum proves the
// default daily maintenance actually executes incremental_vacuum: on an
// auto_vacuum=INCREMENTAL file (what NewDatabase provisions on fresh deploys)
// freed pages stay on the freelist until incremental_vacuum releases them, so
// an empty freelist after runMaintenance means the statement ran.
func TestRetentionScheduler_MaintenanceDefaultRunsIncrementalVacuum(t *testing.T) {
	repo := newFileTestRepo(t)
	var av int64
	if err := repo.db.Raw("PRAGMA auto_vacuum").Scan(&av).Error; err != nil || av != 2 {
		t.Fatalf("precondition: fresh DB should be auto_vacuum=INCREMENTAL, got %d (err=%v)", av, err)
	}
	seedAndDeleteLogs(t, repo.db)

	r := NewRetentionScheduler(repo, 7, 10_000, 0)
	r.runMaintenance(context.Background())

	if n := freelistCount(t, repo.db); n != 0 {
		t.Fatalf("freelist=%d after default maintenance; incremental_vacuum should have released all pages", n)
	}
}

// TestRetentionScheduler_MaintenanceDefaultSkipsFullVacuum proves the default
// daily maintenance no longer runs a full VACUUM: on an auto_vacuum=NONE file
// (every pre-2026-06 deployment) incremental_vacuum is a no-op, so the
// freelist survives — a full VACUUM would have rebuilt the file and emptied it.
func TestRetentionScheduler_MaintenanceDefaultSkipsFullVacuum(t *testing.T) {
	repo := newFileTestRepo(t)
	forceAutoVacuumNone(t, repo.db)
	seedAndDeleteLogs(t, repo.db)

	r := NewRetentionScheduler(repo, 7, 10_000, 0)
	r.runMaintenance(context.Background())

	// PRAGMA optimize may allocate a few stats pages from the freelist, so
	// assert "not fully reclaimed" rather than an exact count.
	if n := freelistCount(t, repo.db); n == 0 {
		t.Fatal("freelist emptied — a full VACUUM ran during default maintenance; expected incremental_vacuum no-op")
	}
}

// TestRetentionScheduler_MaintenanceFullVacuumWhenEnabled proves the
// RETENTION_FULL_VACUUM opt-in restores the legacy behavior on the same
// auto_vacuum=NONE file shape where incremental_vacuum provably no-ops.
func TestRetentionScheduler_MaintenanceFullVacuumWhenEnabled(t *testing.T) {
	repo := newFileTestRepo(t)
	forceAutoVacuumNone(t, repo.db)
	seedAndDeleteLogs(t, repo.db)

	r := NewRetentionScheduler(repo, 7, 10_000, 0)
	r.SetFullVacuum(true)
	r.runMaintenance(context.Background())

	if n := freelistCount(t, repo.db); n != 0 {
		t.Fatalf("freelist=%d; RETENTION_FULL_VACUUM=true must run a full VACUUM and empty the freelist", n)
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
