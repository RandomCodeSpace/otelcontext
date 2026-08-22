package aggregate

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testEngine builds an engine with a fixed clock and generous caps, so a test
// that is not about cardinality never trips a cap by accident.
func testEngine(t *testing.T, now time.Time) *Engine {
	t.Helper()
	e, err := NewEngine(EngineConfig{
		Mode: ModeShadow,
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

func TestWindowStartIsUTCAligned(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2026-08-21T12:00:00Z", "2026-08-21T12:00:00Z"},
		{"2026-08-21T12:03:59Z", "2026-08-21T12:00:00Z"},
		{"2026-08-21T12:04:59.999Z", "2026-08-21T12:00:00Z"},
		{"2026-08-21T12:05:00Z", "2026-08-21T12:05:00Z"},
		{"2026-08-21T23:59:59Z", "2026-08-21T23:55:00Z"},
		{"1969-12-31T23:57:00Z", "1969-12-31T23:55:00Z"}, // pre-epoch: floor, not truncate-toward-zero
	}
	for _, c := range cases {
		got := WindowStart(mustTime(t, c.in))
		want := mustTime(t, c.want).Unix()
		if got != want {
			t.Errorf("WindowStart(%s) = %d, want %d (%s)", c.in, got, want, c.want)
		}
	}
}

// TestWindowStartIndependentOfLocalZone pins that alignment is UTC, not local:
// a non-UTC location must not shift the window boundary.
func TestWindowStartIndependentOfLocalZone(t *testing.T) {
	utc := mustTime(t, "2026-08-21T12:03:00Z")
	kolkata := time.FixedZone("IST", 5*3600+1800) // +05:30, a half-hour offset
	shifted := utc.In(kolkata)
	if WindowStart(utc) != WindowStart(shifted) {
		t.Fatalf("window start moved with location: %d vs %d", WindowStart(utc), WindowStart(shifted))
	}
}

func TestClassifyLateAndFuture(t *testing.T) {
	arrival := mustTime(t, "2026-08-21T12:00:00Z")
	cases := []struct {
		name   string
		offset time.Duration
		want   PointDisposition
	}{
		{"current", 0, PointAccepted},
		{"inside the lateness horizon", -9 * time.Minute, PointAccepted},
		{"past the lateness horizon", -16 * time.Minute, PointLate},
		{"far past", -3 * time.Hour, PointLate},
		{"slightly ahead", time.Minute, PointAccepted},
		{"beyond future skew", 3 * time.Minute, PointFuture},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got := Classify(arrival, arrival.Add(c.offset))
			if got != c.want {
				t.Fatalf("disposition = %v, want %v", got, c.want)
			}
		})
	}
}

// TestReducerExcludesLateAndFuturePointsWithMetrics proves late and future
// points are excluded from aggregates and COUNTED — never dropped silently.
func TestReducerExcludesLateAndFuturePoints(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)
	r := e.NewReducer(now)

	r.ReduceSpan(SpanInput{Tenant: "t", Service: "svc", SpanName: "op", Timestamp: now, DurationMicros: 100})
	r.ReduceSpan(SpanInput{Tenant: "t", Service: "svc", SpanName: "op", Timestamp: now.Add(-30 * time.Minute), DurationMicros: 100})
	r.ReduceSpan(SpanInput{Tenant: "t", Service: "svc", SpanName: "op", Timestamp: now.Add(10 * time.Minute), DurationMicros: 100})

	st := r.Stats()
	if st.InputPoints[SignalTraceOp] != 3 {
		t.Errorf("input points = %d, want 3", st.InputPoints[SignalTraceOp])
	}
	if st.LatePoints[SignalTraceOp] != 1 {
		t.Errorf("late points = %d, want 1", st.LatePoints[SignalTraceOp])
	}
	if st.FuturePoints[SignalTraceOp] != 1 {
		t.Errorf("future points = %d, want 1", st.FuturePoints[SignalTraceOp])
	}
	if st.Accepted[SignalTraceOp] != 1 {
		t.Errorf("accepted = %d, want 1", st.Accepted[SignalTraceOp])
	}

	e.ApplyReducer(r)
	count, _ := e.Snapshot().Totals(SignalTraceOp)
	if count != 1 {
		t.Fatalf("aggregated count = %d, want 1 (late and future excluded)", count)
	}
}

