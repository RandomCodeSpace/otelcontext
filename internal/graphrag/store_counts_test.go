package graphrag

import (
	"testing"
	"time"
)

func TestStoreCountsAggregatesAcrossTenants(t *testing.T) {
	g := newTestGraphRAG(t)
	now := time.Now()

	a := g.storesForTenant("tenant-a")
	a.service.UpsertService("svc-1", 10, false, now)
	a.service.UpsertService("svc-2", 20, true, now)
	a.service.UpsertOperation("svc-1", "GET /x", 10, false, now)
	a.service.UpsertCallEdge("svc-1", "svc-2", 10, false, now)
	a.traces.UpsertTrace("trace-1", "svc-1", "OK", 12, now)
	a.traces.UpsertSpan(SpanNode{ID: "span-1", TraceID: "trace-1", Service: "svc-1", Timestamp: now})

	b := g.storesForTenant("tenant-b")
	b.signals.UpsertLogCluster("cl-1", "tmpl", "ERROR", "svc-3", now)
	b.signals.UpsertMetric("cpu", "svc-3", 0.5, now)
	b.anomalies.AddAnomaly(AnomalyNode{ID: "anom-1", Service: "svc-3", Timestamp: now})
	b.anomalies.AddAnomaly(AnomalyNode{ID: "anom-2", Service: "svc-3", Timestamp: now})
	b.anomalies.AddPrecededByEdge("anom-2", "anom-1", now)

	c := g.StoreCounts()

	if c.Tenants < 2 {
		t.Fatalf("Tenants = %d, want >= 2", c.Tenants)
	}
	if c.Services != 2 {
		t.Fatalf("Services = %d, want 2", c.Services)
	}
	if c.Operations != 1 {
		t.Fatalf("Operations = %d, want 1", c.Operations)
	}
	if c.Spans != 1 {
		t.Fatalf("Spans = %d, want 1", c.Spans)
	}
	if c.LogClusters != 1 {
		t.Fatalf("LogClusters = %d, want 1", c.LogClusters)
	}
	if c.Metrics != 1 {
		t.Fatalf("Metrics = %d, want 1", c.Metrics)
	}
	if c.Anomalies != 2 {
		t.Fatalf("Anomalies = %d, want 2", c.Anomalies)
	}
	// UpsertCallEdge creates a CALLS edge; UpsertOperation an EXPOSES edge.
	if c.ServiceEdges != 2 {
		t.Fatalf("ServiceEdges = %d, want 2", c.ServiceEdges)
	}
	// Each AddAnomaly creates a TRIGGERED_BY edge, plus one PRECEDED_BY edge.
	if c.AnomalyEdges != 3 {
		t.Fatalf("AnomalyEdges = %d, want 3", c.AnomalyEdges)
	}
	// Metric upsert creates a MEASURED_BY edge; log cluster upsert an EMITTED_BY edge.
	if c.SignalEdges != 2 {
		t.Fatalf("SignalEdges = %d, want 2", c.SignalEdges)
	}
}

func TestDrainTemplateCount(t *testing.T) {
	d := NewDrain()
	if got := d.TemplateCount(); got != 0 {
		t.Fatalf("empty miner TemplateCount = %d, want 0", got)
	}
	d.Match("user 42 logged in", time.Now())
	d.Match("user 43 logged in", time.Now())
	if got := d.TemplateCount(); got != 1 {
		t.Fatalf("TemplateCount = %d, want 1 (same template)", got)
	}
}
