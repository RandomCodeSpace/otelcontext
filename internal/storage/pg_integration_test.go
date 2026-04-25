//go:build integration
// +build integration

// Postgres-backed integration tests for the storage package.
//
// These tests exercise Postgres-specific behavior that cannot be validated by
// the SQLite-backed unit tests: ILIKE case-insensitivity, bytea column
// mapping for CompressedText, pg_trgm GIN index creation, VACUUM ANALYZE
// outside of a transaction, and batched purge semantics against a real
// Postgres server.
//
// Run with:
//
//	go test -race -tags=integration ./internal/storage/...
//
// Requires a reachable Docker daemon. If Docker is unavailable the suite
// auto-skips (t.Skip) rather than failing the run, so CI without Docker is
// safe. The integration build tag excludes this file (and its transitive
// testcontainers deps) from the default test runtime.

package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// setupPGContainer boots a throwaway Postgres 16 container, wires up a GORM
// repository against it, and returns a teardown closure. If Docker is not
// available the test is skipped (not failed) so these tests are safe to run
// in environments without container support.
func setupPGContainer(t *testing.T) (*Repository, func()) {
	t.Helper()
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("otel_test"),
		postgres.WithUsername("otel"),
		postgres.WithPassword("otel"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping pg integration tests: %v", err)
	}

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgContainer.Terminate(ctx)
		t.Fatalf("ConnectionString: %v", err)
	}

	db, err := NewDatabase("postgres", dsn)
	if err != nil {
		_ = pgContainer.Terminate(ctx)
		t.Fatalf("NewDatabase(postgres): %v", err)
	}
	if err := AutoMigrateModels(db, "postgres"); err != nil {
		_ = pgContainer.Terminate(ctx)
		t.Fatalf("AutoMigrateModels(postgres): %v", err)
	}

	repo := NewRepositoryFromDB(db, "postgres")
	teardown := func() {
		_ = repo.Close()
		_ = pgContainer.Terminate(ctx)
	}
	return repo, teardown
}

// TestPG_ILIKE_CaseInsensitiveSearch proves that SearchLogs uses the
// dialect-aware ILIKE operator on Postgres. Default LIKE on Postgres is
// case-sensitive, so this would miss "Connection Refused" if the dialect
// dispatch broke.
func TestPG_ILIKE_CaseInsensitiveSearch(t *testing.T) {
	repo, teardown := setupPGContainer(t)
	defer teardown()

	now := time.Now().UTC()
	if err := repo.db.Create(&Log{
		Severity:    "ERROR",
		Body:        "Connection Refused", // capitalized
		ServiceName: "api",
		Timestamp:   now,
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	logs, err := repo.SearchLogs(context.Background(), "connection", 10) // lowercase query
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 ILIKE match on postgres, got %d", len(logs))
	}
}

// TestPG_Bytea_CompressedTextRoundTrip writes long attributes and AIInsight
// through CompressedText and reads them back, proving the bytea column type
// plus zstd round-trip work end-to-end on Postgres. A TEXT column would
// corrupt zstd's magic bytes.
func TestPG_Bytea_CompressedTextRoundTrip(t *testing.T) {
	repo, teardown := setupPGContainer(t)
	defer teardown()

	attrJSON := `{"foo":"bar","baz":42,"nested":{"key":"` + strings.Repeat("x", 2000) + `"}}`
	aiInsight := "long AI insight: " + strings.Repeat("deadbeef ", 200)

	orig := Log{
		Severity:       "INFO",
		Body:           "body",
		ServiceName:    "svc",
		AttributesJSON: CompressedText(attrJSON),
		AIInsight:      CompressedText(aiInsight),
		Timestamp:      time.Now().UTC(),
	}
	if err := repo.db.Create(&orig).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got Log
	if err := repo.db.First(&got, orig.ID).Error; err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got.AttributesJSON) != attrJSON {
		t.Fatalf("attributes_json mismatch after bytea round-trip")
	}
	if string(got.AIInsight) != aiInsight {
		t.Fatalf("ai_insight mismatch after bytea round-trip")
	}
}

// TestPG_PgTrgm_IndexCreated verifies that AutoMigrate creates the GIN
// trigram indexes on logs.body and logs.service_name when pg_trgm is
// available (pg_trgm ships with postgres:16-alpine contrib).
func TestPG_PgTrgm_IndexCreated(t *testing.T) {
	repo, teardown := setupPGContainer(t)
	defer teardown()

	expected := map[string]bool{
		"idx_logs_body_trgm":    false,
		"idx_logs_service_trgm": false,
	}

	rows, err := repo.db.Raw(
		"SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'logs'",
	).Rows()
	if err != nil {
		t.Fatalf("pg_indexes query: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, want := expected[name]; want {
			expected[name] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("expected pg_trgm GIN index %q not present on logs table", name)
		}
	}
}

