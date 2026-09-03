package graphrag

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/latency"
)

// tenantStores bundles one tenant's slice of the four layered in-memory
// stores. Every mutation and query lands in exactly one composite, keyed by
// tenant ID in the coordinator (see storesFor / storesForTenant in builder.go).
// Storage is lazily created on first reference — empty-tenant contexts coerce
// to storage.DefaultTenantID at the lookup boundary.
type tenantStores struct {
	service   *ServiceStore
	traces    *TraceStore
	signals   *SignalStore
	anomalies *AnomalyStore

	// topology records cross-service CALLS edges from EVERY received span,
	// pre-sample (see topology_observer.go). Bounded and tenant-isolated by
	// living on this slice; guarantees the service map shows flow direction
	// even when sampling starves the sampled edge path.
	topology *topologyObserver

	// lastAccess is the unix-nano time of the most recent ingest event or
	// query routed through storesForTenant. Drives idle-tenant eviction in
	// refreshLoop; background maintenance uses tenantStoresNoTouch so a 60s
	// bookkeeping pass cannot keep a dormant tenant alive.
	lastAccess atomic.Int64

	// lastEventAt is the unix-nano time of the most recent span/log/metric
	// processed for this tenant. detectAnomalies skips tenants whose value
	// predates the previous scan tick — their stats cannot have changed.
	lastEventAt atomic.Int64

	// lastRebuildMax is the high-water-mark (unix nanos) of span start_time
	// merged by rebuildFromDBForTenant. Subsequent rebuilds only re-read
	// rows newer than HWM minus a small overlap instead of the full
	// trailing window. Zero on a fresh slice (first build / post-eviction)
	// forces a full-window rebuild.
	lastRebuildMax atomic.Int64

	// --- aggregate mode (#174) ---

	// topoEpoch and topoRevision record the aggregate topology snapshot this
	// slice currently reflects. A snapshot whose pair matches is a no-op; a
	// changed epoch means the engine restarted and the revision counter began
	// again, so the slice is replaced rather than reconciled.
	topoEpoch    atomic.Uint64
	topoRevision atomic.Uint64

	// lastTopology is the most recently applied snapshot, kept so the
	// aggregate anomaly detector reads windowed history without going back to
	// the engine (and never to a database).
	lastTopology atomic.Pointer[aggregate.TopologySnapshot]

	// anomalyRevision is the topology revision the aggregate anomaly detector
	// last evaluated. Detection is revision-gated: an unchanged topology
	// cannot produce a new anomaly.
	anomalyRevision atomic.Uint64
}

func newTenantStores(traceTTL time.Duration, maxSpans int) *tenantStores {
	ts := &tenantStores{
		service:   newServiceStore(),
		traces:    newTraceStore(traceTTL, maxSpans),
		signals:   newSignalStore(),
		anomalies: newAnomalyStore(),
		topology:  newTopologyObserver(),
	}
	// Creation counts as access — a slice re-created by the DB rebuild gets
	// a full idle window rather than being evicted on the next tick.
	ts.lastAccess.Store(time.Now().UnixNano())
	return ts
}

// ServiceStore holds permanent service topology data.
type ServiceStore struct {
	mu         sync.RWMutex
	Services   map[string]*ServiceNode   // key: service name
	Operations map[string]*OperationNode // key: service|operation
	Edges      map[string]*Edge          // key: type|from|to
	// Adjacency indexes over the CALLS edges and operations, maintained at the
	// upsert sites so ServiceMap/ImpactAnalysis don't rescan the whole Edges map
	// per service (O(N·E)→O(N+E)). Safe as plain slices because ServiceStore
	// CALLS edges and operations are append-only at runtime — the only
	// .Edges deletions live in TraceStore/SignalStore/AnomalyStore, and the
	// store is never replaced or cleared after newServiceStore().
	edgesByFrom  map[string][]*Edge          // CALLS edges keyed by FromID
	edgesByTo    map[string][]*Edge          // CALLS edges keyed by ToID
	opsByService map[string][]*OperationNode // operations keyed by service
	// latency holds one bounded duration sketch per service (#291), keyed
	// like Services and fed wherever UpsertService updates AvgLatency — the
	// ingest callback and the 60s DB rebuild alike — so it shares the
	// lifecycle of the other per-service aggregates: it resets when the
	// tenant slice is re-created (idle eviction, restart), never on its own.
	// A Sketch is a fixed ~2.1 KiB value (512 uint32 bins plus counters, no
	// pointers), so the per-tenant memory is 2.1 KiB × len(Services): the
	// same bound as the ServiceNode map itself.
	latency map[string]*aggregate.Sketch
}

