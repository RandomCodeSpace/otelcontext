package topology

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/latency"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

type fakeLegacyRepository struct {
	result *storage.ServiceMapMetrics
	err    error
}

func (f fakeLegacyRepository) GetServiceMapMetrics(context.Context, time.Time, time.Time) (*storage.ServiceMapMetrics, error) {
	return f.result, f.err
}

func TestLegacyProviderExplicitRangeUsesLegacyTopology(t *testing.T) {
	repo := fakeLegacyRepository{result: &storage.ServiceMapMetrics{
		Nodes: []storage.ServiceMapNode{
			{Name: "gateway", TotalTraces: 4, P99LatencyMs: 52, LatencyProvenance: &latency.Provenance{P99: &latency.Percentile{Status: latency.StatusEstimated, Method: latency.MethodAverageMultiplier, SampleCount: 4, EstimateFactor: 2.5}}},
			{Name: "legacy-payments", TotalTraces: 3},
		},
		Edges: []storage.ServiceMapEdge{{Source: "gateway", Target: "legacy-payments", CallCount: 3}},
	}}
	provider, err := NewLegacyProvider(repo, nil, nil)
	if err != nil {
		t.Fatalf("NewLegacyProvider: %v", err)
	}

	start := time.Unix(100, 0).UTC()
	end := start.Add(time.Minute)
	snapshot, err := provider.Snapshot(context.Background(), Query{Start: start, End: end})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Meta.Source != SourceLegacy {
		t.Fatalf("source = %q, want %q", snapshot.Meta.Source, SourceLegacy)
	}
	if len(snapshot.Nodes) != 2 || len(snapshot.Edges) != 1 {
		t.Fatalf("snapshot = %+v, want two nodes and one edge", snapshot)
	}
	if got := snapshot.Edges[0]; got.Source != "gateway" || got.Target != "legacy-payments" {
		t.Fatalf("edge = %+v, want gateway -> legacy-payments", got)
	}
	if got := snapshot.Nodes[0]; got.P99LatencyMs != 52 || got.LatencyProvenance == nil || got.LatencyProvenance.P99.Status != latency.StatusEstimated {
		t.Fatalf("latency contract = %+v", got)
	}
}

func TestAggregateProjectionCarriesLatestSketchProvenance(t *testing.T) {
	p99 := &latency.Percentile{Status: latency.StatusApproximate, Method: latency.MethodDDSketch, SampleCount: 1000, SketchScale: 4, RelativeErrorBound: 0.0217}
	snapshot := fromAggregateProjection(aggregate.TopologySnapshot{Services: []aggregate.TopologyService{{
		Name: "checkout",
		Windows: []aggregate.TopologyWindow{{
			Count: 1000, DurationCount: 1000, P99Micros: 1_000_000,
			LatencyProvenance: &latency.Provenance{P99: p99},
		}},
	}}})
	if len(snapshot.Nodes) != 1 {
		t.Fatalf("nodes = %+v", snapshot.Nodes)
	}
	node := snapshot.Nodes[0]
	if node.P99LatencyMs != 1000 || node.LatencyProvenance == nil || node.LatencyProvenance.P99.SampleCount != 1000 || node.LatencyProvenance.P99.RelativeErrorBound != 0.0217 {
		t.Fatalf("node latency = %+v", node)
	}
}

func TestLegacyProviderReturnsNonNilEmptySlices(t *testing.T) {
	provider, err := NewLegacyProvider(fakeLegacyRepository{result: &storage.ServiceMapMetrics{}}, nil, nil)
	if err != nil {
		t.Fatalf("NewLegacyProvider: %v", err)
	}
	snapshot, err := provider.Snapshot(context.Background(), Query{Start: time.Unix(1, 0), End: time.Unix(2, 0)})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Nodes == nil || snapshot.Edges == nil {
		t.Fatalf("empty replacement contains nil slices: nodes=%v edges=%v", snapshot.Nodes, snapshot.Edges)
	}
}

func TestAggregateProviderRequiresAggregateMode(t *testing.T) {
	engine, err := aggregate.NewEngine(aggregate.EngineConfig{Mode: aggregate.ModeShadow})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if _, err := NewAggregateProvider(engine); err == nil {
		t.Fatal("NewAggregateProvider accepted a shadow engine")
	}
}

func TestAggregateProviderLiveSnapshotCarriesActualWindow(t *testing.T) {
	now := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	engine, err := aggregate.NewEngine(aggregate.EngineConfig{
		Mode: aggregate.ModeAggregate,
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	provider, err := NewAggregateProvider(engine)
	if err != nil {
		t.Fatalf("NewAggregateProvider: %v", err)
	}
	snapshot, err := provider.Snapshot(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !snapshot.Meta.End.Equal(now) || !snapshot.Meta.Start.Equal(now.Add(-engine.TopologyHorizon())) {
		t.Fatalf("live window = %s..%s, want %s..%s", snapshot.Meta.Start, snapshot.Meta.End, now.Add(-engine.TopologyHorizon()), now)
	}
}

func TestAggregateMetadataReportsProjectionTruncation(t *testing.T) {
	meta := aggregateMetadata(Identity{Epoch: "boot-a", Revision: 7}, aggregate.TopologySnapshot{
		DroppedServices:   1,
		DroppedOperations: 2,
		DroppedEdges:      3,
		DroppedMetrics:    4,
	})
	if meta.Coverage != string(aggregate.CoverageSampled) || !meta.Truncated {
		t.Fatalf("coverage=%q truncated=%v, want sampled/true", meta.Coverage, meta.Truncated)
	}
	if meta.CoverageNote == "" {
		t.Fatal("truncated topology has no coverage explanation")
	}
	if meta.DroppedServices != 1 || meta.DroppedOperations != 2 || meta.DroppedEdges != 3 || meta.DroppedMetrics != 4 {
		t.Fatalf("dropped counts = %+v", meta)
	}
}

func TestIdentityStringIsStable(t *testing.T) {
	if got := (Identity{Epoch: "boot-a", Revision: 42}).String(); got != "boot-a:42" {
		t.Fatalf("Identity.String() = %q, want boot-a:42", got)
	}
}
