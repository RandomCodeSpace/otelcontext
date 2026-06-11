package storage

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"gorm.io/gorm"
)

// referenceServiceMapMetrics is a verbatim copy of the pre-P2.1
// GetServiceMapMetrics aggregation: load full Span rows (including the
// CompressedText attributes column) and group in Go. It exists so the
// SQL-aggregated implementation can be proven equivalent on a shared fixture.
//
// Note: this old path never populated ServiceMapNode.ErrorCount or
// ServiceMapEdge.ErrorRate — both stayed 0 — so node error counts are
// asserted separately in TestGetServiceMapMetrics_NodeErrorCounts.
func referenceServiceMapMetrics(t *testing.T, repo *Repository, ctx context.Context, start, end time.Time) *ServiceMapMetrics {
	t.Helper()
	tenant := TenantFromContext(ctx)
	var spans []Span
	query := repo.db.WithContext(ctx).Model(&Span{}).Where(sqlWhereTenantID, tenant)
	if !start.IsZero() && !end.IsZero() {
		query = query.Where("start_time BETWEEN ? AND ?", start, end)
	}
	if err := query.Limit(serviceMapSpanLimit).Find(&spans).Error; err != nil {
		t.Fatalf("reference span load: %v", err)
	}

	spanMap := make(map[string]Span)
	nodeStats := make(map[string]*ServiceMapNode)
	edgeStats := make(map[string]*ServiceMapEdge)

	for _, s := range spans {
		spanMap[s.SpanID] = s
		if s.ServiceName == "" {
			continue
		}
		if _, ok := nodeStats[s.ServiceName]; !ok {
			nodeStats[s.ServiceName] = &ServiceMapNode{Name: s.ServiceName}
		}
		ns := nodeStats[s.ServiceName]
		ns.TotalTraces++
		ns.AvgLatencyMs += float64(s.Duration)
	}

	nodes := make([]ServiceMapNode, 0) //nolint:prealloc // verbatim copy of the old implementation
	for _, ns := range nodeStats {
		if ns.TotalTraces > 0 {
			ns.AvgLatencyMs = ns.AvgLatencyMs / float64(ns.TotalTraces) / 1000.0
			ns.AvgLatencyMs = math.Round(ns.AvgLatencyMs*100) / 100
		}
		nodes = append(nodes, *ns)
	}

	for _, s := range spans {
		if s.ParentSpanID == "" || s.ParentSpanID == "0000000000000000" {
			continue
		}
		parent, ok := spanMap[s.ParentSpanID]
		if !ok {
			continue
		}
		source := parent.ServiceName
		target := s.ServiceName
		if source == "" || target == "" || source == target {
			continue
		}
		key := fmt.Sprintf("%s->%s", source, target)
		if _, ok := edgeStats[key]; !ok {
			edgeStats[key] = &ServiceMapEdge{Source: source, Target: target}
		}
		es := edgeStats[key]
		es.CallCount++
		es.AvgLatencyMs += float64(s.Duration)
	}

	edges := make([]ServiceMapEdge, 0) //nolint:prealloc // verbatim copy of the old implementation
	for _, es := range edgeStats {
		if es.CallCount > 0 {
			es.AvgLatencyMs = es.AvgLatencyMs / float64(es.CallCount) / 1000.0
			es.AvgLatencyMs = math.Round(es.AvgLatencyMs*100) / 100
		}
		edges = append(edges, *es)
	}

	return &ServiceMapMetrics{Nodes: nodes, Edges: edges}
}

// serviceMapFixtureWindow is the query window the fixture spans sit inside.
func serviceMapFixtureWindow() (start, end time.Time) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return base, base.Add(10 * time.Minute)
}

