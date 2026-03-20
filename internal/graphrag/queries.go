package graphrag

import (
	"math"
	"sort"
	"time"
)

// ErrorChain traces error spans upstream to find the root cause service.
func (g *GraphRAG) ErrorChain(service string, since time.Time, limit int) []ErrorChainResult {
	if limit <= 0 {
		limit = 10
	}

	errorSpans := g.TraceStore.ErrorSpans(service, since)
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

		chain := g.traceErrorChainUpstream(span)
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
				clusters := g.SignalStore.LogClustersForService(s.Service)
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

// traceErrorChainUpstream walks CHILD_OF edges upstream from an error span to the root.
func (g *GraphRAG) traceErrorChainUpstream(span *SpanNode) []SpanNode {
	var chain []SpanNode
	visited := make(map[string]bool)
	current := span

	for current != nil && !visited[current.ID] {
		visited[current.ID] = true
		chain = append(chain, *current)

		if current.ParentSpanID == "" {
			break
		}
		parent, ok := g.TraceStore.GetSpan(current.ParentSpanID)
		if !ok {
			break
		}
		current = parent
	}

	return chain
}

// ImpactAnalysis performs BFS downstream from a service to find affected services.
func (g *GraphRAG) ImpactAnalysis(service string, maxDepth int) *ImpactResult {
	if maxDepth <= 0 {
		maxDepth = 5
	}

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

		edges := g.ServiceStore.CallEdgesFrom(item.svc)
		for _, e := range edges {
			if visited[e.ToID] {
				continue
			}
			visited[e.ToID] = true

			svc, _ := g.ServiceStore.GetService(e.ToID)
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
func (g *GraphRAG) RootCauseAnalysis(service string, since time.Time) []RankedCause {
	errorChains := g.ErrorChain(service, since, 20)
	anomalies := g.AnomalyStore.AnomaliesForService(service, since)

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
func (g *GraphRAG) DependencyChain(traceID string) []SpanNode {
	spans := g.TraceStore.SpansForTrace(traceID)
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
	Service     string           `json:"service"`
	ErrorLogs   []LogClusterNode `json:"error_logs"`
	Metrics     []MetricNode     `json:"metrics"`
	Anomalies   []AnomalyNode    `json:"anomalies"`
	ErrorChains []ErrorChainResult `json:"error_chains,omitempty"`
}

func (g *GraphRAG) CorrelatedSignals(service string, since time.Time) *CorrelatedSignalsResult {
	result := &CorrelatedSignalsResult{Service: service}

	// Error logs
	clusters := g.SignalStore.LogClustersForService(service)
	for _, lc := range clusters {
		if lc.LastSeen.After(since) {
			result.ErrorLogs = append(result.ErrorLogs, *lc)
		}
	}

	// Metrics
	metrics := g.SignalStore.MetricsForService(service)
	for _, m := range metrics {
		result.Metrics = append(result.Metrics, *m)
	}

	// Anomalies
	anomalies := g.AnomalyStore.AnomaliesForService(service, since)
	for _, a := range anomalies {
		result.Anomalies = append(result.Anomalies, *a)
	}

	// Recent error chains
	result.ErrorChains = g.ErrorChain(service, since, 5)

	return result
}

// ShortestPath finds the shortest path between two services using Dijkstra.
func (g *GraphRAG) ShortestPath(from, to string) []string {
	// Build adjacency from CALLS edges
	g.ServiceStore.mu.RLock()
	adj := make(map[string]map[string]float64)
	for _, e := range g.ServiceStore.Edges {
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
	g.ServiceStore.mu.RUnlock()

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
func (g *GraphRAG) AnomalyTimeline(since time.Time) []*AnomalyNode {
	anomalies := g.AnomalyStore.AnomaliesSince(since)
	sort.Slice(anomalies, func(i, j int) bool {
		return anomalies[i].Timestamp.After(anomalies[j].Timestamp)
	})
	return anomalies
}

// ServiceMap returns the service topology with health scores for the API.
type ServiceMapEntry struct {
	Service     *ServiceNode     `json:"service"`
	Operations  []*OperationNode `json:"operations,omitempty"`
	CallsTo     []*Edge          `json:"calls_to,omitempty"`
	CalledBy    []*Edge          `json:"called_by,omitempty"`
}

func (g *GraphRAG) ServiceMap(depth int) []ServiceMapEntry {
	services := g.ServiceStore.AllServices()
	result := make([]ServiceMapEntry, 0, len(services))

	for _, svc := range services {
		entry := ServiceMapEntry{
			Service:  svc,
			CallsTo:  g.ServiceStore.CallEdgesFrom(svc.Name),
			CalledBy: g.ServiceStore.CallEdgesTo(svc.Name),
		}

		// Get operations for this service
		g.ServiceStore.mu.RLock()
		for _, op := range g.ServiceStore.Operations {
			if op.Service == svc.Name {
				entry.Operations = append(entry.Operations, op)
			}
		}
		g.ServiceStore.mu.RUnlock()

		result = append(result, entry)
	}

	return result
}
