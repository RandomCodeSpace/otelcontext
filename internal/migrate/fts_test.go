package migrate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

func TestSQLiteUpProvisionsConfiguredLogFTS(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "true")
	db := newSQLiteMigrationDB(t)
	ctx := context.Background()
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireExact(ctx, db, "sqlite"); err != nil {
		t.Fatalf("enabled-feature startup after migrate up: %v", err)
	}
	row := storage.Log{TenantID: "alpha", Body: "workers running", ServiceName: "api", Timestamp: time.Now().UTC()}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	// Porter stemming matches runs to running; the legacy LIKE fallback does not.
	logs, err := repo.SearchLogs(storage.WithTenantContext(ctx, "alpha"), "runs", 10)
	if err != nil || len(logs) != 1 || logs[0].ID != row.ID {
		t.Fatalf("production migration must provision FTS keyword search: logs=%v err=%v", logs, err)
	}
}

func TestSQLiteUpBackfillsExactSchemaAndStartupIsReadOnly(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "false")
	db := newSQLiteMigrationDB(t)
	ctx := context.Background()
	before, err := Up(ctx, db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RequireExact(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if got := ftsSchema(t, db); len(got) != 0 {
		t.Fatalf("disabled FTS installed objects: %v", got)
	}
	rows := []storage.Log{
		{TenantID: "alpha", Body: "workers running", ServiceName: "api", Timestamp: time.Now().UTC()},
		{TenantID: "beta", Body: "workers running", ServiceName: "api", Timestamp: time.Now().UTC()},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	// Unset is the production default, and must enable both provisioning and validation.
	if err := os.Unsetenv("LOG_FTS_ENABLED"); err != nil {
		t.Fatal(err)
	}
	mustExecFTS(t, db, "PRAGMA query_only=ON")
	missing, err := RequireExact(ctx, db, "sqlite")
	if err == nil || missing.Usable() || !strings.Contains(err.Error(), "logs_fts") || !strings.Contains(err.Error(), "migrate up") {
		t.Fatalf("missing-feature startup: status=%#v err=%v", missing, err)
	}
	if got := ftsSchema(t, db); len(got) != 0 {
		t.Fatalf("startup changed schema: %v", got)
	}
	mustExecFTS(t, db, "PRAGMA query_only=OFF")
	after, err := Up(ctx, db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if after.ActualVersion != CurrentVersion || before.Fingerprint != after.Fingerprint || !reflect.DeepEqual(before.Applied, after.Applied) {
		t.Fatalf("optional feature changed versioned contract: before=%#v after=%#v", before, after)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	for _, row := range rows {
		assertLogSearch(t, repo, row.TenantID, "runs", row.ID)
	}
	if err := db.Model(&rows[0]).Updates(map[string]any{"body": "workers stopped", "service_name": "billing"}).Error; err != nil {
		t.Fatal(err)
	}
	assertLogSearch(t, repo, "alpha", "runs")
	assertLogSearch(t, repo, "alpha", "stops", rows[0].ID)
	assertLogSearch(t, repo, "alpha", "billing", rows[0].ID)
	assertLogSearch(t, repo, "beta", "runs", rows[1].ID)
	if err := db.Delete(&rows[0]).Error; err != nil {
		t.Fatal(err)
	}
	assertFTSMatchIDs(t, db, "stops")
	assertLogSearch(t, repo, "alpha", "stops")
	// An operational index is accepted with SQLite itself forbidding writes.
	mustExecFTS(t, db, "PRAGMA query_only=ON")
	if _, err := RequireExact(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteLogFTSCompleteIndexIsNoOp(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "true")
	db := newSQLiteMigrationDB(t)
	ctx := context.Background()
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	row := storage.Log{Body: "workers running", Timestamp: time.Now().UTC()}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	before := sqliteChangeCounts(t, db)
	start := time.Now()
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	upElapsed := time.Since(start)
	start = time.Now()
	if _, err := RequireExact(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if after := sqliteChangeCounts(t, db); after != before {
		t.Fatalf("complete index caused writes/rebuild: before=%v after=%v", before, after)
	}
	t.Logf("diagnostic only: complete-index Up=%s RequireExact=%s; total_changes and schema_version unchanged", upElapsed, time.Since(start))
}

func TestSQLiteLogFTSRepairsMissingObjectsAndBackfills(t *testing.T) {
	for _, missing := range []string{"logs_fts", "logs_ai", "logs_ad", "logs_au"} {
		t.Run(missing, func(t *testing.T) {
			t.Setenv("LOG_FTS_ENABLED", "true")
			db := newSQLiteMigrationDB(t)
			ctx := context.Background()
			if _, err := Up(ctx, db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			row := storage.Log{TenantID: "alpha", Body: "workers running", Timestamp: time.Now().UTC()}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			if missing == "logs_fts" {
				mustExecFTS(t, db, "DROP TABLE logs_fts")
			} else {
				mustExecFTS(t, db, "DROP TRIGGER "+missing)
			}
			switch missing {
			case "logs_ai":
				row = storage.Log{TenantID: "alpha", Body: "workers stopped", Timestamp: time.Now().UTC()}
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			case "logs_ad":
				if err := db.Delete(&row).Error; err != nil {
					t.Fatal(err)
				}
			case "logs_au":
				if err := db.Model(&row).Update("body", "workers stopped").Error; err != nil {
					t.Fatal(err)
				}
			}
			before := ftsSchema(t, db)
			if status, err := RequireExact(ctx, db, "sqlite"); err == nil || status.Usable() || !strings.Contains(err.Error(), missing) {
				t.Fatalf("incomplete index accepted: status=%#v err=%v", status, err)
			}
			if !reflect.DeepEqual(before, ftsSchema(t, db)) {
				t.Fatal("startup repaired missing object")
			}
			if _, err := Up(ctx, db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			if _, err := RequireExact(ctx, db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			switch missing {
			case "logs_ai", "logs_au":
				assertFTSMatchIDs(t, db, "stops", row.ID)
			case "logs_ad":
				assertFTSMatchIDs(t, db, "runs")
			case "logs_fts":
				assertFTSMatchIDs(t, db, "runs", row.ID)
			}
			if missing == "logs_au" {
				assertFTSMatchIDs(t, db, "runs")
			}
			// Exercise all repaired triggers after the backfill.
			fresh := storage.Log{TenantID: "beta", Body: "repair inserting", Timestamp: time.Now().UTC()}
			if err := db.Create(&fresh).Error; err != nil {
				t.Fatal(err)
			}
			assertFTSMatchIDs(t, db, "inserts", fresh.ID)
			if err := db.Model(&fresh).Update("body", "repair updating").Error; err != nil {
				t.Fatal(err)
			}
			assertFTSMatchIDs(t, db, "inserts")
			assertFTSMatchIDs(t, db, "updates", fresh.ID)
			if err := db.Delete(&fresh).Error; err != nil {
				t.Fatal(err)
			}
			assertFTSMatchIDs(t, db, "updates")
		})
	}
}

func TestSQLiteLogFTSRejectsConflictingDefinitions(t *testing.T) {
	for _, wrong := range []string{
		"CREATE TABLE logs_fts (body text, service_name text)",
		"CREATE TRIGGER logs_ai AFTER INSERT ON logs BEGIN SELECT 1; END",
		"CREATE TRIGGER LOGS_AI AFTER INSERT ON logs BEGIN SELECT 1; END",
		"CREATE TRIGGER logs_au AFTER UPDATE ON logs BEGIN INSERT INTO logs_fts(rowid, body, service_name) VALUES (new.id, new.body, new.service_name); END",
	} {
		t.Run(wrong, func(t *testing.T) {
			t.Setenv("LOG_FTS_ENABLED", "false")
			db := newSQLiteMigrationDB(t)
			ctx := context.Background()
			if _, err := Up(ctx, db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			mustExecFTS(t, db, wrong)
			before := ftsSchema(t, db)
			t.Setenv("LOG_FTS_ENABLED", "true")
			if _, err := RequireExact(ctx, db, "sqlite"); err == nil || !strings.Contains(err.Error(), "incompatible SQLite log FTS object") {
				t.Fatalf("startup error=%v", err)
			}
			if _, err := Up(ctx, db, "sqlite"); err == nil || !strings.Contains(err.Error(), "incompatible SQLite log FTS object") {
				t.Fatalf("migration error=%v", err)
			}
			if !reflect.DeepEqual(before, ftsSchema(t, db)) {
				t.Fatal("conflicting definitions were overwritten")
			}
		})
	}
}

func TestSQLiteLogFTSDisabledLeavesExistingIndexAlone(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "true")
	db := newSQLiteMigrationDB(t)
	ctx := context.Background()
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	mustExecFTS(t, db, "DROP TRIGGER logs_ai")
	before := ftsSchema(t, db)
	t.Setenv("LOG_FTS_ENABLED", "false")
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireExact(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, ftsSchema(t, db)) {
		t.Fatal("disabled FTS mutated existing index")
	}
}

// Fault injection at the SQL transaction boundary makes cancellation after DDL
// deterministic, including on a fast machine with only a few fixture rows.
type interruptedFTSConnection struct {
	sqlExecutor
	failAt string
	fail   error
	cancel context.CancelFunc
}

func (c interruptedFTSConnection) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, c.failAt) {
		if c.cancel != nil {
			c.cancel()
			return c.sqlExecutor.ExecContext(ctx, query, args...)
		}
		return nil, c.fail
	}
	return c.sqlExecutor.ExecContext(ctx, query, args...)
}

func TestSQLiteLogFTSRepairFailureRollsBack(t *testing.T) {
	for _, mode := range []string{"ddl-error", "rebuild-error", "cancel-rebuild"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("LOG_FTS_ENABLED", "false")
			db := newSQLiteMigrationDB(t)
			ctx := context.Background()
			if _, err := Up(ctx, db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			row := storage.Log{TenantID: "alpha", Body: "workers running", Timestamp: time.Now().UTC()}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			before, err := Inspect(ctx, db, "sqlite")
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			repairCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			failure := errors.New("injected FTS failure")
			err = withLockedTransaction(repairCtx, sqlDB, "sqlite", func(tx sqlExecutor) error {
				conn := interruptedFTSConnection{sqlExecutor: tx, failAt: "'rebuild'", fail: failure}
				if mode == "ddl-error" {
					conn.failAt = "CREATE TRIGGER IF NOT EXISTS logs_ad"
				}
				if mode == "cancel-rebuild" {
					conn.cancel = cancel
				}
				return storage.EnsureSQLiteLogFTS(repairCtx, conn)
			})
			if mode == "cancel-rebuild" {
				// modernc may return SQLITE_INTERRUPT rather than wrapping
				// context.Canceled. Both must abort and roll back the feature.
				if err == nil || repairCtx.Err() != context.Canceled {
					t.Fatalf("cancelled repair: err=%v context=%v", err, repairCtx.Err())
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("failure=%v want=%v", err, failure)
			}
			if got := ftsSchema(t, db); len(got) != 0 {
				t.Fatalf("partially installed feature survived rollback: %v", got)
			}
			after, err := Inspect(ctx, db, "sqlite")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("versioned schema changed: before=%#v after=%#v err=%v", before, after, err)
			}
			t.Setenv("LOG_FTS_ENABLED", "true")
			if _, err := RequireExact(ctx, db, "sqlite"); err == nil {
				t.Fatal("rolled-back feature accepted at startup")
			}
			if _, err := Up(ctx, db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			assertFTSMatchIDs(t, db, "runs", row.ID)
		})
	}
}

func TestSQLiteLogFTSCancelledTriggerRepairPreservesOriginalIndex(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "true")
	db := newSQLiteMigrationDB(t)
	ctx := context.Background()
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	row := storage.Log{TenantID: "alpha", Body: "workers running", Timestamp: time.Now().UTC()}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	mustExecFTS(t, db, "DROP TRIGGER logs_au")
	if err := db.Model(&row).Update("body", "workers stopped").Error; err != nil {
		t.Fatal(err)
	}
	before := ftsSchema(t, db)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	repairCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	err = withLockedTransaction(repairCtx, sqlDB, "sqlite", func(tx sqlExecutor) error {
		return storage.EnsureSQLiteLogFTS(repairCtx, interruptedFTSConnection{sqlExecutor: tx, failAt: "'rebuild'", cancel: cancel})
	})
	if err == nil || repairCtx.Err() != context.Canceled {
		t.Fatalf("cancelled repair: %v", err)
	}
	if !reflect.DeepEqual(before, ftsSchema(t, db)) {
		t.Fatal("cancelled repair left a new trigger installed")
	}
	assertFTSMatchIDs(t, db, "runs", row.ID)
	assertFTSMatchIDs(t, db, "stops")
	if _, err := RequireExact(ctx, db, "sqlite"); err == nil {
		t.Fatal("cancelled trigger repair accepted at startup")
	}
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	assertFTSMatchIDs(t, db, "runs")
	assertFTSMatchIDs(t, db, "stops", row.ID)
}

func TestLogFTSDoesNotChangePreviewDriverContracts(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "true")
	for _, driver := range []string{"mysql", "mssql"} {
		status, err := Up(context.Background(), nil, driver)
		if err == nil || status.State != StateUnverified {
			t.Fatalf("Up(%s): status=%#v err=%v", driver, status, err)
		}
		status, err = RequireExact(context.Background(), nil, driver)
		if err == nil || status.State != StateUnverified {
			t.Fatalf("RequireExact(%s): status=%#v err=%v", driver, status, err)
		}
	}
}

func TestSQLiteLogFTSCancelledUpDoesNotRepair(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "false")
	db := newSQLiteMigrationDB(t)
	if _, err := Up(context.Background(), db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOG_FTS_ENABLED", "true")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Up(ctx, db, "sqlite"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Up error=%v", err)
	}
	if got := ftsSchema(t, db); len(got) != 0 {
		t.Fatalf("cancelled migration installed FTS: %v", got)
	}
}

func assertLogSearch(t *testing.T, repo *storage.Repository, tenant, keyword string, ids ...uint) {
	t.Helper()
	ctx := storage.WithTenantContext(context.Background(), tenant)
	rows, err := repo.SearchLogs(ctx, keyword, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]uint, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.ID)
	}
	if len(got) != len(ids) || (len(ids) != 0 && !reflect.DeepEqual(got, ids)) {
		t.Fatalf("tenant=%s keyword=%s got=%v want=%v", tenant, keyword, got, ids)
	}
	rows, total, err := repo.GetLogsV2(ctx, storage.LogFilter{Search: keyword, Limit: 10})
	if err != nil || total != int64(len(ids)) || len(rows) != len(ids) {
		t.Fatalf("GetLogsV2 tenant=%s keyword=%s rows=%v total=%d err=%v", tenant, keyword, rows, total, err)
	}
	for i := range rows {
		if rows[i].ID != ids[i] {
			t.Fatalf("GetLogsV2 row ID=%d want=%d", rows[i].ID, ids[i])
		}
	}
}

func assertFTSMatchIDs(t *testing.T, db *gorm.DB, keyword string, ids ...uint) {
	t.Helper()
	var got []uint
	if err := db.Raw("SELECT rowid FROM logs_fts WHERE logs_fts MATCH ? ORDER BY rowid", keyword).Scan(&got).Error; err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) || (len(ids) != 0 && !reflect.DeepEqual(got, ids)) {
		t.Fatalf("FTS keyword=%s got=%v want=%v", keyword, got, ids)
	}
}

func ftsSchema(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var objects []string
	if err := db.Raw("SELECT type || ':' || name || ':' || sql FROM sqlite_master WHERE name LIKE 'logs_fts%' OR name IN ('logs_ai','logs_ad','logs_au') ORDER BY name").Scan(&objects).Error; err != nil {
		t.Fatal(err)
	}
	return objects
}

func sqliteChangeCounts(t *testing.T, db *gorm.DB) [2]int64 {
	t.Helper()
	var counts [2]int64
	if err := db.Raw("SELECT total_changes()").Scan(&counts[0]).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("PRAGMA schema_version").Scan(&counts[1]).Error; err != nil {
		t.Fatal(err)
	}
	return counts
}

func mustExecFTS(t *testing.T, db *gorm.DB, query string) {
	t.Helper()
	if err := db.Exec(query).Error; err != nil {
		t.Fatal(err)
	}
}
