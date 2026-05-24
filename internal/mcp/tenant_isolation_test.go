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
func setupTenantIsolationServer(t *testing.T) (*httptest.Server, *graphrag.GraphRAG, *storage.Repository) {
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

	cfg := graphrag.DefaultConfig()
	cfg.RefreshEvery = 24 * time.Hour
	cfg.SnapshotEvery = 24 * time.Hour
	cfg.AnomalyEvery = 24 * time.Hour
	cfg.WorkerCount = 4

	g := graphrag.New(repo, nil, nil, cfg)
	bgCtx, cancel := context.WithCancel(context.Background())
	go g.Start(bgCtx)

	srv := New("", repo, nil, nil)
	srv.SetGraphRAG(g)

	httpSrv := httptest.NewServer(srv.Handler())

	t.Cleanup(func() {
		httpSrv.Close()
		cancel()
		g.Stop()
		_ = repo.Close()
	})

	return httpSrv, g, repo
}

// seedTenant ingests a small but representative slice of telemetry for
// tenant T: a parent OK span, a child ERROR span, a matching ERROR log,
// and an injected anomaly. All identifiers (trace_id, span_id) collide
// across tenants on purpose — the tenant slice is the only thing keeping
// them apart.
//
// repo is accepted but currently unused; future tests may seed DB rows
// directly. It is preserved so callers can switch back to DB-shaped
// seeding without a signature change.
func seedTenant(t *testing.T, g *graphrag.GraphRAG, _ *storage.Repository, _ any, tenant string, ts time.Time) {
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

	// Log carrying the per-tenant marker — drives Drain clustering and the
	// LogClusterNode side-effect that CorrelatedSignals would consume.
	g.OnLogIngested(storage.Log{
		TenantID:    tenant,
		TraceID:     traceID,
		SpanID:      childSpanID,
		ServiceName: service,
		Severity:    "ERROR",
		Body:        logBody,
		Timestamp:   ts.Add(2 * time.Millisecond),
	})

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

// TestMCP_TenantIsolation_AllGraphRAGTools is the merge gate for the 7-tool
// triage MCP surface (post-2026-05-24 reduction). For every kept tool, it
// issues the same call from three callers — X-Tenant-ID: acme,
// X-Tenant-ID: beta, no header — against overlapping seeded data and
// asserts each response contains only the caller-tenant's data and never
// leaks another tenant's service name, log marker, operation, or anomaly.
func TestMCP_TenantIsolation_AllGraphRAGTools(t *testing.T) {
	ts, g, repo := setupTenantIsolationServer(t)

	now := time.Now().Add(-time.Minute) // a hair in the past so since=now-15m sees us

	for _, tenant := range allTenants {
		seedTenant(t, g, repo, nil, tenant, now)
	}
	waitForServiceMaps(t, g, allTenants)

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
		_ = ownLogMarker

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
			// RankedCause carries Service + Operation, so the caller's
			// own service name MUST appear; an empty result here would
			// silently regress the tool to a vacuous "[]" response that
			// trivially "passes" leak checks (review feedback fix).
			assertNoLeak(t, "root_cause_analysis", body, ownService, leakMarkers)
		})

		t.Run(caller.name+"/get_anomaly_timeline", func(t *testing.T) {
			_, body := callTool(t, ts, caller.header, "get_anomaly_timeline", nil)
			assertNoLeak(t, "get_anomaly_timeline", body, ownAnomalyMarker, leakMarkers)
		})

	}
}

