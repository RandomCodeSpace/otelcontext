package aggregate

import (
	"testing"
	"time"
)

func testKey(name uint32) SeriesKey {
	return SeriesKey{TenantID: 1, ServiceID: 2, NameID: name, Signal: SignalMetric}
}

func newTestTracker(perSeries, global int) *BaselineTracker {
	return NewBaselineTracker(BaselineTrackerConfig{
		MaxProducersPerSeries: perSeries,
		MaxBaselines:          global,
	})
}

// --- the four normative cases of #166, in order ---

func TestCumulativeCase1StaleAndDuplicateIgnored(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	start := mustTime(t, "2026-08-21T11:00:00Z")
	tr := newTestTracker(8, 100)
	key := testKey(1)

	tr.ObserveCumulative(key, 7, start, base, 100) // seed
	if out := tr.ObserveCumulative(key, 7, start, base.Add(time.Second), 110); out.Delta != 10 {
		t.Fatalf("progression delta = %v, want 10", out.Delta)
	}

	// Duplicate: identical timestamp and value.
	dup := tr.ObserveCumulative(key, 7, start, base.Add(time.Second), 110)
	if !dup.Ignored || dup.Delta != 0 || dup.Reset {
		t.Errorf("duplicate outcome = %+v, want ignored with no delta and no reset", dup)
	}

	// Out of order: an older timestamp, and a LOWER value — which case 3 would
	// call a reset if the stale check did not come first.
	old := tr.ObserveCumulative(key, 7, start, base, 100)
	if !old.Ignored || old.Reset {
		t.Errorf("out-of-order outcome = %+v, want ignored and NOT a reset", old)
	}

	// The baseline must not have moved.
	b, ok := tr.Baseline(key, 7)
	if !ok || b.Value != 110 || !b.LastTimestamp.Equal(base.Add(time.Second)) {
		t.Fatalf("baseline moved on a stale point: %+v", b)
	}
	if got := tr.Stats().Stale; got != 2 {
		t.Errorf("stale count = %d, want 2", got)
	}
}

func TestCumulativeCase2StartTimeChangeIsAReset(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	start := mustTime(t, "2026-08-21T11:00:00Z")
	tr := newTestTracker(8, 100)
	key := testKey(1)

	tr.ObserveCumulative(key, 7, start, base, 500)
	// Producer restarted: new start_time, and the value happens to be HIGHER
	// than the prior one, so only the start-time rule can catch this.
	out := tr.ObserveCumulative(key, 7, start.Add(time.Hour), base.Add(time.Minute), 600)
	if !out.Reset || out.Reason != ResetStartTimeChange {
		t.Fatalf("outcome = %+v, want reset with reason start_time_change", out)
	}
	if out.Delta != 600 {
		t.Errorf("delta = %v, want 600 (the whole current value)", out.Delta)
	}
	if got := tr.Stats().ResetsStartTime; got != 1 {
		t.Errorf("start-time reset count = %d, want 1", got)
	}
}

func TestCumulativeCase3ValueRegressionIsAnImplicitReset(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	start := mustTime(t, "2026-08-21T11:00:00Z")
	tr := newTestTracker(8, 100)
	key := testKey(1)

	tr.ObserveCumulative(key, 7, start, base, 500)
	out := tr.ObserveCumulative(key, 7, start, base.Add(time.Minute), 12)
	if !out.Reset || out.Reason != ResetValueRegression {
		t.Fatalf("outcome = %+v, want reset with reason value_regression", out)
	}
	if out.Delta != 12 {
		t.Errorf("delta = %v, want 12", out.Delta)
	}
	if got := tr.Stats().ResetsRegression; got != 1 {
		t.Errorf("regression reset count = %d, want 1", got)
	}
}

func TestCumulativeCase4NormalProgression(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	start := mustTime(t, "2026-08-21T11:00:00Z")
	tr := newTestTracker(8, 100)
	key := testKey(1)

	seed := tr.ObserveCumulative(key, 7, start, base, 100)
	if !seed.Seeded || seed.Delta != 0 {
		t.Fatalf("seed outcome = %+v, want Seeded with zero delta", seed)
	}
	for i, want := range []float64{5, 15, 1} {
		ts := base.Add(time.Duration(i+1) * time.Minute)
		prev, _ := tr.Baseline(key, 7)
		out := tr.ObserveCumulative(key, 7, start, ts, prev.Value+want)
		if out.Reset || out.Ignored || out.Gap {
			t.Fatalf("step %d outcome = %+v, want plain progression", i, out)
		}
		if out.Delta != want {
			t.Errorf("step %d delta = %v, want %v", i, out.Delta, want)
		}
	}
}

// --- downtime gap ---

