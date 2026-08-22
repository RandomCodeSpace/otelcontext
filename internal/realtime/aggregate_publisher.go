package realtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// EnginePublisher is the AggregatePublisher backed by the aggregate engine.
//
// The payload it builds is deliberately the COALESCED one #164 froze: summary,
// recent traffic, service health and topology over a short trailing window. The
// seven-day history is never in a WebSocket message — a client that wants it
// asks for it over HTTP once, not on every revision bump.
type EnginePublisher struct {
	engine *aggregate.Engine
	// window is how far back the coalesced payload reaches.
	window time.Duration
	// edges supplies caller/callee topology, which is not aggregate data.
	// nil is allowed and yields a node-only topology.
	edges func(ctx context.Context) []storage.ServiceMapEdge
	// tenant scopes the queries. The WebSocket protocol carries no tenant, so
	// this is the default tenant, exactly as the legacy snapshot path assumed.
	tenant string
}

// EnginePublisherConfig configures an EnginePublisher.
type EnginePublisherConfig struct {
	// Engine is the aggregate query facade. Required.
	Engine *aggregate.Engine
	// Window is the trailing range of the coalesced payload. Zero takes 15
	// minutes, matching the legacy snapshot window.
	Window time.Duration
	// Tenant scopes every query. Empty takes storage.DefaultTenantID.
	Tenant string
	// Edges supplies topology edges. Optional.
	Edges func(ctx context.Context) []storage.ServiceMapEdge
}

// NewEnginePublisher builds a publisher over the engine. It returns nil when no
// engine is configured, which is what keeps the caller's wiring a one-liner.
func NewEnginePublisher(cfg EnginePublisherConfig) *EnginePublisher {
	if cfg.Engine == nil {
		return nil
	}
	if cfg.Window <= 0 {
		cfg.Window = 15 * time.Minute
	}
	if cfg.Tenant == "" {
		cfg.Tenant = storage.DefaultTenantID
	}
	return &EnginePublisher{
		engine: cfg.Engine,
		window: cfg.Window,
		edges:  cfg.Edges,
		tenant: cfg.Tenant,
	}
}

// Epoch implements AggregatePublisher.
func (p *EnginePublisher) Epoch() string { return p.engine.Epoch() }

// Revision implements AggregatePublisher.
func (p *EnginePublisher) Revision() uint64 { return p.engine.Revision() }

// Snapshot implements AggregatePublisher.
func (p *EnginePublisher) Snapshot(ctx context.Context, service string) *LiveSnapshot {
	now := time.Now()
	start := now.Add(-p.window)

	var services []string
	if service != "" {
		services = []string{service}
	}
	q := aggregate.Query{Tenant: p.tenant, Start: start, End: now, Services: services}

	snap := &LiveSnapshot{
		Type:     "live_snapshot",
		Epoch:    p.engine.Epoch(),
		Revision: p.engine.Revision(),
	}

	// Topology edges are exemplar-fed, so the coalesced payload as a whole is
	// "sampled": the counts are exact, the edges are not.
	coverage := aggregate.CoverageSampled
	snap.Coverage = string(coverage)
	snap.CoverageNote = coverage.Note()

	if dash, err := p.engine.QueryDashboard(q); err == nil {
		snap.Dashboard = &storage.DashboardStats{
			// Headline trio on the REQUEST basis, restated by name alongside
			// the span basis — same contract as the HTTP dashboard view.
			TotalTraces:      dash.RequestCount,
			TotalErrors:      dash.ErrorRequestCount,
			ErrorRate:        dash.RequestErrorRate,
			Requests:         dash.RequestCount,
			RequestErrors:    dash.ErrorRequestCount,
			RequestErrorRate: dash.RequestErrorRate,
			Spans:            dash.SpanCount,
			SpanErrors:       dash.SpanErrorCount,
			SpanErrorRate:    dash.SpanErrorRate,
			TotalLogs:        dash.TotalLogs,
			AvgLatencyMs:     dash.AvgLatencyMs,
			ActiveServices:   dash.ActiveServices,
			P99Latency:       int64(dash.P99LatencyMicros),
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

	if topo, err := p.engine.QueryTopology(q); err == nil {
		sm := &storage.ServiceMapMetrics{
			Nodes: make([]storage.ServiceMapNode, 0, len(topo.Nodes)),
			Edges: []storage.ServiceMapEdge{},
		}
		for _, n := range topo.Nodes {
			sm.Nodes = append(sm.Nodes, storage.ServiceMapNode{
				Name:         n.Service,
				TotalTraces:  n.Count,
				ErrorCount:   n.ErrorCount,
				AvgLatencyMs: n.AvgLatencyMs,
			})
		}
		if p.edges != nil {
			if e := p.edges(ctx); len(e) > 0 {
				sm.Edges = e
			}
		}
		snap.ServiceMap = sm
	} else {
		slog.Debug("aggregate snapshot: topology query failed", "error", err)
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
