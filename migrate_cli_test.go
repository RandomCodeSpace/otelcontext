package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/config"
	schemamigrate "github.com/RandomCodeSpace/otelcontext/internal/migrate"
)

func configureMigrationCLI(t *testing.T, mainPath, mode, aggregatePath string) {
	t.Helper()
	t.Setenv("DB_DRIVER", "sqlite")
	t.Setenv("DB_DSN", mainPath)
	t.Setenv("AGGREGATE_MODE", mode)
	t.Setenv("AGGREGATE_DB_PATH", aggregatePath)
	t.Setenv("AGGREGATE_ALLOW_REBUILD", "true")
	t.Setenv("APP_ENV", "development")
}

func runMigrationCLIForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	handled, code := maybeRunMigrationCommand(args, &stdout, &stderr)
	if !handled {
		t.Fatalf("command was not handled: %v", args)
	}
	return code, stdout.String(), stderr.String()
}

func TestMigrationCLIStatusAndUpHaveStableOperatorOutput(t *testing.T) {
	dir := t.TempDir()
	configureMigrationCLI(t, filepath.Join(dir, "main.db"), aggregate.ModeLegacy, filepath.Join(dir, "aggregate.db"))

	code, stdout, stderr := runMigrationCLIForTest(t, "migrate", "status")
	if code != schemamigrate.ExitEmpty || stderr != "" || !strings.Contains(stdout, "main state=empty expected=2 actual=none") || !strings.Contains(stdout, "result=action-required") {
		t.Fatalf("empty status code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runMigrationCLIForTest(t, "migrate", "up")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "main state=exact expected=2 actual=2") || !strings.Contains(stdout, "result=ready") {
		t.Fatalf("up code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runMigrationCLIForTest(t, "migrate", "status")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "aggregate state=not-required") {
		t.Fatalf("exact status code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestMigrationCLIUpCreatesOnlyAnEmptyAggregateV5(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	aggregatePath := filepath.Join(dir, "aggregate.db")
	configureMigrationCLI(t, mainPath, aggregate.ModeShadow, aggregatePath)
	code, stdout, stderr := runMigrationCLIForTest(t, "migrate", "up")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "aggregate state=exact expected=5 actual=5") || !strings.Contains(stdout, "migration_result=created-v5") {
		t.Fatalf("up code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	db, err := sql.Open("sqlite", aggregatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE aggregate_meta SET value='4' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runMigrationCLIForTest(t, "migrate", "up")
	if code != schemamigrate.ExitIncompatible || !strings.Contains(stderr, "cannot be migrated losslessly") || !strings.Contains(stdout, "main state=exact") {
		t.Fatalf("v4 up code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	db, err = sql.Open("sqlite", aggregatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var version string
	if err := db.QueryRow(`SELECT value FROM aggregate_meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "4" {
		t.Fatalf("migrate up rebuilt aggregate v4 despite explicit policy: version=%s", version)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := aggregate.OpenSQLiteStore(aggregate.StoreConfig{Path: aggregatePath, AllowRebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runMigrationCLIForTest(t, "migrate", "up")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "main state=exact") || !strings.Contains(stdout, "aggregate state=exact") {
		t.Fatalf("partial rerun code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestMigrationCLIUsageAndNormalInvocationDispatch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if handled, _ := maybeRunMigrationCommand([]string{"--version"}, &stdout, &stderr); handled {
		t.Fatal("normal invocation was intercepted by migration dispatch")
	}
	mainPath := filepath.Join(t.TempDir(), "main.db")
	configureMigrationCLI(t, mainPath, aggregate.ModeLegacy, "")
	code, _, stderrText := runMigrationCLIForTest(t, "migrate", "baseline")
	if code != schemamigrate.ExitUsage || !strings.Contains(stderrText, migrationUsage) {
		t.Fatalf("usage code=%d stderr=%q", code, stderrText)
	}
	if _, err := os.Stat(mainPath); !os.IsNotExist(err) {
		t.Fatalf("invalid command opened or created the configured database: %v", err)
	}
	if err := versionedProfileError(&config.Config{DBDriver: "postgres", DBPostgresPartitioning: "daily"}); err == nil || !strings.Contains(err.Error(), "not promoted") {
		t.Fatalf("daily partition profile error = %v", err)
	}
}
