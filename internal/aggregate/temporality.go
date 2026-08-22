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
// Baselines are durable when the store is enabled (#173): every mutation marks
// the record dirty, the group-commit writer drains the dirty set into the same
// transaction as the deltas it justifies, and startup recovery seeds the
// tracker from the store. Without a store the tracker is in-memory only and a
// restart re-seeds every baseline — the accepted-gap fallback #166 documents,
// not the target contract.

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
	// Recovered is true when Delta also carries an increase whose group commit
	// failed earlier and which this point re-attributes (#194 blocker 2). It is
	// informational: the stranded amount is already folded into Delta.
	Recovered bool
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
	// Owed is the number of records currently carrying an increase whose
	// group commit failed. A number that does not return to zero means the
	// store is refusing writes, not that accounting is drifting.
	Owed int
	// Recovered counts points that re-attributed a stranded increase.
	Recovered uint64
	// Stranded counts stranded increases dropped by a downtime re-seed,
	// which is the one place the owed ledger is deliberately not honoured.
	Stranded uint64
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

// DirtyBaseline is one baseline awaiting durable upsert. The group-commit
// writer drains them into the same transaction as the deltas they justify
// (#166), which is what closes the restart gap durable ACK exists to close.
type DirtyBaseline struct {
	Key      SeriesKey
	Producer ProducerID
	Baseline Baseline
	// Inflight is the increase this record emitted as deltas since its last
	// drain. It rides the drain so a failed commit can hand the amount back
	// as owed instead of stranding it (#194 blocker 2). It is not part of the
	// durable BaselineRow — the store never sees it.
	Inflight float64
}

// baselineRef identifies one baseline record.
type baselineRef struct {
	key      SeriesKey
	producer ProducerID
}

// baselineState is one live baseline plus the ledger that makes a failed group
// commit exactly recoverable (#194 blocker 2).
//
// The baseline VALUE is never rewound on commit failure. Rewinding looks like
// the obvious fix and is wrong: between the drain and the commit result another
// point may already have chained off the advanced value, and rewinding then
// either fabricates the next increment or swallows it depending on which point
// lands first. What a failed commit actually loses is an AMOUNT — the increase
// whose delta rows never became durable — so that amount is carried forward and
// re-attributed to the first point that can carry it. An identical client retry
// (stale by timestamp, and therefore normally ignored) is such a point, which is
// what makes "commit fails, client retries, delta survives" hold without
// duplicating anything when a newer point arrives instead.
type baselineState struct {
	Baseline
	// pending is the increase emitted as deltas since the last drain. The
	// drain moves it into DirtyBaseline.Inflight and clears it.
	pending float64
	// owed is in-flight increase whose commit failed. The next point that
	// moves this record adds it to its own delta and clears it.
	owed float64
}

// BaselineTracker converts cumulative monotonic points into deltas and detects
// resets. It is safe for concurrent use.
type BaselineTracker struct {
	cfg BaselineTrackerConfig

	mu      sync.Mutex
	series  map[SeriesKey]map[ProducerID]*baselineState
	dirty   map[baselineRef]struct{}
	entries int
	owed    int

	stale            uint64
	seeded           uint64
	gaps             uint64
	resetsStartTime  uint64
	resetsRegression uint64
	producerOverflow uint64
	globalOverflow   uint64
	recovered        uint64
	stranded         uint64
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
		series: make(map[SeriesKey]map[ProducerID]*baselineState),
		dirty:  make(map[baselineRef]struct{}),
	}
}

// Seed installs a baseline read back from the durable store at startup. It
// does NOT mark the record dirty: it is already durable, and re-writing every
// recovered baseline on the first commit would make restart the most expensive
// transaction the store ever runs.
func (t *BaselineTracker) Seed(key SeriesKey, producer ProducerID, b Baseline) {
	t.mu.Lock()
	defer t.mu.Unlock()
	byProducer, ok := t.series[key]
	if !ok {
		byProducer = make(map[ProducerID]*baselineState, 1)
		t.series[key] = byProducer
	}
	if _, exists := byProducer[producer]; !exists {
		t.entries++
	}
	byProducer[producer] = &baselineState{Baseline: b}
}

