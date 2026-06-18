package graphrag

import (
	"container/list"
	"sync"
	"time"
)

// topologyObserverSpanCap bounds the per-tenant spanID→service LRU. Cross-service
// edges are inferred by joining a child span's ParentSpanID to its parent's
// service, so we must remember recently-seen span→service mappings long enough
// for the child to arrive. Spans within a trace arrive close together, so a
// window of ~100k recent spans per tenant comfortably covers in-flight traces
// while bounding memory: each entry is two short strings (~0.1 KB incl. map and
// list overhead), so the cap is ≲ a few tens of MB per tenant worst case.
//
// This is the LRU pattern already used by the Drain miner (drain.go): a
// container/list for O(1) recency + a map for O(1) lookup, evicting from the
// back when over capacity.
const topologyObserverSpanCap = 100_000

// topologyObserver records cross-service CALLS edges INDEPENDENT of the trace
// sampler's keep/drop decision. The OTLP receiver invokes ObserveSpanTopology
// for every received span BEFORE the sampler runs; without this, an edge is
// only ever formed when BOTH the parent and child spans survive sampling — at
// low sample rates the joint survival of a pair is vanishingly small, so the
// in-memory service map shows nodes but almost no edges (no flow direction).
//
// It is deliberately lightweight and strictly bounded:
//   - a per-tenant LRU map[spanID]serviceName (cap topologyObserverSpanCap)
//     so memory is bounded by recent span volume, not total spans seen;
//   - a per-tenant set of already-emitted (source→target) pairs so repeated
//     observations of the same edge do no work and the set is bounded by the
//     number of distinct service-pairs (~services²), not span count.
//
// Edge existence is guaranteed via ServiceStore.EnsureCallEdge, which creates
// the edge with zeroed aggregates if absent and is a no-op otherwise. Edge
// aggregates (CallCount/latency/error-rate) remain exclusively owned by the
// sampled path (processSpan → UpsertCallEdge) and the 60s DB rebuild, so this
// observer never pollutes them.
//
// Each topologyObserver belongs to exactly one tenantStores slice, so its data
// is tenant-isolated by construction and inherits the slice's idle eviction.
type topologyObserver struct {
	mu sync.Mutex

	// spanService is a bounded LRU of spanID → serviceName for recently seen
	// spans. order tracks recency (front = most recent); elems maps spanID to
	// its list element for O(1) move/remove. The list element's Value is the
	// spanID string.
	spanService map[string]string
	order       *list.List
	elems       map[string]*list.Element

	// emittedPairs deduplicates (source→target) so observing a hot edge
	// thousands of times costs one EnsureCallEdge and one map write total.
	emittedPairs map[string]struct{}

	// emittedServices deduplicates service-node creation so the ServiceStore
	// write lock is taken once per new service, not once per span. Bounded by
	// the number of distinct services in the tenant.
	emittedServices map[string]struct{}

	cap int
}

func newTopologyObserver() *topologyObserver {
	return &topologyObserver{
		spanService:     make(map[string]string),
		order:           list.New(),
		elems:           make(map[string]*list.Element),
		emittedPairs:    make(map[string]struct{}),
		emittedServices: make(map[string]struct{}),
		cap:             topologyObserverSpanCap,
	}
}