func newServiceStore() *ServiceStore {
	return &ServiceStore{
		Services:     make(map[string]*ServiceNode),
		Operations:   make(map[string]*OperationNode),
		Edges:        make(map[string]*Edge),
		edgesByFrom:  make(map[string][]*Edge),
		edgesByTo:    make(map[string][]*Edge),
		opsByService: make(map[string][]*OperationNode),
		latency:      make(map[string]*aggregate.Sketch),
	}
}

// TraceStore holds trace/span detail with TTL-based pruning.
type TraceStore struct {
	mu     sync.RWMutex
	Traces map[string]*TraceNode // key: trace_id
	Spans  map[string]*SpanNode  // key: span_id
	Edges  map[string]*Edge      // key: type|from|to
	TTL    time.Duration
	// MaxSpans hard-caps the Spans map: at the cap, NEW span IDs are
	// skipped (UpsertSpan returns false) while updates to resident IDs
	// still apply. <=0 disables the cap.
	MaxSpans int
}

func newTraceStore(ttl time.Duration, maxSpans int) *TraceStore {
	return &TraceStore{
		Traces:   make(map[string]*TraceNode),
		Spans:    make(map[string]*SpanNode),
		Edges:    make(map[string]*Edge),
		TTL:      ttl,
		MaxSpans: maxSpans,
	}
}

// SignalStore holds log cluster and metric correlation data.
type SignalStore struct {
	mu          sync.RWMutex
	LogClusters map[string]*LogClusterNode // key: cluster ID
	Metrics     map[string]*MetricNode     // key: metric|service
	Edges       map[string]*Edge           // key: type|from|to
}

func newSignalStore() *SignalStore {
	return &SignalStore{
		LogClusters: make(map[string]*LogClusterNode),
		Metrics:     make(map[string]*MetricNode),
		Edges:       make(map[string]*Edge),
	}
}

// AnomalyStore holds detected anomalies and their temporal correlations.
type AnomalyStore struct {
	mu        sync.RWMutex
	Anomalies map[string]*AnomalyNode // key: anomaly ID
	Edges     map[string]*Edge        // key: type|from|to
}

func newAnomalyStore() *AnomalyStore {
	return &AnomalyStore{
		Anomalies: make(map[string]*AnomalyNode),
		Edges:     make(map[string]*Edge),
	}
}

// edgeKey generates a deterministic key for an edge.
func edgeKey(et EdgeType, from, to string) string {
	return string(et) + "|" + from + "|" + to
}

// --- ServiceStore methods ---

func (s *ServiceStore) UpsertService(name string, durationMs float64, isError bool, ts time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	svc, ok := s.Services[name]
	if !ok {
		svc = &ServiceNode{
			ID:        name,
			Name:      name,
			FirstSeen: ts,
			LastSeen:  ts,
		}
		s.Services[name] = svc
	}
	svc.CallCount++
	svc.TotalMs += durationMs
	if isError {
		svc.ErrorCount++
	}
	if ts.After(svc.LastSeen) {
		svc.LastSeen = ts
	}
	if ts.Before(svc.FirstSeen) {
		svc.FirstSeen = ts
	}
	svc.AvgLatency = svc.TotalMs / float64(svc.CallCount)
	sketch, ok := s.latency[name]
	if !ok {
		sketch = aggregate.NewSketch()
		s.latency[name] = sketch
	}
	sketch.Observe(durationMs)
	svc.P99Latency = sketch.Quantile(0.99)
	svc.LatencyProvenance = &latency.Provenance{P99: aggregate.PercentileFromSketch(sketch)}
	svc.ErrorRate = float64(svc.ErrorCount) / float64(svc.CallCount)
	svc.HealthScore = computeHealth(svc.ErrorRate, svc.AvgLatency)
}

