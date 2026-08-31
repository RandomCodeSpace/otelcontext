package topologyproof

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/api"
	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/latency"
	"github.com/RandomCodeSpace/otelcontext/internal/mcp"
	"github.com/RandomCodeSpace/otelcontext/internal/realtime"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
	"github.com/coder/websocket"
)

const (
	proofModeEnv = "OTELCONTEXT_TOPOLOGY_PROOF_MODE"
	proofDirEnv  = "OTELCONTEXT_TOPOLOGY_PROOF_DIR"
	proofFloor   = 25 * time.Millisecond
)

type proofEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type proofAssertion struct {
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type modeProof struct {
	SchemaVersion    string                    `json:"schema_version"`
	Mode             string                    `json:"mode"`
	Source           string                    `json:"source"`
	Epochs           []string                  `json:"epochs"`
	Revisions        []uint64                  `json:"revisions"`
	EdgeSet          []proofEdge               `json:"edge_set"`
	EmptyReplacement string                    `json:"empty_replacement"`
	Reconnect        string                    `json:"reconnect"`
	Coverage         string                    `json:"coverage"`
	Assertions       map[string]proofAssertion `json:"assertions"`
}

func newModeProof(mode string, source topology.Source) *modeProof {
	return &modeProof{
		SchemaVersion: "otelcontext.topology-proof.v1",
		Mode:          mode,
		Source:        string(source),
		Epochs:        []string{},
		Revisions:     []uint64{},
		Assertions:    make(map[string]proofAssertion),
	}
}

func (p *modeProof) check(t *testing.T, name string, passed bool, detail string) {
	t.Helper()
	p.Assertions[name] = proofAssertion{Passed: passed, Detail: detail}
	if !passed {
		t.Errorf("%s: %s", name, detail)
	}
}

func (p *modeProof) write(t *testing.T) {
	t.Helper()
	dir := os.Getenv(proofDirEnv)
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("create proof directory: %v", err)
		return
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Errorf("marshal proof: %v", err)
		return
	}
	path := filepath.Join(dir, p.Mode+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Errorf("write proof: %v", err)
		return
	}
	t.Logf("topology proof: %s", path)
}

type scriptedProvider struct {
	mu       sync.RWMutex
	source   topology.Source
	identity topology.Identity
	snapshot topology.Snapshot
	err      error

	aggEpoch uint64
	aggSnap  aggregate.TopologySnapshot
}

func (p *scriptedProvider) Source() topology.Source { return p.source }

func (p *scriptedProvider) Identity(context.Context) topology.Identity {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.identity
}

func (p *scriptedProvider) Snapshot(context.Context, topology.Query) (topology.Snapshot, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.err != nil {
		return topology.Snapshot{}, p.err
	}
	snapshot := p.snapshot
	snapshot.Nodes = append([]topology.Node(nil), snapshot.Nodes...)
	snapshot.Edges = append([]topology.Edge(nil), snapshot.Edges...)
	return snapshot, nil
}

func (p *scriptedProvider) TopologyEpoch() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.aggEpoch
}

func (*scriptedProvider) TopologyTenants() []string { return []string{storage.DefaultTenantID} }

func (p *scriptedProvider) TopologyRevision(string) uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.aggSnap.Revision
}

func (p *scriptedProvider) TopologySnapshot(string) aggregate.TopologySnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	snapshot := p.aggSnap
	snapshot.Services = append([]aggregate.TopologyService(nil), snapshot.Services...)
	snapshot.Operations = append([]aggregate.TopologyOperation(nil), snapshot.Operations...)
	snapshot.Edges = append([]aggregate.SnapshotEdge(nil), snapshot.Edges...)
	snapshot.Metrics = append([]aggregate.TopologyMetric(nil), snapshot.Metrics...)
	return snapshot
}

func (*scriptedProvider) PruneTopology() {}

func (p *scriptedProvider) setAggregateState(epoch string, epochNumber, revision uint64, target string, empty bool, providerErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.identity = topology.Identity{Epoch: epoch, Revision: revision}
	p.err = providerErr
	p.aggEpoch = epochNumber
	p.snapshot = topologySnapshot(p.source, epoch, revision, target, empty)
	p.aggSnap = aggregateSnapshot(epochNumber, revision, target, empty)
}

