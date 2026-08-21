package ingest

import (
	"hash/fnv"
	"math"
	"sync"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// Bounded exemplar retention (#176, policy frozen in #161, quality contract in
// #163).
//
// In AGGREGATE_MODE=aggregate the adaptive Sampler is retired and this policy
// is the ONLY gate on raw persistence. It sits AFTER aggregate reduction so the
// invariant that matters during an outage holds unconditionally:
//
//	aggregate counts : 100% of accepted telemetry
//	raw exemplars    : bounded by count AND bytes, regardless of severity
//
// An error storm therefore raises aggregate error counts and leaves raw
// persistence flat. "Always eligible" never means "always persisted".
//
// Selection properties (all four are load-bearing, #161):
//
//   - Deterministic: the verdict is a pure function of hash(traceID) and the
//     window's bounded selection state. No RNG, no clock.
//   - Arrival-order independent: healthy traces use a stateless hash threshold;
//     errors/slow use per-stratum top-K by SMALLEST hash, so the set that wins a
//     window is the same set regardless of the order the spans showed up in.
//   - Retry-stable: #160's at-least-once redelivery re-derives the same verdict.
//   - Duplicate-safe: a duplicate re-selects the same slot instead of consuming
//     a second one.
//
// Boundary behaviour, stated honestly: selection state is bounded, so a
// later-arriving trace with a smaller hash can displace an already-selected
// trace. Spans of the displaced trace that were ALREADY handed to persistence
// are not deleted — the policy has no delete path and would not use one if it
// had. The effect is bounded OVER-retention (at most one extra trace's already
// persisted spans per eviction), never under-retention of a better-ranked
// trace's future spans. Every displacement is counted as
// otelcontext_exemplar_eviction_total so operators can see the slack.

// Exemplar priority classes. Lower value = higher priority when the unified
// per-service budget is under pressure: ERROR/FATAL displaces slow, slow
// displaces healthy, and nothing displaces an error (#161).
const (
	exemplarClassError   = 0
	exemplarClassSlow    = 1
	exemplarClassHealthy = 2
	exemplarClassCount   = 3
)

// exemplarClassName maps a class to its metric label.
func exemplarClassName(class int) string {
	switch class {
	case exemplarClassError:
		return "error"
	case exemplarClassSlow:
		return "slow"
	default:
		return "healthy"
	}
}

// Drop reasons, published as otelcontext_exemplar_dropped_total{reason}.
const (
	exemplarReasonBudgetCount = "budget_count"
	exemplarReasonBudgetBytes = "budget_bytes"
	exemplarReasonStratum     = "stratum"
)

// ExemplarMetrics is the policy's view of the metric surface. It mirrors the
// aggregate package's recorder indirection so tests (and any caller passing a
// nil *telemetry.Metrics) do not need a live Prometheus registry.
type ExemplarMetrics interface {
	RecordExemplarEligible(signal, class string)
	RecordExemplarDropped(signal, reason string)
	RecordExemplarEviction()
	RecordExemplarTruncation()
}

// noopExemplarMetrics is the default when no recorder is wired.
type noopExemplarMetrics struct{}

func (noopExemplarMetrics) RecordExemplarEligible(string, string) {}
func (noopExemplarMetrics) RecordExemplarDropped(string, string)  {}
func (noopExemplarMetrics) RecordExemplarEviction()               {}
func (noopExemplarMetrics) RecordExemplarTruncation()             {}

// ExemplarConfig carries the frozen #161 budgets. Zero fields fall back to the
// defaults so tests can construct a policy with one or two overrides.
type ExemplarConfig struct {
	// TracesPerServiceWindow is the unified per-service/window trace budget
	// filled by priority ERROR/FATAL > slow > healthy. Default 25.
	TracesPerServiceWindow int
	// TracesGlobalWindow bounds selected traces across all services in one
	// window. Default 1500.
	TracesGlobalWindow int
	// BytesPerServiceWindow / BytesGlobalWindow bound the bytes actually handed
	// to persistence. Counts and bytes both bind; first breach wins.
	BytesPerServiceWindow int64 // Default 512 KiB
	BytesGlobalWindow     int64 // Default 8 MiB
	// HealthyRate is the stateless hash-threshold eligibility target for
	// healthy traces. Caps dominate under load. Default 0.005 (0.5%).
	HealthyRate float64
	// StratumTopK bounds how many exemplars one (operation × status class)
	// stratum may hold, so a single repeated failure cannot monopolize the
	// per-service budget. Default 5.
	StratumTopK int
	// LatencyThresholdMs is the shared definition of "slow"
	// (SAMPLING_LATENCY_THRESHOLD_MS). A predicate, not a policy — it survives
	// the sampler's retirement in every mode (#161).
	LatencyThresholdMs float64
	// LogsErrorPerServiceWindow is the raw ERROR/FATAL log budget per
	// service/window. Default 50.
	LogsErrorPerServiceWindow int
	// LogsWarnEnabled opts WARN logs into raw retention. Off by default.
	LogsWarnEnabled bool
	// LogsWarnPerServiceWindow is the WARN budget when enabled. Default 20.
	LogsWarnPerServiceWindow int
	// MaxSpansPerTrace / MaxBytesPerTrace bound one retained trace so the
	// complete-retained-trace contract (#163) cannot be turned into an
	// unbounded write by a pathological trace. Breaching either forces
	// truncation, which is persisted as truncated=true plus retained/observed
	// span counts.
	MaxSpansPerTrace int   // Default 500
	MaxBytesPerTrace int64 // Default 256 KiB

	// WindowSize is the tumbling budget window. Defaults to the aggregate
	// engine's window so exemplar budgets and aggregate buckets share edges.
	WindowSize time.Duration
	// Metrics receives the policy's counters. nil = no-op.
	Metrics ExemplarMetrics
}

// Exemplar budget defaults, frozen in #161's resolution.
const (
	DefaultExemplarTracesPerServiceWindow    = 25
	DefaultExemplarTracesGlobalWindow        = 1500
	DefaultExemplarBytesPerServiceWindow     = 512 * 1024
	DefaultExemplarBytesGlobalWindow         = 8 * 1024 * 1024
	DefaultExemplarHealthyRate               = 0.005
	DefaultExemplarStratumTopK               = 5
	DefaultExemplarLogsErrorPerServiceWindow = 50
	DefaultExemplarLogsWarnPerServiceWindow  = 20
	DefaultExemplarMaxSpansPerTrace          = 500
	DefaultExemplarMaxBytesPerTrace          = 256 * 1024
)

func (c *ExemplarConfig) applyDefaults() {
	if c.TracesPerServiceWindow <= 0 {
		c.TracesPerServiceWindow = DefaultExemplarTracesPerServiceWindow
	}
	if c.TracesGlobalWindow <= 0 {
		c.TracesGlobalWindow = DefaultExemplarTracesGlobalWindow
	}
	if c.BytesPerServiceWindow <= 0 {
		c.BytesPerServiceWindow = DefaultExemplarBytesPerServiceWindow
	}
	if c.BytesGlobalWindow <= 0 {
		c.BytesGlobalWindow = DefaultExemplarBytesGlobalWindow
	}
	if c.HealthyRate < 0 {
		c.HealthyRate = 0
	}
	if c.HealthyRate > 1 {
		c.HealthyRate = 1
	}
	if c.StratumTopK <= 0 {
		c.StratumTopK = DefaultExemplarStratumTopK
	}
	if c.LogsErrorPerServiceWindow <= 0 {
		c.LogsErrorPerServiceWindow = DefaultExemplarLogsErrorPerServiceWindow
	}
	if c.LogsWarnPerServiceWindow <= 0 {
		c.LogsWarnPerServiceWindow = DefaultExemplarLogsWarnPerServiceWindow
	}
	if c.MaxSpansPerTrace <= 0 {
		c.MaxSpansPerTrace = DefaultExemplarMaxSpansPerTrace
	}
	if c.MaxBytesPerTrace <= 0 {
		c.MaxBytesPerTrace = DefaultExemplarMaxBytesPerTrace
	}
	if c.WindowSize <= 0 {
		c.WindowSize = aggregate.WindowSize
	}
	if c.Metrics == nil {
		c.Metrics = noopExemplarMetrics{}
	}
}

// traceState is the per-selected-trace accounting behind the
// complete-retained-trace contract (#163). Only SELECTED traces get one, so the
// map is bounded by the per-service budget.
type traceState struct {
	hash      uint64
	class     int
	stratum   string
	observed  int   // spans of this trace seen by the policy
	retained  int   // spans actually handed to persistence
	bytes     int64 // bytes actually handed to persistence
	truncated bool
	evicted   bool // displaced by a better-ranked trace; future spans stop
}

// stratumState holds the top-K smallest hashes selected for one
// (operation × status class) stratum. K is small (default 5), so a linear scan
// beats a heap and keeps eviction order obvious.
type stratumState struct {
	members map[string]uint64 // traceID -> hash
}

// serviceWindow is one (service, window) budget cell.
type serviceWindow struct {
	traces  map[string]*traceState
	strata  map[string]*stratumState
	perName [exemplarClassCount]int
	bytes   int64
	logs    [2]int // [0] = ERROR/FATAL raw logs, [1] = WARN raw logs
}

func newServiceWindow() *serviceWindow {
	return &serviceWindow{
		traces: make(map[string]*traceState),
		strata: make(map[string]*stratumState),
	}
}

func (sw *serviceWindow) selectedCount() int {
	return sw.perName[exemplarClassError] + sw.perName[exemplarClassSlow] + sw.perName[exemplarClassHealthy]
}

// globalWindow carries the instance-wide budgets for one window.
type globalWindow struct {
	traces int
	bytes  int64
}

// windowKey identifies a (tenant, service, window) budget cell. Budgets are
// per-tenant as well as per-service: a noisy tenant must not spend another
// tenant's exemplar slots.
type windowKey struct {
	tenant  string
	service string
	window  int64
}

// ExemplarPolicy is the bounded raw-retention gate for aggregate mode.
//
// Concurrency: state is sharded by service so the 100–200-service target does
// not funnel every span through one mutex. Global (instance-wide) budgets live
// behind their own small mutex, taken only when a shard actually wants to
// reserve or release a slot — never on the common "healthy trace fails the hash
// threshold" path.
type ExemplarPolicy struct {
	cfg ExemplarConfig

	healthyThreshold uint64 // hash(traceID) < this => healthy-eligible

	shards [exemplarShardCount]exemplarShard

	globalMu sync.Mutex
	global   map[int64]*globalWindow
}

const exemplarShardCount = 16

type exemplarShard struct {
	mu       sync.Mutex
	services map[windowKey]*serviceWindow
}

// NewExemplarPolicy builds a policy from cfg, filling unset fields with the
// #161 defaults.
func NewExemplarPolicy(cfg ExemplarConfig) *ExemplarPolicy {
	cfg.applyDefaults()
	p := &ExemplarPolicy{
		cfg:    cfg,
		global: make(map[int64]*globalWindow),
	}
	// Threshold in the full uint64 hash space. rate=0 disables healthy
	// eligibility entirely; rate=1 makes every healthy trace eligible (the
	// count/byte budgets still bind).
	p.healthyThreshold = uint64(cfg.HealthyRate * math.Pow(2, 64))
	if cfg.HealthyRate >= 1 {
		p.healthyThreshold = math.MaxUint64
	}
	if cfg.HealthyRate <= 0 {
		p.healthyThreshold = 0
	}
	for i := range p.shards {
		p.shards[i].services = make(map[windowKey]*serviceWindow)
	}
	return p
}

// exemplarHash is the trace-ID hash the whole policy ranks on. FNV-1a over the
// hex trace ID: stable across processes and restarts (no per-process seed), so
// two replicas fed the same trace reach the same verdict.
func exemplarHash(traceID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(traceID))
	return h.Sum64()
}