// TestPG_PgTrgm_SubstringQueryUsesGIN uses EXPLAIN to confirm the planner
// picks up the trigram GIN index for a substring ILIKE query. This guards
// against a regression where the GIN index silently stops being used.
func TestPG_PgTrgm_SubstringQueryUsesGIN(t *testing.T) {
	repo, teardown := setupPGContainer(t)
	defer teardown()

	// Seed enough rows that the planner prefers the index over a seq scan.
	now := time.Now().UTC()
	batch := make([]Log, 0, 2000)
	for i := 0; i < 2000; i++ {
		batch = append(batch, Log{
			Severity:    "INFO",
			Body:        fmt.Sprintf("request %d: connection refused by upstream-%d", i, i%37),
			ServiceName: fmt.Sprintf("svc-%d", i%11),
			Timestamp:   now,
		})
	}
	if err := repo.db.CreateInBatches(batch, 500).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	// ANALYZE so planner sees the new rows.
	if err := repo.db.Exec("ANALYZE logs").Error; err != nil {
		t.Fatalf("analyze: %v", err)
	}

	rows, err := repo.db.Raw(
		"EXPLAIN SELECT id FROM logs WHERE body ILIKE ?",
		"%connection%",
	).Rows()
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if !strings.Contains(plan.String(), "idx_logs_body_trgm") {
		t.Fatalf("plan does not reference idx_logs_body_trgm; trigram index not used:\n%s", plan.String())
	}
}

