package migrate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

func versionThreeRetentionDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newSQLiteMigrationDB(t)
	r, err := newRunner("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if applied, err := r.applyNext(context.Background(), db, "sqlite"); err != nil || !applied {
			t.Fatalf("prepare version 3: applied=%v err=%v", applied, err)
		}
	}
	return db
}

func TestSQLiteRetentionIndexUpgradePreservesData(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "true")
	ctx := context.Background()
	db := versionThreeRetentionDB(t)
	// Model an existing optional FTS index without running AutoMigrate,
	// which would install the new retention index before this upgrade.
	for _, statement := range []string{
		`CREATE VIRTUAL TABLE logs_fts USING fts5(body, service_name,
content='logs', content_rowid='id', tokenize='porter unicode61 remove_diacritics 2')`,
		`CREATE TRIGGER logs_ai AFTER INSERT ON logs BEGIN
INSERT INTO logs_fts(rowid, body, service_name) VALUES (new.id, new.body, new.service_name); END`,
		`CREATE TRIGGER logs_ad AFTER DELETE ON logs BEGIN
INSERT INTO logs_fts(logs_fts, rowid, body, service_name) VALUES ('delete', old.id, old.body, old.service_name); END`,
		`CREATE TRIGGER logs_au AFTER UPDATE ON logs BEGIN
INSERT INTO logs_fts(logs_fts, rowid, body, service_name) VALUES ('delete', old.id, old.body, old.service_name);
INSERT INTO logs_fts(rowid, body, service_name) VALUES (new.id, new.body, new.service_name); END`,
	} {
		mustExecRetentionSQL(t, db, statement)
	}
	stamp := time.Date(2026, 10, 12, 4, 0, 0, 123456789, time.UTC)
	if err := db.Exec(`INSERT INTO traces (tenant_id, trace_id, service_name, timestamp, retained_span_count)
VALUES ('alpha', 'kept', 'api', ?, 1), ('beta', 'zero-span', 'worker', ?, 0)`, stamp, stamp).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO spans (tenant_id, trace_id, span_id, start_time, attributes_json)
VALUES ('alpha', 'kept', 'span', ?, '{"preserve":"payload"}')`, stamp).Error; err != nil {
		t.Fatal(err)
	}
	log := storage.Log{TenantID: "alpha", Body: "workers running", ServiceName: "api", Timestamp: stamp}
	if err := db.Create(&log).Error; err != nil {
		t.Fatal(err)
	}
	before, err := Inspect(ctx, db, "sqlite")
	if err != nil || before.State != StateBehind || before.ActualVersion != 3 {
		t.Fatalf("old schema: status=%#v err=%v", before, err)
	}
	readRows := func() map[string][]map[string]any {
		t.Helper()
		out := make(map[string][]map[string]any)
		for _, table := range []string{"traces", "spans", "logs"} {
			var rows []map[string]any
			if err := db.Table(table).Order("id").Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			out[table] = rows
		}
		return out
	}
	rowsBefore, ftsBefore := readRows(), retentionFTSSchema(t, db)
	mustExecRetentionSQL(t, db, "PRAGMA query_only=ON")
	if status, err := RequireExact(ctx, db, "sqlite"); err == nil || status.State != StateBehind {
		t.Fatalf("version 3 startup must refuse without migration: status=%#v err=%v", status, err)
	}
	mustExecRetentionSQL(t, db, "PRAGMA query_only=OFF")
	after, err := Up(ctx, db, "sqlite")
	if err != nil || after.ActualVersion != 4 || after.State != StateExact {
		t.Fatalf("upgrade: status=%#v err=%v", after, err)
	}
	if !reflect.DeepEqual(before.Applied, after.Applied[:3]) {
		t.Fatal("upgrade changed migration 1-3 ledger entries")
	}
	if !reflect.DeepEqual(rowsBefore, readRows()) || !reflect.DeepEqual(ftsBefore, retentionFTSSchema(t, db)) {
		t.Fatal("index migration changed rows or FTS definitions")
	}
	logs, err := storage.NewRepositoryFromDB(db, "sqlite").SearchLogs(storage.WithTenantContext(ctx, "alpha"), "runs", 10)
	if err != nil || len(logs) != 1 || logs[0].ID != log.ID {
		t.Fatalf("FTS search after migration: logs=%v err=%v", logs, err)
	}
	var plan []struct{ Detail string }
	if err := db.Raw(`EXPLAIN QUERY PLAN SELECT id FROM spans WHERE start_time < ? ORDER BY start_time, id LIMIT ?`, stamp, 100).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !strings.Contains(plan[0].Detail, "SEARCH spans USING COVERING INDEX idx_spans_start_time") {
		t.Fatalf("age retention must use the covering time index without a sort: %v", plan)
	}
	changes := retentionSQLiteChangeCounts(t, db)
	mustExecRetentionSQL(t, db, "PRAGMA query_only=ON")
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireExact(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if got := retentionSQLiteChangeCounts(t, db); got != changes {
		t.Fatalf("repeat migration/startup wrote database: before=%v after=%v", changes, got)
	}
}

func TestSQLiteRetentionIndexStartupRequiresExactIndex(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "false")
	for _, replacement := range []string{"", "CREATE INDEX idx_spans_start_time ON spans(end_time)"} {
		t.Run(replacement, func(t *testing.T) {
			db := newSQLiteMigrationDB(t)
			if _, err := Up(context.Background(), db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			mustExecRetentionSQL(t, db, "DROP INDEX idx_spans_start_time")
			if replacement != "" {
				mustExecRetentionSQL(t, db, replacement)
			}
			before := retentionSQLiteChangeCounts(t, db)
			mustExecRetentionSQL(t, db, "PRAGMA query_only=ON")
			status, err := RequireExact(context.Background(), db, "sqlite")
			if err == nil || status.State != StateIncompatible || !strings.Contains(status.Detail, "idx_spans_start_time") {
				t.Fatalf("invalid retention index accepted: status=%#v err=%v", status, err)
			}
			if status, err := Up(context.Background(), db, "sqlite"); err == nil || status.State != StateIncompatible {
				t.Fatalf("invalid index migration must refuse: status=%#v err=%v", status, err)
			}
			if got := retentionSQLiteChangeCounts(t, db); got != before {
				t.Fatalf("startup repaired schema: before=%v after=%v", before, got)
			}
		})
	}
}

func TestSQLiteRetentionIndexCanceledUpgradeDoesNotAdvanceLedger(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "false")
	db := versionThreeRetentionDB(t)
	before := retentionSQLiteChangeCounts(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Up(ctx, db, "sqlite"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled migration: %v", err)
	}
	status, err := Inspect(context.Background(), db, "sqlite")
	if err != nil || status.ActualVersion != 3 || status.State != StateBehind {
		t.Fatalf("canceled migration changed ledger: status=%#v err=%v", status, err)
	}
	if got := retentionSQLiteChangeCounts(t, db); got != before {
		t.Fatalf("canceled migration wrote database: before=%v after=%v", before, got)
	}
}

func retentionFTSSchema(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var objects []string
	if err := db.Raw("SELECT type || ':' || name || ':' || sql FROM sqlite_master WHERE name LIKE 'logs_fts%' OR name IN ('logs_ai','logs_ad','logs_au') ORDER BY name").Scan(&objects).Error; err != nil {
		t.Fatal(err)
	}
	return objects
}

func retentionSQLiteChangeCounts(t *testing.T, db *gorm.DB) [2]int64 {
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

func mustExecRetentionSQL(t *testing.T, db *gorm.DB, query string) {
	t.Helper()
	if err := db.Exec(query).Error; err != nil {
		t.Fatal(err)
	}
}
