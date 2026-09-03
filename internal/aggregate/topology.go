package aggregate

import (
	"sort"
	"sync"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/latency"
)

// Bounded per-tenant topology projection (#163, #174).
//
// GraphRAG in aggregate mode must not scan the spans table and must not query
// aggregate.db. It consumes a snapshot published by the engine instead:
// {revision, services, operations, edges} over the mutable windows plus a
// configured recent finalized horizon. The consumer REPLACES its state per
// revision — it never re-applies a cumulative snapshot through incrementing
// upserts, which is what made the old 60 s rebuild double-count.
//
// The projection is fed from the same AggregateDelta values the shards receive,
// keyed by the identity STRINGS the reducer already resolved. Nothing is
// aggregated twice and no dictionary ID is ever reversed: reversing IDs would
// be unreadable after a restart, when the durable dictionary is warm but the
// in-memory intern cache is empty.
//
// Every map is bounded. Past a cap the fact is dropped and counted, and the
// count is published on the snapshot so a consumer can say "topology is
// truncated" instead of quietly showing a partial graph as complete.

// Topology projection defaults.
const (
	// DefaultTopologyMaxServices bounds distinct services per tenant.
	DefaultTopologyMaxServices = 512
	// DefaultTopologyMaxOperationsPerService bounds distinct operations per
	// (tenant, service).
	DefaultTopologyMaxOperationsPerService = 64
	// DefaultTopologyMaxEdges bounds distinct caller/callee pairs per tenant.
	DefaultTopologyMaxEdges = 4096
	// DefaultTopologyMaxMetrics bounds distinct (service, metric) pairs per
	// tenant. Metric windows carry the baselines the anomaly detector needs.
	DefaultTopologyMaxMetrics = 4096
	// DefaultTopologyHorizon is how much finalized history the projection
	// retains behind the mutable set. Six five-minute windows is enough for a
	// rolling mean/variance baseline without pinning a day of state in RAM.
	DefaultTopologyHorizon = 30 * time.Minute
)

// TopologyConfig bounds the projection. Zero values take the defaults above.
type TopologyConfig struct {
	MaxServices             int
	MaxOperationsPerService int
	MaxEdges                int
	MaxMetrics              int
	// Horizon is the finalized history retained behind the mutable windows.
	Horizon time.Duration
}

func (c TopologyConfig) withDefaults() TopologyConfig {
	if c.MaxServices <= 0 {
		c.MaxServices = DefaultTopologyMaxServices
	}
	if c.MaxOperationsPerService <= 0 {
		c.MaxOperationsPerService = DefaultTopologyMaxOperationsPerService
	}
	if c.MaxEdges <= 0 {
		c.MaxEdges = DefaultTopologyMaxEdges
	}
	if c.MaxMetrics <= 0 {
		c.MaxMetrics = DefaultTopologyMaxMetrics
	}
	if c.Horizon <= 0 {
		c.Horizon = DefaultTopologyHorizon
	}
	return c
}

// TopologyWindow is one five-minute window of one topology entity.
//
// Closed and Final are the partial-window guard the anomaly detector needs:
// Closed means the wall clock has passed the window's end, Final means the
// lateness horizon has expired too and no further point can land in it. A
// detector that compares the current, still-filling window against a baseline
// without consulting Elapsed and Count is the "anomaly storm from a nearly
// empty window" #163 removed.
type TopologyWindow struct {
	Start   time.Time     `json:"start"`
	End     time.Time     `json:"end"`
	Closed  bool          `json:"closed"`
	Final   bool          `json:"final"`
	Elapsed time.Duration `json:"elapsed"`

	Count      uint64 `json:"count"`
	ErrorCount uint64 `json:"error_count"`

	DurationCount     uint64  `json:"duration_count,omitempty"`
	DurationSumMicros float64 `json:"duration_sum_micros,omitempty"`
	DurationMinMicros float64 `json:"duration_min_micros,omitempty"`
	DurationMaxMicros float64 `json:"duration_max_micros,omitempty"`
	// P95Micros and P99Micros come from the merged latency sketch. They are
	// zero when the entity carries no sketch (operations, edges and metrics
	// deliberately do not, so a 2 KiB sketch is not multiplied by every
	// operation of every service of every window).
	P95Micros         float64             `json:"p95_micros,omitempty"`
	P99Micros         float64             `json:"p99_micros,omitempty"`
	LatencyProvenance *latency.Provenance `json:"latency_provenance,omitempty"`

	// Value* carry metric samples: gauge samples plus counter deltas, which
	// is what a rolling mean/variance baseline is computed over.
	ValueCount uint64  `json:"value_count,omitempty"`
	ValueSum   float64 `json:"value_sum,omitempty"`
	ValueMin   float64 `json:"value_min,omitempty"`
	ValueMax   float64 `json:"value_max,omitempty"`
}

