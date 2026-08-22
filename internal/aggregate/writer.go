package aggregate

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// The group-commit writer (#160, ADR 0002).
//
// Contract: an Export is acknowledged only after its reduced deltas are inside
// a committed SQLite transaction AND those committed deltas have been applied
// to the shards. Commit first, apply second, release waiters third — in that
// order, so the shards are a projection of committed state and memory can lag
// durability but never lead it.
//
// Cadence: the first waiter opens a coalescing window of at most
// CoalesceWindow; the commit fires early once the batch reaches its size or
// count target. While a COMMIT is in flight the next batch accumulates in the
// submission channel, which is bounded by the same waiter cap that bounds
// admission — there is exactly one place where memory can grow and it is
// capped three ways.
//
// Admission is bounded by pending bytes, waiter count and a defensive delta
// count. A breach returns *SaturationError (errors.Is(err, ErrSaturated)),
// which internal/ingest maps to gRPC RESOURCE_EXHAUSTED / HTTP 429 exactly
// like the raw pipeline's ErrQueueFull. It never degrades to bounded-loss ACK
// on its own: that is explicit configuration only, never runtime degradation.

// Writer defaults. They are the provisional numbers of #160/#162; the
// benchmark gate ratifies or replaces them.
const (
	// DefaultCommitCoalesceMs is the first-waiter coalescing window. 25 ms,
	// not #160's provisional 5 ms: measured at 10k pts/s on 2 vCPU, 5 ms cost
	// a 37.8% writer duty cycle and a 286 ms ACK p99 against 109 ms at 25 ms
	// (#173). Kept in step with config.AggregateCommitCoalesceMs, which is
	// what production actually passes in.
	DefaultCommitCoalesceMs = 25
	// DefaultCommitMaxDeltas is the early-commit count target — the "5k dirty
	// series" shape the gate benchmarks.
	DefaultCommitMaxDeltas = 5000
	// DefaultCommitMaxBytes is the early-commit size target.
	DefaultCommitMaxBytes = 8 << 20
	// DefaultCommitMaxPendingBytes bounds un-committed delta payload.
	DefaultCommitMaxPendingBytes = 64 << 20
	// DefaultCommitMaxWaiters bounds Export goroutines parked on a commit.
	DefaultCommitMaxWaiters = 512
	// DefaultCommitMaxPendingDeltas is the defensive delta-count bound.
	DefaultCommitMaxPendingDeltas = 200000
	// DefaultFinalizeIntervalSec is how often the writer looks for windows
	// whose lateness horizon has expired.
	DefaultFinalizeIntervalSec = 30
	// finalizeWindowsPerPass bounds one finalization pass.
	finalizeWindowsPerPass = 32
)

