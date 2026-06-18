package graphrag

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// TestObserveSpanTopology_EdgeSurvivesSampling is the core regression for the
// service-map edge-starvation bug: under aggressive sampling almost no
// parent+child span PAIR survives, so the sampled path (OnSpanIngested →
// processSpan → UpsertCallEdge) never forms the cross-service CALLS edge.
//
// The pre-sample topology observer must guarantee the checkout→payments edge
// EXISTS even though no sampled span ever did. Here we simulate the worst case
// directly: NOT A SINGLE span reaches OnSpanIngested (everything was dropped by
// the sampler upstream), yet the observer saw both spans before the drop.
func TestObserveSpanTopology_EdgeSurvivesSampling(t *testing.T) {
	g := newTestGraphRAG(t)

	tenant := storage.DefaultTenantID
	// Parent span "checkout" (root: empty parent), child span "payments" whose
	// parent is the checkout span. Observed pre-sample, in arrival order.
	g.ObserveSpanTopology(tenant, "trace-1", "span-checkout", "", "checkout")
	g.ObserveSpanTopology(tenant, "trace-1", "span-payments", "span-checkout", "payments")

	ctx := storage.WithTenantContext(context.Background(), tenant)

	// The observer upserts synchronously (no event channel), so the edge is
	// present immediately — no polling needed.
	if !hasCallEdge(g.ServiceMap(ctx, 0), "checkout", "payments") {
		t.Fatalf("checkout→payments CALLS edge absent; observer failed to record topology independent of sampling")
	}
}

// TestObserveSpanTopology_UnknownParentNoEdge confirms that a child whose
// parent span has not been observed yet produces no edge (we cannot attribute
// the call to a source service). It must not panic or fabricate an edge.
func TestObserveSpanTopology_UnknownParentNoEdge(t *testing.T) {
	g := newTestGraphRAG(t)
	tenant := storage.DefaultTenantID

	g.ObserveSpanTopology(tenant, "trace-1", "span-child", "span-missing-parent", "payments")

	ctx := storage.WithTenantContext(context.Background(), tenant)
	for _, e := range allCallEdges(g.ServiceMap(ctx, 0)) {
		t.Fatalf("unexpected edge with unknown parent: %s→%s", e.FromID, e.ToID)
	}
}

// TestObserveSpanTopology_SameServiceNoEdge confirms a parent/child pair in the
// SAME service does not create a self-edge — CALLS is cross-service only.
func TestObserveSpanTopology_SameServiceNoEdge(t *testing.T) {
	g := newTestGraphRAG(t)
	tenant := storage.DefaultTenantID

	g.ObserveSpanTopology(tenant, "trace-1", "span-a", "", "checkout")
	g.ObserveSpanTopology(tenant, "trace-1", "span-b", "span-a", "checkout")

	ctx := storage.WithTenantContext(context.Background(), tenant)
	for _, e := range allCallEdges(g.ServiceMap(ctx, 0)) {
		t.Fatalf("unexpected self-edge: %s→%s", e.FromID, e.ToID)
	}
}

