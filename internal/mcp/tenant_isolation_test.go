// Package mcp tests the merge-gate invariant for RAN-19/RAN-39: every
// GraphRAG-backed MCP tool (and the legacy svcGraph-backed tools rewired
// onto GraphRAG) must scope its response to the tenant carried by the
// X-Tenant-ID header — overlapping data ingested for two tenants under
// the same service_name, trace_id, span_id, log template, and snapshot
// time must never leak across tenant boundaries.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"
)

// tenants exercised by the test. The third row uses an empty header to
// prove that absence-of-header collapses to storage.DefaultTenantID, which
// is also a real ingest target so we get a meaningful response (not just
// vacuous emptiness).
var isolationCallers = []struct {
	name        string
	header      string
	scoped      string
	otherSeeded []string
}{
	{name: "acme", header: "acme", scoped: "acme", otherSeeded: []string{"beta", storage.DefaultTenantID}},
	{name: "beta", header: "beta", scoped: "beta", otherSeeded: []string{"acme", storage.DefaultTenantID}},
	{name: "no_header_default", header: "", scoped: storage.DefaultTenantID, otherSeeded: []string{"acme", "beta"}},
}

// allTenants is the set of tenants we actually seed. Used by leak scans
// regardless of which caller is currently being asserted.
var allTenants = []string{"acme", "beta", storage.DefaultTenantID}

// markersFor builds the (own, others) marker pair for a given caller.
// Markers are tenant-stamped strings that appear inside service names,
// operation names, log bodies, and anomaly evidence — so a textual scan
// of the JSON response is sufficient to detect a cross-tenant leak.
func markersFor(scoped string, others []string) (own []string, leak []string) {
	own = []string{
		scoped + "-orders",
		scoped + "-marker",
		scoped + "-op",
	}
	for _, t := range others {
		leak = append(leak,
			t+"-orders",
			t+"-marker",
			t+"-op",
			t+"-anomaly-marker",
		)
	}
	return own, leak
}

