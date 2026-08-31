package aggregate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixedClock is a settable clock for the engine and the writer.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(t time.Time) *fixedClock { return &fixedClock{now: t} }

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// hookStore wraps a Store so a test can block, fail, or observe a commit.
type hookStore struct {
	Store
	beforeCommit func(*GroupBatch)
	commitErr    error
	commits      atomic.Int64
}

func (h *hookStore) CommitGroup(b *GroupBatch) error {
	h.commits.Add(1)
	if h.beforeCommit != nil {
		h.beforeCommit(b)
	}
	if h.commitErr != nil {
		return h.commitErr
	}
	return h.Store.CommitGroup(b)
}

// newTestEngine builds an engine on a fixed clock.
func newTestEngine(t *testing.T, clock *fixedClock, reg Registrar) *Engine {
	t.Helper()
	eng, err := NewEngine(EngineConfig{
		Mode:      ModeAggregate,
		Registrar: reg,
		Now:       clock.Now,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

// newTestWriter wires a writer over store and engine and starts it.
func newTestWriter(t *testing.T, store Store, eng *Engine, clock *fixedClock, cfg WriterConfig) *Writer {
	t.Helper()
	cfg.Store = store
	cfg.Engine = eng
	cfg.Now = clock.Now
	if cfg.FinalizeInterval == 0 {
		cfg.FinalizeInterval = -1 // driven by hand in tests
	}
	w, err := NewWriter(cfg)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	eng.SetApplier(w)
	w.Start()
	t.Cleanup(w.Stop)
	return w
}

// deltaFor builds a one-series delta map in the current window.
func deltaFor(now time.Time, name uint32, count int) DeltaMap {
	return DeltaMap{
		SeriesWindowKey{Key: storeKey(name), WindowStart: WindowStart(now)}: spanDelta(count, 250),
	}
}

func TestWriterCommitsAndApplies(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	store := newTestStore(t)
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, store, eng, clock, WriterConfig{})

	rev, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 4))
	if err != nil {
		t.Fatalf("ApplyDeltasErr: %v", err)
	}
	if rev == 0 {
		t.Fatal("revision did not advance after a durable apply")
	}
	snap := eng.Snapshot()
	count, _ := snap.Totals(SignalTraceOp)
	if count != 4 {
		t.Fatalf("engine count = %d, want 4", count)
	}
	rows, err := store.ReplayMutable(0)
	if err != nil {
		t.Fatalf("ReplayMutable: %v", err)
	}
	if len(rows) != 1 || rows[0].Delta.Count != 4 {
		t.Fatalf("durable rows = %+v, want one row with count 4", rows)
	}
	if got := w.Stats().Deltas; got != 1 {
		t.Fatalf("writer wrote %d delta rows, want 1", got)
	}
}

// TestWriterCommitPrecedesApply proves the ordering the ACK contract depends
// on: at the moment the transaction is being written, the shards must not yet
// hold the deltas.
func TestWriterCommitPrecedesApply(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	eng := newTestEngine(t, clock, nil)

	var appliedAtCommit uint64
	hooked := &hookStore{Store: base}
	hooked.beforeCommit = func(*GroupBatch) {
		snap := eng.Snapshot()
		appliedAtCommit, _ = snap.Totals(SignalTraceOp)
	}
	newTestWriter(t, hooked, eng, clock, WriterConfig{})

	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 3)); err != nil {
		t.Fatalf("ApplyDeltasErr: %v", err)
	}
	if appliedAtCommit != 0 {
		t.Fatalf("shards held %d points during COMMIT; apply must follow the commit", appliedAtCommit)
	}
	snap := eng.Snapshot()
	if count, _ := snap.Totals(SignalTraceOp); count != 3 {
		t.Fatalf("shards hold %d points after the commit, want 3", count)
	}
}

func TestWriterCommitFailureIsNotAcknowledged(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	eng := newTestEngine(t, clock, nil)
	boom := errors.New("commit exploded")
	hooked := &hookStore{Store: base, commitErr: boom}
	newTestWriter(t, hooked, eng, clock, WriterConfig{})

	_, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 3))
	if !errors.Is(err, boom) {
		t.Fatalf("ApplyDeltasErr = %v, want the commit error", err)
	}
	snap := eng.Snapshot()
	if count, _ := snap.Totals(SignalTraceOp); count != 0 {
		t.Fatalf("shards hold %d points after a failed commit, want 0", count)
	}
}