func TestCumulativeDowntimeGapReseedsAndCounts(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	start := mustTime(t, "2026-08-21T11:00:00Z")
	tr := newTestTracker(8, 100)
	key := testKey(1)

	tr.ObserveCumulative(key, 7, start, base, 100)
	// The producer went away for an hour and came back with a much larger
	// counter. Crediting an hour of increase to one 5-minute window would
	// fabricate a rate spike, so the delta is discarded and the baseline
	// re-seeded.
	out := tr.ObserveCumulative(key, 7, start, base.Add(time.Hour), 100000)
	if !out.Gap {
		t.Fatalf("outcome = %+v, want a gap", out)
	}
	if out.Delta != 0 {
		t.Errorf("delta = %v, want 0 — a downtime increase belongs to no window we own", out.Delta)
	}
	if got := tr.Stats().Gaps; got != 1 {
		t.Errorf("gap count = %d, want 1", got)
	}
	b, _ := tr.Baseline(key, 7)
	if b.Value != 100000 {
		t.Errorf("baseline value = %v, want 100000 (re-seeded)", b.Value)
	}

	// The next point continues normally from the re-seeded baseline.
	next := tr.ObserveCumulative(key, 7, start, base.Add(time.Hour+time.Minute), 100005)
	if next.Gap || next.Reset || next.Delta != 5 {
		t.Errorf("post-gap outcome = %+v, want plain delta 5", next)
	}
}

// TestCumulativeGapBoundary pins that the gap threshold is the allowed-lateness
// horizon exactly: at the boundary it is still a normal progression.
func TestCumulativeGapBoundary(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	start := mustTime(t, "2026-08-21T11:00:00Z")
	tr := newTestTracker(8, 100)
	key := testKey(1)

	tr.ObserveCumulative(key, 7, start, base, 100)
	at := tr.ObserveCumulative(key, 7, start, base.Add(AllowedLateness), 150)
	if at.Gap {
		t.Errorf("exactly at the threshold reported a gap: %+v", at)
	}
	just := tr.ObserveCumulative(key, 7, start, base.Add(2*AllowedLateness+time.Second), 200)
	if !just.Gap {
		t.Errorf("past the threshold did not report a gap: %+v", just)
	}
}

// --- producer identity and overflow ---

func TestProducerIDPrefersServiceInstanceID(t *testing.T) {
	withInstance := ResourceIdentity{ServiceInstanceID: "abc", ServiceName: "svc", Host: "h1"}
	sameInstanceOtherHost := ResourceIdentity{ServiceInstanceID: "abc", ServiceName: "svc", Host: "h2"}
	if ResolveProducerID(withInstance) != ResolveProducerID(sameInstanceOtherHost) {
		t.Error("service.instance.id must fully determine the producer when present")
	}

	a := ResourceIdentity{ServiceName: "svc", Host: "h1", Workload: "pod-1"}
	b := ResourceIdentity{ServiceName: "svc", Host: "h1", Workload: "pod-2"}
	if ResolveProducerID(a) == ResolveProducerID(b) {
		t.Error("distinct workloads must produce distinct producer IDs")
	}
	sameTuple := ResourceIdentity{ServiceName: "svc", Host: "h1", Workload: "pod-1"}
	if ResolveProducerID(a) != ResolveProducerID(sameTuple) {
		t.Error("producer ID is not deterministic for an identical tuple")
	}
	if ResolveProducerID(ResourceIdentity{}) == degradedProducer {
		t.Error("a derived producer ID must never collide with the degraded slot")
	}
}

func TestProducerOverflowDegradesWithoutEvicting(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	start := mustTime(t, "2026-08-21T11:00:00Z")
	tr := newTestTracker(2, 100)
	key := testKey(1)

	// Two producers fill the per-series bound.
	tr.ObserveCumulative(key, 101, start, base, 10)
	tr.ObserveCumulative(key, 102, start, base, 20)

	// A third degrades to the shared slot.
	out := tr.ObserveCumulative(key, 103, start, base, 30)
	if !out.Degraded {
		t.Fatalf("third producer outcome = %+v, want degraded", out)
	}
	if got := tr.Stats().ProducerOverflow; got != 1 {
		t.Errorf("producer overflow count = %d, want 1", got)
	}

	// The first two baselines survive untouched — overflow never evicts a
	// correct baseline.
	if b, ok := tr.Baseline(key, 101); !ok || b.Value != 10 {
		t.Errorf("first baseline was disturbed: %+v ok=%v", b, ok)
	}
	if b, ok := tr.Baseline(key, 102); !ok || b.Value != 20 {
		t.Errorf("second baseline was disturbed: %+v ok=%v", b, ok)
	}
	if _, ok := tr.Baseline(key, degradedProducer); !ok {
		t.Error("degraded shared baseline was not created")
	}

	// The first two producers keep converting correctly afterwards.
	if got := tr.ObserveCumulative(key, 101, start, base.Add(time.Minute), 15); got.Delta != 5 {
		t.Errorf("bounded producer delta = %v, want 5", got.Delta)
	}
}

