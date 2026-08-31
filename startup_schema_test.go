package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/config"
	schemamigrate "github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func openStartupRepository(t *testing.T, path string, autoMigrate bool) *storage.Repository {
	t.Helper()
	t.Setenv("DB_DRIVER", "sqlite")
	t.Setenv("DB_DSN", path)
	if autoMigrate {
		t.Setenv("DB_AUTOMIGRATE", "true")
	} else {
		t.Setenv("DB_AUTOMIGRATE", "false")
	}
	repo, err := storage.NewRepository(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func sqliteSchemaObjects(t *testing.T, repo *storage.Repository) []string {
	t.Helper()
	var objects []string
	if err := repo.DB().Raw(`SELECT type || ':' || name FROM sqlite_master
WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`).Scan(&objects).Error; err != nil {
		t.Fatal(err)
	}
	return objects
}

func TestProductionStartupRefusalDoesNotWriteSchema(t *testing.T) {
	repo := openStartupRepository(t, filepath.Join(t.TempDir(), "empty.db"), false)
	before := sqliteSchemaObjects(t, repo)
	cfg := &config.Config{DBDriver: "sqlite", AggregateMode: aggregate.ModeLegacy}
	err := prepareDatabaseSchemas(context.Background(), cfg, repo)
	if err == nil || !strings.Contains(err.Error(), "state=empty") || !strings.Contains(err.Error(), "migrate up") {
		t.Fatalf("error = %v", err)
	}
	after := sqliteSchemaObjects(t, repo)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("schema changed on read-only refusal: before=%v after=%v", before, after)
	}
	if repo.DB().Migrator().HasTable(schemamigrate.LedgerTable) {
		t.Fatal("startup refusal created the migration ledger")
	}
}

func TestDevelopmentStartupUsesOneMainAndGraphRAGOwner(t *testing.T) {
	repo := openStartupRepository(t, filepath.Join(t.TempDir(), "development.db"), true)
	cfg := &config.Config{DBDriver: "sqlite", AggregateMode: aggregate.ModeLegacy}
	if err := prepareDatabaseSchemas(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"traces", "spans", "logs", "metric_buckets", "investigations", "drain_templates"} {
		if !repo.DB().Migrator().HasTable(table) {
			t.Fatalf("shared AutoMigrate missed %s", table)
		}
	}
	if repo.DB().Migrator().HasTable(schemamigrate.LedgerTable) {
		t.Fatal("development AutoMigrate claimed a versioned ledger")
	}
}

func TestProductionStartupAcceptsExactMainAndAggregateV5(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	db, err := storage.NewDatabase("sqlite", mainPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schemamigrate.Up(context.Background(), db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
	aggregatePath := filepath.Join(dir, "aggregate.db")
	store, err := aggregate.OpenSQLiteStore(aggregate.StoreConfig{Path: aggregatePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	repo := openStartupRepository(t, mainPath, false)
	cfg := &config.Config{DBDriver: "sqlite", AggregateMode: aggregate.ModeShadow, AggregateDBPath: aggregatePath}
	if err := prepareDatabaseSchemas(context.Background(), cfg, repo); err != nil {
		t.Fatal(err)
	}

	aggregateDB, err := sql.Open("sqlite", aggregatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aggregateDB.Exec(`UPDATE aggregate_meta SET value='4' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := aggregateDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepareDatabaseSchemas(context.Background(), cfg, repo); err == nil || !strings.Contains(err.Error(), "aggregate schema state=incompatible") {
		t.Fatalf("v4 without explicit rebuild error = %v", err)
	}
	cfg.AggregateAllowRebuild = true
	if err := prepareDatabaseSchemas(context.Background(), cfg, repo); err != nil {
		t.Fatalf("explicit aggregate rebuild: %v", err)
	}
	inspection, err := aggregate.InspectSQLiteStore(aggregatePath)
	if err != nil || !inspection.Usable() {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
}

func TestSchemaGatePrecedesEveryWorkerAndListenerInMain(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	gate := strings.Index(text, "prepareDatabaseSchemas(appCtx, cfg, repo)")
	if gate < 0 {
		t.Fatal("main does not call the schema gate")
	}
	for _, marker := range []string{
		"startPprofServer(cfg.PprofAddr, logger)",
		"partitionScheduler.Start(ctxPart)",
		"go hub.Run()",
		"go graphRAG.Start(ctxGraphRAG)",
	} {
		position := strings.Index(text, marker)
		if position < 0 || position < gate {
			t.Fatalf("%q position=%d must follow schema gate position=%d", marker, position, gate)
		}
	}
	if strings.Contains(text, "graphrag.AutoMigrateGraphRAG") {
		t.Fatal("main still has split GraphRAG migration ownership")
	}
}