// seedServiceMapFixture inserts a topology exercising every branch of the
// aggregation: cross-service parent-child edges, error spans, same-service
// child (no edge), root span, zero parent id, dangling parent id, empty
// service name, out-of-window span, and a foreign-tenant span.
//
// Expected (window-scoped, tenant "default"):
//
//	nodes: svc-a {2 spans, 0 errors, 55ms}   — a1 root 100000µs, a2 child-of-a1 10000µs
//	       svc-b {4 spans, 1 error,  25ms}   — b1 50000µs ERROR, b2 30000µs, m1 7000µs
//	                                           (dangling parent), f1 13000µs (parent has
//	                                           empty service → no edge)
//	       svc-c {2 spans, 1 error,  30ms}   — c1 20000µs ERROR (parent b1),
//	                                           z1 40000µs (parent 0000…)
//	edges: svc-a→svc-b {2 calls, 40ms}, svc-b→svc-c {1 call, 20ms}
func seedServiceMapFixture(t *testing.T, repo *Repository) {
	t.Helper()
	start, _ := serviceMapFixtureWindow()
	in := start.Add(1 * time.Minute)
	out := start.Add(20 * time.Minute)

	mk := func(tenant, spanID, parentID, service string, dur int64, status string, ts time.Time) Span {
		return Span{
			TenantID:       tenant,
			TraceID:        "trace-svcmap-1",
			SpanID:         spanID,
			ParentSpanID:   parentID,
			OperationName:  "op-" + spanID,
			StartTime:      ts,
			EndTime:        ts.Add(time.Duration(dur) * time.Microsecond),
			Duration:       dur,
			ServiceName:    service,
			Status:         status,
			AttributesJSON: CompressedText(`{"http.method":"GET","fixture":"` + spanID + `"}`),
		}
	}

	spans := []Span{
		mk("default", "a1", "", "svc-a", 100000, "STATUS_CODE_UNSET", in),
		mk("default", "a2", "a1", "svc-a", 10000, "STATUS_CODE_UNSET", in),               // same-service child: node yes, edge no
		mk("default", "b1", "a1", "svc-b", 50000, "STATUS_CODE_ERROR", in),               // edge a→b
		mk("default", "b2", "a1", "svc-b", 30000, "STATUS_CODE_UNSET", in),               // edge a→b
		mk("default", "m1", "ffffffffffffffff", "svc-b", 7000, "STATUS_CODE_UNSET", in),  // dangling parent: no edge
		mk("default", "e1", "a1", "", 5000, "STATUS_CODE_UNSET", in),                     // empty service: no node, no edge
		mk("default", "f1", "e1", "svc-b", 13000, "STATUS_CODE_UNSET", in),               // parent has empty service: no edge
		mk("default", "c1", "b1", "svc-c", 20000, "STATUS_CODE_ERROR", in),               // edge b→c
		mk("default", "z1", "0000000000000000", "svc-c", 40000, "STATUS_CODE_UNSET", in), // zero parent: no edge
		mk("default", "o1", "", "svc-a", 999999, "STATUS_CODE_ERROR", out),               // outside window
		mk("tenant-b", "x1", "", "svc-a", 888888, "STATUS_CODE_ERROR", in),               // foreign tenant
	}
	if err := repo.BatchCreateSpans(spans); err != nil {
		t.Fatalf("seedServiceMapFixture: %v", err)
	}
}

func sortServiceMap(m *ServiceMapMetrics) {
	sort.Slice(m.Nodes, func(i, j int) bool { return m.Nodes[i].Name < m.Nodes[j].Name })
	sort.Slice(m.Edges, func(i, j int) bool {
		if m.Edges[i].Source != m.Edges[j].Source {
			return m.Edges[i].Source < m.Edges[j].Source
		}
		return m.Edges[i].Target < m.Edges[j].Target
	})
}

