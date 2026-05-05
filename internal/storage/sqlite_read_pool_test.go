package storage

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestSQLiteReadPoolSizeFromEnv(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want int
	}{
		{name: "unset_default", set: false, want: 4},
		{name: "empty_string_falls_to_default", set: true, val: "", want: 4},
		{name: "zero_disables", set: true, val: "0", want: 0},
		{name: "off_disables", set: true, val: "off", want: 0},
		{name: "OFF_disables", set: true, val: "OFF", want: 0},
		{name: "false_disables", set: true, val: "false", want: 0},
		{name: "no_disables", set: true, val: "no", want: 0},
		{name: "positive_int", set: true, val: "12", want: 12},
		{name: "garbage_falls_to_default", set: true, val: "not-a-number", want: 4},
		{name: "negative_falls_to_default", set: true, val: "-5", want: 4},
		{name: "whitespace_trimmed", set: true, val: "   8   ", want: 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("DB_SQLITE_READ_POOL_SIZE", tc.val)
			} else {
				// Truly unset — exercises the !ok branch in sqliteReadPoolSizeFromEnv.
				// t.Setenv with an existing parent value would set it; explicitly
				// unset and restore via t.Cleanup so other sub-tests are unaffected.
				prev, hadPrev := os.LookupEnv("DB_SQLITE_READ_POOL_SIZE")
				os.Unsetenv("DB_SQLITE_READ_POOL_SIZE")
				t.Cleanup(func() {
					if hadPrev {
						os.Setenv("DB_SQLITE_READ_POOL_SIZE", prev)
					} else {
						os.Unsetenv("DB_SQLITE_READ_POOL_SIZE")
					}
				})
			}
			got := sqliteReadPoolSizeFromEnv()
			if got != tc.want {
				t.Fatalf("sqliteReadPoolSizeFromEnv(%q) = %d, want %d", tc.val, got, tc.want)
			}
		})
	}
}

// TestNewSQLiteReadPool_QueryOnly verifies the read pool is provisioned with
// query_only=ON, so a buggy caller that routes a write through reader() gets
// a fast SQLite error rather than silently corrupting state.
func TestNewSQLiteReadPool_QueryOnly(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "rp.db")

	// Writer pool first — that's the migration owner.
	writer, err := NewDatabase("sqlite", dsn)
	if err != nil {
		t.Fatalf("NewDatabase writer: %v", err)
	}
	t.Cleanup(func() { _ = closeDB(writer) })
	if err := AutoMigrateModels(writer, "sqlite"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rp, err := NewSQLiteReadPool(dsn, 4)
	if err != nil {
		t.Fatalf("NewSQLiteReadPool: %v", err)
	}
	t.Cleanup(func() { _ = closeDB(rp) })

	// SELECT must succeed.
	var count int64
	if err := rp.Model(&Log{}).Count(&count).Error; err != nil {
		t.Fatalf("read-pool SELECT failed: %v", err)
	}

	// INSERT must fail because query_only=ON.
	err = rp.Create(&Log{Body: "should-fail", ServiceName: "svc", Severity: "INFO", Timestamp: time.Now()}).Error
	if err == nil {
		t.Fatal("expected query_only INSERT to fail; it succeeded")
	}
}

// TestRepository_ReaderFallback covers both branches of reader():
//   - readPool == nil → falls back to writer
//   - readPool != nil → returns the read pool
func TestRepository_ReaderFallback(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "fb.db")

	writer, err := NewDatabase("sqlite", dsn)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { _ = closeDB(writer) })
	if err := AutoMigrateModels(writer, "sqlite"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	repo := &Repository{db: writer, driver: "sqlite"}
	if repo.reader() != writer {
		t.Fatal("reader() should fall back to writer when readPool is nil")
	}

	rp, err := NewSQLiteReadPool(dsn, 2)
	if err != nil {
		t.Fatalf("NewSQLiteReadPool: %v", err)
	}
	repo.readPool = rp
	t.Cleanup(func() { _ = closeDB(rp) })

	if repo.reader() != rp {
		t.Fatal("reader() should return readPool when it is set")
	}
}

// TestSQLiteReadPool_ConcurrentReadsAndWrites runs N parallel reads through
// the read pool while a background goroutine pumps writes through the writer.
// All reads must succeed and the run must complete within a generous deadline,
// proving the writer's MaxOpen=1 connection is not on the read critical path.
func TestSQLiteReadPool_ConcurrentReadsAndWrites(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "concurrent.db")

	writer, err := NewDatabase("sqlite", dsn)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { _ = closeDB(writer) })
	if err := AutoMigrateModels(writer, "sqlite"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	seedLogs(t, writer, 64, time.Now(), "svc-rp")

	rp, err := NewSQLiteReadPool(dsn, 4)
	if err != nil {
		t.Fatalf("NewSQLiteReadPool: %v", err)
	}
	t.Cleanup(func() { _ = closeDB(rp) })

	repo := &Repository{db: writer, driver: "sqlite", readPool: rp}

	// Identity-based assertions: prove the read path is wired through rp,
	// not silently routed back through the writer pool.
	if repo.reader() == writer {
		t.Fatal("repo.reader() returned writer pool — read pool not wired")
	}
	if repo.reader() != rp {
		t.Fatal("repo.reader() did not return the configured read pool")
	}

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = writer.Create(&Log{
				TraceID: "t", SpanID: "s", Severity: "INFO",
				Body: "writer-pump", ServiceName: "svc-rp", Timestamp: time.Now(),
			}).Error
		}
	}()

	const readers = 8
	const readsEach = 20
	var wg sync.WaitGroup
	wg.Add(readers)
	var latMu sync.Mutex
	lats := make([]time.Duration, 0, readers*readsEach)
	for range readers {
		go func() {
			defer wg.Done()
			for range readsEach {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				t0 := time.Now()
				_, err := repo.RecentLogs(ctx, 8)
				cancel()
				if err != nil {
					t.Errorf("RecentLogs concurrent read failed: %v", err)
					return
				}
				latMu.Lock()
				lats = append(lats, time.Since(t0))
				latMu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-writerDone

	// p99 latency check — well above the writer's MaxOpen=1 contention window
	// but below what a serialized pool would produce when the writer is hot.
	slices.Sort(lats)
	p99 := lats[len(lats)*99/100]
	t.Logf("concurrent read pool: n=%d p50=%s p99=%s max=%s", len(lats), lats[len(lats)/2], p99, lats[len(lats)-1])
	if p99 > 200*time.Millisecond {
		t.Fatalf("p99 read latency %s exceeds 200ms — read pool likely serialized", p99)
	}
}
