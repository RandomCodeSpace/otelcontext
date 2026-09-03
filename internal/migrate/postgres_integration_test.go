//go:build integration

package migrate

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/gorm"
)

func startMigrationPostgres16(t *testing.T) (string, *gorm.DB) {
	t.Helper()
	if os.Getenv("OTELCONTEXT_TEST_DRIVER") == "postgres" && os.Getenv("OTELCONTEXT_TEST_DSN") != "" {
		dsn := os.Getenv("OTELCONTEXT_TEST_DSN")
		admin, err := storage.NewDatabase("postgres", dsn)
		if err != nil {
			t.Fatalf("connect required PostgreSQL workflow service: %v", err)
		}
		if sqlDB, dbErr := admin.DB(); dbErr == nil {
			t.Cleanup(func() { _ = sqlDB.Close() })
		}
		return dsn, admin
	}
	if os.Getenv("OTELCONTEXT_TEST_REQUIRE_DB") == "1" {
		t.Fatal("required PostgreSQL proof is missing OTELCONTEXT_TEST_DSN")
	}
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:16.15-alpine3.24@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685",
		postgres.WithDatabase("otel_migrate"),
		postgres.WithUsername("otel"),
		postgres.WithPassword("otel"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL 16: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := storage.NewDatabase("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := admin.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	return dsn, admin
}

func newPostgresMigrationDB(t *testing.T, baseDSN string, admin *gorm.DB, sequence *atomic.Int64) *gorm.DB {
	t.Helper()
	schema := fmt.Sprintf("migration_case_%d", sequence.Add(1))
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil { //nolint:gosec // generated identifier
		t.Fatal(err)
	}
	separator := "?"
	if strings.Contains(baseDSN, "?") {
		separator = "&"
	}
	db, err := storage.NewDatabase("postgres", baseDSN+separator+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := db.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	return db
}

func applyPostgresFixture(t *testing.T, db *gorm.DB, throughVersion int) {
	t.Helper()
	registry, err := registryFor("postgres")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range registry[:throughVersion] {
		for number, statement := range migrationStatements(entry.SQL) {
			if err := db.Exec(statement).Error; err != nil {
				t.Fatalf("fixture migration %d statement %d: %v", entry.Version, number+1, err)
			}
		}
	}
}

func TestPostgres16MigrationLifecycle(t *testing.T) {
	baseDSN, admin := startMigrationPostgres16(t)
	var sequence atomic.Int64
	t.Run("empty install and idempotent rerun", func(t *testing.T) {
		db := newPostgresMigrationDB(t, baseDSN, admin, &sequence)
		first, err := Up(context.Background(), db, "postgres")
		if err != nil {
			t.Fatal(err)
		}
		second, err := Up(context.Background(), db, "postgres")
		if err != nil {
			t.Fatal(err)
		}
		if first.State != StateExact || second.State != StateExact || first.Fingerprint != second.Fingerprint {
			t.Fatalf("first=%#v second=%#v", first, second)
		}
	})

	t.Run("stable baseline upgrade preserves main and GraphRAG rows", func(t *testing.T) {
		db := newPostgresMigrationDB(t, baseDSN, admin, &sequence)
		applyPostgresFixture(t, db, 1)
		if err := db.Exec(`INSERT INTO traces
(tenant_id,trace_id,service_name,duration,status,timestamp,created_at,updated_at)
VALUES ('default','pg-stable','checkout',42,'STATUS_CODE_ERROR',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO investigations
(tenant_id,id,created_at,status,severity,trigger_service)
VALUES ('','pg-inv',CURRENT_TIMESTAMP,'detected','warning','checkout')`).Error; err != nil {
			t.Fatal(err)
		}
		baseline, err := Baseline(context.Background(), db, "postgres", "v0.3.1")
		if err != nil {
			t.Fatal(err)
		}
		if baseline.Status.State != StateBehind || baseline.BeforeFingerprint != baseline.AfterFingerprint {
			t.Fatalf("baseline=%#v", baseline)
		}
		status, err := Up(context.Background(), db, "postgres")
		if err != nil || status.State != StateExact {
			t.Fatalf("status=%#v err=%v", status, err)
		}
		var traces, investigations int64
		if err := db.Table("traces").Where("trace_id = ?", "pg-stable").Count(&traces).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Table("investigations").Where("id = ? AND tenant_id = ?", "pg-inv", storage.DefaultTenantID).Count(&investigations).Error; err != nil {
			t.Fatal(err)
		}
		if traces != 1 || investigations != 1 {
			t.Fatalf("preserved traces=%d investigations=%d", traces, investigations)
		}
	})

	t.Run("prerelease baseline records version two behind current", func(t *testing.T) {
		db := newPostgresMigrationDB(t, baseDSN, admin, &sequence)
		applyPostgresFixture(t, db, 2)
		result, err := Baseline(context.Background(), db, "postgres", "v0.4.0-beta.2")
		if err != nil {
			t.Fatal(err)
		}
		if result.Status.State != StateBehind || result.RecordedVersion != 2 || result.BeforeFingerprint != result.AfterFingerprint {
			t.Fatalf("result=%#v", result)
		}
		status, err := Up(context.Background(), db, "postgres")
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		if status.State != StateExact || !db.Migrator().HasTable("resource_registry") {
			t.Fatalf("upgrade from beta baseline did not add resource_registry: %#v", status)
		}
	})

	t.Run("failed transaction rolls back and default registry reruns", func(t *testing.T) {
		db := newPostgresMigrationDB(t, baseDSN, admin, &sequence)
		registry, err := registryFor("postgres")
		if err != nil {
			t.Fatal(err)
		}
		registry[1].SQL += "\n-- migrate:split\nCREATE TABLE migration_probe (id bigint)\n-- migrate:split\nTHIS IS NOT SQL"
		r := &runner{registry: registry}
		if applied, err := r.applyNext(context.Background(), db, "postgres"); err != nil || !applied {
			t.Fatalf("v1 applied=%t err=%v", applied, err)
		}
		if _, err := r.applyNext(context.Background(), db, "postgres"); err == nil {
			t.Fatal("broken v2 unexpectedly succeeded")
		}
		if db.Migrator().HasTable("migration_probe") {
			t.Fatal("failed transaction left migration_probe")
		}
		status, err := Up(context.Background(), db, "postgres")
		if err != nil || status.State != StateExact {
			t.Fatalf("status=%#v err=%v", status, err)
		}
	})

	t.Run("advisory lock serializes concurrent runners", func(t *testing.T) {
		db := newPostgresMigrationDB(t, baseDSN, admin, &sequence)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		const callers = 4
		var wg sync.WaitGroup
		errs := make(chan error, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				status, err := Up(ctx, db, "postgres")
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
	})

	t.Run("versioned and shared AutoMigrate structures agree", func(t *testing.T) {
		versioned := newPostgresMigrationDB(t, baseDSN, admin, &sequence)
		if _, err := Up(context.Background(), versioned, "postgres"); err != nil {
			t.Fatal(err)
		}
		versionedFingerprint, err := SchemaFingerprint(context.Background(), versioned, "postgres", CurrentVersion)
		if err != nil {
			t.Fatal(err)
		}
		auto := newPostgresMigrationDB(t, baseDSN, admin, &sequence)
		if err := AutoMigrate(auto, "postgres", storage.MigrateOptions{}); err != nil {
			t.Fatal(err)
		}
		autoFingerprint, err := SchemaFingerprint(context.Background(), auto, "postgres", CurrentVersion)
		if err != nil {
			t.Fatal(err)
		}
		if versionedFingerprint != autoFingerprint {
			versionedLines, _ := schemaFingerprintLines(context.Background(), versioned, "postgres", CurrentVersion)
			autoLines, _ := schemaFingerprintLines(context.Background(), auto, "postgres", CurrentVersion)
			t.Fatalf("schema parity mismatch: versioned=%s automigrate=%s\nversioned:\n%s\nautomigrate:\n%s",
				versionedFingerprint, autoFingerprint, strings.Join(versionedLines, "\n"), strings.Join(autoLines, "\n"))
		}
	})
}