func (s *ServiceStore) UpsertOperation(service, operation string, durationMs float64, isError bool, ts time.Time) {
	key := service + "|" + operation
	s.mu.Lock()
	defer s.mu.Unlock()

	op, ok := s.Operations[key]
	if !ok {
		op = &OperationNode{
			ID:        key,
			Service:   service,
			Operation: operation,
			FirstSeen: ts,
			LastSeen:  ts,
		}
		s.Operations[key] = op
		s.opsByService[service] = append(s.opsByService[service], op)
	}
	op.CallCount++
	op.TotalMs += durationMs
	if isError {
		op.ErrorCount++
	}
	if ts.After(op.LastSeen) {
		op.LastSeen = ts
	}
	op.AvgLatency = op.TotalMs / float64(op.CallCount)
	op.LatencyProvenance = unavailableOperationLatency()
	op.ErrorRate = float64(op.ErrorCount) / float64(op.CallCount)
	op.HealthScore = computeHealth(op.ErrorRate, op.AvgLatency)

	// EXPOSES edge
	ek := edgeKey(EdgeExposes, service, key)
	if _, exists := s.Edges[ek]; !exists {
		s.Edges[ek] = &Edge{
			Type:      EdgeExposes,
			FromID:    service,
			ToID:      key,
			UpdatedAt: ts,
		}
	}
}

func unavailableOperationLatency() *latency.Provenance {
	percentile := func() *latency.Percentile {
		return &latency.Percentile{
			Status: latency.StatusUnavailable,
			Reason: latency.ReasonPercentileNotRecorded,
		}
	}
	return &latency.Provenance{P50: percentile(), P95: percentile(), P99: percentile()}
}

func (s *ServiceStore) UpsertCallEdge(source, target string, durationMs float64, isError bool, ts time.Time) {
	ek := edgeKey(EdgeCalls, source, target)
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.Edges[ek]
	if !ok {
		e = &Edge{
			Type:   EdgeCalls,
			FromID: source,
			ToID:   target,
		}
		s.Edges[ek] = e
		s.edgesByFrom[source] = append(s.edgesByFrom[source], e)
		s.edgesByTo[target] = append(s.edgesByTo[target], e)
	}
	e.CallCount++
	e.TotalMs += durationMs
	if isError {
		e.ErrorCount++
	}
	e.AvgMs = e.TotalMs / float64(e.CallCount)
	e.ErrorRate = float64(e.ErrorCount) / float64(e.CallCount)
	e.Weight = float64(e.CallCount)
	e.UpdatedAt = ts
}

// EnsureService guarantees a ServiceNode for name EXISTS without touching its
// aggregates. Absent → created with zeroed call/error stats (FirstSeen/LastSeen
// = ts); present → no-op. The pre-sample topology observer uses this so the
// service map has a row to hang flow-direction edges on even when sampling
// dropped every span for the service; the sampled path (UpsertService) and the
// 60s DB rebuild remain the sole source of call/error/latency aggregates.
// Returns true if a new node was created.
func (s *ServiceStore) EnsureService(name string, ts time.Time) bool {
	if name == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.Services[name]; ok {
		return false
	}
	s.Services[name] = &ServiceNode{
		ID:        name,
		Name:      name,
		FirstSeen: ts,
		LastSeen:  ts,
		LatencyProvenance: &latency.Provenance{P99: &latency.Percentile{
			Status: latency.StatusUnavailable,
			Method: latency.MethodAverageMultiplier,
			Reason: latency.ReasonNoObservations,
		}},
	}
	return true
}

