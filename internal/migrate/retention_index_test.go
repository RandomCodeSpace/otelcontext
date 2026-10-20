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
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := withLockedTransaction(ctx, sqlDB, "sqlite", func(tx sqlExecutor) error {
		return storage.EnsureSQLiteLogFTS(ctx, tx)
	}); err != nil {
		t.Fatal(err)
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
	rowsBefore, ftsBefore := readRows(), ftsSchema(t, db)
	mustExecFTS(t, db, "PRAGMA query_only=ON")
	if status, err := RequireExact(ctx, db, "sqlite"); err == nil || status.State != StateBehind {
		t.Fatalf("version 3 startup must refuse without migration: status=%#v err=%v", status, err)
	}
	mustExecFTS(t, db, "PRAGMA query_only=OFF")
	after, err := Up(ctx, db, "sqlite")
	if err != nil || after.ActualVersion != 4 || after.State != StateExact {
		t.Fatalf("upgrade: status=%#v err=%v", after, err)
	}
	if !reflect.DeepEqual(before.Applied, after.Applied[:3]) {
		t.Fatal("upgrade changed migration 1-3 ledger entries")
	}
	if !reflect.DeepEqual(rowsBefore, readRows()) || !reflect.DeepEqual(ftsBefore, ftsSchema(t, db)) {
		t.Fatal("index migration changed rows or FTS definitions")
	}
	assertLogSearch(t, storage.NewRepositoryFromDB(db, "sqlite"), "alpha", "runs", log.ID)
	var plan []struct{ Detail string }
	if err := db.Raw(`EXPLAIN QUERY PLAN SELECT id FROM spans WHERE start_time < ? ORDER BY start_time, id LIMIT ?`, stamp, 100).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !strings.Contains(plan[0].Detail, "SEARCH spans USING COVERING INDEX idx_spans_start_time") {
		t.Fatalf("age retention must use the covering time index without a sort: %v", plan)
	}
	changes := sqliteChangeCounts(t, db)
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	mustExecFTS(t, db, "PRAGMA query_only=ON")
	if _, err := RequireExact(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if got := sqliteChangeCounts(t, db); got != changes {
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
			mustExecFTS(t, db, "DROP INDEX idx_spans_start_time")
			if replacement != "" {
				mustExecFTS(t, db, replacement)
			}
			before := sqliteChangeCounts(t, db)
			mustExecFTS(t, db, "PRAGMA query_only=ON")
			status, err := RequireExact(context.Background(), db, "sqlite")
			if err == nil || status.State != StateIncompatible || !strings.Contains(status.Detail, "idx_spans_start_time") {
				t.Fatalf("invalid retention index accepted: status=%#v err=%v", status, err)
			}
			if status, err := Up(context.Background(), db, "sqlite"); err == nil || status.State != StateIncompatible {
				t.Fatalf("invalid index migration must refuse: status=%#v err=%v", status, err)
			}
			if got := sqliteChangeCounts(t, db); got != before {
				t.Fatalf("startup repaired schema: before=%v after=%v", before, got)
			}
		})
	}
}

func TestSQLiteRetentionIndexCanceledUpgradeDoesNotAdvanceLedger(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "false")
	db := versionThreeRetentionDB(t)
	before := sqliteChangeCounts(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Up(ctx, db, "sqlite"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled migration: %v", err)
	}
	status, err := Inspect(context.Background(), db, "sqlite")
	if err != nil || status.ActualVersion != 3 || status.State != StateBehind {
		t.Fatalf("canceled migration changed ledger: status=%#v err=%v", status, err)
	}
	if got := sqliteChangeCounts(t, db); got != before {
		t.Fatalf("canceled migration wrote database: before=%v after=%v", before, got)
	}
}
