package api

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"time"
)

// SystemSummary is the top-level system health summary.
type SystemSummary struct {
	TotalServices      int     `json:"total_services"`
	Healthy            int     `json:"healthy"`
	Degraded           int     `json:"degraded"`
	Critical           int     `json:"critical"`
	OverallHealthScore float64 `json:"overall_health_score"`
	TotalErrorRate     float64 `json:"total_error_rate"`
	AvgLatencyMs       float64 `json:"avg_latency_ms"`
	UptimeSeconds      float64 `json:"uptime_seconds"`
}

// GraphNode represents a service in the system graph.
type GraphNode struct {
	ID          string      `json:"id"`
	Type        string      `json:"type"`
	HealthScore float64     `json:"health_score"`
	Status      string      `json:"status"`
	Metrics     NodeMetrics `json:"metrics"`
	Alerts      []string    `json:"alerts"`
}

// NodeMetrics holds per-service observability metrics.
type NodeMetrics struct {
	RequestRateRPS float64 `json:"request_rate_rps"`
	ErrorRate      float64 `json:"error_rate"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	P99LatencyMs   float64 `json:"p99_latency_ms"`
	SpanCount1H    int64   `json:"span_count_1h"`
}

// GraphEdge represents a call relationship between two services.
type GraphEdge struct {
	Source       string  `json:"source"`
	Target       string  `json:"target"`
	CallCount    int64   `json:"call_count"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	ErrorRate    float64 `json:"error_rate"`
	Status       string  `json:"status"`
}

// SystemGraphResponse is the full AI-consumable system graph.
type SystemGraphResponse struct {
	Timestamp time.Time     `json:"timestamp"`
	System    SystemSummary `json:"system"`
	Nodes     []GraphNode   `json:"nodes"`
	Edges     []GraphEdge   `json:"edges"`
}

var argusStartTime = time.Now()

// handleGetSystemGraph handles GET /api/system/graph.
// When the in-memory graph has been populated it returns instantly from memory.
// Falls back to a DB query only when the graph has never been built yet.
// Results are additionally cached for 10s to smooth out burst traffic.
func (s *Server) handleGetSystemGraph(w http.ResponseWriter, r *http.Request) {
	const cacheKey = "system_graph"
	const cacheTTL = 10 * time.Second

	if cached, ok := s.cache.Get(cacheKey); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		json.NewEncoder(w).Encode(cached)
		return
	}

	resp := s.buildGraphFromMemory()
	if resp == nil {
		// Graph not yet hydrated — fall back to DB path.
		resp = s.buildGraphFromDB()
		if resp == nil {
			http.Error(w, "failed to build system graph", http.StatusInternalServerError)
			return
		}
	}

	s.cache.Set(cacheKey, resp, cacheTTL)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	json.NewEncoder(w).Encode(resp)
}

// buildGraphFromMemory converts the in-memory graph snapshot to the API response.
// Returns nil if the graph has not been built yet.
func (s *Server) buildGraphFromMemory() *SystemGraphResponse {
	if s.graph == nil {
		return nil
	}
	snap := s.graph.Snapshot()
	if snap.UpdatedAt.IsZero() || len(snap.Nodes) == 0 {
		return nil
	}

	nodes := make([]GraphNode, 0, len(snap.Nodes))
	var totalErrorRate, totalLatency float64

	for _, n := range snap.Nodes {
		alerts := n.Alerts
		if alerts == nil {
			alerts = []string{}
		}
		nodes = append(nodes, GraphNode{
			ID:          n.Name,
			Type:        "service",
			HealthScore: math.Round(n.HealthScore*100) / 100,
			Status:      n.Status,
			Metrics: NodeMetrics{
				RequestRateRPS: math.Round(n.RequestRateRPS*100) / 100,
				ErrorRate:      math.Round(n.ErrorRate*10000) / 10000,
				AvgLatencyMs:   math.Round(n.AvgLatencyMs*100) / 100,
				P99LatencyMs:   math.Round(n.P99LatencyMs*100) / 100,
				SpanCount1H:    n.SpanCount,
			},
			Alerts: alerts,
		})
		totalErrorRate += n.ErrorRate
		totalLatency += n.AvgLatencyMs
	}

	edges := make([]GraphEdge, 0, len(snap.Edges))
	for _, e := range snap.Edges {
		edges = append(edges, GraphEdge{
			Source:       e.Source,
			Target:       e.Target,
			CallCount:    e.CallCount,
			AvgLatencyMs: math.Round(e.AvgLatencyMs*100) / 100,
			ErrorRate:    math.Round(e.ErrorRate*10000) / 10000,
			Status:       e.Status,
		})
	}

	return buildSummaryResponse(nodes, edges, totalErrorRate, totalLatency)
}

