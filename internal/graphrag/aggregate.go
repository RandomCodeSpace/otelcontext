package graphrag

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// Aggregate consumption (#174, decisions frozen in #163).
//
// In AGGREGATE_MODE=aggregate the in-memory service topology is no longer
// derived from a 60-second rescan of the spans table. It is REPLACED from the
// aggregate engine's per-tenant topology snapshot, keyed by revision:
//
//   - GraphRAG never queries the spans table (the ~27M-row/min scan at target
//     load dies here) and never opens aggregate.db;
//   - an unchanged revision is skipped outright;
//   - a changed epoch — the engine restarted — replaces rather than reconciles,
//     because the revision counter began again;
//   - nothing is ever re-applied through incrementing upserts, which is what
//     made the old rebuild multiply counters by the number of ticks a window
//     survived.
//
// What the refresh loop keeps in aggregate mode: pruning, tenant eviction,
// anomaly cleanup and investigation-cooldown maintenance. Nothing else.

// AggregateSource is the read side of the aggregate engine that GraphRAG
// consumes. It is an interface so the coordinator depends on the four calls it
// makes rather than on the engine's whole surface, and so tests can drive
// reconciliation without building a store.
type AggregateSource interface {
	// TopologyEpoch identifies the engine instance. A change means the
	// revision counter restarted.
	TopologyEpoch() uint64
	// TopologyTenants lists the tenants the projection holds topology for.
	TopologyTenants() []string
	// TopologyRevision reports a tenant's revision without rendering a
	// snapshot.
	TopologyRevision(tenant string) uint64
	// TopologySnapshot renders one tenant's topology.
	TopologySnapshot(tenant string) aggregate.TopologySnapshot
	// PruneTopology drops projection windows past the retention horizon.
	PruneTopology()
}

// AggregateMode reports whether this coordinator consumes aggregate snapshots
// instead of rebuilding from raw spans.
func (g *GraphRAG) AggregateMode() bool { return g.mode == aggregate.ModeAggregate }

// externalTemplates reports whether log templates are mined by the
// ingest-owned miner rather than by GraphRAG's Drain. True in shadow and
// aggregate modes so two template ID spaces never exist (#163).
func (g *GraphRAG) externalTemplates() bool { return g.mode != aggregate.ModeLegacy }

// SetAggregateSource wires the aggregate engine's topology projection. Main
// calls it before Start; the lock also keeps tests and alternate construction
// paths from racing the refresh loop.
func (g *GraphRAG) SetAggregateSource(src AggregateSource) {
	g.topologyMu.Lock()
	g.aggSource = src
	g.topologyMu.Unlock()
}

// reconcileTopology replaces each tenant's service topology from the aggregate
// snapshot whose revision it has not already applied. It is the aggregate-mode
// stand-in for rebuildAllTenantsFromDB and issues no database query at all.
func (g *GraphRAG) reconcileTopology() {
	if !g.AggregateMode() {
		return
	}
	g.topologyMu.Lock()
	defer g.topologyMu.Unlock()
	if g.aggSource == nil {
		return
	}
	// Expiry is itself a publishable replacement. Prune before reading the
	// identity so this read can apply the empty tombstone immediately.
	g.aggSource.PruneTopology()
	epoch := g.aggSource.TopologyEpoch()
	applied := 0
	for _, tenant := range g.aggSource.TopologyTenants() {
		if tenant == "" {
			tenant = storage.DefaultTenantID
		}
		rev := g.aggSource.TopologyRevision(tenant)
		// NoTouch: reconciliation is bookkeeping. It must not refresh the
		// idle-eviction clock, or a dormant tenant would never age out.
		stores := g.tenantStoresNoTouch(tenant)
		if stores.topoEpoch.Load() == epoch && stores.topoRevision.Load() == rev {
			continue
		}
		snap := g.aggSource.TopologySnapshot(tenant)
		applyTopologySnapshot(stores, snap)
		stores.topoEpoch.Store(snap.Epoch)
		stores.topoRevision.Store(snap.Revision)
		stores.lastTopology.Store(&snap)
		applied++
	}
	if applied > 0 {
		slog.Debug("GraphRAG reconciled aggregate topology", "tenants", applied, "epoch", epoch)
	}
}

// ensureTopologyCurrent makes the aggregate provider revision, rather than
// the 60-second cleanup tick, the freshness bound for a topology read.
func (g *GraphRAG) ensureTopologyCurrent() {
	if g.AggregateMode() {
		g.reconcileTopology()
	}
}

// applyTopologySnapshot projects one snapshot onto a tenant's stores. Every
// counter is an absolute value taken from the snapshot's retained windows, so
// applying the same snapshot twice yields the same state.
func applyTopologySnapshot(stores *tenantStores, snap aggregate.TopologySnapshot) {
	services := make([]*ServiceNode, 0, len(snap.Services))
	for _, svc := range snap.Services {
		services = append(services, serviceNodeFrom(svc))
	}
	operations := make([]*OperationNode, 0, len(snap.Operations))
	for _, op := range snap.Operations {
		operations = append(operations, operationNodeFrom(op))
	}
	edges := make([]*Edge, 0, len(snap.Edges))
	for _, e := range snap.Edges {
		edges = append(edges, callEdgeFrom(e))
	}
	stores.service.ReplaceTopology(services, operations, edges)

	metrics := make([]*MetricNode, 0, len(snap.Metrics))
	for _, m := range snap.Metrics {
		metrics = append(metrics, metricNodeFrom(m))
	}
	stores.signals.ReplaceMetrics(metrics)
}

// windowTotals sums a snapshot entity's retained windows into the absolute
// counters a graph node carries.
type windowTotals struct {
	count      uint64
	errors     uint64
	durCount   uint64
	durSumMs   float64
	p95Ms      float64
	p99Ms      float64
	valueCount uint64
	valueSum   float64
	valueMin   float64
	valueMax   float64
}

