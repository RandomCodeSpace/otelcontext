package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/latency"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// honestyServer wires an MCP server against an in-memory repo and a GraphRAG
// whose background loops never fire inside the test window.
func honestyServer(t *testing.T) (*Server, *graphrag.GraphRAG, *storage.Repository) {
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

	cfg := graphrag.DefaultConfig()
	cfg.RefreshEvery = 24 * time.Hour
	cfg.SnapshotEvery = 24 * time.Hour
	cfg.AnomalyEvery = 24 * time.Hour
	g := graphrag.New(repo, nil, nil, cfg)
	bgCtx, cancel := context.WithCancel(context.Background())
	go g.Start(bgCtx)
	t.Cleanup(func() {
		cancel()
		g.Stop()
	})

	srv := New(storage.DefaultTenantID, repo, nil, nil)
	srv.SetGraphRAG(g)
	return srv, g, repo
}

// decodeTraceGraph pulls the TraceGraphResult out of a tool result.
func decodeTraceGraph(t *testing.T, res ToolCallResult) graphrag.TraceGraphResult {
	t.Helper()
	if res.IsError {
		t.Fatalf("trace_graph returned an error result: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatalf("trace_graph returned no content")
	}
	payload := res.Content[0].Text
	if res.Content[0].Resource != nil {
		payload = res.Content[0].Resource.Text
	}
	var out graphrag.TraceGraphResult
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("unmarshal trace_graph payload %q: %v", payload, err)
	}
	return out
}

// TestTraceGraphSaysNotRetainedRatherThanFabricating is the honesty contract:
// a trace nobody retained gets a definitive "not retained or not found", as a
// SUCCESSFUL response, never an invented tree.
func TestTraceGraphSaysNotRetainedRatherThanFabricating(t *testing.T) {
	srv, _, _ := honestyServer(t)

	res := srv.toolHandler(context.Background(), "trace_graph", map[string]any{"trace_id": "ghost-trace"})
	out := decodeTraceGraph(t, res)

	if out.Coverage.Source != graphrag.CoverageNone {
		t.Fatalf("coverage source = %q, want %q", out.Coverage.Source, graphrag.CoverageNone)
	}
	if out.Coverage.Complete {
		t.Fatalf("an absent trace was reported complete: %+v", out.Coverage)
	}
	if len(out.Spans) != 0 {
		t.Fatalf("an absent trace returned %d spans", len(out.Spans))
	}
	if out.Coverage.Note == "" {
		t.Fatalf("no explanation for the absent trace")
	}
}

// TestTraceGraphReportsCompleteRetainedExemplar proves a fully retained trace
// is reported as complete.
func TestTraceGraphReportsCompleteRetainedExemplar(t *testing.T) {
	srv, g, _ := honestyServer(t)
	now := time.Now()
	for _, sp := range []storage.Span{
		{TenantID: storage.DefaultTenantID, TraceID: "tr-1", SpanID: "root", ServiceName: "gateway", OperationName: "/in", StartTime: now, Duration: 1000},
		{TenantID: storage.DefaultTenantID, TraceID: "tr-1", SpanID: "leaf", ParentSpanID: "root", ServiceName: "checkout", OperationName: "/pay", StartTime: now, Duration: 1000},
	} {
		g.OnSpanIngested(sp)
	}
	waitForSpans(t, g, "tr-1", 2)

	out := decodeTraceGraph(t, srv.toolHandler(context.Background(), "trace_graph", map[string]any{"trace_id": "tr-1"}))
	if !out.Coverage.Complete || out.Coverage.Source != graphrag.CoverageRetained {
		t.Fatalf("complete retained trace reported as %+v", out.Coverage)
	}
	if out.Coverage.RetainedSpans != 2 {
		t.Fatalf("retained spans = %d, want 2", out.Coverage.RetainedSpans)
	}
}

