package aggregate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Acceptance tests for #194 blockers 2, 3 and 6 — what a failed group commit
// and a closed-but-unfinalized window must leave behind.
//
// All three defects share one shape: a resource was taken at reduction or
// admission time and released on the assumption that the write would land.
// These tests drive the write failing and assert on what is left.

var errCommitRefused = errors.New("aggregate: commit refused (test)")

// failableStore is a Store whose commits and finalizations can be switched off
// and on, so one test can drive a failure and then the retry through the same
// writer.
type failableStore struct {
	Store
	failCommit   atomic.Bool
	failFinalize atomic.Bool
	commits      atomic.Int64
	finalizes    atomic.Int64
}

func (f *failableStore) CommitGroup(b *GroupBatch) error {
	f.commits.Add(1)
	if f.failCommit.Load() {
		return errCommitRefused
	}
	return f.Store.CommitGroup(b)
}

func (f *failableStore) FinalizeWindow(window int64) (FinalizeStats, error) {
	f.finalizes.Add(1)
	if f.failFinalize.Load() {
		return FinalizeStats{}, errCommitRefused
	}
	return f.Store.FinalizeWindow(window)
}

// cumulativeFixture is an engine and writer over a store whose commits can be
// refused on demand.
type cumulativeFixture struct {
	clock *fixedClock
	base  *SQLiteStore
	store *failableStore
	eng   *Engine
	w     *Writer
}

func newCumulativeFixture(t *testing.T) *cumulativeFixture {
	t.Helper()
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	store := &failableStore{Store: base}
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, store, eng, clock, WriterConfig{})
	return &cumulativeFixture{clock: clock, base: base, store: store, eng: eng, w: w}
}

// observe reduces and applies one cumulative monotonic point, returning the
// applier's refusal so a test can assert the durable-ACK contract.
func (f *cumulativeFixture) observe(start, ts time.Time, value float64) error {
	r := f.eng.NewReducer(f.clock.Now())
	r.ReduceMetricPoint(MetricInput{
		Tenant:      "acme",
		Service:     "svc",
		Name:        "requests",
		Value:       value,
		Timestamp:   ts,
		StartTime:   start,
		Temporality: TemporalityCumulative,
		Monotonic:   true,
		Resource:    ResourceIdentity{ServiceInstanceID: "pod-1"},
	})
	_, err := f.eng.ApplyReducerErr(r)
	return err
}

func (f *cumulativeFixture) mustObserve(t *testing.T, start, ts time.Time, value float64) {
	t.Helper()
	if err := f.observe(start, ts, value); err != nil {
		t.Fatalf("observe(%v) = %v, want success", value, err)
	}
}

// durableCounterDelta sums the counter increase that actually reached the
// store. It is the only number that matters: memory can be as clever as it
// likes, the delta log is what survives a restart.
func durableCounterDelta(t *testing.T, s *SQLiteStore) float64 {
	t.Helper()
	rows, err := s.ReplayMutable(0)
	if err != nil {
		t.Fatalf("ReplayMutable: %v", err)
	}
	var total float64
	for _, r := range rows {
		total += r.Delta.CounterDelta
	}
	return total
}

// --- blocker 2: baselines must not advance past the durable commit ---

// TestCommitFailureThenIdenticalRetryPreservesTheDelta is the exact failure
// #194 describes: baseline 100, point 125, commit fails, client retries 125.
// Before the fix the retry classified as stale and the increase of 25 was gone.
func TestCommitFailureThenIdenticalRetryPreservesTheDelta(t *testing.T) {
	f := newCumulativeFixture(t)
	start := f.clock.Now().Add(-time.Hour)
	t0 := f.clock.Now()

	f.mustObserve(t, start, t0, 100) // seed: no delta is attributed
	if got := durableCounterDelta(t, f.base); got != 0 {
		t.Fatalf("seed attributed %v, want 0", got)
	}

	f.store.failCommit.Store(true)
	if err := f.observe(start, t0.Add(10*time.Second), 125); !errors.Is(err, errCommitRefused) {
		t.Fatalf("failed commit = %v, want the commit error — it must not be acknowledged", err)
	}

	// The client retries the identical Export.
	f.store.failCommit.Store(false)
	f.mustObserve(t, start, t0.Add(10*time.Second), 125)

	if got := durableCounterDelta(t, f.base); got != 25 {
		t.Fatalf("durable increase = %v, want 25 — the retry must reproduce the lost delta", got)
	}
	if owed := f.eng.Baselines().Stats().Owed; owed != 0 {
		t.Errorf("%d baselines still owed after a successful retry", owed)
	}
}

