package aggregate

import (
	"fmt"
	"sync"
)

// Cardinality enforcement per issue #158.
//
// The budget allocates MATERIALIZED ACTIVE SERIES — full SeriesKeys present in
// at least one mutable window — not semantic objects. One operation legitimately
// yields several trace series once method, HTTP class, status and span kind are
// multiplied out, and pretending otherwise is how a 6,000-series budget silently
// becomes a 30,000-series one.
//
// The enforcement order is FROZEN:
//
//	tenant fraction -> per-service cap -> signal sub-cap -> global cap
//
// Blame localizes before the shared pool is touched: a single noisy service
// hits its own ceiling first, so the global cap stays a backstop rather than
// becoming the policy by accident.
//
// Nothing is ever dropped. Past a cap, telemetry merges into the per-(service,
// signal) __other__ series, which preserves StatusClass — error visibility must
// survive overflow — while collapsing Method to OTHER, HTTPClass to NONE, the
// dimension tuple to none and the name to the dictionary's __other__ entry.
// Overflow series draw on reserved capacity: a cap can never block creation of
// the series whose job is to absorb violations of that cap.
//
// "Reserved" is literal: an __other__ series is NOT charged against the budget
// it exists to enforce. Charging it made the caps non-binding — at 150 services
// the log sub-cap of 500 was observed holding 1,005 active series, 500 real and
// 505 __other__, because every overflow series it minted counted toward the
// same sub-cap and pushed the census past it (#173). The occupancy the
// reserve costs is reported separately, per signal, so it is visible rather
// than folded into a number that claims to be bounded by a cap.

// Platform default caps, matching the #158 resolution and the AGGREGATE_*
// defaults in internal/config.
const (
	DefaultMaxSeries        = 6000
	DefaultMaxSeriesMetrics = 2400
	DefaultMaxSeriesTraces  = 2400
	DefaultMaxSeriesEdges   = 500
	DefaultMaxSeriesLogs    = 500
	DefaultMaxSeriesSystem  = 200

	DefaultMaxOperationsPerService   = 20
	DefaultMaxTraceSeriesPerService  = 50
	DefaultMaxMetricSeriesPerService = 50

	DefaultMaxProducerBaselinesPerSeries = 8
)

// OverflowReason names the cap that forced a series into its __other__ series.
// The strings are metric label values and are part of the contract.
type OverflowReason uint8

// OverflowReason values, in enforcement order.
const (
	OverflowNone OverflowReason = iota
	// OverflowTenant — the tenant's fraction of the global budget is full.
	OverflowTenant
	// OverflowServiceNames — the service has too many distinct names
	// (operations for traces, log templates for logs).
	OverflowServiceNames
	// OverflowServiceSeries — the service has too many materialized series.
	OverflowServiceSeries
	// OverflowSignal — the signal's sub-cap is full.
	OverflowSignal
	// OverflowGlobal — the instance-wide backstop is full.
	OverflowGlobal
)

// String implements fmt.Stringer.
func (r OverflowReason) String() string {
	switch r {
	case OverflowTenant:
		return "tenant"
	case OverflowServiceNames:
		return "service_names"
	case OverflowServiceSeries:
		return "service_series"
	case OverflowSignal:
		return "signal"
	case OverflowGlobal:
		return "global"
	default:
		return "none"
	}
}

// LimiterConfig holds the cardinality budget. Zero values take the platform
// defaults; a negative value disables that cap.
type LimiterConfig struct {
	MaxSeries        int
	MaxSeriesMetrics int
	MaxSeriesTraces  int
	MaxSeriesEdges   int
	MaxSeriesLogs    int
	MaxSeriesSystem  int

	MaxOperationsPerService   int
	MaxLogTemplatesPerService int
	MaxTraceSeriesPerService  int
	MaxMetricSeriesPerService int

	// SeriesPerTenantFraction is the fraction of MaxSeries one tenant may
	// hold. 0 disables the tenant cap (the single-tenant default).
	SeriesPerTenantFraction float64

	// OtherNameID resolves the dictionary __other__ ID for the name kind of a
	// signal, which is what an overflow series uses as its NameID. Required.
	OtherNameID func(tenantID uint32, signal Signal) uint32
}