// windowOf aligns t onto the policy's tumbling window.
func (p *ExemplarPolicy) windowOf(t time.Time) int64 {
	w := int64(p.cfg.WindowSize / time.Second)
	if w <= 0 {
		w = int64(aggregate.WindowSize / time.Second)
	}
	sec := t.Unix()
	rem := sec % w
	if rem < 0 {
		rem += w
	}
	return sec - rem
}

func (p *ExemplarPolicy) shardFor(service string) *exemplarShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(service))
	return &p.shards[h.Sum32()%exemplarShardCount]
}

// ExemplarSpan is one span offered to the policy. Everything here is already
// computed on the hot path, so admission adds no parsing.
type ExemplarSpan struct {
	Tenant     string
	Service    string
	TraceID    string
	Operation  string
	Status     string
	DurationMs float64
	Timestamp  time.Time
}

func (in ExemplarSpan) class(latencyThresholdMs float64) int {
	if in.Status == storage.StatusCodeError {
		return exemplarClassError
	}
	// Overlap is resolved here, once: an error that is ALSO slow consumes one
	// slot at error priority, never two (#161).
	if latencyThresholdMs > 0 && in.DurationMs >= latencyThresholdMs {
		return exemplarClassSlow
	}
	return exemplarClassHealthy
}

