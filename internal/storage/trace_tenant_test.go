package storage

import (
	"sort"
	"testing"
	"time"
)

// TestCreateTrace_SameTraceIDAcrossTenants_Succeeds proves that identical
// trace_ids under distinct tenants coexist after RAN-21. Before the fix the
// second insert silently collapsed into a no-op via OnConflict.DoNothing
// because the old standalone uniqueIndex on trace_id matched regardless of
// tenant.
func TestCreateTrace_SameTraceIDAcrossTenants_Succeeds(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	traceID := "shared-trace-id-0001"

	acme := Trace{TenantID: "acme", TraceID: traceID, ServiceName: "svc-a", Status: "OK", Timestamp: now}
	beta := Trace{TenantID: "beta", TraceID: traceID, ServiceName: "svc-b", Status: "OK", Timestamp: now}

	if err := repo.CreateTrace(acme); err != nil {
		t.Fatalf("CreateTrace(acme): %v", err)
	}
	if err := repo.CreateTrace(beta); err != nil {
		t.Fatalf("CreateTrace(beta): %v", err)
	}

	var rows []Trace
	if err := repo.db.Where("trace_id = ?", traceID).Find(&rows).Error; err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("expected %d trace rows for shared trace_id, got %d", want, got)
	}
	seenTenants := map[string]string{}
	for _, r := range rows {
		seenTenants[r.TenantID] = r.ServiceName
	}
	if seenTenants["acme"] != "svc-a" || seenTenants["beta"] != "svc-b" {
		t.Fatalf("tenant→service mismatch: %+v", seenTenants)
	}

	acmeCtx := WithTenantContext(t.Context(), "acme")
	betaCtx := WithTenantContext(t.Context(), "beta")
	got, err := repo.GetTrace(acmeCtx, traceID)
	if err != nil {
		t.Fatalf("GetTrace(acme): %v", err)
	}
	if got.ServiceName != "svc-a" || got.TenantID != "acme" {
		t.Fatalf("GetTrace(acme) returned wrong row: %+v", got)
	}
	got, err = repo.GetTrace(betaCtx, traceID)
	if err != nil {
		t.Fatalf("GetTrace(beta): %v", err)
	}
	if got.ServiceName != "svc-b" || got.TenantID != "beta" {
		t.Fatalf("GetTrace(beta) returned wrong row: %+v", got)
	}
}

// TestCreateTrace_SameTraceIDSameTenant_IsIgnored proves the intra-tenant
// dedupe behaviour survives the composite-uniqueness change. Ingestion retries
// under the same tenant must still be absorbed by OnConflict.DoNothing.
func TestCreateTrace_SameTraceIDSameTenant_IsIgnored(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	traceID := "same-tenant-trace-0001"

	first := Trace{TenantID: "acme", TraceID: traceID, ServiceName: "svc-a", Status: "OK", Timestamp: now}
	dup := Trace{TenantID: "acme", TraceID: traceID, ServiceName: "svc-a-renamed", Status: "OK", Timestamp: now.Add(time.Second)}

	if err := repo.CreateTrace(first); err != nil {
		t.Fatalf("CreateTrace first: %v", err)
	}
	if err := repo.CreateTrace(dup); err != nil {
		t.Fatalf("CreateTrace dup: %v", err)
	}

	var rows []Trace
	if err := repo.db.Where("tenant_id = ? AND trace_id = ?", "acme", traceID).Find(&rows).Error; err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got, want := len(rows), 1; got != want {
		t.Fatalf("expected intra-tenant dedupe to keep %d row, got %d", want, got)
	}
	if rows[0].ServiceName != "svc-a" {
		t.Fatalf("expected first-write-wins semantics, got %q", rows[0].ServiceName)
	}
}

// TestBatchCreateTraces_SameTraceIDAcrossTenants_Succeeds exercises the batch
// path, which uses the same OnConflict.DoNothing / INSERT IGNORE plumbing.
func TestBatchCreateTraces_SameTraceIDAcrossTenants_Succeeds(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	traceID := "batch-shared-0001"

	batch := []Trace{
		{TenantID: "acme", TraceID: traceID, ServiceName: "svc-a", Status: "OK", Timestamp: now},
		{TenantID: "beta", TraceID: traceID, ServiceName: "svc-b", Status: "OK", Timestamp: now},
	}
	if err := repo.BatchCreateTraces(batch); err != nil {
		t.Fatalf("BatchCreateTraces: %v", err)
	}

	var rows []Trace
	if err := repo.db.Where("trace_id = ?", traceID).Order("tenant_id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("expected %d rows, got %d", want, got)
	}
	if rows[0].TenantID != "acme" || rows[1].TenantID != "beta" {
		t.Fatalf("unexpected tenant ordering: %v", []string{rows[0].TenantID, rows[1].TenantID})
	}
}