func TestGlobalBaselineCapDegradesToSharedSlot(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	start := mustTime(t, "2026-08-21T11:00:00Z")
	tr := newTestTracker(8, 2)

	tr.ObserveCumulative(testKey(1), 101, start, base, 1)
	tr.ObserveCumulative(testKey(2), 102, start, base, 1)

	// Global budget exhausted: the next series may still take the shared slot,
	// which is reserved capacity — refusing it would strand the series.
	out := tr.ObserveCumulative(testKey(3), 103, start, base, 1)
	if !out.Degraded {
		t.Fatalf("outcome at the global cap = %+v, want degraded", out)
	}
	if got := tr.Stats().GlobalOverflow; got != 1 {
		t.Errorf("global overflow count = %d, want 1", got)
	}
	if _, ok := tr.Baseline(testKey(3), degradedProducer); !ok {
		t.Error("series past the global cap got no baseline at all")
	}
}

// --- non-monotonic ---

func TestNonMonotonicNeverResetDetected(t *testing.T) {
	if IsGaugeLike(TemporalityCumulative, false) != true {
		t.Error("cumulative non-monotonic must be gauge-like")
	}
	if IsGaugeLike(TemporalityCumulative, true) != false {
		t.Error("cumulative monotonic must not be gauge-like")
	}
	if IsGaugeLike(TemporalityDelta, false) != false {
		t.Error("delta temporality must merge directly, not gauge-like")
	}
	if IsGaugeLike(TemporalityUnspecified, false) != true {
		t.Error("a plain gauge must be gauge-like")
	}
}

// TestReducerNonMonotonicSumIsGaugeLike drives the whole path: an UpDownCounter
// that goes down must not register a single reset.
func TestReducerNonMonotonicSumIsGaugeLike(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)
	r := e.NewReducer(now)

	for i, v := range []float64{10, 4, 9, 1} {
		r.ReduceMetricPoint(MetricInput{
			Tenant: "t", Service: "svc", Name: "pool.inuse", Value: v,
			Timestamp:   now.Add(time.Duration(i) * time.Second),
			StartTime:   now.Add(-time.Hour),
			Temporality: TemporalityCumulative, Monotonic: false,
		})
	}
	e.ApplyReducer(r)

	if got := e.Baselines().Stats(); got.ResetsRegression != 0 || got.ResetsStartTime != 0 || got.Entries != 0 {
		t.Fatalf("non-monotonic sum touched the baseline tracker: %+v", got)
	}
	for _, d := range r.Deltas() {
		if d.ResetCount != 0 {
			t.Errorf("reset recorded for a non-monotonic sum: %+v", d)
		}
		if d.GaugeCount != 4 || d.GaugeMin != 1 || d.GaugeMax != 10 {
			t.Errorf("gauge-like aggregation wrong: %+v", d)
		}
	}
}

// TestReducerCumulativeMonotonicThroughEngine exercises the reducer's use of
// the tracker, including the reset counter landing on the delta.
func TestReducerCumulativeMonotonicThroughEngine(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)
	r := e.NewReducer(now)

	start := now.Add(-time.Hour)
	emit := func(offset time.Duration, startTime time.Time, v float64) {
		r.ReduceMetricPoint(MetricInput{
			Tenant: "t", Service: "svc", Name: "requests.total", Value: v,
			Timestamp: now.Add(offset), StartTime: startTime,
			Temporality: TemporalityCumulative, Monotonic: true,
			Resource: ResourceIdentity{ServiceInstanceID: "inst-1"},
		})
	}
	emit(0, start, 100)             // seed, delta 0
	emit(time.Second, start, 130)   // +30
	emit(2*time.Second, start, 125) // regression reset, delta = 125
	emit(2*time.Second, start, 125) // duplicate, ignored

	if got := r.Stats().StaleCumulative; got != 1 {
		t.Errorf("stale cumulative count = %d, want 1", got)
	}
	e.ApplyReducer(r)

	var found bool
	for _, w := range e.Snapshot().Windows {
		for key, d := range w.Series {
			if key.Signal != SignalMetric {
				continue
			}
			found = true
			if d.CounterDelta != 155 {
				t.Errorf("counter delta = %v, want 155 (0 + 30 + 125)", d.CounterDelta)
			}
			if d.ResetCount != 1 {
				t.Errorf("reset count = %d, want 1", d.ResetCount)
			}
			if d.Count != 3 {
				t.Errorf("point count = %d, want 3 (the duplicate contributes nothing)", d.Count)
			}
		}
	}
	if !found {
		t.Fatal("no metric series recorded")
	}
}