// AdmitSpan reports whether this span's raw row may be built and offered to
// persistence. It is the count-budget half of admission; ChargeSpan meters the
// bytes actually produced.
//
// A trace already selected in this window always admits — that is the
// complete-retained-trace contract, and it is what makes multi-batch traces and
// at-least-once retries cohere with zero coordination.
func (p *ExemplarPolicy) AdmitSpan(in ExemplarSpan) bool {
	window := p.windowOf(in.Timestamp)
	class := in.class(p.cfg.LatencyThresholdMs)
	h := exemplarHash(in.TraceID)

	sh := p.shardFor(in.Service)
	sh.mu.Lock()
	sw := sh.window(windowKey{tenant: in.Tenant, service: in.Service, window: window})

	// Already decided? Duplicates and later spans of the same trace re-select
	// the same slot.
	if st, ok := sw.traces[in.TraceID]; ok {
		if st.evicted {
			sh.mu.Unlock()
			return false
		}
		// Status upgrade: a trace admitted as healthy that turns out to carry
		// an error keeps its slot but is re-priced at the higher class, so
		// budget pressure displaces it last rather than first.
		if class < st.class {
			sw.perName[st.class]--
			sw.perName[class]++
			st.class = class
		}
		if st.observed >= p.cfg.MaxSpansPerTrace {
			// Bounded trace: stop admitting, record truncation once.
			st.observed++
			p.markTruncated(st)
			sh.mu.Unlock()
			p.cfg.Metrics.RecordExemplarDropped("traces", exemplarReasonBudgetCount)
			return false
		}
		st.observed++
		sh.mu.Unlock()
		return true
	}

	// Not yet selected. Eligibility first, budget second.
	if class == exemplarClassHealthy {
		if p.healthyThreshold == 0 || h >= p.healthyThreshold {
			sh.mu.Unlock()
			return false
		}
	} else if !p.stratumAdmits(sw, in.Operation, class, h) {
		// Errors and slow spans are ALWAYS eligible — that is the whole point
		// of #161's wording — but eligibility is not retention.
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarEligible("traces", exemplarClassName(class))
		p.cfg.Metrics.RecordExemplarDropped("traces", exemplarReasonStratum)
		return false
	}
	p.cfg.Metrics.RecordExemplarEligible("traces", exemplarClassName(class))

	// Reserve the instance-wide slot before displacing anything, so global
	// exhaustion can never cost an already-selected exemplar its slot.
	if !p.reserveGlobalTrace(window) {
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("traces", exemplarReasonBudgetCount)
		return false
	}

	// Stratum displacement first: a full stratum gives up its worst-ranked
	// member, which also frees the service slot that member was holding.
	stratum := ""
	if class != exemplarClassHealthy {
		stratum = exemplarStratum(in.Operation, class)
		if ss, ok := sw.strata[stratum]; ok && len(ss.members) >= p.cfg.StratumTopK {
			if worstID, _ := stratumWorst(ss); worstID != "" {
				p.evictLocked(sw, window, worstID)
			}
		}
	}

	// Unified per-service budget with priority fill.
	if sw.selectedCount() >= p.cfg.TracesPerServiceWindow {
		victim := sw.lowestPriorityVictim(class, h)
		if victim == "" {
			p.releaseGlobalTrace(window)
			sh.mu.Unlock()
			p.cfg.Metrics.RecordExemplarDropped("traces", exemplarReasonBudgetCount)
			return false
		}
		p.evictLocked(sw, window, victim)
	}

	st := &traceState{hash: h, class: class, observed: 1, stratum: stratum}
	if stratum != "" {
		ss, ok := sw.strata[stratum]
		if !ok {
			ss = &stratumState{members: make(map[string]uint64, p.cfg.StratumTopK)}
			sw.strata[stratum] = ss
		}
		ss.members[in.TraceID] = h
	}
	sw.traces[in.TraceID] = st
	sw.perName[class]++
	sh.mu.Unlock()
	return true
}