// Mean returns the mean observed value, or 0 when the window holds none.
func (w TopologyWindow) Mean() float64 {
	if w.ValueCount == 0 {
		return 0
	}
	return w.ValueSum / float64(w.ValueCount)
}

// ErrorRate returns errors/count, or 0 for an empty window.
func (w TopologyWindow) ErrorRate() float64 {
	if w.Count == 0 {
		return 0
	}
	return float64(w.ErrorCount) / float64(w.Count)
}

// AvgLatencyMs returns the mean duration in milliseconds, or 0 when the window
// carries no duration observations.
func (w TopologyWindow) AvgLatencyMs() float64 {
	if w.DurationCount == 0 {
		return 0
	}
	return w.DurationSumMicros / float64(w.DurationCount) / 1000.0
}

// TopologyService is one service and its retained windows, oldest first.
type TopologyService struct {
	Name      string           `json:"name"`
	FirstSeen time.Time        `json:"first_seen"`
	LastSeen  time.Time        `json:"last_seen"`
	Windows   []TopologyWindow `json:"windows"`
}

// TopologyOperation is one (service, operation) and its retained windows.
type TopologyOperation struct {
	Service   string           `json:"service"`
	Operation string           `json:"operation"`
	FirstSeen time.Time        `json:"first_seen"`
	LastSeen  time.Time        `json:"last_seen"`
	Windows   []TopologyWindow `json:"windows"`
}

// SnapshotEdge is one caller/callee service pair and its retained windows.
type SnapshotEdge struct {
	Caller    string           `json:"caller"`
	Callee    string           `json:"callee"`
	FirstSeen time.Time        `json:"first_seen"`
	LastSeen  time.Time        `json:"last_seen"`
	Windows   []TopologyWindow `json:"windows"`
}

// TopologyMetric is one (service, metric) series and its retained windows.
type TopologyMetric struct {
	Service   string           `json:"service"`
	Metric    string           `json:"metric"`
	FirstSeen time.Time        `json:"first_seen"`
	LastSeen  time.Time        `json:"last_seen"`
	Windows   []TopologyWindow `json:"windows"`
}

// TopologySnapshot is one tenant's replacement-by-revision topology view.
//
// Revision is the engine revision at the last fold that CHANGED this tenant.
// A consumer that already applied the same (Epoch, Revision) pair must do
// nothing. Epoch changes when the engine's identity resets — a restart — after
// which Revision starts over and the consumer must replace rather than
// reconcile.
type TopologySnapshot struct {
	Tenant   string    `json:"tenant"`
	Epoch    uint64    `json:"epoch"`
	Revision uint64    `json:"revision"`
	Now      time.Time `json:"now"`

	Horizon time.Duration `json:"horizon"`

	Services   []TopologyService   `json:"services"`
	Operations []TopologyOperation `json:"operations"`
	Edges      []SnapshotEdge      `json:"edges"`
	Metrics    []TopologyMetric    `json:"metrics"`

	// Dropped* count facts refused by the projection caps since startup.
	// Non-zero means the topology is truncated and must be presented as such.
	DroppedServices   uint64 `json:"dropped_services,omitempty"`
	DroppedOperations uint64 `json:"dropped_operations,omitempty"`
	DroppedEdges      uint64 `json:"dropped_edges,omitempty"`
	DroppedMetrics    uint64 `json:"dropped_metrics,omitempty"`
}