// TestObserveSpanTopology_PreservesSampledAggregates is the no-regression guard
// for edge statistics. The sampled path computes CallCount/latency/error-rate.
// The observer only guarantees existence: when a sampled span already formed the
// edge, observing the same pair again must NOT inflate the sampled aggregates.
func TestObserveSpanTopology_PreservesSampledAggregates(t *testing.T) {
	g := newTestGraphRAG(t)
	tenant := storage.DefaultTenantID
	now := time.Now()

	// Deterministically establish a checkout→payments edge WITH real aggregates,
	// exactly as the sampled path does (processSpan → UpsertCallEdge). Done
	// directly on the store so this test does not depend on async event-worker
	// ordering (the sampled path needs the parent span resident before the child
	// is processed; that ordering is not guaranteed and is orthogonal to the
	// invariant under test).
	stores := g.storesForTenant(tenant)
	stores.service.UpsertService("checkout", 2.0, false, now)
	stores.service.UpsertService("payments", 2.0, false, now)
	const sampledCalls = 5
	for i := 0; i < sampledCalls; i++ {
		stores.service.UpsertCallEdge("checkout", "payments", 2.0, false, now)
	}

	ctx := storage.WithTenantContext(context.Background(), tenant)
	edge := getCallEdge(g.ServiceMap(ctx, 0), "checkout", "payments")
	if edge == nil {
		t.Fatalf("setup: sampled-path edge not present")
	}
	wantCount, wantTotalMs, wantWeight := edge.CallCount, edge.TotalMs, edge.Weight
	if wantCount != sampledCalls {
		t.Fatalf("setup: CallCount = %d, want %d", wantCount, sampledCalls)
	}

	// Now the observer sees the SAME pair many times (as it would pre-sample for
	// every received span). Existence-only upsert must leave aggregates intact.
	for i := 0; i < 100; i++ {
		g.ObserveSpanTopology(tenant, "trace-1", "span-checkout", "", "checkout")
		g.ObserveSpanTopology(tenant, "trace-1", "span-payments", "span-checkout", "payments")
	}

	edge = getCallEdge(g.ServiceMap(ctx, 0), "checkout", "payments")
	if edge.CallCount != wantCount || edge.TotalMs != wantTotalMs || edge.Weight != wantWeight {
		t.Fatalf("observer polluted sampled edge aggregates: got {CallCount:%d TotalMs:%g Weight:%g}, want {CallCount:%d TotalMs:%g Weight:%g} (aggregates must come from sampled spans only)",
			edge.CallCount, edge.TotalMs, edge.Weight, wantCount, wantTotalMs, wantWeight)
	}
}

// TestObserveSpanTopology_BoundedLRUEvicts confirms the per-tenant
// spanID→service map evicts the oldest entries past its cap, so a long-running
// process cannot grow it without bound. After eviction the evicted parent can no
// longer be resolved, so a late child referencing it forms no edge.
func TestObserveSpanTopology_BoundedLRUEvicts(t *testing.T) {
	g := newTestGraphRAG(t)
	tenant := storage.DefaultTenantID
	cap := topologyObserverSpanCap

	// Observe one "anchor" parent span in service "anchor-svc".
	g.ObserveSpanTopology(tenant, "trace-anchor", "anchor-span", "", "anchor-svc")

	// Now observe `cap` further root spans (no parent ⇒ no edges) to push the
	// anchor past the LRU window. Each distinct spanID consumes one slot.
	for i := 0; i < cap; i++ {
		g.ObserveSpanTopology(tenant, "trace-fill", fmt.Sprintf("fill-%d", i), "", "filler-svc")
	}

	// A child referencing the now-evicted anchor parent must NOT form an edge,
	// because the parent's service is no longer resolvable.
	g.ObserveSpanTopology(tenant, "trace-late", "late-child", "anchor-span", "late-svc")

	ctx := storage.WithTenantContext(context.Background(), tenant)
	if hasCallEdge(g.ServiceMap(ctx, 0), "anchor-svc", "late-svc") {
		t.Fatalf("anchor parent was not evicted: edge formed from an entry that should have aged out of the bounded LRU")
	}

	// Sanity: the LRU map size is bounded at the cap.
	if got := g.topologyObserverLen(tenant); got > cap {
		t.Fatalf("LRU size %d exceeds cap %d", got, cap)
	}
}