// WriterConfig configures the group-commit writer.
type WriterConfig struct {
	// Store is the durable store. Required.
	Store Store
	// Engine is the engine whose shards receive committed deltas. Required.
	Engine *Engine
	// Registrar is the durable dictionary registrar whose staged rows ride
	// each commit. Required when the engine was built with one.
	Registrar *DurableRegistrar

	// CoalesceWindow, MaxBatchDeltas and MaxBatchBytes set the commit cadence.
	CoalesceWindow time.Duration
	MaxBatchDeltas int
	MaxBatchBytes  int64

	// MaxPendingBytes, MaxWaiters and MaxPendingDeltas are the triple
	// admission bound.
	MaxPendingBytes  int64
	MaxWaiters       int
	MaxPendingDeltas int

	// FinalizeInterval is the window-finalization tick. Zero takes the
	// default; negative disables the loop (tests drive it by hand).
	FinalizeInterval time.Duration

	// Metrics is the durable path's metric surface.
	Metrics StoreMetrics

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// withDefaults fills unset knobs.
func (c WriterConfig) withDefaults() WriterConfig {
	if c.CoalesceWindow <= 0 {
		c.CoalesceWindow = DefaultCommitCoalesceMs * time.Millisecond
	}
	if c.MaxBatchDeltas <= 0 {
		c.MaxBatchDeltas = DefaultCommitMaxDeltas
	}
	if c.MaxBatchBytes <= 0 {
		c.MaxBatchBytes = DefaultCommitMaxBytes
	}
	if c.MaxPendingBytes <= 0 {
		c.MaxPendingBytes = DefaultCommitMaxPendingBytes
	}
	if c.MaxWaiters <= 0 {
		c.MaxWaiters = DefaultCommitMaxWaiters
	}
	if c.MaxPendingDeltas <= 0 {
		c.MaxPendingDeltas = DefaultCommitMaxPendingDeltas
	}
	if c.FinalizeInterval == 0 {
		c.FinalizeInterval = DefaultFinalizeIntervalSec * time.Second
	}
	if c.Metrics == nil {
		c.Metrics = noopStoreMetrics{}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// commitResult is what a waiter receives once its batch is durable and applied.
type commitResult struct {
	revision uint64
	err      error
}

// submission is one Export's deltas awaiting a commit.
type submission struct {
	deltas DeltaMap
	bytes  int64
	count  int
	done   chan commitResult
}

// WriterStats is a snapshot of writer counters.
type WriterStats struct {
	// Commits and CommitErrors count group commits attempted and failed.
	Commits, CommitErrors uint64
	// Deltas counts delta rows written.
	Deltas uint64
	// Rejections counts ErrSaturated refusals.
	Rejections uint64
	// PendingBytes, PendingDeltas and Waiters are the live admission
	// occupancy.
	PendingBytes  int64
	PendingDeltas int
	Waiters       int
	// Finalized counts windows finalized by the writer's state machine.
	Finalized uint64
}

// Writer is the group-commit writer. It is the engine's Applier once the
// durable store is enabled.
type Writer struct {
	cfg    WriterConfig
	store  Store
	engine *Engine
	reg    *DurableRegistrar
	series *seriesRegistry

	miner *TemplateMiner

	submissions chan *submission
	// barriers carries identity-maintenance work onto the writer goroutine
	// (#200 Q2). Running it here is what makes "no group commit interleaves
	// with an identity delete" structural: there is one commit goroutine and
	// the barrier IS that goroutine.
	barriers chan *barrierRequest
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	closed   atomic.Bool

	// admission counters, guarded by mu.
	mu            sync.Mutex
	pendingBytes  int64
	pendingDeltas int
	waiters       int

	commits      atomic.Uint64
	commitErrors atomic.Uint64
	deltas       atomic.Uint64
	rejections   atomic.Uint64
	finalized    atomic.Uint64
}

// NewWriter builds a writer over store and engine. It does not start it.
func NewWriter(cfg WriterConfig) (*Writer, error) {
	cfg = cfg.withDefaults()
	if cfg.Store == nil {
		return nil, errNilStore
	}
	if cfg.Engine == nil {
		return nil, errNilEngine
	}
	series, err := newSeriesRegistry(cfg.Store)
	if err != nil {
		return nil, err
	}
	// The engine's read path serves finalized windows from the store; the
	// writer is the one place that already holds both.
	cfg.Engine.SetStore(cfg.Store)
	return &Writer{
		cfg:         cfg,
		store:       cfg.Store,
		engine:      cfg.Engine,
		reg:         cfg.Registrar,
		series:      series,
		miner:       cfg.Engine.Miner(),
		submissions: make(chan *submission, cfg.MaxWaiters),
		barriers:    make(chan *barrierRequest),
		stop:        make(chan struct{}),
	}, nil
}

// barrierRequest is one unit of identity-maintenance work to run between
// commits.
type barrierRequest struct {
	fn   func()
	done chan struct{}
}

// RunBarrier implements Barrier. It parks fn on the commit goroutine, so no
// group commit is in flight while it runs and none can start until it returns.
//
// fn must be bounded: every Export waiting on the writer is waiting on it too.
// The collector honours that by keeping its full table scan OUTSIDE the
// barrier and passing only the revalidate-fence-delete tail in here.
func (w *Writer) RunBarrier(fn func()) error {
	if fn == nil {
		return nil
	}
	if w.closed.Load() {
		return ErrStoreClosed
	}
	req := &barrierRequest{fn: fn, done: make(chan struct{})}
	select {
	case w.barriers <- req:
	case <-w.stop:
		return ErrStoreClosed
	}
	<-req.done
	return nil
}

// CollectIdentities runs one identity garbage-collection pass. It is the entry
// point retention's daily maintenance tick calls.
func (w *Writer) CollectIdentities() (GCStats, error) {
	return Collect(GCConfig{
		Store:     w.store,
		Registrar: w.reg,
		Cache:     w.engine.Cache(),
		Miner:     w.miner,
		Engine:    w.engine,
		Series:    w.series,
		Barrier:   w,
		Metrics:   w.cfg.Metrics,
	})
}

// SaveTemplateStats writes the miner's dirty non-identity counters. Identity
// mutations do NOT come through here — they ride the group commit — so a lost
// batch costs a count that the next line restores.
func (w *Writer) SaveTemplateStats() {
	if w.miner == nil {
		return
	}
	gcs, ok := w.store.(GCStore)
	if !ok {
		return
	}
	rows := w.miner.DrainDirtyStats()
	if len(rows) == 0 {
		return
	}
	if err := gcs.SaveTemplateStats(rows); err != nil {
		slog.Warn("aggregate: template statistics write failed", "error", err, "rows", len(rows))
	}
}

// Start launches the commit loop and, unless disabled, the finalize loop.
func (w *Writer) Start() {
	w.wg.Add(1)
	go w.commitLoop()
	if w.cfg.FinalizeInterval > 0 {
		w.wg.Add(1)
		go w.finalizeLoop()
	}
}

// Stop drains the in-flight submissions, commits them, and returns once the
// loops have exited. It is idempotent.
func (w *Writer) Stop() {
	w.stopOnce.Do(func() {
		w.closed.Store(true)
		close(w.stop)
	})
	w.wg.Wait()
	// Final best-effort statistics save, after the loops are gone so nothing
	// races the writer lock. Identity is already durable — it rode the group
	// commits — so this only saves counters.
	w.SaveTemplateStats()
}

// Stats returns a snapshot of the writer counters.
func (w *Writer) Stats() WriterStats {
	w.mu.Lock()
	pendingBytes, pendingDeltas, waiters := w.pendingBytes, w.pendingDeltas, w.waiters
	w.mu.Unlock()
	return WriterStats{
		Commits:       w.commits.Load(),
		CommitErrors:  w.commitErrors.Load(),
		Deltas:        w.deltas.Load(),
		Rejections:    w.rejections.Load(),
		PendingBytes:  pendingBytes,
		PendingDeltas: pendingDeltas,
		Waiters:       waiters,
		Finalized:     w.finalized.Load(),
	}
}

// Apply implements Applier. It exists so a caller that cannot act on an error
// still gets the durable path; every ingest caller uses ApplyErr instead.
func (w *Writer) Apply(m DeltaMap) uint64 {
	rev, err := w.ApplyErr(m)
	if err != nil {
		slog.Warn("aggregate: group commit failed", "error", err)
	}
	return rev
}

// ApplyErr implements FailableApplier: it blocks until the deltas are durable
// and applied, or until admission refuses them.
func (w *Writer) ApplyErr(m DeltaMap) (uint64, error) {
	if len(m) == 0 {
		return w.engine.Revision(), nil
	}
	if w.closed.Load() {
		return w.engine.Revision(), ErrStoreClosed
	}
	s := &submission{
		deltas: m,
		bytes:  estimateDeltaBytes(m),
		count:  len(m),
		done:   make(chan commitResult, 1),
	}
	if err := w.admit(s); err != nil {
		return w.engine.Revision(), err
	}
	select {
	case w.submissions <- s:
	case <-w.stop:
		// The writer is shutting down and may already have stopped reading.
		// Release the admission budget and refuse rather than park forever.
		w.release(s)
		return w.engine.Revision(), ErrStoreClosed
	}
	res := <-s.done
	return res.revision, res.err
}

// admit applies the triple bound.
func (w *Writer) admit(s *submission) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err *SaturationError
	switch {
	case w.waiters >= w.cfg.MaxWaiters:
		err = &SaturationError{Bound: "waiters", Current: int64(w.waiters), Limit: int64(w.cfg.MaxWaiters)}
	case w.pendingBytes+s.bytes > w.cfg.MaxPendingBytes:
		err = &SaturationError{Bound: "bytes", Current: w.pendingBytes + s.bytes, Limit: w.cfg.MaxPendingBytes}
	case w.pendingDeltas+s.count > w.cfg.MaxPendingDeltas:
		err = &SaturationError{Bound: "deltas", Current: int64(w.pendingDeltas + s.count), Limit: int64(w.cfg.MaxPendingDeltas)}
	}
	if err != nil {
		w.rejections.Add(1)
		w.cfg.Metrics.RecordAdmissionRejected(err.Bound)
		return err
	}
	w.waiters++
	w.pendingBytes += s.bytes
	w.pendingDeltas += s.count
	return nil
}

// release returns one submission's admission budget.
func (w *Writer) release(s *submission) {
	w.mu.Lock()
	w.waiters--
	w.pendingBytes -= s.bytes
	w.pendingDeltas -= s.count
	w.mu.Unlock()
}

// commitLoop is the writer's state machine.
func (w *Writer) commitLoop() {
	defer w.wg.Done()
	for {
		select {
		case <-w.stop:
			w.drain()
			return
		case req := <-w.barriers:
			req.fn()
			close(req.done)
		case s := <-w.submissions:
			w.collectAndCommit(s)
		}
	}
}

// drain commits whatever is still queued at shutdown. gRPC GracefulStop has
// already stopped new Exports by the time this runs, so the queue is finite.
func (w *Writer) drain() {
	for {
		select {
		case s := <-w.submissions:
			w.collectAndCommit(s)
		default:
			return
		}
	}
}

// collectAndCommit opens the coalescing window on the first waiter and commits
// once the window closes or a size/count target is reached.
func (w *Writer) collectAndCommit(first *submission) {
	batch := []*submission{first}
	bytes, count := first.bytes, first.count

	timer := time.NewTimer(w.cfg.CoalesceWindow)
	defer timer.Stop()
collect:
	for count < w.cfg.MaxBatchDeltas && bytes < w.cfg.MaxBatchBytes {
		select {
		case s := <-w.submissions:
			batch = append(batch, s)
			bytes += s.bytes
			count += s.count
		case <-timer.C:
			break collect
		}
	}
	w.commit(batch)
}

// commit is the durability boundary: one transaction, then the shard apply,
// then the waiters.
func (w *Writer) commit(batch []*submission) {
	// Pre-merge: the same (series, window) touched by several Exports becomes
	// one delta row. This is the pre-merge ratio #162's gate measures.
	merged := make(DeltaMap, len(batch[0].deltas))
	for _, s := range batch {
		for swk, d := range s.deltas {
			if cur, ok := merged[swk]; ok {
				cur.Merge(d)
				continue
			}
			merged[swk] = d
		}
	}

	// Admission against the cardinality budget happens BEFORE the write so the
	// row that becomes durable carries the same identity the shards will hold.
	// Doing it after the commit would let the store accumulate series the
	// in-memory caps already rejected. It is a RESERVATION, not a charge:
	// exactly one of CommitAdmission or RollbackAdmission settles it below, so
	// a commit that never lands cannot keep eating cardinality budget (#194).
	plan := w.engine.PlanAdmission(merged)

	gb := &GroupBatch{Deltas: make([]DeltaRow, 0, len(plan.Resolved))}
	for swk, d := range plan.Resolved {
		gb.Deltas = append(gb.Deltas, DeltaRow{
			SeriesID:    w.series.Resolve(swk.Key),
			WindowStart: swk.WindowStart,
			Delta:       d,
		})
	}
	// Baselines are keyed by the FULL canonical identity, before cardinality
	// overflow routing (#166): two series that collapse onto one __other__
	// series still have independent counters. That means a baseline can mint a
	// series row for a key the shards never materialize — bounded, because the
	// baseline table itself is bounded by AGGREGATE_MAX_BASELINES.
	dirty := w.engine.Baselines().DrainDirty()
	for _, b := range dirty {
		gb.Baselines = append(gb.Baselines, BaselineRow{
			SeriesID: w.series.Resolve(b.Key),
			Producer: b.Producer,
			Baseline: b.Baseline,
		})
	}
	// Series first, dictionary second: resolving a series may have minted a
	// dictionary-free identity, but never the reverse, and both drains must
	// happen after every Resolve above.
	gb.Series = w.series.DrainPending()
	if w.reg != nil {
		gb.Dicts = w.reg.DrainPending()
	}
	// Identity-critical miner mutations ride the same transaction as the
	// delta that used the resulting identity (#200 Q4). A periodic snapshot
	// alone would let a crash acknowledge a bucket whose NameID the reloaded
	// miner has never heard of.
	if w.miner != nil {
		gb.Templates = w.miner.DrainPending()
	}

	err := w.store.CommitGroup(gb)
	w.commits.Add(1)
	if err != nil {
		w.commitErrors.Add(1)
		// Nothing became durable, so nothing may keep the resources the write
		// would have justified: release the cardinality reservation, hand the
		// drained baselines back the increase this batch was carrying, and
		// leave the staged registrations staged.
		w.engine.RollbackAdmission(plan)
		w.engine.Baselines().Rollback(dirty)
		w.finish(batch, commitResult{revision: w.engine.Revision(), err: err})
		return
	}
	w.deltas.Add(uint64(len(gb.Deltas)))
	w.series.Committed(gb.Series)
	if w.reg != nil {
		w.reg.Committed(gb.Dicts)
	}
	if w.miner != nil {
		w.miner.Committed(gb.Templates)
	}

	// Commit-then-apply: the shards only ever see committed state.
	rev := w.engine.CommitAdmission(plan)
	w.finish(batch, commitResult{revision: rev})
}

// finish releases the batch's waiters and their admission budget.
func (w *Writer) finish(batch []*submission, res commitResult) {
	for _, s := range batch {
		w.release(s)
		s.done <- res
	}
}

// finalizeLoop drives window finalization. It lives on the writer because
// finalization has to serialize with incoming late deltas (#162): the store's
// writer lock is what provides that, and the loop is what decides when.
func (w *Writer) finalizeLoop() {
	defer w.wg.Done()
	tick := time.NewTicker(w.cfg.FinalizeInterval)
	defer tick.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-tick.C:
			w.FinalizeDue(w.cfg.Now())
			w.publishBacklog(w.cfg.Now())
			// Non-identity template counters take the cheap periodic path.
			w.SaveTemplateStats()
		}
	}
}