// Truncated reports whether any projection cap has refused a fact for this
// tenant. A consumer must not present a truncated topology as complete.
func (s TopologySnapshot) Truncated() bool {
	return s.DroppedServices+s.DroppedOperations+s.DroppedEdges+s.DroppedMetrics > 0
}

// Empty reports whether the snapshot carries no topology at all.
func (s TopologySnapshot) Empty() bool {
	return len(s.Services) == 0 && len(s.Operations) == 0 && len(s.Edges) == 0 && len(s.Metrics) == 0
}

// --- internal state ---

// topoKind selects which maps a fact lands in.
type topoKind uint8

const (
	// topoTrace is a per-operation trace series. It folds into BOTH the
	// service entry and the (service, operation) entry: a service's counters
	// are the sum over its operations, computed once here rather than twice
	// in the reducer.
	topoTrace topoKind = iota
	// topoEdge is a caller/callee service pair.
	topoEdge
	// topoMetric is a (service, metric) series.
	topoMetric
)

// topoIdentity is the string identity of one reduced series, recorded by the
// reducer alongside its delta so the projection never reverses a dictionary ID.
type topoIdentity struct {
	Kind topoKind
	// Tenant is the tenant name. The series key carries only the interned
	// tenant ID, and the projection is keyed by name so a consumer never has
	// to resolve one.
	Tenant string
	// A is the service for services, operations and metrics, and the CALLER
	// for edges. B is the operation, metric name, or CALLEE.
	A string
	B string
}

type topoPair struct{ a, b string }

// topoIdentityFor derives the projection identity of a DURABLE series.
//
// The fold path never reverses a dictionary ID — the reducer records the
// identity strings alongside the delta. Startup restore has no reducer to ask,
// so it reverses IDs here, which is safe for exactly the reason the fold path
// could not rely on: by the time recovery runs, the durable dictionary is
// warm. A series whose names no longer resolve is refused rather than folded
// under an invented name.
func (e *Engine) topoIdentityFor(key SeriesKey) (topoIdentity, bool) {
	var kind topoKind
	switch key.Signal {
	case SignalTraceOp:
		kind = topoTrace
	case SignalServiceEdge:
		kind = topoEdge
	case SignalMetric:
		kind = topoMetric
	default:
		return topoIdentity{}, false
	}
	tenant := e.nameByID(key.TenantID)
	a := e.nameByID(key.ServiceID)
	if tenant == "" || a == "" {
		return topoIdentity{}, false
	}
	b := e.nameByID(key.NameID)
	if b == "" && kind != topoTrace {
		// An edge with no callee and a metric with no name are not facts.
		// A trace series with an unresolvable operation still describes its
		// service, so it folds as a service-only observation.
		return topoIdentity{}, false
	}
	return topoIdentity{Kind: kind, Tenant: tenant, A: a, B: b}, true
}

type topoWindowState struct {
	count      uint64
	errors     uint64
	durCount   uint64
	durSum     float64
	durMin     float64
	durMax     float64
	sketch     *Sketch
	valueCount uint64
	valueSum   float64
	valueMin   float64
	valueMax   float64
}

type topoEntry struct {
	first   time.Time
	last    time.Time
	windows map[int64]*topoWindowState
}

func newTopoEntry() *topoEntry {
	return &topoEntry{windows: make(map[int64]*topoWindowState, 4)}
}

type tenantTopology struct {
	services map[string]*topoEntry
	ops      map[topoPair]*topoEntry
	edges    map[topoPair]*topoEntry
	metrics  map[topoPair]*topoEntry

	opsPerService map[string]int

	revision uint64

	droppedServices   uint64
	droppedOperations uint64
	droppedEdges      uint64
	droppedMetrics    uint64
}