// TestCommitFailureThenNewerPointNeitherLosesNorDuplicates covers the case a
// naive baseline rewind gets wrong: nothing retries the failed point, a newer
// one arrives instead, and the totals must still be exact afterwards.
func TestCommitFailureThenNewerPointNeitherLosesNorDuplicates(t *testing.T) {
	f := newCumulativeFixture(t)
	start := f.clock.Now().Add(-time.Hour)
	t0 := f.clock.Now()

	f.mustObserve(t, start, t0, 100)

	// Every point stays inside MaxFutureSkew of the fixed arrival clock, so
	// the reducer accepts them all into the same window.
	f.store.failCommit.Store(true)
	if err := f.observe(start, t0.Add(10*time.Second), 125); err == nil {
		t.Fatal("failed commit was acknowledged")
	}

	// No retry: a newer point arrives. 100 -> 150 is an increase of 50.
	f.store.failCommit.Store(false)
	f.mustObserve(t, start, t0.Add(20*time.Second), 150)
	if got := durableCounterDelta(t, f.base); got != 50 {
		t.Fatalf("durable increase = %v, want 50 (100 -> 150)", got)
	}

	// And the ledger must be settled exactly once: the next point contributes
	// only its own increase.
	f.mustObserve(t, start, t0.Add(30*time.Second), 175)
	if got := durableCounterDelta(t, f.base); got != 75 {
		t.Fatalf("durable increase = %v, want 75 (100 -> 175) — the stranded delta was counted twice", got)
	}

	// A late duplicate of the failed point must now be inert.
	f.mustObserve(t, start, t0.Add(10*time.Second), 125)
	if got := durableCounterDelta(t, f.base); got != 75 {
		t.Fatalf("durable increase = %v after a stale duplicate, want 75", got)
	}
}

// TestConcurrentCumulativePointsCommitDeterministically drives many concurrent
// Exports of one cumulative series. Whatever order the tracker serializes them
// in, the accepted chain telescopes: the durable increase is last value minus
// the seed, exactly, with no dependence on interleaving.
func TestConcurrentCumulativePointsCommitDeterministically(t *testing.T) {
	f := newCumulativeFixture(t)
	start := f.clock.Now().Add(-time.Hour)
	t0 := f.clock.Now()
	f.mustObserve(t, start, t0, 100)

	const points = 64
	var wg sync.WaitGroup
	errs := make([]error, points)
	for i := 0; i < points; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f.observe(start, t0.Add(time.Duration(i+1)*time.Millisecond), float64(101+i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("point %d: %v", i, err)
		}
	}
	if got := durableCounterDelta(t, f.base); got != points {
		t.Fatalf("durable increase = %v, want %d (100 -> %d)", got, points, 100+points)
	}
}

// --- blocker 3: cardinality admission must not leak on commit failure ---

// occupancy is the limiter census a rolled-back commit must leave untouched.
func occupancy(e *Engine) (active, overflow int) {
	ls := e.Limiter().Stats()
	return ls.Active, ls.OverflowSeries
}

func TestAdmissionOccupancyIsExactAfterRepeatedCommitFailures(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	store := &failableStore{Store: base}
	eng := newTestEngine(t, clock, nil)
	newTestWriter(t, store, eng, clock, WriterConfig{})

	store.failCommit.Store(true)
	for i := 0; i < 20; i++ {
		if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), uint32(i%3)+1, 1)); err == nil {
			t.Fatalf("apply %d was acknowledged despite a refused commit", i)
		}
	}
	if active, overflow := occupancy(eng); active != 0 || overflow != 0 {
		t.Fatalf("occupancy after 20 refused commits = (%d active, %d overflow), want (0, 0) — "+
			"a series that never reached a shard has no window to be released from", active, overflow)
	}
}

func TestAdmissionOccupancyIsExactAfterFailureThenRetry(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	store := &failableStore{Store: base}
	eng := newTestEngine(t, clock, nil)
	newTestWriter(t, store, eng, clock, WriterConfig{})

	store.failCommit.Store(true)
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 1)); err == nil {
		t.Fatal("refused commit was acknowledged")
	}
	store.failCommit.Store(false)
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 1)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if active, _ := occupancy(eng); active != 1 {
		t.Fatalf("active series after failure-then-retry = %d, want 1", active)
	}

	// A later failure on the SAME series must not release the occupancy the
	// committed batch is still using: the reservation only covers presence the
	// failed plan itself created.
	store.failCommit.Store(true)
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 1)); err == nil {
		t.Fatal("refused commit was acknowledged")
	}
	if active, _ := occupancy(eng); active != 1 {
		t.Fatalf("active series = %d after a failure on a live series, want 1 — "+
			"rollback released budget belonging to committed data", active)
	}
}

func TestAdmissionOccupancyIsExactAcrossWindowRollover(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	store := &failableStore{Store: base}
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, store, eng, clock, WriterConfig{})

	first := WindowStart(clock.Now())
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 1)); err != nil {
		t.Fatalf("first window: %v", err)
	}
	// The same series in the next window: one series, two windows, one charge.
	clock.Advance(WindowSize)
	second := WindowStart(clock.Now())
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 1)); err != nil {
		t.Fatalf("second window: %v", err)
	}
	if active, _ := occupancy(eng); active != 1 {
		t.Fatalf("active series across two windows = %d, want 1", active)
	}

	// A refused commit for a new series in the live window leaves nothing.
	store.failCommit.Store(true)
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 2, 1)); err == nil {
		t.Fatal("refused commit was acknowledged")
	}
	store.failCommit.Store(false)
	if active, _ := occupancy(eng); active != 1 {
		t.Fatalf("active series = %d after a refused commit, want 1", active)
	}

	// Finalizing the first window does not release a series still live in the
	// second; finalizing the second does.
	clock.Advance(AllowedLateness + time.Minute)
	if n := w.FinalizeDue(clock.Now()); n != 1 {
		t.Fatalf("finalized %d windows, want 1 (%d)", n, first)
	}
	if active, _ := occupancy(eng); active != 1 {
		t.Fatalf("active series = %d after finalizing the older window, want 1", active)
	}
	clock.Advance(WindowSize)
	if n := w.FinalizeDue(clock.Now()); n != 1 {
		t.Fatalf("finalized %d windows, want 1 (%d)", n, second)
	}
	if active, overflow := occupancy(eng); active != 0 || overflow != 0 {
		t.Fatalf("occupancy after both windows finalized = (%d, %d), want (0, 0)", active, overflow)
	}
}