// TestGetTrace_IsolatesChildrenAcrossTenants confirms that when the same
// trace_id exists in two tenants, GetTrace returns each tenant's own spans
// and logs — no cross-tenant children leak through the Preload filter.
func TestGetTrace_IsolatesChildrenAcrossTenants(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	traceID := "preload-shared-0001"

	// Both tenants share trace_id but have distinct child payloads.
	if err := repo.db.Create(&Trace{TenantID: "acme", TraceID: traceID, ServiceName: "svc-a", Status: "OK", Timestamp: now}).Error; err != nil {
		t.Fatalf("Create acme trace: %v", err)
	}
	if err := repo.db.Create(&Trace{TenantID: "beta", TraceID: traceID, ServiceName: "svc-b", Status: "OK", Timestamp: now}).Error; err != nil {
		t.Fatalf("Create beta trace: %v", err)
	}
	if err := repo.db.Create(&Span{TenantID: "acme", TraceID: traceID, SpanID: "acme-span", OperationName: "op-a", ServiceName: "svc-a", StartTime: now, EndTime: now}).Error; err != nil {
		t.Fatalf("Create acme span: %v", err)
	}
	if err := repo.db.Create(&Span{TenantID: "beta", TraceID: traceID, SpanID: "beta-span", OperationName: "op-b", ServiceName: "svc-b", StartTime: now, EndTime: now}).Error; err != nil {
		t.Fatalf("Create beta span: %v", err)
	}
	if err := repo.db.Create(&Log{TenantID: "acme", TraceID: traceID, SpanID: "acme-span", Severity: "INFO", Body: "acme log", ServiceName: "svc-a", Timestamp: now}).Error; err != nil {
		t.Fatalf("Create acme log: %v", err)
	}
	if err := repo.db.Create(&Log{TenantID: "beta", TraceID: traceID, SpanID: "beta-span", Severity: "INFO", Body: "beta log", ServiceName: "svc-b", Timestamp: now}).Error; err != nil {
		t.Fatalf("Create beta log: %v", err)
	}

	cases := []struct {
		tenant  string
		wantSvc string
		wantOp  string
		wantLog string
	}{
		{"acme", "svc-a", "op-a", "acme log"},
		{"beta", "svc-b", "op-b", "beta log"},
	}
	for _, tc := range cases {
		ctx := WithTenantContext(t.Context(), tc.tenant)
		tr, err := repo.GetTrace(ctx, traceID)
		if err != nil {
			t.Fatalf("GetTrace(%s): %v", tc.tenant, err)
		}
		if tr.ServiceName != tc.wantSvc {
			t.Fatalf("%s: wrong trace row: svc=%q", tc.tenant, tr.ServiceName)
		}
		if len(tr.Spans) != 1 || tr.Spans[0].OperationName != tc.wantOp {
			names := []string{}
			for _, s := range tr.Spans {
				names = append(names, s.OperationName)
			}
			sort.Strings(names)
			t.Fatalf("%s: span leak — got ops %v, want [%s]", tc.tenant, names, tc.wantOp)
		}
		if len(tr.Logs) != 1 || tr.Logs[0].Body != tc.wantLog {
			bodies := []string{}
			for _, l := range tr.Logs {
				bodies = append(bodies, l.Body)
			}
			sort.Strings(bodies)
			t.Fatalf("%s: log leak — got bodies %v, want [%s]", tc.tenant, bodies, tc.wantLog)
		}
	}
}

// TestAutoMigrateModels_DropsLegacyTraceIDUniqueIndex proves the idempotent
// migration hook retires a pre-RAN-21 standalone unique index on
// traces.trace_id. Simulates an upgrade by creating the legacy index
// out-of-band and rerunning AutoMigrateModels.
func TestAutoMigrateModels_DropsLegacyTraceIDUniqueIndex(t *testing.T) {
	repo := newTestRepo(t)

	// Install a legacy-shaped standalone unique index on traces.trace_id.
	const legacyIdx = "idx_legacy_traces_trace_id"
	if err := repo.db.Exec("CREATE UNIQUE INDEX " + legacyIdx + " ON traces(trace_id)").Error; err != nil {
		t.Fatalf("create legacy index: %v", err)
	}
	if !repo.db.Migrator().HasIndex(&Trace{}, legacyIdx) {
		t.Fatal("precondition: legacy index should exist")
	}

	// Rerun migration — the hook must drop the legacy index.
	if err := AutoMigrateModels(repo.db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	if repo.db.Migrator().HasIndex(&Trace{}, legacyIdx) {
		t.Fatal("legacy standalone unique index on traces.trace_id was not dropped")
	}

	// Cross-tenant reuse must now succeed end-to-end.
	now := time.Now().UTC()
	if err := repo.CreateTrace(Trace{TenantID: "acme", TraceID: "after-drop", ServiceName: "svc", Timestamp: now}); err != nil {
		t.Fatalf("CreateTrace acme: %v", err)
	}
	if err := repo.CreateTrace(Trace{TenantID: "beta", TraceID: "after-drop", ServiceName: "svc", Timestamp: now}); err != nil {
		t.Fatalf("CreateTrace beta: %v", err)
	}
	var n int64
	if err := repo.db.Model(&Trace{}).Where("trace_id = ?", "after-drop").Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rows across tenants, got %d", n)
	}
}

// TestAutoMigrateModels_CreatesCompositeTraceIDUniqueIndex asserts the new
// composite index landed with the expected name after a fresh migration.
func TestAutoMigrateModels_CreatesCompositeTraceIDUniqueIndex(t *testing.T) {
	repo := newTestRepo(t)
	if !repo.db.Migrator().HasIndex(&Trace{}, "idx_traces_tenant_trace_id") {
		t.Fatal("expected composite uniqueIndex idx_traces_tenant_trace_id on traces")
	}
}
