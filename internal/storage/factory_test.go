package storage

import (
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/RandomCodeSpace/otelcontext/internal/membudget"
)

func TestNewDatabase_UnsupportedDriver(t *testing.T) {
	_, err := NewDatabase("mongodb", "whatever")
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("want unsupported-driver error, got %v", err)
	}
}

func TestNewDatabase_PostgresRequiresDSN(t *testing.T) {
	_, err := NewDatabase("postgres", "")
	if err == nil || !strings.Contains(err.Error(), "DB_DSN is required") {
		t.Fatalf("postgres without DSN must error; got %v", err)
	}
	_, err = NewDatabase("postgresql", "")
	if err == nil {
		t.Fatal("postgresql alias should also require DSN")
	}
}

func TestNewDatabase_SQLServerRequiresDSN(t *testing.T) {
	_, err := NewDatabase("sqlserver", "")
	if err == nil || !strings.Contains(err.Error(), "DB_DSN is required") {
		t.Fatalf("want DSN required; got %v", err)
	}
	_, err = NewDatabase("mssql", "")
	if err == nil {
		t.Fatal("mssql alias should require DSN")
	}
}

func TestNewDatabase_SQLiteDefaults(t *testing.T) {
	// Use explicit in-memory to avoid polluting the working dir.
	db, err := NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if db == nil {
		t.Fatal("nil db")
	}
	_ = closeDB(db)
}

func TestNewDatabase_DriverCaseInsensitive(t *testing.T) {
	for _, drv := range []string{"SQLite", "SQLITE", "Sqlite"} {
		db, err := NewDatabase(drv, ":memory:")
		if err != nil {
			t.Fatalf("%s: %v", drv, err)
		}
		_ = closeDB(db)
	}
}

func TestAutoMigrateModels_SQLite_AllTablesCreated(t *testing.T) {
	db, err := NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer closeDB(db)

	if err := AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, table := range []string{"traces", "spans", "logs", "metric_buckets"} {
		var exists int
		db.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&exists)
		if exists != 1 {
			t.Fatalf("table %s not created", table)
		}
	}
}

// clearSQLiteMemoryEnv neutralizes the two PRAGMA override env vars for the
// duration of the test. t.Setenv("") registers the restore; an empty value
// fails strconv parsing and is therefore ignored by sqliteMemorySizes.
func clearSQLiteMemoryEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SQLITE_CACHE_SIZE_KB", "")
	t.Setenv("SQLITE_MMAP_SIZE_BYTES", "")
}

// pragmaInt64 round-trips a PRAGMA query against the live connection.
func pragmaInt64(t *testing.T, db *gorm.DB, pragma string) int64 {
	t.Helper()
	var v int64
	if err := db.Raw("PRAGMA " + pragma).Scan(&v).Error; err != nil {
		t.Fatalf("PRAGMA %s: %v", pragma, err)
	}
	return v
}

func TestSQLiteMemorySizes_BudgetScaling(t *testing.T) {
	clearSQLiteMemoryEnv(t)
	cases := []struct {
		name        string
		budget      int64
		wantCacheKB int64
		wantMmap    int64
	}{
		{"detection failed -> legacy hardcoded", 0, 262144, 1073741824},
		{"4GB host -> 128MB cache, 512MB mmap", 4 << 30, 131072, 512 << 20},
		{"1GB host clamps to floors", 1 << 30, 65536, 268435456},
		{"64GB host clamps to ceilings", 64 << 30, 262144, 1073741824},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cacheKB, mmapBytes := sqliteMemorySizes(tc.budget)
			if cacheKB != tc.wantCacheKB || mmapBytes != tc.wantMmap {
				t.Fatalf("sqliteMemorySizes(%d)=(%d,%d) want (%d,%d)",
					tc.budget, cacheKB, mmapBytes, tc.wantCacheKB, tc.wantMmap)
			}
		})
	}
}