// TestTraceGraphReportsPartialWhenParentMissing proves a trace whose parent
// span was never retained is reported as partial, not presented as a tree.
func TestTraceGraphReportsPartialWhenParentMissing(t *testing.T) {
	srv, g, _ := honestyServer(t)
	now := time.Now()
	g.OnSpanIngested(storage.Span{
		TenantID: storage.DefaultTenantID, TraceID: "tr-2", SpanID: "orphan",
		ParentSpanID: "never-retained", ServiceName: "checkout", OperationName: "/pay",
		StartTime: now, Duration: 1000,
	})
	waitForSpans(t, g, "tr-2", 1)

	out := decodeTraceGraph(t, srv.toolHandler(context.Background(), "trace_graph", map[string]any{"trace_id": "tr-2"}))
	if out.Coverage.Complete {
		t.Fatalf("a trace with a dangling parent was reported complete: %+v", out.Coverage)
	}
	if out.Coverage.Source != graphrag.CoveragePartial || !out.Coverage.Truncated {
		t.Fatalf("coverage = %+v, want partial + truncated", out.Coverage)
	}
}

// TestToolDescriptionsStateTheirCoverageLimits proves the descriptions the
// 7-tool surface advertises actually carry the contract, so a client is told
// the limits before it calls anything.
func TestToolDescriptionsStateTheirCoverageLimits(t *testing.T) {
	byName := map[string]string{}
	for _, tool := range toolDefs {
		byName[tool.Name] = tool.Description
	}
	if len(byName) != 7 {
		t.Fatalf("tool surface has %d tools, want 7", len(byName))
	}
	for name, want := range map[string]string{
		"trace_graph":          "not_retained_or_not_found",
		"root_cause_analysis":  "partial_exemplar",
		"get_service_map":      "aggregate retention",
		"impact_analysis":      "aggregate retention",
		"get_anomaly_timeline": "aggregate retention",
	} {
		if !strings.Contains(byName[name], want) {
			t.Errorf("%s description does not state %q: %s", name, want, byName[name])
		}
	}
}

func TestTopologyToolsPreserveLatencyProvenance(t *testing.T) {
	srv, g, _ := honestyServer(t)
	g.OnSpanIngested(storage.Span{
		TenantID: storage.DefaultTenantID, TraceID: "latency-trace", SpanID: "latency-span",
		ServiceName: "checkout", OperationName: "/pay", StartTime: time.Now(), Duration: 20_000,
	})
	ctx := storage.WithTenantContext(context.Background(), storage.DefaultTenantID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(g.ServiceMap(ctx, 0)) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	mapResult := srv.toolHandler(context.Background(), "get_service_map", nil)
	if mapResult.IsError || len(mapResult.Content) == 0 {
		t.Fatalf("get_service_map = %+v", mapResult)
	}
	var entries []graphrag.ServiceMapEntry
	if err := json.Unmarshal([]byte(mapResult.Content[0].Text), &entries); err != nil {
		t.Fatal(err)
	}
	// Legacy GraphRAG p99 is sketch-derived since #291: approximate, within
	// the sketch bound of the 20ms the fixture emits, no longer avg*2.5.
	if len(entries) != 1 || entries[0].Service.LatencyProvenance == nil || entries[0].Service.LatencyProvenance.P99.Status != latency.StatusApproximate ||
		entries[0].Service.P99Latency < 19 || entries[0].Service.P99Latency > 21 {
		t.Fatalf("service map latency = %+v", entries)
	}
	if len(entries[0].Operations) != 1 || entries[0].Operations[0].LatencyProvenance.P99.Status != latency.StatusUnavailable {
		t.Fatalf("operation latency = %+v", entries[0].Operations)
	}

	health := srv.toolHandler(context.Background(), "get_service_health", map[string]any{"service_name": "checkout"})
	if health.IsError || len(health.Content) == 0 || !strings.Contains(health.Content[0].Text, `"latency_provenance"`) {
		t.Fatalf("get_service_health = %+v", health)
	}
}

// waitForSpans blocks until GraphRAG's async workers have folded n spans of
// traceID into the trace store.
func waitForSpans(t *testing.T, g *graphrag.GraphRAG, traceID string, n int) {
	t.Helper()
	ctx := storage.WithTenantContext(context.Background(), storage.DefaultTenantID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(g.DependencyChain(ctx, traceID)) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("trace %s never reached %d spans in the trace store", traceID, n)
}
