package migrate

import (
	"context"
	"reflect"
	"testing"
)

func TestSQLiteLocalPromQLUpgrade(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "false")
	db := newSQLiteMigrationDB(t)
	ctx := context.Background()
	r, err := newRunner("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if ok, err := r.applyNext(ctx, db, "sqlite"); err != nil || !ok {
			t.Fatalf("prepare v4: %v", err)
		}
	}
	if err := db.Exec(`INSERT INTO metric_buckets(tenant_id,name,service_name,time_bucket,min,max,sum,count) VALUES ('default','old','svc',CURRENT_TIMESTAMP,1,1,1,1)`).Error; err != nil {
		t.Fatal(err)
	}
	var before, after []map[string]any
	if err := db.Table("metric_buckets").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RequireExact(ctx, db, "sqlite"); err == nil {
		t.Fatal("old schema allowed")
	}
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if err := db.Table("metric_buckets").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("upgrade changed aggregate metric rows")
	}
	if err := db.Exec("PRAGMA query_only=ON").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RequireExact(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("PRAGMA query_only=OFF").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP INDEX idx_promql_samples_series_time").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RequireExact(ctx, db, "sqlite"); err == nil {
		t.Fatal("sample uniqueness index missing but startup passed")
	}
}