// buildGraphFromDB is the fallback path used before the in-memory graph is ready.
func (s *Server) buildGraphFromDB() *SystemGraphResponse {
	end := time.Now()
	start := end.Add(-1 * time.Hour)

	svcMap, err := s.repo.GetServiceMapMetrics(start, end)
	if err != nil {
		slog.Error("Failed to get service map for system graph", "error", err)
		return nil
	}

	nodes := make([]GraphNode, 0, len(svcMap.Nodes))
	var totalErrorRate, totalLatency float64

	for _, n := range svcMap.Nodes {
		errorRate := 0.0
		if n.TotalTraces > 0 {
			errorRate = float64(n.ErrorCount) / float64(n.TotalTraces)
		}
		healthScore := computeHealthScore(errorRate, n.AvgLatencyMs)
		alerts := generateAlerts(n.Name, errorRate, n.AvgLatencyMs)

		nodes = append(nodes, GraphNode{
			ID:          n.Name,
			Type:        "service",
			HealthScore: healthScore,
			Status:      healthStatus(healthScore),
			Metrics: NodeMetrics{
				RequestRateRPS: math.Round(float64(n.TotalTraces)/3600*100) / 100,
				ErrorRate:      math.Round(errorRate*10000) / 10000,
				AvgLatencyMs:   n.AvgLatencyMs,
				P99LatencyMs:   n.AvgLatencyMs * 2.5,
				SpanCount1H:    n.TotalTraces,
			},
			Alerts: alerts,
		})
		totalErrorRate += errorRate
		totalLatency += n.AvgLatencyMs
	}

	edges := make([]GraphEdge, 0, len(svcMap.Edges))
	for _, e := range svcMap.Edges {
		edgeStatus := "healthy"
		if e.ErrorRate > 0.05 {
			edgeStatus = "degraded"
		}
		edges = append(edges, GraphEdge{
			Source:       e.Source,
			Target:       e.Target,
			CallCount:    e.CallCount,
			AvgLatencyMs: e.AvgLatencyMs,
			ErrorRate:    e.ErrorRate,
			Status:       edgeStatus,
		})
	}

	return buildSummaryResponse(nodes, edges, totalErrorRate, totalLatency)
}

// buildSummaryResponse computes system-level aggregates and returns the final response.
func buildSummaryResponse(nodes []GraphNode, edges []GraphEdge, totalErrorRate, totalLatency float64) *SystemGraphResponse {
	healthy, degraded, critical := 0, 0, 0
	for _, n := range nodes {
		switch n.Status {
		case "healthy":
			healthy++
		case "degraded":
			degraded++
		case "critical":
			critical++
		}
	}

	overallHealth := 1.0
	avgLatency := 0.0
	if len(nodes) > 0 {
		overallHealth = math.Round((1.0-totalErrorRate/float64(len(nodes)))*100) / 100
		if overallHealth < 0 {
			overallHealth = 0
		}
		avgLatency = math.Round(totalLatency/float64(len(nodes))*100) / 100
	}

	resp := &SystemGraphResponse{
		Timestamp: time.Now().UTC(),
		System: SystemSummary{
			TotalServices:      len(nodes),
			Healthy:            healthy,
			Degraded:           degraded,
			Critical:           critical,
			OverallHealthScore: overallHealth,
			TotalErrorRate:     math.Round(totalErrorRate/float64(max(len(nodes), 1))*10000) / 10000,
			AvgLatencyMs:       avgLatency,
			UptimeSeconds:      time.Since(argusStartTime).Seconds(),
		},
		Nodes: nodes,
		Edges: edges,
	}
	return resp
}

// computeHealthScore returns a 0.0–1.0 score where 1.0 is fully healthy.
func computeHealthScore(errorRate, avgLatencyMs float64) float64 {
	score := 1.0 - (errorRate * 5.0)
	if avgLatencyMs > 200 {
		score -= (avgLatencyMs - 200) / 2000
	}
	if score < 0 {
		score = 0
	}
	return math.Round(score*100) / 100
}

// healthStatus converts a health score to a status label.
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

// generateAlerts returns human-readable alert strings for an AI agent to reason over.
func generateAlerts(service string, errorRate, avgLatencyMs float64) []string {
	var alerts []string
	if errorRate > 0.05 {
		alerts = append(alerts, "error rate above 5%")
	}
	if errorRate > 0.10 {
		alerts = append(alerts, "error rate above 10% — investigate immediately")
	}
	if avgLatencyMs > 500 {
		alerts = append(alerts, "avg latency above 500ms")
	}
	if avgLatencyMs > 1000 {
		alerts = append(alerts, "avg latency above 1s — SLA breach risk")
	}
	return alerts
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
