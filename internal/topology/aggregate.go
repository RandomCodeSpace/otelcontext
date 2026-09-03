package topology

import (
	"context"
	"errors"
	"math"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/latency"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// AggregateProvider is both the ordinary mode-selected provider and the
// narrow aggregate projection source GraphRAG already consumes.
type AggregateProvider struct {
	HostReader
	engine *aggregate.Engine
}

func NewAggregateProvider(engine *aggregate.Engine) (*AggregateProvider, error) {
	if engine == nil {
		return nil, errors.New("aggregate topology provider requires an engine")
	}
	if engine.Mode() != aggregate.ModeAggregate {
		return nil, errors.New("aggregate topology provider requires AGGREGATE_MODE=aggregate")
	}
	return &AggregateProvider{engine: engine}, nil
}

func (*AggregateProvider) Source() Source { return SourceAggregate }

func (p *AggregateProvider) Identity(context.Context) Identity {
	p.engine.PruneTopology()
	return Identity{Epoch: p.engine.Epoch(), Revision: p.engine.Revision()}
}

func (p *AggregateProvider) Snapshot(ctx context.Context, q Query) (Snapshot, error) {
	snap, err := p.snapshot(ctx, q)
	if err != nil {
		return snap, err
	}
	p.stampHosts(ctx, &snap)
	return snap, nil
}

func (p *AggregateProvider) snapshot(ctx context.Context, q Query) (Snapshot, error) {
	id := p.Identity(ctx)
	tenant := storage.TenantFromContext(ctx)
	projection := p.engine.TopologySnapshot(tenant)
	meta := aggregateMetadata(id, projection)

	if q.Start.IsZero() && q.End.IsZero() {
		meta.Start = projection.Now.Add(-projection.Horizon)
		meta.End = projection.Now
		snap := fromAggregateProjection(projection)
		snap.Meta = meta
		filterSnapshot(&snap, q.Services)
		finishSnapshot(&snap, nil)
		return snap, nil
	}
	meta.Start, meta.End = q.Start, q.End

	result, err := p.engine.QueryTopology(aggregate.Query{
		Tenant:   tenant,
		Start:    q.Start,
		End:      q.End,
		Services: append([]string(nil), q.Services...),
	})
	if err != nil {
		return Snapshot{}, err
	}
	meta.Epoch = result.Epoch
	meta.Revision = result.Revision
	if result.Coverage != "" && !meta.Truncated {
		meta.Coverage = string(result.Coverage)
		meta.CoverageNote = result.Coverage.Note()
	}
	snap := Snapshot{
		Nodes: make([]Node, 0, len(result.Nodes)),
		Edges: make([]Edge, 0, len(result.Edges)),
		Meta:  meta,
	}
	for _, node := range result.Nodes {
		errorRate := node.ErrorRate
		health := aggregateHealth(errorRate, node.AvgLatencyMs)
		snap.Nodes = append(snap.Nodes, Node{
			Name:              node.Service,
			TotalTraces:       node.Count,
			ErrorCount:        node.ErrorCount,
			AvgLatencyMs:      node.AvgLatencyMs,
			P99LatencyMs:      node.P99LatencyMicros / 1000.0,
			LatencyProvenance: &node.LatencyProvenance,
			ErrorRate:         errorRate,
			SpanCount:         node.Count,
			HealthScore:       health,
			Status:            healthStatus(health),
			Alerts:            alerts(errorRate, node.AvgLatencyMs),
		})
	}
	for _, edge := range result.Edges {
		snap.Edges = append(snap.Edges, Edge{
			Source:       edge.Source,
			Target:       edge.Target,
			CallCount:    edge.CallCount,
			AvgLatencyMs: edge.AvgLatencyMs,
			ErrorRate:    edge.ErrorRate,
			Status:       healthStatus(aggregateHealth(edge.ErrorRate, edge.AvgLatencyMs)),
		})
	}
	finishSnapshot(&snap, nil)
	return snap, nil
}

func aggregateMetadata(id Identity, projection aggregate.TopologySnapshot) Metadata {
	coverage := aggregate.CoverageFull
	coverageNote := coverage.Note()
	if projection.Truncated() {
		coverage = aggregate.CoverageSampled
		coverageNote = "topology is incomplete because configured projection caps refused accepted facts; dropped_* counts identify the omissions"
	}
	return Metadata{
		Source:            SourceAggregate,
		Coverage:          string(coverage),
		CoverageNote:      coverageNote,
		Epoch:             id.Epoch,
		Revision:          id.Revision,
		Truncated:         projection.Truncated(),
		DroppedServices:   projection.DroppedServices,
		DroppedOperations: projection.DroppedOperations,
		DroppedEdges:      projection.DroppedEdges,
		DroppedMetrics:    projection.DroppedMetrics,
	}
}

func fromAggregateProjection(projection aggregate.TopologySnapshot) Snapshot {
	snap := Snapshot{
		Nodes: make([]Node, 0, len(projection.Services)),
		Edges: make([]Edge, 0, len(projection.Edges)),
	}
	for _, service := range projection.Services {
		totals := sumWindows(service.Windows)
		errorRate := totals.errorRate()
		avg := totals.avgLatencyMs()
		health := aggregateHealth(errorRate, avg)
		provenance := totals.latencyProvenance
		if provenance == nil {
			provenance = &latency.Provenance{P99: &latency.Percentile{
				Status: latency.StatusUnavailable,
				Method: latency.MethodDDSketch,
				Reason: latency.ReasonNoObservations,
			}}
		}
		snap.Nodes = append(snap.Nodes, Node{
			Name:              service.Name,
			TotalTraces:       saturatingInt64(totals.count),
			ErrorCount:        saturatingInt64(totals.errors),
			AvgLatencyMs:      avg,
			RequestRateRPS:    float64(totals.count) / 300,
			ErrorRate:         errorRate,
			P99LatencyMs:      totals.p99Ms,
			LatencyProvenance: provenance,
			SpanCount:         saturatingInt64(totals.count),
			HealthScore:       health,
			Status:            healthStatus(health),
			Alerts:            alerts(errorRate, avg),
		})
	}
	for _, edge := range projection.Edges {
		totals := sumWindows(edge.Windows)
		errorRate := totals.errorRate()
		avg := totals.avgLatencyMs()
		snap.Edges = append(snap.Edges, Edge{
			Source:       edge.Caller,
			Target:       edge.Callee,
			CallCount:    saturatingInt64(totals.count),
			AvgLatencyMs: avg,
			ErrorRate:    errorRate,
			Status:       healthStatus(aggregateHealth(errorRate, avg)),
		})
	}
	return snap
}

type windowTotals struct {
	count, errors, durationCount uint64
	durationSumMicros, p99Ms     float64
	latencyProvenance            *latency.Provenance
}

func sumWindows(windows []aggregate.TopologyWindow) windowTotals {
	var totals windowTotals
	for _, window := range windows {
		totals.count += window.Count
		totals.errors += window.ErrorCount
		totals.durationCount += window.DurationCount
		totals.durationSumMicros += window.DurationSumMicros
		if window.LatencyProvenance != nil && window.LatencyProvenance.P99 != nil {
			totals.p99Ms = window.P99Micros / 1000
			provenance := *window.LatencyProvenance
			totals.latencyProvenance = &provenance
		}
	}
	return totals
}

func (t windowTotals) errorRate() float64 {
	if t.count == 0 {
		return 0
	}
	return float64(t.errors) / float64(t.count)
}

func (t windowTotals) avgLatencyMs() float64 {
	if t.durationCount == 0 {
		return 0
	}
	return t.durationSumMicros / float64(t.durationCount) / 1000
}

func aggregateHealth(errorRate, avgLatencyMs float64) float64 {
	latencyDeviation := math.Max(0, (avgLatencyMs-100)/100)
	return math.Max(0, math.Min(1, 1-errorRate*5-latencyDeviation*0.1))
}

func saturatingInt64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

// The following methods implement graphrag.AggregateSource. Keeping that
// projection narrow avoids teaching GraphRAG about range queries.
func (p *AggregateProvider) TopologyEpoch() uint64 { return p.engine.TopologyEpoch() }

func (p *AggregateProvider) TopologyTenants() []string { return p.engine.TopologyTenants() }

func (p *AggregateProvider) TopologyRevision(tenant string) uint64 {
	return p.engine.TopologyRevision(tenant)
}

func (p *AggregateProvider) TopologySnapshot(tenant string) aggregate.TopologySnapshot {
	return p.engine.TopologySnapshot(tenant)
}

func (p *AggregateProvider) PruneTopology() { p.engine.PruneTopology() }

var _ Provider = (*AggregateProvider)(nil)