// TestWriterCoalescesConcurrentApplies asserts the natural group commit: many
// concurrent Exports must produce materially fewer transactions than callers,
// and every caller must be released only after its commit.
func TestWriterCoalescesConcurrentApplies(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	eng := newTestEngine(t, clock, nil)

	release := make(chan struct{})
	var firstCommit sync.Once
	hooked := &hookStore{Store: base}
	hooked.beforeCommit = func(*GroupBatch) {
		// Hold the first commit open long enough for the rest of the callers
		// to queue behind it — the "next batch accumulates while a COMMIT is
		// in flight" shape from #160.
		firstCommit.Do(func() { <-release })
	}
	newTestWriter(t, hooked, eng, clock, WriterConfig{CoalesceWindow: 20 * time.Millisecond})

	const callers = 32
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = eng.ApplyDeltasErr(deltaFor(clock.Now(), uint32(i%4)+1, 1))
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	commits := hooked.commits.Load()
	if commits >= callers {
		t.Fatalf("%d callers produced %d commits; coalescing did nothing", callers, commits)
	}
	snap := eng.Snapshot()
	if count, _ := snap.Totals(SignalTraceOp); count != callers {
		t.Fatalf("engine counted %d points, want %d", count, callers)
	}
	rows, err := base.ReplayMutable(0)
	if err != nil {
		t.Fatalf("ReplayMutable: %v", err)
	}
	var durable uint64
	for _, r := range rows {
		durable += r.Delta.Count
	}
	if durable != callers {
		t.Fatalf("durable points = %d, want %d", durable, callers)
	}
	if w := eng; w == nil {
		t.Fatal("unreachable")
	}
}

func TestWriterAdmissionBounds(t *testing.T) {
	cases := []struct {
		name  string
		cfg   WriterConfig
		bound string
	}{
		{"waiters", WriterConfig{MaxWaiters: 1}, "waiters"},
		{"bytes", WriterConfig{MaxPendingBytes: deltaRowBytes + 64}, "bytes"},
		{"deltas", WriterConfig{MaxPendingDeltas: 1}, "deltas"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock(time.Unix(3_000_000, 0).UTC())
			base := newTestStore(t)
			eng := newTestEngine(t, clock, nil)
			release := make(chan struct{})
			var once sync.Once
			hooked := &hookStore{Store: base}
			hooked.beforeCommit = func(*GroupBatch) { once.Do(func() { <-release }) }
			newTestWriter(t, hooked, eng, clock, tc.cfg)

			// One caller parks inside the commit, holding its admission budget.
			parked := make(chan error, 1)
			go func() {
				_, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 1))
				parked <- err
			}()
			waitFor(t, func() bool { return hooked.commits.Load() == 1 })

			// Two more series so the delta-count bound is genuinely exceeded.
			m := deltaFor(clock.Now(), 2, 1)
			for k, v := range deltaFor(clock.Now(), 3, 1) {
				m[k] = v
			}
			_, err := eng.ApplyDeltasErr(m)
			if !errors.Is(err, ErrSaturated) {
				t.Fatalf("second apply = %v, want ErrSaturated", err)
			}
			var sat *SaturationError
			if !errors.As(err, &sat) || sat.Bound != tc.bound {
				t.Fatalf("saturation bound = %+v, want %s", sat, tc.bound)
			}

			close(release)
			if err := <-parked; err != nil {
				t.Fatalf("parked caller: %v", err)
			}
		})
	}
}