// observe records span→service and, when the span's parent is a known span in a
// DIFFERENT service, ensures the parentService→service CALLS edge exists in the
// supplied ServiceStore. svc must be non-empty (the caller guarantees this).
func (o *topologyObserver) observe(svcStore *ServiceStore, spanID, parentSpanID, service string) {
	if spanID == "" || service == "" {
		return
	}

	o.mu.Lock()
	// Record/refresh this span's service mapping (LRU touch).
	o.put(spanID, service)

	// Decide what to materialize while holding the lock so all map reads/writes
	// are race-free; apply to the ServiceStore after releasing it. We dedup both
	// service-node and edge creation so steady-state observation does no work.
	ensureChild := o.markServiceOnce(service)
	var ensureSource bool
	var emitSource string
	if parentSpanID != "" {
		if parentService, ok := o.spanService[parentSpanID]; ok && parentService != service {
			o.touch(parentSpanID) // parent is still active in this trace
			pairKey := parentService + "\x00" + service
			if _, done := o.emittedPairs[pairKey]; !done {
				o.emittedPairs[pairKey] = struct{}{}
				emitSource = parentService
				ensureSource = o.markServiceOnce(parentService)
			}
		}
	}
	o.mu.Unlock()

	// Existence-only writes: create nodes/edge if absent, no-op otherwise, never
	// touching sampled aggregates.
	now := time.Now()
	if ensureChild {
		svcStore.EnsureService(service, now)
	}
	if ensureSource {
		svcStore.EnsureService(emitSource, now)
	}
	if emitSource != "" {
		svcStore.EnsureCallEdge(emitSource, service, now)
	}
}

// markServiceOnce returns true the first time it sees a service name (recording
// it), false thereafter. Caller holds o.mu. Lets the caller take the ServiceStore
// write lock only once per distinct service rather than once per span.
func (o *topologyObserver) markServiceOnce(service string) bool {
	if _, ok := o.emittedServices[service]; ok {
		return false
	}
	o.emittedServices[service] = struct{}{}
	return true
}

// put inserts or refreshes spanID→service and enforces the LRU cap. Caller holds o.mu.
func (o *topologyObserver) put(spanID, service string) {
	if el, ok := o.elems[spanID]; ok {
		o.spanService[spanID] = service
		o.order.MoveToFront(el)
		return
	}
	o.spanService[spanID] = service
	o.elems[spanID] = o.order.PushFront(spanID)
	o.evictIfNeeded()
}

// touch marks spanID as most-recently used without changing its mapping. Caller holds o.mu.
func (o *topologyObserver) touch(spanID string) {
	if el, ok := o.elems[spanID]; ok {
		o.order.MoveToFront(el)
	}
}

// evictIfNeeded drops least-recently-used span mappings past the cap. The
// emittedPairs set is intentionally NOT pruned here — it is independently
// bounded by the number of distinct service-pairs and an evicted span does not
// invalidate an already-recorded edge. Caller holds o.mu.
func (o *topologyObserver) evictIfNeeded() {
	for o.cap > 0 && len(o.spanService) > o.cap {
		back := o.order.Back()
		if back == nil {
			return
		}
		victim, _ := back.Value.(string)
		o.order.Remove(back)
		delete(o.elems, victim)
		delete(o.spanService, victim)
	}
}

// len reports the current LRU size. Exported via GraphRAG.topologyObserverLen
// for tests.
func (o *topologyObserver) len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.spanService)
}

// ObserveSpanTopology records one received span's cross-service call topology
// INDEPENDENT of the sampler. The OTLP trace receiver calls this for every span
// BEFORE the sampler's keep/drop decision, so the service map retains flow
// direction even when sampling drops the spans that would have formed the edge.
//
// tenant is the resolved tenant (empty collapses to DefaultTenantID, matching
// the rest of GraphRAG). traceID is accepted for symmetry with the event path
// and possible future per-trace scoping; the current implementation keys solely
// on span IDs, which are globally unique within a tenant. Safe to call from many
// goroutines.
func (g *GraphRAG) ObserveSpanTopology(tenant, traceID, spanID, parentSpanID, service string) {
	if service == "" || spanID == "" {
		return
	}
	stores := g.storesForTenant(tenant)
	stores.lastEventAt.Store(time.Now().UnixNano())
	stores.topology.observe(stores.service, spanID, parentSpanID, service)
}

// topologyObserverLen returns the per-tenant topology observer's current LRU
// size. Test-only accessor.
func (g *GraphRAG) topologyObserverLen(tenant string) int {
	return g.storesForTenant(tenant).topology.len()
}