// TestGetServiceMapMetrics_MatchesReferenceImplementation proves the
// SQL-aggregated implementation is output-equivalent to the old full-row
// in-Go aggregation on a fixture covering every branch. ErrorCount is the
// one intentional delta (the old path left it permanently 0 — a latent bug
// since downstream buildGraphFromDB divides ErrorCount/TotalTraces) and is
// asserted in TestGetServiceMapMetrics_NodeErrorCounts instead.
func TestGetServiceMapMetrics_MatchesReferenceImplementation(t *testing.T) {
	repo := newTestRepo(t)
	seedServiceMapFixture(t, repo)
	ctx := context.Background()
	start, end := serviceMapFixtureWindow()

	want := referenceServiceMapMetrics(t, repo, ctx, start, end)
	got, err := repo.GetServiceMapMetrics(ctx, start, end)
	if err != nil {
		t.Fatalf("GetServiceMapMetrics: %v", err)
	}

	sortServiceMap(want)
	sortServiceMap(got)

	if len(got.Nodes) != len(want.Nodes) {
		t.Fatalf("node count: got %d want %d (%+v vs %+v)", len(got.Nodes), len(want.Nodes), got.Nodes, want.Nodes)
	}
	for i := range want.Nodes {
		g, w := got.Nodes[i], want.Nodes[i]
		if g.Name != w.Name || g.TotalTraces != w.TotalTraces || g.AvgLatencyMs != w.AvgLatencyMs {
			t.Errorf("node %d mismatch: got %+v want %+v", i, g, w)
		}
	}
	if len(got.Edges) != len(want.Edges) {
		t.Fatalf("edge count: got %d want %d (%+v vs %+v)", len(got.Edges), len(want.Edges), got.Edges, want.Edges)
	}
	for i := range want.Edges {
		if got.Edges[i] != want.Edges[i] {
			t.Errorf("edge %d mismatch: got %+v want %+v", i, got.Edges[i], want.Edges[i])
		}
	}
}

// TestGetServiceMapMetrics_NodeErrorCounts asserts that node error counts are
// populated from span status. The old implementation never set ErrorCount
// (always 0), so this test documents the intentional fix that landed with the
// SQL aggregation.
func TestGetServiceMapMetrics_NodeErrorCounts(t *testing.T) {
	repo := newTestRepo(t)
	seedServiceMapFixture(t, repo)
	start, end := serviceMapFixtureWindow()

	got, err := repo.GetServiceMapMetrics(context.Background(), start, end)
	if err != nil {
		t.Fatalf("GetServiceMapMetrics: %v", err)
	}
	sortServiceMap(got)

	wantErrors := map[string]int64{"svc-a": 0, "svc-b": 1, "svc-c": 1}
	if len(got.Nodes) != len(wantErrors) {
		t.Fatalf("node count: got %d want %d (%+v)", len(got.Nodes), len(wantErrors), got.Nodes)
	}
	for _, n := range got.Nodes {
		if n.ErrorCount != wantErrors[n.Name] {
			t.Errorf("node %s ErrorCount = %d, want %d", n.Name, n.ErrorCount, wantErrors[n.Name])
		}
	}
}

// TestGetServiceMapMetrics_FixtureValues pins the exact expected fixture
// output so a regression in BOTH implementations (reference and SQL) cannot
// slip through the equality test unnoticed.
func TestGetServiceMapMetrics_FixtureValues(t *testing.T) {
	repo := newTestRepo(t)
	seedServiceMapFixture(t, repo)
	start, end := serviceMapFixtureWindow()

	got, err := repo.GetServiceMapMetrics(context.Background(), start, end)
	if err != nil {
		t.Fatalf("GetServiceMapMetrics: %v", err)
	}
	sortServiceMap(got)

	wantNodes := []ServiceMapNode{
		{Name: "svc-a", TotalTraces: 2, ErrorCount: 0, AvgLatencyMs: 55},
		{Name: "svc-b", TotalTraces: 4, ErrorCount: 1, AvgLatencyMs: 25},
		{Name: "svc-c", TotalTraces: 2, ErrorCount: 1, AvgLatencyMs: 30},
	}
	wantEdges := []ServiceMapEdge{
		{Source: "svc-a", Target: "svc-b", CallCount: 2, AvgLatencyMs: 40},
		{Source: "svc-b", Target: "svc-c", CallCount: 1, AvgLatencyMs: 20},
	}

	if len(got.Nodes) != len(wantNodes) {
		t.Fatalf("nodes: got %+v want %+v", got.Nodes, wantNodes)
	}
	for i := range wantNodes {
		if got.Nodes[i] != wantNodes[i] {
			t.Errorf("node %d: got %+v want %+v", i, got.Nodes[i], wantNodes[i])
		}
	}
	if len(got.Edges) != len(wantEdges) {
		t.Fatalf("edges: got %+v want %+v", got.Edges, wantEdges)
	}
	for i := range wantEdges {
		if got.Edges[i] != wantEdges[i] {
			t.Errorf("edge %d: got %+v want %+v", i, got.Edges[i], wantEdges[i])
		}
	}
}