func newTenantTopology() *tenantTopology {
	return &tenantTopology{
		services:      make(map[string]*topoEntry),
		ops:           make(map[topoPair]*topoEntry),
		edges:         make(map[topoPair]*topoEntry),
		metrics:       make(map[topoPair]*topoEntry),
		opsPerService: make(map[string]int),
	}
}

// topologyProjection is the engine's per-tenant topology state. One mutex
// guards the whole projection: folding happens once per Export (a handful of
// map writes per reduced delta, not per point) and snapshots once per consumer
// tick, so contention is nowhere near the shard path's.
type topologyProjection struct {
	cfg   TopologyConfig
	epoch uint64

	mu      sync.Mutex
	tenants map[string]*tenantTopology
}

func newTopologyProjection(cfg TopologyConfig, epoch uint64) *topologyProjection {
	return &topologyProjection{
		cfg:     cfg.withDefaults(),
		epoch:   epoch,
		tenants: make(map[string]*tenantTopology),
	}
}

// fold merges one reduced Export request into the projection. revision is the
// engine revision the deltas were applied under; a tenant's snapshot revision
// advances only when one of its facts actually landed.
//
// It returns how many (series, window) deltas actually landed. The count is
// what the startup restore reports: a row read from the store but dropped by
// the retention cutoff or a projection cap was not restored, and saying it was
// would make the restore counter a lie.
func (p *topologyProjection) fold(now time.Time, revision uint64, ids map[SeriesWindowKey]topoIdentity, deltas DeltaMap) int {
	if len(ids) == 0 {
		return 0
	}
	cutoff := p.retainCutoff(now)

	folded := 0
	p.mu.Lock()
	defer p.mu.Unlock()
	touched := make(map[*tenantTopology]struct{}, 1)
	for swk, id := range ids {
		d, ok := deltas[swk]
		if !ok || d == nil {
			continue
		}
		if swk.WindowStart < cutoff {
			continue
		}
		tt := p.tenants[id.Tenant]
		if tt == nil {
			tt = newTenantTopology()
			p.tenants[id.Tenant] = tt
		}
		if p.foldOne(tt, id, swk.WindowStart, d) {
			touched[tt] = struct{}{}
			folded++
		}
	}
	for tt := range touched {
		tt.revision = revision
		p.pruneTenantLocked(tt, cutoff)
	}
	return folded
}

func (p *topologyProjection) retainCutoff(now time.Time) int64 {
	return WindowStart(now.Add(-p.cfg.Horizon)) - int64(WindowSize/time.Second) - int64(AllowedLateness/time.Second)
}

// foldOne merges one delta into every entity its identity names, enforcing the
// caps. It reports whether the projection actually changed.
func (p *topologyProjection) foldOne(tt *tenantTopology, id topoIdentity, window int64, d *AggregateDelta) bool {
	changed := false
	switch id.Kind {
	case topoTrace:
		if e, ok := p.serviceEntry(tt, id.A); ok {
			// Services carry the latency sketch: percentile detection needs
			// one, and one per service per window is the bound that keeps
			// 512 services affordable at 2 KiB a sketch.
			foldEntry(e, window, d, true)
			changed = true
		}
		if id.B != "" {
			if e, ok := p.operationEntry(tt, id.A, id.B); ok {
				foldEntry(e, window, d, false)
				changed = true
			}
		}
	case topoEdge:
		if e, ok := p.edgeEntry(tt, id.A, id.B); ok {
			foldEntry(e, window, d, false)
			changed = true
		}
	case topoMetric:
		if e, ok := p.metricEntry(tt, id.A, id.B); ok {
			foldEntry(e, window, d, false)
			changed = true
		}
	}
	return changed
}

// foldEntry merges one delta into one entity's window bucket.
func foldEntry(entry *topoEntry, window int64, d *AggregateDelta, withSketch bool) {
	w := entry.windows[window]
	if w == nil {
		w = &topoWindowState{}
		entry.windows[window] = w
	}
	mergeTopoWindow(w, d, withSketch)
	touchTimes(entry, d, window)
}

