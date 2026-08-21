package storage

import (
	"testing"
	"time"
)

// seedGraphTrace inserts one trace plus one span sharing the given tenant and
// trace ID, so GetSpansForGraph has a joinable pair to project.
func seedGraphTrace(t *testing.T, repo *Repository, tenant, traceID, spanID, status string, ts time.Time) {
	t.Helper()
	tr := Trace{
		TenantID:    tenant,
		TraceID:     traceID,
		ServiceName: "svc-" + tenant,
		Duration:    1000,
		Status:      status,
		Timestamp:   ts,
	}
	if err := repo.db.Create(&tr).Error; err != nil {
		t.Fatalf("create trace(%s/%s): %v", tenant, traceID, err)
	}
	sp := Span{
		TenantID:      tenant,
		TraceID:       traceID,
		SpanID:        spanID,
		OperationName: "op",
		StartTime:     ts,
		EndTime:       ts.Add(time.Millisecond),
		Duration:      1000,
		ServiceName:   "svc-" + tenant,
		Status:        status,
	}
	if err := repo.db.Create(&sp).Error; err != nil {
		t.Fatalf("create span(%s/%s): %v", tenant, spanID, err)
	}
}

// TestGetSpansForGraph_ErrorStatusSetsIsError is the regression test for #169.
// The projection compared traces.status against the literal "ERROR", but the
// column holds OTLP code strings (STATUS_CODE_ERROR), so IsError was never
// true and the legacy in-memory graph never saw an error edge from this path.
func TestGetSpansForGraph_ErrorStatusSetsIsError(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()

	seedGraphTrace(t, repo, DefaultTenantID, "trace-err-0001", "span-err-01", StatusCodeError, now)
	seedGraphTrace(t, repo, DefaultTenantID, "trace-ok-0001", "span-ok-01", "STATUS_CODE_OK", now)

	rows, err := repo.GetSpansForGraph(now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetSpansForGraph: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("row count = %d, want %d (%+v)", got, want, rows)
	}

	bySpan := map[string]SpanGraphRow{}
	for _, r := range rows {
		bySpan[r.SpanID] = r
	}
	if !bySpan["span-err-01"].IsError {
		t.Fatalf("span on a %s trace: IsError = false, want true", StatusCodeError)
	}
	if bySpan["span-ok-01"].IsError {
		t.Fatalf("span on a STATUS_CODE_OK trace: IsError = true, want false")
	}
}

// TestGetSpansForGraph_TenantIsolatedJoin proves the spans⋈traces join keys on
// (tenant_id, trace_id) rather than trace_id alone. Two tenants reuse the same
// trace ID with opposite statuses; before the fix the join fanned out and each
// span picked up whichever tenant's trace row the planner matched first, so a
// tenant's spans could inherit another tenant's error status.
func TestGetSpansForGraph_TenantIsolatedJoin(t *testing.T) {
	repo := newTestRepo(t)
	now := time.Now().UTC()
	traceID := "shared-trace-id-0001"

	seedGraphTrace(t, repo, "acme", traceID, "span-acme-01", StatusCodeError, now)
	seedGraphTrace(t, repo, "beta", traceID, "span-beta-01", "STATUS_CODE_OK", now)

	rows, err := repo.GetSpansForGraph(now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetSpansForGraph: %v", err)
	}
	// A trace_id-only join produces a cartesian 2x2 fan-out.
	if got, want := len(rows), 2; got != want {
		t.Fatalf("row count = %d, want %d — join fanned out across tenants (%+v)", got, want, rows)
	}

	bySpan := map[string]SpanGraphRow{}
	for _, r := range rows {
		bySpan[r.SpanID] = r
	}
	acme, ok := bySpan["span-acme-01"]
	if !ok {
		t.Fatalf("acme span missing from projection: %+v", rows)
	}
	beta, ok := bySpan["span-beta-01"]
	if !ok {
		t.Fatalf("beta span missing from projection: %+v", rows)
	}
	if !acme.IsError {
		t.Fatalf("acme span: IsError = false, want true (its own trace is %s)", StatusCodeError)
	}
	if beta.IsError {
		t.Fatalf("beta span: IsError = true, want false — inherited acme's trace status")
	}
}