// TestGetServiceMapMetrics_TenantScoped verifies the foreign tenant sees only
// its own spans through the same code path.
func TestGetServiceMapMetrics_TenantScoped(t *testing.T) {
	repo := newTestRepo(t)
	seedServiceMapFixture(t, repo)
	start, end := serviceMapFixtureWindow()
	ctx := WithTenantContext(context.Background(), "tenant-b")

	got, err := repo.GetServiceMapMetrics(ctx, start, end)
	if err != nil {
		t.Fatalf("GetServiceMapMetrics: %v", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].Name != "svc-a" || got.Nodes[0].TotalTraces != 1 {
		t.Fatalf("tenant-b nodes: %+v", got.Nodes)
	}
	if got.Nodes[0].ErrorCount != 1 {
		t.Fatalf("tenant-b ErrorCount = %d, want 1", got.Nodes[0].ErrorCount)
	}
	if len(got.Edges) != 0 {
		t.Fatalf("tenant-b edges: %+v", got.Edges)
	}
}

// TestGetServiceMapMetrics_ZeroWindowIncludesAll preserves the legacy
// semantics: a zero start or end skips the time filter entirely.
func TestGetServiceMapMetrics_ZeroWindowIncludesAll(t *testing.T) {
	repo := newTestRepo(t)
	seedServiceMapFixture(t, repo)

	got, err := repo.GetServiceMapMetrics(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetServiceMapMetrics: %v", err)
	}
	want := referenceServiceMapMetrics(t, repo, context.Background(), time.Time{}, time.Time{})
	sortServiceMap(got)
	sortServiceMap(want)

	// The out-of-window span o1 (svc-a) must now be included: 3 svc-a spans.
	var svcA *ServiceMapNode
	for i := range got.Nodes {
		if got.Nodes[i].Name == "svc-a" {
			svcA = &got.Nodes[i]
		}
	}
	if svcA == nil || svcA.TotalTraces != 3 {
		t.Fatalf("zero window svc-a node: %+v", got.Nodes)
	}
	if len(got.Nodes) != len(want.Nodes) || len(got.Edges) != len(want.Edges) {
		t.Fatalf("zero window mismatch vs reference: got %+v / %+v want %+v / %+v",
			got.Nodes, got.Edges, want.Nodes, want.Edges)
	}
}

// TestGetServiceMapMetrics_Empty returns non-nil empty slices on a fresh DB
// (the JSON API contract expects [] rather than null).
func TestGetServiceMapMetrics_Empty(t *testing.T) {
	repo := newTestRepo(t)
	got, err := repo.GetServiceMapMetrics(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetServiceMapMetrics: %v", err)
	}
	if got.Nodes == nil || got.Edges == nil {
		t.Fatalf("expected non-nil empty slices, got %+v", got)
	}
	if len(got.Nodes) != 0 || len(got.Edges) != 0 {
		t.Fatalf("expected empty result, got %+v", got)
	}
}

// TestGetServiceMapMetrics_CancelledContext surfaces query errors.
func TestGetServiceMapMetrics_CancelledContext(t *testing.T) {
	repo := newTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.GetServiceMapMetrics(ctx, time.Time{}, time.Time{}); err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// TestGetServiceMapMetrics_EdgeQueryError exercises the second (edge-pass)
// error branch. The node aggregate runs via Scan, which does not pass through
// GORM's Query callback chain; the edge scan runs via Find, which does. A
// before-query callback therefore fires exactly once — for the edge query —
// and cancels the context just before it executes, failing only that query.
func TestGetServiceMapMetrics_EdgeQueryError(t *testing.T) {
	repo := newTestRepo(t)
	seedServiceMapFixture(t, repo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := repo.db.Callback().Query().Before("gorm:query").Register("test_cancel_edge_query", func(*gorm.DB) {
		cancel()
	})
	if err != nil {
		t.Fatalf("register callback: %v", err)
	}
	t.Cleanup(func() { _ = repo.db.Callback().Query().Remove("test_cancel_edge_query") })

	if _, err := repo.GetServiceMapMetrics(ctx, time.Time{}, time.Time{}); err == nil {
		t.Fatal("expected edge query error after context cancellation")
	}
}

// TestGetServiceMapMetrics_EdgeRowLimit lowers serviceMapSpanLimit so the
// warning path runs: edges are computed from the truncated scan while node
// stats (GROUP BY, unbounded) still cover every row.
func TestGetServiceMapMetrics_EdgeRowLimit(t *testing.T) {
	repo := newTestRepo(t)
	seedServiceMapFixture(t, repo)
	start, end := serviceMapFixtureWindow()

	orig := serviceMapSpanLimit
	serviceMapSpanLimit = 4
	t.Cleanup(func() { serviceMapSpanLimit = orig })

	got, err := repo.GetServiceMapMetrics(context.Background(), start, end)
	if err != nil {
		t.Fatalf("GetServiceMapMetrics: %v", err)
	}
	// Node aggregation is not subject to the edge-scan limit.
	if len(got.Nodes) != 3 {
		t.Fatalf("expected all 3 nodes despite edge row limit, got %+v", got.Nodes)
	}
	// The edge pass saw at most 4 of the 9 in-window rows.
	var calls int64
	for _, e := range got.Edges {
		calls += e.CallCount
	}
	if calls > 3 {
		t.Fatalf("edge pass exceeded the row limit: %+v", got.Edges)
	}
}

// BenchmarkGetServiceMapMetrics measures the aggregation over a few thousand
// seeded spans with realistic compressed attribute payloads — the workload
// that made the old full-row implementation take seconds on the dashboard.
func BenchmarkGetServiceMapMetrics(b *testing.B) {
	db, err := NewDatabase("sqlite", ":memory:")
	if err != nil {
		b.Fatalf("NewDatabase: %v", err)
	}
	if err := AutoMigrateModels(db, "sqlite"); err != nil {
		b.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := &Repository{db: db, driver: "sqlite"}
	defer repo.Close()

	const (
		numSpans    = 5000
		numServices = 10
	)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	spans := make([]Span, numSpans)
	for i := range numSpans {
		svc := fmt.Sprintf("svc-%d", i%numServices)
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("%016x", i-1) // chain → cross-service edges
		}
		status := "STATUS_CODE_UNSET"
		if i%20 == 0 {
			status = "STATUS_CODE_ERROR"
		}
		spans[i] = Span{
			TenantID:      "default",
			TraceID:       fmt.Sprintf("trace-%04d", i/50),
			SpanID:        fmt.Sprintf("%016x", i),
			ParentSpanID:  parent,
			OperationName: "GET /api/resource",
			StartTime:     base.Add(time.Duration(i) * time.Millisecond),
			EndTime:       base.Add(time.Duration(i)*time.Millisecond + time.Millisecond),
			Duration:      int64(1000 + i%5000),
			ServiceName:   svc,
			Status:        status,
			AttributesJSON: CompressedText(fmt.Sprintf(
				`{"http.method":"GET","http.url":"/api/resource/%d","http.status_code":200,"net.peer.name":"%s","enduser.id":"user-%d"}`,
				i, svc, i%100)),
		}
	}
	if err := repo.BatchCreateSpans(spans); err != nil {
		b.Fatalf("seed: %v", err)
	}

	ctx := context.Background()
	start := base.Add(-time.Minute)
	end := base.Add(time.Hour)

	b.ResetTimer()
	for b.Loop() {
		if _, err := repo.GetServiceMapMetrics(ctx, start, end); err != nil {
			b.Fatalf("GetServiceMapMetrics: %v", err)
		}
	}
}
