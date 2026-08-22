package aggregate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The engine: mutable windows, four mutex-guarded shards, and the apply path.
//
// Time bounds are fixed by #160 and #153 §6: five-minute UTC tumbling windows,
// ten minutes of allowed lateness, two minutes of tolerated future skew. The
// mutable set is the current window plus the windows still inside the lateness
// horizon; everything older is finalized and never re-enters memory.
//
// ROLLOVER CLOSES A WINDOW; FINALIZATION EVICTS IT (#194 blocker 6). A window
// past its lateness horizon stops accepting points but keeps its shard
// contents, its memory ownership and its readability until the writer's
// finalize pass has materialized it from the delta log into aggregate_buckets
// and called MarkFinalized. Rolling ownership forward at close time instead —
// what this engine used to do — pointed reads at a store that had no buckets
// yet, so the window silently disappeared from every query until the next
// successful finalize, and permanently while the finalizer was failing.
//
// The exception is the closed-window cap: memory may not grow without bound
// behind a wedged finalizer, so past DefaultMaxClosedWindows the oldest closed
// windows are evicted anyway. That is loss, and it is counted — see
// Snapshot.ClosedWindowsForced and otelcontext_aggregate_closed_windows_*.
// WITHOUT a store (no writer installed, engine constructed directly in a test)
// nothing ever finalizes, so the cap is the only thing bounding memory and
// every window it drops is gone: nothing may be presented to a user as
// authoritative unless the group-commit writer is the applier.
//
// Shards are four plain mutex-guarded maps (hash & 3). No shard goroutines, no
// channels. Two shard locks are NEVER held at once: a delta touches exactly one
// shard, and the apply path walks the shards one at a time. The limiter is the
// only other lock in play and it never reaches back for a shard, so the order
// is always shard -> limiter.

// Engine time bounds.
const (
	// WindowSize is the tumbling window width. Windows are aligned to UTC.
	WindowSize = 5 * time.Minute
	// AllowedLateness is how long after a window closes its points are still
	// accepted. Later points are excluded from aggregates and counted.
	AllowedLateness = 10 * time.Minute
	// MaxFutureSkew is how far ahead of arrival a point may be timestamped.
	// Future windows are never created.
	MaxFutureSkew = 2 * time.Minute
	// NumShards is the shard count. Fixed at four per #160.
	NumShards = 4
	// DefaultMaxClosedWindows bounds how many closed-but-unfinalized windows
	// the engine keeps in RAM (#194 blocker 6).
	//
	// A window closes one lateness horizon after it opens and the writer's
	// finalize pass runs every 30 s, so the steady state is one closed window
	// or none. Twelve is an hour of finalizer backlog: enough that a slow or
	// briefly failing store never costs data, small enough that a wedged
	// finalizer cannot grow memory without bound. Past it the engine falls
	// back to the old lossy eviction and counts every window it drops.
	DefaultMaxClosedWindows = 12
)

// Modes. Phase 1 ships legacy and aggregate-shadow; ModeAggregate is accepted
// by config but the read path does not switch over until a later phase.
const (
	ModeLegacy    = "legacy"
	ModeShadow    = "aggregate-shadow"
	ModeAggregate = "aggregate"
)

// PointDisposition classifies a point against the mutable-window horizon.
type PointDisposition uint8

// PointDisposition values. The non-accepted strings are metric label values.
const (
	// PointAccepted — the point belongs to a mutable window.
	PointAccepted PointDisposition = iota
	// PointLate — the point is older than the lateness horizon. It is excluded
	// from aggregates and counted; the raw/exemplar path still sees it, because
	// a late error trace is still evidence (#160).
	PointLate
	// PointFuture — the point is timestamped beyond the tolerated skew.
	PointFuture
)

// String implements fmt.Stringer.
func (d PointDisposition) String() string {
	switch d {
	case PointLate:
		return "late"
	case PointFuture:
		return "future"
	default:
		return "accepted"
	}
}

// DeltaMap is one reducer's output: the deltas of one Export request, keyed by
// series and window.
type DeltaMap map[SeriesWindowKey]*AggregateDelta

// Applier applies deltas to the engine's shards. Phase 1 wires the direct
// applier, which mutates the shards inline. Phase 2 (#173) interposes the
// group-commit writer here: it batches deltas from many Export requests into
// one SQLite transaction and calls ApplyCommitted only after the COMMIT, making
// the shards a projection of committed state. No caller changes when it does.
type Applier interface {
	Apply(DeltaMap) uint64
}

// FailableApplier is an Applier whose apply path can refuse. The durable
// group-commit writer (#173) implements it: admission can be saturated
// (ErrSaturated, mapped to RESOURCE_EXHAUSTED / 429) and a COMMIT can fail, and
// under the durable-ACK contract neither may be acknowledged as success. The
// Phase 1 direct applier does not implement it, which is what keeps legacy and
// shadow behaviour identical when no store is wired.
type FailableApplier interface {
	Applier
	// ApplyErr applies the deltas, returning the revision and any refusal.
	ApplyErr(DeltaMap) (uint64, error)
}