func totalsOf(windows []aggregate.TopologyWindow) windowTotals {
	var t windowTotals
	for i, w := range windows {
		t.count += w.Count
		t.errors += w.ErrorCount
		t.durCount += w.DurationCount
		t.durSumMs += w.DurationSumMicros / 1000.0
		t.valueCount += w.ValueCount
		t.valueSum += w.ValueSum
		if i == 0 || w.ValueMin < t.valueMin {
			t.valueMin = w.ValueMin
		}
		if i == 0 || w.ValueMax > t.valueMax {
			t.valueMax = w.ValueMax
		}
		// Percentiles do not sum. The most recent window that carries a
		// sketch is the current picture; older windows are baseline material
		// for the anomaly detector, not for the node's displayed latency.
		if w.P95Micros > 0 {
			t.p95Ms = w.P95Micros / 1000.0
		}
		if w.P99Micros > 0 {
			t.p99Ms = w.P99Micros / 1000.0
		}
	}
	return t
}

func (t windowTotals) errorRate() float64 {
	if t.count == 0 {
		return 0
	}
	return float64(t.errors) / float64(t.count)
}

func (t windowTotals) avgLatencyMs() float64 {
	if t.durCount == 0 {
		return 0
	}
	return t.durSumMs / float64(t.durCount)
}

func serviceNodeFrom(svc aggregate.TopologyService) *ServiceNode {
	t := totalsOf(svc.Windows)
	node := &ServiceNode{
		ID:         svc.Name,
		Name:       svc.Name,
		FirstSeen:  svc.FirstSeen,
		LastSeen:   svc.LastSeen,
		CallCount:  int64(t.count), // #nosec G115 -- counts are bounded by ingest volume
		ErrorCount: int64(t.errors),
		TotalMs:    t.durSumMs,
	}
	node.ErrorRate = t.errorRate()
	node.AvgLatency = t.avgLatencyMs()
	node.HealthScore = computeHealth(node.ErrorRate, node.AvgLatency)
	return node
}

func operationNodeFrom(op aggregate.TopologyOperation) *OperationNode {
	t := totalsOf(op.Windows)
	node := &OperationNode{
		ID:         op.Service + "|" + op.Operation,
		Service:    op.Service,
		Operation:  op.Operation,
		FirstSeen:  op.FirstSeen,
		LastSeen:   op.LastSeen,
		CallCount:  int64(t.count), // #nosec G115 -- counts are bounded by ingest volume
		ErrorCount: int64(t.errors),
		TotalMs:    t.durSumMs,
	}
	node.ErrorRate = t.errorRate()
	node.AvgLatency = t.avgLatencyMs()
	node.HealthScore = computeHealth(node.ErrorRate, node.AvgLatency)
	return node
}

func callEdgeFrom(e aggregate.SnapshotEdge) *Edge {
	t := totalsOf(e.Windows)
	edge := &Edge{
		Type:       EdgeCalls,
		FromID:     e.Caller,
		ToID:       e.Callee,
		CallCount:  int64(t.count), // #nosec G115 -- counts are bounded by ingest volume
		ErrorCount: int64(t.errors),
		TotalMs:    t.durSumMs,
		UpdatedAt:  e.LastSeen,
	}
	edge.ErrorRate = t.errorRate()
	edge.AvgMs = t.avgLatencyMs()
	edge.Weight = float64(edge.CallCount)
	return edge
}

func metricNodeFrom(m aggregate.TopologyMetric) *MetricNode {
	t := totalsOf(m.Windows)
	node := &MetricNode{
		ID:          m.Metric + "|" + m.Service,
		MetricName:  m.Metric,
		Service:     m.Service,
		RollingMin:  t.valueMin,
		RollingMax:  t.valueMax,
		SampleCount: int64(t.valueCount), // #nosec G115 -- counts are bounded by ingest volume
		LastSeen:    m.LastSeen,
	}
	if t.valueCount > 0 {
		node.RollingAvg = t.valueSum / float64(t.valueCount)
	}
	return node
}

// OnTemplateFact consumes one ingest-mined log template fact. It is the
// aggregate/shadow replacement for GraphRAG's own Drain miner: the template ID
// is minted once, on the ingest path, so a template never has two identities
// (#163).
//
// The miner calls this SYNCHRONOUSLY on the OTLP goroutine, once per log line,
// so it must not take a store lock. It enqueues onto the same best-effort
// channel every other ingest callback uses and returns; a full channel drops
// the fact, exactly as a full channel drops a span.
func (g *GraphRAG) OnTemplateFact(fact aggregate.TemplateFact) {
	if g == nil || fact.Service == "" {
		return
	}
	if fact.Tenant == "" {
		fact.Tenant = storage.DefaultTenantID
	}
	if fact.Timestamp.IsZero() {
		fact.Timestamp = time.Now()
	}
	g.enqueueEvent(event{tmpl: &fact}, "log")
}

// processTemplateFact folds one template fact into its tenant's log clusters.
// Runs on an event worker, never on the OTLP goroutine.
func (g *GraphRAG) processTemplateFact(fact *aggregate.TemplateFact) {
	stores := g.storesForTenant(fact.Tenant)
	stores.lastEventAt.Store(time.Now().UnixNano())
	clusterID := fmt.Sprintf("lc_%s_%x", fact.Service, fact.TemplateID)
	stores.signals.UpsertLogClusterWithTemplate(
		clusterID,
		fact.Template,
		fact.Severity,
		fact.Service,
		uint64(fact.TemplateID),
		nil,
		"",
		fact.Timestamp,
	)
}
