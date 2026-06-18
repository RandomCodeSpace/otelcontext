package graphrag

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

// edgeIDs collects from→to keys so two []*Edge slices can be compared
// order-independently.
func edgeIDs(edges []*Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.FromID+"->"+e.ToID)
	}
	sort.Strings(out)
	return out
}

func fullScanFrom(s *ServiceStore, service string) []*Edge {
	var out []*Edge
	for _, e := range s.Edges {
		if e.Type == EdgeCalls && e.FromID == service {
			out = append(out, e)
		}
	}
	return out
}

func fullScanTo(s *ServiceStore, service string) []*Edge {
	var out []*Edge
	for _, e := range s.Edges {
		if e.Type == EdgeCalls && e.ToID == service {
			out = append(out, e)
		}
	}
	return out
}

// TestAdjacencyIndexMatchesFullScan asserts the indexed CallEdgesFrom/To return
// exactly the same CALLS edges a full Edges-map scan would, across both the
// aggregating UpsertCallEdge path and the existence-only EnsureCallEdge path.
func TestAdjacencyIndexMatchesFullScan(t *testing.T) {
	g := newTestGraphRAG(t)
	now := time.Now()
	s := g.storesForTenant("t").service

	// Aggregating path.
	s.UpsertCallEdge("a", "b", 5, false, now)
	s.UpsertCallEdge("a", "c", 5, true, now)
	s.UpsertCallEdge("b", "c", 5, false, now)
	// Existence-only (pre-sample topology observer) path.
	s.EnsureService("d", now)
	s.EnsureCallEdge("a", "d", now)
	s.EnsureCallEdge("d", "c", now)
	// Repeat upserts must not duplicate index entries.
	s.UpsertCallEdge("a", "b", 5, false, now)
	s.EnsureCallEdge("a", "d", now)

	for _, svc := range []string{"a", "b", "c", "d"} {
		gotFrom := edgeIDs(s.CallEdgesFrom(svc))
		wantFrom := edgeIDs(fullScanFrom(s, svc))
		if fmt.Sprint(gotFrom) != fmt.Sprint(wantFrom) {
			t.Errorf("CallEdgesFrom(%q) = %v, full scan = %v", svc, gotFrom, wantFrom)
		}
		gotTo := edgeIDs(s.CallEdgesTo(svc))
		wantTo := edgeIDs(fullScanTo(s, svc))
		if fmt.Sprint(gotTo) != fmt.Sprint(wantTo) {
			t.Errorf("CallEdgesTo(%q) = %v, full scan = %v", svc, gotTo, wantTo)
		}
	}

	// a→b should be a single edge despite the repeated upsert.
	if from := s.CallEdgesFrom("a"); len(from) != 3 { // a→b, a→c, a→d
		t.Fatalf("CallEdgesFrom(a) = %d edges, want 3", len(from))
	}
}

// TestOperationsForServiceIndex asserts the per-service operations index matches
// a filter over the full Operations map.
func TestOperationsForServiceIndex(t *testing.T) {
	g := newTestGraphRAG(t)
	now := time.Now()
	s := g.storesForTenant("t").service

	s.UpsertOperation("a", "GET /x", 1, false, now)
	s.UpsertOperation("a", "GET /y", 1, false, now)
	s.UpsertOperation("b", "POST /z", 1, false, now)
	s.UpsertOperation("a", "GET /x", 1, true, now) // repeat: no new index entry

	if ops := s.OperationsForService("a"); len(ops) != 2 {
		t.Fatalf("OperationsForService(a) = %d, want 2", len(ops))
	}
	if ops := s.OperationsForService("b"); len(ops) != 1 {
		t.Fatalf("OperationsForService(b) = %d, want 1", len(ops))
	}
	if ops := s.OperationsForService("missing"); ops != nil {
		t.Fatalf("OperationsForService(missing) = %v, want nil", ops)
	}
}

// TestServiceMapAroundBounded asserts the focused service map only returns the
// subgraph reachable within depth hops, while the unfocused full map returns all
// services.
func TestServiceMapAroundBounded(t *testing.T) {
	g := newTestGraphRAG(t)
	now := time.Now()
	ctx := context.Background()
	s := g.storesFor(ctx).service

	// Chain a→b→c→d plus an isolated island e→f.
	for _, sv := range []string{"a", "b", "c", "d", "e", "f"} {
		s.UpsertService(sv, 1, false, now)
	}
	s.UpsertCallEdge("a", "b", 1, false, now)
	s.UpsertCallEdge("b", "c", 1, false, now)
	s.UpsertCallEdge("c", "d", 1, false, now)
	s.UpsertCallEdge("e", "f", 1, false, now)

	// depth 1 from b → {b, a, c}.
	got := g.ServiceMapAround(ctx, "b", 1)
	if len(got) != 3 {
		t.Fatalf("ServiceMapAround(b, 1) = %d entries, want 3", len(got))
	}

	// depth 2 from a → {a, b, c} (downstream only reaches c at 2 hops).
	got = g.ServiceMapAround(ctx, "a", 2)
	if len(got) != 3 {
		t.Fatalf("ServiceMapAround(a, 2) = %d entries, want 3", len(got))
	}

	// Unknown seed → nil.
	if got := g.ServiceMapAround(ctx, "nope", 3); got != nil {
		t.Fatalf("ServiceMapAround(nope) = %v, want nil", got)
	}

	// Full map returns every service (island included).
	if full := g.ServiceMap(ctx, 0); len(full) != 6 {
		t.Fatalf("ServiceMap = %d entries, want 6", len(full))
	}
}

// BenchmarkServiceMap exercises the full topology dump at 200 services / ~800
// edges to confirm the adjacency index keeps it linear.
func BenchmarkServiceMap(b *testing.B) {
	g := newTestGraphRAG(b)
	now := time.Now()
	ctx := context.Background()
	s := g.storesFor(ctx).service

	const n = 200
	for i := 0; i < n; i++ {
		s.UpsertService(fmt.Sprintf("svc-%d", i), 1, false, now)
	}
	// ~4 outgoing edges per service.
	for i := 0; i < n; i++ {
		for k := 1; k <= 4; k++ {
			s.UpsertCallEdge(fmt.Sprintf("svc-%d", i), fmt.Sprintf("svc-%d", (i+k)%n), 1, false, now)
		}
		s.UpsertOperation(fmt.Sprintf("svc-%d", i), "GET /", 1, false, now)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.ServiceMap(ctx, 0)
	}
}