// --- blocker 6: only a committed finalize may evict and advance ownership ---

// seedTraceWindow lands `spans` spans for one service through the writer and
// returns the window they landed in.
func seedTraceWindow(t *testing.T, f *cumulativeFixture, spans int) int64 {
	t.Helper()
	r := f.eng.NewReducer(f.clock.Now())
	for i := 0; i < spans; i++ {
		r.ReduceSpan(SpanInput{
			Tenant:         "acme",
			Service:        "svc",
			SpanName:       "GET /orders",
			Timestamp:      f.clock.Now(),
			DurationMicros: 250,
		})
	}
	if _, err := f.eng.ApplyReducerErr(r); err != nil {
		t.Fatalf("seed spans: %v", err)
	}
	return WindowStart(f.clock.Now())
}

// dashboardSpans queries the whole retained range and returns the SPAN count.
// These tests seed spans directly and are about window VISIBILITY, not about
// the request/span basis, so the span counter is the honest assertion here.
func dashboardSpans(t *testing.T, e *Engine, from, to time.Time) int64 {
	t.Helper()
	res, err := e.QueryDashboard(context.Background(), Query{Tenant: "acme", Start: from, End: to})
	if err != nil {
		t.Fatalf("QueryDashboard: %v", err)
	}
	return res.SpanCount
}

// TestQueryDuringRolloverFinalizeGapReturnsTheWindow is the read-side proof of
// blocker 6: between the lateness horizon expiring and the finalizer committing
// buckets, the window must still answer queries. It used to vanish, because
// rollover had already handed ownership to a store with nothing in it.
func TestQueryDuringRolloverFinalizeGapReturnsTheWindow(t *testing.T) {
	f := newCumulativeFixture(t)
	from := f.clock.Now().Add(-time.Minute)
	seedTraceWindow(t, f, 5)

	f.clock.Advance(WindowSize + AllowedLateness + time.Minute)
	// The gap: closed, not finalized.
	if forced := f.eng.Rollover(f.clock.Now()); forced != 0 {
		t.Fatalf("rollover force-evicted %d windows, want 0", forced)
	}
	if got := dashboardSpans(t, f.eng, from, f.clock.Now()); got != 5 {
		t.Fatalf("SpanCount in the rollover-to-finalize gap = %d, want 5 — the window vanished", got)
	}

	// After the finalize commits, the same query is served from the store.
	if n := f.w.FinalizeDue(f.clock.Now()); n != 1 {
		t.Fatalf("finalized %d windows, want 1", n)
	}
	if got := dashboardSpans(t, f.eng, from, f.clock.Now()); got != 5 {
		t.Fatalf("SpanCount after finalize = %d, want 5", got)
	}
}

// TestFinalizerFailureKeepsTheWindowReadable is the same property against a
// finalizer that keeps failing: memory holds the window until materialization
// actually commits, however long that takes.
func TestFinalizerFailureKeepsTheWindowReadable(t *testing.T) {
	f := newCumulativeFixture(t)
	from := f.clock.Now().Add(-time.Minute)
	seedTraceWindow(t, f, 7)

	f.clock.Advance(WindowSize + AllowedLateness + time.Minute)
	f.store.failFinalize.Store(true)
	for i := 0; i < 5; i++ {
		if n := f.w.FinalizeDue(f.clock.Now()); n != 0 {
			t.Fatalf("pass %d finalized %d windows against a failing store", i, n)
		}
		if got := dashboardSpans(t, f.eng, from, f.clock.Now()); got != 7 {
			t.Fatalf("SpanCount after %d failed finalizes = %d, want 7", i+1, got)
		}
	}
	if own := f.eng.Ownership(); !own.OwnsInMemory(WindowStart(from.Add(time.Minute))) {
		t.Error("a window the finalizer never materialized lost memory ownership")
	}

	// The finalizer recovers and the handover happens exactly once.
	f.store.failFinalize.Store(false)
	if n := f.w.FinalizeDue(f.clock.Now()); n != 1 {
		t.Fatalf("finalized %d windows after recovery, want 1", n)
	}
	if got := dashboardSpans(t, f.eng, from, f.clock.Now()); got != 7 {
		t.Fatalf("SpanCount after the finalizer recovered = %d, want 7", got)
	}
}