// ChargeSpan meters the bytes a retained span actually hands to persistence and
// reports whether it fits. A false return means the row must be dropped: the
// byte budget bound before the count budget did, and the trace is marked
// truncated so the gap is visible in the persisted data rather than inferred.
func (p *ExemplarPolicy) ChargeSpan(tenant, service, traceID string, ts time.Time, size int) bool {
	window := p.windowOf(ts)
	sh := p.shardFor(service)
	sh.mu.Lock()
	sw := sh.window(windowKey{tenant: tenant, service: service, window: window})
	st, ok := sw.traces[traceID]
	if !ok || st.evicted {
		sh.mu.Unlock()
		return false
	}
	n := int64(size)
	if st.bytes+n > p.cfg.MaxBytesPerTrace || sw.bytes+n > p.cfg.BytesPerServiceWindow {
		p.markTruncated(st)
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("traces", exemplarReasonBudgetBytes)
		return false
	}
	if !p.reserveGlobalBytes(window, n) {
		p.markTruncated(st)
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("traces", exemplarReasonBudgetBytes)
		return false
	}
	st.bytes += n
	st.retained++
	sw.bytes += n
	sh.mu.Unlock()
	return true
}

// markTruncated flips a trace to truncated exactly once (counter hygiene).
// Caller holds the shard lock.
func (p *ExemplarPolicy) markTruncated(st *traceState) {
	if st.truncated {
		return
	}
	st.truncated = true
	p.cfg.Metrics.RecordExemplarTruncation()
}

