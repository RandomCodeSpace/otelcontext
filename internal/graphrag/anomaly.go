package graphrag

import (
	"context"
	"fmt"
	"time"
)

// detectAnomalies runs anomaly detection across all services.
func (g *GraphRAG) detectAnomalies() {
	services := g.ServiceStore.AllServices()
	now := time.Now()

	for _, svc := range services {
		// Error rate spike: > 2x baseline (baseline = long-term avg error rate capped at 5%)
		baselineErrorRate := 0.02 // reasonable baseline
		if svc.ErrorRate > baselineErrorRate*2 && svc.ErrorRate > 0.05 {
			anomaly := AnomalyNode{
				ID:        fmt.Sprintf("anom_%s_err_%d", svc.Name, now.UnixNano()),
				Type:      AnomalyErrorSpike,
				Severity:  classifyErrorSeverity(svc.ErrorRate),
				Service:   svc.Name,
				Evidence:  fmt.Sprintf("error rate %.1f%% (baseline ~%.1f%%)", svc.ErrorRate*100, baselineErrorRate*100),
				Timestamp: now,
			}
			g.AnomalyStore.AddAnomaly(anomaly)
			g.correlateWithRecent(anomaly)

			// Trigger investigation
			chains := g.ErrorChain(svc.Name, now.Add(-5*time.Minute), 5)
			if len(chains) > 0 {
				anomalies := g.AnomalyStore.AnomaliesForService(svc.Name, now.Add(-1*time.Minute))
				g.PersistInvestigation(svc.Name, chains, anomalies)
			}
		}

		// Latency degradation: p99-like check using avg * 3 as proxy
		if svc.AvgLatency > 500 && svc.CallCount > 10 {
			anomaly := AnomalyNode{
				ID:        fmt.Sprintf("anom_%s_lat_%d", svc.Name, now.UnixNano()),
				Type:      AnomalyLatencySpike,
				Severity:  classifyLatencySeverity(svc.AvgLatency),
				Service:   svc.Name,
				Evidence:  fmt.Sprintf("avg latency %.0fms", svc.AvgLatency),
				Timestamp: now,
			}
			g.AnomalyStore.AddAnomaly(anomaly)
			g.correlateWithRecent(anomaly)
		}
	}

	// Metric z-score anomalies (check metrics in SignalStore)
	g.SignalStore.mu.RLock()
	metrics := make([]*MetricNode, 0, len(g.SignalStore.Metrics))
	for _, m := range g.SignalStore.Metrics {
		metrics = append(metrics, m)
	}
	g.SignalStore.mu.RUnlock()

	for _, m := range metrics {
		if m.SampleCount < 10 {
			continue
		}
		// Simple z-score proxy: if current avg deviates significantly from rolling range
		rangeSize := m.RollingMax - m.RollingMin
		if rangeSize > 0 {
			deviation := (m.RollingAvg - (m.RollingMin + rangeSize/2)) / (rangeSize / 2)
			if deviation > 3.0 || deviation < -3.0 {
				anomaly := AnomalyNode{
					ID:        fmt.Sprintf("anom_%s_metric_%d", m.Service, now.UnixNano()),
					Type:      AnomalyMetricZScore,
					Severity:  SeverityWarning,
					Service:   m.Service,
					Evidence:  fmt.Sprintf("metric %s z-score %.1f (avg=%.2f, range=[%.2f, %.2f])", m.MetricName, deviation, m.RollingAvg, m.RollingMin, m.RollingMax),
					Timestamp: now,
				}
				g.AnomalyStore.AddAnomaly(anomaly)
				g.correlateWithRecent(anomaly)
			}
		}
	}
}

// correlateWithRecent links an anomaly to other anomalies within ±30s.
func (g *GraphRAG) correlateWithRecent(anomaly AnomalyNode) {
	window := 30 * time.Second
	recent := g.AnomalyStore.AnomaliesSince(anomaly.Timestamp.Add(-window))
	for _, prev := range recent {
		if prev.ID == anomaly.ID {
			continue
		}
		if prev.Timestamp.After(anomaly.Timestamp.Add(-window)) && prev.Timestamp.Before(anomaly.Timestamp.Add(window)) {
			g.AnomalyStore.AddPrecededByEdge(anomaly.ID, prev.ID, anomaly.Timestamp)
		}
	}
}

func classifyErrorSeverity(errorRate float64) AnomalySeverity {
	switch {
	case errorRate > 0.2:
		return SeverityCritical
	case errorRate > 0.1:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}

func classifyLatencySeverity(avgMs float64) AnomalySeverity {
	switch {
	case avgMs > 2000:
		return SeverityCritical
	case avgMs > 1000:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}

// pruneOldAnomalies removes anomalies older than 24 hours.
func (g *GraphRAG) pruneOldAnomalies() {
	cutoff := time.Now().Add(-24 * time.Hour)
	g.AnomalyStore.mu.Lock()
	defer g.AnomalyStore.mu.Unlock()
	for id, a := range g.AnomalyStore.Anomalies {
		if a.Timestamp.Before(cutoff) {
			delete(g.AnomalyStore.Anomalies, id)
		}
	}
	for ek, e := range g.AnomalyStore.Edges {
		if e.UpdatedAt.Before(cutoff) {
			delete(g.AnomalyStore.Edges, ek)
		}
	}
}

// anomalyLoop runs anomaly detection on a timer.
func (g *GraphRAG) anomalyLoop(ctx context.Context) {
	ticker := time.NewTicker(g.anomalyEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.detectAnomalies()
		}
	}
}