func (p *scriptedProvider) setAggregateTruncated(epoch string, epochNumber, revision uint64, target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.identity = topology.Identity{Epoch: epoch, Revision: revision}
	p.err = nil
	p.aggEpoch = epochNumber
	p.snapshot = topologySnapshot(p.source, epoch, revision, target, false)
	p.aggSnap = aggregateSnapshot(epochNumber, revision, target, false)
	p.snapshot.Meta.Coverage = string(aggregate.CoverageSampled)
	p.snapshot.Meta.CoverageNote = aggregate.CoverageSampled.Note()
	p.snapshot.Meta.Truncated = true
	p.snapshot.Meta.DroppedEdges = 2
	p.aggSnap.DroppedEdges = 2
}

func (p *scriptedProvider) bumpShadowRevision(revision uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.aggSnap.Revision = revision
}

func topologySnapshot(source topology.Source, epoch string, revision uint64, target string, empty bool) topology.Snapshot {
	meta := topology.Metadata{Source: source}
	if source == topology.SourceAggregate {
		meta.Coverage = string(aggregate.CoverageFull)
		meta.CoverageNote = aggregate.CoverageFull.Note()
		meta.Epoch = epoch
		meta.Revision = revision
	}
	if empty {
		return topology.Snapshot{Nodes: []topology.Node{}, Edges: []topology.Edge{}, Meta: meta}
	}
	p99 := &latency.Percentile{
		Status: latency.StatusEstimated, Method: latency.MethodAverageMultiplier,
		SampleCount: 1000, EstimateFactor: 2.5,
	}
	p99LatencyMs := 52.225
	if source == topology.SourceAggregate {
		p99 = &latency.Percentile{
			Status: latency.StatusApproximate, Method: latency.MethodDDSketch,
			SampleCount: 1000, SketchScale: aggregate.SketchDefaultScale, RelativeErrorBound: 0.0217,
		}
		p99LatencyMs = 1000
	}
	return topology.Snapshot{
		Nodes: []topology.Node{
			{Name: "gateway", TotalTraces: 1000, AvgLatencyMs: 20.89, P99LatencyMs: p99LatencyMs, LatencyProvenance: &latency.Provenance{P99: p99}, SpanCount: 1000, HealthScore: 100, Status: "healthy"},
			{Name: target, TotalTraces: 1000, AvgLatencyMs: 20.89, P99LatencyMs: p99LatencyMs, LatencyProvenance: &latency.Provenance{P99: p99}, SpanCount: 1000, HealthScore: 100, Status: "healthy"},
		},
		Edges: []topology.Edge{{Source: "gateway", Target: target, CallCount: 10, AvgLatencyMs: 4, Status: "healthy"}},
		Meta:  meta,
	}
}

func aggregateSnapshot(epoch, revision uint64, target string, empty bool) aggregate.TopologySnapshot {
	now := time.Now().UTC().Truncate(aggregate.WindowSize)
	base := aggregate.TopologySnapshot{
		Tenant:     storage.DefaultTenantID,
		Epoch:      epoch,
		Revision:   revision,
		Now:        now,
		Services:   []aggregate.TopologyService{},
		Operations: []aggregate.TopologyOperation{},
		Edges:      []aggregate.SnapshotEdge{},
		Metrics:    []aggregate.TopologyMetric{},
	}
	if empty {
		return base
	}
	window := aggregate.TopologyWindow{
		Start:             now,
		End:               now.Add(aggregate.WindowSize),
		Closed:            true,
		Final:             true,
		Elapsed:           aggregate.WindowSize,
		Count:             1000,
		DurationCount:     1000,
		DurationSumMicros: 20_890_000,
		P95Micros:         10_000,
		P99Micros:         1_000_000,
		LatencyProvenance: &latency.Provenance{P95: &latency.Percentile{
			Status: latency.StatusApproximate, Method: latency.MethodDDSketch, SampleCount: 1000, SketchScale: aggregate.SketchDefaultScale, RelativeErrorBound: 0.0217,
		}, P99: &latency.Percentile{
			Status: latency.StatusApproximate, Method: latency.MethodDDSketch, SampleCount: 1000, SketchScale: aggregate.SketchDefaultScale, RelativeErrorBound: 0.0217,
		}},
	}
	base.Services = []aggregate.TopologyService{
		{Name: "gateway", FirstSeen: now, LastSeen: window.End, Windows: []aggregate.TopologyWindow{window}},
		{Name: target, FirstSeen: now, LastSeen: window.End, Windows: []aggregate.TopologyWindow{window}},
	}
	base.Edges = []aggregate.SnapshotEdge{{
		Caller: "gateway", Callee: target, FirstSeen: now, LastSeen: window.End,
		Windows: []aggregate.TopologyWindow{window},
	}}
	return base
}