// ExemplarTraceStats is the persisted-side view of a retained trace.
type ExemplarTraceStats struct {
	Truncated bool
	Retained  int
	Observed  int
}

// TraceStats returns the accounting for a selected trace so the caller can
// stamp truncated / retained / observed onto the persisted trace row (#163).
// The second return is false when the trace was never selected.
func (p *ExemplarPolicy) TraceStats(tenant, service, traceID string, ts time.Time) (ExemplarTraceStats, bool) {
	window := p.windowOf(ts)
	sh := p.shardFor(service)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sw, ok := sh.services[windowKey{tenant: tenant, service: service, window: window}]
	if !ok {
		return ExemplarTraceStats{}, false
	}
	st, ok := sw.traces[traceID]
	if !ok {
		return ExemplarTraceStats{}, false
	}
	return ExemplarTraceStats{Truncated: st.truncated, Retained: st.retained, Observed: st.observed}, true
}

// AdmitLog reports whether a raw log row may be persisted in aggregate mode.
// INFO/DEBUG are aggregate-only and never raw; ERROR/FATAL get a per-service
// window budget; WARN is opt-in (#161). Bytes are charged to the same
// service/global byte budgets as spans — one pool, so a log flood cannot buy
// itself past the trace budget.
//
// size is the variable-length payload (body + serialized attributes); the
// fixed row overhead is added here so callers do not have to know it.
func (p *ExemplarPolicy) AdmitLog(tenant, service, severity string, ts time.Time, size int) bool {
	slot, ok := p.logSlot(severity)
	if !ok {
		// Not eligible at all — not an exemplar drop, never was one.
		return false
	}
	budget := p.cfg.LogsErrorPerServiceWindow
	class := "error"
	if slot == 1 {
		budget = p.cfg.LogsWarnPerServiceWindow
		class = "warn"
	}
	p.cfg.Metrics.RecordExemplarEligible("logs", class)

	window := p.windowOf(ts)
	sh := p.shardFor(service)
	sh.mu.Lock()
	sw := sh.window(windowKey{tenant: tenant, service: service, window: window})
	if sw.logs[slot] >= budget {
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("logs", exemplarReasonBudgetCount)
		return false
	}
	n := int64(size + logRowFixedBytes)
	if sw.bytes+n > p.cfg.BytesPerServiceWindow {
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("logs", exemplarReasonBudgetBytes)
		return false
	}
	if !p.reserveGlobalBytes(window, n) {
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("logs", exemplarReasonBudgetBytes)
		return false
	}
	sw.logs[slot]++
	sw.bytes += n
	sh.mu.Unlock()
	return true
}

// AllowSynthesizedLog gates logs synthesized from span events / span status.
// These ride along with a span the policy already admitted and budgeted, so
// only the severity floor applies — re-budgeting them would punch holes in the
// complete-retained-trace contract for no bytes saved.
func (p *ExemplarPolicy) AllowSynthesizedLog(severity string) bool {
	_, ok := p.logSlot(severity)
	return ok
}

