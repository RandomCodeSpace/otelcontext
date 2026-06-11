package graphrag

// StoreCounts is a point-in-time census of every long-lived in-memory
// structure the coordinator owns, aggregated across tenants. It feeds the
// otelcontext_graphrag_* gauges so operators can attribute RSS growth to a
// specific store before reaching for a heap profile.
type StoreCounts struct {
	Tenants      int
	Services     int
	Operations   int
	Traces       int
	Spans        int
	LogClusters  int
	Metrics      int
	Anomalies    int
	ServiceEdges int
	TraceEdges   int
	SignalEdges  int
	AnomalyEdges int
}

// StoreCounts walks a tenant snapshot and takes len() under each store's
// read lock — no slice building, cheap enough for a periodic sampler.
func (g *GraphRAG) StoreCounts() StoreCounts {
	var c StoreCounts
	for _, st := range g.snapshotTenants() {
		c.Tenants++

		st.service.mu.RLock()
		c.Services += len(st.service.Services)
		c.Operations += len(st.service.Operations)
		c.ServiceEdges += len(st.service.Edges)
		st.service.mu.RUnlock()

		st.traces.mu.RLock()
		c.Traces += len(st.traces.Traces)
		c.Spans += len(st.traces.Spans)
		c.TraceEdges += len(st.traces.Edges)
		st.traces.mu.RUnlock()

		st.signals.mu.RLock()
		c.LogClusters += len(st.signals.LogClusters)
		c.Metrics += len(st.signals.Metrics)
		c.SignalEdges += len(st.signals.Edges)
		st.signals.mu.RUnlock()

		st.anomalies.mu.RLock()
		c.Anomalies += len(st.anomalies.Anomalies)
		c.AnomalyEdges += len(st.anomalies.Edges)
		st.anomalies.mu.RUnlock()
	}
	return c
}

// DrainTemplateCount reports the number of live Drain templates (bounded by
// maxTemplates, but the gauge proves it in production).
func (g *GraphRAG) DrainTemplateCount() int {
	if g.drain == nil {
		return 0
	}
	return g.drain.TemplateCount()
}