func (p *topologyProjection) serviceEntry(tt *tenantTopology, service string) (*topoEntry, bool) {
	if e, ok := tt.services[service]; ok {
		return e, true
	}
	if len(tt.services) >= p.cfg.MaxServices {
		tt.droppedServices++
		return nil, false
	}
	e := newTopoEntry()
	tt.services[service] = e
	return e, true
}

func (p *topologyProjection) operationEntry(tt *tenantTopology, service, operation string) (*topoEntry, bool) {
	key := topoPair{service, operation}
	if e, ok := tt.ops[key]; ok {
		return e, true
	}
	if tt.opsPerService[service] >= p.cfg.MaxOperationsPerService {
		tt.droppedOperations++
		return nil, false
	}
	e := newTopoEntry()
	tt.ops[key] = e
	tt.opsPerService[service]++
	return e, true
}

func (p *topologyProjection) edgeEntry(tt *tenantTopology, caller, callee string) (*topoEntry, bool) {
	key := topoPair{caller, callee}
	if e, ok := tt.edges[key]; ok {
		return e, true
	}
	if len(tt.edges) >= p.cfg.MaxEdges {
		tt.droppedEdges++
		return nil, false
	}
	e := newTopoEntry()
	tt.edges[key] = e
	return e, true
}

func (p *topologyProjection) metricEntry(tt *tenantTopology, service, metric string) (*topoEntry, bool) {
	key := topoPair{service, metric}
	if e, ok := tt.metrics[key]; ok {
		return e, true
	}
	if len(tt.metrics) >= p.cfg.MaxMetrics {
		tt.droppedMetrics++
		return nil, false
	}
	e := newTopoEntry()
	tt.metrics[key] = e
	return e, true
}

// mergeTopoWindow folds a delta's aggregates into one window bucket. withSketch
// is true only for services: percentile detection needs a sketch, and one per
// service per window is the bound that keeps 512 services affordable.
func mergeTopoWindow(w *topoWindowState, d *AggregateDelta, withSketch bool) {
	w.count += d.Count
	w.errors += d.ErrorCount
	if d.DurationCount > 0 {
		if w.durCount == 0 || d.DurationMin < w.durMin {
			w.durMin = d.DurationMin
		}
		if w.durCount == 0 || d.DurationMax > w.durMax {
			w.durMax = d.DurationMax
		}
		w.durCount += d.DurationCount
		w.durSum += d.DurationSum
		if withSketch && d.Sketch != nil {
			if w.sketch == nil {
				w.sketch = NewSketch()
			}
			w.sketch.Merge(d.Sketch)
		}
	}
	if d.GaugeCount > 0 {
		if w.valueCount == 0 || d.GaugeMin < w.valueMin {
			w.valueMin = d.GaugeMin
		}
		if w.valueCount == 0 || d.GaugeMax > w.valueMax {
			w.valueMax = d.GaugeMax
		}
		w.valueCount += d.GaugeCount
		w.valueSum += d.GaugeSum
	} else if d.CounterDelta != 0 {
		// A counter contributes one observation per window: its increase.
		// Min/max track the per-fold increase, which is what a rolling
		// baseline over windows compares.
		if w.valueCount == 0 || d.CounterDelta < w.valueMin {
			w.valueMin = d.CounterDelta
		}
		if w.valueCount == 0 || d.CounterDelta > w.valueMax {
			w.valueMax = d.CounterDelta
		}
		w.valueCount++
		w.valueSum += d.CounterDelta
	}
}

// touchTimes advances an entry's first/last seen. Log deltas carry real
// timestamps; span and metric deltas do not, so the window bounds stand in.
func touchTimes(entry *topoEntry, d *AggregateDelta, window int64) {
	first := d.FirstTimestamp
	last := d.LastTimestamp
	if first.IsZero() {
		first = time.Unix(window, 0).UTC()
	}
	if last.IsZero() {
		last = time.Unix(window+int64(WindowSize/time.Second), 0).UTC()
	}
	if entry.first.IsZero() || first.Before(entry.first) {
		entry.first = first
	}
	if last.After(entry.last) {
		entry.last = last
	}
}

