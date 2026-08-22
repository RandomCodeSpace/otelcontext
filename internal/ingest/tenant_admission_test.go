package ingest

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/metadata"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
)

// tenantCtx stamps the gRPC transport tenant the OTLP receivers resolve from.
func tenantCtx(tenant string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(tenantHeader, tenant))
}

// admissionHarness wires trace+log servers to a pipeline with NO workers, so
// nothing drains the queue and the per-tenant cap is observed deterministically.
type admissionHarness struct {
	traces   *TraceServer
	logs     *LogsServer
	pipeline *Pipeline
}

func newAdmissionHarness(t *testing.T, capacity, perTenantCap int, trustResourceTenant bool) *admissionHarness {
	t.Helper()
	cfg := &config.Config{
		IngestMinSeverity:          "DEBUG",
		SamplingLatencyThresholdMs: 500,
		OTLPTrustResourceTenant:    trustResourceTenant,
	}
	traces := NewTraceServer(nil, nil, cfg)
	logs := NewLogsServer(nil, nil, cfg)

	pl := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: capacity, Workers: 0, SoftThreshold: 0.9})
	pl.SetPerTenantCap(perTenantCap)
	traces.SetPipeline(pl)
	logs.SetPipeline(pl)
	return &admissionHarness{traces: traces, logs: logs, pipeline: pl}
}

// TestExport_PerTenantAdmission_IsolatesTenants is the acceptance test for
// #194 finding 12. Two tenants export concurrently; the noisy one saturates
// its own cap and every excess submission is charged to it, while the quiet
// tenant's exports all land untouched.
func TestExport_PerTenantAdmission_IsolatesTenants(t *testing.T) {
	const perTenantCap = 4
	const noisyExports = 12
	h := newAdmissionHarness(t, 256, perTenantCap, false)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx := tenantCtx("noisy")
		for i := range noisyExports {
			if _, err := h.traces.Export(ctx, buildTracesRequest(fmt.Sprintf("svc-noisy-%d", i), 1)); err != nil {
				t.Errorf("noisy export %d: %v", i, err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		ctx := tenantCtx("quiet")
		for i := range perTenantCap {
			if _, err := h.logs.Export(ctx, buildLogsRequest(fmt.Sprintf("svc-quiet-%d", i), 1)); err != nil {
				t.Errorf("quiet export %d: %v", i, err)
			}
		}
	}()
	wg.Wait()

	stats := h.pipeline.Stats()
	// Each tenant occupies exactly its own cap; the noisy tenant's excess is
	// dropped without touching the quiet tenant's slots.
	if want := int64(2 * perTenantCap); stats.Enqueued != want {
		t.Fatalf("Enqueued=%d, want %d (cap per tenant, two tenants)", stats.Enqueued, want)
	}
	if want := int64(noisyExports - perTenantCap); h.pipeline.TenantDropped() != want {
		t.Fatalf("TenantDropped=%d, want %d", h.pipeline.TenantDropped(), want)
	}
	if stats.DroppedHealthy != 0 || stats.RejectedFull != 0 {
		t.Fatalf("unrelated backpressure fired: DroppedHealthy=%d RejectedFull=%d",
			stats.DroppedHealthy, stats.RejectedFull)
	}
}

// TestExport_BatchTenantPopulated pins the underlying defect: before the fix
// Batch.Tenant was always empty, which made the cap a no-op. Drain the queue
// and read the field directly.
func TestExport_BatchTenantPopulated(t *testing.T) {
	h := newAdmissionHarness(t, 8, 0, false)

	if _, err := h.traces.Export(tenantCtx("acme"), buildTracesRequest("svc-a", 2)); err != nil {
		t.Fatalf("trace export: %v", err)
	}
	if _, err := h.logs.Export(tenantCtx("acme"), buildLogsRequest("svc-a", 2)); err != nil {
		t.Fatalf("log export: %v", err)
	}

	for i := range 2 {
		select {
		case b := <-h.pipeline.queue:
			if b.Tenant != "acme" {
				t.Fatalf("batch %d Tenant=%q, want acme", i, b.Tenant)
			}
		default:
			t.Fatalf("expected 2 batches on the queue, got %d", i)
		}
	}
}

// strAttr builds an OTLP string-valued resource attribute.
func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   k,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}},
	}
}

// resourceSpansForTenant builds one ResourceSpans carrying an explicit
// tenant.id resource attribute and a single span.
func resourceSpansForTenant(tenant, svc string, seed byte) *tracepb.ResourceSpans {
	now := uint64(time.Now().UnixNano())
	return &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			strAttr("service.name", svc),
			strAttr("tenant.id", tenant),
		}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId:           bytes.Repeat([]byte{seed}, 16),
			SpanId:            bytes.Repeat([]byte{seed}, 8),
			Name:              "op",
			StartTimeUnixNano: now,
			EndTimeUnixNano:   now + uint64(time.Millisecond),
		}}}},
	}
}

// TestExport_MixedTenantResources_SplitPerTenant covers the trusted-resource
// tenancy path: one Export carrying resources for two tenants must produce one
// batch per tenant so each is charged its own admission slot instead of both
// landing on a single shared slot.
func TestExport_MixedTenantResources_SplitPerTenant(t *testing.T) {
	h := newAdmissionHarness(t, 8, 0, true)

	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			resourceSpansForTenant("alpha", "svc-a", 0x11),
			resourceSpansForTenant("beta", "svc-b", 0x22),
			resourceSpansForTenant("alpha", "svc-c", 0x33),
		},
	}
	// No transport tenant on the context, so the trusted resource attribute
	// is what resolves.
	if _, err := h.traces.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}

	got := map[string]int{}
	for {
		select {
		case b := <-h.pipeline.queue:
			got[b.Tenant] += len(b.Spans)
			continue
		default:
		}
		break
	}
	if len(got) != 2 {
		t.Fatalf("batches grouped into %d tenants (%v), want 2", len(got), got)
	}
	if got["alpha"] != 2 {
		t.Errorf("alpha spans=%d, want 2 (two resources merged into one batch)", got["alpha"])
	}
	if got["beta"] != 1 {
		t.Errorf("beta spans=%d, want 1", got["beta"])
	}
}