// EngineConfig configures an Engine. Zero values take platform defaults.
type EngineConfig struct {
	// Mode is the aggregate mode (AGGREGATE_MODE). An Engine is only
	// constructed for a non-legacy mode.
	Mode string

	// Limiter holds the cardinality budget.
	Limiter LimiterConfig

	// MaxProducerBaselinesPerSeries and MaxBaselines bound the cumulative
	// baseline tracker.
	MaxProducerBaselinesPerSeries int
	MaxBaselines                  int

	// Registrar mints dictionary IDs. Defaults to an in-memory registrar
	// whose IDs are provisional and vanish on restart (#173 replaces it).
	Registrar Registrar

	// Bounds are the identity bounds of #200 Q3: encoded-value length caps,
	// per-(tenant, kind) and instance-wide dictionary counts, and the tenant
	// cap. The zero value takes every default.
	Bounds Bounds

	// Miner is the ingest-owned template miner (#163). Defaults to one built
	// from the log-template cap.
	Miner *TemplateMiner

	// Metrics is the Prometheus recorder. nil disables metric recording.
	Metrics MetricsRecorder

	// MetricDims maps metric names to their configured aggregation dimension keys.
	// Nil or empty means no metrics are configured for custom dimensions.
	MetricDims DimsConfig

	// Topology bounds the per-tenant topology projection GraphRAG consumes in
	// aggregate mode (#174). Zero values take the package defaults.
	Topology TopologyConfig

	// EdgeResolverSpans bounds the span-ID memory used to recover a call
	// edge's caller service. Zero takes DefaultEdgeResolverSpans.
	EdgeResolverSpans int

	// MaxClosedWindows bounds the closed-but-unfinalized windows held in
	// memory. Zero takes DefaultMaxClosedWindows; negative is unbounded and
	// is for tests only.
	MaxClosedWindows int

	// Epoch identifies this engine instance. A consumer that sees a new epoch
	// knows the revision counter restarted and must replace its state rather
	// than reconcile against it. Zero derives one from the clock.
	Epoch uint64

	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time
}

// Engine is the aggregate accounting engine.
type Engine struct {
	mode string
	now  func() time.Time

	cache     *Cache
	miner     *TemplateMiner
	limiter   *Limiter
	baselines *BaselineTracker
	metrics   MetricsRecorder
	dims      DimsConfig
	topology  *topologyProjection
	edges     *EdgeResolver
	epoch     uint64

	shards [NumShards]shard

	// own is the ownership record: which windows memory owns, which windows
	// the store owns, and the process generation the revision counter belongs
	// to. It is the OUTER lock of the engine — a shard lock is only ever taken
	// while it is held, never the reverse. See ownership.
	own ownership

	// store is the durable store used by the read path for finalized windows.
	// nil (the Phase 1 / test case) means finalized windows are simply gone.
	store atomic.Pointer[Store]

	applier  atomic.Pointer[Applier]
	revision atomic.Uint64

	maxClosedWindows int

	windowsDiscarded atomic.Uint64
	seriesDiscarded  atomic.Uint64
	closedForced     atomic.Uint64
}

// shard is one mutex-guarded slice of the mutable window set.
type shard struct {
	mu      sync.Mutex
	windows map[int64]map[SeriesKey]*AggregateDelta
}

// ownership answers "who owns this window" for the read path (#164).
//
// A window is owned by memory or by the store, never by both and never by
// neither. Reads of an engine-owned window come exclusively from the shards
// even when crash-recovery delta rows exist for it; reads of a store-owned
// window come exclusively from the store. Ownership, not row presence, is the
// dedup rule.
//
// Every transition — a new window appearing, rollover evicting one,
// finalization handing one to the store — happens under mu together with the
// shard mutation that implements it, so a concurrent Ownership() can neither
// omit nor double-count a window. That is why mu is the engine's OUTER lock:
// a shard mutex may be taken while mu is held, never the other way round.
type ownership struct {
	mu sync.RWMutex
	// mutable is the set of window starts the shards own.
	mutable map[int64]struct{}
	// closed is the subset of mutable whose lateness horizon has expired.
	// A closed window refuses NEW points but is still memory-owned and still
	// readable: only a committed FinalizeWindow may take it out of memory
	// (#194 blocker 6). Evicting at rollover instead made the window vanish
	// from queries until the next successful finalize — indefinitely while the
	// finalizer was failing.
	closed map[int64]struct{}
	// watermark is the newest window start handed to the store. Every window
	// at or below it is store-owned.
	watermark int64
	// epoch identifies this process generation. The revision counter restarts
	// at zero on every boot, so a consumer that only compared revisions could
	// mistake a restarted counter for a rollback; the epoch is what makes the
	// pair {epoch, revision} a total order (#164).
	epoch string
}