// FinalizeDue finalizes every window whose lateness horizon expired at now and
// returns how many it finalized. Exported so recovery and tests can drive the
// same state machine the loop drives.
func (w *Writer) FinalizeDue(now time.Time) int {
	windows, err := w.store.FinalizableWindows(FinalizeCutoff(now), finalizeWindowsPerPass)
	if err != nil {
		slog.Error("aggregate: list finalizable windows failed", "error", err)
		return 0
	}
	done := 0
	for _, window := range windows {
		if _, err := w.store.FinalizeWindow(window); err != nil {
			slog.Error("aggregate: finalize window failed", "window_start", window, "error", err)
			continue
		}
		// The buckets are committed, so ownership of the window moves to the
		// store in the same step that evicts it from the shards (#164).
		w.engine.MarkFinalized(window)
		w.finalized.Add(1)
		done++
	}
	return done
}

// publishBacklog pushes the delta-log health bounds into the metric surface.
func (w *Writer) publishBacklog(now time.Time) {
	stats, err := w.store.Backlog()
	if err != nil {
		slog.Warn("aggregate: backlog probe failed", "error", err)
		return
	}
	age := 0.0
	if stats.OldestWindow > 0 {
		age = now.Sub(time.Unix(stats.OldestWindow, 0)).Seconds()
	}
	w.cfg.Metrics.SetBacklog(stats.Rows, age)
}

