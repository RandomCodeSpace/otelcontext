package topology

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graph"
	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

type legacyRepository interface {
	GetServiceMapMetrics(context.Context, time.Time, time.Time) (*storage.ServiceMapMetrics, error)
}

// LegacyProvider preserves the existing historical repository query and the
// current GraphRAG/graph/DB preference for live topology.
type LegacyProvider struct {
	repo     legacyRepository
	graph    *graph.Graph
	graphRAG *graphrag.GraphRAG
	now      func() time.Time
}

func NewLegacyProvider(repo legacyRepository, serviceGraph *graph.Graph, graphRAG *graphrag.GraphRAG) (*LegacyProvider, error) {
	if repo == nil {
		return nil, errors.New("legacy topology provider requires a repository")
	}
	return &LegacyProvider{repo: repo, graph: serviceGraph, graphRAG: graphRAG, now: time.Now}, nil
}

func (*LegacyProvider) Source() Source { return SourceLegacy }

func (*LegacyProvider) Identity(context.Context) Identity { return Identity{} }

func (p *LegacyProvider) Snapshot(ctx context.Context, q Query) (Snapshot, error) {
	if !q.Start.IsZero() || !q.End.IsZero() {
		return p.rangeSnapshot(ctx, q)
	}
	return p.liveSnapshot(ctx, q.Services)
}

func (p *LegacyProvider) rangeSnapshot(ctx context.Context, q Query) (Snapshot, error) {
	metrics, err := p.repo.GetServiceMapMetrics(ctx, q.Start, q.End)
	if err != nil {
		return Snapshot{}, err
	}
	snap := fromStorage(metrics)
	snap.Meta = Metadata{Source: SourceLegacy, Start: q.Start, End: q.End}
	filterSnapshot(&snap, q.Services)
	return snap, nil
}

func (p *LegacyProvider) liveSnapshot(ctx context.Context, services []string) (Snapshot, error) {
	now := p.now().UTC()
	if p.graphRAG != nil {
		entries := p.graphRAG.ServiceMap(ctx, 0)
		if len(entries) > 0 {
			snap := Snapshot{
				Nodes: make([]Node, 0, len(entries)),
				Edges: []Edge{},
				Meta:  Metadata{Source: SourceLegacy, Start: now.Add(-5 * time.Minute), End: now},
			}
			for _, entry := range entries {
				if entry.Service == nil {
					continue
				}
				svc := entry.Service
				snap.Nodes = append(snap.Nodes, Node{
					Name:           svc.Name,
					TotalTraces:    svc.CallCount,
					ErrorCount:     svc.ErrorCount,
					AvgLatencyMs:   svc.AvgLatency,
					RequestRateRPS: float64(svc.CallCount) / 300,
					ErrorRate:      svc.ErrorRate,
					P99LatencyMs:   svc.AvgLatency * 2.5,
					SpanCount:      svc.CallCount,
					HealthScore:    svc.HealthScore,
					Status:         healthStatus(svc.HealthScore),
					Alerts:         alerts(svc.ErrorRate, svc.AvgLatency),
				})
			}
			for _, edge := range p.graphRAG.AllServiceEdges(ctx) {
				if edge == nil || edge.Type != graphrag.EdgeCalls {
					continue
				}
				snap.Edges = append(snap.Edges, Edge{
					Source:       edge.FromID,
					Target:       edge.ToID,
					CallCount:    edge.CallCount,
					AvgLatencyMs: edge.AvgMs,
					ErrorRate:    edge.ErrorRate,
					Status:       healthStatus(legacyHealth(edge.ErrorRate, edge.AvgMs)),
				})
			}
			finishSnapshot(&snap, services)
			return snap, nil
		}
	}

	if p.graph != nil {
		current := p.graph.Snapshot()
		if current != nil && !current.UpdatedAt.IsZero() && len(current.Nodes) > 0 {
			snap := Snapshot{
				Nodes: make([]Node, 0, len(current.Nodes)),
				Edges: make([]Edge, 0, len(current.Edges)),
				Meta:  Metadata{Source: SourceLegacy, Start: current.UpdatedAt.Add(-5 * time.Minute), End: current.UpdatedAt},
			}
			for _, node := range current.Nodes {
				if node == nil {
					continue
				}
				snap.Nodes = append(snap.Nodes, Node{
					Name:           node.Name,
					TotalTraces:    node.SpanCount,
					AvgLatencyMs:   node.AvgLatencyMs,
					RequestRateRPS: node.RequestRateRPS,
					ErrorRate:      node.ErrorRate,
					P99LatencyMs:   node.P99LatencyMs,
					SpanCount:      node.SpanCount,
					HealthScore:    node.HealthScore,
					Status:         node.Status,
					Alerts:         append([]string(nil), node.Alerts...),
				})
			}
			for _, edge := range current.Edges {
				snap.Edges = append(snap.Edges, Edge{
					Source:       edge.Source,
					Target:       edge.Target,
					CallCount:    edge.CallCount,
					AvgLatencyMs: edge.AvgLatencyMs,
					ErrorRate:    edge.ErrorRate,
					Status:       edge.Status,
				})
			}
			finishSnapshot(&snap, services)
			return snap, nil
		}
	}

	return p.rangeSnapshot(ctx, Query{Start: now.Add(-time.Hour), End: now, Services: services})
}