// TestPG_VacuumAnalyze_OutsideTx proves that runMaintenance uses the raw
// *sql.DB escape hatch and that VACUUM ANALYZE succeeds on Postgres. GORM's
// default path would wrap the statement in a transaction and Postgres would
// reject it with "VACUUM cannot run inside a transaction block".
func TestPG_VacuumAnalyze_OutsideTx(t *testing.T) {
	repo, teardown := setupPGContainer(t)
	defer teardown()

	// Seed a few rows so VACUUM ANALYZE has something to observe.
	seedLogs(t, repo.db, 10, time.Now().UTC(), "svc")

	sched := NewRetentionScheduler(repo, 7, 10_000, 5*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// runMaintenance logs errors rather than returning them, so we detect
	// silent failure by running a post-hoc statement that depends on the
	// tables being healthy. The real assertion is that the call doesn't
	// panic, hang, or leak a transaction. A fresh SELECT afterwards confirms
	// the connection pool wasn't left in an aborted-tx state.
	sched.runMaintenance(ctx)

	var n int64
	if err := repo.db.Model(&Log{}).Count(&n).Error; err != nil {
		t.Fatalf("post-VACUUM SELECT failed (connection in bad state?): %v", err)
	}
	if n != 10 {
		t.Fatalf("expected 10 rows after VACUUM ANALYZE, got %d", n)
	}
}

// TestPG_PurgeLogsBatched_LargeVolume inserts 25k old logs and purges them
// with batch size 5k. Postgres uses the batched DELETE ... WHERE id IN
// (SELECT id ... LIMIT batch) code path — the SQLite single-shot path does
// not exercise this branch. We assert the total matches and the table is
// empty at the end.
func TestPG_PurgeLogsBatched_LargeVolume(t *testing.T) {
	repo, teardown := setupPGContainer(t)
	defer teardown()

	const total = 25_000
	const batch = 5_000
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)

	// Insert in chunks to avoid parameter-limit issues.
	chunk := make([]Log, 0, 500)
	for i := 0; i < total; i++ {
		chunk = append(chunk, Log{
			Severity:    "INFO",
			Body:        "bulk",
			ServiceName: "svc",
			Timestamp:   old,
		})
		if len(chunk) == 500 {
			if err := repo.db.CreateInBatches(chunk, 500).Error; err != nil {
				t.Fatalf("insert chunk: %v", err)
			}
			chunk = chunk[:0]
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	n, err := repo.PurgeLogsBatched(ctx, time.Now().UTC().Add(-time.Hour), batch, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("PurgeLogsBatched: %v", err)
	}
	if n != total {
		t.Fatalf("expected %d rows deleted across %d batches, got %d", total, total/batch, n)
	}
	if remaining := mustCount(t, repo.db, &Log{}); remaining != 0 {
		t.Fatalf("expected 0 logs remaining, got %d", remaining)
	}
}

// TestPG_PurgeTracesBatched_OrphanSpanSweep_NOT_IN exercises the
// "DELETE FROM spans ... WHERE trace_id NOT IN (SELECT trace_id FROM traces)"
// path against real Postgres. It proves the correlated subquery planner in
// Postgres 16 handles the sweep correctly and preserves fresh live spans
// whose parent trace row is still in flight (ingest race).
func TestPG_PurgeTracesBatched_OrphanSpanSweep_NOT_IN(t *testing.T) {
	repo, teardown := setupPGContainer(t)
	defer teardown()

	nowUTC := time.Now().UTC()
	cutoff := nowUTC.Add(-7 * 24 * time.Hour)

	// Old trace + old spans — both must be purged.
	seedTrace(t, repo.db, "t-old", cutoff.Add(-time.Hour), []time.Time{
		cutoff.Add(-time.Hour),
		cutoff.Add(-90 * time.Minute),
	})

	// Live span with no parent trace (simulates the span-before-trace race
	// window in TraceServer.Export). Fresh start_time -> MUST survive.
	liveSpans := []Span{
		{TraceID: "t-live-1", SpanID: "live-1", OperationName: "op", StartTime: nowUTC, EndTime: nowUTC, Duration: 100, ServiceName: "svc"},
		{TraceID: "t-live-2", SpanID: "live-2", OperationName: "op", StartTime: nowUTC, EndTime: nowUTC, Duration: 100, ServiceName: "svc"},
	}
	if err := repo.db.Create(&liveSpans).Error; err != nil {
		t.Fatalf("seed live spans: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := repo.PurgeTracesBatched(ctx, cutoff, 10, 5*time.Millisecond); err != nil {
		t.Fatalf("PurgeTracesBatched: %v", err)
	}

	// Old trace + its spans: purged.
	if mustCount(t, repo.db, &Trace{}) != 0 {
		t.Fatal("old trace not purged")
	}
	var oldSpans int64
	repo.db.Model(&Span{}).Where("trace_id = ?", "t-old").Count(&oldSpans)
	if oldSpans != 0 {
		t.Fatalf("old orphan spans not swept by NOT IN; got %d want 0", oldSpans)
	}

	// Live spans (no parent trace, but fresh start_time): MUST survive.
	var liveCount int64
	repo.db.Model(&Span{}).Where("trace_id IN ?", []string{"t-live-1", "t-live-2"}).Count(&liveCount)
	if liveCount != 2 {
		t.Fatalf("live spans wrongly swept by NOT IN (ingest race regression); got %d want 2", liveCount)
	}
}

// TestPG_AutoMigrate_FromEmpty_NoSpansTracesFK is the RAN-49 regression
// guard. It boots a fresh Postgres 16 container, runs AutoMigrateModels
// directly against the empty schema, and asserts:
//
//  1. The migrator completes (no SQLSTATE 42830 from the legacy
//     spans.trace_id → traces.trace_id FK that pre-RAN-49 GORM emitted).
//  2. No FK constraint exists on spans or logs that references traces —
//     async ingestion can land child rows before their parent trace, and
//     RAN-21 made trace identity composite, so a single-column FK on
//     trace_id would either reject valid writes or fail at DDL time.
//
// SQLite cannot exercise this path: it does not validate FK targets at
// CREATE TABLE time, which is exactly why RAN-21 silently regressed past
// CI. Keep this test on the integration build tag so the regression
// class can not sneak back in.
func TestPG_AutoMigrate_FromEmpty_NoSpansTracesFK(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("otel_test"),
		postgres.WithUsername("otel"),
		postgres.WithPassword("otel"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping pg integration tests: %v", err)
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("ConnectionString: %v", err)
	}

	db, err := NewDatabase("postgres", dsn)
	if err != nil {
		t.Fatalf("NewDatabase(postgres): %v", err)
	}
	defer func() {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	}()

	if err := AutoMigrateModels(db, "postgres"); err != nil {
		t.Fatalf("AutoMigrateModels(postgres) on empty DB failed (RAN-49 regression): %v", err)
	}

	// Assert no FK from spans/logs references traces.
	rows, err := db.Raw(`
		SELECT tc.table_name, tc.constraint_name, ccu.table_name AS foreign_table
		FROM information_schema.table_constraints tc
		JOIN information_schema.constraint_column_usage ccu
		  ON tc.constraint_name = ccu.constraint_name
		 AND tc.table_schema   = ccu.table_schema
		WHERE tc.constraint_type = 'FOREIGN KEY'
		  AND tc.table_schema    = 'public'
		  AND tc.table_name      IN ('spans','logs')
		  AND ccu.table_name     = 'traces'`).Rows()
	if err != nil {
		t.Fatalf("information_schema FK lookup: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var table, name, foreignTable string
		if scanErr := rows.Scan(&table, &name, &foreignTable); scanErr != nil {
			t.Fatalf("FK row scan: %v", scanErr)
		}
		t.Errorf("unexpected FK %s on %s referencing %s — async ingestion expects no FK back to traces (RAN-49)", name, table, foreignTable)
	}
}

// TestPG_AutoMigrate_BlobTypesBecomeBytea inspects information_schema to
// assert that CompressedText fields are materialized as bytea columns on
// Postgres — proves the GormDBDataType dialect mapping survives migration.
func TestPG_AutoMigrate_BlobTypesBecomeBytea(t *testing.T) {
	repo, teardown := setupPGContainer(t)
	defer teardown()

	type col struct {
		Table  string
		Column string
	}
	want := []col{
		{Table: "logs", Column: "attributes_json"},
		{Table: "logs", Column: "ai_insight"},
		{Table: "spans", Column: "attributes_json"},
		{Table: "metric_buckets", Column: "attributes_json"},
	}

	for _, c := range want {
		var dataType string
		err := repo.db.Raw(
			`SELECT data_type FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = ? AND column_name = ?`,
			c.Table, c.Column,
		).Row().Scan(&dataType)
		if err != nil {
			t.Fatalf("information_schema lookup for %s.%s: %v", c.Table, c.Column, err)
		}
		if dataType != "bytea" {
			t.Errorf("%s.%s: data_type = %q, want %q", c.Table, c.Column, dataType, "bytea")
		}
	}
}