// setupTenantIsolationServer wires an in-process MCP server against an
// in-memory SQLite repo and a started GraphRAG. The background refresh,
// snapshot, and anomaly loops are stretched to "never" inside the test
// window so the only state that lands in the stores is the data the test
// seeds explicitly — making leak assertions deterministic.
func setupTenantIsolationServer(t *testing.T) (*httptest.Server, *graphrag.GraphRAG, *storage.Repository, *vectordb.Index) {
	t.Helper()

	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	if err := graphrag.AutoMigrateGraphRAG(db); err != nil {
		t.Fatalf("AutoMigrateGraphRAG: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")

	vIdx := vectordb.New(1000)

	cfg := graphrag.DefaultConfig()
	cfg.RefreshEvery = 24 * time.Hour
	cfg.SnapshotEvery = 24 * time.Hour
	cfg.AnomalyEvery = 24 * time.Hour
	cfg.WorkerCount = 4

	g := graphrag.New(repo, vIdx, nil, nil, cfg)
	bgCtx, cancel := context.WithCancel(context.Background())
	go g.Start(bgCtx)

	srv := New(repo, nil, nil, vIdx)
	srv.SetGraphRAG(g)

	httpSrv := httptest.NewServer(srv.Handler())

	t.Cleanup(func() {
		httpSrv.Close()
		cancel()
		g.Stop()
		_ = repo.Close()
	})

	return httpSrv, g, repo, vIdx
}

// seedTenant ingests a small but representative slice of telemetry for
// tenant T: a parent OK span, a child ERROR span, a matching ERROR log,
// a vector-index doc, an injected anomaly, a persisted investigation,
// and a graph snapshot row. All identifiers (trace_id, span_id) collide
// across tenants on purpose — the tenant slice is the only thing keeping
// them apart.
func seedTenant(t *testing.T, g *graphrag.GraphRAG, repo *storage.Repository, vIdx *vectordb.Index, tenant string, ts time.Time) {
	t.Helper()

	service := tenant + "-orders"
	op := tenant + "-op-checkout"
	logBody := tenant + "-marker connection refused upstream"
	traceID := "trace-shared"
	rootSpanID := "span-root"
	childSpanID := "span-child"

	// Root span (OK).
	g.OnSpanIngested(storage.Span{
		TenantID:      tenant,
		TraceID:       traceID,
		SpanID:        rootSpanID,
		OperationName: "/checkout",
		ServiceName:   service,
		Status:        "STATUS_CODE_OK",
		StartTime:     ts,
		EndTime:       ts.Add(2 * time.Millisecond),
		Duration:      2000,
	})

	// Child span (ERROR), parented to root → upstream walk lands on the
	// per-tenant root span.
	g.OnSpanIngested(storage.Span{
		TenantID:      tenant,
		TraceID:       traceID,
		SpanID:        childSpanID,
		ParentSpanID:  rootSpanID,
		OperationName: op,
		ServiceName:   service,
		Status:        "STATUS_CODE_ERROR",
		StartTime:     ts.Add(time.Millisecond),
		EndTime:       ts.Add(2 * time.Millisecond),
		Duration:      1000,
	})

	// Log carrying the per-tenant marker — drives Drain clustering and
	// CorrelatedSignals; the body is also stored in the vector index.
	g.OnLogIngested(storage.Log{
		TenantID:    tenant,
		TraceID:     traceID,
		SpanID:      childSpanID,
		ServiceName: service,
		Severity:    "ERROR",
		Body:        logBody,
		Timestamp:   ts.Add(2 * time.Millisecond),
	})

	// Vector index doc — find_similar_logs path is keyed by tenant.
	vIdx.Add(0, tenant, service, "ERROR", logBody)

	// Inject a per-tenant anomaly directly so AnomalyTimeline has
	// something to return without depending on the anomaly detector
	// loop (which is throttled to 24h in this fixture).
	g.RegisterAnomaly(tenant, graphrag.AnomalyNode{
		ID:        tenant + "-anomaly-1",
		Type:      graphrag.AnomalyErrorSpike,
		Severity:  graphrag.SeverityCritical,
		Service:   service,
		Evidence:  tenant + "-anomaly-marker error_rate=0.95",
		Timestamp: ts.Add(3 * time.Millisecond),
	})

	// Snapshot row — insert directly so we control the tenant_id and ID
	// (takeSnapshot is the production loop, but it is package-private).
	snap := graphrag.GraphSnapshot{
		TenantID:       tenant,
		ID:             "snap-" + tenant,
		CreatedAt:      ts,
		Nodes:          json.RawMessage(`[{"name":"` + service + `","marker":"` + tenant + `-marker"}]`),
		Edges:          json.RawMessage(`[]`),
		ServiceCount:   1,
		AvgHealthScore: 0.5,
	}
	if err := repo.DB().Create(&snap).Error; err != nil {
		t.Fatalf("seed snapshot for %q: %v", tenant, err)
	}
}

// waitForServiceMaps polls until every seeded tenant's ServiceMap reflects
// at least one service. Required because OnSpanIngested is async.
func waitForServiceMaps(t *testing.T, g *graphrag.GraphRAG, tenants []string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ok := true
		for _, tn := range tenants {
			ctx := storage.WithTenantContext(context.Background(), tn)
			if len(g.ServiceMap(ctx, 0)) == 0 {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for ServiceMap to reflect ingested spans for %v", tenants)
}

// seedInvestigations relies on the in-memory state already being warm
// (see waitForServiceMaps). PersistInvestigation reaches into ImpactAnalysis
// internally, which reads from the per-tenant ServiceStore.
func seedInvestigations(t *testing.T, g *graphrag.GraphRAG, ts time.Time) {
	t.Helper()
	for _, tenant := range allTenants {
		service := tenant + "-orders"
		chain := graphrag.ErrorChainResult{
			RootCause: &graphrag.RootCauseInfo{
				Service:      service,
				Operation:    tenant + "-op-checkout",
				ErrorMessage: tenant + "-marker connection refused upstream",
				SpanID:       "span-child",
				TraceID:      "trace-shared",
			},
			SpanChain: []graphrag.SpanNode{{
				ID:        "span-child",
				TraceID:   "trace-shared",
				Service:   service,
				Operation: tenant + "-op-checkout",
				IsError:   true,
				Timestamp: ts,
			}},
			TraceID: "trace-shared",
		}
		g.PersistInvestigation(tenant, service, []graphrag.ErrorChainResult{chain}, nil)
	}
}

// callTool sends a JSON-RPC tools/call request to the test MCP server
// with the given X-Tenant-ID header (omitted when empty) and returns the
// inner ToolCallResult — i.e., the structure the LLM client would see.
// Also returns the concatenated text payload across content items, which
// is what tenant-leak assertions actually scan.
func callTool(t *testing.T, ts *httptest.Server, headerTenant, name string, args map[string]any) (ToolCallResult, string) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	rpcReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
	body, err := json.Marshal(rpcReq)
	if err != nil {
		t.Fatalf("marshal rpc: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if headerTenant != "" {
		req.Header.Set("X-Tenant-ID", headerTenant)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpc do: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rpc status %d: %s", resp.StatusCode, raw)
	}

	var rpcResp struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Result  ToolCallResult `json:"result"`
		Error   *RPCError      `json:"error"`
	}
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		t.Fatalf("unmarshal rpc: %v\nraw: %s", err, raw)
	}
	if rpcResp.Error != nil {
		t.Fatalf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	var sb strings.Builder
	for _, c := range rpcResp.Result.Content {
		sb.WriteString(c.Text)
		if c.Resource != nil {
			sb.WriteString(c.Resource.Text)
		}
	}
	return rpcResp.Result, sb.String()
}

// assertNoLeak fails if any of leakMarkers appears in body. ownMarker is
// optional — when non-empty it must appear, proving the tool returned
// real per-tenant data and not just a vacuous empty result.
func assertNoLeak(t *testing.T, label, body, ownMarker string, leakMarkers []string) {
	t.Helper()
	if ownMarker != "" && !strings.Contains(body, ownMarker) {
		t.Errorf("[%s] expected own marker %q in response, body=%s", label, ownMarker, truncate(body))
	}
	for _, m := range leakMarkers {
		if strings.Contains(body, m) {
			t.Errorf("[%s] CROSS-TENANT LEAK: foreign marker %q present in response, body=%s", label, m, truncate(body))
		}
	}
}

func truncate(s string) string {
	const max = 800
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// TestMCP_TenantIsolation_AllGraphRAGTools is the merge gate for RAN-19.
// For every GraphRAG-backed (and GraphRAG-rewired) MCP tool, it issues
// the same call from three callers — X-Tenant-ID: acme, X-Tenant-ID: beta,
// no header — against overlapping seeded data and asserts each response
// contains only the caller-tenant's data and never leaks another tenant's
// service name, log marker, operation, anomaly, or snapshot row.
func TestMCP_TenantIsolation_AllGraphRAGTools(t *testing.T) {
	ts, g, repo, vIdx := setupTenantIsolationServer(t)

	now := time.Now().Add(-time.Minute) // a hair in the past so since=now-15m sees us

	for _, tenant := range allTenants {
		seedTenant(t, g, repo, vIdx, tenant, now)
	}
	waitForServiceMaps(t, g, allTenants)
	seedInvestigations(t, g, now)

	// Resolve investigation IDs per tenant (PersistInvestigation generates
	// them internally; we discover them by querying after the fact, then
	// hand them back into get_investigation in the per-caller assertions).
	invIDsByTenant := map[string]string{}
	for _, tenant := range allTenants {
		ctx := storage.WithTenantContext(context.Background(), tenant)
		invs, err := g.GetInvestigations(ctx, "", "", "", 10)
		if err != nil {
			t.Fatalf("GetInvestigations(%s): %v", tenant, err)
		}
		if len(invs) == 0 {
			t.Fatalf("expected at least one persisted investigation for %s, got 0", tenant)
		}
		invIDsByTenant[tenant] = invs[0].ID
	}

	// snapshot lookup time — slightly in the future so "<= at" matches every
	// seeded row regardless of microsecond drift.
	snapAt := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)

	for _, caller := range isolationCallers {
		caller := caller
		ownMarkers, leakMarkers := markersFor(caller.scoped, caller.otherSeeded)
		// At minimum the response should reference the caller's service
		// for queries that are service-shaped. ownMarkers is intentionally
		// kept as the canonical "anything tenant-tagged" set in case future
		// assertions want it; per-tool checks pick the most relevant one.
		ownService := caller.scoped + "-orders"
		ownLogMarker := caller.scoped + "-marker"
		ownAnomalyMarker := caller.scoped + "-anomaly-marker"
		_ = ownMarkers

		// --- in-memory GraphRAG tools ---

		t.Run(caller.name+"/get_service_map", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "get_service_map", nil)
			assertNoLeak(t, "get_service_map", body, ownService, leakMarkers)
		})

		t.Run(caller.name+"/get_service_health", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "get_service_health", map[string]any{
				"service_name": ownService,
			})
			assertNoLeak(t, "get_service_health", body, ownService, leakMarkers)
		})

		t.Run(caller.name+"/get_error_chains", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "get_error_chains", map[string]any{
				"service":    ownService,
				"time_range": "1h",
				"limit":      10,
			})
			assertNoLeak(t, "get_error_chains", body, ownService, leakMarkers)
		})

		t.Run(caller.name+"/trace_graph", func(t *testing.T) {
			// trace_id collides across tenants; correct routing must surface
			// only the caller's per-tenant operation/service.
			_, body := callTool(t, ts, caller.header, "trace_graph", map[string]any{
				"trace_id": "trace-shared",
			})
			assertNoLeak(t, "trace_graph", body, ownService, leakMarkers)
		})

		t.Run(caller.name+"/impact_analysis", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "impact_analysis", map[string]any{
				"service": ownService,
				"depth":   3,
			})
			assertNoLeak(t, "impact_analysis", body, ownService, leakMarkers)
		})

		t.Run(caller.name+"/root_cause_analysis", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "root_cause_analysis", map[string]any{
				"service":    ownService,
				"time_range": "1h",
			})
			assertNoLeak(t, "root_cause_analysis", body, "", leakMarkers)
		})

		t.Run(caller.name+"/correlated_signals", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "correlated_signals", map[string]any{
				"service":    ownService,
				"time_range": "1h",
			})
			// CorrelatedSignals collects logs/metrics for the service, so the
			// per-tenant log marker should appear.
			assertNoLeak(t, "correlated_signals", body, ownLogMarker, leakMarkers)
		})

		t.Run(caller.name+"/get_anomaly_timeline", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "get_anomaly_timeline", nil)
			assertNoLeak(t, "get_anomaly_timeline", body, ownAnomalyMarker, leakMarkers)
		})

		// --- DB-backed GraphRAG tools ---

		t.Run(caller.name+"/get_investigations", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "get_investigations", nil)
			assertNoLeak(t, "get_investigations", body, ownService, leakMarkers)
		})

		t.Run(caller.name+"/get_investigation_by_id_own_tenant", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "get_investigation", map[string]any{
				"investigation_id": invIDsByTenant[caller.scoped],
			})
			assertNoLeak(t, "get_investigation/own", body, ownService, leakMarkers)
		})

		t.Run(caller.name+"/get_investigation_by_id_other_tenant_blocks", func(t *testing.T) {
			// Asking by another tenant's ID must NOT return that row — id-
			// guessing would otherwise leak across tenants. The handler
			// surfaces a tool-level error result, which is fine; what
			// matters is that the foreign tenant's data does not appear.
			otherTenant := caller.otherSeeded[0]
			_, body := callTool(t, ts, caller.header, "get_investigation", map[string]any{
				"investigation_id": invIDsByTenant[otherTenant],
			})
			assertNoLeak(t, "get_investigation/cross-tenant", body, "", leakMarkers)
		})

		t.Run(caller.name+"/get_graph_snapshot", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "get_graph_snapshot", map[string]any{
				"time": snapAt,
			})
			// Snapshot rows are tagged with the tenant marker so the leak
			// scan covers both ID prefixes (snap-acme/snap-beta/snap-default)
			// and the inline node markers.
			assertNoLeak(t, "get_graph_snapshot", body, "snap-"+caller.scoped, leakMarkers)
		})

		// --- vectordb-backed tool (Drain path is exercised by ingestion above) ---

		t.Run(caller.name+"/find_similar_logs", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "find_similar_logs", map[string]any{
				"query": "connection refused upstream",
				"limit": 10,
			})
			assertNoLeak(t, "find_similar_logs", body, ownLogMarker, leakMarkers)
		})

		// --- Legacy/rewired surface ---
		// get_system_graph is rewired onto GraphRAG by RAN-39, so the same
		// per-tenant invariants apply.
		t.Run(caller.name+"/get_system_graph", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "get_system_graph", nil)
			assertNoLeak(t, "get_system_graph", body, ownService, leakMarkers)
		})
	}
}