// DrainDirty returns the baselines mutated since the last drain and clears the
// dirty set. The writer calls it while building a group batch, so every drained
// record is at least as new as the deltas in that batch: on a crash a baseline
// can be ahead of the durable deltas (the next point under-counts by one
// interval) but never behind them (which would double-count).
//
// Each drained row also carries the increase that record emitted since the
// previous drain, so Rollback can hand it back if the commit fails.
func (t *BaselineTracker) DrainDirty() []DirtyBaseline {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.dirty) == 0 {
		return nil
	}
	out := make([]DirtyBaseline, 0, len(t.dirty))
	for ref := range t.dirty {
		st, ok := t.series[ref.key][ref.producer]
		if !ok {
			continue
		}
		out = append(out, DirtyBaseline{
			Key:      ref.key,
			Producer: ref.producer,
			Baseline: st.Baseline,
			Inflight: st.pending,
		})
		st.pending = 0
	}
	clear(t.dirty)
	return out
}

// Rollback puts drained baselines back after a failed commit AND hands each
// record the increase that commit was carrying, so the amount is re-attributed
// to the next point instead of being lost (#194 blocker 2).
//
// Re-dirtying alone was the bug: the in-memory baseline had already advanced at
// reduction time, so an identical client retry classified as stale and its delta
// vanished. See baselineState for why the amount is carried forward rather than
// the value rewound.
func (t *BaselineTracker) Rollback(rows []DirtyBaseline) {
	if len(rows) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range rows {
		t.dirty[baselineRef{key: r.Key, producer: r.Producer}] = struct{}{}
		st, ok := t.series[r.Key][r.Producer]
		if !ok || r.Inflight == 0 {
			continue
		}
		if st.owed == 0 {
			t.owed++
		}
		st.owed += r.Inflight
	}
}

// DirtyCount reports how many baselines are awaiting a durable upsert.
func (t *BaselineTracker) DirtyCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.dirty)
}

// markDirtyLocked records that (key, producer) needs a durable upsert. t.mu
// must be held.
func (t *BaselineTracker) markDirtyLocked(key SeriesKey, producer ProducerID) {
	t.dirty[baselineRef{key: key, producer: producer}] = struct{}{}
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
		byProducer = make(map[ProducerID]*baselineState, 1)
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
		byProducer[producer] = &baselineState{
			Baseline: Baseline{StartTime: startTime, LastTimestamp: ts, Value: value},
		}
		t.entries++
		t.seeded++
		t.markDirtyLocked(key, producer)
		out.Seeded = true
		return out
	}

	// (1) Stale or duplicate: never moves the baseline, never a reset. It may
	// still settle an owed increase — an identical retry after a failed commit
	// is exactly this case, and refusing it there is what lost the delta.
	if !ts.After(b.LastTimestamp) {
		t.stale++
		if b.owed > 0 {
			out.Delta = b.owed
			out.Recovered = true
			b.pending += b.owed
			b.owed = 0
			t.owed--
			t.recovered++
			t.markDirtyLocked(key, producer)
			return out
		}
		out.Ignored = true
		return out
	}

	// Downtime gap: the increase spans more than the mutable-window horizon,
	// so it cannot be attributed to any window we still own. Re-seed. An owed
	// increase belongs to a window that is equally gone, so it is dropped here
	// too — counted, not silent (#166 downtime handling).
	if ts.Sub(b.LastTimestamp) > t.cfg.GapThreshold {
		b.StartTime = startTime
		b.LastTimestamp = ts
		b.Value = value
		if b.owed > 0 {
			b.owed = 0
			t.owed--
			t.stranded++
		}
		t.gaps++
		t.markDirtyLocked(key, producer)
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

	// Settle any increase a failed commit stranded on this record. Totals stay
	// exact across a commit failure whichever point arrives first: an identical
	// retry settles it above, a newer point settles it here, and only one of
	// them can, because settling clears the ledger.
	if b.owed > 0 {
		out.Delta += b.owed
		out.Recovered = true
		b.owed = 0
		t.owed--
		t.recovered++
	}
	b.pending += out.Delta
	b.StartTime = startTime
	b.LastTimestamp = ts
	b.Value = value
	t.markDirtyLocked(key, producer)
	return out
}

// Baseline returns a copy of the baseline for (key, producer). Tests and
// diagnostics only.
func (t *BaselineTracker) Baseline(key SeriesKey, producer ProducerID) (Baseline, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.series[key][producer]
	if !ok {
		return Baseline{}, false
	}
	return st.Baseline, true
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
		Owed:             t.owed,
		Recovered:        t.recovered,
		Stranded:         t.stranded,
	}
}

// IsGaugeLike reports whether a metric point aggregates gauge-like — last, min,
// max, sum-of-samples and count, with no reset detection. Gauges do, and so do
// cumulative non-monotonic sums (UpDownCounter): negative movement there is
// legitimate, not a reset (#166 case 2).
func IsGaugeLike(temporality Temporality, monotonic bool) bool {
	return temporality != TemporalityDelta && !monotonic
}
