package aggregate

import (
	"testing"
	"time"
)

// reduceSpanAt folds one span into r at ts.
func reduceSpanAt(r *Reducer, tenant, service, op string, status int32, durMicros float64, ts time.Time) {
	r.ReduceSpan(SpanInput{
		Tenant:         tenant,
		Service:        service,
		SpanName:       op,
		StatusCode:     status,
		Timestamp:      ts,
		DurationMicros: durMicros,
	})
}

func findService(t *testing.T, snap TopologySnapshot, name string) TopologyService {
	t.Helper()
	for _, s := range snap.Services {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("service %q not in snapshot %+v", name, snap.Services)
	return TopologyService{}
}

func totalCount(windows []TopologyWindow) (count, errors uint64) {
	for _, w := range windows {
		count += w.Count
		errors += w.ErrorCount
	}
	return count, errors
}

// TestTopologySnapshotCarriesServicesOperationsAndEdges proves the projection
// records all three entity kinds from a single Export, with the edge derived
// from a caller the reducer was handed explicitly.
func TestTopologySnapshotCarriesServicesOperationsAndEdges(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e := testEngine(t, now)

	r := e.NewReducer(now)
	reduceSpanAt(r, "acme", "checkout", "/pay", 0, 5000, now)
	reduceSpanAt(r, "acme", "checkout", "/pay", 2, 9000, now)
	r.ReduceEdge(EdgeInput{
		Tenant: "acme", Caller: "gateway", Callee: "checkout",
		SpanName: "/pay", Timestamp: now, DurationMicros: 5000,
	})
	if _, err := e.ApplyReducerErr(r); err != nil {
		t.Fatalf("ApplyReducerErr: %v", err)
	}

	snap := e.TopologySnapshot("acme")
	if snap.Revision == 0 {
		t.Fatalf("snapshot revision did not advance: %+v", snap)
	}
	svc := findService(t, snap, "checkout")
	count, errs := totalCount(svc.Windows)
	if count != 2 || errs != 1 {
		t.Fatalf("checkout totals = (%d, %d), want (2, 1)", count, errs)
	}
	if len(snap.Operations) != 1 || snap.Operations[0].Operation != "/pay" {
		t.Fatalf("operations = %+v, want one /pay entry", snap.Operations)
	}
	if len(snap.Edges) != 1 || snap.Edges[0].Caller != "gateway" || snap.Edges[0].Callee != "checkout" {
		t.Fatalf("edges = %+v, want one gateway->checkout entry", snap.Edges)
	}
	if snap.Truncated() {
		t.Fatalf("snapshot reports truncation with generous caps: %+v", snap)
	}
}

// TestTopologyRevisionUnchangedWithoutFacts proves an Export that produces no
// topology facts does not move a tenant's revision — the property the GraphRAG
// consumer's "skip unchanged revision" gate rests on.
func TestTopologyRevisionUnchangedWithoutFacts(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e := testEngine(t, now)

	r := e.NewReducer(now)
	reduceSpanAt(r, "acme", "checkout", "/pay", 0, 1000, now)
	if _, err := e.ApplyReducerErr(r); err != nil {
		t.Fatalf("ApplyReducerErr: %v", err)
	}
	first := e.TopologyRevision("acme")

	// A second Export carrying only another tenant's traffic must not move
	// acme's revision, even though the engine revision advances.
	r2 := e.NewReducer(now)
	reduceSpanAt(r2, "other", "billing", "/charge", 0, 1000, now)
	if _, err := e.ApplyReducerErr(r2); err != nil {
		t.Fatalf("ApplyReducerErr: %v", err)
	}
	if got := e.TopologyRevision("acme"); got != first {
		t.Fatalf("acme revision moved on another tenant's traffic: %d -> %d", first, got)
	}
	if e.TopologyRevision("other") == 0 {
		t.Fatalf("other tenant revision did not advance")
	}
}

// TestTopologyLatePointsExcludedFromProjection proves the projection honours
// the same lateness horizon the shards do: a point the reducer excluded never
// reaches the topology.
func TestTopologyLatePointsExcludedFromProjection(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e := testEngine(t, now)

	r := e.NewReducer(now)
	reduceSpanAt(r, "acme", "checkout", "/pay", 0, 1000, now.Add(-2*time.Hour))
	if _, err := e.ApplyReducerErr(r); err != nil {
		t.Fatalf("ApplyReducerErr: %v", err)
	}
	if snap := e.TopologySnapshot("acme"); !snap.Empty() {
		t.Fatalf("late point entered the topology: %+v", snap)
	}
}

// TestTopologyPartialWindowMetadata proves a still-open window reports Closed
// false with a real Elapsed, and a window past the lateness horizon reports
// Final. This metadata is the entire partial-window guard.
func TestTopologyPartialWindowMetadata(t *testing.T) {
	start := mustTime(t, "2026-08-21T12:00:00Z")
	clock := start.Add(2 * time.Minute)
	e, err := NewEngine(EngineConfig{Mode: ModeShadow, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	r := e.NewReducer(clock)
	reduceSpanAt(r, "acme", "checkout", "/pay", 0, 1000, clock)
	if _, err := e.ApplyReducerErr(r); err != nil {
		t.Fatalf("ApplyReducerErr: %v", err)
	}

	svc := findService(t, e.TopologySnapshot("acme"), "checkout")
	w := svc.Windows[len(svc.Windows)-1]
	if w.Closed || w.Final {
		t.Fatalf("window reported closed/final while still open: %+v", w)
	}
	if w.Elapsed != 2*time.Minute {
		t.Fatalf("elapsed = %v, want 2m", w.Elapsed)
	}

	// Advance past window end + lateness: the same window is now final.
	clock = start.Add(WindowSize + AllowedLateness + time.Minute)
	svc = findService(t, e.TopologySnapshot("acme"), "checkout")
	w = svc.Windows[0]
	if !w.Closed || !w.Final {
		t.Fatalf("window not closed/final past the lateness horizon: %+v", w)
	}
	if w.Elapsed != WindowSize {
		t.Fatalf("closed window elapsed = %v, want %v", w.Elapsed, WindowSize)
	}
}

// TestTopologyServiceCapTruncatesAndReports proves the projection caps are
// enforced and that breaching one is REPORTED rather than silently producing a
// partial topology a consumer would present as complete.
func TestTopologyServiceCapTruncatesAndReports(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e, err := NewEngine(EngineConfig{
		Mode:     ModeShadow,
		Now:      func() time.Time { return now },
		Topology: TopologyConfig{MaxServices: 2},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	r := e.NewReducer(now)
	for _, svc := range []string{"a", "b", "c", "d"} {
		reduceSpanAt(r, "acme", svc, "/op", 0, 1000, now)
	}
	if _, err := e.ApplyReducerErr(r); err != nil {
		t.Fatalf("ApplyReducerErr: %v", err)
	}
	snap := e.TopologySnapshot("acme")
	if len(snap.Services) != 2 {
		t.Fatalf("services = %d, want 2 (capped)", len(snap.Services))
	}
	if !snap.Truncated() || snap.DroppedServices == 0 {
		t.Fatalf("cap breach not reported: %+v", snap)
	}
}

// TestEdgeResolverJoinsParentToCaller proves the caller-resolution join: a
// child span whose parent was seen in a different service resolves, a
// same-service parent does not (an internal call is not a topology edge), and
// an unknown parent does not invent one.
func TestEdgeResolverJoinsParentToCaller(t *testing.T) {
	r := NewEdgeResolver(1000)

	if _, ok := r.Observe("acme", "parent-1", "", "gateway"); ok {
		t.Fatalf("a root span resolved a caller")
	}
	caller, ok := r.Observe("acme", "child-1", "parent-1", "checkout")
	if !ok || caller != "gateway" {
		t.Fatalf("caller = (%q, %v), want (gateway, true)", caller, ok)
	}
	if _, ok := r.Observe("acme", "child-2", "parent-1", "gateway"); ok {
		t.Fatalf("same-service parent produced an edge")
	}
	if _, ok := r.Observe("acme", "child-3", "unknown", "checkout"); ok {
		t.Fatalf("unknown parent invented a caller")
	}
	// Tenant isolation: another tenant's span ID must not resolve.
	if _, ok := r.Observe("other", "child-4", "parent-1", "checkout"); ok {
		t.Fatalf("cross-tenant parent resolved")
	}
}

// TestEdgeResolverEvictsPastCap proves the span memory is bounded.
func TestEdgeResolverEvictsPastCap(t *testing.T) {
	r := NewEdgeResolver(edgeResolverShards) // one slot per shard
	for i := 0; i < 500; i++ {
		r.Observe("acme", string(rune('a'+i%26))+string(rune('0'+i/26)), "", "svc")
	}
	if got := r.Len(); got > edgeResolverShards {
		t.Fatalf("resolver held %d entries, cap is %d", got, edgeResolverShards)
	}
}