// EnsureCallEdge guarantees a CALLS edge source→target EXISTS without touching
// its aggregates. If the edge is absent it is created with zeroed
// CallCount/latency/error stats (UpdatedAt = ts); if it already exists this is a
// no-op. The pre-sample topology observer uses this so the service map shows
// flow direction even when sampling dropped every span that would have formed
// the edge — while the sampled path (UpsertCallEdge) remains the sole source of
// CallCount/latency/error-rate aggregates. Returns true if a new edge was created.
func (s *ServiceStore) EnsureCallEdge(source, target string, ts time.Time) bool {
	ek := edgeKey(EdgeCalls, source, target)
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.Edges[ek]; ok {
		return false
	}
	e := &Edge{
		Type:      EdgeCalls,
		FromID:    source,
		ToID:      target,
		UpdatedAt: ts,
	}
	s.Edges[ek] = e
	s.edgesByFrom[source] = append(s.edgesByFrom[source], e)
	s.edgesByTo[target] = append(s.edgesByTo[target], e)
	return true
}

func (s *ServiceStore) GetService(name string) (*ServiceNode, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	svc, ok := s.Services[name]
	return svc, ok
}

func (s *ServiceStore) AllServices() []*ServiceNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ServiceNode, 0, len(s.Services))
	for _, svc := range s.Services {
		out = append(out, svc)
	}
	return out
}

func (s *ServiceStore) AllEdges() []*Edge {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Edge, 0, len(s.Edges))
	for _, e := range s.Edges {
		out = append(out, e)
	}
	return out
}

func (s *ServiceStore) CallEdgesFrom(service string) []*Edge {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.edgesByFrom[service]
	if len(src) == 0 {
		return nil
	}
	out := make([]*Edge, len(src))
	copy(out, src)
	return out
}

func (s *ServiceStore) CallEdgesTo(service string) []*Edge {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.edgesByTo[service]
	if len(src) == 0 {
		return nil
	}
	out := make([]*Edge, len(src))
	copy(out, src)
	return out
}

// OperationsForService returns the operations exposed by a service via the
// per-service index, avoiding a full Operations-map scan per service.
func (s *ServiceStore) OperationsForService(service string) []*OperationNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.opsByService[service]
	if len(src) == 0 {
		return nil
	}
	out := make([]*OperationNode, len(src))
	copy(out, src)
	return out
}

// --- TraceStore methods ---

func (ts *TraceStore) UpsertTrace(traceID, rootService, status string, durationMs float64, timestamp time.Time) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	t, ok := ts.Traces[traceID]
	if !ok {
		t = &TraceNode{
			ID:          traceID,
			RootService: rootService,
			Status:      status,
			Duration:    durationMs,
			Timestamp:   timestamp,
		}
		ts.Traces[traceID] = t
	}
	t.SpanCount++
	if durationMs > t.Duration {
		t.Duration = durationMs
	}
	if status == "STATUS_CODE_ERROR" {
		t.Status = status
	}
}

// UpsertSpan inserts or updates a span node and its CONTAINS/CHILD_OF edges.
// Returns false when the span is NEW and MaxSpans is already reached — the
// span is skipped entirely (the graph is best-effort; the DB is the source
// of truth, same doctrine as the event-channel overflow in builder.go).
func (ts *TraceStore) UpsertSpan(span SpanNode) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.MaxSpans > 0 && len(ts.Spans) >= ts.MaxSpans {
		if _, resident := ts.Spans[span.ID]; !resident {
			return false
		}
	}
	ts.Spans[span.ID] = &span

	// CONTAINS edge: trace → span
	ck := edgeKey(EdgeContains, span.TraceID, span.ID)
	if _, ok := ts.Edges[ck]; !ok {
		ts.Edges[ck] = &Edge{
			Type:      EdgeContains,
			FromID:    span.TraceID,
			ToID:      span.ID,
			UpdatedAt: span.Timestamp,
		}
	}

	// CHILD_OF edge: span → parent
	if span.ParentSpanID != "" {
		pk := edgeKey(EdgeChildOf, span.ID, span.ParentSpanID)
		if _, ok := ts.Edges[pk]; !ok {
			ts.Edges[pk] = &Edge{
				Type:      EdgeChildOf,
				FromID:    span.ID,
				ToID:      span.ParentSpanID,
				UpdatedAt: span.Timestamp,
			}
		}
	}
	return true
}

