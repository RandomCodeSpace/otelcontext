// Package graph provides an in-memory service dependency graph rebuilt
// periodically from recent span data. It is a processing accelerator —
// the relational DB remains the source of truth.
package graph

import (
	"context"
	"math"
	"sync"
	"time"
)

// SpanRow is the minimal span data needed to build the graph.
// Callers supply this via the DataProvider to decouple the graph from GORM.
type SpanRow struct {
	SpanID       string
	ParentSpanID string
	ServiceName  string
	OperationName string
	DurationMs   float64
	IsError      bool
	Timestamp    time.Time
}

// DataProvider is the function the graph calls every refresh cycle to fetch
// recent spans. Decouples graph from storage layer.
type DataProvider func(since time.Time) ([]SpanRow, error)

// EdgeKey identifies a directed service-to-service call.
type EdgeKey struct {
	Source string
	Target string
}

// edgeStats accumulates call stats for one directed edge.
type edgeStats struct {
	callCount  int64
	errorCount int64
	totalMs    float64
	minMs      float64
	maxMs      float64
	latencies  []float64 // capped at 1000 samples for p99
}

// ServiceNode holds aggregated health data for a single service.
type ServiceNode struct {
	Name        string
	HealthScore float64 // 0.0–1.0
	Status      string  // "healthy" | "degraded" | "critical"

	RequestRateRPS  float64
	ErrorRate       float64
	AvgLatencyMs    float64
	P99LatencyMs    float64
	SpanCount       int64
	LogErrorCount   int64 // set externally by callers if needed

	Alerts []string
}

// ServiceEdge is a directed dependency between two services.
type ServiceEdge struct {
	Source      string
	Target      string
	CallCount   int64
	AvgLatencyMs float64
	ErrorRate   float64
	Status      string // "healthy" | "degraded" | "critical"
}

// Snapshot is the immutable graph state returned to callers.
type Snapshot struct {
	Nodes     map[string]*ServiceNode
	Edges     []ServiceEdge
	UpdatedAt time.Time
}

// Graph is a thread-safe in-memory service dependency graph.
type Graph struct {
	mu       sync.RWMutex
	snapshot *Snapshot

	provider     DataProvider
	windowSize   time.Duration // how far back to look for spans
	refreshEvery time.Duration
}

// New creates a Graph. refreshEvery controls how often it rebuilds from the DB.
// windowSize is the lookback window for span data (e.g. 5 minutes).
func New(provider DataProvider, windowSize, refreshEvery time.Duration) *Graph {
	return &Graph{
		provider:     provider,
		windowSize:   windowSize,
		refreshEvery: refreshEvery,
		snapshot:     &Snapshot{Nodes: map[string]*ServiceNode{}, UpdatedAt: time.Time{}},
	}
}

// Start rebuilds the graph on a background goroutine until ctx is cancelled.
func (g *Graph) Start(ctx context.Context) {
	// Initial build — non-blocking best-effort.
	g.rebuild()

	ticker := time.NewTicker(g.refreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.rebuild()
		}
	}
}

// Snapshot returns the latest computed graph state (safe for concurrent reads).
func (g *Graph) Snapshot() *Snapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.snapshot
}