// SeriesID exposes the writer's series resolution for tests and for the read
// path that has to turn a SeriesKey into a durable ID.
func (w *Writer) SeriesID(key SeriesKey) SeriesID { return w.series.Resolve(key) }

// SeriesKeyByID resolves a durable ID back to its identity from memory.
func (w *Writer) SeriesKeyByID(id SeriesID) (SeriesKey, bool) { return w.series.Key(id) }

// estimateDeltaBytes approximates the on-disk cost of one reducer's output. It
// is an admission input, not an accounting figure: it must be cheap and it must
// never under-count a sketch badly enough to matter.
func estimateDeltaBytes(m DeltaMap) int64 {
	var n int64
	for _, d := range m {
		n += deltaRowBytes
		if d != nil && d.Sketch != nil {
			bins := int64(d.Sketch.PopulatedBins())
			if bins > SketchMaxSerializedBins {
				bins = SketchMaxSerializedBins
			}
			n += 16 + bins*3
		}
	}
	return n
}

// compile-time assertions.
var (
	_ Applier         = (*Writer)(nil)
	_ FailableApplier = (*Writer)(nil)
	_ Barrier         = (*Writer)(nil)
)

// Writer construction errors.
var (
	errNilStore  = errStr("aggregate: writer requires a store")
	errNilEngine = errStr("aggregate: writer requires an engine")
)

// errStr is a tiny constant error type: these two are programming errors at
// construction time, not conditions a caller branches on.
type errStr string

func (e errStr) Error() string { return string(e) }