// Validate reports a budget that cannot hold: sum of sub-caps above the global
// cap, or a tenant fraction outside [0,1]. config.Load() performs the same
// checks at startup (fail-closed); this exists so an engine constructed
// directly in a test cannot quietly run an impossible budget.
func (c LimiterConfig) Validate() error {
	if c.SeriesPerTenantFraction < 0 || c.SeriesPerTenantFraction > 1 {
		return fmt.Errorf("aggregate: series-per-tenant fraction %v outside [0,1]", c.SeriesPerTenantFraction)
	}
	sum := c.MaxSeriesMetrics + c.MaxSeriesTraces + c.MaxSeriesEdges + c.MaxSeriesLogs + c.MaxSeriesSystem
	if sum > c.MaxSeries {
		return fmt.Errorf("aggregate: sum of signal sub-caps (%d) exceeds global cap (%d)", sum, c.MaxSeries)
	}
	return nil
}

// withDefaults fills unset caps.
func (c LimiterConfig) withDefaults() LimiterConfig {
	if c.MaxSeries == 0 {
		c.MaxSeries = DefaultMaxSeries
	}
	if c.MaxSeriesMetrics == 0 {
		c.MaxSeriesMetrics = DefaultMaxSeriesMetrics
	}
	if c.MaxSeriesTraces == 0 {
		c.MaxSeriesTraces = DefaultMaxSeriesTraces
	}
	if c.MaxSeriesEdges == 0 {
		c.MaxSeriesEdges = DefaultMaxSeriesEdges
	}
	if c.MaxSeriesLogs == 0 {
		c.MaxSeriesLogs = DefaultMaxSeriesLogs
	}
	if c.MaxSeriesSystem == 0 {
		c.MaxSeriesSystem = DefaultMaxSeriesSystem
	}
	if c.MaxOperationsPerService == 0 {
		c.MaxOperationsPerService = DefaultMaxOperationsPerService
	}
	if c.MaxLogTemplatesPerService == 0 {
		c.MaxLogTemplatesPerService = DefaultMaxLogTemplatesPerService
	}
	if c.MaxTraceSeriesPerService == 0 {
		c.MaxTraceSeriesPerService = DefaultMaxTraceSeriesPerService
	}
	if c.MaxMetricSeriesPerService == 0 {
		c.MaxMetricSeriesPerService = DefaultMaxMetricSeriesPerService
	}
	return c
}

// serviceScope is the (service, signal) scope of the per-service caps.
type serviceScope struct {
	tenant  uint32
	service uint32
	signal  Signal
}

// SeriesWindowKey identifies one series inside one mutable window. Window starts
// are UTC-aligned Unix seconds.
type SeriesWindowKey struct {
	Key         SeriesKey
	WindowStart int64
}

// Admission is the outcome of evaluating one series against the budget.
type Admission struct {
	// Key is the series to record under. It differs from the requested key
	// when a cap forced overflow routing.
	Key SeriesKey
	// Overflowed reports that a cap was hit and Key is an __other__ series.
	Overflowed bool
	// Reason names the cap that triggered overflow.
	Reason OverflowReason
	// Reserved reports that THIS call created (Key, window) presence, and so
	// that a matching Release is the exact undo. It is false when the pair was
	// already present, which is what lets a rolled-back group commit release
	// only the occupancy it charged and never occupancy an earlier committed
	// batch is still using (#194 blocker 3).
	Reserved bool
}