// rebuild fetches recent spans and recomputes the graph.
func (g *Graph) rebuild() {
	since := time.Now().UTC().Add(-g.windowSize)
	rows, err := g.provider(since)
	if err != nil || len(rows) == 0 {
		return
	}

	// --- Pass 1: index spanID → serviceName for edge resolution ---
	spanService := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.SpanID != "" {
			spanService[r.SpanID] = r.ServiceName
		}
	}

	// --- Pass 2: accumulate per-service stats and per-edge stats ---
	type nodeAcc struct {
		totalMs    float64
		errorCount int64
		spanCount  int64
		latencies  []float64
		firstSeen  time.Time
		lastSeen   time.Time
	}
	nodes := map[string]*nodeAcc{}
	edges := map[EdgeKey]*edgeStats{}

	for i := range rows {
		r := &rows[i]
		n, ok := nodes[r.ServiceName]
		if !ok {
			n = &nodeAcc{firstSeen: r.Timestamp, lastSeen: r.Timestamp}
			nodes[r.ServiceName] = n
		}
		n.spanCount++
		n.totalMs += r.DurationMs
		if r.IsError {
			n.errorCount++
		}
		if len(n.latencies) < 1000 {
			n.latencies = append(n.latencies, r.DurationMs)
		}
		if r.Timestamp.Before(n.firstSeen) {
			n.firstSeen = r.Timestamp
		}
		if r.Timestamp.After(n.lastSeen) {
			n.lastSeen = r.Timestamp
		}

		// Edge: if parent span belongs to a different service, record the call.
		if r.ParentSpanID != "" {
			parentSvc, found := spanService[r.ParentSpanID]
			if found && parentSvc != r.ServiceName {
				ek := EdgeKey{Source: parentSvc, Target: r.ServiceName}
				e, ok := edges[ek]
				if !ok {
					e = &edgeStats{minMs: math.MaxFloat64}
					edges[ek] = e
				}
				e.callCount++
				e.totalMs += r.DurationMs
				if r.DurationMs < e.minMs {
					e.minMs = r.DurationMs
				}
				if r.DurationMs > e.maxMs {
					e.maxMs = r.DurationMs
				}
				if r.IsError {
					e.errorCount++
				}
				if len(e.latencies) < 1000 {
					e.latencies = append(e.latencies, r.DurationMs)
				}
			}
		}
	}

	// --- Pass 3: compute derived metrics and build final snapshot ---
	windowSeconds := g.windowSize.Seconds()

	resultNodes := make(map[string]*ServiceNode, len(nodes))
	for name, acc := range nodes {
		var avgMs, p99Ms, errorRate float64
		if acc.spanCount > 0 {
			avgMs = acc.totalMs / float64(acc.spanCount)
			errorRate = float64(acc.errorCount) / float64(acc.spanCount)
		}
		p99Ms = percentile(acc.latencies, 99)

		rps := float64(acc.spanCount) / windowSeconds

		score, status := healthScore(errorRate, avgMs)
		alerts := buildAlerts(name, errorRate, p99Ms)

		resultNodes[name] = &ServiceNode{
			Name:           name,
			HealthScore:    score,
			Status:         status,
			RequestRateRPS: rps,
			ErrorRate:      errorRate,
			AvgLatencyMs:   avgMs,
			P99LatencyMs:   p99Ms,
			SpanCount:      acc.spanCount,
			Alerts:         alerts,
		}
	}

	resultEdges := make([]ServiceEdge, 0, len(edges))
	for ek, e := range edges {
		var avgMs, errRate float64
		if e.callCount > 0 {
			avgMs = e.totalMs / float64(e.callCount)
			errRate = float64(e.errorCount) / float64(e.callCount)
		}
		_, status := healthScore(errRate, avgMs)
		resultEdges = append(resultEdges, ServiceEdge{
			Source:       ek.Source,
			Target:       ek.Target,
			CallCount:    e.callCount,
			AvgLatencyMs: avgMs,
			ErrorRate:    errRate,
			Status:       status,
		})
	}

	g.mu.Lock()
	g.snapshot = &Snapshot{
		Nodes:     resultNodes,
		Edges:     resultEdges,
		UpdatedAt: time.Now().UTC(),
	}
	g.mu.Unlock()
}

// healthScore returns (score 0.0–1.0, status string) for given error rate and
// average latency. Formula: 1.0 - (error_rate × 5) - (latency_deviation × 0.1)
// where latency_deviation = max(0, (avgLatencyMs-100)/100).
func healthScore(errorRate, avgLatencyMs float64) (float64, string) {
	latencyDev := math.Max(0, (avgLatencyMs-100)/100)
	score := 1.0 - (errorRate * 5) - (latencyDev * 0.1)
	if score < 0 {
		score = 0
	}
	switch {
	case score >= 0.9:
		return score, "healthy"
	case score >= 0.7:
		return score, "degraded"
	default:
		return score, "critical"
	}
}

// buildAlerts generates human-readable alert strings for AI agent consumption.
func buildAlerts(service string, errorRate, p99Ms float64) []string {
	var alerts []string
	if errorRate > 0.05 {
		alerts = append(alerts, "high error rate: "+pctStr(errorRate))
	}
	if p99Ms > 500 {
		alerts = append(alerts, "p99 latency exceeds 500ms")
	}
	return alerts
}

func pctStr(f float64) string {
	pct := int(f * 100)
	s := ""
	if pct >= 10 {
		s = string(rune('0'+pct/10)) + string(rune('0'+pct%10)) + "%"
	} else {
		s = string(rune('0'+pct)) + "%"
	}
	return s
}

// percentile returns the p-th percentile (0–100) of a float64 slice.
// Uses a simple sort-based approach; slice is small (≤1000 items).
func percentile(data []float64, p float64) float64 {
	if len(data) == 0 {
		return 0
	}
	// Insertion sort is fine for ≤1000 items.
	sorted := make([]float64, len(data))
	copy(sorted, data)
	for i := 1; i < len(sorted); i++ {
		key := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j] > key {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = key
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
