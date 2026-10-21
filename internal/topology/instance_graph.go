package topology

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	GraphWindow        = 5 * time.Minute
	GraphFlushInterval = 5 * time.Second
	graphMaxEntities   = 20000 // nodes and directed pairs, across all tenants
	graphMaxPending    = 20000 // distinct dirty entity/window pairs
	graphMaxSpans      = 100000
	graphMaxChildren   = 256
	graphSpanTTL       = 5 * time.Minute
	// At the entity bound, the least recently seen flushed entities idle for
	// graphEvictMinIdle are forgotten in memory (rows and buckets stay in the
	// database) so new instances and pairs are not refused for the whole
	// retention period.
	graphEvictBatch   = graphMaxEntities / 10
	graphEvictMinIdle = 15 * time.Minute
)

// Loss reasons, reported per reason in the graph metadata and on
// otelcontext_instance_graph_lost_total{kind,reason}.
const (
	DropInvalidIdentity = "invalid_identity" // empty tenant/service or over-length value
	DropOutOfWindow     = "out_of_window"    // older than retention or more than 2m in the future
	DropUnknownEntity   = "unknown_entity"   // span for a resource that was never admitted
	DropInvalidSpan     = "invalid_span"     // ID length or duration out of range
	DropEntityCap       = "entity_cap"       // graphMaxEntities reached
	DropChildrenCap     = "children_cap"     // graphMaxChildren waiting on one parent
	DropPendingCap      = "pending_cap"      // graphMaxPending unflushed buckets
	UnmatchedExpired    = "expired"          // parent did not arrive within graphSpanTTL
	UnmatchedEvicted    = "evicted"          // span buffer full; child evicted before its parent arrived
	UnmatchedEndpoint   = "unknown_endpoint" // an endpoint entity was pruned before the join
)

// InstanceIdentity never derives environment from a host name. Explicit
// instance IDs are scoped by tenant, environment, namespace and service.
// Host/workload is a fallback; host alone is explicitly ambiguous.
type InstanceIdentity struct {
	Tenant, Environment, Namespace, Service, Instance, Host, Workload string
}

func identityHash(parts ...string) string {
	b, _ := json.Marshal(parts) // length-delimited, including embedded NULs
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (i InstanceIdentity) entity(at time.Time) (storage.TopologyEntity, bool) {
	for _, s := range []string{i.Tenant, i.Environment, i.Namespace, i.Service, i.Instance, i.Host, i.Workload} {
		if len(s) > RegistryMaxValueBytes {
			return storage.TopologyEntity{}, false
		}
	}
	if i.Tenant == "" || i.Service == "" {
		return storage.TopologyEntity{}, false
	}
	quality, instanceKey := "instance_id", i.Instance
	switch {
	case i.Instance != "":
	case i.Host != "" && i.Workload != "":
		quality, instanceKey = "host_workload", identityHash(i.Host, i.Workload)
	case i.Host != "":
		quality, instanceKey = "host_only", i.Host
	default:
		quality, instanceKey = "unresolved", i.Workload
	}
	return storage.TopologyEntity{
		ID:       identityHash("node", i.Tenant, i.Environment, i.Namespace, i.Service, quality, instanceKey),
		TenantID: i.Tenant, Environment: i.Environment, Namespace: i.Namespace,
		Service: i.Service, Instance: i.Instance, Host: i.Host, Workload: i.Workload,
		Identity: quality, FirstSeen: at.Unix(), LastSeen: at.Unix(),
	}, true
}

type graphEntity struct {
	row      storage.TopologyEntity
	revision uint64
	dirty    bool
}

type graphBucketKey struct {
	entity string
	start  int64
}

type graphSpanKey struct{ tenant, trace, span string }
type graphSpan struct {
	key, parent graphSpanKey
	entity      string
	at          time.Time
	arrival     time.Time
	micros      int64
	failed      bool
}

// InstanceGraph is a best-effort, pre-sampling visualization projection.
// A five-second dirty tick persists summaries; raw ingestion ACKs are not
// changed. A crash can lose the unflushed interval. Correlation is bounded and
// in-memory, with no raw trace/span IDs persisted by this projection.
type InstanceGraph struct {
	db        *gorm.DB
	retention time.Duration
	now       func() time.Time
	gate      chan struct{} // context-aware serialization of snapshot/flush
	mu        sync.Mutex
	entities  map[string]*graphEntity
	pending   map[graphBucketKey]storage.TopologyBucket
	spans     map[graphSpanKey]*list.Element
	order     *list.List
	waiting   map[graphSpanKey]map[graphSpanKey]struct{}
	dropped   uint64
	unmatched uint64
	drops     map[string]uint64
	misses    map[string]uint64
	lastLoss  time.Time
	lastEvict time.Time
	lastFlush time.Time
	flushErr  bool
	metrics   *telemetry.Metrics
}

func NewInstanceGraph(ctx context.Context, db *gorm.DB, retention time.Duration) (*InstanceGraph, error) {
	if retention <= 0 {
		return nil, errors.New("topology retention must be positive")
	}
	g := &InstanceGraph{
		db: db, retention: retention, now: time.Now, gate: make(chan struct{}, 1),
		entities: make(map[string]*graphEntity), pending: make(map[graphBucketKey]storage.TopologyBucket),
		spans: make(map[graphSpanKey]*list.Element), order: list.New(),
		waiting: make(map[graphSpanKey]map[graphSpanKey]struct{}),
		drops:   make(map[string]uint64), misses: make(map[string]uint64),
	}
	// Eviction leaves more retained rows than the bound; reload the most
	// recently seen ones.
	var rows []storage.TopologyEntity
	if err := db.WithContext(ctx).Where("last_seen >= ?", time.Now().Add(-retention).Unix()).
		Order("last_seen DESC, id").Limit(graphMaxEntities).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load instance topology: %w", err)
	}
	for _, row := range rows {
		g.entities[row.ID] = &graphEntity{row: row}
	}
	return g, nil
}