// TestMCP_TenantIsolation_DrainClusterIDsStayPerTenant proves that two
// tenants writing identical log bodies under an *identical* service name
// do not share a single shared LogClusterNode. Drain itself is currently
// a shared miner — without per-tenant SignalStore partitioning the same
// (template, service) pair would collapse to one cluster row visible to
// both tenants. The test inspects the actual LogClusterNodes returned by
// CorrelatedSignals (not just the response text) and asserts each tenant
// only ever sees rows tagged with its own marker.
func TestMCP_TenantIsolation_DrainClusterIDsStayPerTenant(t *testing.T) {
	ts, g, _ := setupTenantIsolationServer(t)
	now := time.Now().Add(-time.Minute)

	// Identical service AND identical log template across tenants — Drain
	// is a shared miner so the (service, templateID) cluster key would
	// collide if SignalStore weren't tenant-partitioned. The body marker
	// is the only per-tenant differentiator.
	const sharedService = "shared-orders"
	const sharedTrace = "trace-shared"
	const sharedSpan = "span-shared"

	for _, tenant := range []string{"acme", "beta"} {
		g.OnSpanIngested(storage.Span{
			TenantID:      tenant,
			TraceID:       sharedTrace,
			SpanID:        sharedSpan,
			ServiceName:   sharedService,
			OperationName: "/checkout",
			Status:        "STATUS_CODE_ERROR",
			StartTime:     now,
			EndTime:       now.Add(time.Millisecond),
			Duration:      1000,
		})
		g.OnLogIngested(storage.Log{
			TenantID:    tenant,
			TraceID:     sharedTrace,
			SpanID:      sharedSpan,
			ServiceName: sharedService,
			Severity:    "ERROR",
			Body:        tenant + "-marker upstream connection refused",
			Timestamp:   now.Add(time.Millisecond),
		})
	}

	ctxA := storage.WithTenantContext(context.Background(), "acme")
	ctxB := storage.WithTenantContext(context.Background(), "beta")

	// Wait for both tenants' SignalStores to surface the cluster row.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a := g.CorrelatedSignals(ctxA, sharedService, now.Add(-time.Hour))
		b := g.CorrelatedSignals(ctxB, sharedService, now.Add(-time.Hour))
		if a != nil && b != nil && len(a.ErrorLogs) >= 1 && len(b.ErrorLogs) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	sigA := g.CorrelatedSignals(ctxA, sharedService, now.Add(-time.Hour))
	sigB := g.CorrelatedSignals(ctxB, sharedService, now.Add(-time.Hour))
	if sigA == nil || len(sigA.ErrorLogs) == 0 {
		t.Fatalf("acme CorrelatedSignals returned no ErrorLogs — Drain/SignalStore did not see the seeded log")
	}
	if sigB == nil || len(sigB.ErrorLogs) == 0 {
		t.Fatalf("beta CorrelatedSignals returned no ErrorLogs — Drain/SignalStore did not see the seeded log")
	}

	// Per-tenant cluster row content must carry only that tenant's marker.
	// We probe both Template and SampleLog because Drain stores the
	// templated form on Template and the original body on SampleLog, and
	// both should be uncontaminated.
	checkClusters := func(name string, clusters []graphrag.LogClusterNode, ownMarker, foreignMarker string) []string {
		t.Helper()
		var ids []string
		for _, lc := range clusters {
			ids = append(ids, lc.ID)
			joined := lc.Template + "\n" + lc.SampleLog
			if !strings.Contains(joined, ownMarker) {
				t.Errorf("[%s] cluster %q missing own marker %q (template=%q sample=%q)", name, lc.ID, ownMarker, lc.Template, lc.SampleLog)
			}
			if strings.Contains(joined, foreignMarker) {
				t.Errorf("[%s] cluster %q LEAKED foreign marker %q (template=%q sample=%q)", name, lc.ID, foreignMarker, lc.Template, lc.SampleLog)
			}
		}
		return ids
	}
	idsA := checkClusters("acme", sigA.ErrorLogs, "acme-marker", "beta-marker")
	idsB := checkClusters("beta", sigB.ErrorLogs, "beta-marker", "acme-marker")

	// The cluster IDs themselves can be identical across tenants (Drain ID
	// is service-scoped, not tenant-scoped) — that is precisely WHY the
	// SignalStore partition matters: without it, the same key would point
	// at one shared row. Surface this fact in the test record so a future
	// refactor that makes IDs tenant-stamped doesn't accidentally weaken
	// the assertion above.
	t.Logf("drain cluster IDs: acme=%v beta=%v", idsA, idsB)

	// Note: the legacy end-to-end probe used the `correlated_signals` MCP
	// tool to assert the same isolation across the HTTP transport. That
	// tool was cut on 2026-05-24 alongside 13 others; the in-process
	// CorrelatedSignals invariant above is still the truth-test for Drain
	// + SignalStore tenant partitioning. The 7-tool MCP transport invariant
	// for the kept tools is covered by TestMCP_TenantIsolation_AllGraphRAGTools.
	_ = ts
}