func (ts *TraceStore) GetSpan(spanID string) (*SpanNode, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	s, ok := ts.Spans[spanID]
	return s, ok
}

func (ts *TraceStore) GetTrace(traceID string) (*TraceNode, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	t, ok := ts.Traces[traceID]
	return t, ok
}

func (ts *TraceStore) SpansForTrace(traceID string) []*SpanNode {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	var out []*SpanNode
	for _, s := range ts.Spans {
		if s.TraceID == traceID {
			out = append(out, s)
		}
	}
	return out
}

func (ts *TraceStore) ErrorSpans(service string, since time.Time) []*SpanNode {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	var out []*SpanNode
	for _, s := range ts.Spans {
		if s.IsError && s.Service == service && s.Timestamp.After(since) {
			out = append(out, s)
		}
	}
	return out
}

// Prune removes spans and traces older than TTL.
func (ts *TraceStore) Prune() int {
	cutoff := time.Now().Add(-ts.TTL)
	ts.mu.Lock()
	defer ts.mu.Unlock()

	pruned := 0
	for id, s := range ts.Spans {
		if s.Timestamp.Before(cutoff) {
			delete(ts.Spans, id)
			pruned++
		}
	}
	for id, t := range ts.Traces {
		if t.Timestamp.Before(cutoff) {
			delete(ts.Traces, id)
		}
	}
	// Clean up orphaned edges
	for ek, e := range ts.Edges {
		if e.UpdatedAt.Before(cutoff) {
			delete(ts.Edges, ek)
		}
	}
	return pruned
}

// --- SignalStore methods ---

func (ss *SignalStore) UpsertLogCluster(id, template, severity, service string, ts time.Time) {
	ss.UpsertLogClusterWithTemplate(id, template, severity, service, 0, nil, "", ts)
}

// UpsertLogClusterWithTemplate is the Drain-aware upsert. It stores the
// mined template tokens, the stable template ID, and a sample raw log.
// The older UpsertLogCluster is preserved for backward compatibility.
func (ss *SignalStore) UpsertLogClusterWithTemplate(id, template, severity, service string, templateID uint64, tokens []string, sample string, ts time.Time) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	lc, ok := ss.LogClusters[id]
	if !ok {
		lc = &LogClusterNode{
			ID:           id,
			Template:     template,
			TemplateID:   templateID,
			FirstSeen:    ts,
			LastSeen:     ts,
			SeverityDist: make(map[string]int64),
			SampleLog:    sample,
		}
		if tokens != nil {
			lc.TemplateTokens = append([]string(nil), tokens...)
		}
		ss.LogClusters[id] = lc
	} else {
		// Template may have generalized (merge in Drain): update tokens/id/string.
		if templateID != 0 {
			lc.TemplateID = templateID
		}
		if template != "" {
			lc.Template = template
		}
		if tokens != nil {
			lc.TemplateTokens = append(lc.TemplateTokens[:0], tokens...)
		}
	}
	lc.Count++
	lc.SeverityDist[severity]++
	if ts.After(lc.LastSeen) {
		lc.LastSeen = ts
	}

	// EMITTED_BY edge. Refresh UpdatedAt on every hit so Prune's edge sweep
	// never severs a cluster that is still receiving logs.
	ek := edgeKey(EdgeEmittedBy, id, service)
	if e, exists := ss.Edges[ek]; exists {
		if ts.After(e.UpdatedAt) {
			e.UpdatedAt = ts
		}
	} else {
		ss.Edges[ek] = &Edge{
			Type:      EdgeEmittedBy,
			FromID:    id,
			ToID:      service,
			UpdatedAt: ts,
		}
	}
}

