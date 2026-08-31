package migrate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

func newSQLiteMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := storage.NewDatabase("sqlite", filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func applySQLiteFixture(t *testing.T, db *gorm.DB, throughVersion int) {
	t.Helper()
	registry, err := registryFor("sqlite")
	if err != nil {
		t.Fatalf("registryFor: %v", err)
	}
	for _, entry := range registry[:throughVersion] {
		for number, statement := range migrationStatements(entry.SQL) {
			if err := db.Exec(statement).Error; err != nil {
				t.Fatalf("fixture migration %d statement %d: %v", entry.Version, number+1, err)
			}
		}
	}
}

func TestSQLiteUpEmptyAndIdempotent(t *testing.T) {
	db := newSQLiteMigrationDB(t)
	before, err := Inspect(context.Background(), db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if before.State != StateEmpty || before.ExitCode() != ExitEmpty {
		t.Fatalf("before = %#v", before)
	}

	first, err := Up(context.Background(), db, "sqlite")
	if err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if first.State != StateExact || first.ActualVersion != CurrentVersion || len(first.Applied) != CurrentVersion {
		t.Fatalf("first status = %#v", first)
	}
	second, err := Up(context.Background(), db, "sqlite")
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if second.Fingerprint != first.Fingerprint {
		t.Fatalf("idempotent fingerprint changed: %s -> %s", first.Fingerprint, second.Fingerprint)
	}
	var ledgerRows int64
	if err := db.Table(LedgerTable).Count(&ledgerRows).Error; err != nil {
		t.Fatal(err)
	}
	if ledgerRows != CurrentVersion {
		t.Fatalf("ledger rows = %d, want %d", ledgerRows, CurrentVersion)
	}
}

func TestSQLiteStableBaselineUpgradePreservesData(t *testing.T) {
	db := newSQLiteMigrationDB(t)
	applySQLiteFixture(t, db, 1)
	if err := db.Exec(`INSERT INTO traces
(tenant_id, trace_id, service_name, duration, status, timestamp, created_at, updated_at)
VALUES ('default', 'trace-stable', 'checkout', 42, 'STATUS_CODE_ERROR', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO investigations
(tenant_id, id, created_at, status, severity, trigger_service)
VALUES ('', 'inv-stable', CURRENT_TIMESTAMP, 'detected', 'warning', 'checkout')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO drain_templates
(tenant_id, id, tokens, count, first_seen, last_seen, sample)
VALUES ('', 7, '["failed"]', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'failed')`).Error; err != nil {
		t.Fatal(err)
	}

	baseline, err := Baseline(context.Background(), db, "sqlite", "v0.3.1")
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if baseline.Status.State != StateBehind || baseline.RecordedVersion != 1 {
		t.Fatalf("baseline = %#v", baseline)
	}
	if baseline.BeforeFingerprint != baseline.AfterFingerprint {
		t.Fatalf("baseline changed schema: %s -> %s", baseline.BeforeFingerprint, baseline.AfterFingerprint)
	}

	status, err := Up(context.Background(), db, "sqlite")
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if status.State != StateExact {
		t.Fatalf("status = %#v", status)
	}
	var traceCount, graphRows int64
	if err := db.Table("traces").Where("trace_id = ?", "trace-stable").Count(&traceCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("investigations").Where("id = ? AND tenant_id = ?", "inv-stable", storage.DefaultTenantID).Count(&graphRows).Error; err != nil {
		t.Fatal(err)
	}
	if traceCount != 1 || graphRows != 1 {
		t.Fatalf("preserved trace=%d graph_row=%d", traceCount, graphRows)
	}
	if !db.Migrator().HasColumn("traces", "truncated") || !db.Migrator().HasIndex("spans", "idx_spans_tenant_status_start") {
		t.Fatal("upgrade did not add the current trace columns and span status index")
	}
}

func TestSQLiteBetaBaselineIsProductSchemaNoOp(t *testing.T) {
	db := newSQLiteMigrationDB(t)
	applySQLiteFixture(t, db, CurrentVersion)
	result, err := Baseline(context.Background(), db, "sqlite", "v0.4.0-beta.2")
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if result.Status.State != StateExact || result.BeforeFingerprint != result.AfterFingerprint {
		t.Fatalf("result = %#v", result)
	}
}

func TestSQLiteBaselineRejectsTheWrongPublishedRelease(t *testing.T) {
	db := newSQLiteMigrationDB(t)
	applySQLiteFixture(t, db, CurrentVersion)
	result, err := Baseline(context.Background(), db, "sqlite", "v0.3.1")
	var stateErr *StateError
	if !errors.As(err, &stateErr) || result.Status.State != StateIncompatible || !strings.Contains(result.Status.Detail, "does not match frozen baseline") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if db.Migrator().HasTable(LedgerTable) {
		t.Fatal("rejected baseline wrote a ledger")
	}
}

func TestSQLiteStatusStatesAndExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(*testing.T, *gorm.DB)
		state    State
		exitCode int
		detail   string
	}{
		{name: "empty", state: StateEmpty, exitCode: ExitEmpty, detail: "no OtelContext"},
		{name: "unmanaged", prepare: func(t *testing.T, db *gorm.DB) {
			applySQLiteFixture(t, db, 1)
		}, state: StateUnmanaged, exitCode: ExitUnmanaged, detail: "without a migration ledger"},
		{name: "behind", prepare: func(t *testing.T, db *gorm.DB) {
			applySQLiteFixture(t, db, 1)
			if _, err := Baseline(context.Background(), db, "sqlite", "v0.3.1"); err != nil {
				t.Fatal(err)
			}
		}, state: StateBehind, exitCode: ExitBehind, detail: "behind"},
		{name: "ahead", prepare: func(t *testing.T, db *gorm.DB) {
			if _, err := Up(context.Background(), db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec(`INSERT INTO otelcontext_schema_migrations
(version,name,checksum,started_at,completed_at,dirty)
VALUES (3,'future',?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,0)`, strings.Repeat("a", 64)).Error; err != nil {
				t.Fatal(err)
			}
		}, state: StateAhead, exitCode: ExitAhead, detail: "ahead"},
		{name: "dirty", prepare: func(t *testing.T, db *gorm.DB) {
			if _, err := Up(context.Background(), db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec(`UPDATE otelcontext_schema_migrations SET dirty=1, completed_at=NULL WHERE version=2`).Error; err != nil {
				t.Fatal(err)
			}
		}, state: StateDirty, exitCode: ExitDirty, detail: "incomplete"},
		{name: "checksum", prepare: func(t *testing.T, db *gorm.DB) {
			if _, err := Up(context.Background(), db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec(`UPDATE otelcontext_schema_migrations SET checksum=? WHERE version=2`, strings.Repeat("b", 64)).Error; err != nil {
				t.Fatal(err)
			}
		}, state: StateIncompatible, exitCode: ExitIncompatible, detail: "checksum mismatch"},
		{name: "structure", prepare: func(t *testing.T, db *gorm.DB) {
			if _, err := Up(context.Background(), db, "sqlite"); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec(`DROP INDEX idx_spans_tenant_status_start`).Error; err != nil {
				t.Fatal(err)
			}
		}, state: StateIncompatible, exitCode: ExitIncompatible, detail: "missing index"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newSQLiteMigrationDB(t)
			if test.prepare != nil {
				test.prepare(t, db)
			}
			first, err := Inspect(context.Background(), db, "sqlite")
			if err != nil {
				t.Fatal(err)
			}
			second, err := Inspect(context.Background(), db, "sqlite")
			if err != nil {
				t.Fatal(err)
			}
			if first.State != test.state || first.ExitCode() != test.exitCode || !strings.Contains(first.Detail, test.detail) {
				t.Fatalf("status = %#v", first)
			}
			if first.Description() != second.Description() {
				t.Fatalf("nondeterministic status:\n%s\n%s", first.Description(), second.Description())
			}
		})
	}
}

func TestSQLiteFailedMigrationRollsBackAndCanRerun(t *testing.T) {
	db := newSQLiteMigrationDB(t)
	registry, err := registryFor("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	registry[1].SQL += "\n-- migrate:split\nCREATE TABLE migration_probe (id integer)\n-- migrate:split\nTHIS IS NOT SQL"
	r := &runner{registry: registry}
	if applied, err := r.applyNext(context.Background(), db, "sqlite"); err != nil || !applied {
		t.Fatalf("apply v1: applied=%t err=%v", applied, err)
	}
	if _, err := r.applyNext(context.Background(), db, "sqlite"); err == nil {
		t.Fatal("broken v2 unexpectedly succeeded")
	}
	if db.Migrator().HasTable("migration_probe") {
		t.Fatal("failed transactional migration left migration_probe behind")
	}
	var rows int64
	if err := db.Table(LedgerTable).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("ledger rows after rollback = %d, want 1", rows)
	}
	status, err := Up(context.Background(), db, "sqlite")
	if err != nil || status.State != StateExact {
		t.Fatalf("rerun status=%#v err=%v", status, err)
	}
}

func TestSQLiteConcurrentUpUsesOneOrderedLedger(t *testing.T) {
	db := newSQLiteMigrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const callers = 4
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, err := Up(ctx, db, "sqlite")
			if err != nil {
				errs <- err
				return
			}
			if status.State != StateExact {
				errs <- fmt.Errorf("state=%s", status.State)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var rows int64
	if err := db.Table(LedgerTable).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != CurrentVersion {
		t.Fatalf("ledger rows = %d", rows)
	}
}

func TestSQLiteVersionedInstallMatchesSharedAutoMigrateContract(t *testing.T) {
	versioned := newSQLiteMigrationDB(t)
	if _, err := Up(context.Background(), versioned, "sqlite"); err != nil {
		t.Fatal(err)
	}
	versionedFingerprint, err := SchemaFingerprint(context.Background(), versioned, "sqlite", CurrentVersion)
	if err != nil {
		t.Fatal(err)
	}

	automigrated := newSQLiteMigrationDB(t)
	if err := AutoMigrate(automigrated, "sqlite", storage.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	if automigrated.Migrator().HasTable(LedgerTable) {
		t.Fatal("development AutoMigrate must not claim a versioned ledger")
	}
	autoFingerprint, err := SchemaFingerprint(context.Background(), automigrated, "sqlite", CurrentVersion)
	if err != nil {
		t.Fatal(err)
	}
	if versionedFingerprint != autoFingerprint {
		t.Fatalf("schema parity mismatch: versioned=%s automigrate=%s", versionedFingerprint, autoFingerprint)
	}
}

func TestRegistryAndPreviewDriverContracts(t *testing.T) {
	registry, err := registryFor("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	broken := append([]migration(nil), registry...)
	broken[1].Version = 1
	if err := validateRegistry(broken); err == nil {
		t.Fatal("duplicate/gapped registry accepted")
	}
	status, err := Inspect(context.Background(), nil, "mysql")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateUnverified || status.ExitCode() != ExitUnverified || !strings.Contains(status.Detail, "DB_AUTOMIGRATE=true") {
		t.Fatalf("preview status = %#v", status)
	}
}
