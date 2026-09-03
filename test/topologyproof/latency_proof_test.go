package topologyproof

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/api"
	"github.com/RandomCodeSpace/otelcontext/internal/api/views"
	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/latency"
	"github.com/RandomCodeSpace/otelcontext/internal/mcp"
	"github.com/RandomCodeSpace/otelcontext/internal/realtime"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

const (
	latencyProofModeEnv = "OTELCONTEXT_LATENCY_PROOF_MODE"
	latencyProofDirEnv  = "OTELCONTEXT_LATENCY_PROOF_DIR"
	latencyService      = "latency-sentinel"
	latencyLowMicros    = 10_000
	latencyTailMicros   = 1_000_000
	latencyLowCount     = 989
	latencyTailCount    = 11
)

type latencySentinel struct {
	LowCount     int     `json:"low_count"`
	LowMS        float64 `json:"low_ms"`
	TailCount    int     `json:"tail_count"`
	TailMS       float64 `json:"tail_ms"`
	Population   int     `json:"population"`
	AverageMS    float64 `json:"average_ms"`
	NearestP99MS float64 `json:"nearest_rank_p99_ms"`
	MultiplierMS float64 `json:"average_multiplier_ms"`
	Multiplier   float64 `json:"average_multiplier"`
}

type latencySurface struct {
	Value      float64             `json:"value"`
	Unit       string              `json:"unit"`
	Provenance *latency.Provenance `json:"latency_provenance,omitempty"`
}

type latencyContractProof struct {
	SchemaVersion string                    `json:"schema_version"`
	Mode          string                    `json:"mode"`
	Sentinel      latencySentinel           `json:"sentinel"`
	Dashboard     latencySurface            `json:"rest_dashboard"`
	SystemGraph   latencySurface            `json:"rest_system_graph"`
	ServiceMap    latencySurface            `json:"rest_service_map"`
	WebSocket     latencySurface            `json:"websocket_dashboard"`
	GraphRAG      latencySurface            `json:"graphrag_service"`
	MCPMap        latencySurface            `json:"mcp_get_service_map"`
	MCPHealth     latencySurface            `json:"mcp_get_service_health"`
	Operation     latencySurface            `json:"graphrag_operation"`
	UILabels      []string                  `json:"ui_labels"`
	Assertions    map[string]proofAssertion `json:"assertions"`
}

func newLatencyContractProof(mode string) *latencyContractProof {
	return &latencyContractProof{
		SchemaVersion: "otelcontext.latency-contract.v1",
		Mode:          mode,
		Sentinel: latencySentinel{
			LowCount:     latencyLowCount,
			LowMS:        latencyLowMicros / 1000,
			TailCount:    latencyTailCount,
			TailMS:       latencyTailMicros / 1000,
			Population:   latencyLowCount + latencyTailCount,
			AverageMS:    20.89,
			NearestP99MS: 1000,
			MultiplierMS: 52.225,
			Multiplier:   2.5,
		},
		UILabels: []string{
			"P99",
			"Approx. p99",
			"Estimated tail",
			"Sample p99",
			"P99 unavailable",
			"Reported p99",
			"Average",
		},
		Assertions: make(map[string]proofAssertion),
	}
}

func (p *latencyContractProof) check(t *testing.T, name string, passed bool, detail string) {
	t.Helper()
	p.Assertions[name] = proofAssertion{Passed: passed, Detail: detail}
	if !passed {
		t.Errorf("%s: %s", name, detail)
	}
}

func (p *latencyContractProof) write(t *testing.T) {
	t.Helper()
	dir := os.Getenv(latencyProofDirEnv)
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("create latency proof directory: %v", err)
		return
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Errorf("marshal latency proof: %v", err)
		return
	}
	path := filepath.Join(dir, p.Mode+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Errorf("write latency proof: %v", err)
		return
	}
	t.Logf("latency proof: %s", path)
}

