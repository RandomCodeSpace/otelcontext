package graphrag

import (
	"context"
	"fmt"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// detectAnomalies runs anomaly detection across every known tenant. For each
// tenant slice we walk the ServiceStore and SignalStore under their own
// locks and emit anomalies into that tenant's AnomalyStore.
func (g *GraphRAG) detectAnomalies(ctx context.Context) {
	tenants := g.snapshotTenants()
	for tenant, stores := range tenants {
		tctx := storage.WithTenantContext(ctx, tenant)
		g.detectAnomaliesForTenant(tctx, tenant, stores)
	}
}

func (g *GraphRAG) detectAnomaliesForTenant(ctx context.Context, tenant string, stores *tenantStores) {
	services := stores.service.AllServices()
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
			stores.anomalies.AddAnomaly(anomaly)
			correlateWithRecent(stores, anomaly)

			// Trigger investigation (scoped to this tenant).
			chains := g.ErrorChain(ctx, svc.Name, now.Add(-5*time.Minute), 5)
			if len(chains) > 0 {
				anomalies := stores.anomalies.AnomaliesForService(svc.Name, now.Add(-1*time.Minute))
				g.PersistInvestigation(tenant, svc.Name, chains, anomalies)
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
			stores.anomalies.AddAnomaly(anomaly)
			correlateWithRecent(stores, anomaly)
		}
	}

	// Metric z-score anomalies (check metrics in this tenant's SignalStore).
	stores.signals.mu.RLock()
	metrics := make([]*MetricNode, 0, len(stores.signals.Metrics))
	for _, m := range stores.signals.Metrics {
		metrics = append(metrics, m)
	}
	stores.signals.mu.RUnlock()

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
				stores.anomalies.AddAnomaly(anomaly)
				correlateWithRecent(stores, anomaly)
			}
		}
	}
}

// correlateWithRecent links an anomaly to other anomalies within ±30s in the
// same tenant's AnomalyStore.
func correlateWithRecent(stores *tenantStores, anomaly AnomalyNode) {
	window := 30 * time.Second
	recent := stores.anomalies.AnomaliesSince(anomaly.Timestamp.Add(-window))
	for _, prev := range recent {
		if prev.ID == anomaly.ID {
			continue
		}
		if prev.Timestamp.After(anomaly.Timestamp.Add(-window)) && prev.Timestamp.Before(anomaly.Timestamp.Add(window)) {
			stores.anomalies.AddPrecededByEdge(anomaly.ID, prev.ID, anomaly.Timestamp)
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

// pruneOldAnomalies removes anomalies older than 24 hours across every
// tenant slice.
func (g *GraphRAG) pruneOldAnomalies() {
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, stores := range g.snapshotTenants() {
		stores.anomalies.mu.Lock()
		for id, a := range stores.anomalies.Anomalies {
			if a.Timestamp.Before(cutoff) {
				delete(stores.anomalies.Anomalies, id)
			}
		}
		for ek, e := range stores.anomalies.Edges {
			if e.UpdatedAt.Before(cutoff) {
				delete(stores.anomalies.Edges, ek)
			}
		}
		stores.anomalies.mu.Unlock()
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
			g.detectAnomalies(ctx)
		}
	}
}