func (ss *SignalStore) AddLoggedDuringEdge(clusterID, spanID string, ts time.Time) {
	ek := edgeKey(EdgeLoggedDuring, clusterID, spanID)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if e, exists := ss.Edges[ek]; exists {
		// Keep the correlation alive across Prune's edge sweep.
		if ts.After(e.UpdatedAt) {
			e.UpdatedAt = ts
		}
		return
	}
	ss.Edges[ek] = &Edge{
		Type:      EdgeLoggedDuring,
		FromID:    clusterID,
		ToID:      spanID,
		UpdatedAt: ts,
	}
}

func (ss *SignalStore) UpsertMetric(metricName, service string, value float64, ts time.Time) {
	key := metricName + "|" + service
	ss.mu.Lock()
	defer ss.mu.Unlock()

	m, ok := ss.Metrics[key]
	if !ok {
		m = &MetricNode{
			ID:         key,
			MetricName: metricName,
			Service:    service,
			RollingMin: value,
			RollingMax: value,
			RollingAvg: value,
			LastSeen:   ts,
		}
		ss.Metrics[key] = m
	}
	// MEASURED_BY edge — created on the first sample, UpdatedAt refreshed on
	// every sample so Prune's edge sweep never severs a live metric.
	ek := edgeKey(EdgeMeasuredBy, key, service)
	if e, exists := ss.Edges[ek]; exists {
		if ts.After(e.UpdatedAt) {
			e.UpdatedAt = ts
		}
	} else {
		ss.Edges[ek] = &Edge{
			Type:      EdgeMeasuredBy,
			FromID:    key,
			ToID:      service,
			UpdatedAt: ts,
		}
	}
	m.SampleCount++
	if value < m.RollingMin {
		m.RollingMin = value
	}
	if value > m.RollingMax {
		m.RollingMax = value
	}
	// Exponential moving average (alpha = 0.1)
	m.RollingAvg = m.RollingAvg*0.9 + value*0.1
	m.LastSeen = ts
}

func (ss *SignalStore) LogClustersForService(service string) []*LogClusterNode {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	var out []*LogClusterNode
	for _, e := range ss.Edges {
		if e.Type == EdgeEmittedBy && e.ToID == service {
			if lc, ok := ss.LogClusters[e.FromID]; ok {
				out = append(out, lc)
			}
		}
	}
	return out
}

