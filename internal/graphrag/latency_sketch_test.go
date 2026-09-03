package graphrag

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
	"unsafe"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/latency"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// TestUpsertService_SketchP99 proves the per-service sketch (#291) on a
// bimodal population where average × 2.5 is nowhere near the tail: 900
// calls at 10ms and 100 at 500ms have a nearest-rank p99 of 500ms, while
// the old estimate said 147.5ms (59ms × 2.5, 70% low). The sketch answer
// lands inside its advertised relative-error bound and says so.
func TestUpsertService_SketchP99(t *testing.T) {
	store := newServiceStore()
	now := time.Now()
	for i := 0; i < 1000; i++ {
		ms := 10.0
		if i%10 == 9 {
			ms = 500.0
		}
		store.UpsertService("bimodal", ms, false, now)
	}
	svc := store.Services["bimodal"]
	const trueP99 = 500.0
	oldEstimate := svc.AvgLatency * 2.5
	if svc.AvgLatency != 59 {
		t.Fatalf("avg = %v, want 59", svc.AvgLatency)
	}
	if oldErr := math.Abs(oldEstimate-trueP99) / trueP99; oldErr < 0.5 {
		t.Fatalf("old estimate %v is within %.0f%% of %v; fixture no longer contradicts the multiplier", oldEstimate, oldErr*100, trueP99)
	}
	claim := svc.LatencyProvenance.P99
	if claim.Status != latency.StatusApproximate || claim.Method != latency.MethodDDSketch ||
		claim.SampleCount != 1000 || claim.LowSample || claim.Degraded ||
		claim.SketchScale != aggregate.SketchDefaultScale || claim.RelativeErrorBound <= 0 || claim.EstimateFactor != 0 {
		t.Fatalf("provenance = %+v", claim)
	}
	if err := math.Abs(svc.P99Latency-trueP99) / trueP99; err > claim.RelativeErrorBound {
		t.Fatalf("sketch p99 %v is %.2f%% from %v, bound %.2f%%", svc.P99Latency, err*100, trueP99, claim.RelativeErrorBound*100)
	}
	t.Logf("true p99=%vms old estimate=%vms (avg×2.5) sketch=%.3fms bound=±%.2f%% provenance=%s/%s",
		trueP99, oldEstimate, svc.P99Latency, claim.RelativeErrorBound*100, claim.Status, claim.Method)

	if size := unsafe.Sizeof(aggregate.Sketch{}); size > 2200 {
		t.Fatalf("sketch is %d bytes; the store comment promises ~2.1 KiB", size)
	}
}

func feedLatencySpan(g *GraphRAG, service string, i int, micros int64) {
	g.processSpan(&spanEvent{
		Span: storage.Span{
			TraceID: "trace-lat", SpanID: fmt.Sprintf("%s-%d", service, i),
			OperationName: "/op", ServiceName: service, Duration: micros, StartTime: time.Now(),
		},
		TraceID: "trace-lat", Status: "STATUS_CODE_UNSET", Tenant: storage.DefaultTenantID,
	})
}

// TestDetectAnomalies_LatencyUsesSketch proves the legacy latency check
// gates on the sketch p99 (#291). A service whose tail is 1500ms over a 10ms
// median fires although its average (159ms) never crossed the old avg > 500ms
// gate; a uniformly slow 800ms service keeps firing exactly as it did before.
func TestDetectAnomalies_LatencyUsesSketch(t *testing.T) {
	g := New(nil, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)
	for i := 0; i < 100; i++ {
		micros := int64(10_000)
		if i%10 == 9 {
			micros = 1_500_000
		}
		feedLatencySpan(g, "tail", i, micros)
	}
	for i := 0; i < 20; i++ {
		feedLatencySpan(g, "slow", i, 800_000)
	}
	g.detectAnomalies(context.Background())

	stores := g.storesForTenant(storage.DefaultTenantID)
	tail, ok := stores.anomalies.Anomalies["anom_tail_lat"]
	if !ok {
		t.Fatalf("bimodal tail did not fire: %+v", stores.service.Services["tail"])
	}
	if tail.Severity != SeverityWarning {
		t.Fatalf("severity = %s, want %s for a ~1500ms p99: %s", tail.Severity, SeverityWarning, tail.Evidence)
	}
	if _, ok := stores.anomalies.Anomalies["anom_slow_lat"]; !ok {
		t.Fatalf("uniformly slow service stopped firing: %+v", stores.service.Services["slow"])
	}
	t.Logf("evidence: %s", tail.Evidence)
}