func TestSQLiteMemorySizes_EnvOverridesWinOverBudget(t *testing.T) {
	t.Setenv("SQLITE_CACHE_SIZE_KB", "12345")
	t.Setenv("SQLITE_MMAP_SIZE_BYTES", "0") // 0 = disable mmap, a legitimate override
	cacheKB, mmapBytes := sqliteMemorySizes(4 << 30)
	if cacheKB != 12345 || mmapBytes != 0 {
		t.Fatalf("env overrides ignored: got (%d,%d) want (12345,0)", cacheKB, mmapBytes)
	}
}

func TestSQLiteMemorySizes_InvalidEnvIgnored(t *testing.T) {
	t.Setenv("SQLITE_CACHE_SIZE_KB", "not-a-number")
	t.Setenv("SQLITE_MMAP_SIZE_BYTES", "-1") // negative mmap is invalid
	cacheKB, mmapBytes := sqliteMemorySizes(4 << 30)
	if cacheKB != 131072 || mmapBytes != 512<<20 {
		t.Fatalf("invalid env must fall back to budget scaling: got (%d,%d)", cacheKB, mmapBytes)
	}
}

func TestNewDatabase_SQLitePragmas_EnvOverrideRoundTrip(t *testing.T) {
	t.Setenv("SQLITE_CACHE_SIZE_KB", "12345")
	t.Setenv("SQLITE_MMAP_SIZE_BYTES", "33554432")

	db, err := NewDatabase("sqlite", filepath.Join(t.TempDir(), "override.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer closeDB(db)

	if got := pragmaInt64(t, db, "cache_size"); got != -12345 {
		t.Fatalf("cache_size=%d want -12345", got)
	}
	if got := pragmaInt64(t, db, "mmap_size"); got != 33554432 {
		t.Fatalf("mmap_size=%d want 33554432", got)
	}
}

func TestNewDatabase_SQLitePragmas_BudgetScaledRoundTrip(t *testing.T) {
	clearSQLiteMemoryEnv(t)

	// Whatever this host's budget resolves to, the live connection must carry
	// the same numbers the sizing function computes — proves the wiring, not
	// the host RAM.
	budget, _ := membudget.Detect()
	wantCacheKB, wantMmap := sqliteMemorySizes(budget)

	db, err := NewDatabase("sqlite", filepath.Join(t.TempDir(), "scaled.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer closeDB(db)

	if got := pragmaInt64(t, db, "cache_size"); got != -wantCacheKB {
		t.Fatalf("cache_size=%d want %d", got, -wantCacheKB)
	}
	if got := pragmaInt64(t, db, "mmap_size"); got != wantMmap {
		t.Fatalf("mmap_size=%d want %d", got, wantMmap)
	}

	// The rest of the hardened stanza must stay byte-identical — guard the
	// values that round-trip numerically.
	fixed := []struct {
		pragma string
		want   int64
	}{
		{"synchronous", 1},          // NORMAL
		{"temp_store", 2},           // MEMORY
		{"wal_autocheckpoint", 10000},
		{"journal_size_limit", 67108864},
		{"busy_timeout", 5000},
	}
	for _, f := range fixed {
		if got := pragmaInt64(t, db, f.pragma); got != f.want {
			t.Fatalf("PRAGMA %s=%d want %d (hardened stanza must not drift)", f.pragma, got, f.want)
		}
	}
	var mode string
	if err := db.Raw("PRAGMA journal_mode").Scan(&mode).Error; err != nil || !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode=%q err=%v want wal", mode, err)
	}
}

func TestNewDatabase_SQLiteAutoVacuumIncremental(t *testing.T) {
	clearSQLiteMemoryEnv(t)

	// Best-effort PRAGMA at startup: on a fresh database file it must take
	// effect (auto_vacuum=2 = INCREMENTAL) because it runs before the first
	// table is created. Pre-existing files keep their stored mode — that is
	// accepted and not asserted here.
	db, err := NewDatabase("sqlite", filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer closeDB(db)

	if got := pragmaInt64(t, db, "auto_vacuum"); got != 2 {
		t.Fatalf("auto_vacuum=%d want 2 (INCREMENTAL) on a fresh database", got)
	}
}

func TestScrubDSN(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		mustContain string // expected token in the output
		mustNotHave string // sensitive token that must be gone
	}{
		{
			name:        "url-form-password",
			in:          "postgres://me:s3cret@h/db",
			mustContain: "REDACTED",
			mustNotHave: "s3cret",
		},
		{
			name:        "url-form-with-port-and-query",
			in:          "postgresql://admin:h@ckme@db.example.com:5432/app?sslmode=require",
			mustContain: "REDACTED",
			mustNotHave: "h@ckme",
		},
		{
			name:        "kv-form-password",
			in:          "host=x user=u password=s3cret sslmode=require",
			mustContain: "password=REDACTED",
			mustNotHave: "s3cret",
		},
		{
			name:        "kv-form-quoted-password",
			in:          `host=x user=u password='s3 cr3t' sslmode=require`,
			mustContain: "password=REDACTED",
			mustNotHave: "s3 cr3t",
		},
		{
			name:        "mixed-case-kv",
			in:          "host=x Password=TopSecret",
			mustContain: "REDACTED",
			mustNotHave: "TopSecret",
		},
		{
			name:        "embedded-in-error-wrap",
			in:          "dial failed: connect host=db user=u password=s3cret: timeout",
			mustContain: "REDACTED",
			mustNotHave: "s3cret",
		},
		{
			name:        "no-password-kv-unchanged",
			in:          "host=x user=u sslmode=require",
			mustContain: "host=x",
			mustNotHave: "REDACTED",
		},
		{
			name:        "empty",
			in:          "",
			mustContain: "",
			mustNotHave: "REDACTED",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scrubDSN(c.in)
			if c.mustContain != "" && !strings.Contains(got, c.mustContain) {
				t.Fatalf("scrubDSN(%q) = %q; want contains %q", c.in, got, c.mustContain)
			}
			if c.mustNotHave != "" && strings.Contains(got, c.mustNotHave) {
				t.Fatalf("scrubDSN(%q) = %q; leaked %q", c.in, got, c.mustNotHave)
			}
		})
	}
}

func TestAutoMigrateModels_IsIdempotent(t *testing.T) {
	db, err := NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer closeDB(db)

	for i := 0; i < 3; i++ {
		if err := AutoMigrateModels(db, "sqlite"); err != nil {
			t.Fatalf("migrate pass %d: %v", i, err)
		}
	}
}

// TestAutoMigrate_CreatesTenantCompositeIndexes asserts that every tenant-scoped
// composite index declared on our models is actually materialised on SQLite.
// Single-column tenant_id indexes were replaced with composites in the storage
// perf pass — this test prevents a silent regression.
func TestAutoMigrate_CreatesTenantCompositeIndexes(t *testing.T) {
	db, err := NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer closeDB(db)

	if err := AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// (table, expected_index_name) pairs.
	expected := []struct {
		table string
		index string
	}{
		// Trace
		{"traces", "idx_traces_tenant_ts"},
		{"traces", "idx_traces_tenant_service"},
		// Span
		{"spans", "idx_spans_tenant_trace"},
		{"spans", "idx_spans_tenant_service_start"},
		// Log
		{"logs", "idx_logs_tenant_ts"},
		{"logs", "idx_logs_tenant_service"},
		{"logs", "idx_logs_tenant_severity"},
		// MetricBucket
		{"metric_buckets", "idx_metrics_tenant_name_bucket"},
		{"metric_buckets", "idx_metrics_tenant_service_bucket"},
	}

	for _, tc := range expected {
		var count int
		// sqlite_master holds indexes under type='index'.
		err := db.Raw(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name=? AND name=?",
			tc.table, tc.index,
		).Scan(&count).Error
		if err != nil {
			t.Fatalf("sqlite_master query for %s.%s: %v", tc.table, tc.index, err)
		}
		if count != 1 {
			t.Errorf("expected composite index %s on table %s (count=%d)", tc.index, tc.table, count)
		}
	}
}
