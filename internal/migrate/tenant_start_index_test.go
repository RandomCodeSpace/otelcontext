package migrate

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSQLiteTenantStartIndexUpgradePreservesData(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "false")
	ctx := context.Background()
	db := newSQLiteMigrationDB(t)
	r, err := newRunner("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if applied, err := r.applyNext(ctx, db, "sqlite"); err != nil || !applied {
			t.Fatalf("prepare version 6: applied=%v err=%v", applied, err)
		}
	}
	stamp := time.Date(2026, 10, 20, 4, 0, 0, 123456789, time.UTC)
	if err := db.Exec(`INSERT INTO traces (tenant_id, trace_id, service_name, timestamp, retained_span_count)
VALUES ('alpha', 't1', 'api', ?, 1), ('beta', 't2', 'worker', ?, 1)`, stamp, stamp).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO spans (tenant_id, trace_id, span_id, start_time, attributes_json)
VALUES ('alpha', 't1', 's1', ?, '{"preserve":"payload"}'), ('beta', 't2', 's2', ?, NULL)`, stamp, stamp).Error; err != nil {
		t.Fatal(err)
	}
	readRows := func() map[string][]map[string]any {
		t.Helper()
		out := make(map[string][]map[string]any)
		for _, table := range []string{"traces", "spans"} {
			var rows []map[string]any
			if err := db.Table(table).Order("id").Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			out[table] = rows
		}
		return out
	}
	before, err := Inspect(ctx, db, "sqlite")
	if err != nil || before.State != StateBehind || before.ActualVersion != 6 {
		t.Fatalf("version 6 schema: status=%#v err=%v", before, err)
	}
	rowsBefore := readRows()
	if status, err := RequireExact(ctx, db, "sqlite"); err == nil || status.State != StateBehind {
		t.Fatalf("version 6 startup must refuse without migration: status=%#v err=%v", status, err)
	}
	after, err := Up(ctx, db, "sqlite")
	if err != nil || after.ActualVersion != CurrentVersion || after.State != StateExact {
		t.Fatalf("upgrade: status=%#v err=%v", after, err)
	}
	if !reflect.DeepEqual(before.Applied, after.Applied[:6]) {
		t.Fatal("upgrade changed migration 1-6 ledger entries")
	}
	if !reflect.DeepEqual(rowsBefore, readRows()) {
		t.Fatal("index migration changed rows")
	}
	var plan []struct{ Detail string }
	if err := db.Raw(`EXPLAIN QUERY PLAN SELECT span_id FROM spans WHERE start_time > ? AND tenant_id = ? ORDER BY start_time LIMIT 10`, stamp, "alpha").Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !strings.Contains(plan[0].Detail, "SEARCH spans USING INDEX idx_spans_tenant_start (tenant_id=? AND start_time>?)") {
		t.Fatalf("tenant window must seek the tenant/start index without a sort: %v", plan)
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
	mustExecFTS(t, db, "PRAGMA query_only=OFF")

	mustExecFTS(t, db, "DROP INDEX idx_spans_tenant_start")
	mustExecFTS(t, db, "CREATE INDEX idx_spans_tenant_start ON spans(tenant_id)")
	if status, err := RequireExact(ctx, db, "sqlite"); err == nil || status.State != StateIncompatible || !strings.Contains(status.Detail, "idx_spans_tenant_start") {
		t.Fatalf("wrong tenant/start index accepted: status=%#v err=%v", status, err)
	}
}