// Ownership is an atomic snapshot of {mutable set, finalized watermark,
// revision, epoch}. A query captures one and reads every window through it.
type Ownership struct {
	// Epoch is the process generation.
	Epoch string
	// Revision is the engine revision at capture time.
	Revision uint64
	// Mutable is the memory-owned window starts, oldest first.
	Mutable []int64
	// FinalizedWatermark is the newest store-owned window start. Zero means
	// nothing has been handed over yet.
	FinalizedWatermark int64
}

// OwnsInMemory reports whether windowStart is memory-owned in this snapshot.
func (o Ownership) OwnsInMemory(windowStart int64) bool {
	for _, w := range o.Mutable {
		if w == windowStart {
			return true
		}
	}
	return false
}

// NewEngine builds an Engine. It fails on a budget that cannot hold — a
// misconfigured cap must stop startup, not silently become policy.
func NewEngine(cfg EngineConfig) (*Engine, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeShadow
	}
	limCfg := cfg.Limiter.withDefaults()
	if err := limCfg.Validate(); err != nil {
		return nil, err
	}
	reg := cfg.Registrar
	if reg == nil {
		reg = NewMemRegistrar(nil)
	}
	epoch := cfg.Epoch
	if epoch == 0 {
		epoch = uint64(cfg.Now().UnixNano()) // #nosec G115 -- wall clock as an opaque instance tag
	}
	e := &Engine{
		mode:    cfg.Mode,
		now:     cfg.Now,
		cache:   NewCacheWithBounds(reg, cfg.Bounds),
		miner:   cfg.Miner,
		metrics: cfg.Metrics,
		dims:    cfg.MetricDims,
		epoch:   epoch,
	}
	e.maxClosedWindows = cfg.MaxClosedWindows
	if e.maxClosedWindows == 0 {
		e.maxClosedWindows = DefaultMaxClosedWindows
	}
	e.topology = newTopologyProjection(cfg.Topology, epoch)
	e.edges = NewEdgeResolver(cfg.EdgeResolverSpans)
	if e.metrics == nil {
		e.metrics = noopRecorder{}
	}
	e.cache.SetOverflowSink(e.metrics.RecordIdentityOverflow)
	limCfg.OtherNameID = e.otherNameID
	e.limiter = NewLimiter(limCfg)
	e.baselines = NewBaselineTracker(BaselineTrackerConfig{
		MaxProducersPerSeries: cfg.MaxProducerBaselinesPerSeries,
		MaxBaselines:          cfg.MaxBaselines,
		GapThreshold:          AllowedLateness,
	})
	if e.miner == nil {
		// The miner's IDs ARE the log series NameIDs, so they must be minted
		// in the same dictionary namespace the overflow __other__ ID comes
		// from (#159 as amended by #163). A private counter would let an
		// ordinary template collide with an unrelated dictionary entry.
		e.miner = NewTemplateMiner(TemplateMinerConfig{
			MaxTemplatesPerService: limCfg.MaxLogTemplatesPerService,
			Registrar:              TemplateRegistrarFunc(e.registerTemplate),
		})
	}
	for i := range e.shards {
		e.shards[i].windows = make(map[int64]map[SeriesKey]*AggregateDelta)
	}
	e.own.mutable = make(map[int64]struct{})
	e.own.closed = make(map[int64]struct{})
	e.own.epoch = newEpoch(cfg.Now())
	var a Applier = directApplier{e}
	e.applier.Store(&a)
	return e, nil
}

// Mode returns the configured aggregate mode.
func (e *Engine) Mode() string { return e.mode }

// Cache returns the dictionary cache.
func (e *Engine) Cache() *Cache { return e.cache }

// Miner returns the ingest-owned template miner.
func (e *Engine) Miner() *TemplateMiner { return e.miner }

// Limiter returns the cardinality limiter.
func (e *Engine) Limiter() *Limiter { return e.limiter }

// Baselines returns the cumulative baseline tracker.
func (e *Engine) Baselines() *BaselineTracker { return e.baselines }

// MetricDims returns the configured metric dimension keys for aggregation.
func (e *Engine) MetricDims() DimsConfig { return e.dims }

// EdgeResolver returns the caller-resolution memory used to derive service
// edges. The OTLP trace receiver feeds every received span through it.
func (e *Engine) EdgeResolver() *EdgeResolver { return e.edges }

// TopologyEpoch returns this engine instance's epoch.
func (e *Engine) TopologyEpoch() uint64 { return e.epoch }

// TopologyTenants returns the tenants the projection currently holds.
func (e *Engine) TopologyTenants() []string { return e.topology.Tenants() }