// pruneTenantLocked drops windows behind the retention cutoff and forgets
// entities that no longer hold any window. It reports whether the visible
// replacement changed.
func (p *topologyProjection) pruneTenantLocked(tt *tenantTopology, cutoff int64) (changed bool) {
	for name, e := range tt.services {
		empty, pruned := pruneEntry(e, cutoff)
		changed = changed || pruned
		if empty {
			delete(tt.services, name)
		}
	}
	for key, e := range tt.ops {
		empty, pruned := pruneEntry(e, cutoff)
		changed = changed || pruned
		if empty {
			delete(tt.ops, key)
			if n := tt.opsPerService[key.a]; n > 1 {
				tt.opsPerService[key.a] = n - 1
			} else {
				delete(tt.opsPerService, key.a)
			}
		}
	}
	for key, e := range tt.edges {
		empty, pruned := pruneEntry(e, cutoff)
		changed = changed || pruned
		if empty {
			delete(tt.edges, key)
		}
	}
	for key, e := range tt.metrics {
		empty, pruned := pruneEntry(e, cutoff)
		changed = changed || pruned
		if empty {
			delete(tt.metrics, key)
		}
	}
	return changed
}

// pruneEntry drops expired windows and reports whether the entry is now empty.
func pruneEntry(e *topoEntry, cutoff int64) (empty, changed bool) {
	for start := range e.windows {
		if start < cutoff {
			delete(e.windows, start)
			changed = true
		}
	}
	return len(e.windows) == 0, changed
}

// Prune drops windows past the retention horizon for every tenant. The fold
// path prunes the tenants it touches; this is what keeps a tenant that has gone
// silent from holding its last windows forever.
func (p *topologyProjection) Prune(now time.Time, nextRevision func() uint64) {
	cutoff := p.retainCutoff(now)
	p.mu.Lock()
	defer p.mu.Unlock()
	var revision uint64
	for _, tt := range p.tenants {
		if p.pruneTenantLocked(tt, cutoff) {
			if revision == 0 {
				revision = nextRevision()
			}
			tt.revision = revision
		}
	}
}

// Tenants returns the tenants the projection currently holds topology for.
func (p *topologyProjection) Tenants() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.tenants))
	for tenant := range p.tenants {
		out = append(out, tenant)
	}
	sort.Strings(out)
	return out
}

// Revision returns the revision of a tenant's topology without building a
// snapshot, so a consumer can skip an unchanged tenant for the cost of one map
// lookup.
func (p *topologyProjection) Revision(tenant string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tt, ok := p.tenants[tenant]; ok {
		return tt.revision
	}
	return 0
}

