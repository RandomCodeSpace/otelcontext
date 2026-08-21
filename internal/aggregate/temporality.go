package aggregate

import (
	"hash/fnv"
	"sync"
	"time"
)

// Cumulative-sum handling per issue #166.
//
// The evaluation order below is NORMATIVE. Getting it wrong does not produce a
// slightly wrong number, it produces fabricated rate spikes and silently
// swallowed resets, so the order is spelled out rather than implied by control
// flow:
//
//  1. stale/duplicate vs baseline.LastTimestamp -> ignore and count; never a reset
//  2. start_time change                          -> reset, delta = current
//  3. same start_time and current < prior        -> implicit reset, delta = current
//  4. normal progression                         -> delta = current - prior
//
// Non-monotonic sums never enter this path at all: they are gauge-like, and
// applying value-decrease reset detection to an UpDownCounter mangles it.
//
// Baselines are in-memory in Phase 1. Durability — upserting the baseline row
// inside the same group commit as the deltas it justifies — is #173. Until then
// a restart re-seeds every baseline, which is the accepted-gap fallback #166
// documents, not the target contract.

// Temporality is the OTLP aggregation temporality of a metric point.
type Temporality uint8

// Temporality values, mirroring the OTLP numbering.
const (
	TemporalityUnspecified Temporality = 0
	TemporalityDelta       Temporality = 1
	TemporalityCumulative  Temporality = 2
)

// ProducerID discriminates the concrete emitter of a cumulative series — an
// instance, pod or process. It is internal baseline state only: it never enters
// a SeriesKey and is never an aggregate dimension (#159's allowlist, #166's
// producer keying). Two producers sharing one canonical series would otherwise
// look like perpetual resets.
type ProducerID uint64

// degradedProducer is the shared baseline slot used once a series has more
// distinct producers than the configured per-series bound. Apparent resets from
// interleaved producers are accepted and counted rather than letting baseline
// state grow without bound (#166).
const degradedProducer ProducerID = 0

// ResourceIdentity is the stable slice of resource attributes used to derive a
// ProducerID. Every field is optional; the first present value per slot is used.
// This is deliberately NOT the full resource attribute set: attributes that vary
// per export would fragment baselines into uselessness.
type ResourceIdentity struct {
	// ServiceInstanceID is service.instance.id. When present it IS the
	// producer identity — the semantic convention exists for exactly this.
	ServiceInstanceID string
	// ServiceNamespace is service.namespace.
	ServiceNamespace string
	// ServiceName is service.name.
	ServiceName string
	// Host is host.id, else host.name.
	Host string
	// Workload is k8s.pod.uid, else container.id, else process.pid.
	Workload string
}

// ResolveProducerID returns the producer discriminator for a resource.
// service.instance.id wins when present; otherwise the ID is a deterministic
// FNV-1a hash of the stable tuple {service.namespace, service.name,
// host.id|host.name, k8s.pod.uid|container.id|process.pid}.
//
// The hash is never zero: zero is the degraded shared slot.
func ResolveProducerID(id ResourceIdentity) ProducerID {
	h := fnv.New64a()
	if id.ServiceInstanceID != "" {
		_, _ = h.Write([]byte("i\x00"))
		_, _ = h.Write([]byte(id.ServiceInstanceID))
	} else {
		_, _ = h.Write([]byte("t\x00"))
		for _, part := range [...]string{id.ServiceNamespace, id.ServiceName, id.Host, id.Workload} {
			_, _ = h.Write([]byte(part))
			_, _ = h.Write([]byte{0})
		}
	}
	sum := h.Sum64()
	if sum == uint64(degradedProducer) {
		sum = 1
	}
	return ProducerID(sum)
}

// Baseline is the per-(series, producer) record of the last cumulative point.
type Baseline struct {
	// StartTime is the point's start_time_unix_nano. A change means the
	// producer restarted its counter.
	StartTime time.Time
	// LastTimestamp is the timestamp of the last accepted point. Points at or
	// before it are stale or duplicate.
	LastTimestamp time.Time
	// Value is the last accepted cumulative value.
	Value float64
}

// ResetReason names why a counter reset was recorded.
type ResetReason uint8