// TopologyRevision returns a tenant's topology revision without rendering a
// snapshot, so an unchanged tenant costs one map lookup.
func (e *Engine) TopologyRevision(tenant string) uint64 { return e.topology.Revision(tenant) }

// TopologySnapshot renders one tenant's replacement-by-revision topology.
func (e *Engine) TopologySnapshot(tenant string) TopologySnapshot {
	return e.topology.Snapshot(tenant, e.now())
}

// PruneTopology drops topology windows past the retention horizon. The fold
// path prunes the tenants it touches; this is what bounds a tenant that has
// gone silent.
func (e *Engine) PruneTopology() { e.topology.Prune(e.now()) }

// TopologyHorizon is how much finalized history the projection retains behind
// the mutable set. Startup restore reads no further back than this: a window
// the projection would immediately prune is a window there is no point paying
// to read.
func (e *Engine) TopologyHorizon() time.Duration { return e.topology.cfg.Horizon }

// RestoreTopology folds durable rows into the TOPOLOGY PROJECTION ONLY.
//
// It is the read side of the bounded startup exception (#194 finding 15): the
// mutable shards are untouched, so nothing here can resurrect finalized
// history as mutable state or double-count a window the delta-log replay
// already restored. Identities come from reversed dictionary IDs rather than
// from a reducer, and the projection's own retention cutoff still applies —
// which is why the folded count, not the row count, is what recovery reports.
func (e *Engine) RestoreTopology(ids map[SeriesWindowKey]topoIdentity, deltas DeltaMap) int {
	return e.topology.fold(e.now(), e.revision.Load(), ids, deltas)
}

// SetTemplateFactSink installs the log-fact consumer on the ingest-owned
// template miner (#163). GraphRAG performs no mining of its own in shadow and
// aggregate modes; it consumes these facts. Passing nil detaches the sink.
func (e *Engine) SetTemplateFactSink(fn func(TemplateFact)) {
	if e.miner != nil {
		e.miner.SetFactSink(fn)
	}
}

// Revision returns the current revision. It increases by one on every applied
// batch and never decreases, so a consumer can tell "nothing changed" from
// "changed back to the same numbers" (#163's replacement-by-revision topology).
func (e *Engine) Revision() uint64 { return e.revision.Load() }

// Epoch returns the process generation identifier. It is stable for the life
// of the engine and pairs with Revision to give consumers a total order across
// restarts (#164).
func (e *Engine) Epoch() string { return e.own.epoch }

// SetStore wires the durable store into the READ path. The write path already
// reaches the store through the group-commit writer; this is what lets the
// query facade serve finalized windows. Call it once at startup, before the
// engine takes traffic.
func (e *Engine) SetStore(st Store) {
	if st == nil {
		return
	}
	e.store.Store(&st)
}

// Store returns the wired durable store, or nil when none is configured.
func (e *Engine) Store() Store {
	p := e.store.Load()
	if p == nil {
		return nil
	}
	return *p
}

// TenantID resolves a tenant name to its dictionary ID for the read path. ok
// is false when the tenant identity was REJECTED — over-length, empty, or past
// the instance-wide tenant cap (#200 Q3). A rejected tenant is never collapsed
// into a shared identity; the caller reads nothing rather than another
// tenant's data.
func (e *Engine) TenantID(name string) (uint32, bool) { return e.cache.InternTenant(name) }

// ActiveSeriesKeys returns every series identity the mutable shards currently
// hold. It is a GC root set (#200 Q1): a series present in memory is live even
// if the durable tables have not caught up with it yet.
//
// Bounded by AGGREGATE_MAX_SERIES plus the overflow reserve, so the map is
// thousands of entries, not millions.
func (e *Engine) ActiveSeriesKeys() map[SeriesKey]struct{} {
	out := make(map[SeriesKey]struct{}, 1024)
	if e == nil {
		return out
	}
	for i := range e.shards {
		sh := &e.shards[i]
		sh.mu.Lock()
		for _, window := range sh.windows {
			for key := range window {
				out[key] = struct{}{}
			}
		}
		sh.mu.Unlock()
	}
	return out
}

// Ownership captures {mutable set, finalized watermark, revision, epoch} in
// one critical section. Every read path starts here.
func (e *Engine) Ownership() Ownership {
	e.own.mu.RLock()
	defer e.own.mu.RUnlock()
	return e.ownershipLocked()
}

// ownershipLocked builds the snapshot. e.own.mu must be held (read or write).
func (e *Engine) ownershipLocked() Ownership {
	mutable := make([]int64, 0, len(e.own.mutable))
	for start := range e.own.mutable {
		mutable = append(mutable, start)
	}
	sort.Slice(mutable, func(i, j int) bool { return mutable[i] < mutable[j] })
	return Ownership{
		Epoch:              e.own.epoch,
		Revision:           e.revision.Load(),
		Mutable:            mutable,
		FinalizedWatermark: e.own.watermark,
	}
}