// Snapshot renders one tenant's topology. The result shares nothing with the
// projection: every window is copied by value and percentiles are read out of
// the sketch here, so the consumer never touches engine state.
func (p *topologyProjection) Snapshot(tenant string, now time.Time) TopologySnapshot {
	snap := TopologySnapshot{
		Tenant:     tenant,
		Epoch:      p.epoch,
		Now:        now,
		Horizon:    p.cfg.Horizon,
		Services:   []TopologyService{},
		Operations: []TopologyOperation{},
		Edges:      []SnapshotEdge{},
		Metrics:    []TopologyMetric{},
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tt, ok := p.tenants[tenant]
	if !ok {
		return snap
	}
	snap.Revision = tt.revision
	snap.DroppedServices = tt.droppedServices
	snap.DroppedOperations = tt.droppedOperations
	snap.DroppedEdges = tt.droppedEdges
	snap.DroppedMetrics = tt.droppedMetrics

	snap.Services = make([]TopologyService, 0, len(tt.services))
	for name, e := range tt.services {
		snap.Services = append(snap.Services, TopologyService{
			Name:      name,
			FirstSeen: e.first,
			LastSeen:  e.last,
			Windows:   renderWindows(e, now),
		})
	}
	sort.Slice(snap.Services, func(i, j int) bool { return snap.Services[i].Name < snap.Services[j].Name })

	snap.Operations = make([]TopologyOperation, 0, len(tt.ops))
	for key, e := range tt.ops {
		snap.Operations = append(snap.Operations, TopologyOperation{
			Service:   key.a,
			Operation: key.b,
			FirstSeen: e.first,
			LastSeen:  e.last,
			Windows:   renderWindows(e, now),
		})
	}
	sort.Slice(snap.Operations, func(i, j int) bool {
		if snap.Operations[i].Service != snap.Operations[j].Service {
			return snap.Operations[i].Service < snap.Operations[j].Service
		}
		return snap.Operations[i].Operation < snap.Operations[j].Operation
	})

	snap.Edges = make([]SnapshotEdge, 0, len(tt.edges))
	for key, e := range tt.edges {
		snap.Edges = append(snap.Edges, SnapshotEdge{
			Caller:    key.a,
			Callee:    key.b,
			FirstSeen: e.first,
			LastSeen:  e.last,
			Windows:   renderWindows(e, now),
		})
	}
	sort.Slice(snap.Edges, func(i, j int) bool {
		if snap.Edges[i].Caller != snap.Edges[j].Caller {
			return snap.Edges[i].Caller < snap.Edges[j].Caller
		}
		return snap.Edges[i].Callee < snap.Edges[j].Callee
	})

	snap.Metrics = make([]TopologyMetric, 0, len(tt.metrics))
	for key, e := range tt.metrics {
		snap.Metrics = append(snap.Metrics, TopologyMetric{
			Service:   key.a,
			Metric:    key.b,
			FirstSeen: e.first,
			LastSeen:  e.last,
			Windows:   renderWindows(e, now),
		})
	}
	sort.Slice(snap.Metrics, func(i, j int) bool {
		if snap.Metrics[i].Service != snap.Metrics[j].Service {
			return snap.Metrics[i].Service < snap.Metrics[j].Service
		}
		return snap.Metrics[i].Metric < snap.Metrics[j].Metric
	})

	return snap
}

// renderWindows copies one entry's windows, oldest first, stamping the
// closed/final/elapsed metadata the partial-window guard reads.
func renderWindows(e *topoEntry, now time.Time) []TopologyWindow {
	starts := make([]int64, 0, len(e.windows))
	for start := range e.windows {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	out := make([]TopologyWindow, 0, len(starts))
	for _, start := range starts {
		w := e.windows[start]
		tw := TopologyWindow{
			Start:             time.Unix(start, 0).UTC(),
			End:               time.Unix(start+int64(WindowSize/time.Second), 0).UTC(),
			Count:             w.count,
			ErrorCount:        w.errors,
			DurationCount:     w.durCount,
			DurationSumMicros: w.durSum,
			DurationMinMicros: w.durMin,
			DurationMaxMicros: w.durMax,
			ValueCount:        w.valueCount,
			ValueSum:          w.valueSum,
			ValueMin:          w.valueMin,
			ValueMax:          w.valueMax,
		}
		tw.Closed = !now.Before(tw.End)
		tw.Final = now.Unix() >= start+int64(WindowSize/time.Second)+int64(AllowedLateness/time.Second)
		if tw.Closed {
			tw.Elapsed = WindowSize
		} else if el := now.Sub(tw.Start); el > 0 {
			tw.Elapsed = el
		}
		if w.sketch != nil && w.sketch.Count() > 0 {
			tw.P95Micros = w.sketch.Quantile(0.95)
			tw.P99Micros = w.sketch.Quantile(0.99)
			p95 := *PercentileFromSketch(w.sketch)
			p99 := p95
			tw.LatencyProvenance = &latency.Provenance{P95: &p95, P99: &p99}
		}
		out = append(out, tw)
	}
	return out
}
