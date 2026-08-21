package aggregate

import (
	"fmt"
	"sort"
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
// ROLLOVER IS AN EVICTION, NOT A DELETION — once the durable store is wired
// (#173). A window leaving the mutable set is dropped from the shards, and the
// writer's finalize pass has already materialized it from the delta log into
// aggregate_buckets; finalized history lives on disk and never comes back into
// RAM. WITHOUT a store (no writer installed, engine constructed directly in a
// test) rollover really is loss: the counts in the expiring window are gone,
// which is why nothing may be presented to a user as authoritative unless the
// group-commit writer is the applier.
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

	// Miner is the ingest-owned template miner (#163). Defaults to one built
	// from the log-template cap.
	Miner *TemplateMiner

	// Metrics is the Prometheus recorder. nil disables metric recording.
	Metrics MetricsRecorder

	// MetricDims maps metric names to their configured aggregation dimension keys.
	// Nil or empty means no metrics are configured for custom dimensions.
	MetricDims DimsConfig

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

	shards [NumShards]shard

	applier  atomic.Pointer[Applier]
	revision atomic.Uint64

	windowsDiscarded atomic.Uint64
	seriesDiscarded  atomic.Uint64
}

// shard is one mutex-guarded slice of the mutable window set.
type shard struct {
	mu      sync.Mutex
	windows map[int64]map[SeriesKey]*AggregateDelta
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
	e := &Engine{
		mode:    cfg.Mode,
		now:     cfg.Now,
		cache:   NewCache(reg),
		miner:   cfg.Miner,
		metrics: cfg.Metrics,
		dims:    cfg.MetricDims,
	}
	if e.metrics == nil {
		e.metrics = noopRecorder{}
	}
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

// Revision returns the current revision. It increases by one on every applied
// batch and never decreases, so a consumer can tell "nothing changed" from
// "changed back to the same numbers" (#163's replacement-by-revision topology).
func (e *Engine) Revision() uint64 { return e.revision.Load() }

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
	tenantID := e.cache.InternTenant(r.Tenant)
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
	if r == nil {
		return e.revision.Load()
	}
	e.recordReduction(r)
	return e.ApplyDeltas(r.Deltas())
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
	return e.ApplyDeltasErr(r.Deltas())
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
	resolved := e.Admit(m)

	for i := range e.shards {
		e.applyShard(i, resolved)
	}

	rev := e.revision.Add(1)
	e.publishActiveSeries()
	return rev
}

// Admit rolls the mutable window set forward and resolves one batch of deltas
// against the cardinality budget WITHOUT touching the shards.
//
// The durable writer (#173) calls it before its COMMIT so the row that becomes
// durable carries the same identity the shards will later hold: admitting after
// the write would let the store accumulate series the in-memory caps already
// rerouted to __other__. Admission is idempotent for a series already present
// in a window, so the ApplyCommitted that follows the commit re-resolves the
// same map to itself.
func (e *Engine) Admit(m DeltaMap) DeltaMap {
	if len(m) == 0 {
		return m
	}
	e.Rollover(e.now())

	cutoff := e.now().Unix() - int64(WindowSize/time.Second) - int64(AllowedLateness/time.Second)
	resolved := make(DeltaMap, len(m))
	for swk, d := range m {
		if swk.WindowStart <= cutoff {
			// The window's lateness horizon expired between reduction and
			// apply. Recreating it here would only have it discarded at the
			// next rollover.
			e.seriesDiscarded.Add(1)
			continue
		}
		adm := e.limiter.Admit(swk.Key, swk.WindowStart)
		if adm.Overflowed {
			e.metrics.RecordOverflow(swk.Key.Signal, adm.Reason)
		}
		target := SeriesWindowKey{Key: adm.Key, WindowStart: swk.WindowStart}
		if existing, ok := resolved[target]; ok {
			// Two source series collapsed onto the same overflow series.
			// Totals are preserved; only identity detail is gone.
			existing.Merge(d)
			continue
		}
		resolved[target] = d
	}
	return resolved
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

// Rollover evicts every window whose lateness horizon has expired and returns
// how many it dropped.
//
// With the durable store wired, the window has already been finalized into
// aggregate_buckets by the writer's finalize pass, so this is an eviction from
// RAM. Without a store it is loss. See the file header.
func (e *Engine) Rollover(now time.Time) int {
	cutoff := now.Unix() - int64(WindowSize/time.Second) - int64(AllowedLateness/time.Second)
	dropped := 0
	var released []SeriesWindowKey
	for i := range e.shards {
		sh := &e.shards[i]
		sh.mu.Lock()
		for start, w := range sh.windows {
			if start > cutoff {
				continue
			}
			for key := range w {
				released = append(released, SeriesWindowKey{Key: key, WindowStart: start})
			}
			delete(sh.windows, start)
			dropped++
		}
		sh.mu.Unlock()
	}
	for _, swk := range released {
		e.limiter.Release(swk.Key, swk.WindowStart)
	}
	if dropped > 0 {
		e.windowsDiscarded.Add(uint64(dropped))
		e.seriesDiscarded.Add(uint64(len(released)))
		e.publishActiveSeries()
	}
	return dropped
}

// publishActiveSeries pushes the limiter's occupancy into the active-series
// gauge.
func (e *Engine) publishActiveSeries() {
	e.metrics.SetActiveSeries(e.limiter.Stats().ActiveBySignal)
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
	// WindowsDiscarded and SeriesDiscarded count Phase 1 rollover loss.
	WindowsDiscarded uint64
	SeriesDiscarded  uint64
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
	return Snapshot{
		Revision:         e.revision.Load(),
		Windows:          windows,
		ActiveSeries:     ls.Active,
		ActiveBySignal:   ls.ActiveBySignal,
		Overflow:         ls.Overflow,
		WindowsDiscarded: e.windowsDiscarded.Load(),
		SeriesDiscarded:  e.seriesDiscarded.Load(),
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