// MarkFinalized transitions one window from memory ownership to store
// ownership. The writer calls it after store.FinalizeWindow has committed, so
// the handover is exactly as atomic as the transaction that materialized the
// buckets: a query either sees the window in memory or in the store, never in
// both and never in neither.
//
// It is the ONLY path that may evict a window from the shards, drop its mutable
// ownership and advance the watermark — with one counted exception, the closed
// window cap in Rollover (#194 blocker 6).
func (e *Engine) MarkFinalized(windowStart int64) {
	e.own.mu.Lock()
	released := e.evictWindowLocked(windowStart)
	delete(e.own.mutable, windowStart)
	delete(e.own.closed, windowStart)
	if windowStart > e.own.watermark {
		e.own.watermark = windowStart
	}
	closed := len(e.own.closed)
	e.own.mu.Unlock()
	e.metrics.SetClosedWindows(closed)
	for _, swk := range released {
		e.limiter.Release(swk.Key, swk.WindowStart)
	}
	if len(released) > 0 {
		e.seriesDiscarded.Add(uint64(len(released)))
		e.publishActiveSeries()
	}
}

// evictWindowLocked drops one window from every shard and returns the series
// it released. e.own.mu must be held for writing.
func (e *Engine) evictWindowLocked(start int64) []SeriesWindowKey {
	var released []SeriesWindowKey
	for i := range e.shards {
		sh := &e.shards[i]
		sh.mu.Lock()
		if w, ok := sh.windows[start]; ok {
			for key := range w {
				released = append(released, SeriesWindowKey{Key: key, WindowStart: start})
			}
			delete(sh.windows, start)
		}
		sh.mu.Unlock()
	}
	return released
}

// newEpoch derives a process generation identifier. It only has to differ from
// the previous generation of the same process lineage, which a boot timestamp
// in base 36 does without pulling in a UUID dependency.
func newEpoch(now time.Time) string {
	return strconv.FormatInt(now.UnixNano(), 36)
}

// SetApplier replaces the apply path. Phase 2 uses it to insert the
// group-commit writer between the reducer and the shards.
func (e *Engine) SetApplier(a Applier) {
	if a == nil {
		return
	}
	e.applier.Store(&a)
}

// otherNameID resolves the dictionary __other__ entry for a signal's name
// namespace, which is what an overflow series carries as its NameID.
func (e *Engine) otherNameID(tenantID uint32, signal Signal) uint32 {
	kind, ok := NameKind(signal)
	if !ok {
		return 0
	}
	return e.cache.OtherID(tenantID, kind)
}

// registerTemplate mints a template identity in the log_template dictionary
// namespace. The registered value pairs the service with the template text at
// registration time: that pair is the immutable surrogate identity, and the
// text may evolve under the same ID later without the ID moving (#163).
func (e *Engine) registerTemplate(r TemplateRegistration) (uint32, error) {
	tenantID, ok := e.cache.InternTenant(r.Tenant)
	if !ok {
		// No tenant identity means no template identity. The miner treats a
		// zero ID as "no identity available" and routes the line to its
		// partition overflow; the reducer has already refused the point.
		return 0, ErrTenantRejected
	}
	if r.IsOther {
		return e.cache.OtherID(tenantID, KindLogTemplate), nil
	}
	var sb strings.Builder
	sb.Grow(len(r.Service) + 1 + len(r.Template))
	sb.WriteString(r.Service)
	sb.WriteByte(0)
	sb.WriteString(r.Template)
	return e.cache.Intern(tenantID, KindLogTemplate, sb.String()), nil
}

// WindowStart returns the UTC-aligned start of the window containing t, as Unix
// seconds. Alignment is computed arithmetically rather than with Truncate so it
// is obviously independent of the local zone.
func WindowStart(t time.Time) int64 {
	const w = int64(WindowSize / time.Second)
	sec := t.Unix()
	rem := sec % w
	if rem < 0 {
		rem += w
	}
	return sec - rem
}

// Classify places a point in a window relative to one Export's arrival time.
// A single arrivalTime is captured per Export request and used for every point
// in it (#160): lateness must not depend on where in the batch a point sits.
func Classify(arrival, pointTime time.Time) (int64, PointDisposition) {
	if pointTime.Sub(arrival) > MaxFutureSkew {
		return 0, PointFuture
	}
	start := WindowStart(pointTime)
	// A window stays mutable until lateness expires after it closes.
	if arrival.Unix() >= start+int64(WindowSize/time.Second)+int64(AllowedLateness/time.Second) {
		return start, PointLate
	}
	return start, PointAccepted
}

// ApplyReducer records the reduction metrics for one Export request and applies
// its deltas. This is the entry point the OTLP servers call.
func (e *Engine) ApplyReducer(r *Reducer) uint64 {
	rev, _ := e.ApplyReducerErr(r)
	return rev
}