// ResetReason values. The strings double as metric label values.
const (
	ResetNone ResetReason = iota
	// ResetStartTimeChange — the producer reported a new start_time.
	ResetStartTimeChange
	// ResetValueRegression — same start_time, value went backwards.
	ResetValueRegression
)

// String implements fmt.Stringer.
func (r ResetReason) String() string {
	switch r {
	case ResetStartTimeChange:
		return "start_time_change"
	case ResetValueRegression:
		return "value_regression"
	default:
		return "none"
	}
}

// CumulativeOutcome classifies what happened to one cumulative point. Exactly
// one of Ignored/Seeded/Gap/Reset/Normal describes the point; Delta carries the
// increase to attribute to the point's window.
type CumulativeOutcome struct {
	// Delta is the increase attributable to this point. Zero for ignored,
	// seeded and gap outcomes.
	Delta float64
	// Ignored is true for a stale or duplicate point: it did not move the
	// baseline and produced no delta (case 1).
	Ignored bool
	// Seeded is true when this point created a baseline. No delta is
	// attributed: the accumulation predates our observation window and
	// crediting it to one 5-minute window would fabricate a rate spike.
	Seeded bool
	// Gap is true when the point arrived more than the allowed-lateness
	// window after the baseline's last timestamp. The computed delta is
	// discarded and the baseline re-seeded (#166 downtime handling): totals
	// under-count across long outages, rates never lie.
	Gap bool
	// Reset is true when a counter reset was detected; Reason says which.
	Reset  bool
	Reason ResetReason
	// Degraded is true when the point was attributed to the per-series shared
	// baseline because the producer bound was exhausted.
	Degraded bool
}

// BaselineStats is a snapshot of BaselineTracker counters.
type BaselineStats struct {
	// Entries is the number of live baseline records.
	Entries int
	// Series is the number of series holding at least one baseline.
	Series int
	// Stale counts points ignored as stale or duplicate.
	Stale uint64
	// Seeded counts baselines created.
	Seeded uint64
	// Gaps counts baselines re-seeded after a downtime gap.
	Gaps uint64
	// ResetsStartTime and ResetsRegression count resets by reason.
	ResetsStartTime  uint64
	ResetsRegression uint64
	// ProducerOverflow counts points routed to a degraded shared baseline
	// because the per-series producer bound was exhausted.
	ProducerOverflow uint64
	// GlobalOverflow counts points that could not take a dedicated baseline
	// because the global baseline budget was exhausted.
	GlobalOverflow uint64
}

// BaselineTrackerConfig bounds the tracker.
type BaselineTrackerConfig struct {
	// MaxProducersPerSeries is the per-series producer-baseline bound
	// (AGGREGATE_MAX_PRODUCER_BASELINES_PER_SERIES, default 8). Excess
	// producers share one degraded baseline and never evict the first N.
	MaxProducersPerSeries int
	// MaxBaselines is the global baseline-entry budget
	// (the resolved AGGREGATE_MAX_BASELINES). Past it, only the per-series
	// degraded slot may still be created — it is reserved capacity, exactly
	// like the __other__ series, because refusing it would strand the series.
	MaxBaselines int
	// GapThreshold is the downtime bound: a point more than this after the
	// baseline's last timestamp re-seeds instead of crediting the increase.
	// Defaults to AllowedLateness.
	GapThreshold time.Duration
}

// BaselineTracker converts cumulative monotonic points into deltas and detects
// resets. It is safe for concurrent use.
type BaselineTracker struct {
	cfg BaselineTrackerConfig

	mu      sync.Mutex
	series  map[SeriesKey]map[ProducerID]*Baseline
	entries int

	stale            uint64
	seeded           uint64
	gaps             uint64
	resetsStartTime  uint64
	resetsRegression uint64
	producerOverflow uint64
	globalOverflow   uint64
}

// NewBaselineTracker returns a tracker bounded by cfg. Zero or negative bounds
// take the platform defaults.
func NewBaselineTracker(cfg BaselineTrackerConfig) *BaselineTracker {
	if cfg.MaxProducersPerSeries <= 0 {
		cfg.MaxProducersPerSeries = DefaultMaxProducerBaselinesPerSeries
	}
	if cfg.MaxBaselines <= 0 {
		cfg.MaxBaselines = cfg.MaxProducersPerSeries * DefaultMaxSeriesMetrics
	}
	if cfg.GapThreshold <= 0 {
		cfg.GapThreshold = AllowedLateness
	}
	return &BaselineTracker{
		cfg:    cfg,
		series: make(map[SeriesKey]map[ProducerID]*Baseline),
	}
}

