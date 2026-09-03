package realtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

// EnginePublisher is the AggregatePublisher backed by the aggregate engine.
//
// The payload it builds is deliberately the COALESCED one #164 froze: summary,
// recent traffic, service health and topology over a short trailing window. The
// seven-day history is never in a WebSocket message — a client that wants it
// asks for it over HTTP once, not on every revision bump.
type EnginePublisher struct {
	engine   *aggregate.Engine
	topology topology.Provider
	// window is how far back the coalesced payload reaches.
	window time.Duration
	// tenant scopes the queries when the caller's context carries none. Since
	// the handshake gate pins one tenant per socket, ctx normally decides; this
	// stays the fallback for an unauthenticated (development) deployment.
	tenant string
}

// EnginePublisherConfig configures an EnginePublisher.
type EnginePublisherConfig struct {
	// Engine is the aggregate query facade. Required.
	Engine *aggregate.Engine
	// Topology is the same mode-selected provider injected into REST, MCP and
	// GraphRAG. It is required and must be aggregate-owned.
	Topology topology.Provider
	// Window is the trailing range of the coalesced payload. Zero takes 15
	// minutes, matching the legacy snapshot window.
	Window time.Duration
	// Tenant scopes every query. Empty takes storage.DefaultTenantID.
	Tenant string
	// Edges is IGNORED since #194 finding 15: topology edges come from the
	// engine's own service-edge series, read in the same query as the nodes.
	// The field remains so existing wiring compiles.
	//
	// Deprecated: supply nothing; QueryTopology carries the edges.
	Edges func(ctx context.Context) []storage.ServiceMapEdge
}

// NewEnginePublisher builds a publisher over the engine. It returns nil when no
// engine is configured, which is what keeps the caller's wiring a one-liner.
func NewEnginePublisher(cfg EnginePublisherConfig) *EnginePublisher {
	if cfg.Engine == nil || cfg.Topology == nil || cfg.Topology.Source() != topology.SourceAggregate {
		return nil
	}
	if cfg.Window <= 0 {
		cfg.Window = 15 * time.Minute
	}
	if cfg.Tenant == "" {
		cfg.Tenant = storage.DefaultTenantID
	}
	return &EnginePublisher{
		engine:   cfg.Engine,
		topology: cfg.Topology,
		window:   cfg.Window,
		tenant:   cfg.Tenant,
	}
}

// Epoch implements AggregatePublisher.
func (p *EnginePublisher) Epoch() string { return p.topology.Identity(context.Background()).Epoch }

// Revision implements AggregatePublisher.
func (p *EnginePublisher) Revision() uint64 {
	return p.topology.Identity(context.Background()).Revision
}