type providerPublisher struct{ provider *scriptedProvider }

func (p providerPublisher) Epoch() string { return p.provider.Identity(context.Background()).Epoch }

func (p providerPublisher) Revision() uint64 {
	return p.provider.Identity(context.Background()).Revision
}

func (p providerPublisher) Snapshot(ctx context.Context, _ string) *realtime.LiveSnapshot {
	snapshot, err := p.provider.Snapshot(ctx, topology.Query{})
	if err != nil {
		return nil
	}
	nodes := make([]storage.ServiceMapNode, len(snapshot.Nodes))
	for i, node := range snapshot.Nodes {
		nodes[i] = storage.ServiceMapNode{Name: node.Name, TotalTraces: node.TotalTraces, ErrorCount: node.ErrorCount, AvgLatencyMs: node.AvgLatencyMs, P99LatencyMs: node.P99LatencyMs, LatencyProvenance: node.LatencyProvenance}
	}
	edges := make([]storage.ServiceMapEdge, len(snapshot.Edges))
	for i, edge := range snapshot.Edges {
		edges[i] = storage.ServiceMapEdge{Source: edge.Source, Target: edge.Target, CallCount: edge.CallCount, AvgLatencyMs: edge.AvgLatencyMs, ErrorRate: edge.ErrorRate}
	}
	return &realtime.LiveSnapshot{
		Type:              "live_snapshot",
		ServiceMap:        &storage.ServiceMapMetrics{Nodes: nodes, Edges: edges},
		Source:            string(snapshot.Meta.Source),
		Coverage:          snapshot.Meta.Coverage,
		CoverageNote:      snapshot.Meta.CoverageNote,
		Epoch:             snapshot.Meta.Epoch,
		Revision:          snapshot.Meta.Revision,
		Truncated:         snapshot.Meta.Truncated,
		DroppedServices:   snapshot.Meta.DroppedServices,
		DroppedOperations: snapshot.Meta.DroppedOperations,
		DroppedEdges:      snapshot.Meta.DroppedEdges,
		DroppedMetrics:    snapshot.Meta.DroppedMetrics,
	}
}

type topologyWire struct {
	Nodes        []json.RawMessage `json:"nodes"`
	Edges        []proofEdge       `json:"edges"`
	Source       string            `json:"source"`
	Coverage     string            `json:"coverage"`
	Epoch        string            `json:"epoch"`
	Revision     uint64            `json:"revision"`
	Truncated    bool              `json:"truncated"`
	DroppedEdges uint64            `json:"dropped_edges"`
}