// Prune bounds the SignalStore (modeled on TraceStore.Prune): MetricNodes and
// LogClusterNodes whose LastSeen predates cutoff are removed; if either map
// still exceeds its cap (maxMetrics / maxLogClusters; <=0 = uncapped) the
// oldest-LastSeen overflow is evicted. Each removed node takes its edges with
// it — metric MEASURED_BY edges inline, evicted-cluster EMITTED_BY /
// LOGGED_DURING edges in the final sweep — and any edge whose UpdatedAt
// predates cutoff is swept too (upsert paths refresh edge timestamps, so live
// correlations survive). Returns the number of nodes (metrics + clusters)
// removed.
func (ss *SignalStore) Prune(cutoff time.Time, maxMetrics, maxLogClusters int) int {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	pruned := 0
	for id, m := range ss.Metrics {
		if m.LastSeen.Before(cutoff) {
			delete(ss.Metrics, id)
			delete(ss.Edges, edgeKey(EdgeMeasuredBy, id, m.Service))
			pruned++
		}
	}
	if maxMetrics > 0 && len(ss.Metrics) > maxMetrics {
		type metricAge struct {
			id      string
			service string
			seen    time.Time
		}
		byAge := make([]metricAge, 0, len(ss.Metrics))
		for id, m := range ss.Metrics {
			byAge = append(byAge, metricAge{id, m.Service, m.LastSeen})
		}
		sort.Slice(byAge, func(i, j int) bool { return byAge[i].seen.Before(byAge[j].seen) })
		for _, e := range byAge[:len(byAge)-maxMetrics] {
			delete(ss.Metrics, e.id)
			delete(ss.Edges, edgeKey(EdgeMeasuredBy, e.id, e.service))
			pruned++
		}
	}
	// LogClusters: TTL + cap, mirroring the metric bound above. This map was
	// previously unbounded — keyed by service×Drain-template-ID with no cap, it
	// grew with every distinct (service, template-id) pair ever seen (template
	// IDs shift as Drain generalizes), leaking under a sustained high-shape log
	// stream. The TTL ages out that accretion; the cap is the backstop. Evicted
	// cluster IDs are collected so their EMITTED_BY / LOGGED_DURING edges
	// (FromID = cluster id) are swept below even when not yet time-stale.
	evictedClusters := make(map[string]struct{})
	for id, lc := range ss.LogClusters {
		if lc.LastSeen.Before(cutoff) {
			delete(ss.LogClusters, id)
			evictedClusters[id] = struct{}{}
			pruned++
		}
	}
	if maxLogClusters > 0 && len(ss.LogClusters) > maxLogClusters {
		type clusterAge struct {
			id   string
			seen time.Time
		}
		byAge := make([]clusterAge, 0, len(ss.LogClusters))
		for id, lc := range ss.LogClusters {
			byAge = append(byAge, clusterAge{id, lc.LastSeen})
		}
		sort.Slice(byAge, func(i, j int) bool { return byAge[i].seen.Before(byAge[j].seen) })
		for _, e := range byAge[:len(byAge)-maxLogClusters] {
			delete(ss.LogClusters, e.id)
			evictedClusters[e.id] = struct{}{}
			pruned++
		}
	}

	for ek, e := range ss.Edges {
		if _, fromEvicted := evictedClusters[e.FromID]; e.UpdatedAt.Before(cutoff) || fromEvicted {
			delete(ss.Edges, ek)
		}
	}
	return pruned
}

func (ss *SignalStore) MetricsForService(service string) []*MetricNode {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	var out []*MetricNode
	for _, m := range ss.Metrics {
		if m.Service == service {
			out = append(out, m)
		}
	}
	return out
}

// --- AnomalyStore methods ---

func (as *AnomalyStore) AddAnomaly(anomaly AnomalyNode) {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.Anomalies[anomaly.ID] = &anomaly

	// TRIGGERED_BY edge
	ek := edgeKey(EdgeTriggeredBy, anomaly.ID, anomaly.Service)
	as.Edges[ek] = &Edge{
		Type:      EdgeTriggeredBy,
		FromID:    anomaly.ID,
		ToID:      anomaly.Service,
		UpdatedAt: anomaly.Timestamp,
	}
}

func (as *AnomalyStore) AddPrecededByEdge(anomalyID, precedingID string, ts time.Time) {
	ek := edgeKey(EdgePrecededBy, anomalyID, precedingID)
	as.mu.Lock()
	defer as.mu.Unlock()
	as.Edges[ek] = &Edge{
		Type:      EdgePrecededBy,
		FromID:    anomalyID,
		ToID:      precedingID,
		UpdatedAt: ts,
	}
}

func (as *AnomalyStore) AnomaliesSince(since time.Time) []*AnomalyNode {
	return as.AnomaliesSinceLimit(since, 0)
}

// AnomaliesSinceLimit is AnomaliesSince with a result cap (n <= 0 means
// unlimited). correlateWithRecent walks this on every detection tick, so the
// cap keeps a pathological anomaly backlog from turning each tick into an
// O(N) scan plus O(N) edge fan-out. Selection past the cap follows map
// iteration order — correlation is best-effort by design.
func (as *AnomalyStore) AnomaliesSinceLimit(since time.Time, n int) []*AnomalyNode {
	as.mu.RLock()
	defer as.mu.RUnlock()
	var out []*AnomalyNode
	for _, a := range as.Anomalies {
		if a.Timestamp.After(since) {
			out = append(out, a)
			if n > 0 && len(out) >= n {
				break
			}
		}
	}
	return out
}

