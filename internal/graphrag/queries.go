package graphrag

import (
	"context"
	"math"
	"sort"
	"time"
)

// ErrorChain traces error spans upstream to find the root cause service.
// The tenant slice is selected via ctx — callers without a tenant ctx
// collapse to storage.DefaultTenantID at the coordinator boundary.
func (g *GraphRAG) ErrorChain(ctx context.Context, service string, since time.Time, limit int) []ErrorChainResult {
	if limit <= 0 {
		limit = 10
	}

	stores := g.storesFor(ctx)

	errorSpans := stores.traces.ErrorSpans(service, since)
	if len(errorSpans) > limit {
		errorSpans = errorSpans[:limit]
	}

	var results []ErrorChainResult
	seen := make(map[string]bool)

	for _, span := range errorSpans {
		if seen[span.TraceID] {
			continue
		}
		seen[span.TraceID] = true

		chain := traceErrorChainUpstream(stores, span)
		if len(chain) == 0 {
			continue
		}

		rootSpan := chain[len(chain)-1]
		result := ErrorChainResult{
			RootCause: &RootCauseInfo{
				Service:   rootSpan.Service,
				Operation: rootSpan.Operation,
				SpanID:    rootSpan.ID,
				TraceID:   rootSpan.TraceID,
			},
			SpanChain: chain,
			TraceID:   span.TraceID,
		}

		// Gather correlated logs
		for _, s := range chain {
			if s.IsError {
				clusters := stores.signals.LogClustersForService(s.Service)
				for _, lc := range clusters {
					if lc.LastSeen.After(since) {
						result.CorrelatedLogs = append(result.CorrelatedLogs, *lc)
					}
				}
			}
		}

		results = append(results, result)
	}

	return results
}

// traceErrorChainUpstream walks CHILD_OF edges upstream from an error span to
// the root within a single tenant's TraceStore.
func traceErrorChainUpstream(stores *tenantStores, span *SpanNode) []SpanNode {
	var chain []SpanNode
	visited := make(map[string]bool)
	current := span

	for current != nil && !visited[current.ID] {
		visited[current.ID] = true
		chain = append(chain, *current)

		if current.ParentSpanID == "" {
			break
		}
		parent, ok := stores.traces.GetSpan(current.ParentSpanID)
		if !ok {
			break
		}
		current = parent
	}

	return chain
}

// ImpactAnalysis performs BFS downstream from a service to find affected services.
func (g *GraphRAG) ImpactAnalysis(ctx context.Context, service string, maxDepth int) *ImpactResult {
	if maxDepth <= 0 {
		maxDepth = 5
	}

	stores := g.storesFor(ctx)

	result := &ImpactResult{Service: service}
	visited := map[string]bool{service: true}

	type queueItem struct {
		svc   string
		depth int
	}
	queue := []queueItem{{service, 0}}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		if item.depth >= maxDepth {
			continue
		}

		edges := stores.service.CallEdgesFrom(item.svc)
		for _, e := range edges {
			if visited[e.ToID] {
				continue
			}
			visited[e.ToID] = true

			svc, _ := stores.service.GetService(e.ToID)
			impact := 1.0
			if svc != nil {
				impact = 1.0 - svc.HealthScore
			}

			result.AffectedServices = append(result.AffectedServices, AffectedEntry{
				Service:     e.ToID,
				Depth:       item.depth + 1,
				CallCount:   e.CallCount,
				ImpactScore: impact,
			})

			queue = append(queue, queueItem{e.ToID, item.depth + 1})
		}
	}

	result.TotalDownstream = len(result.AffectedServices)
	return result
}

// RootCauseAnalysis combines ErrorChain with anomaly correlation to rank probable causes.
func (g *GraphRAG) RootCauseAnalysis(ctx context.Context, service string, since time.Time) []RankedCause {
	stores := g.storesFor(ctx)
	errorChains := g.ErrorChain(ctx, service, since, 20)
	anomalies := stores.anomalies.AnomaliesForService(service, since)

	// Score services by how often they appear as root cause
	causeScores := make(map[string]*RankedCause)

	for _, ec := range errorChains {
		if ec.RootCause == nil {
			continue
		}
		key := ec.RootCause.Service + "|" + ec.RootCause.Operation
		rc, ok := causeScores[key]
		if !ok {
			rc = &RankedCause{
				Service:   ec.RootCause.Service,
				Operation: ec.RootCause.Operation,
			}
			causeScores[key] = rc
		}
		rc.Score += 1.0
		rc.Evidence = append(rc.Evidence, "error chain from trace "+ec.TraceID)
		if len(ec.SpanChain) > 0 {
			rc.ErrorChain = ec.SpanChain
		}
	}

	// Boost score with anomalies
	for _, a := range anomalies {
		key := a.Service + "|"
		for k, rc := range causeScores {
			if len(k) >= len(a.Service) && k[:len(a.Service)] == a.Service {
				rc.Score += 2.0
				rc.Anomalies = append(rc.Anomalies, *a)
				rc.Evidence = append(rc.Evidence, "anomaly: "+a.Evidence)
			}
		}
		// Add service itself if not seen
		if _, ok := causeScores[key]; !ok {
			causeScores[key] = &RankedCause{
				Service:   a.Service,
				Score:     2.0,
				Anomalies: []AnomalyNode{*a},
				Evidence:  []string{"anomaly: " + a.Evidence},
			}
		}
	}

	// Sort by score descending
	ranked := make([]RankedCause, 0, len(causeScores))
	for _, rc := range causeScores {
		ranked = append(ranked, *rc)
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].Score > ranked[j].Score
	})

	return ranked
}