func TestTopologyModeProof(t *testing.T) {
	mode := os.Getenv(proofModeEnv)
	if mode == "" {
		t.Skipf("%s is set by the dedicated topology-proof CI job", proofModeEnv)
	}
	if mode != aggregate.ModeLegacy && mode != aggregate.ModeShadow && mode != aggregate.ModeAggregate {
		t.Fatalf("unsupported proof mode %q", mode)
	}

	source := topology.SourceLegacy
	target := "legacy-payments"
	forbidden := "aggregate-payments"
	if mode == aggregate.ModeAggregate {
		source = topology.SourceAggregate
		target = "aggregate-payments"
		forbidden = "legacy-payments"
	}
	proof := newModeProof(mode, source)
	defer proof.write(t)

	provider := &scriptedProvider{source: source}
	if source == topology.SourceAggregate {
		provider.setAggregateState("epoch-a", 41, 1, target, false, nil)
		proof.Epochs = []string{"epoch-a", "epoch-b"}
		proof.Revisions = []uint64{1, 2, 3, 4}
		proof.Coverage = string(aggregate.CoverageFull)
	} else {
		provider.identity = topology.Identity{}
		provider.snapshot = topologySnapshot(source, "", 0, target, false)
		provider.aggEpoch = 41
		provider.aggSnap = aggregateSnapshot(41, 1, "aggregate-payments", false)
		proof.Coverage = "omitted (legacy-compatible)"
	}
	proof.EdgeSet = []proofEdge{{Source: "gateway", Target: target}}

	repo := proofRepository(t)
	seedLegacyEdge(t, repo, target)
	graphRAG, stopGraphRAG := proofGraphRAG(t, mode, provider, target)
	defer stopGraphRAG()

	metrics := telemetry.New()
	var rawConnections atomic.Int64
	rawHub := realtime.NewHub(func(count int) { rawConnections.Store(int64(count)) })
	rawHub.SetTopologyProvider(provider, proofFloor)
	if mode == aggregate.ModeAggregate {
		rawHub.SetAggregateMode(true)
	}
	go rawHub.Run()

	eventHub := realtime.NewEventHub(repo, nil, nil)
	if mode == aggregate.ModeAggregate {
		eventHub.SetAggregatePublisher(providerPublisher{provider: provider}, proofFloor)
	}
	eventCtx, cancelEvents := context.WithCancel(context.Background())
	go eventHub.Start(eventCtx, 50*time.Millisecond, 20*time.Millisecond)

	apiServer := api.NewServer(repo, rawHub, eventHub, metrics)
	apiServer.SetGraphRAG(graphRAG)
	apiServer.SetTopologyProvider(provider)
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
	})

	serviceMap := getTopology(t, httpServer.URL+"/api/metrics/service-map")
	proof.check(t, "rest_service_map", hasOnlyEdge(serviceMap.Edges, target, forbidden), fmt.Sprintf("edges=%v", serviceMap.Edges))
	systemGraph := getTopology(t, httpServer.URL+"/api/system/graph")
	proof.check(t, "rest_system_graph", hasOnlyEdge(systemGraph.Edges, target, forbidden), fmt.Sprintf("edges=%v", systemGraph.Edges))

	if source == topology.SourceAggregate {
		metadataOK := serviceMap.Source == "aggregate" && serviceMap.Coverage == "full" && serviceMap.Epoch == "epoch-a" && serviceMap.Revision == 1
		proof.check(t, "coverage_additive", metadataOK, fmt.Sprintf("source=%q coverage=%q identity=%s/%d", serviceMap.Source, serviceMap.Coverage, serviceMap.Epoch, serviceMap.Revision))
	} else {
		metadataOK := serviceMap.Source == "" && serviceMap.Coverage == "" && serviceMap.Epoch == "" && serviceMap.Revision == 0
		proof.check(t, "legacy_wire_compatibility", metadataOK, fmt.Sprintf("source=%q coverage=%q identity=%s/%d", serviceMap.Source, serviceMap.Coverage, serviceMap.Epoch, serviceMap.Revision))
		cached := getTopologyResponse(t, httpServer.URL+"/api/system/graph")
		proof.check(t, "legacy_cache_timing", cached.Header.Get("X-Cache") == "HIT", "second legacy graph read keeps the established cache window")
		cached.Body.Close()
	}

	graphEdges := graphRAGEdges(graphRAG)
	proof.check(t, "graphrag", hasOnlyEdge(graphEdges, target, forbidden), fmt.Sprintf("edges=%v", graphEdges))

	mapTool := callTool(t, mcpHTTP.URL, "get_service_map", nil)
	proof.check(t, "mcp_get_service_map", strings.Contains(mapTool, target) && !strings.Contains(mapTool, forbidden), compact(mapTool))
	healthTool := callTool(t, mcpHTTP.URL, "get_service_health", map[string]any{"service_name": target})
	proof.check(t, "mcp_get_service_health", strings.Contains(healthTool, target) && !strings.Contains(healthTool, forbidden), compact(healthTool))
	if source == topology.SourceAggregate {
		proof.check(t, "mcp_coverage", strings.Contains(mapTool, `"coverage":"full"`) && strings.Contains(mapTool, `"source":"aggregate"`), compact(mapTool))
	}

	sse := immediateSSE(t, mcpServer)
	proof.check(t, "mcp_sse", strings.Contains(sse, target) && !strings.Contains(sse, forbidden) && strings.Contains(sse, "notifications/resources/updated"), compact(sse))

	eventConn := dialWS(t, httpServer.URL+"/ws/events")
	initialEvent := readEventSnapshot(t, eventConn, 2*time.Second)
	proof.check(t, "websocket_events", initialEvent.ServiceMap != nil && hasStorageEdge(initialEvent.ServiceMap.Edges, target, forbidden), fmt.Sprintf("snapshot=%+v", initialEvent.ServiceMap))
	proof.check(t, "reconnect_immediate", initialEvent.Type == "live_snapshot" && (source != topology.SourceAggregate || initialEvent.Reset), fmt.Sprintf("type=%q reset=%v", initialEvent.Type, initialEvent.Reset))

	rawConn := dialWS(t, httpServer.URL+"/ws")
	waitFor(t, 2*time.Second, func() bool { return rawConnections.Load() == 1 })
	if source == topology.SourceAggregate {
		initialRaw := readRawRefresh(t, rawConn, 2*time.Second)
		proof.check(t, "websocket_raw", initialRaw.Source == "aggregate" && initialRaw.Epoch == "epoch-a" && initialRaw.Revision == 1 && initialRaw.Reset, fmt.Sprintf("refresh=%+v", initialRaw))
	} else {
		provider.bumpShadowRevision(2)
		time.Sleep(3 * proofFloor)
		rawHub.Broadcast(realtime.LogEntry{ServiceName: "gateway", Body: "legacy-batch"})
		rawType := readEnvelopeType(t, rawConn, 2*time.Second)
		eventHub.BroadcastLog(realtime.LogEntry{ServiceName: "gateway", Body: "legacy-batch"})
		eventType := readEnvelopeType(t, eventConn, 2*time.Second)
		proof.check(t, "websocket_raw", rawType == "logs", fmt.Sprintf("message_type=%q", rawType))
		proof.check(t, "shadow_revision_suppressed", mode != aggregate.ModeShadow || (rawType == "logs" && eventType == "logs"), fmt.Sprintf("raw=%q events=%q", rawType, eventType))
		proof.EmptyReplacement = "not applicable; legacy cache and stream timing preserved"
		proof.Reconnect = "immediate full /ws/events and MCP SSE snapshots"
		return
	}

	// Move the aggregate identity and prove every cache/stream consumes the
	// replacement. The stale same-epoch revision is deliberately inserted
	// between 2 and 3; neither WebSocket may publish it.
	provider.setAggregateState("epoch-a", 41, 2, target, false, nil)
	nextEvent, eventRegressed := readEventUntil(t, eventConn, "epoch-a", 2, 2*time.Second)
	nextRaw, rawRegressed := readRawUntil(t, rawConn, "epoch-a", 2, 2*time.Second)
	proof.check(t, "same_epoch_monotonic", !eventRegressed && !rawRegressed && !nextEvent.Reset && !nextRaw.Reset, fmt.Sprintf("event=%s/%d raw=%s/%d", nextEvent.Epoch, nextEvent.Revision, nextRaw.Epoch, nextRaw.Revision))
	cacheMove := getTopologyResponse(t, httpServer.URL+"/api/system/graph")
	cacheBody := decodeTopology(t, cacheMove)
	proof.check(t, "cache_identity", cacheMove.Header.Get("X-Cache") == "MISS" && hasOnlyEdge(cacheBody.Edges, target, forbidden), fmt.Sprintf("cache=%q edges=%v", cacheMove.Header.Get("X-Cache"), cacheBody.Edges))
	cacheMove.Body.Close()

	provider.setAggregateState("epoch-a", 41, 1, "stale-exemplar", false, nil)
	time.Sleep(3 * proofFloor)
	provider.setAggregateState("epoch-a", 41, 3, target, false, nil)
	_, eventRegressed = readEventUntil(t, eventConn, "epoch-a", 3, 2*time.Second)
	_, rawRegressed = readRawUntil(t, rawConn, "epoch-a", 3, 2*time.Second)
	proof.check(t, "stale_exemplar_excluded", !eventRegressed && !rawRegressed, "same-epoch stale exemplar replacement was not published")

	provider.setAggregateState("epoch-b", 42, 1, target, false, nil)
	epochEvent, _ := readEventUntil(t, eventConn, "epoch-b", 1, 2*time.Second)
	epochRaw, _ := readRawUntil(t, rawConn, "epoch-b", 1, 2*time.Second)
	proof.check(t, "epoch_reset", epochEvent.Reset && epochRaw.Reset, fmt.Sprintf("event_reset=%v raw_reset=%v", epochEvent.Reset, epochRaw.Reset))

	provider.setAggregateState("epoch-b", 42, 2, target, false, errors.New("proof provider unavailable"))
	time.Sleep(3 * proofFloor)
	errorSSE := immediateSSE(t, mcpServer)
	provider.setAggregateState("epoch-b", 42, 2, target, false, nil)
	recoveredEvent, _ := readEventUntil(t, eventConn, "epoch-b", 2, 2*time.Second)
	recoveredRaw, _ := readRawUntil(t, rawConn, "epoch-b", 2, 2*time.Second)
	retained := !strings.Contains(errorSSE, "notifications/resources/updated") && hasStorageEdge(recoveredEvent.ServiceMap.Edges, target, forbidden) && recoveredRaw.Revision == 2
	proof.check(t, "provider_error_retains_last_good", retained, fmt.Sprintf("error_sse=%s event_revision=%d raw_revision=%d", compact(errorSSE), recoveredEvent.Revision, recoveredRaw.Revision))

	provider.setAggregateTruncated("epoch-b", 42, 3, target)
	truncatedEvent, _ := readEventUntil(t, eventConn, "epoch-b", 3, 2*time.Second)
	_, _ = readRawUntil(t, rawConn, "epoch-b", 3, 2*time.Second)
	truncatedREST := getTopology(t, httpServer.URL+"/api/metrics/service-map")
	truncatedTool := callTool(t, mcpHTTP.URL, "get_service_map", nil)
	truncationOK := truncatedEvent.Coverage == "sampled" && truncatedEvent.Truncated && truncatedEvent.DroppedEdges == 2 && truncatedREST.Coverage == "sampled" && truncatedREST.Truncated && truncatedREST.DroppedEdges == 2 && strings.Contains(truncatedTool, `"coverage":"sampled"`) && strings.Contains(truncatedTool, `"dropped_edges":2`) && strings.Contains(truncatedTool, `"truncated":true`)
	proof.check(t, "truncation_metadata_additive", truncationOK, fmt.Sprintf("event=%q/%v/%d rest=%q/%v/%d tool=%s", truncatedEvent.Coverage, truncatedEvent.Truncated, truncatedEvent.DroppedEdges, truncatedREST.Coverage, truncatedREST.Truncated, truncatedREST.DroppedEdges, compact(truncatedTool)))

	provider.setAggregateState("epoch-b", 42, 4, target, true, nil)
	emptyEvent, _ := readEventUntil(t, eventConn, "epoch-b", 4, 2*time.Second)
	emptyRaw, _ := readRawUntil(t, rawConn, "epoch-b", 4, 2*time.Second)
	emptyREST := getTopology(t, httpServer.URL+"/api/system/graph")
	emptySSE := immediateSSE(t, mcpServer)
	emptyGraph := graphRAG.ServiceMap(context.Background(), 0)
	emptyOK := emptyEvent.ServiceMap != nil && emptyEvent.ServiceMap.Nodes != nil && emptyEvent.ServiceMap.Edges != nil && len(emptyEvent.ServiceMap.Nodes) == 0 && len(emptyEvent.ServiceMap.Edges) == 0 && emptyRaw.Revision == 4 && emptyREST.Nodes != nil && emptyREST.Edges != nil && len(emptyREST.Nodes) == 0 && len(emptyREST.Edges) == 0 && strings.Contains(emptySSE, "notifications/resources/updated") && len(emptyGraph) == 0
	proof.check(t, "empty_replacement", emptyOK, fmt.Sprintf("event_nodes=%d event_edges=%d rest_nodes=%d rest_edges=%d graphrag=%d", len(emptyEvent.ServiceMap.Nodes), len(emptyEvent.ServiceMap.Edges), len(emptyREST.Nodes), len(emptyREST.Edges), len(emptyGraph)))

	_, failClosed := topology.NewAggregateProvider(nil)
	proof.check(t, "aggregate_provider_fail_closed", failClosed != nil, "nil aggregate engine is rejected")
	proof.EmptyReplacement = "epoch-b/revision-4 published nodes=[] edges=[] and cleared GraphRAG"
	proof.Reconnect = "immediate full /ws/events, /ws refresh, and MCP SSE snapshots"
}

