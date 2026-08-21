package aggregate

import (
	"container/list"
	"hash/fnv"
	"sync"
)

// Caller resolution for service-edge series (#174, deferred from #183).
//
// #183 shipped SignalServiceEdge — its sub-cap, its overflow path and its
// encoding — but emitted nothing into it, because a single span does not know
// who called it. The caller is recovered the only way it can be: by joining a
// child span's ParentSpanID against the service of the parent span, which
// arrives in a different Export request from a different process.
//
// The join needs a bounded memory of recently-seen spans. Spans of one trace
// arrive close together, so a per-tenant LRU of recent span IDs covers
// in-flight traces while bounding memory by recent span volume rather than by
// total spans seen. This is the same shape as the legacy graphrag topology
// observer it replaces in aggregate mode — with two differences: it is sharded
// by span ID so 100+ services do not funnel through one mutex, and it holds
// only the mapping, never a graph.
//
// The resolver is consulted for EVERY received span, before the exemplar
// policy, because an edge must exist whether or not the spans forming it were
// retained as raw exemplars.

// DefaultEdgeResolverSpans bounds the span-ID memory across all shards. Each
// entry is two short strings plus map and list overhead (~0.1 KiB), so the
// default is a few tens of MiB at worst.
const DefaultEdgeResolverSpans = 100_000

// edgeResolverShards is the shard count. Fixed and small: the map is touched
// once per span, and every operation holds exactly one shard lock.
const edgeResolverShards = 16

// EdgeResolver recovers the caller service of a span from its parent span ID.
// Safe for concurrent use.
type EdgeResolver struct {
	shards [edgeResolverShards]edgeShard
}

type edgeShard struct {
	mu    sync.Mutex
	cap   int
	byID  map[edgeSpanKey]string
	order *list.List
	elems map[edgeSpanKey]*list.Element
}

// edgeSpanKey is (tenant, span ID). Span IDs are unique within a tenant; the
// tenant is part of the key so one tenant's span can never resolve another
// tenant's caller.
type edgeSpanKey struct {
	tenant string
	spanID string
}

// NewEdgeResolver builds a resolver whose total span memory is capped at
// maxSpans across all shards. Zero or negative takes DefaultEdgeResolverSpans.
func NewEdgeResolver(maxSpans int) *EdgeResolver {
	if maxSpans <= 0 {
		maxSpans = DefaultEdgeResolverSpans
	}
	per := maxSpans / edgeResolverShards
	if per < 1 {
		per = 1
	}
	r := &EdgeResolver{}
	for i := range r.shards {
		r.shards[i] = edgeShard{
			cap:   per,
			byID:  make(map[edgeSpanKey]string),
			order: list.New(),
			elems: make(map[edgeSpanKey]*list.Element),
		}
	}
	return r
}

// Observe records this span's service and resolves its caller.
//
// It returns the parent's service and true only when the parent span is still
// remembered AND belongs to a different service — a same-service parent is an
// internal call, not a topology edge. Recording and lookup touch different
// shards and never hold two locks at once.
func (r *EdgeResolver) Observe(tenant, spanID, parentSpanID, service string) (string, bool) {
	if r == nil || spanID == "" || service == "" {
		return "", false
	}
	r.put(edgeSpanKey{tenant, spanID}, service)
	if parentSpanID == "" {
		return "", false
	}
	caller, ok := r.get(edgeSpanKey{tenant, parentSpanID})
	if !ok || caller == service {
		return "", false
	}
	return caller, true
}

// Len reports how many span mappings are currently held. Test and diagnostic
// accessor only.
func (r *EdgeResolver) Len() int {
	n := 0
	for i := range r.shards {
		sh := &r.shards[i]
		sh.mu.Lock()
		n += len(sh.byID)
		sh.mu.Unlock()
	}
	return n
}

func (r *EdgeResolver) shardFor(key edgeSpanKey) *edgeShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key.spanID))
	return &r.shards[h.Sum32()%edgeResolverShards]
}

func (r *EdgeResolver) put(key edgeSpanKey, service string) {
	sh := r.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if el, ok := sh.elems[key]; ok {
		sh.byID[key] = service
		sh.order.MoveToFront(el)
		return
	}
	sh.byID[key] = service
	sh.elems[key] = sh.order.PushFront(key)
	for len(sh.byID) > sh.cap {
		back := sh.order.Back()
		if back == nil {
			return
		}
		victim, _ := back.Value.(edgeSpanKey)
		sh.order.Remove(back)
		delete(sh.elems, victim)
		delete(sh.byID, victim)
	}
}

func (r *EdgeResolver) get(key edgeSpanKey) (string, bool) {
	sh := r.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	service, ok := sh.byID[key]
	if ok {
		if el, found := sh.elems[key]; found {
			sh.order.MoveToFront(el)
		}
	}
	return service, ok
}