func TestLatencyModeProof(t *testing.T) {
	mode := os.Getenv(latencyProofModeEnv)
	if mode == "" {
		t.Skipf("%s is set by the dedicated latency-proof CI job", latencyProofModeEnv)
	}
	if mode != aggregate.ModeLegacy && mode != aggregate.ModeShadow && mode != aggregate.ModeAggregate {
		t.Fatalf("unsupported proof mode %q", mode)
	}

	proof := newLatencyContractProof(mode)
	defer proof.write(t)
	now := time.Now().UTC().Truncate(time.Second)
	repo := proofRepository(t)
	seedLatencyRepository(t, repo, now)

	var engine *aggregate.Engine
	var provider topology.Provider
	var graphRAG *graphrag.GraphRAG
	if mode == aggregate.ModeAggregate {
		engine = latencyAggregateEngine(t, now)
		aggregateProvider, err := topology.NewAggregateProvider(engine)
		if err != nil {
			t.Fatalf("new aggregate topology provider: %v", err)
		}
		provider = aggregateProvider
		graphRAG = newLatencyGraphRAG(t, mode, now, aggregateProvider)
	} else {
		graphRAG = newLatencyGraphRAG(t, mode, now, nil)
		legacyProvider, err := topology.NewLegacyProvider(repo, nil, graphRAG)
		if err != nil {
			t.Fatalf("new legacy topology provider: %v", err)
		}
		provider = legacyProvider
	}

	metrics := telemetry.New()
	rawHub := realtime.NewHub(nil)
	rawHub.SetTopologyProvider(provider, proofFloor)
	if mode == aggregate.ModeAggregate {
		rawHub.SetAggregateMode(true)
	}
	go rawHub.Run()

	eventHub := realtime.NewEventHub(repo, nil, nil)
	if mode == aggregate.ModeAggregate {
		publisher := realtime.NewEnginePublisher(realtime.EnginePublisherConfig{
			Engine: engine, Topology: provider, Window: 15 * time.Minute,
		})
		if publisher == nil {
			t.Fatal("aggregate latency publisher was not constructed")
		}
		eventHub.SetAggregatePublisher(publisher, proofFloor)
	}
	eventCtx, cancelEvents := context.WithCancel(context.Background())
	go eventHub.Start(eventCtx, 50*time.Millisecond, 20*time.Millisecond)

	apiServer := api.NewServer(repo, rawHub, eventHub, metrics)
	apiServer.SetGraphRAG(graphRAG)
	apiServer.SetTopologyProvider(provider)
	if mode == aggregate.ModeAggregate {
		apiServer.SetAggregateEngine(engine)
	}
	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	httpServer := httptest.NewServer(mux)

	mcpServer := mcp.New(storage.DefaultTenantID, repo, metrics, provider)
	mcpServer.SetGraphRAG(graphRAG)
	mcpServer.SetAggregateMode(mode == aggregate.ModeAggregate)
	mcpHTTP := httptest.NewServer(mcpServer.Handler())

	t.Cleanup(func() {
		mcpHTTP.Close()
		httpServer.Close()
		cancelEvents()
		eventHub.Stop()
		rawHub.Stop()
		graphRAG.Stop()
	})

	var dashboard struct {
		P99LatencyMS      float64             `json:"p99_latency_ms"`
		LatencyProvenance *latency.Provenance `json:"latency_provenance"`
	}
	getProofJSON(t, httpServer.URL+"/api/metrics/dashboard", &dashboard)
	proof.Dashboard = latencySurface{Value: dashboard.P99LatencyMS, Unit: "milliseconds", Provenance: dashboard.LatencyProvenance}

	var systemGraph api.SystemGraphResponse
	getProofJSON(t, httpServer.URL+"/api/system/graph", &systemGraph)
	systemNode := findGraphNode(t, systemGraph.Nodes, latencyService)
	proof.SystemGraph = latencySurface{Value: systemNode.Metrics.P99LatencyMs, Unit: "milliseconds", Provenance: systemNode.Metrics.LatencyProvenance}

	var serviceMap views.ServiceMapMetrics
	getProofJSON(t, httpServer.URL+"/api/metrics/service-map", &serviceMap)
	proof.ServiceMap = findServiceMapNode(t, serviceMap.Nodes, latencyService)

	graphEntry := findServiceEntry(t, graphRAG.ServiceMap(context.Background(), 0), latencyService)
	proof.GraphRAG = latencySurface{Value: graphEntry.Service.P99Latency, Unit: "milliseconds", Provenance: graphEntry.Service.LatencyProvenance}
	if len(graphEntry.Operations) == 0 {
		t.Fatal("latency sentinel operation is absent from GraphRAG")
	}
	operation := graphEntry.Operations[0]
	proof.Operation = latencySurface{Value: operation.P99Latency, Unit: "milliseconds", Provenance: operation.LatencyProvenance}

	mapRaw := callTool(t, mcpHTTP.URL, "get_service_map", nil)
	var mapEntries []graphrag.ServiceMapEntry
	decodeFirstJSON(t, mapRaw, &mapEntries)
	mapEntry := findServiceEntry(t, mapEntries, latencyService)
	proof.MCPMap = latencySurface{Value: mapEntry.Service.P99Latency, Unit: "milliseconds", Provenance: mapEntry.Service.LatencyProvenance}

	healthRaw := callTool(t, mcpHTTP.URL, "get_service_health", map[string]any{"service_name": latencyService})
	var healthEntry graphrag.ServiceMapEntry
	decodeFirstJSON(t, healthRaw, &healthEntry)
	if healthEntry.Service == nil {
		t.Fatalf("MCP service health omitted %q: %s", latencyService, compact(healthRaw))
	}
	proof.MCPHealth = latencySurface{Value: healthEntry.Service.P99Latency, Unit: "milliseconds", Provenance: healthEntry.Service.LatencyProvenance}

	eventConn := dialWS(t, httpServer.URL+"/ws/events")
	eventSnapshot := readEventSnapshot(t, eventConn, 3*time.Second)
	if eventSnapshot.Dashboard == nil {
		t.Fatal("WebSocket snapshot omitted dashboard")
	}
	proof.WebSocket = latencySurface{
		Value:      float64(eventSnapshot.Dashboard.P99Latency),
		Unit:       "microseconds",
		Provenance: eventSnapshot.Dashboard.LatencyProvenance,
	}

	if mode == aggregate.ModeAggregate {
		proof.check(t, "rest_dashboard_approximate", validApproximate(proof.Dashboard, 1000), describeLatency(proof.Dashboard))
		proof.check(t, "rest_system_graph_approximate", validApproximate(proof.SystemGraph, 1000), describeLatency(proof.SystemGraph))
		proof.check(t, "rest_service_map_approximate", validApproximate(proof.ServiceMap, 1000), describeLatency(proof.ServiceMap))
		proof.check(t, "websocket_approximate", validApproximate(proof.WebSocket, latencyTailMicros), describeLatency(proof.WebSocket))
		proof.check(t, "graphrag_approximate", validApproximate(proof.GraphRAG, 1000), describeLatency(proof.GraphRAG))
		proof.check(t, "mcp_map_approximate", validApproximate(proof.MCPMap, 1000), describeLatency(proof.MCPMap))
		proof.check(t, "mcp_health_approximate", validApproximate(proof.MCPHealth, 1000), describeLatency(proof.MCPHealth))
		proof.check(t, "aggregate_source_owned", systemGraph.Source == "aggregate", fmt.Sprintf("graph=%q", systemGraph.Source))
	} else {
		// #291: the legacy database path (service map) ranks the population
		// exactly and the GraphRAG service store (system graph, GraphRAG, MCP)
		// answers from its per-service sketch, so no legacy surface may report
		// the 52.225ms average multiplier any more.
		proof.check(t, "rest_dashboard_measured", validMeasured(proof.Dashboard, 1000, latency.MethodOrderedRank), describeLatency(proof.Dashboard))
		proof.check(t, "rest_system_graph_approximate", validApproximate(proof.SystemGraph, 1000), describeLatency(proof.SystemGraph))
		proof.check(t, "rest_service_map_measured", validMeasured(proof.ServiceMap, 1000, latency.MethodOrderedRank), describeLatency(proof.ServiceMap))
		proof.check(t, "websocket_measured", validMeasured(proof.WebSocket, latencyTailMicros, latency.MethodOrderedRank), describeLatency(proof.WebSocket))
		proof.check(t, "graphrag_approximate", validApproximate(proof.GraphRAG, 1000), describeLatency(proof.GraphRAG))
		proof.check(t, "mcp_map_approximate", validApproximate(proof.MCPMap, 1000), describeLatency(proof.MCPMap))
		proof.check(t, "mcp_health_approximate", validApproximate(proof.MCPHealth, 1000), describeLatency(proof.MCPHealth))
		proof.check(t, "no_legacy_surface_estimated", !anyEstimated(proof), "average × 2.5 is no longer reported by any legacy surface")
		proof.check(t, "legacy_source_owned", systemGraph.Source == "", fmt.Sprintf("graph=%q", systemGraph.Source))
	}

	proof.check(t, "websocket_unit_preserved", proof.WebSocket.Unit == "microseconds" && math.Abs(proof.WebSocket.Value-proof.Dashboard.Value*1000) <= 1, fmt.Sprintf("REST=%vms websocket=%vµs", proof.Dashboard.Value, proof.WebSocket.Value))
	proof.check(t, "graphrag_mcp_consistent", sameLatency(proof.GraphRAG, proof.MCPMap) && sameLatency(proof.GraphRAG, proof.MCPHealth), fmt.Sprintf("graph=%s map=%s health=%s", describeLatency(proof.GraphRAG), describeLatency(proof.MCPMap), describeLatency(proof.MCPHealth)))
	proof.check(t, "operation_percentiles_unavailable", unavailableOperation(operation), fmt.Sprintf("value=%v provenance=%+v", operation.P99Latency, operation.LatencyProvenance))
	proof.check(t, "sample_count_preserved", allSampleCounts(proof, latencyLowCount+latencyTailCount), "every available surface reports the 1,000-observation source population")
	proof.check(t, "ui_label_contract", len(proof.UILabels) == 7 && proof.UILabels[0] == "P99" && proof.UILabels[6] == "Average", strings.Join(proof.UILabels, ", "))
}