// DependencyChain returns the full span tree for a trace.
func (g *GraphRAG) DependencyChain(ctx context.Context, traceID string) []SpanNode {
	stores := g.storesFor(ctx)
	spans := stores.traces.SpansForTrace(traceID)
	out := make([]SpanNode, len(spans))
	for i, s := range spans {
		out[i] = *s
	}
	// Sort by timestamp
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out
}

// CorrelatedSignals gathers all related signals for a service within a time range.
type CorrelatedSignalsResult struct {
	Service     string             `json:"service"`
	ErrorLogs   []LogClusterNode   `json:"error_logs"`
	Metrics     []MetricNode       `json:"metrics"`
	Anomalies   []AnomalyNode      `json:"anomalies"`
	ErrorChains []ErrorChainResult `json:"error_chains,omitempty"`
}

func (g *GraphRAG) CorrelatedSignals(ctx context.Context, service string, since time.Time) *CorrelatedSignalsResult {
	stores := g.storesFor(ctx)
	result := &CorrelatedSignalsResult{Service: service}

	// Error logs
	clusters := stores.signals.LogClustersForService(service)
	for _, lc := range clusters {
		if lc.LastSeen.After(since) {
			result.ErrorLogs = append(result.ErrorLogs, *lc)
		}
	}

	// Metrics
	metrics := stores.signals.MetricsForService(service)
	for _, m := range metrics {
		result.Metrics = append(result.Metrics, *m)
	}

	// Anomalies
	anomalies := stores.anomalies.AnomaliesForService(service, since)
	for _, a := range anomalies {
		result.Anomalies = append(result.Anomalies, *a)
	}

	// Recent error chains
	result.ErrorChains = g.ErrorChain(ctx, service, since, 5)

	return result
}

// ShortestPath finds the shortest path between two services using Dijkstra.
func (g *GraphRAG) ShortestPath(ctx context.Context, from, to string) []string {
	stores := g.storesFor(ctx)
	// Build adjacency from CALLS edges
	stores.service.mu.RLock()
	adj := make(map[string]map[string]float64)
	for _, e := range stores.service.Edges {
		if e.Type != EdgeCalls {
			continue
		}
		if adj[e.FromID] == nil {
			adj[e.FromID] = make(map[string]float64)
		}
		weight := 1.0
		if e.CallCount > 0 {
			weight = 1.0 / float64(e.CallCount) // inverse call frequency
		}
		adj[e.FromID][e.ToID] = weight
		// Also add reverse for bidirectional search
		if adj[e.ToID] == nil {
			adj[e.ToID] = make(map[string]float64)
		}
		adj[e.ToID][e.FromID] = weight
	}
	stores.service.mu.RUnlock()

	// Dijkstra
	dist := map[string]float64{from: 0}
	prev := map[string]string{}
	visited := map[string]bool{}

	for {
		// Find unvisited node with minimum distance
		var u string
		minDist := math.MaxFloat64
		for node, d := range dist {
			if !visited[node] && d < minDist {
				u = node
				minDist = d
			}
		}
		if u == "" || u == to {
			break
		}
		visited[u] = true

		for neighbor, weight := range adj[u] {
			alt := dist[u] + weight
			if d, ok := dist[neighbor]; !ok || alt < d {
				dist[neighbor] = alt
				prev[neighbor] = u
			}
		}
	}

	// Reconstruct path
	if _, ok := dist[to]; !ok {
		return nil
	}
	var path []string
	for at := to; at != ""; at = prev[at] {
		path = append([]string{at}, path...)
		if at == from {
			break
		}
	}
	if len(path) > 0 && path[0] != from {
		return nil // no path
	}
	return path
}

// AnomalyTimeline returns recent anomalies sorted by time.
func (g *GraphRAG) AnomalyTimeline(ctx context.Context, since time.Time) []*AnomalyNode {
	stores := g.storesFor(ctx)
	anomalies := stores.anomalies.AnomaliesSince(since)
	sort.Slice(anomalies, func(i, j int) bool {
		return anomalies[i].Timestamp.After(anomalies[j].Timestamp)
	})
	return anomalies
}

// AnomaliesForService is a tenant-aware read-through to the per-tenant
// AnomalyStore, exported so handlers outside this package never need to
// reach into the store maps directly.
func (g *GraphRAG) AnomaliesForService(ctx context.Context, service string, since time.Time) []*AnomalyNode {
	return g.storesFor(ctx).anomalies.AnomaliesForService(service, since)
}

// AllServiceEdges returns every edge in the caller's tenant's ServiceStore.
// Kept as a narrow helper so API handlers do not need to traverse the
// tenantStores composite themselves.
func (g *GraphRAG) AllServiceEdges(ctx context.Context) []*Edge {
	return g.storesFor(ctx).service.AllEdges()
}

// ServiceMap returns the service topology with health scores for the API.
type ServiceMapEntry struct {
	Service    *ServiceNode     `json:"service"`
	Operations []*OperationNode `json:"operations,omitempty"`
	CallsTo    []*Edge          `json:"calls_to,omitempty"`
	CalledBy   []*Edge          `json:"called_by,omitempty"`
}

func (g *GraphRAG) ServiceMap(ctx context.Context, depth int) []ServiceMapEntry {
	stores := g.storesFor(ctx)
	services := stores.service.AllServices()
	result := make([]ServiceMapEntry, 0, len(services))

	for _, svc := range services {
		entry := ServiceMapEntry{
			Service:  svc,
			CallsTo:  stores.service.CallEdgesFrom(svc.Name),
			CalledBy: stores.service.CallEdgesTo(svc.Name),
		}

		// Get operations for this service
		stores.service.mu.RLock()
		for _, op := range stores.service.Operations {
			if op.Service == svc.Name {
				entry.Operations = append(entry.Operations, op)
			}
		}
		stores.service.mu.RUnlock()

		result = append(result, entry)
	}

	return result
}