// TestLatePointStillLandsInItsOwnWindow: a point inside the lateness horizon
// belongs to the window of its own timestamp, not the window of arrival.
func TestLatePointLandsInItsOwnWindow(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)
	r := e.NewReducer(now)
	r.ReduceSpan(SpanInput{Tenant: "t", Service: "svc", SpanName: "op", Timestamp: now, DurationMicros: 1})
	r.ReduceSpan(SpanInput{Tenant: "t", Service: "svc", SpanName: "op", Timestamp: now.Add(-7 * time.Minute), DurationMicros: 1})
	e.ApplyReducer(r)

	snap := e.Snapshot()
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %d, want 2 (%v)", len(snap.Windows), snap)
	}
	if got, want := snap.Windows[0].Start, mustTime(t, "2026-08-21T11:50:00Z"); !got.Equal(want) {
		t.Errorf("oldest window = %s, want %s", got, want)
	}
	if got, want := snap.Windows[1].Start, mustTime(t, "2026-08-21T12:00:00Z"); !got.Equal(want) {
		t.Errorf("newest window = %s, want %s", got, want)
	}
}

// TestRolloverClosesWithoutEvicting pins #194 blocker 6: a window past its
// lateness horizon stops accepting points but stays in memory, readable and
// still holding its budget, until a committed finalize hands it to the store.
func TestRolloverClosesWithoutEvicting(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	clock := base
	e, err := NewEngine(EngineConfig{Mode: ModeShadow, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	r := e.NewReducer(clock)
	r.ReduceSpan(SpanInput{Tenant: "t", Service: "svc", SpanName: "op", Timestamp: clock, DurationMicros: 1})
	e.ApplyReducer(r)

	if got := e.Snapshot().ActiveSeries; got != 1 {
		t.Fatalf("active series = %d, want 1", got)
	}

	// Advance past the window's close plus the full lateness horizon.
	clock = base.Add(WindowSize + AllowedLateness + time.Second)
	if forced := e.Rollover(clock); forced != 0 {
		t.Fatalf("rollover force-evicted %d windows under the cap, want 0", forced)
	}

	snap := e.Snapshot()
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %d after rollover, want 1 — a closed window must stay readable", len(snap.Windows))
	}
	if snap.ClosedWindows != 1 {
		t.Errorf("closed windows = %d, want 1", snap.ClosedWindows)
	}
	if snap.ActiveSeries != 1 {
		t.Errorf("active series = %d, want 1 — a closed window still occupies budget", snap.ActiveSeries)
	}
	if snap.WindowsDiscarded != 0 || snap.ClosedWindowsForced != 0 {
		t.Errorf("discard counters = (%d, %d), want (0, 0) — nothing was lost", snap.WindowsDiscarded, snap.ClosedWindowsForced)
	}

	// Finalization is the only thing that may evict it and release the budget.
	e.MarkFinalized(WindowStart(base))
	snap = e.Snapshot()
	if len(snap.Windows) != 0 || snap.ActiveSeries != 0 || snap.ClosedWindows != 0 {
		t.Errorf("after MarkFinalized: windows=%d active=%d closed=%d, want 0/0/0",
			len(snap.Windows), snap.ActiveSeries, snap.ClosedWindows)
	}
}

// TestRolloverCapForcesLossyEvictionAndCountsIt pins the memory bound: a wedged
// finalizer must not grow RAM without limit, and the fallback to the old lossy
// eviction must be counted rather than silent.
func TestRolloverCapForcesLossyEvictionAndCountsIt(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	clock := base
	const cap = 2
	e, err := NewEngine(EngineConfig{
		Mode:             ModeShadow,
		MaxClosedWindows: cap,
		Now:              func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// Four windows, one point each, no finalizer ever running.
	const windows = 4
	for i := 0; i < windows; i++ {
		clock = base.Add(time.Duration(i) * WindowSize)
		r := e.NewReducer(clock)
		r.ReduceSpan(SpanInput{Tenant: "t", Service: "svc", SpanName: "op", Timestamp: clock, DurationMicros: 1})
		e.ApplyReducer(r)
	}

	clock = base.Add(time.Duration(windows)*WindowSize + AllowedLateness + time.Second)
	forced := e.Rollover(clock)
	snap := e.Snapshot()
	if snap.ClosedWindows > cap {
		t.Fatalf("holding %d closed windows against a cap of %d", snap.ClosedWindows, cap)
	}
	if forced != windows-cap {
		t.Fatalf("rollover force-evicted %d windows, want %d", forced, windows-cap)
	}
	if snap.ClosedWindowsForced != uint64(windows-cap) {
		t.Errorf("ClosedWindowsForced = %d, want %d — forced loss must be counted",
			snap.ClosedWindowsForced, windows-cap)
	}
	// The oldest windows are the ones that go, and only they.
	if own := e.Ownership(); own.OwnsInMemory(WindowStart(base)) {
		t.Error("the oldest closed window survived the cap")
	}
	if own := e.Ownership(); !own.OwnsInMemory(WindowStart(base.Add((windows - 1) * WindowSize))) {
		t.Error("the newest closed window was evicted before older ones")
	}
}

func TestRolloverKeepsWindowsInsideTheHorizon(t *testing.T) {
	base := mustTime(t, "2026-08-21T12:00:00Z")
	clock := base
	e, err := NewEngine(EngineConfig{Mode: ModeShadow, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	r := e.NewReducer(clock)
	r.ReduceSpan(SpanInput{Tenant: "t", Service: "svc", SpanName: "op", Timestamp: clock, DurationMicros: 1})
	e.ApplyReducer(r)

	clock = base.Add(WindowSize + AllowedLateness - time.Minute)
	if dropped := e.Rollover(clock); dropped != 0 {
		t.Fatalf("rollover dropped %d windows while still mutable", dropped)
	}
	if len(e.Snapshot().Windows) != 1 {
		t.Fatal("window disappeared while still inside the lateness horizon")
	}
}

func TestRevisionIsMonotonic(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)
	prev := e.Revision()
	for i := 0; i < 20; i++ {
		r := e.NewReducer(now)
		r.ReduceSpan(SpanInput{Tenant: "t", Service: "svc", SpanName: "op", Timestamp: now, DurationMicros: 1})
		rev := e.ApplyReducer(r)
		if rev <= prev {
			t.Fatalf("revision %d did not advance past %d", rev, prev)
		}
		prev = rev
	}
}

// TestConcurrentApplyDeltas exercises the shard locking under -race and proves
// no delta is lost and no revision is handed out twice.
func TestConcurrentApplyDeltas(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)

	const goroutines = 8
	const perGoroutine = 50
	// Enough distinct operations to spread across all four shards.
	operations := []string{"a", "b", "c", "d", "e", "f", "g", "h"}

	var wg sync.WaitGroup
	revs := make([][]uint64, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			revs[g] = make([]uint64, 0, perGoroutine)
			for i := 0; i < perGoroutine; i++ {
				r := e.NewReducer(now)
				for _, op := range operations {
					r.ReduceSpan(SpanInput{
						Tenant: "t", Service: "svc", SpanName: op,
						Timestamp: now, DurationMicros: float64(i + 1),
					})
				}
				revs[g] = append(revs[g], e.ApplyReducer(r))
			}
		}(g)
	}
	wg.Wait()

	count, _ := e.Snapshot().Totals(SignalTraceOp)
	want := uint64(goroutines * perGoroutine * len(operations))
	if count != want {
		t.Errorf("aggregated count = %d, want %d", count, want)
	}

	seen := make(map[uint64]bool)
	for _, rs := range revs {
		for _, rev := range rs {
			if seen[rev] {
				t.Fatalf("revision %d handed out twice", rev)
			}
			seen[rev] = true
		}
	}
	if got := e.Revision(); got != uint64(goroutines*perGoroutine) {
		t.Errorf("final revision = %d, want %d", got, goroutines*perGoroutine)
	}
}

// TestShardsAreUsed guards against a hash that collapses every series onto one
// shard, which would make the four-shard design decorative.
func TestShardsAreUsed(t *testing.T) {
	used := make(map[int]bool)
	for i := uint32(0); i < 256; i++ {
		used[shardIndex(SeriesKey{TenantID: 1, ServiceID: 2, NameID: i, Signal: SignalTraceOp})] = true
	}
	if len(used) != NumShards {
		t.Fatalf("hash reached %d of %d shards", len(used), NumShards)
	}
}

// TestApplierInterposition proves the Phase 2 group-commit writer can slot in
// between the reducer and the shards without touching the callers.
func TestApplierInterposition(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)

	var calls atomic.Int64
	e.SetApplier(applierFunc(func(m DeltaMap) uint64 {
		calls.Add(1)
		return e.ApplyCommitted(m) // stands in for "commit, then apply"
	}))

	r := e.NewReducer(now)
	r.ReduceSpan(SpanInput{Tenant: "t", Service: "svc", SpanName: "op", Timestamp: now, DurationMicros: 1})
	e.ApplyReducer(r)

	if calls.Load() != 1 {
		t.Fatalf("interposed applier called %d times, want 1", calls.Load())
	}
	if count, _ := e.Snapshot().Totals(SignalTraceOp); count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

type applierFunc func(DeltaMap) uint64

func (f applierFunc) Apply(m DeltaMap) uint64 { return f(m) }

func TestDeltaMergeIsOrderIndependent(t *testing.T) {
	ts := mustTime(t, "2026-08-21T12:00:00Z")
	// Distinct gauge timestamps per builder: two samples carrying the SAME
	// timestamp for one series are genuinely ambiguous about which is "last",
	// and no merge order can resolve that.
	build := func(vals []float64, offset int) *AggregateDelta {
		d := &AggregateDelta{}
		for i, v := range vals {
			d.ObserveSpan(v, i%3 == 0)
			d.ObserveGauge(v, ts.Add(time.Duration(offset+i)*time.Second))
		}
		return d
	}
	a := build([]float64{1, 2, 3}, 0)
	b := build([]float64{4, 5, 6}, 10)

	ab := build([]float64{1, 2, 3}, 0)
	ab.Merge(b)
	ba := build([]float64{4, 5, 6}, 10)
	ba.Merge(a)

	if ab.Count != ba.Count || ab.ErrorCount != ba.ErrorCount {
		t.Errorf("counts diverged: %+v vs %+v", ab, ba)
	}
	if ab.DurationSum != ba.DurationSum || ab.DurationMin != ba.DurationMin || ab.DurationMax != ba.DurationMax {
		t.Errorf("duration stats diverged: %+v vs %+v", ab, ba)
	}
	if ab.GaugeLast != ba.GaugeLast || !ab.GaugeLastTime.Equal(ba.GaugeLastTime) {
		t.Errorf("gauge last diverged: %v@%v vs %v@%v", ab.GaugeLast, ab.GaugeLastTime, ba.GaugeLast, ba.GaugeLastTime)
	}
	if ab.Sketch.Count() != ba.Sketch.Count() {
		t.Errorf("sketch counts diverged: %d vs %d", ab.Sketch.Count(), ba.Sketch.Count())
	}
}

func TestSeverityTierMapping(t *testing.T) {
	cases := []struct {
		text string
		num  int32
		want StatusClass
	}{
		{"", 17, SeverityTierError},
		{"ERROR", 0, SeverityTierError},
		{"error", 0, SeverityTierError},
		{"WARNING", 0, SeverityTierWarn},
		{"FATAL", 0, SeverityTierFatal},
		{"CRITICAL", 0, SeverityTierFatal},
		{"DEBUG", 0, SeverityTierDebug},
		{"TRACE", 0, SeverityTierTrace},
		{"INFO", 0, SeverityTierInfo},
		{"something-else", 0, SeverityTierInfo},
		{"", 0, SeverityTierUnspecified},
	}
	for _, c := range cases {
		if got := SeverityTier(c.text, c.num); got != c.want {
			t.Errorf("SeverityTier(%q, %d) = %d, want %d", c.text, c.num, got, c.want)
		}
	}
}

func TestReduceLogUsesTemplateIdentityAndSeverity(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)
	r := e.NewReducer(now)

	for i := 0; i < 5; i++ {
		r.ReduceLog(LogInput{
			Tenant: "t", Service: "svc", Severity: "ERROR",
			Body: "connection to db-7 failed after 30 retries", Timestamp: now,
		})
	}
	r.ReduceLog(LogInput{Tenant: "t", Service: "svc", Severity: "INFO", Body: "request served", Timestamp: now})
	e.ApplyReducer(r)

	// Five identical-shape ERROR lines cluster into one template series; the
	// INFO line is a second template AND a second severity tier.
	if got := len(r.Deltas()); got != 2 {
		t.Fatalf("log deltas = %d, want 2", got)
	}
	count, errors := e.Snapshot().Totals(SignalLog)
	if count != 6 {
		t.Errorf("log count = %d, want 6", count)
	}
	if errors != 5 {
		t.Errorf("log error count = %d, want 5", errors)
	}
}

func TestReduceMetricGaugeAndDelta(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e := testEngine(t, now)
	r := e.NewReducer(now)

	r.ReduceMetricPoint(MetricInput{Tenant: "t", Service: "svc", Name: "queue.depth", Value: 5, Timestamp: now})
	r.ReduceMetricPoint(MetricInput{Tenant: "t", Service: "svc", Name: "queue.depth", Value: 9, Timestamp: now.Add(time.Second)})
	r.ReduceMetricPoint(MetricInput{Tenant: "t", Service: "svc", Name: "queue.depth", Value: 2, Timestamp: now.Add(2 * time.Second)})
	r.ReduceMetricPoint(MetricInput{
		Tenant: "t", Service: "svc", Name: "requests", Value: 7,
		Timestamp: now, Temporality: TemporalityDelta, Monotonic: true,
	})

	var gauge, counter *AggregateDelta
	for swk, d := range r.Deltas() {
		if d.GaugeCount > 0 {
			gauge = d
			_ = swk
			continue
		}
		counter = d
	}
	if gauge == nil || counter == nil {
		t.Fatalf("expected a gauge delta and a counter delta, got %d deltas", len(r.Deltas()))
	}
	if gauge.GaugeCount != 3 || gauge.GaugeMin != 2 || gauge.GaugeMax != 9 || gauge.GaugeSum != 16 {
		t.Errorf("gauge stats wrong: %+v", gauge)
	}
	if gauge.GaugeLast != 2 {
		t.Errorf("gauge last = %v, want 2 (latest timestamp wins)", gauge.GaugeLast)
	}
	if counter.CounterDelta != 7 {
		t.Errorf("delta-temporality counter = %v, want 7", counter.CounterDelta)
	}
}