// logSlot maps a severity onto its raw-retention slot: 0 = ERROR/FATAL,
// 1 = WARN (opt-in). ok=false means aggregate-only. Classification reuses
// shouldIngestSeverity so the policy and the ingest severity gate can never
// disagree about what "ERROR" looks like on the wire.
func (p *ExemplarPolicy) logSlot(severity string) (int, bool) {
	switch {
	case shouldIngestSeverity(severity, 40):
		return 0, true
	case shouldIngestSeverity(severity, 30):
		// WARN. INFO/DEBUG and unknown severities are aggregate-only (#161).
		if !p.cfg.LogsWarnEnabled {
			return 0, false
		}
		return 1, true
	default:
		return 0, false
	}
}

// window returns (creating if needed) the budget cell for key, pruning windows
// that can no longer receive data. Caller holds the shard lock.
func (sh *exemplarShard) window(key windowKey) *serviceWindow {
	if sw, ok := sh.services[key]; ok {
		return sw
	}
	// Bounded state: drop every cell for this (tenant, service) older than the
	// incoming window. Windows only ever move forward for a given service, and
	// the aggregate engine's lateness horizon already rejects points from
	// windows this far back.
	for k := range sh.services {
		if k.tenant == key.tenant && k.service == key.service && k.window < key.window {
			delete(sh.services, k)
		}
	}
	sw := newServiceWindow()
	sh.services[key] = sw
	return sw
}

// exemplarStratum is the (operation × status class) stratum key. Stratifying
// stops one repeated failure from consuming every slot while the other ninety
// operations of the service go unrepresented.
func exemplarStratum(operation string, class int) string {
	return exemplarClassName(class) + "\x00" + operation
}

// stratumAdmits reports whether hash h ranks into the stratum's top-K smallest.
// A stratum below K always admits. Caller holds the shard lock.
func (p *ExemplarPolicy) stratumAdmits(sw *serviceWindow, operation string, class int, h uint64) bool {
	st, ok := sw.strata[exemplarStratum(operation, class)]
	if !ok || len(st.members) < p.cfg.StratumTopK {
		return true
	}
	// Full: admit only if strictly better than the current worst. The displaced
	// trace's re-evaluation then yields "reject" for the same reason, which is
	// what keeps every verdict recomputable without tombstones.
	_, worst := stratumWorst(st)
	return h < worst
}

// stratumWorst returns the member with the LARGEST hash — the one a better
// candidate displaces. Ties break on trace ID so the choice is deterministic
// regardless of map iteration order.
func stratumWorst(st *stratumState) (string, uint64) {
	var worstID string
	var worst uint64
	for id, h := range st.members {
		if worstID == "" || h > worst || (h == worst && id > worstID) {
			worstID, worst = id, h
		}
	}
	return worstID, worst
}

// lowestPriorityVictim picks the trace to displace so a higher-priority class
// can take a slot in a full service window: the worst-ranked member of the
// lowest-priority class strictly below the incoming class. Returns "" when
// nothing may be displaced — the budget then simply refuses, which is exactly
// the behaviour that keeps an error storm from becoming a write storm.
func (sw *serviceWindow) lowestPriorityVictim(incoming int, incomingHash uint64) string {
	for class := exemplarClassHealthy; class > incoming; class-- {
		if sw.perName[class] == 0 {
			continue
		}
		var victim string
		var worst uint64
		for id, st := range sw.traces {
			if st.evicted || st.class != class {
				continue
			}
			if victim == "" || st.hash > worst || (st.hash == worst && id > victim) {
				victim, worst = id, st.hash
			}
		}
		if victim != "" {
			return victim
		}
	}
	// Same class, full window: rank decides. Displace the worst member only
	// when the newcomer is strictly better, so the retained set for a window
	// stays a pure function of the hashes offered to it.
	if sw.perName[incoming] > 0 {
		var victim string
		var worst uint64
		for id, st := range sw.traces {
			if st.evicted || st.class != incoming {
				continue
			}
			if victim == "" || st.hash > worst || (st.hash == worst && id > victim) {
				victim, worst = id, st.hash
			}
		}
		if victim != "" && incomingHash < worst {
			return victim
		}
	}
	return ""
}