// LimiterStats is a snapshot of Limiter occupancy.
type LimiterStats struct {
	// Active is the number of distinct budgeted series present in a mutable
	// window. __other__ series are excluded: they are the reserve the caps
	// spend, not occupancy the caps admit, and counting them here is what let
	// the observed census exceed its own sub-cap (#173).
	Active int
	// ActiveBySignal breaks Active down per signal. Each entry is bounded by
	// that signal's sub-cap.
	ActiveBySignal map[Signal]int
	// Overflow counts admissions routed to an __other__ series, per reason.
	Overflow map[OverflowReason]uint64
	// OverflowSeries is the number of live __other__ series.
	OverflowSeries int
	// OverflowSeriesBySignal breaks OverflowSeries down per signal. This is
	// the reserve's real occupancy — unbudgeted, bounded by
	// (services x signals x status classes).
	OverflowSeriesBySignal map[Signal]int
}

// Limiter owns the active-series census and enforces the budget. It is safe for
// concurrent use.
//
// The engine calls Admit before taking any shard lock and Release during window
// rollover. The limiter never takes a shard lock, so the lock order is always
// engine -> limiter and can never invert.
type Limiter struct {
	cfg LimiterConfig

	mu sync.Mutex
	// present records which (series, window) pairs exist. It is the definition
	// of "active": a series is active while it appears in a mutable window.
	present map[SeriesWindowKey]struct{}
	// windows counts how many mutable windows hold each series, so a series
	// leaving one window does not release budget it still occupies elsewhere.
	windows map[SeriesKey]int

	total     int
	bySignal  map[Signal]int
	byService map[serviceScope]int
	names     map[serviceScope]map[uint32]struct{}
	byTenant  map[uint32]int
	// overflow and overflowBySignal are the reserve's census. They are kept
	// apart from total/bySignal/byService/byTenant on purpose: the reserve is
	// what a cap spends, so it must never be part of what the cap is compared
	// against.
	overflow         map[SeriesKey]struct{}
	overflowBySignal map[Signal]int

	overflowCounts map[OverflowReason]uint64
}

// NewLimiter returns a Limiter for cfg.
func NewLimiter(cfg LimiterConfig) *Limiter {
	return &Limiter{
		cfg:              cfg.withDefaults(),
		present:          make(map[SeriesWindowKey]struct{}),
		windows:          make(map[SeriesKey]int),
		bySignal:         make(map[Signal]int),
		byService:        make(map[serviceScope]int),
		names:            make(map[serviceScope]map[uint32]struct{}),
		byTenant:         make(map[uint32]int),
		overflow:         make(map[SeriesKey]struct{}),
		overflowBySignal: make(map[Signal]int),
		overflowCounts:   make(map[OverflowReason]uint64),
	}
}

// Admit evaluates key against the budget for the given window and returns the
// series to record under. The full materialized key is evaluated before
// admission; nothing is ever refused, only redirected.
func (l *Limiter) Admit(key SeriesKey, window int64) Admission {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.present[SeriesWindowKey{Key: key, WindowStart: window}]; ok {
		return Admission{Key: key}
	}
	if l.windows[key] > 0 {
		// Already active in another mutable window: it holds budget already.
		l.addLocked(key, window, false)
		return Admission{Key: key, Reserved: true}
	}
	if reason := l.checkLocked(key); reason != OverflowNone {
		l.overflowCounts[reason]++
		other := l.overflowKey(key)
		reserved := l.admitOverflowLocked(other, window)
		return Admission{Key: other, Overflowed: true, Reason: reason, Reserved: reserved}
	}
	l.addLocked(key, window, false)
	return Admission{Key: key, Reserved: true}
}