func seedLatencyRepository(t *testing.T, repo *storage.Repository, now time.Time) {
	t.Helper()
	traces := make([]storage.Trace, 0, latencyLowCount+latencyTailCount)
	spans := make([]storage.Span, 0, latencyLowCount+latencyTailCount)
	for i := 0; i < latencyLowCount+latencyTailCount; i++ {
		duration := int64(latencyLowMicros)
		if i >= latencyLowCount {
			duration = latencyTailMicros
		}
		traces = append(traces, storage.Trace{
			TenantID: storage.DefaultTenantID, TraceID: fmt.Sprintf("latency-%04d", i),
			ServiceName: latencyService, Duration: duration, Status: "STATUS_CODE_UNSET",
			Timestamp: now.Add(-time.Minute),
		})
		// The same population as spans, so the service-map database path
		// (#291) ranks exactly what the dashboard ranks.
		spans = append(spans, storage.Span{
			TenantID: storage.DefaultTenantID, TraceID: fmt.Sprintf("latency-%04d", i),
			SpanID: fmt.Sprintf("%016x", i+1), ServiceName: latencyService, OperationName: "GET /latency",
			StartTime: now.Add(-time.Minute), EndTime: now.Add(-time.Minute + time.Duration(duration)*time.Microsecond),
			Duration: duration, Status: "STATUS_CODE_UNSET",
		})
	}
	if err := repo.BatchCreateTraces(traces); err != nil {
		t.Fatalf("seed latency traces: %v", err)
	}
	if err := repo.BatchCreateSpans(spans); err != nil {
		t.Fatalf("seed latency spans: %v", err)
	}
}

