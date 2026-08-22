package ingest

import (
	"hash/fnv"
	"math"
	"sync"
	"sync/atomic"
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
	// Synthesized-log metering (#201 Q3). Synthesized logs ride a selected
	// trace and never touch the ordinary log-exemplar quota, so their
	// refusals need their own reasons or they would be invisible.
	exemplarReasonSynthPerSpan  = "synth_per_span"
	exemplarReasonSynthPerTrace = "synth_per_trace"
	// Disk watchdog shedding (#201 Q5).
	exemplarReasonShedErrorsOnly = "shed_errors_only"
	exemplarReasonShedRawOff     = "shed_raw_off"
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

	// SynthLogsPerSpan / SynthLogsPerTrace bound the logs synthesized from
	// span events and span status (#201 Q3). Before this they were
	// unmetered: a span carrying two hundred exception events wrote two
	// hundred log rows that no budget had ever seen. Defaults 8 and 64.
	SynthLogsPerSpan  int
	SynthLogsPerTrace int

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
	DefaultExemplarBytesGlobalWindow         = 3 * 1024 * 1024
	DefaultExemplarHealthyRate               = 0.005
	DefaultExemplarStratumTopK               = 5
	DefaultExemplarLogsErrorPerServiceWindow = 50
	DefaultExemplarLogsWarnPerServiceWindow  = 20
	DefaultExemplarMaxSpansPerTrace          = 500
	DefaultExemplarMaxBytesPerTrace          = 256 * 1024
	DefaultExemplarSynthLogsPerSpan          = 8
	DefaultExemplarSynthLogsPerTrace         = 64
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
	if c.SynthLogsPerSpan <= 0 {
		c.SynthLogsPerSpan = DefaultExemplarSynthLogsPerSpan
	}
	if c.SynthLogsPerTrace <= 0 {
		c.SynthLogsPerTrace = DefaultExemplarSynthLogsPerTrace
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
	hash     uint64
	class    int
	stratum  string
	observed int // spans of this trace seen by the policy
	retained int // spans actually handed to persistence
	// bytes is COMMITTED: rows a downstream destination accepted. It never
	// decreases while the window lives (#201 Q4).
	bytes int64
	// reserved is held for rows built but not yet accepted anywhere. It
	// converts to bytes on commit and evaporates on release.
	reserved  int64
	truncated bool
	evicted   bool // displaced by a better-ranked trace; future spans stop
	// synthPerSpan / synthTotal meter the logs synthesized from this trace's
	// spans (#201 Q3). The map is bounded by MaxSpansPerTrace because only
	// spans that actually synthesized a log get an entry.
	synthPerSpan map[string]int
	synthTotal   int
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
	// bytes is committed, reservedBytes is in flight. Both count against
	// BytesPerServiceWindow: a reservation that has not committed yet is a
	// row that is about to be written, and pretending otherwise is how a
	// budget gets overshot by exactly one Export.
	bytes         int64
	reservedBytes int64
	logs          [2]int // [0] = ERROR/FATAL raw logs, [1] = WARN raw logs
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
	traces   int
	bytes    int64
	reserved int64
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

	// shed carries the disk watchdog's SheddingState (#201 Q5). Read on every
	// admission, written rarely by the watchdog goroutine, so an atomic beats
	// putting the hot path behind another mutex.
	shed atomic.Int32
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

	// Disk shedding outranks the complete-retained-trace contract (#201 Q5).
	// At >=90% only error exemplars are admitted; at >=95% none are. A trace
	// already selected in this window is cut short rather than continued, and
	// stamped truncated so the persisted data says so instead of implying a
	// short trace was all there was.
	if reason, shed := p.shedSpan(class); shed {
		if st, ok := sw.traces[in.TraceID]; ok && !st.evicted {
			p.markTruncated(st)
		}
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("traces", reason)
		return false
	}

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

// Reservation lifecycle (#201 Q4).
//
// The old model charged bytes the moment a row was built and refunded the
// global pool on nothing at all, while eviction refunded the count slot. That
// is unsound in one direction that matters: a refund for bytes a queue had
// already accepted lets the window write more than its budget, because the
// refunded bytes are on disk. So:
//
//	reserve  before the row is constructed
//	commit   when the primary queue or the DLQ accepts the batch
//	release  only when the row is dropped BEFORE submission, or when both
//	         destinations refused it and it is permanently gone
//
// Once a submission is accepted the charge is monotonic for that window. A
// later, better-ranked trace displacing the selection releases the COUNT slot
// (a slot is a seat, not a byte) and nothing else.
//
// Reservations are not safe for concurrent use. One per per-resource goroutine,
// merged after the group finishes — which is exactly how Export already
// structures its work.

// exemplarReservationEntry is one reserved charge. It holds pointers to the
// budget cells rather than their keys: a window cell can be pruned between
// reserve and commit, and mutating a detached cell is harmless, whereas
// re-looking-up a pruned key would silently drop the accounting.
type exemplarReservationEntry struct {
	shard *exemplarShard
	sw    *serviceWindow
	st    *traceState // nil for client logs, which belong to no selected trace
	gw    *globalWindow
	bytes int64
	// span marks the entry as holding a retained-SPAN slot, so a release hands
	// that slot back too. Synthesized logs also carry a traceState but never a
	// span slot; decrementing st.retained for one would understate the
	// retained/observed counts stamped on the trace row.
	span bool
}

// ExemplarReservation accumulates one submission unit's reserved bytes.
type ExemplarReservation struct {
	p       *ExemplarPolicy
	entries []exemplarReservationEntry
	settled bool
}

// NewReservation opens a reservation. A nil policy yields a nil reservation,
// and every method below is nil-safe, so the legacy and shadow paths need no
// branches.
func (p *ExemplarPolicy) NewReservation() *ExemplarReservation {
	if p == nil {
		return nil
	}
	return &ExemplarReservation{p: p}
}

// Bytes reports the total reserved so far. Diagnostic surface.
func (r *ExemplarReservation) Bytes() int64 {
	if r == nil {
		return 0
	}
	var n int64
	for _, e := range r.entries {
		n += e.bytes
	}
	return n
}

// Len reports how many rows are reserved.
func (r *ExemplarReservation) Len() int {
	if r == nil {
		return 0
	}
	return len(r.entries)
}

// Merge folds other into r and neutralizes other, so exactly one of them can
// ever settle the charges.
func (r *ExemplarReservation) Merge(other *ExemplarReservation) {
	if r == nil || other == nil || other.settled {
		return
	}
	if r.p == nil {
		r.p = other.p
	}
	r.entries = append(r.entries, other.entries...)
	other.entries = nil
	other.settled = true
}

// Commit converts every reserved byte into a committed byte. Call it exactly
// when a destination has accepted the rows. Idempotent.
func (r *ExemplarReservation) Commit() {
	if r == nil || r.settled || r.p == nil {
		return
	}
	r.settled = true
	for _, e := range r.entries {
		r.p.commitEntry(e)
	}
	r.entries = nil
}

// Release gives every reserved byte back. Legitimate only while the rows have
// NOT been accepted anywhere: dropped before submission, or refused by both the
// primary queue and the DLQ. Idempotent.
func (r *ExemplarReservation) Release() {
	if r == nil || r.settled || r.p == nil {
		return
	}
	r.settled = true
	for _, e := range r.entries {
		r.p.releaseEntry(e)
	}
	r.entries = nil
}

// commitEntry moves one charge from reserved to committed.
func (p *ExemplarPolicy) commitEntry(e exemplarReservationEntry) {
	e.shard.mu.Lock()
	e.sw.reservedBytes -= e.bytes
	e.sw.bytes += e.bytes
	if e.st != nil {
		e.st.reserved -= e.bytes
		e.st.bytes += e.bytes
	}
	e.shard.mu.Unlock()

	p.globalMu.Lock()
	e.gw.reserved -= e.bytes
	e.gw.bytes += e.bytes
	p.globalMu.Unlock()
}

// releaseEntry drops one un-committed charge.
func (p *ExemplarPolicy) releaseEntry(e exemplarReservationEntry) {
	e.shard.mu.Lock()
	e.sw.reservedBytes -= e.bytes
	if e.st != nil {
		e.st.reserved -= e.bytes
		if e.span {
			e.st.retained--
		}
	}
	e.shard.mu.Unlock()

	p.globalMu.Lock()
	e.gw.reserved -= e.bytes
	p.globalMu.Unlock()
}

// settle files an entry: into res when there is a submission boundary to wait
// for, straight to committed when there is not. Caller must NOT hold the shard
// lock.
func (p *ExemplarPolicy) settle(res *ExemplarReservation, e exemplarReservationEntry) {
	if res == nil {
		p.commitEntry(e)
		return
	}
	res.entries = append(res.entries, e)
}

// ReserveSpan meters the bytes a retained span will hand to persistence and
// reports whether it fits. A false return means the row must be dropped: the
// byte budget bound before the count budget did, and the trace is marked
// truncated so the gap is visible in the persisted data rather than inferred.
//
// res may be nil, in which case the charge commits immediately — the caller is
// asserting it has no submission boundary. Production paths always pass one.
func (p *ExemplarPolicy) ReserveSpan(res *ExemplarReservation, tenant, service, traceID string, ts time.Time, size int) bool {
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
	if st.bytes+st.reserved+n > p.cfg.MaxBytesPerTrace || sw.bytes+sw.reservedBytes+n > p.cfg.BytesPerServiceWindow {
		p.markTruncated(st)
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("traces", exemplarReasonBudgetBytes)
		return false
	}
	gw, ok := p.reserveGlobalBytes(window, n)
	if !ok {
		p.markTruncated(st)
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("traces", exemplarReasonBudgetBytes)
		return false
	}
	st.reserved += n
	st.retained++
	sw.reservedBytes += n
	sh.mu.Unlock()

	p.settle(res, exemplarReservationEntry{shard: sh, sw: sw, st: st, gw: gw, bytes: n, span: true})
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

// ReserveLog reports whether a raw client log row may be persisted in
// aggregate mode. INFO/DEBUG are aggregate-only and never raw; ERROR/FATAL get
// a per-service window budget; WARN is opt-in (#161). Bytes are reserved
// against the same service/global byte budgets as spans — one pool, so a log
// flood cannot buy itself past the trace budget.
//
// size is the variable-length payload (body + serialized attributes); the
// fixed row overhead is added here so callers do not have to know it.
func (p *ExemplarPolicy) ReserveLog(res *ExemplarReservation, tenant, service, severity string, ts time.Time, size int) bool {
	slot, ok := p.logSlot(severity)
	if !ok {
		// Not eligible at all — not an exemplar drop, never was one.
		return false
	}
	if reason, shed := p.shedLog(slot); shed {
		p.cfg.Metrics.RecordExemplarDropped("logs", reason)
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
	if sw.bytes+sw.reservedBytes+n > p.cfg.BytesPerServiceWindow {
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("logs", exemplarReasonBudgetBytes)
		return false
	}
	gw, ok := p.reserveGlobalBytes(window, n)
	if !ok {
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("logs", exemplarReasonBudgetBytes)
		return false
	}
	sw.logs[slot]++
	sw.reservedBytes += n
	sh.mu.Unlock()

	p.settle(res, exemplarReservationEntry{shard: sh, sw: sw, gw: gw, bytes: n})
	return true
}

// ReserveSynthesizedLog gates and METERS logs synthesized from span events and
// span status (#201 Q3).
//
// These used to pass on a severity check alone, on the theory that they ride a
// span the policy had already budgeted. They do ride it — and they are not
// weightless. A span carrying two hundred exception events wrote two hundred
// log rows that no budget had ever seen, which is precisely the kind of
// unmetered write the 4.5 GiB main tier cannot absorb.
//
// So a synthesized log now reserves len(body) + len(attributesJSON) +
// logRowFixedBytes against the selected trace's per-trace budget AND the shared
// per-service and global window budgets, under its own per-span and per-trace
// count caps. It does NOT consume the ordinary log-exemplar quota: that budget
// exists for logs a client sent, and charging synthesized rows against it would
// silently evict real ones.
//
// A refusal drops the log, counts a reasoned drop, and marks the trace
// truncated so the gap appears in the persisted data.
func (p *ExemplarPolicy) ReserveSynthesizedLog(res *ExemplarReservation, tenant, service, traceID, spanID, severity string, ts time.Time, size int) bool {
	slot, ok := p.logSlot(severity)
	if !ok {
		// Aggregate-only severity. Never eligible, never a drop.
		return false
	}
	if reason, shed := p.shedLog(slot); shed {
		p.cfg.Metrics.RecordExemplarDropped("logs", reason)
		return false
	}

	window := p.windowOf(ts)
	sh := p.shardFor(service)
	sh.mu.Lock()
	sw := sh.window(windowKey{tenant: tenant, service: service, window: window})
	st, ok := sw.traces[traceID]
	if !ok || st.evicted {
		// Its span is not retained, so the log would be dangling evidence.
		sh.mu.Unlock()
		return false
	}
	if st.synthTotal >= p.cfg.SynthLogsPerTrace {
		p.markTruncated(st)
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("logs", exemplarReasonSynthPerTrace)
		return false
	}
	if st.synthPerSpan[spanID] >= p.cfg.SynthLogsPerSpan {
		p.markTruncated(st)
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("logs", exemplarReasonSynthPerSpan)
		return false
	}
	n := int64(size + logRowFixedBytes)
	if st.bytes+st.reserved+n > p.cfg.MaxBytesPerTrace || sw.bytes+sw.reservedBytes+n > p.cfg.BytesPerServiceWindow {
		p.markTruncated(st)
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("logs", exemplarReasonBudgetBytes)
		return false
	}
	gw, ok := p.reserveGlobalBytes(window, n)
	if !ok {
		p.markTruncated(st)
		sh.mu.Unlock()
		p.cfg.Metrics.RecordExemplarDropped("logs", exemplarReasonBudgetBytes)
		return false
	}
	if st.synthPerSpan == nil {
		st.synthPerSpan = make(map[string]int, 4)
	}
	st.synthPerSpan[spanID]++
	st.synthTotal++
	st.reserved += n
	sw.reservedBytes += n
	sh.mu.Unlock()

	p.settle(res, exemplarReservationEntry{shard: sh, sw: sw, st: st, gw: gw, bytes: n})
	return true
}

// SynthesizedLogEligible is the CHEAP half of ReserveSynthesizedLog: the
// severity floor plus the shedding ladder, with no locks and no accounting.
//
// It exists so the OTLP path can refuse an INFO span event before marshaling
// its attributes. Passing it is necessary, not sufficient — the caller must
// still ReserveSynthesizedLog once it knows the row's size, and that call
// re-checks everything this one did.
func (p *ExemplarPolicy) SynthesizedLogEligible(severity string) bool {
	slot, ok := p.logSlot(severity)
	if !ok {
		return false
	}
	_, shed := p.shedLog(slot)
	return !shed
}

// SetShedding publishes the disk watchdog's current state to the policy.
// Called from the watchdog goroutine; safe on a nil policy.
func (p *ExemplarPolicy) SetShedding(s storage.SheddingState) {
	if p == nil {
		return
	}
	p.shed.Store(int32(s))
}

// Shedding reports the current shedding state. Safe on a nil policy.
func (p *ExemplarPolicy) Shedding() storage.SheddingState {
	if p == nil {
		return storage.SheddingNone
	}
	return storage.SheddingState(p.shed.Load())
}

// DLQDisabled reports whether the exemplar DLQ fallback is closed. True at
// raw-off: deferring an exemplar to the DLQ at 95% is still writing to the
// disk that is about to fill (#201 Q5).
func (p *ExemplarPolicy) DLQDisabled() bool {
	return p.Shedding() >= storage.SheddingRawOff
}

// shedSpan applies the shedding ladder to a span's priority class.
func (p *ExemplarPolicy) shedSpan(class int) (string, bool) {
	switch p.Shedding() {
	case storage.SheddingRawOff:
		return exemplarReasonShedRawOff, true
	case storage.SheddingErrorsOnly:
		if class != exemplarClassError {
			return exemplarReasonShedErrorsOnly, true
		}
	}
	return "", false
}

// shedLog applies the shedding ladder to a log slot, returning the drop reason
// when the log must be refused. slot 0 is ERROR/FATAL, slot 1 is WARN.
func (p *ExemplarPolicy) shedLog(slot int) (string, bool) {
	switch p.Shedding() {
	case storage.SheddingRawOff:
		return exemplarReasonShedRawOff, true
	case storage.SheddingErrorsOnly:
		if slot != 0 {
			return exemplarReasonShedErrorsOnly, true
		}
	}
	return "", false
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
	// refused deterministically) and returns its global COUNT slot. Its bytes
	// are not touched here in either direction (#201 Q4): committed bytes are
	// on disk and refunding them would let the window write past its budget,
	// and reserved bytes belong to rows already handed to the submit path,
	// which will commit or release them itself.
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

// releaseGlobalTrace returns one instance-wide COUNT slot for the window. Bytes
// are never refunded here: they are settled by the reservation lifecycle, which
// is the only place that knows whether a destination accepted the row.
func (p *ExemplarPolicy) releaseGlobalTrace(window int64) {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	if gw, ok := p.global[window]; ok && gw.traces > 0 {
		gw.traces--
	}
}

// reserveGlobalBytes holds n bytes of the instance-wide window budget and
// returns the cell holding them, so commit/release can find it again without a
// map lookup that a window prune could have invalidated.
//
// Reserved bytes count against the cap exactly like committed ones. They are
// rows that are about to be written; treating them as free is how a budget
// overshoots by one Export per window.
func (p *ExemplarPolicy) reserveGlobalBytes(window int64, n int64) (*globalWindow, bool) {
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	gw := p.globalFor(window)
	if gw.bytes+gw.reserved+n > p.cfg.BytesGlobalWindow {
		return nil, false
	}
	gw.reserved += n
	return gw, true
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

// WindowBytes returns the instance-wide (committed, reserved) bytes for the
// window containing ts. Test and diagnostic surface: the monotonicity property
// of #201 Q4 is stated over the committed number.
func (p *ExemplarPolicy) WindowBytes(ts time.Time) (committed, reserved int64) {
	window := p.windowOf(ts)
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	gw, ok := p.global[window]
	if !ok {
		return 0, 0
	}
	return gw.bytes, gw.reserved
}

// GlobalTraceSlots returns how many instance-wide trace slots the window
// containing ts is holding. Test and diagnostic surface.
func (p *ExemplarPolicy) GlobalTraceSlots(ts time.Time) int {
	window := p.windowOf(ts)
	p.globalMu.Lock()
	defer p.globalMu.Unlock()
	if gw, ok := p.global[window]; ok {
		return gw.traces
	}
	return 0
}

// ServiceWindowBytes returns the (committed, reserved) bytes of one
// (tenant, service, window) budget cell. Test and diagnostic surface.
func (p *ExemplarPolicy) ServiceWindowBytes(tenant, service string, ts time.Time) (committed, reserved int64) {
	window := p.windowOf(ts)
	sh := p.shardFor(service)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sw, ok := sh.services[windowKey{tenant: tenant, service: service, window: window}]
	if !ok {
		return 0, 0
	}
	return sw.bytes, sw.reservedBytes
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