// checkLocked applies the frozen enforcement order and returns the first cap
// that key violates. l.mu must be held.
func (l *Limiter) checkLocked(key SeriesKey) OverflowReason {
	// 1. Tenant fraction.
	if l.cfg.SeriesPerTenantFraction > 0 {
		limit := int(l.cfg.SeriesPerTenantFraction * float64(l.cfg.MaxSeries))
		if limit < 1 {
			limit = 1
		}
		if l.byTenant[key.TenantID] >= limit {
			return OverflowTenant
		}
	}

	scope := serviceScope{tenant: key.TenantID, service: key.ServiceID, signal: key.Signal}

	// 2a. Per-service distinct-name cap (operations for traces and edges, log
	// templates for logs). #159: the per-service operation cap counts distinct
	// names; the sub-caps and global cap count full SeriesKeys.
	if limit := l.nameCap(key.Signal); limit > 0 {
		if names := l.names[scope]; len(names) >= limit {
			if _, known := names[key.NameID]; !known {
				return OverflowServiceNames
			}
		}
	}

	// 2b. Per-service materialized-series cap. Per-service caps are isolation
	// ceilings, not reservations: 150 services x 50 may exceed the sub-cap,
	// and the instance-wide cap remains the final shared bound.
	if limit := l.seriesCap(key.Signal); limit > 0 && l.byService[scope] >= limit {
		return OverflowServiceSeries
	}

	// 3. Signal sub-cap.
	if limit := l.signalCap(key.Signal); limit > 0 && l.bySignal[key.Signal] >= limit {
		return OverflowSignal
	}

	// 4. Global backstop.
	if l.cfg.MaxSeries > 0 && l.total >= l.cfg.MaxSeries {
		return OverflowGlobal
	}
	return OverflowNone
}

// admitOverflowLocked records an __other__ series. It bypasses every cap: the
// series that absorbs quota violations can never be blocked by a quota.
//
// It is also not CHARGED to any cap. An __other__ series is the reserve a cap
// spends when it binds, so adding it to the same census the cap is compared
// against makes the cap unenforceable: the census walks past the limit by one
// per (service, status class) that overflowed, which is exactly the 1,005
// active log series against a 500 sub-cap the wave-5 run measured (#173). The
// reserve is reported through OverflowSeriesBySignal instead, so its cost is
// visible without being laundered through a bounded-looking number.
// It returns whether this call created the presence, so a rolled-back commit
// can release exactly what it reserved.
func (l *Limiter) admitOverflowLocked(other SeriesKey, window int64) bool {
	if _, ok := l.present[SeriesWindowKey{Key: other, WindowStart: window}]; ok {
		return false
	}
	l.addLocked(other, window, true)
	return true
}

// addLocked records presence of key in window and, on the series' first window,
// charges it against the budget. Overflow series are recorded but not charged.
// l.mu must be held.
func (l *Limiter) addLocked(key SeriesKey, window int64, isOverflow bool) {
	l.present[SeriesWindowKey{Key: key, WindowStart: window}] = struct{}{}
	l.windows[key]++
	if l.windows[key] > 1 {
		return
	}
	if isOverflow {
		l.overflow[key] = struct{}{}
		l.overflowBySignal[key.Signal]++
		return
	}
	l.total++
	l.bySignal[key.Signal]++
	scope := serviceScope{tenant: key.TenantID, service: key.ServiceID, signal: key.Signal}
	l.byService[scope]++
	l.byTenant[key.TenantID]++
	if l.nameCap(key.Signal) > 0 {
		names := l.names[scope]
		if names == nil {
			names = make(map[uint32]struct{})
			l.names[scope] = names
		}
		names[key.NameID] = struct{}{}
	}
}

// Release drops one series' presence in one window. When the series leaves its
// last mutable window it stops consuming budget: historical series are free
// (#158), only active ones are charged.
func (l *Limiter) Release(key SeriesKey, window int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releaseLocked(key, window)
}