func (as *AnomalyStore) AnomaliesForService(service string, since time.Time) []*AnomalyNode {
	as.mu.RLock()
	defer as.mu.RUnlock()
	var out []*AnomalyNode
	for _, a := range as.Anomalies {
		if a.Service == service && a.Timestamp.After(since) {
			out = append(out, a)
		}
	}
	return out
}

// --- Health score helper ---

func computeHealth(errorRate, avgLatencyMs float64) float64 {
	latencyDev := math.Max(0, (avgLatencyMs-100)/100)
	score := 1.0 - (errorRate * 5) - (latencyDev * 0.1)
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score
}

// --- replacement-by-revision (aggregate mode) ---

// ReplaceTopology swaps the entire service topology for a new one under a
// single write lock.
//
// Aggregate mode consumes a per-revision snapshot from the aggregate engine
// rather than accumulating per-span upserts, so the only correct way to apply
// it is replacement: re-applying a cumulative snapshot through UpsertService /
// UpsertCallEdge would multiply every counter by the number of ticks it
// survived. Rebuilding the adjacency indexes here (rather than mutating them)
// is what keeps their append-only invariant intact — after the swap they
// describe exactly the maps they were built from.
func (s *ServiceStore) ReplaceTopology(services []*ServiceNode, operations []*OperationNode, edges []*Edge) {
	svcMap := make(map[string]*ServiceNode, len(services))
	for _, svc := range services {
		if svc == nil || svc.Name == "" {
			continue
		}
		svcMap[svc.Name] = svc
	}

	opMap := make(map[string]*OperationNode, len(operations))
	opsByService := make(map[string][]*OperationNode, len(svcMap))
	edgeMap := make(map[string]*Edge, len(edges)+len(operations))
	for _, op := range operations {
		if op == nil || op.Service == "" || op.Operation == "" {
			continue
		}
		opMap[op.ID] = op
		opsByService[op.Service] = append(opsByService[op.Service], op)
		ek := edgeKey(EdgeExposes, op.Service, op.ID)
		edgeMap[ek] = &Edge{Type: EdgeExposes, FromID: op.Service, ToID: op.ID, UpdatedAt: op.LastSeen}
	}

	edgesByFrom := make(map[string][]*Edge, len(svcMap))
	edgesByTo := make(map[string][]*Edge, len(svcMap))
	for _, e := range edges {
		if e == nil || e.Type != EdgeCalls || e.FromID == "" || e.ToID == "" {
			continue
		}
		edgeMap[edgeKey(EdgeCalls, e.FromID, e.ToID)] = e
		edgesByFrom[e.FromID] = append(edgesByFrom[e.FromID], e)
		edgesByTo[e.ToID] = append(edgesByTo[e.ToID], e)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.Services = svcMap
	s.Operations = opMap
	s.Edges = edgeMap
	s.edgesByFrom = edgesByFrom
	s.edgesByTo = edgesByTo
	s.opsByService = opsByService
}

// ReplaceMetrics swaps the metric nodes (and their MEASURED_BY edges) for a new
// set. Same reasoning as ReplaceTopology: aggregate-mode metric state is a
// projection of a revision, not an accumulation. Log-cluster nodes and their
// edges are untouched — they arrive as ingest-owned template facts on their own
// schedule.
func (ss *SignalStore) ReplaceMetrics(metrics []*MetricNode) {
	metricMap := make(map[string]*MetricNode, len(metrics))
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for ek, e := range ss.Edges {
		if e.Type == EdgeMeasuredBy {
			delete(ss.Edges, ek)
		}
	}
	for _, m := range metrics {
		if m == nil || m.ID == "" {
			continue
		}
		metricMap[m.ID] = m
		ek := edgeKey(EdgeMeasuredBy, m.ID, m.Service)
		ss.Edges[ek] = &Edge{Type: EdgeMeasuredBy, FromID: m.ID, ToID: m.Service, UpdatedAt: m.LastSeen}
	}
	ss.Metrics = metricMap
}
