package ingest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// observedPair is a service→service topology observation captured by the test
// hook. We record source/target service so we can assert the cross-service
// CALLS edge was observed regardless of sampler keep/drop decisions.
type observedSpan struct {
	tenant, traceID, spanID, parentSpanID, service string
}

// twoServiceTraceReq builds one trace with a "checkout" root span and a
// "payments" child span (child.ParentSpanId == root.SpanId), across two
// resource_spans so each carries its own service.name resource attribute.
func twoServiceTraceReq() *coltracepb.ExportTraceServiceRequest {
	res := func(svc string) *resourcepb.Resource {
		return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
			Key:   "service.name",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: svc}},
		}}}
	}
	now := uint64(time.Now().UnixNano())
	span := func(name string, spanID, parentID []byte) *tracepb.Span {
		return &tracepb.Span{
			TraceId:           []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
			SpanId:            spanID,
			ParentSpanId:      parentID,
			Name:              name,
			StartTimeUnixNano: now,
			EndTimeUnixNano:   now + uint64(time.Millisecond),
			Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
		}
	}
	checkoutSpanID := []byte{0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa}
	paymentsSpanID := []byte{0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb, 0xbb}
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				Resource:   res("checkout"),
				ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span("/checkout", checkoutSpanID, nil)}}},
			},
			{
				Resource:   res("payments"),
				ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span("/charge", paymentsSpanID, checkoutSpanID)}}},
			},
		},
	}
}

// newTopologyWiringServer builds a TraceServer with a near-zero sampling rate
// and a recording topology observer. asyncPipeline selects whether the async
// ingest pipeline is wired (true) or the synchronous fallback path is used
// (false) — the observer MUST fire on both before the sampler drop.
func newTopologyWiringServer(t *testing.T, asyncPipeline bool) (*TraceServer, *recorder) {
	t.Helper()

	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })

	cfg := &config.Config{IngestMinSeverity: "DEBUG"}
	srv := NewTraceServer(repo, nil, cfg)

	// Aggressively drop healthy spans. rate≈0 means only the per-service burst
	// (first span) survives; the joint survival of the parent+child PAIR across
	// two services is effectively nil — exactly the starvation scenario.
	srv.SetSampler(NewSampler(0.0001, true, 1_000_000))

	rec := &recorder{}
	srv.SetTopologyObserver(rec.observe)

	if asyncPipeline {
		p := NewPipeline(repo, nil, PipelineConfig{Capacity: 100, Workers: 1, SoftThreshold: 0.9})
		p.Start(context.Background())
		t.Cleanup(p.Stop)
		srv.SetPipeline(p)
	}
	return srv, rec
}

func TestTopologyObserver_FiresPreSample_AsyncPath(t *testing.T) {
	srv, rec := newTopologyWiringServer(t, true)

	if _, err := srv.Export(context.Background(), twoServiceTraceReq()); err != nil {
		t.Fatalf("Export: %v", err)
	}

	rec.assertObservedPair(t, "checkout", "payments")
}

func TestTopologyObserver_FiresPreSample_SyncPath(t *testing.T) {
	srv, rec := newTopologyWiringServer(t, false)

	if _, err := srv.Export(context.Background(), twoServiceTraceReq()); err != nil {
		t.Fatalf("Export: %v", err)
	}

	rec.assertObservedPair(t, "checkout", "payments")
}

// recorder captures topology observations from the hook under test.
type recorder struct {
	mu    sync.Mutex
	spans []observedSpan
}

func (r *recorder) observe(tenant, traceID, spanID, parentSpanID, service string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, observedSpan{tenant, traceID, spanID, parentSpanID, service})
}

// assertObservedPair asserts the observer saw both the parent (source) and the
// child (target) spans, and that the child's parent links to the source span —
// i.e. enough information to form the source→target CALLS edge pre-sample.
func (r *recorder) assertObservedPair(t *testing.T, sourceService, targetService string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	bySpanID := make(map[string]observedSpan, len(r.spans))
	for _, s := range r.spans {
		bySpanID[s.spanID] = s
	}

	var sawSource, sawLinkedTarget bool
	for _, s := range r.spans {
		if s.service == sourceService && s.parentSpanID == "" {
			sawSource = true
		}
		if s.service == targetService && s.parentSpanID != "" {
			if parent, ok := bySpanID[s.parentSpanID]; ok && parent.service == sourceService {
				sawLinkedTarget = true
			}
		}
	}
	if !sawSource {
		t.Fatalf("observer never saw the %s source span (observed=%+v)", sourceService, r.spans)
	}
	if !sawLinkedTarget {
		t.Fatalf("observer never saw the %s child linked to its %s parent (observed=%+v)", targetService, sourceService, r.spans)
	}
}