// ApplyDeltas applies one reducer's output through the configured applier.
func (e *Engine) ApplyDeltas(m DeltaMap) uint64 {
	rev, _ := e.ApplyDeltasErr(m)
	return rev
}

// ApplyReducerErr is ApplyReducer for callers that must honour the durable-ACK
// contract: it returns the applier's refusal so an Export can answer
// RESOURCE_EXHAUSTED instead of acknowledging telemetry that is not durable.
func (e *Engine) ApplyReducerErr(r *Reducer) (uint64, error) {
	if r == nil {
		return e.revision.Load(), nil
	}
	e.recordReduction(r)
	rev, err := e.ApplyDeltasErr(r.Deltas())
	if err != nil {
		// Nothing became durable, so nothing may enter the topology a
		// consumer will present as accounted-for.
		return rev, err
	}
	e.topology.fold(e.now(), rev, r.topologyIDs(), r.Deltas())
	return rev, nil
}

// ApplyDeltasErr applies one reducer's output and surfaces any refusal. When
// the configured applier cannot fail (Phase 1's direct applier) the error is
// always nil.
func (e *Engine) ApplyDeltasErr(m DeltaMap) (uint64, error) {
	if len(m) == 0 {
		return e.revision.Load(), nil
	}
	a := *e.applier.Load()
	if fa, ok := a.(FailableApplier); ok {
		return fa.ApplyErr(m)
	}
	return a.Apply(m), nil
}

// directApplier is the Phase 1 apply path: no durability, no batching.
type directApplier struct{ e *Engine }

// Apply implements Applier.
func (d directApplier) Apply(m DeltaMap) uint64 { return d.e.ApplyCommitted(m) }

// ApplyCommitted admits the deltas against the cardinality budget and folds
// them into the mutable windows. Phase 2's writer calls it after its COMMIT;
// Phase 1 calls it inline.
//
// Admission happens first, with no shard lock held, because a series pushed
// past a cap is rerouted to an __other__ series that may hash to a different
// shard. Resolving identity before touching any shard is what keeps the rule
// "never hold two shard locks" trivially true.
func (e *Engine) ApplyCommitted(m DeltaMap) uint64 {
	if len(m) == 0 {
		return e.revision.Load()
	}
	return e.CommitAdmission(e.PlanAdmission(m))
}

// AdmissionPlan is one batch's cardinality reservation: the identities the
// shards will hold, plus the occupancy this plan charged the limiter for.
//
// It exists because admission has to precede the durable write — the row that
// becomes durable must carry the identity the shards will hold — while the
// charge must not survive a write that never happened (#194 blocker 3). Before
// it, a failed CommitGroup left the occupancy charged with no shard window to
// release it from, so a store that kept refusing writes ate the cardinality
// budget permanently and forced live telemetry into __other__.
type AdmissionPlan struct {
	// Resolved is the batch keyed by the identity to record under.
	Resolved DeltaMap
	// reserved is the (series, window) presence this plan created. Pairs that
	// were already present belong to an earlier committed batch and are not
	// listed: releasing those would take budget from data that is still live.
	reserved []SeriesWindowKey
}

// PlanAdmission rolls the mutable window set forward and reserves one batch of
// deltas against the cardinality budget WITHOUT touching the shards.
//
// The durable writer (#173) calls it before its COMMIT so the row that becomes
// durable carries the same identity the shards will later hold: admitting after
// the write would let the store accumulate series the in-memory caps already
// rerouted to __other__. Exactly one of CommitAdmission or RollbackAdmission
// must follow.
func (e *Engine) PlanAdmission(m DeltaMap) *AdmissionPlan {
	plan := &AdmissionPlan{Resolved: m}
	if len(m) == 0 {
		return plan
	}
	e.Rollover(e.now())

	cutoff := e.now().Unix() - int64(WindowSize/time.Second) - int64(AllowedLateness/time.Second)
	resolved := make(DeltaMap, len(m))
	for swk, d := range m {
		if swk.WindowStart <= cutoff {
			// The window's lateness horizon expired between reduction and
			// apply, so it is closed and no longer accepts points.
			e.seriesDiscarded.Add(1)
			continue
		}
		adm := e.limiter.Admit(swk.Key, swk.WindowStart)
		if adm.Overflowed {
			e.metrics.RecordOverflow(swk.Key.Signal, adm.Reason)
		}
		target := SeriesWindowKey{Key: adm.Key, WindowStart: swk.WindowStart}
		if adm.Reserved {
			plan.reserved = append(plan.reserved, target)
		}
		if existing, ok := resolved[target]; ok {
			// Two source series collapsed onto the same overflow series.
			// Totals are preserved; only identity detail is gone.
			existing.Merge(d)
			continue
		}
		resolved[target] = d
	}
	plan.Resolved = resolved
	return plan
}