// TestWriterDurableRegistrationRidesTheCommit proves #162's first atomicity
// invariant end to end: the dictionary and series rows an Export minted are in
// the same transaction as its first delta.
func TestWriterDurableRegistrationRidesTheCommit(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	reg, err := NewDurableRegistrar(base, nil)
	if err != nil {
		t.Fatalf("NewDurableRegistrar: %v", err)
	}
	eng := newTestEngine(t, clock, reg)
	w := newTestWriter(t, base, eng, clock, WriterConfig{Registrar: reg})

	tenant, _ := eng.Cache().InternTenant("acme")
	nameID := eng.Cache().Intern(tenant, KindOperation, "GET /orders")
	if reg.PendingCount() == 0 {
		t.Fatal("registrations were not staged before the commit")
	}
	assertEmpty(t, base, "aggregate_dict")

	key := SeriesKey{TenantID: tenant, ServiceID: 1, NameID: nameID, Signal: SignalTraceOp}
	m := DeltaMap{SeriesWindowKey{Key: key, WindowStart: WindowStart(clock.Now())}: spanDelta(2, 100)}
	if _, err := eng.ApplyDeltasErr(m); err != nil {
		t.Fatalf("ApplyDeltasErr: %v", err)
	}
	if reg.PendingCount() != 0 {
		t.Fatalf("%d registrations still staged after a successful commit", reg.PendingCount())
	}
	dicts, err := base.LoadDict(0)
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	if len(dicts) < 2 {
		t.Fatalf("dictionary holds %d rows, want the tenant and operation entries", len(dicts))
	}
	series, err := base.LoadSeries(0)
	if err != nil {
		t.Fatalf("LoadSeries: %v", err)
	}
	if len(series) != 1 || series[0].Key != key {
		t.Fatalf("series rows = %+v, want exactly the referenced key", series)
	}
	if _, ok := w.SeriesKeyByID(series[0].ID); !ok {
		t.Fatal("writer cannot resolve the series id it just minted")
	}
}

// TestWriterBaselineRidesTheCommit covers #166: the baseline upsert and the
// delta it justifies are one transaction.
func TestWriterBaselineRidesTheCommit(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	eng := newTestEngine(t, clock, nil)
	newTestWriter(t, base, eng, clock, WriterConfig{})

	key := SeriesKey{TenantID: 1, ServiceID: 1, NameID: 9, Signal: SignalMetric}
	out := eng.Baselines().ObserveCumulative(key, 77, clock.Now().Add(-time.Hour), clock.Now(), 100)
	if !out.Seeded {
		t.Fatalf("first cumulative point = %+v, want a seed", out)
	}
	if eng.Baselines().DirtyCount() != 1 {
		t.Fatal("baseline mutation did not mark the record dirty")
	}
	m := DeltaMap{SeriesWindowKey{Key: key, WindowStart: WindowStart(clock.Now())}: spanDelta(1, 10)}
	if _, err := eng.ApplyDeltasErr(m); err != nil {
		t.Fatalf("ApplyDeltasErr: %v", err)
	}
	if eng.Baselines().DirtyCount() != 0 {
		t.Fatal("baseline is still dirty after a successful commit")
	}
	rows, err := base.LoadBaselines(0)
	if err != nil {
		t.Fatalf("LoadBaselines: %v", err)
	}
	if len(rows) != 1 || rows[0].Producer != 77 || rows[0].Baseline.Value != 100 {
		t.Fatalf("durable baselines = %+v", rows)
	}
}

func TestWriterFinalizeDue(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, base, eng, clock, WriterConfig{})

	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 5)); err != nil {
		t.Fatalf("ApplyDeltasErr: %v", err)
	}
	if n := w.FinalizeDue(clock.Now()); n != 0 {
		t.Fatalf("finalized %d windows while still mutable", n)
	}
	clock.Advance(WindowSize + AllowedLateness + time.Minute)
	if n := w.FinalizeDue(clock.Now()); n != 1 {
		t.Fatalf("finalized %d windows after the lateness horizon, want 1", n)
	}
	assertCount(t, base, "aggregate_delta_log", 0)
	assertCount(t, base, "aggregate_buckets", 1)
}

func TestWriterRefusesAfterStop(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, base, eng, clock, WriterConfig{})
	w.Stop()
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 1)); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("apply after Stop = %v, want ErrStoreClosed", err)
	}
}

func TestWriterShutdownDrainsAndIsIdempotent(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, base, eng, clock, WriterConfig{})
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 1)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

// waitFor spins until cond is true or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
