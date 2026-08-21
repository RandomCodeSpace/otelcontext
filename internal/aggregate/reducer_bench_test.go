package aggregate

import (
	"fmt"
	"testing"
	"time"
)

// benchOperations is the operation set the reduction benchmarks replay. Ten
// operations is the shape #172's acceptance criterion names.
var benchOperations = []string{
	"GET /orders", "POST /orders", "GET /orders/{id}", "DELETE /orders/{id}",
	"GET /users", "POST /users", "GET /health", "POST /checkout",
	"GET /inventory", "POST /payments",
}

// benchSpans builds a batch of spans across the operation set, alternating
// status so the batch materializes both healthy and error series.
func benchSpans(n int, ts time.Time) []SpanInput {
	spans := make([]SpanInput, 0, n)
	for i := 0; i < n; i++ {
		status := int32(1)
		if i%13 == 0 {
			status = 2
		}
		spans = append(spans, SpanInput{
			Tenant:         "default",
			Service:        "checkout",
			SpanName:       benchOperations[i%len(benchOperations)],
			Method:         "GET",
			HTTPStatusCode: 200,
			SpanKind:       2,
			StatusCode:     status,
			Timestamp:      ts,
			DurationMicros: float64(100 + i%5000),
		})
	}
	return spans
}

// TestReductionRatio is the acceptance criterion: 1,000 spans across 10
// operations must collapse to a handful of deltas.
func TestReductionRatio(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)
	spans := benchSpans(1000, now)

	r := e.NewReducer(now)
	for _, s := range spans {
		r.ReduceSpan(s)
	}

	deltas := r.Len()
	if deltas == 0 {
		t.Fatal("no deltas produced")
	}
	if deltas > 40 {
		t.Errorf("1000 spans over %d operations produced %d deltas, want <= 40", len(benchOperations), deltas)
	}
	t.Logf("reduction: 1000 spans -> %d deltas (ratio %.1f:1)", deltas, 1000.0/float64(deltas))

	e.ApplyReducer(r)
	if count, _ := e.Snapshot().Totals(SignalTraceOp); count != 1000 {
		t.Fatalf("aggregated count = %d, want 1000 — reduction must not lose points", count)
	}
}

// TestReducerAllocationsAreFlatInBatchSize pins the property that makes the
// engine worth having: cost per Export is bounded by the number of distinct
// SERIES, not by the number of points. A tenfold batch must not cost tenfold
// allocations.
func TestReducerAllocationsAreFlatInBatchSize(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation measurement is noisy under -short")
	}
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)

	measure := func(n int) float64 {
		spans := benchSpans(n, now)
		// Warm the dictionary and template caches: first-sight interning is a
		// one-off cost, not a per-request one.
		warm := e.NewReducer(now)
		for _, s := range spans {
			warm.ReduceSpan(s)
		}
		return testing.AllocsPerRun(20, func() {
			r := e.NewReducer(now)
			for _, s := range spans {
				r.ReduceSpan(s)
			}
		})
	}

	small := measure(100)
	large := measure(1000)
	t.Logf("allocs per Export: 100 spans = %.0f, 1000 spans = %.0f", small, large)

	if large > small*3 {
		t.Errorf("allocations scaled with batch size: %.0f at 100 spans, %.0f at 1000 spans", small, large)
	}
	// Per-point allocation must fall as the batch grows — that is reduction.
	if perPoint := large / 1000; perPoint > small/100 {
		t.Errorf("per-point allocations did not improve with batch size: %.3f vs %.3f", perPoint, small/100)
	}
}

func BenchmarkReduceSpans(b *testing.B) {
	now := time.Unix(1787000000, 0).UTC()
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("spans=%d", n), func(b *testing.B) {
			e, err := NewEngine(EngineConfig{Mode: ModeShadow, Now: func() time.Time { return now }})
			if err != nil {
				b.Fatalf("NewEngine: %v", err)
			}
			spans := benchSpans(n, now)
			warm := e.NewReducer(now)
			for _, s := range spans {
				warm.ReduceSpan(s)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := e.NewReducer(now)
				for _, s := range spans {
					r.ReduceSpan(s)
				}
			}
			b.ReportMetric(float64(warm.Len()), "deltas")
		})
	}
}

func BenchmarkApplyDeltas(b *testing.B) {
	now := time.Unix(1787000000, 0).UTC()
	e, err := NewEngine(EngineConfig{Mode: ModeShadow, Now: func() time.Time { return now }})
	if err != nil {
		b.Fatalf("NewEngine: %v", err)
	}
	spans := benchSpans(1000, now)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := e.NewReducer(now)
		for _, s := range spans {
			r.ReduceSpan(s)
		}
		e.ApplyReducer(r)
	}
}