// CommitAdmission folds a committed plan into the mutable windows and returns
// the new revision. The reservations become ordinary occupancy, released later
// by MarkFinalized like any other series in the window.
func (e *Engine) CommitAdmission(plan *AdmissionPlan) uint64 {
	if plan == nil || len(plan.Resolved) == 0 {
		return e.revision.Load()
	}
	// The shard writes and the ownership registration happen inside one
	// ownership critical section so a concurrent Ownership() never reports a
	// window as memory-owned before memory holds it, nor the reverse.
	e.own.mu.Lock()
	for i := range e.shards {
		e.applyShard(i, plan.Resolved)
	}
	for swk := range plan.Resolved {
		e.own.mutable[swk.WindowStart] = struct{}{}
	}
	rev := e.revision.Add(1)
	e.own.mu.Unlock()

	e.publishActiveSeries()
	return rev
}

// RollbackAdmission releases the occupancy a plan charged after its commit
// failed. Nothing reached the shards, so there is no window eviction that would
// ever release it — this is the only undo there is.
func (e *Engine) RollbackAdmission(plan *AdmissionPlan) {
	if plan == nil || len(plan.reserved) == 0 {
		return
	}
	for _, swk := range plan.reserved {
		e.limiter.Release(swk.Key, swk.WindowStart)
	}
	e.publishActiveSeries()
}

// applyShard folds every entry belonging to shard i into it, under that shard's
// lock and no other.
func (e *Engine) applyShard(i int, resolved DeltaMap) {
	sh := &e.shards[i]
	locked := false
	defer func() {
		if locked {
			sh.mu.Unlock()
		}
	}()
	for swk, d := range resolved {
		if shardIndex(swk.Key) != i {
			continue
		}
		if !locked {
			sh.mu.Lock()
			locked = true
		}
		w := sh.windows[swk.WindowStart]
		if w == nil {
			w = make(map[SeriesKey]*AggregateDelta)
			sh.windows[swk.WindowStart] = w
		}
		if cur, ok := w[swk.Key]; ok {
			cur.Merge(d)
			continue
		}
		w[swk.Key] = d.Clone()
	}
}

// Rollover CLOSES every window whose lateness horizon has expired and returns
// how many windows the closed-window cap forced out of memory — which is loss,
// and is normally zero.
//
// It does not evict. A closed window refuses new points (Admit drops deltas at
// or below the cutoff) but keeps its shard contents, its mutable ownership and
// its place below the watermark until a committed FinalizeWindow hands it over
// through MarkFinalized. Evicting here — the pre-#194 behaviour — advanced
// ownership to a store that had not yet materialized aggregate_buckets, so
// every read of that window skipped the delta log and returned nothing until
// the next successful finalize, and forever while the finalizer was failing.
//
// The one thing memory cannot do is grow without bound behind a wedged
// finalizer, so the closed set is capped. Past the cap the oldest closed
// windows are evicted the old way and counted: lossy, but no longer silent.
func (e *Engine) Rollover(now time.Time) int {
	cutoff := now.Unix() - int64(WindowSize/time.Second) - int64(AllowedLateness/time.Second)
	forced := 0
	var released []SeriesWindowKey
	e.own.mu.Lock()
	for start := range e.own.mutable {
		if start > cutoff {
			continue
		}
		e.own.closed[start] = struct{}{}
	}
	for _, start := range e.overCapClosedLocked() {
		released = append(released, e.evictWindowLocked(start)...)
		delete(e.own.closed, start)
		delete(e.own.mutable, start)
		if start > e.own.watermark {
			e.own.watermark = start
		}
		forced++
	}
	closed := len(e.own.closed)
	e.own.mu.Unlock()
	for _, swk := range released {
		e.limiter.Release(swk.Key, swk.WindowStart)
	}
	e.metrics.SetClosedWindows(closed)
	if forced > 0 {
		e.windowsDiscarded.Add(uint64(forced))
		e.seriesDiscarded.Add(uint64(len(released)))
		e.closedForced.Add(uint64(forced))
		for i := 0; i < forced; i++ {
			e.metrics.RecordClosedWindowEvicted()
		}
		e.publishActiveSeries()
	}
	return forced
}