// SetMetrics wires loss counters and size gauges. Call before ingestion starts.
func (g *InstanceGraph) SetMetrics(m *telemetry.Metrics) {
	if g != nil {
		g.metrics = m
	}
}

// Caller holds mu.
func (g *InstanceGraph) drop(reason string) {
	g.dropped++
	g.drops[reason]++
	g.lastLoss = g.now()
	g.metrics.RecordInstanceGraphLoss("dropped", reason)
}

// Caller holds mu.
func (g *InstanceGraph) miss(reason string) {
	g.unmatched++
	g.misses[reason]++
	g.lastLoss = g.now()
	g.metrics.RecordInstanceGraphLoss("unmatched", reason)
}

func (g *InstanceGraph) acquire(ctx context.Context) error {
	select {
	case g.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Register is called once per resource batch for all three signals. It records
// presence without inventing trace counts for metrics-only/logs-only resources.
func (g *InstanceGraph) Register(i InstanceIdentity, signal Signal, at time.Time) string {
	if g == nil {
		return ""
	}
	row, ok := i.entity(at)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !ok {
		g.drop(DropInvalidIdentity)
		return ""
	}
	if at.Before(g.now().Add(-g.retention)) || at.After(g.now().Add(2*time.Minute)) {
		g.drop(DropOutOfWindow)
		return ""
	}
	row.Signals = int64(signal)
	if !g.admit(row) {
		return ""
	}
	return row.ID
}

// Caller holds mu.
func (g *InstanceGraph) admit(row storage.TopologyEntity) bool {
	if e := g.entities[row.ID]; e != nil {
		if row.FirstSeen < e.row.FirstSeen {
			e.row.FirstSeen = row.FirstSeen
		}
		if row.LastSeen > e.row.LastSeen {
			e.row.LastSeen = row.LastSeen
		}
		// An explicit instance may move hosts. Retain the most recent
		// reported placement; this is not a historical placement timeline.
		if row.LastSeen >= e.row.LastSeen && row.Source == "" {
			e.row.Host, e.row.Workload = row.Host, row.Workload
		}
		e.row.Signals |= row.Signals
		e.dirty, e.revision = true, e.revision+1
		return true
	}
	if len(g.entities) >= graphMaxEntities {
		g.evictStale()
	}
	if len(g.entities) >= graphMaxEntities {
		g.drop(DropEntityCap)
		return false
	}
	g.entities[row.ID] = &graphEntity{row: row, revision: 1, dirty: true}
	return true
}

// evictStale forgets up to graphEvictBatch flushed entities idle for at least
// graphEvictMinIdle, least recently seen first. It scans at most once per
// second, so a live set that genuinely exceeds the bound costs one scan per
// second rather than one per refused admission. Caller holds mu.
func (g *InstanceGraph) evictStale() {
	now := g.now()
	if now.Sub(g.lastEvict) < time.Second {
		return
	}
	g.lastEvict = now
	cutoff := now.Add(-graphEvictMinIdle).Unix()
	var stale []*graphEntity
	for _, e := range g.entities {
		if !e.dirty && e.row.LastSeen < cutoff {
			stale = append(stale, e)
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].row.LastSeen < stale[j].row.LastSeen })
	for _, e := range stale[:min(len(stale), graphEvictBatch)] {
		delete(g.entities, e.row.ID)
	}
}

// Observe records one successfully accepted span before exemplar filtering.
// Both arrival orders work, including calls between copies of the same
// service. A parent in a different trace or tenant can never create an edge.
func (g *InstanceGraph) Observe(entity, traceID, spanID, parentID string, at time.Time, micros int64, request, failed bool) {
	if g == nil || entity == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entities[entity]
	now := g.now()
	switch {
	case e == nil:
		g.drop(DropUnknownEntity)
		return
	case len(traceID) != 32 || len(spanID) != 16 || (parentID != "" && len(parentID) != 16) ||
		micros < 0 || micros > int64((24*time.Hour)/time.Microsecond):
		g.drop(DropInvalidSpan)
		return
	case at.Before(now.Add(-g.retention)) || at.After(now.Add(2*time.Minute)):
		g.drop(DropOutOfWindow)
		return
	}
	key := graphSpanKey{e.row.TenantID, traceID, spanID}
	g.expireSpans(now)
	if _, seen := g.spans[key]; seen {
		return
	}
	s := graphSpan{key: key, parent: graphSpanKey{key.tenant, traceID, parentID}, entity: entity, at: at, arrival: now, micros: micros, failed: failed}
	g.spans[key] = g.order.PushFront(s)
	if at.Unix() < e.row.FirstSeen {
		e.row.FirstSeen, e.dirty, e.revision = at.Unix(), true, e.revision+1
	}
	b := storage.TopologyBucket{Spans: 1}
	if request {
		b.Requests, b.DurationMicros = 1, micros
		if failed {
			b.Errors = 1
		}
	}
	g.add(entity, at, b)
	if parentID != "" && parentID != spanID {
		if parent := g.spans[s.parent]; parent != nil {
			g.connect(parent.Value.(graphSpan), s)
		} else {
			pending := g.waiting[s.parent]
			if len(pending) >= graphMaxChildren {
				g.drop(DropChildrenCap)
			} else {
				if pending == nil {
					pending = make(map[graphSpanKey]struct{})
					g.waiting[s.parent] = pending
				}
				pending[key] = struct{}{}
			}
		}
	}
	for child := range g.waiting[key] {
		if el := g.spans[child]; el != nil {
			g.connect(s, el.Value.(graphSpan))
		}
	}
	delete(g.waiting, key)
	for len(g.spans) > graphMaxSpans {
		g.removeOldestSpan(UnmatchedEvicted)
	}
}

func (g *InstanceGraph) expireSpans(now time.Time) {
	for g.order.Len() > 0 && now.Sub(g.order.Back().Value.(graphSpan).arrival) > graphSpanTTL {
		g.removeOldestSpan(UnmatchedExpired)
	}
}

// removeOldestSpan counts reason only when the span was a child still
// waiting for its parent; a matched or root span leaves silently.
func (g *InstanceGraph) removeOldestSpan(reason string) {
	el := g.order.Back()
	s := el.Value.(graphSpan)
	if pending := g.waiting[s.parent]; pending != nil {
		if _, exists := pending[s.key]; exists {
			g.miss(reason)
		}
		delete(pending, s.key)
		if len(pending) == 0 {
			delete(g.waiting, s.parent)
		}
	}
	delete(g.spans, s.key)
	g.order.Remove(el)
}

func (g *InstanceGraph) connect(parent, child graphSpan) {
	if parent.entity == child.entity {
		return
	}
	source, target := g.entities[parent.entity], g.entities[child.entity]
	if source == nil || target == nil {
		g.miss(UnmatchedEndpoint)
		return
	}
	// Keep endpoint metadata for as long as a retained call references it.
	// This is presence in the observed graph, not a host placement timeline.
	for _, endpoint := range []*graphEntity{source, target} {
		if child.at.Unix() > endpoint.row.LastSeen {
			endpoint.row.LastSeen = child.at.Unix()
			endpoint.dirty, endpoint.revision = true, endpoint.revision+1
		}
		if child.at.Unix() < endpoint.row.FirstSeen {
			endpoint.row.FirstSeen = child.at.Unix()
			endpoint.dirty, endpoint.revision = true, endpoint.revision+1
		}
	}
	row := storage.TopologyEntity{
		ID: identityHash("edge", parent.entity, child.entity), TenantID: child.key.tenant,
		Environment: target.row.Environment, Source: parent.entity, Target: child.entity,
		FirstSeen: child.at.Unix(), LastSeen: child.at.Unix(), Signals: int64(SignalTraces),
	}
	if !g.admit(row) {
		return
	}
	b := storage.TopologyBucket{Requests: 1, DurationMicros: child.micros}
	if child.failed {
		b.Errors = 1
	}
	g.add(row.ID, child.at, b)
}

func (g *InstanceGraph) add(id string, at time.Time, delta storage.TopologyBucket) {
	key := graphBucketKey{id, at.Truncate(GraphWindow).Unix()}
	b, exists := g.pending[key]
	if !exists && len(g.pending) >= graphMaxPending {
		g.drop(DropPendingCap)
		return
	}
	b.EntityID, b.WindowStart = id, key.start
	b.Spans += delta.Spans
	b.Requests += delta.Requests
	b.Errors += delta.Errors
	b.DurationMicros += delta.DurationMicros
	g.pending[key] = b
}

// Flush snapshots dirty values without holding the ingestion mutex during I/O.
// A failed transaction keeps the original pending values. Successful commit
// subtracts only that snapshot; concurrent observations remain pending.
func (g *InstanceGraph) Flush(ctx context.Context) error {
	if err := g.acquire(ctx); err != nil {
		return err
	}
	defer func() { <-g.gate }()
	g.mu.Lock()
	rows := make([]storage.TopologyEntity, 0)
	revisions := make(map[string]uint64)
	for id, e := range g.entities {
		if e.dirty {
			rows = append(rows, e.row)
			revisions[id] = e.revision
		}
	}
	buckets := make([]storage.TopologyBucket, 0, len(g.pending))
	for _, b := range g.pending {
		buckets = append(buckets, b)
	}
	g.metrics.SetInstanceGraphSize(len(g.spans), len(g.waiting), len(g.entities), len(g.pending))
	g.mu.Unlock()
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(rows) > 0 {
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}},
				DoUpdates: clause.AssignmentColumns([]string{"host", "workload", "first_seen", "last_seen", "signals"}),
			}).CreateInBatches(rows, 100).Error; err != nil {
				return err
			}
		}
		if len(buckets) == 0 {
			return nil
		}
		updates := make(map[string]any)
		for _, col := range []string{"spans", "requests", "errors", "duration_micros"} {
			updates[col] = gorm.Expr("topology_buckets." + col + " + excluded." + col)
		}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "entity_id"}, {Name: "window_start"}},
			DoUpdates: clause.Assignments(updates),
		}).CreateInBatches(buckets, 100).Error
	})
	g.mu.Lock()
	defer g.mu.Unlock()
	g.flushErr = err != nil
	if err != nil {
		return err
	}
	for id, revision := range revisions {
		if e := g.entities[id]; e != nil && e.revision == revision {
			e.dirty = false
		}
	}
	for _, b := range buckets {
		key := graphBucketKey{b.EntityID, b.WindowStart}
		left := g.pending[key]
		left.Spans -= b.Spans
		left.Requests -= b.Requests
		left.Errors -= b.Errors
		left.DurationMicros -= b.DurationMicros
		if left.Spans == 0 && left.Requests == 0 {
			delete(g.pending, key)
		} else {
			g.pending[key] = left
		}
	}
	g.lastFlush = g.now()
	return nil
}