// evictLocked displaces a selected trace. Spans of the evicted trace that were
// already handed to persistence stay persisted — this is the bounded
// over-retention documented at the top of the file, and it is counted rather
// than hidden. Caller holds the shard lock.
func (p *ExemplarPolicy) evictLocked(sw *serviceWindow, window int64, traceID string) {
	st, ok := sw.traces[traceID]
	if !ok || st.evicted {
		return
	}
	st.evicted = true
	sw.perName[st.class]--
	if st.stratum != "" {
		if s, ok := sw.strata[st.stratum]; ok {
			delete(s.members, traceID)
			if len(s.members) == 0 {
				delete(sw.strata, st.stratum)
			}
		}
	}
	// The evicted trace keeps its traceState (so its own future spans are
	// refused deterministically) but returns its global slot; its already
	// charged bytes are NOT refunded, because those bytes really were written.
	p.releaseGlobalTrace(window)
	p.cfg.Metrics.RecordExemplarEviction()
	p.pruneEvictedLocked(sw)
}

// pruneEvictedLocked bounds the displaced-trace bookkeeping.
//
// A displaced trace is kept only so its own later spans are refused without a
// second look. Dropping the record is safe because the verdict is recomputable:
// a window's selected set only ever improves (hashes only get smaller, strata
// only fill), so a trace displaced once can never re-qualify — re-evaluating it
// from scratch returns the same "reject". Without this the map would grow with
// admission churn instead of with the budget. Caller holds the shard lock.
func (p *ExemplarPolicy) pruneEvictedLocked(sw *serviceWindow) {
	if len(sw.traces) <= 8*p.cfg.TracesPerServiceWindow {
		return
	}
	for id, st := range sw.traces {
		if st.evicted {
			delete(sw.traces, id)
		}
	}
}

// reserveGlobalTrace takes one instance-wide trace slot for the window.
func (p *ExemplarPolicy) reserveGlobalTrace(window int64) bool {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	gw := p.globalFor(window)
	if gw.traces >= p.cfg.TracesGlobalWindow {
		return false
	}
	gw.traces++
	return true
}

// releaseGlobalTrace returns one instance-wide slot for the window. Bytes are
// deliberately not refunded — those bytes really were written.
func (p *ExemplarPolicy) releaseGlobalTrace(window int64) {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	if gw, ok := p.global[window]; ok && gw.traces > 0 {
		gw.traces--
	}
}

// reserveGlobalBytes takes n bytes from the instance-wide window budget.
func (p *ExemplarPolicy) reserveGlobalBytes(window int64, n int64) bool {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	gw := p.globalFor(window)
	if gw.bytes+n > p.cfg.BytesGlobalWindow {
		return false
	}
	gw.bytes += n
	return true
}

// globalFor returns the global cell for a window, pruning older ones. Caller
// holds globalMu.
func (p *ExemplarPolicy) globalFor(window int64) *globalWindow {
	if gw, ok := p.global[window]; ok {
		return gw
	}
	for w := range p.global {
		if w < window {
			delete(p.global, w)
		}
	}
	gw := &globalWindow{}
	p.global[window] = gw
	return gw
}

// SelectedTraces returns the trace IDs currently selected in the window
// containing ts, for a (tenant, service). Test/diagnostic surface: this is the
// set the determinism property is stated over.
func (p *ExemplarPolicy) SelectedTraces(tenant, service string, ts time.Time) []string {
	window := p.windowOf(ts)
	sh := p.shardFor(service)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sw, ok := sh.services[windowKey{tenant: tenant, service: service, window: window}]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(sw.traces))
	for id, st := range sw.traces {
		if !st.evicted {
			out = append(out, id)
		}
	}
	return out
}

// spanRowBytes estimates the bytes a span row hands to persistence. It counts
// what is actually written — the variable-length columns plus a fixed overhead
// for the numeric/timestamp columns — rather than the wire size of the OTLP
// message, which is what the byte budget is supposed to bound.
const spanRowFixedBytes = 96

func spanRowBytes(s *storage.Span) int {
	return spanRowFixedBytes +
		len(s.TenantID) + len(s.TraceID) + len(s.SpanID) + len(s.ParentSpanID) +
		len(s.OperationName) + len(s.ServiceName) + len(s.Status) + len(s.AttributesJSON)
}

// logRowFixedBytes is the log-row equivalent of spanRowFixedBytes: the
// per-row overhead AdmitLog adds to the caller's body+attributes size.
const logRowFixedBytes = 64