func fromStorage(metrics *storage.ServiceMapMetrics) Snapshot {
	snap := Snapshot{Nodes: []Node{}, Edges: []Edge{}}
	if metrics == nil {
		return snap
	}
	snap.Nodes = make([]Node, 0, len(metrics.Nodes))
	for _, node := range metrics.Nodes {
		errorRate := 0.0
		if node.TotalTraces > 0 {
			errorRate = float64(node.ErrorCount) / float64(node.TotalTraces)
		}
		health := legacyHealth(errorRate, node.AvgLatencyMs)
		snap.Nodes = append(snap.Nodes, Node{
			Name:           node.Name,
			TotalTraces:    node.TotalTraces,
			ErrorCount:     node.ErrorCount,
			AvgLatencyMs:   node.AvgLatencyMs,
			RequestRateRPS: float64(node.TotalTraces) / 3600,
			ErrorRate:      errorRate,
			P99LatencyMs:   node.AvgLatencyMs * 2.5,
			SpanCount:      node.TotalTraces,
			HealthScore:    health,
			Status:         healthStatus(health),
			Alerts:         alerts(errorRate, node.AvgLatencyMs),
		})
	}
	snap.Edges = make([]Edge, 0, len(metrics.Edges))
	for _, edge := range metrics.Edges {
		snap.Edges = append(snap.Edges, Edge{
			Source:       edge.Source,
			Target:       edge.Target,
			CallCount:    edge.CallCount,
			AvgLatencyMs: edge.AvgLatencyMs,
			ErrorRate:    edge.ErrorRate,
			Status:       healthStatus(legacyHealth(edge.ErrorRate, edge.AvgLatencyMs)),
		})
	}
	finishSnapshot(&snap, nil)
	return snap
}

func finishSnapshot(snap *Snapshot, services []string) {
	filterSnapshot(snap, services)
	sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].Name < snap.Nodes[j].Name })
	sort.Slice(snap.Edges, func(i, j int) bool {
		if snap.Edges[i].Source == snap.Edges[j].Source {
			return snap.Edges[i].Target < snap.Edges[j].Target
		}
		return snap.Edges[i].Source < snap.Edges[j].Source
	})
	if snap.Nodes == nil {
		snap.Nodes = []Node{}
	}
	if snap.Edges == nil {
		snap.Edges = []Edge{}
	}
}

func filterSnapshot(snap *Snapshot, services []string) {
	if len(services) == 0 {
		return
	}
	keep := make(map[string]struct{}, len(services))
	for _, service := range services {
		keep[service] = struct{}{}
	}
	nodes := snap.Nodes[:0]
	for _, node := range snap.Nodes {
		if _, ok := keep[node.Name]; ok {
			nodes = append(nodes, node)
		}
	}
	edges := snap.Edges[:0]
	for _, edge := range snap.Edges {
		_, sourceOK := keep[edge.Source]
		_, targetOK := keep[edge.Target]
		if sourceOK && targetOK {
			edges = append(edges, edge)
		}
	}
	snap.Nodes, snap.Edges = nodes, edges
}

func legacyHealth(errorRate, avgLatencyMs float64) float64 {
	score := 1 - errorRate*5
	if avgLatencyMs > 200 {
		score -= (avgLatencyMs - 200) / 2000
	}
	return math.Max(0, math.Min(1, score))
}

func healthStatus(score float64) string {
	switch {
	case score >= 0.9:
		return "healthy"
	case score >= 0.7:
		return "degraded"
	default:
		return "critical"
	}
}

func alerts(errorRate, avgLatencyMs float64) []string {
	out := []string{}
	if errorRate > 0.05 {
		out = append(out, "error rate above 5%")
	}
	if errorRate > 0.10 {
		out = append(out, "error rate above 10% — investigate immediately")
	}
	if avgLatencyMs > 500 {
		out = append(out, "avg latency above 500ms")
	}
	if avgLatencyMs > 1000 {
		out = append(out, "avg latency above 1s — SLA breach risk")
	}
	return out
}