// overCapClosedLocked returns the oldest closed windows that exceed the closed
// window cap, oldest first. e.own.mu must be held for writing.
func (e *Engine) overCapClosedLocked() []int64 {
	if e.maxClosedWindows < 0 {
		return nil
	}
	over := len(e.own.closed) - e.maxClosedWindows
	if over <= 0 {
		return nil
	}
	starts := make([]int64, 0, len(e.own.closed))
	for start := range e.own.closed {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	return starts[:over]
}

// publishActiveSeries pushes the limiter's occupancy into the active-series
// gauge.
func (e *Engine) publishActiveSeries() {
	ls := e.limiter.Stats()
	e.metrics.SetActiveSeries(ls.ActiveBySignal, ls.OverflowSeriesBySignal)
}

// recordReduction publishes one Export request's reduction accounting.
func (e *Engine) recordReduction(r *Reducer) {
	e.metrics.RecordReduction(r.stats, r.deltaCountBySignal())
}

// WindowSnapshot is one mutable window's contents.
type WindowSnapshot struct {
	// Start is the window's UTC start.
	Start time.Time
	// Series maps each active series in the window to a copy of its delta.
	Series map[SeriesKey]*AggregateDelta
}

// Snapshot is a consistent-enough view of the engine for tests and metrics.
//
// It walks the shards one at a time, so it is not a single atomic instant
// across shards. That is deliberate: an atomic cross-shard snapshot would need
// all four locks held at once, which is the one thing the shard design forbids.
type Snapshot struct {
	// Revision is the revision at the end of the walk.
	Revision uint64
	// Windows are the mutable windows, oldest first.
	Windows []WindowSnapshot
	// ActiveSeries and ActiveBySignal mirror the limiter's census.
	ActiveSeries   int
	ActiveBySignal map[Signal]int
	// Overflow counts admissions rerouted to an __other__ series, per reason.
	Overflow map[OverflowReason]uint64
	// WindowsDiscarded and SeriesDiscarded count rollover loss.
	WindowsDiscarded uint64
	SeriesDiscarded  uint64
	// ClosedWindowsForced counts closed-but-unfinalized windows the cap
	// forced out of memory. Every one is data the finalizer never
	// materialized, so a non-zero value means the finalizer is behind.
	ClosedWindowsForced uint64
	// ClosedWindows is how many closed-but-unfinalized windows memory holds.
	ClosedWindows int
}

// Snapshot returns a copy of the mutable window set.
func (e *Engine) Snapshot() Snapshot {
	byWindow := make(map[int64]map[SeriesKey]*AggregateDelta)
	for i := range e.shards {
		sh := &e.shards[i]
		sh.mu.Lock()
		for start, w := range sh.windows {
			dst := byWindow[start]
			if dst == nil {
				dst = make(map[SeriesKey]*AggregateDelta, len(w))
				byWindow[start] = dst
			}
			for key, d := range w {
				dst[key] = d.Clone()
			}
		}
		sh.mu.Unlock()
	}

	starts := make([]int64, 0, len(byWindow))
	for start := range byWindow {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	windows := make([]WindowSnapshot, 0, len(starts))
	for _, start := range starts {
		windows = append(windows, WindowSnapshot{
			Start:  time.Unix(start, 0).UTC(),
			Series: byWindow[start],
		})
	}

	ls := e.limiter.Stats()
	e.own.mu.RLock()
	closed := len(e.own.closed)
	e.own.mu.RUnlock()
	return Snapshot{
		Revision:            e.revision.Load(),
		Windows:             windows,
		ActiveSeries:        ls.Active,
		ActiveBySignal:      ls.ActiveBySignal,
		Overflow:            ls.Overflow,
		WindowsDiscarded:    e.windowsDiscarded.Load(),
		SeriesDiscarded:     e.seriesDiscarded.Load(),
		ClosedWindowsForced: e.closedForced.Load(),
		ClosedWindows:       closed,
	}
}

// Totals sums one signal's counters across every mutable window. It is the
// shadow-mode comparison primitive: the same input stream must produce the same
// totals regardless of the sampling rate.
func (s Snapshot) Totals(signal Signal) (count, errors uint64) {
	for _, w := range s.Windows {
		for key, d := range w.Series {
			if key.Signal != signal {
				continue
			}
			count += d.Count
			errors += d.ErrorCount
		}
	}
	return count, errors
}

// String implements fmt.Stringer for test failures.
func (s Snapshot) String() string {
	return fmt.Sprintf("Snapshot{revision=%d windows=%d active=%d discarded_windows=%d}",
		s.Revision, len(s.Windows), s.ActiveSeries, s.WindowsDiscarded)
}

// shardIndex maps a series to its shard. FNV-1a over the identity fields,
// unrolled so it stays allocation-free and inlinable.
func shardIndex(k SeriesKey) int {
	const (
		offset = uint32(2166136261)
		prime  = uint32(16777619)
	)
	h := offset
	for _, v := range [...]uint32{
		k.TenantID,
		k.ServiceID,
		k.NameID,
		k.DimsID,
		uint32(k.Signal)<<24 | uint32(k.StatusClass)<<16 | uint32(k.HTTPClass)<<8 | uint32(k.Method),
		uint32(k.Variant),
	} {
		h ^= v & 0xff
		h *= prime
		h ^= (v >> 8) & 0xff
		h *= prime
		h ^= (v >> 16) & 0xff
		h *= prime
		h ^= v >> 24
		h *= prime
	}
	return int(h & (NumShards - 1))
}