func latencyAggregateEngine(t *testing.T, now time.Time) *aggregate.Engine {
	t.Helper()
	engine, err := aggregate.NewEngine(aggregate.EngineConfig{
		Mode: aggregate.ModeAggregate, Epoch: 99, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new aggregate engine: %v", err)
	}
	reducer := engine.NewReducer(now)
	for i := 0; i < latencyLowCount+latencyTailCount; i++ {
		duration := float64(latencyLowMicros)
		if i >= latencyLowCount {
			duration = latencyTailMicros
		}
		reducer.ReduceSpan(aggregate.SpanInput{
			Tenant: storage.DefaultTenantID, Service: latencyService, SpanName: "GET /latency",
			SpanKind: int32(aggregate.SpanKindServer), Root: true,
			Timestamp: now.Add(-time.Minute), DurationMicros: duration,
		})
	}
	if revision := engine.ApplyReducer(reducer); revision == 0 {
		t.Fatal("aggregate latency reducer produced no revision")
	}
	return engine
}

func newLatencyGraphRAG(t *testing.T, mode string, now time.Time, source graphrag.AggregateSource) *graphrag.GraphRAG {
	t.Helper()
	cfg := graphrag.DefaultConfig()
	cfg.Mode = mode
	cfg.WorkerCount = 1
	cfg.ChannelSize = 2048
	cfg.RefreshEvery = time.Hour
	cfg.SnapshotEvery = time.Hour
	cfg.AnomalyEvery = time.Hour
	graphRAG := graphrag.New(nil, nil, nil, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if mode == aggregate.ModeAggregate {
		graphRAG.SetAggregateSource(source)
	}
	graphRAG.Start(ctx)
	if mode == aggregate.ModeAggregate {
		entry := findServiceEntry(t, graphRAG.ServiceMap(context.Background(), 0), latencyService)
		if entry.Service.CallCount != latencyLowCount+latencyTailCount {
			t.Fatalf("aggregate GraphRAG call count = %d", entry.Service.CallCount)
		}
		shutdownLatencyGraphRAG(t, graphRAG)
		return graphRAG
	}

	for i := 0; i < latencyLowCount+latencyTailCount; i++ {
		duration := int64(latencyLowMicros)
		if i >= latencyLowCount {
			duration = latencyTailMicros
		}
		graphRAG.OnSpanIngested(storage.Span{
			TenantID: storage.DefaultTenantID, TraceID: fmt.Sprintf("latency-%04d", i),
			SpanID: fmt.Sprintf("%016x", i+1), ServiceName: latencyService,
			OperationName: "GET /latency", StartTime: now.Add(-time.Minute),
			Duration: duration, Status: "STATUS_CODE_UNSET",
		})
	}
	shutdownLatencyGraphRAG(t, graphRAG)
	entry := findServiceEntry(t, graphRAG.ServiceMap(context.Background(), 0), latencyService)
	if entry.Service.CallCount != latencyLowCount+latencyTailCount {
		t.Fatalf("legacy GraphRAG call count = %d", entry.Service.CallCount)
	}
	return graphRAG
}

func shutdownLatencyGraphRAG(t *testing.T, graphRAG *graphrag.GraphRAG) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := graphRAG.Shutdown(ctx); err != nil {
		t.Fatalf("drain latency GraphRAG: %v", err)
	}
}

func getProofJSON(t *testing.T, url string, target any) {
	t.Helper()
	response, err := http.Get(url) //nolint:gosec // local httptest endpoint
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status %d", url, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func decodeFirstJSON(t *testing.T, raw string, target any) {
	t.Helper()
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(target); err != nil {
		t.Fatalf("decode MCP JSON: %v (%s)", err, compact(raw))
	}
}

func findGraphNode(t *testing.T, nodes []api.GraphNode, name string) api.GraphNode {
	t.Helper()
	for _, node := range nodes {
		if node.ID == name {
			return node
		}
	}
	t.Fatalf("system graph omitted %q", name)
	return api.GraphNode{}
}

func findServiceMapNode(t *testing.T, nodes []views.ServiceMapNode, name string) latencySurface {
	t.Helper()
	for _, node := range nodes {
		if node.Name == name {
			return latencySurface{Value: node.P99LatencyMs, Unit: "milliseconds", Provenance: node.LatencyProvenance}
		}
	}
	t.Fatalf("service map omitted %q", name)
	return latencySurface{}
}

func findServiceEntry(t *testing.T, entries []graphrag.ServiceMapEntry, name string) graphrag.ServiceMapEntry {
	t.Helper()
	for _, entry := range entries {
		if entry.Service != nil && entry.Service.Name == name {
			return entry
		}
	}
	t.Fatalf("service map omitted %q", name)
	return graphrag.ServiceMapEntry{}
}

func p99(surface latencySurface) *latency.Percentile {
	if surface.Provenance == nil {
		return nil
	}
	return surface.Provenance.P99
}

func validMeasured(surface latencySurface, want float64, method string) bool {
	claim := p99(surface)
	return math.Abs(surface.Value-want) < 1e-9 && claim != nil &&
		claim.Status == latency.StatusMeasured && claim.Method == method &&
		claim.SampleCount == latencyLowCount+latencyTailCount && !claim.LowSample
}

// anyEstimated reports whether any surface still carries the pre-#291
// average-multiplier value or claim.
func anyEstimated(proof *latencyContractProof) bool {
	for _, surface := range []latencySurface{
		proof.Dashboard, proof.SystemGraph, proof.ServiceMap, proof.WebSocket,
		proof.GraphRAG, proof.MCPMap, proof.MCPHealth,
	} {
		claim := p99(surface)
		if claim == nil || claim.Status == latency.StatusEstimated || claim.Method == latency.MethodAverageMultiplier ||
			claim.EstimateFactor != 0 || math.Abs(surface.Value-52.225) <= 0.01 {
			return true
		}
	}
	return false
}

func validApproximate(surface latencySurface, exact float64) bool {
	claim := p99(surface)
	if claim == nil || claim.Status != latency.StatusApproximate || claim.Method != latency.MethodDDSketch ||
		claim.SampleCount != latencyLowCount+latencyTailCount || claim.RelativeErrorBound <= 0 || claim.Degraded {
		return false
	}
	return math.Abs(surface.Value-exact)/exact <= claim.RelativeErrorBound+1e-6
}

func sameLatency(left, right latencySurface) bool {
	lp, rp := p99(left), p99(right)
	return math.Abs(left.Value-right.Value) < 1e-9 && lp != nil && rp != nil &&
		lp.Status == rp.Status && lp.Method == rp.Method && lp.SampleCount == rp.SampleCount
}

func unavailableOperation(operation *graphrag.OperationNode) bool {
	if operation == nil || operation.P50Latency != 0 || operation.P95Latency != 0 || operation.P99Latency != 0 || operation.LatencyProvenance == nil {
		return false
	}
	claims := []*latency.Percentile{
		operation.LatencyProvenance.P50,
		operation.LatencyProvenance.P95,
		operation.LatencyProvenance.P99,
	}
	for _, claim := range claims {
		if claim == nil || claim.Status != latency.StatusUnavailable || claim.Reason != latency.ReasonPercentileNotRecorded {
			return false
		}
	}
	return true
}

func allSampleCounts(proof *latencyContractProof, want int) bool {
	for _, surface := range []latencySurface{
		proof.Dashboard, proof.SystemGraph, proof.ServiceMap, proof.WebSocket,
		proof.GraphRAG, proof.MCPMap, proof.MCPHealth,
	} {
		claim := p99(surface)
		if claim == nil || claim.SampleCount != uint64(want) {
			return false
		}
	}
	return true
}

func describeLatency(surface latencySurface) string {
	claim := p99(surface)
	if claim == nil {
		return fmt.Sprintf("value=%v%s provenance=nil", surface.Value, surface.Unit)
	}
	return fmt.Sprintf("value=%v%s status=%s method=%s samples=%d bound=%v", surface.Value, surface.Unit, claim.Status, claim.Method, claim.SampleCount, claim.RelativeErrorBound)
}