// Prune removes at most 1000 rows per tick. Query visibility always clamps to
// retention even when physical cleanup is behind.
func (g *InstanceGraph) Prune(ctx context.Context) error {
	if err := g.acquire(ctx); err != nil {
		return err
	}
	defer func() { <-g.gate }()
	cutoff := g.now().Add(-g.retention).Truncate(GraphWindow).Unix()
	var old []storage.TopologyBucket
	if err := g.db.WithContext(ctx).Select("entity_id", "window_start").Where("window_start < ?", cutoff).
		Order("window_start").Limit(1000).Find(&old).Error; err != nil {
		return err
	}
	if len(old) > 0 {
		return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for _, b := range old {
				if err := tx.Where("entity_id = ? AND window_start = ?", b.EntityID, b.WindowStart).Delete(&storage.TopologyBucket{}).Error; err != nil {
					return err
				}
			}
			return nil
		})
	}
	// Also collect expired rows excluded by startup Load. A concurrent
	// registration remains dirty and will recreate its row on the next flush.
	// Never hold the ingestion mutex while waiting for the database.
	var ids []string
	if err := g.db.WithContext(ctx).Model(&storage.TopologyEntity{}).
		Where("last_seen < ?", cutoff).Order("last_seen").Limit(100).Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) > 0 {
		if err := g.db.WithContext(ctx).Where("id IN ? AND last_seen < ?", ids, cutoff).Delete(&storage.TopologyEntity{}).Error; err != nil {
			return err
		}
		g.mu.Lock()
		for _, id := range ids {
			if e := g.entities[id]; e != nil && e.row.LastSeen < cutoff && !e.dirty {
				delete(g.entities, id)
			}
		}
		g.mu.Unlock()
	}
	g.mu.Lock()
	g.expireSpans(g.now())
	g.mu.Unlock()
	return nil
}

func (g *InstanceGraph) Run(ctx context.Context) {
	ticker := time.NewTicker(GraphFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			work, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := g.Flush(work); err != nil && ctx.Err() == nil {
				slog.Warn("instance topology flush failed", "error", err)
			} else if ctx.Err() == nil {
				if err := g.Prune(work); err != nil {
					slog.Warn("instance topology retention deferred", "error", err)
				}
			}
			cancel()
		}
	}
}