func proofRepository(t *testing.T) *storage.Repository {
	t.Helper()
	db, err := storage.NewDatabase("sqlite", filepath.Join(t.TempDir(), "topology-proof.db"))
	if err != nil {
		t.Fatalf("open proof database: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("migrate proof database: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func seedLegacyEdge(t *testing.T, repo *storage.Repository, target string) {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	spans := []storage.Span{
		{TenantID: storage.DefaultTenantID, TraceID: "proof-trace", SpanID: "proof-parent", ServiceName: "gateway", OperationName: "GET /", StartTime: now, EndTime: now.Add(time.Millisecond), Duration: 1_000},
		{TenantID: storage.DefaultTenantID, TraceID: "proof-trace", SpanID: "proof-child", ParentSpanID: "proof-parent", ServiceName: target, OperationName: "GET /pay", StartTime: now.Add(time.Millisecond), EndTime: now.Add(5 * time.Millisecond), Duration: 4_000},
	}
	if err := repo.BatchCreateSpans(spans); err != nil {
		t.Fatalf("seed proof spans: %v", err)
	}
}

func proofGraphRAG(t *testing.T, mode string, provider *scriptedProvider, target string) (*graphrag.GraphRAG, func()) {
	t.Helper()
	cfg := graphrag.DefaultConfig()
	cfg.Mode = mode
	cfg.WorkerCount = 1
	cfg.ChannelSize = 16
	cfg.RefreshEvery = time.Hour
	cfg.SnapshotEvery = time.Hour
	cfg.AnomalyEvery = time.Hour
	graphRAG := graphrag.New(nil, nil, nil, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	if mode == aggregate.ModeAggregate {
		graphRAG.SetAggregateSource(provider)
	}
	graphRAG.Start(ctx)
	if mode != aggregate.ModeAggregate {
		now := time.Now().UTC().Add(-time.Minute)
		graphRAG.OnSpanIngested(storage.Span{TenantID: storage.DefaultTenantID, TraceID: "proof-trace", SpanID: "proof-parent", ServiceName: "gateway", OperationName: "GET /", StartTime: now, Duration: 1_000})
		graphRAG.OnSpanIngested(storage.Span{TenantID: storage.DefaultTenantID, TraceID: "proof-trace", SpanID: "proof-child", ParentSpanID: "proof-parent", ServiceName: target, OperationName: "GET /pay", StartTime: now.Add(time.Millisecond), Duration: 4_000})
		waitFor(t, 2*time.Second, func() bool { return hasOnlyEdge(graphRAGEdges(graphRAG), target, "aggregate-payments") })
	}
	return graphRAG, func() {
		cancel()
		graphRAG.Stop()
	}
}

func getTopology(t *testing.T, url string) topologyWire {
	t.Helper()
	response := getTopologyResponse(t, url)
	defer response.Body.Close()
	return decodeTopology(t, response)
}

func getTopologyResponse(t *testing.T, url string) *http.Response {
	t.Helper()
	response, err := http.Get(url) //nolint:gosec // local httptest endpoint
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("GET %s status %d: %s", url, response.StatusCode, body)
	}
	return response
}

func decodeTopology(t *testing.T, response *http.Response) topologyWire {
	t.Helper()
	var body topologyWire
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode topology: %v", err)
	}
	return body
}

func callTool(t *testing.T, endpoint, name string, arguments map[string]any) string {
	t.Helper()
	if arguments == nil {
		arguments = map[string]any{}
	}
	requestBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": arguments},
	})
	if err != nil {
		t.Fatalf("marshal MCP call: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("new MCP request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("MCP call: %v", err)
	}
	defer response.Body.Close()
	var envelope struct {
		Result mcp.ToolCallResult `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode MCP call: %v", err)
	}
	var text strings.Builder
	for _, item := range envelope.Result.Content {
		text.WriteString(item.Text)
		if item.Resource != nil {
			text.WriteString(item.Resource.Text)
		}
	}
	return text.String()
}

func immediateSSE(t *testing.T, server *mcp.Server) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder.Body.String()
}

func dialWS(t *testing.T, httpURL string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpURL, "http"), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial %s: %v", httpURL, err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "proof complete") })
	return conn
}

func readEventSnapshot(t *testing.T, conn *websocket.Conn, timeout time.Duration) realtime.LiveSnapshot {
	t.Helper()
	message := readWS(t, conn, timeout)
	var snapshot realtime.LiveSnapshot
	if err := json.Unmarshal(message, &snapshot); err != nil {
		t.Fatalf("decode event snapshot: %v (%s)", err, message)
	}
	return snapshot
}

type rawRefresh struct {
	Source   string `json:"source"`
	Epoch    string `json:"epoch"`
	Revision uint64 `json:"revision"`
	Reset    bool   `json:"reset"`
}

func readRawRefresh(t *testing.T, conn *websocket.Conn, timeout time.Duration) rawRefresh {
	t.Helper()
	message := readWS(t, conn, timeout)
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		t.Fatalf("decode raw envelope: %v (%s)", err, message)
	}
	if envelope.Type != "topology_refresh" {
		t.Fatalf("raw message type = %q, want topology_refresh", envelope.Type)
	}
	var refresh rawRefresh
	if err := json.Unmarshal(envelope.Data, &refresh); err != nil {
		t.Fatalf("decode raw refresh: %v", err)
	}
	return refresh
}

func readEventUntil(t *testing.T, conn *websocket.Conn, epoch string, revision uint64, timeout time.Duration) (realtime.LiveSnapshot, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	regressed := false
	for time.Now().Before(deadline) {
		snapshot := readEventSnapshot(t, conn, time.Until(deadline))
		if snapshot.Epoch == epoch && snapshot.Revision < revision {
			regressed = true
		}
		if snapshot.Epoch == epoch && snapshot.Revision == revision {
			return snapshot, regressed
		}
	}
	t.Fatalf("event stream did not reach %s/%d", epoch, revision)
	return realtime.LiveSnapshot{}, regressed
}

func readRawUntil(t *testing.T, conn *websocket.Conn, epoch string, revision uint64, timeout time.Duration) (rawRefresh, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	regressed := false
	for time.Now().Before(deadline) {
		refresh := readRawRefresh(t, conn, time.Until(deadline))
		if refresh.Epoch == epoch && refresh.Revision < revision {
			regressed = true
		}
		if refresh.Epoch == epoch && refresh.Revision == revision {
			return refresh, regressed
		}
	}
	t.Fatalf("raw stream did not reach %s/%d", epoch, revision)
	return rawRefresh{}, regressed
}

func readEnvelopeType(t *testing.T, conn *websocket.Conn, timeout time.Duration) string {
	t.Helper()
	message := readWS(t, conn, timeout)
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, message)
	}
	return envelope.Type
}

func readWS(t *testing.T, conn *websocket.Conn, timeout time.Duration) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, message, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read websocket: %v", err)
	}
	return message
}

func graphRAGEdges(graphRAG *graphrag.GraphRAG) []proofEdge {
	edges := graphRAG.AllServiceEdges(context.Background())
	out := make([]proofEdge, 0, len(edges))
	for _, edge := range edges {
		if edge != nil && edge.Type == graphrag.EdgeCalls {
			out = append(out, proofEdge{Source: edge.FromID, Target: edge.ToID})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Target < out[j].Target
	})
	return out
}

func hasOnlyEdge(edges []proofEdge, target, forbidden string) bool {
	if len(edges) != 1 {
		return false
	}
	return edges[0].Source == "gateway" && edges[0].Target == target && edges[0].Target != forbidden
}

func hasStorageEdge(edges []storage.ServiceMapEdge, target, forbidden string) bool {
	if len(edges) != 1 {
		return false
	}
	return edges[0].Source == "gateway" && edges[0].Target == target && edges[0].Target != forbidden
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

func compact(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 240 {
		return value[:240] + "..."
	}
	return value
}