func (l *Limiter) releaseLocked(key SeriesKey, window int64) {
	swk := SeriesWindowKey{Key: key, WindowStart: window}
	if _, ok := l.present[swk]; !ok {
		return
	}
	delete(l.present, swk)
	l.windows[key]--
	if l.windows[key] > 0 {
		return
	}
	delete(l.windows, key)
	if _, isOverflow := l.overflow[key]; isOverflow {
		// Symmetric with addLocked: the reserve was never charged, so there is
		// nothing to give back except the reserve's own census.
		delete(l.overflow, key)
		l.overflowBySignal[key.Signal]--
		if l.overflowBySignal[key.Signal] <= 0 {
			delete(l.overflowBySignal, key.Signal)
		}
		return
	}
	l.total--
	l.bySignal[key.Signal]--
	scope := serviceScope{tenant: key.TenantID, service: key.ServiceID, signal: key.Signal}
	l.byService[scope]--
	if l.byService[scope] <= 0 {
		delete(l.byService, scope)
	}
	l.byTenant[key.TenantID]--
	if l.byTenant[key.TenantID] <= 0 {
		delete(l.byTenant, key.TenantID)
	}
	if names := l.names[scope]; names != nil {
		delete(names, key.NameID)
		if len(names) == 0 {
			delete(l.names, scope)
		}
	}
}

// overflowKey builds the per-(service, signal) __other__ series for key.
// StatusClass survives — an error that overflows is still an error — while the
// name, dimensions, method, HTTP class and span kind collapse, which is what
// bounds the number of overflow series.
func (l *Limiter) overflowKey(key SeriesKey) SeriesKey {
	other := SeriesKey{
		TenantID:    key.TenantID,
		ServiceID:   key.ServiceID,
		DimsID:      0,
		Signal:      key.Signal,
		StatusClass: key.StatusClass,
		HTTPClass:   HTTPClassNone,
		Variant:     SpanKindUnspecified,
	}
	if key.Signal == SignalTraceOp || key.Signal == SignalServiceEdge {
		other.Method = MethodOther
	}
	if l.cfg.OtherNameID != nil {
		other.NameID = l.cfg.OtherNameID(key.TenantID, key.Signal)
	}
	return other
}

func (l *Limiter) nameCap(s Signal) int {
	switch s {
	case SignalTraceOp, SignalServiceEdge:
		return l.cfg.MaxOperationsPerService
	case SignalLog:
		return l.cfg.MaxLogTemplatesPerService
	default:
		return 0
	}
}

func (l *Limiter) seriesCap(s Signal) int {
	switch s {
	case SignalTraceOp, SignalServiceEdge:
		return l.cfg.MaxTraceSeriesPerService
	case SignalMetric:
		return l.cfg.MaxMetricSeriesPerService
	default:
		return 0
	}
}

func (l *Limiter) signalCap(s Signal) int {
	switch s {
	case SignalTraceOp:
		return l.cfg.MaxSeriesTraces
	case SignalServiceEdge:
		return l.cfg.MaxSeriesEdges
	case SignalLog:
		return l.cfg.MaxSeriesLogs
	case SignalMetric:
		return l.cfg.MaxSeriesMetrics
	default:
		return l.cfg.MaxSeriesSystem
	}
}

// IsOverflowSeries reports whether key is a live __other__ series.
func (l *Limiter) IsOverflowSeries(key SeriesKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.overflow[key]
	return ok
}

// Stats returns a snapshot of occupancy and overflow counters.
func (l *Limiter) Stats() LimiterStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	bySignal := make(map[Signal]int, len(l.bySignal))
	for s, n := range l.bySignal {
		if n > 0 {
			bySignal[s] = n
		}
	}
	overflow := make(map[OverflowReason]uint64, len(l.overflowCounts))
	for r, n := range l.overflowCounts {
		overflow[r] = n
	}
	overflowSeries := make(map[Signal]int, len(l.overflowBySignal))
	for s, n := range l.overflowBySignal {
		if n > 0 {
			overflowSeries[s] = n
		}
	}
	return LimiterStats{
		Active:                 l.total,
		ActiveBySignal:         bySignal,
		Overflow:               overflow,
		OverflowSeries:         len(l.overflow),
		OverflowSeriesBySignal: overflowSeries,
	}
}
