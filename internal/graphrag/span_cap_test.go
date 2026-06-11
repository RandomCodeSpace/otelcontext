package graphrag

import (
	"fmt"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// mkSpanNode builds a minimal SpanNode for cap tests.
func mkSpanNode(id string, ts time.Time) SpanNode {
	return SpanNode{
		ID:        id,
		TraceID:   "trace-cap",
		Service:   "orders",
		Operation: "/op",
		Duration:  1.0,
		Timestamp: ts,
	}
}

// TestTraceStore_UpsertSpan_CapBlocksNewSpans proves the per-tenant span cap:
// once len(Spans) reaches MaxSpans, NEW span IDs are rejected (UpsertSpan
// returns false and the map does not grow) while updates to already-resident
// span IDs still apply.
func TestTraceStore_UpsertSpan_CapBlocksNewSpans(t *testing.T) {
	ts := newTraceStore(time.Hour, 3)
	now := time.Now()

	for i := 0; i < 3; i++ {
		if !ts.UpsertSpan(mkSpanNode(fmt.Sprintf("s-%d", i), now)) {
			t.Fatalf("span s-%d rejected below the cap", i)
		}
	}

	// 4th NEW span: at cap, must be rejected.
	if ts.UpsertSpan(mkSpanNode("s-overflow", now)) {
		t.Fatalf("new span accepted at cap — MaxSpans not enforced")
	}
	if got := len(ts.Spans); got != 3 {
		t.Fatalf("Spans len = %d after rejected insert, want 3", got)
	}

	// Update to an EXISTING span ID at cap: must still apply.
	updated := mkSpanNode("s-1", now)
	updated.Duration = 42.0
	if !ts.UpsertSpan(updated) {
		t.Fatalf("update to resident span s-1 rejected at cap")
	}
	if got, _ := ts.GetSpan("s-1"); got.Duration != 42.0 {
		t.Fatalf("resident span update not applied at cap: Duration=%v, want 42", got.Duration)
	}
}

// TestTraceStore_UpsertSpan_CapDisabled proves MaxSpans <= 0 leaves the
// store unbounded (legacy behavior).
func TestTraceStore_UpsertSpan_CapDisabled(t *testing.T) {
	ts := newTraceStore(time.Hour, 0)
	now := time.Now()
	for i := 0; i < 10; i++ {
		if !ts.UpsertSpan(mkSpanNode(fmt.Sprintf("s-%d", i), now)) {
			t.Fatalf("span rejected with cap disabled")
		}
	}
	if got := len(ts.Spans); got != 10 {
		t.Fatalf("Spans len = %d, want 10", got)
	}
}

// TestProcessSpan_CapDropRecorded proves the coordinator surfaces cap drops:
// when a tenant's TraceStore is full, processSpan records the skip via the
// span_capacity drop counter (and the Prometheus counter when wired).
func TestProcessSpan_CapDropRecorded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSpansPerTenant = 2
	g := New(nil, nil, nil, cfg)
	t.Cleanup(g.Stop)

	now := time.Now()
	for i := 0; i < 3; i++ {
		g.processSpan(&spanEvent{
			Span: storage.Span{
				TraceID:     "trace-cap",
				SpanID:      fmt.Sprintf("s-%d", i),
				ServiceName: "orders",
				StartTime:   now,
			},
			TraceID: "trace-cap",
			Status:  "STATUS_CODE_UNSET",
			Tenant:  storage.DefaultTenantID,
		})
	}

	if got := g.SpanCapacityDropsCount(); got != 1 {
		t.Fatalf("SpanCapacityDropsCount = %d, want 1", got)
	}
	stores := g.storesForTenant(storage.DefaultTenantID)
	stores.traces.mu.RLock()
	n := len(stores.traces.Spans)
	stores.traces.mu.RUnlock()
	if n != 2 {
		t.Fatalf("tenant span count = %d, want 2 (cap)", n)
	}
}
