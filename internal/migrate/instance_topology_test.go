package migrate

import (
	"context"
	"testing"
)

func TestSQLiteInstanceTopologyUpgrade(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "false")
	db := newSQLiteMigrationDB(t)
	ctx := context.Background()
	r, err := newRunner("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if ok, err := r.applyNext(ctx, db, "sqlite"); err != nil || !ok {
			t.Fatalf("prepare v5: %v", err)
		}
	}
	if err := db.Exec("INSERT INTO promql_series VALUES ('unchanged','tenant','metric','[]',42)").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RequireExact(ctx, db, "sqlite"); err == nil {
		t.Fatal("v5 startup accepted without topology tables")
	}
	if _, err := Up(ctx, db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := db.Raw("SELECT id FROM promql_series").Scan(&id).Error; err != nil || id != "unchanged" {
		t.Fatal("existing PromQL series changed", err)
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
	if err := db.Exec("DROP INDEX idx_topology_buckets_window").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RequireExact(ctx, db, "sqlite"); err == nil {
		t.Fatal("retention index missing but startup passed")
	}
}
