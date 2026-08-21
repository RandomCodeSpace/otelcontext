package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// edgeExport builds one resource-spans block for service with the given spans.
func edgeExport(service string, spans ...*tracepb.Span) *tracepb.ResourceSpans {
	return &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
			Key:   "service.name",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}},
		}}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}
}

func edgeSpan(traceID, spanID, parentID, name string, ts time.Time) *tracepb.Span {
	sp := &tracepb.Span{
		TraceId:           []byte(traceID),
		SpanId:            []byte(spanID),
		Name:              name,
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: uint64(ts.UnixNano()), // #nosec G115 -- test timestamps are positive
		EndTimeUnixNano:   uint64(ts.Add(2 * time.Millisecond).UnixNano()),
		Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
	}
	if parentID != "" {
		sp.ParentSpanId = []byte(parentID)
	}
	return sp
}

// TestExportEmitsServiceEdgeSeries proves the deferred half of #183 landed:
// a child span whose parent lives in another service produces a caller->callee
// edge in the aggregate topology, and an internal (same-service) parent does
// not.
func TestExportEmitsServiceEdgeSeries(t *testing.T) {
	now := time.Now()
	engine := newAggregateEngine(t, now)
	repo, _ := newAggregateTestRepo(t)
	srv := NewTraceServer(repo, nil, aggTestConfig())
	srv.SetAggregateEngine(engine)

	// The gateway's span arrives first, in its own Export, exactly as it would
	// from a different process.
	req := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{
		edgeExport("gateway", edgeSpan("trace-0000000001", "span-001", "", "GET /checkout", now)),
	}}
	if _, err := srv.Export(context.Background(), req); err != nil {
		t.Fatalf("gateway Export: %v", err)
	}

	req = &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{
		edgeExport("checkout",
			edgeSpan("trace-0000000001", "span-002", "span-001", "POST /pay", now),
			// Same-service child: an internal call, never a topology edge.
			edgeSpan("trace-0000000001", "span-003", "span-002", "POST /pay/inner", now),
		),
	}}
	if _, err := srv.Export(context.Background(), req); err != nil {
		t.Fatalf("checkout Export: %v", err)
	}

	snap := engine.TopologySnapshot(storage.DefaultTenantID)
	if len(snap.Edges) != 1 {
		t.Fatalf("edges = %+v, want exactly one gateway->checkout edge", snap.Edges)
	}
	edge := snap.Edges[0]
	if edge.Caller != "gateway" || edge.Callee != "checkout" {
		t.Fatalf("edge = %s -> %s, want gateway -> checkout", edge.Caller, edge.Callee)
	}
	var count uint64
	for _, w := range edge.Windows {
		count += w.Count
	}
	if count != 1 {
		t.Fatalf("edge call count = %d, want 1", count)
	}

	// The edge must be a real series in the shards, not just a projection.
	shardCount, _ := engine.Snapshot().Totals(aggregate.SignalServiceEdge)
	if shardCount != 1 {
		t.Fatalf("service-edge series count = %d, want 1", shardCount)
	}
}

// TestExportEdgesSurviveSampling proves the edge is derived before the
// retention gate: at a sampling rate that drops everything, the topology still
// carries the edge, because aggregate counts describe accepted telemetry.
func TestExportEdgesSurviveSampling(t *testing.T) {
	now := time.Now()
	engine := newAggregateEngine(t, now)
	repo, _ := newAggregateTestRepo(t)
	srv := NewTraceServer(repo, nil, aggTestConfig())
	srv.SetAggregateEngine(engine)
	// Rate 0 with always-on-errors: every healthy span is dropped from raw
	// persistence.
	srv.SetSampler(NewSampler(0, true, 500))

	reqs := []*coltracepb.ExportTraceServiceRequest{
		{ResourceSpans: []*tracepb.ResourceSpans{
			edgeExport("gateway", edgeSpan("trace-0000000002", "span-101", "", "GET /x", now)),
		}},
		{ResourceSpans: []*tracepb.ResourceSpans{
			edgeExport("orders", edgeSpan("trace-0000000002", "span-102", "span-101", "POST /y", now)),
		}},
	}
	for _, req := range reqs {
		if _, err := srv.Export(context.Background(), req); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	var persisted int64
	if err := repo.DB().Table("spans").Count(&persisted).Error; err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if persisted != 0 {
		t.Fatalf("sampler persisted %d spans; the test is no longer about sampling", persisted)
	}

	snap := engine.TopologySnapshot(storage.DefaultTenantID)
	if len(snap.Edges) != 1 || snap.Edges[0].Caller != "gateway" || snap.Edges[0].Callee != "orders" {
		t.Fatalf("edge lost to sampling: %+v", snap.Edges)
	}
}