// ObserveCumulative applies the normative #166 evaluation order to one
// cumulative monotonic point and returns what to do with it.
//
// key must be the full canonical metric-series identity BEFORE cardinality
// overflow routing: two series that later collapse into one __other__ series
// still have independent counters, and merging their baselines would invent
// resets.
func (t *BaselineTracker) ObserveCumulative(key SeriesKey, producer ProducerID, startTime, ts time.Time, value float64) CumulativeOutcome {
	t.mu.Lock()
	defer t.mu.Unlock()

	byProducer, ok := t.series[key]
	if !ok {
		byProducer = make(map[ProducerID]*Baseline, 1)
		t.series[key] = byProducer
	}

	var out CumulativeOutcome
	b, ok := byProducer[producer]
	if !ok {
		// The producer has no baseline. Take a dedicated slot when both the
		// per-series bound and the global budget allow; otherwise degrade to
		// the series' shared slot, which is reserved capacity.
		if len(byProducer) >= t.cfg.MaxProducersPerSeries && producer != degradedProducer {
			t.producerOverflow++
			out.Degraded = true
			producer = degradedProducer
			b, ok = byProducer[producer]
		} else if t.entries >= t.cfg.MaxBaselines && producer != degradedProducer {
			t.globalOverflow++
			out.Degraded = true
			producer = degradedProducer
			b, ok = byProducer[producer]
		}
	}

	if !ok {
		byProducer[producer] = &Baseline{StartTime: startTime, LastTimestamp: ts, Value: value}
		t.entries++
		t.seeded++
		out.Seeded = true
		return out
	}

	// (1) Stale or duplicate: never moves the baseline, never a reset.
	if !ts.After(b.LastTimestamp) {
		t.stale++
		out.Ignored = true
		return out
	}

	// Downtime gap: the increase spans more than the mutable-window horizon,
	// so it cannot be attributed to any window we still own. Re-seed.
	if ts.Sub(b.LastTimestamp) > t.cfg.GapThreshold {
		b.StartTime = startTime
		b.LastTimestamp = ts
		b.Value = value
		t.gaps++
		out.Gap = true
		return out
	}

	switch {
	// (2) start_time change -> reset, delta = current.
	case !startTime.Equal(b.StartTime):
		t.resetsStartTime++
		out.Reset = true
		out.Reason = ResetStartTimeChange
		out.Delta = value
	// (3) same start_time, value regressed -> implicit reset, delta = current.
	case value < b.Value:
		t.resetsRegression++
		out.Reset = true
		out.Reason = ResetValueRegression
		out.Delta = value
	// (4) normal progression.
	default:
		out.Delta = value - b.Value
	}

	b.StartTime = startTime
	b.LastTimestamp = ts
	b.Value = value
	return out
}

// Baseline returns a copy of the baseline for (key, producer). Tests and
// diagnostics only.
func (t *BaselineTracker) Baseline(key SeriesKey, producer ProducerID) (Baseline, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.series[key][producer]
	if !ok {
		return Baseline{}, false
	}
	return *b, true
}

// Stats returns a snapshot of the tracker counters.
func (t *BaselineTracker) Stats() BaselineStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return BaselineStats{
		Entries:          t.entries,
		Series:           len(t.series),
		Stale:            t.stale,
		Seeded:           t.seeded,
		Gaps:             t.gaps,
		ResetsStartTime:  t.resetsStartTime,
		ResetsRegression: t.resetsRegression,
		ProducerOverflow: t.producerOverflow,
		GlobalOverflow:   t.globalOverflow,
	}
}

// IsGaugeLike reports whether a metric point aggregates gauge-like — last, min,
// max, sum-of-samples and count, with no reset detection. Gauges do, and so do
// cumulative non-monotonic sums (UpDownCounter): negative movement there is
// legitimate, not a reset (#166 case 2).
func IsGaugeLike(temporality Temporality, monotonic bool) bool {
	return temporality != TemporalityDelta && !monotonic
}