// Snapshot implements AggregatePublisher.
func (p *EnginePublisher) Snapshot(ctx context.Context, service string) *LiveSnapshot {
	now := time.Now()
	start := now.Add(-p.window)

	var services []string
	if service != "" {
		services = []string{service}
	}
	// The socket's authenticated tenant travels on ctx and outranks the
	// configured fallback — otherwise every tenant's dashboard would be served
	// the same tenant's numbers.
	tenant := p.tenant
	if storage.HasTenantContext(ctx) {
		tenant = storage.TenantFromContext(ctx)
	} else {
		ctx = storage.WithTenantContext(ctx, tenant)
	}
	q := aggregate.Query{Tenant: tenant, Start: start, End: now, Services: services}
	topologySnapshot, err := p.topology.Snapshot(ctx, topology.Query{Start: start, End: now, Services: services})
	if err != nil {
		slog.Debug("aggregate snapshot: topology provider failed", "error", err)
		return nil
	}

	snap := &LiveSnapshot{
		Type:              "live_snapshot",
		Epoch:             topologySnapshot.Meta.Epoch,
		Revision:          topologySnapshot.Meta.Revision,
		Source:            string(topologySnapshot.Meta.Source),
		Coverage:          topologySnapshot.Meta.Coverage,
		CoverageNote:      topologySnapshot.Meta.CoverageNote,
		Truncated:         topologySnapshot.Meta.Truncated,
		DroppedServices:   topologySnapshot.Meta.DroppedServices,
		DroppedOperations: topologySnapshot.Meta.DroppedOperations,
		DroppedEdges:      topologySnapshot.Meta.DroppedEdges,
		DroppedMetrics:    topologySnapshot.Meta.DroppedMetrics,
	}

	// Nodes and edges are both engine-sourced and read over the same range
	// (#194 finding 15), so the payload carries the engine's own coverage
	// instead of the blanket "sampled" the exemplar-fed edge side-channel
	// forced on it.
	if snap.Coverage == "" {
		coverage := aggregate.CoverageFull
		snap.Coverage = string(coverage)
		snap.CoverageNote = coverage.Note()
	}

	if dash, err := p.engine.QueryDashboard(q); err == nil {
		provenance := dash.LatencyProvenance
		snap.Dashboard = &storage.DashboardStats{
			// Headline trio on the REQUEST basis, restated by name alongside
			// the span basis — same contract as the HTTP dashboard view.
			TotalTraces:       dash.RequestCount,
			TotalErrors:       dash.ErrorRequestCount,
			ErrorRate:         dash.RequestErrorRate,
			Requests:          dash.RequestCount,
			RequestErrors:     dash.ErrorRequestCount,
			RequestErrorRate:  dash.RequestErrorRate,
			Spans:             dash.SpanCount,
			SpanErrors:        dash.SpanErrorCount,
			SpanErrorRate:     dash.SpanErrorRate,
			TotalLogs:         dash.TotalLogs,
			AvgLatencyMs:      dash.AvgLatencyMs,
			ActiveServices:    dash.ActiveServices,
			P99Latency:        int64(dash.P99LatencyMicros),
			LatencyProvenance: &provenance,
		}
		for _, s := range dash.TopFailing {
			snap.Dashboard.TopFailingServices = append(snap.Dashboard.TopFailingServices, storage.ServiceError{
				ServiceName: s.Service,
				ErrorCount:  s.ErrorCount,
				TotalCount:  s.Count,
				ErrorRate:   s.ErrorRate,
			})
		}
	} else {
		slog.Debug("aggregate snapshot: dashboard query failed", "error", err)
	}

	if traffic, err := p.engine.QueryBuckets(q); err == nil {
		points := make([]storage.TrafficPoint, 0, len(traffic.Points))
		for _, pt := range traffic.Points {
			points = append(points, trafficPointFromAggregate(pt))
		}
		snap.Traffic = points
	} else {
		slog.Debug("aggregate snapshot: traffic query failed", "error", err)
	}

	{
		sm := &storage.ServiceMapMetrics{
			Nodes: make([]storage.ServiceMapNode, 0, len(topologySnapshot.Nodes)),
			Edges: make([]storage.ServiceMapEdge, 0, len(topologySnapshot.Edges)),
		}
		for _, n := range topologySnapshot.Nodes {
			sm.Nodes = append(sm.Nodes, storage.ServiceMapNode{
				Name:              n.Name,
				TotalTraces:       n.TotalTraces,
				ErrorCount:        n.ErrorCount,
				AvgLatencyMs:      n.AvgLatencyMs,
				P99LatencyMs:      n.P99LatencyMs,
				LatencyProvenance: n.LatencyProvenance,
				Kind:              n.Kind,
				HostCount:         n.HostCount,
				Hosts:             n.Hosts,
			})
		}
		for _, e := range topologySnapshot.Edges {
			sm.Edges = append(sm.Edges, storage.ServiceMapEdge{
				Source:       e.Source,
				Target:       e.Target,
				CallCount:    e.CallCount,
				AvgLatencyMs: e.AvgLatencyMs,
				ErrorRate:    e.ErrorRate,
			})
		}
		snap.ServiceMap = sm
	}

	// Traces stay absent: individual traces are exemplars in aggregate mode,
	// and shipping a handful of them inside a snapshot would read as "these
	// are the traces that happened". They are fetched explicitly instead.
	return snap
}

// compile-time assertion.
var _ AggregatePublisher = (*EnginePublisher)(nil)

// trafficPointFromAggregate converts one engine traffic bucket into the wire
// shape. count/error_count carry the REQUEST basis; both bases are also
// restated by name (#197 Q3).
func trafficPointFromAggregate(pt aggregate.TrafficPoint) storage.TrafficPoint {
	return storage.TrafficPoint{
		Timestamp:     pt.WindowStart,
		Count:         pt.RequestCount,
		ErrorCount:    pt.ErrorRequestCount,
		Requests:      pt.RequestCount,
		RequestErrors: pt.ErrorRequestCount,
		Spans:         pt.SpanCount,
		SpanErrors:    pt.SpanErrorCount,
	}
}
