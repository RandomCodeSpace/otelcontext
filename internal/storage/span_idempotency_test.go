package storage

import (
	"context"
	"testing"
	"time"
)

// TestBatchCreateSpans_DuplicateInsertNoOp verifies that re-submitting a span
// with the same (tenant, trace, span_id) is silently absorbed — the second
// insert MUST NOT create a duplicate row, must not return an error, and must
// not overwrite the original. This is the contract DLQ replay relies on.
func TestBatchCreateSpans_DuplicateInsertNoOp(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC().Truncate(time.Second)

	first := Span{
		TenantID:      "acme",
		TraceID:       "trace-1",
		SpanID:        "span-a",
		OperationName: "first",
		StartTime:     now,
		EndTime:       now.Add(time.Millisecond),
		Duration:      1000,
		ServiceName:   "svc",
	}
	if err := repo.BatchCreateSpans([]Span{first}); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Replay with the same dedupe key but a different OperationName proves
	// OnConflict.DoNothing semantics (NOT DoUpdate) — the original row wins.
	replay := first
	replay.OperationName = "second-attempt"
	if err := repo.BatchCreateSpans([]Span{replay}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if got := mustCount(t, repo.db, &Span{}); got != 1 {
		t.Fatalf("expected 1 span after replay, got %d", got)
	}

	var stored Span
	if err := repo.db.Where("tenant_id = ? AND trace_id = ? AND span_id = ?", "acme", "trace-1", "span-a").First(&stored).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.OperationName != "first" {
		t.Fatalf("DoNothing violated: stored op=%q want %q", stored.OperationName, "first")
	}
}

// TestBatchCreateSpans_CrossTenantSameKeyAllowed verifies tenant scope of the
// uniqueIndex — the same (trace_id, span_id) under a different tenant inserts
// cleanly. Without this, multi-tenant correlation by span_id would conflate
// rows across tenants on first ingest.
func TestBatchCreateSpans_CrossTenantSameKeyAllowed(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()

	mk := func(tenant string) Span {
		return Span{
			TenantID:      tenant,
			TraceID:       "shared-trace",
			SpanID:        "shared-span",
			OperationName: "op-" + tenant,
			StartTime:     now,
			EndTime:       now.Add(time.Millisecond),
			ServiceName:   "svc-" + tenant,
		}
	}
	if err := repo.BatchCreateSpans([]Span{mk("acme"), mk("beta")}); err != nil {
		t.Fatalf("cross-tenant insert: %v", err)
	}

	if got := mustCount(t, repo.db, &Span{}); got != 2 {
		t.Fatalf("expected 2 spans (one per tenant), got %d", got)
	}
}

// TestBatchCreateAll_SpanReplayIdempotent covers the same DLQ replay scenario
// through the transactional BatchCreateAll path used by the async ingest
// pipeline. Submitting an entire (traces, spans, logs) batch twice must not
// inflate trace or span counts.
func TestBatchCreateAll_SpanReplayIdempotent(t *testing.T) {
	repo := newTestRepo(t)
	ctx := WithTenantContext(context.Background(), "acme")
	now := time.Now().UTC()

	traces := []Trace{{TenantID: "acme", TraceID: "tr-1", ServiceName: "svc", Duration: 100, Status: "OK", Timestamp: now}}
	spans := []Span{
		{TenantID: "acme", TraceID: "tr-1", SpanID: "sp-1", OperationName: "op", StartTime: now, EndTime: now, ServiceName: "svc"},
		{TenantID: "acme", TraceID: "tr-1", SpanID: "sp-2", OperationName: "op", StartTime: now, EndTime: now, ServiceName: "svc"},
	}
	logs := []Log{{TenantID: "acme", TraceID: "tr-1", SpanID: "sp-1", Severity: "INFO", Body: "hi", ServiceName: "svc", Timestamp: now}}

	if err := repo.BatchCreateAll(traces, spans, logs); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	// Mimic DLQ replay: rows come from JSON deserialization without
	// auto-assigned primary keys. Without this reset GORM would try to
	// re-insert rows with explicit IDs and trip the PK constraint —
	// distinct from the (tenant, trace, span_id) idempotency we're testing.
	traces2 := append([]Trace(nil), traces...)
	spans2 := append([]Span(nil), spans...)
	logs2 := append([]Log(nil), logs...)
	for i := range traces2 {
		traces2[i].ID = 0
	}
	for i := range spans2 {
		spans2[i].ID = 0
	}
	for i := range logs2 {
		logs2[i].ID = 0
	}
	if err := repo.BatchCreateAll(traces2, spans2, logs2); err != nil {
		t.Fatalf("replay batch: %v", err)
	}

	tr, err := repo.GetTrace(ctx, "tr-1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if got := mustCount(t, repo.db, &Trace{}); got != 1 {
		t.Fatalf("traces inflated by replay: got %d, want 1", got)
	}
	if got := mustCount(t, repo.db, &Span{}); got != 2 {
		t.Fatalf("spans inflated by replay: got %d, want 2", got)
	}
	if len(tr.Spans) != 2 {
		t.Fatalf("preloaded span count: got %d, want 2", len(tr.Spans))
	}
}

// TestDedupeSpansForUniqueIndex_RemovesPreExistingDuplicates simulates an
// upgrade from a pre-RAN-65 deployment that accumulated duplicate spans
// from DLQ replays. The dedupe migration must collapse them BEFORE the
// uniqueIndex creation so AutoMigrate succeeds.
func TestDedupeSpansForUniqueIndex_RemovesPreExistingDuplicates(t *testing.T) {
	// Build an unmigrated DB, create the spans table WITHOUT the unique
	// index (mirrors the legacy schema), seed duplicates, then run the
	// dedupe helper directly.
	db, err := NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() })

	if err := db.Exec(`CREATE TABLE spans (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		trace_id TEXT NOT NULL,
		span_id TEXT NOT NULL,
		parent_span_id TEXT,
		operation_name TEXT,
		start_time DATETIME,
		end_time DATETIME,
		duration INTEGER,
		service_name TEXT,
		status TEXT DEFAULT 'STATUS_CODE_UNSET',
		attributes_json BLOB
	)`).Error; err != nil {
		t.Fatalf("create legacy spans table: %v", err)
	}

	insert := func(tenant, trace, span, op string) {
		if err := db.Exec(
			`INSERT INTO spans (tenant_id, trace_id, span_id, operation_name) VALUES (?, ?, ?, ?)`,
			tenant, trace, span, op,
		).Error; err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insert("acme", "tr-1", "sp-1", "first")
	insert("acme", "tr-1", "sp-1", "dup-second") // dup
	insert("acme", "tr-1", "sp-1", "dup-third")  // dup
	insert("acme", "tr-1", "sp-2", "unique")
	insert("beta", "tr-1", "sp-1", "cross-tenant-keep")

	if err := dedupeSpansForUniqueIndex(db, "sqlite"); err != nil {
		t.Fatalf("dedupeSpansForUniqueIndex: %v", err)
	}

	type row struct {
		ID            int
		OperationName string
	}
	var rows []row
	if err := db.Raw(`SELECT id, operation_name FROM spans ORDER BY id`).Scan(&rows).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	// Three rows survive: first acme/tr-1/sp-1 (lowest id), acme/tr-1/sp-2,
	// beta/tr-1/sp-1. The two duplicate rows for acme/tr-1/sp-1 are gone.
	if len(rows) != 3 {
		t.Fatalf("post-dedupe row count: got %d, want 3 (rows=%+v)", len(rows), rows)
	}
	for _, r := range rows {
		if r.OperationName == "dup-second" || r.OperationName == "dup-third" {
			t.Fatalf("duplicate row survived: %+v", r)
		}
	}
}