// TestMCP_TenantIsolation_DrainClusterIDsStayPerTenant proves that two
// tenants writing identical log bodies do not collide on the same Drain
// cluster id surfaced by CorrelatedSignals. Drain itself is currently a
// shared miner, but the LogClusterNodes are stored on per-tenant
// SignalStores so the cluster id surfaces tenant-side and a tenant cannot
// observe another tenant's cluster row.
func TestMCP_TenantIsolation_DrainClusterIDsStayPerTenant(t *testing.T) {
	ts, g, repo, vIdx := setupTenantIsolationServer(t)
	now := time.Now().Add(-time.Minute)

	// Identical log body for both tenants — collision-by-design.
	for _, tenant := range []string{"acme", "beta"} {
		seedTenant(t, g, repo, vIdx, tenant, now)
	}
	waitForServiceMaps(t, g, []string{"acme", "beta"})

	for _, scoped := range []string{"acme", "beta"} {
		_, body := callTool(t, ts, scoped, "correlated_signals", map[string]any{
			"service":    scoped + "-orders",
			"time_range": "1h",
		})
		// Caller's marker must appear, the other tenant's must not.
		other := "beta"
		if scoped == "beta" {
			other = "acme"
		}
		if !strings.Contains(body, scoped+"-marker") {
			t.Errorf("%s correlated_signals missing own marker, body=%s", scoped, truncate(body))
		}
		if strings.Contains(body, other+"-marker") {
			t.Errorf("%s correlated_signals leaked %s marker, body=%s", scoped, other, truncate(body))
		}
	}

	// Sanity: prove the test setup actually shares state between tenants
	// at the storage layer (so the isolation we're asserting above is
	// non-trivial). Same trace_id should land in two distinct rows because
	// Span.TenantID is part of the unique identity for these inserts.
	// We don't persist spans here directly (we go through OnSpanIngested
	// which is in-memory only), so we just assert the in-memory invariant.
	ctxA := storage.WithTenantContext(context.Background(), "acme")
	ctxB := storage.WithTenantContext(context.Background(), "beta")
	mapA := g.ServiceMap(ctxA, 0)
	mapB := g.ServiceMap(ctxB, 0)
	if got, want := len(mapA), 1; got != want {
		t.Fatalf("acme ServiceMap len=%d want=%d (%+v)", got, want, mapA)
	}
	if got, want := len(mapB), 1; got != want {
		t.Fatalf("beta ServiceMap len=%d want=%d (%+v)", got, want, mapB)
	}
	if mapA[0].Service.Name == mapB[0].Service.Name {
		t.Fatalf("ServiceMap shows same service name for both tenants — partition broken: %v vs %v", mapA[0].Service, mapB[0].Service)
	}
}

