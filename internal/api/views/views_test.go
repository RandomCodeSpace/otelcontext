package views

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

// assertNoLeak asserts that the JSON form of v contains none of forbiddens.
func assertNoLeak(t *testing.T, label string, v any, forbidden ...string) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	out := string(b)
	for _, bad := range forbidden {
		if strings.Contains(out, bad) {
			t.Errorf("%s: leaked %q in JSON output: %s", label, bad, out)
		}
	}
}

// TestViews_NoGormBookkeepingLeaksThroughJSON builds a model with every GORM
// bookkeeping field populated and proves the corresponding view's JSON output
// contains none of them.
func TestViews_NoGormBookkeepingLeaksThroughJSON(t *testing.T) {
	ts := time.Now().UTC()
	deleted := gorm.DeletedAt{Time: ts, Valid: true}

	// Trace with all GORM fields populated AND a tenant set.
	tr := storage.Trace{
		ID:          1,
		TenantID:    "acme-secret",
		TraceID:     "trace-x",
		ServiceName: "svc",
		Duration:    1500,
		DurationMs:  1.5,
		SpanCount:   2,
		Operation:   "op",
		Status:      "OK",
		Timestamp:   ts,
		CreatedAt:   ts,
		UpdatedAt:   ts,
		DeletedAt:   deleted,
	}
	traceView := TraceFromModel(tr)
	assertNoLeak(t, "Trace", traceView, "acme-secret", "tenant_id", "DeletedAt", "deleted_at", "CreatedAt", "created_at", "UpdatedAt", "updated_at")

	// Span
	sp := storage.Span{
		ID:             7,
		TenantID:       "acme-secret",
		TraceID:        "trace-x",
		SpanID:         "span-x",
		ParentSpanID:   "parent-x",
		OperationName:  "op",
		StartTime:      ts,
		EndTime:        ts,
		Duration:       100,
		ServiceName:    "svc",
		AttributesJSON: "{}",
	}
	spanView := SpanFromModel(sp)
	assertNoLeak(t, "Span", spanView, "acme-secret", "tenant_id")

	// Log
	lg := storage.Log{
		ID:             9,
		TenantID:       "acme-secret",
		TraceID:        "trace-x",
		SpanID:         "span-x",
		Severity:       "ERROR",
		Body:           "boom",
		ServiceName:    "svc",
		AttributesJSON: "{}",
		AIInsight:      "hint",
		Timestamp:      ts,
	}
	logView := LogFromModel(lg)
	assertNoLeak(t, "Log", logView, "acme-secret", "tenant_id")

	// MetricBucket
	mb := storage.MetricBucket{
		ID:             3,
		TenantID:       "acme-secret",
		Name:           "http.req",
		ServiceName:    "svc",
		TimeBucket:     ts,
		Min:            1,
		Max:            2,
		Sum:            10,
		Count:          5,
		AttributesJSON: "{}",
	}
	mbView := MetricBucketFromModel(mb)
	assertNoLeak(t, "MetricBucket", mbView, "acme-secret", "tenant_id")

	// TracesResponse also must not leak through nested traces.
	tresp := TracesResponseFromModel(&storage.TracesResponse{
		Traces: []storage.Trace{tr},
		Total:  1, Limit: 10, Offset: 0,
	})
	assertNoLeak(t, "TracesResponse", tresp, "acme-secret", "tenant_id", "deleted_at", "created_at", "updated_at")

	// DashboardStats
	ds := DashboardStatsFromModel(&storage.DashboardStats{
		TotalTraces: 10, TotalLogs: 5, TotalErrors: 1,
		AvgLatencyMs: 1.0, ErrorRate: 0.1, ActiveServices: 2, P99Latency: 500,
		TopFailingServices: []storage.ServiceError{
			{ServiceName: "svc", ErrorCount: 1, TotalCount: 10, ErrorRate: 0.1},
		},
	})
	assertNoLeak(t, "DashboardStats", ds, "tenant_id")

	// ServiceMapMetrics
	sm := ServiceMapMetricsFromModel(&storage.ServiceMapMetrics{
		Nodes: []storage.ServiceMapNode{{Name: "svc", TotalTraces: 10}},
		Edges: []storage.ServiceMapEdge{{Source: "a", Target: "b", CallCount: 1}},
	})
	assertNoLeak(t, "ServiceMapMetrics", sm, "tenant_id")

	// GraphRAG views
	lc := LogClusterNodeFromModel(graphrag.LogClusterNode{
		ID: "c1", Template: "tpl", TemplateID: 42, TemplateTokens: []string{"a"}, SampleLog: "s",
		Count: 10, FirstSeen: ts, LastSeen: ts, SeverityDist: map[string]int64{"ERROR": 5},
	})
	assertNoLeak(t, "LogClusterNode", lc, "tenant_id")

	rc := RootCauseInfoFromModel(&graphrag.RootCauseInfo{
		Service: "svc", Operation: "op", ErrorMessage: "e", SpanID: "s", TraceID: "t",
	})
	assertNoLeak(t, "RootCauseInfo", rc, "tenant_id")

	an := AnomalyNodeFromModel(graphrag.AnomalyNode{
		ID: "a1", Type: graphrag.AnomalyErrorSpike, Severity: graphrag.SeverityCritical,
		Service: "svc", Evidence: "x3 above baseline", Timestamp: ts,
	})
	assertNoLeak(t, "AnomalyNode", an, "tenant_id")

	ir := ImpactResultFromModel(&graphrag.ImpactResult{
		Service: "svc",
		AffectedServices: []graphrag.AffectedEntry{
			{Service: "downstream", Depth: 1, CallCount: 5, ImpactScore: 0.9},
		},
		TotalDownstream: 1,
	})
	assertNoLeak(t, "ImpactResult", ir, "tenant_id")

	inv := InvestigationFromModel(graphrag.Investigation{
		ID: "inv1", CreatedAt: ts, Status: "detected", Severity: "warning",
		TriggerService: "svc", TriggerOperation: "op", ErrorMessage: "e",
		RootService: "svc", RootOperation: "op",
		CausalChain:      json.RawMessage(`{"nodes":[]}`),
		TraceIDs:         json.RawMessage(`["t1"]`),
		ErrorLogs:        json.RawMessage(`[]`),
		AnomalousMetrics: json.RawMessage(`[]`),
		AffectedServices: json.RawMessage(`[]`),
		SpanChain:        json.RawMessage(`[]`),
	})
	assertNoLeak(t, "Investigation", inv, "tenant_id")
}

// TestTraceView_PreservesJSONFieldNames asserts the exact JSON shape consumed by
// the UI (trace_id/service_name/duration_ms/span_count/timestamp) is preserved.
func TestTraceView_PreservesJSONFieldNames(t *testing.T) {
	ts := time.Unix(1700000000, 0).UTC()
	tr := storage.Trace{
		ID: 1, TraceID: "tid", ServiceName: "svc", DurationMs: 1.5, SpanCount: 3,
		Status: "OK", Timestamp: ts, Operation: "op", Duration: 1500,
	}
	b, err := json.Marshal(TraceFromModel(tr))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, k := range []string{`"trace_id":"tid"`, `"service_name":"svc"`, `"duration_ms":1.5`, `"span_count":3`, `"operation":"op"`, `"status":"OK"`} {
		if !strings.Contains(s, k) {
			t.Errorf("Trace view missing expected JSON fragment %s in %s", k, s)
		}
	}
}