// TestObserveSpanTopology_DedupBoundsEdges confirms that observing the same
// service-pair many times keeps the edge set bounded to one edge per pair.
func TestObserveSpanTopology_DedupBoundsEdges(t *testing.T) {
	g := newTestGraphRAG(t)
	tenant := storage.DefaultTenantID

	// Distinct span IDs but the SAME service pair (checkout→payments) repeated.
	for i := 0; i < 500; i++ {
		parent := fmt.Sprintf("p-%d", i)
		child := fmt.Sprintf("c-%d", i)
		g.ObserveSpanTopology(tenant, "trace-x", parent, "", "checkout")
		g.ObserveSpanTopology(tenant, "trace-x", child, parent, "payments")
	}

	ctx := storage.WithTenantContext(context.Background(), tenant)
	edges := allCallEdges(g.ServiceMap(ctx, 0))
	if len(edges) != 1 {
		t.Fatalf("expected exactly 1 deduped edge for the single service-pair, got %d: %+v", len(edges), edges)
	}
	if edges[0].FromID != "checkout" || edges[0].ToID != "payments" {
		t.Fatalf("edge = %s→%s, want checkout→payments", edges[0].FromID, edges[0].ToID)
	}
}

// TestObserveSpanTopology_TenantIsolation confirms edges observed under one
// tenant never leak into another tenant's ServiceMap.
func TestObserveSpanTopology_TenantIsolation(t *testing.T) {
	g := newTestGraphRAG(t)

	// Tenant A: orders→inventory.
	g.ObserveSpanTopology("tenant-a", "t-a", "a-parent", "", "orders")
	g.ObserveSpanTopology("tenant-a", "t-a", "a-child", "a-parent", "inventory")
	// Tenant B: gateway→auth.
	g.ObserveSpanTopology("tenant-b", "t-b", "b-parent", "", "gateway")
	g.ObserveSpanTopology("tenant-b", "t-b", "b-child", "b-parent", "auth")

	ctxA := storage.WithTenantContext(context.Background(), "tenant-a")
	ctxB := storage.WithTenantContext(context.Background(), "tenant-b")

	if !hasCallEdge(g.ServiceMap(ctxA, 0), "orders", "inventory") {
		t.Fatalf("tenant-a missing its own orders→inventory edge")
	}
	if !hasCallEdge(g.ServiceMap(ctxB, 0), "gateway", "auth") {
		t.Fatalf("tenant-b missing its own gateway→auth edge")
	}
	// Cross-tenant leakage in BOTH directions.
	if hasCallEdge(g.ServiceMap(ctxA, 0), "gateway", "auth") {
		t.Fatalf("tenant-a leaked tenant-b's gateway→auth edge")
	}
	if hasCallEdge(g.ServiceMap(ctxB, 0), "orders", "inventory") {
		t.Fatalf("tenant-b leaked tenant-a's orders→inventory edge")
	}

	// A parent span ID observed only under tenant A must not resolve under
	// tenant B (the LRU is per-tenant): a B child reusing "a-parent" forms no edge.
	g.ObserveSpanTopology("tenant-b", "t-b", "b-child-2", "a-parent", "billing")
	if hasCallEdge(g.ServiceMap(ctxB, 0), "orders", "billing") {
		t.Fatalf("tenant-b resolved a parent span that belongs to tenant-a's LRU")
	}
}

// --- test helpers ---

func allCallEdges(entries []ServiceMapEntry) []*Edge {
	seen := make(map[string]*Edge)
	for _, e := range entries {
		for _, edge := range e.CallsTo {
			seen[edge.FromID+"|"+edge.ToID] = edge
		}
		for _, edge := range e.CalledBy {
			seen[edge.FromID+"|"+edge.ToID] = edge
		}
	}
	out := make([]*Edge, 0, len(seen))
	for _, edge := range seen {
		out = append(out, edge)
	}
	return out
}

func getCallEdge(entries []ServiceMapEntry, from, to string) *Edge {
	for _, edge := range allCallEdges(entries) {
		if edge.FromID == from && edge.ToID == to {
			return edge
		}
	}
	return nil
}

func hasCallEdge(entries []ServiceMapEntry, from, to string) bool {
	return getCallEdge(entries, from, to) != nil
}