// TestDedupeSpansForUniqueIndex_NoOpOnFreshDB verifies the dedupe is a safe
// no-op when the spans table doesn't exist yet (greenfield startup) — the
// helper must NOT fail in that case.
func TestDedupeSpansForUniqueIndex_NoOpOnFreshDB(t *testing.T) {
	db, err := NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() })

	if err := dedupeSpansForUniqueIndex(db, "sqlite"); err != nil {
		t.Fatalf("dedupe on fresh DB: %v", err)
	}
}

// TestDedupeSpansForUniqueIndex_NoOpOnceIndexExists guards against re-running
// the (potentially expensive) dedupe scan on every restart of an already-
// migrated database.
func TestDedupeSpansForUniqueIndex_NoOpOnceIndexExists(t *testing.T) {
	repo := newTestRepo(t)
	if !repo.db.Migrator().HasIndex("spans", "idx_spans_tenant_trace_span") {
		t.Fatalf("test prerequisite: uniqueIndex idx_spans_tenant_trace_span should be present after AutoMigrate")
	}
	// Stand up a sentinel: a non-conforming raw row that the dedupe SQL
	// would normally remove. If the helper short-circuits on HasIndex,
	// the row stays. We can't actually insert a duplicate (the unique
	// index blocks us), so just re-run the helper and confirm no error.
	if err := dedupeSpansForUniqueIndex(repo.db, "sqlite"); err != nil {
		t.Fatalf("re-run on migrated DB: %v", err)
	}
	// And verify HasIndex is still true (sanity).
	if !repo.db.Migrator().HasIndex("spans", "idx_spans_tenant_trace_span") {
		t.Fatalf("uniqueIndex disappeared after dedupe call")
	}
}

// TestAutoMigrate_AddsSpanUniqueIndex covers the full AutoMigrate path —
// after the migration runs against a fresh DB, the composite uniqueIndex
// must be present. Belt-and-braces against a future refactor that
// accidentally drops the gorm tag.
func TestAutoMigrate_AddsSpanUniqueIndex(t *testing.T) {
	db, err := NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	t.Cleanup(func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() })

	if err := AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	if !db.Migrator().HasIndex("spans", "idx_spans_tenant_trace_span") {
		t.Fatalf("expected uniqueIndex idx_spans_tenant_trace_span after AutoMigrate")
	}
	// Verify it's actually unique (not just a regular index) by attempting
	// a violating insert.
	now := time.Now().UTC()
	mk := Span{TenantID: "t1", TraceID: "tr", SpanID: "sp", OperationName: "op", StartTime: now, EndTime: now, ServiceName: "svc"}
	if err := db.Create(&mk).Error; err != nil {
		t.Fatalf("first insert: %v", err)
	}
	dup := mk
	dup.ID = 0 // let GORM assign
	dup.OperationName = "dup"
	err = db.Create(&dup).Error
	if err == nil {
		t.Fatalf("expected unique-constraint violation on duplicate span insert, got nil")
	}
	// Any non-nil error proves the unique index is enforcing — we don't pin
	// to a specific error string because driver wording varies.
}
