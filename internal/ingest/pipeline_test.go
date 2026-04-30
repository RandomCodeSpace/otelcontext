package ingest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// fakeWriter is a deterministic, in-memory pipelineWriter for tests.
// Records every call in order and supports configurable failure modes
// without needing SQLite.
type fakeWriter struct {
	mu sync.Mutex

	tracesCalls [][]storage.Trace
	spansCalls  [][]storage.Span
	logsCalls   [][]storage.Log
	order       []string // sequence of "traces"/"spans"/"logs" tags

	// Optional failure injectors. When set, the corresponding BatchCreate*
	// returns the configured error on its next call.
	traceErr error
	spanErr  error
	logErr   error

	// When >0, BatchCreateSpans blocks for this duration before returning.
	// Used to keep batches "in flight" while we observe queue depth.
	spanDelay time.Duration
}

func (f *fakeWriter) BatchCreateTraces(t []storage.Trace) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tracesCalls = append(f.tracesCalls, t)
	f.order = append(f.order, "traces")
	return f.traceErr
}

func (f *fakeWriter) BatchCreateSpans(s []storage.Span) error {
	if f.spanDelay > 0 {
		time.Sleep(f.spanDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spansCalls = append(f.spansCalls, s)
	f.order = append(f.order, "spans")
	return f.spanErr
}

func (f *fakeWriter) BatchCreateLogs(l []storage.Log) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsCalls = append(f.logsCalls, l)
	f.order = append(f.order, "logs")
	return f.logErr
}

// BatchCreateAll mirrors Repository.BatchCreateAll's all-or-nothing semantics:
// each inner method is called in Trace→Span→Log order; the first error
// short-circuits and is returned. Existing tests that observe per-method call
// counts and ordering keep working without modification.
func (f *fakeWriter) BatchCreateAll(t []storage.Trace, s []storage.Span, l []storage.Log) error {
	if len(t) > 0 {
		if err := f.BatchCreateTraces(t); err != nil {
			return err
		}
	}
	if len(s) > 0 {
		if err := f.BatchCreateSpans(s); err != nil {
			return err
		}
	}
	if len(l) > 0 {
		if err := f.BatchCreateLogs(l); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeWriter) snapshotOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.order))
	copy(out, f.order)
	return out
}

// healthyBatch builds a Batch with one each of trace/span/log and no
// priority flags — eligible for soft-backpressure drops.
func healthyBatch() *Batch {
	return &Batch{
		Type:   SignalTraces,
		Tenant: "t1",
		Traces: []storage.Trace{{TraceID: "trace-1", ServiceName: "svc"}},
		Spans:  []storage.Span{{TraceID: "trace-1", SpanID: "span-1", ServiceName: "svc"}},
		Logs:   []storage.Log{{TraceID: "trace-1", Body: "ok"}},
	}
}

// errorBatch is identical to healthyBatch but flagged HasError, so soft
// backpressure must let it through.
func errorBatch() *Batch {
	b := healthyBatch()
	b.HasError = true
	return b
}

// waitFor polls until pred() returns true or the deadline elapses.
// Returns false on timeout. Used to bridge async submit→worker latency.
func waitFor(t *testing.T, timeout time.Duration, pred func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return pred()
}

// ===== Submission semantics =====

func TestPipeline_NilBatch_NoOp(t *testing.T) {
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 1})
	if err := p.Submit(nil); err != nil {
		t.Fatalf("Submit(nil) returned %v, want nil", err)
	}
	if got := p.Stats().Enqueued; got != 0 {
		t.Fatalf("Submit(nil) enqueued %d batches, want 0", got)
	}
}

func TestPipeline_EmptyBatch_NoOp(t *testing.T) {
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 1})
	empty := &Batch{Type: SignalTraces, Tenant: "t"}
	if err := p.Submit(empty); err != nil {
		t.Fatalf("Submit(empty) returned %v, want nil", err)
	}
	if got := p.Stats().Enqueued; got != 0 {
		t.Fatalf("empty batch enqueued %d, want 0 — should skip channel entirely", got)
	}
}

func TestPipeline_AcceptsBelowSoftThreshold(t *testing.T) {
	// Capacity 10, soft threshold 0.9 → first 9 healthy submits go through.
	// Workers=0 means nothing drains; depth grows monotonically.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 0, SoftThreshold: 0.9})
	for range 9 {
		if err := p.Submit(healthyBatch()); err != nil {
			t.Fatalf("submit below soft threshold returned %v, want nil", err)
		}
	}
	stats := p.Stats()
	if stats.Enqueued != 9 || stats.DroppedHealthy != 0 {
		t.Fatalf("below soft threshold: enqueued=%d dropped=%d, want 9/0", stats.Enqueued, stats.DroppedHealthy)
	}
}

func TestPipeline_DropsHealthyAtSoftThreshold(t *testing.T) {
	// Capacity 10, soft threshold 0.9. Fill to 9 (below), then submit
	// healthy → should drop. Verify counter and that the queue depth
	// stayed at 9.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 0, SoftThreshold: 0.9})
	for range 9 {
		if err := p.Submit(healthyBatch()); err != nil {
			t.Fatalf("priming submit failed: %v", err)
		}
	}
	// Now at exactly 9/10 = 0.9 fullness — soft backpressure engages.
	if err := p.Submit(healthyBatch()); err != nil {
		t.Fatalf("dropped submit returned err %v, want nil (silent drop)", err)
	}
	stats := p.Stats()
	if stats.Enqueued != 9 {
		t.Fatalf("enqueued=%d after soft-drop, want 9 (drop should not enqueue)", stats.Enqueued)
	}
	if stats.DroppedHealthy != 1 {
		t.Fatalf("DroppedHealthy=%d, want 1", stats.DroppedHealthy)
	}
}

func TestPipeline_PriorityBatchesBypassSoftBackpressure(t *testing.T) {
	// Same setup as drop test, but submit an error-flagged batch — must
	// enqueue, not drop, because errors are diagnostic-critical.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 0, SoftThreshold: 0.9})
	for range 9 {
		_ = p.Submit(healthyBatch())
	}
	if err := p.Submit(errorBatch()); err != nil {
		t.Fatalf("priority submit returned %v, want nil (errors must pass soft backpressure)", err)
	}
	stats := p.Stats()
	if stats.Enqueued != 10 {
		t.Fatalf("priority batch enqueued=%d, want 10", stats.Enqueued)
	}
	if stats.DroppedHealthy != 0 {
		t.Fatalf("DroppedHealthy=%d, want 0 (priority must not be dropped)", stats.DroppedHealthy)
	}
}

func TestPipeline_RejectsAtHardCapacity(t *testing.T) {
	// Fill the queue to 100% with priority traffic (bypasses soft drop),
	// then submit one more priority batch — must return ErrQueueFull.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 5, Workers: 0, SoftThreshold: 0.9})
	for range 5 {
		if err := p.Submit(errorBatch()); err != nil {
			t.Fatalf("priming priority submit failed: %v", err)
		}
	}
	err := p.Submit(errorBatch())
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("hard-capacity submit returned %v, want ErrQueueFull", err)
	}
	stats := p.Stats()
	if stats.RejectedFull != 1 {
		t.Fatalf("RejectedFull=%d, want 1", stats.RejectedFull)
	}
}

// ===== Worker / processing semantics =====

func TestPipeline_PreservesInsertionOrder(t *testing.T) {
	// Trace → Span → Log ordering must hold across processing because of
	// the FK constraint enforced by the synchronous path.
	w := &fakeWriter{}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 4, Workers: 1})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	t.Cleanup(p.Stop)

	if err := p.Submit(healthyBatch()); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return p.Stats().Processed == 1 }) {
		t.Fatalf("worker did not process batch within deadline")
	}

	got := w.snapshotOrder()
	want := []string{"traces", "spans", "logs"}
	if len(got) != len(want) {
		t.Fatalf("call order length=%d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call order[%d]=%q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestPipeline_CallbacksFireAfterPersistence(t *testing.T) {
	// Callbacks must run AFTER the corresponding BatchCreate* succeeds.
	// On failure, callbacks must NOT run for that signal type.
	w := &fakeWriter{}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 2, Workers: 1})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	t.Cleanup(p.Stop)

	var spanHits, logHits atomic.Int64
	b := healthyBatch()
	b.SpanCallback = func(_ storage.Span) { spanHits.Add(1) }
	b.LogCallback = func(_ storage.Log) { logHits.Add(1) }

	if err := p.Submit(b); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return spanHits.Load() == 1 && logHits.Load() == 1 }) {
		t.Fatalf("callbacks did not fire (span=%d log=%d, want 1/1)", spanHits.Load(), logHits.Load())
	}
}

func TestPipeline_FailedSpansSkipsLogs(t *testing.T) {
	// When BatchCreateSpans fails, BatchCreateLogs must NOT run for that
	// batch — preserves the invariant that orphan logs aren't persisted
	// without their span. Mirrors the synchronous path's behavior of
	// returning the span error before log insert.
	w := &fakeWriter{spanErr: errors.New("span db down")}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 2, Workers: 1})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	t.Cleanup(p.Stop)

	if err := p.Submit(healthyBatch()); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return p.Stats().ProcessFailures > 0 }) {
		t.Fatalf("expected ProcessFailures > 0, got %d", p.Stats().ProcessFailures)
	}
	calls := w.snapshotOrder()
	for _, c := range calls {
		if c == "logs" {
			t.Fatalf("BatchCreateLogs ran after spans failed — order=%v", calls)
		}
	}
}

func TestPipeline_FailedTracesAbortsBatch(t *testing.T) {
	// Trace failures roll the entire batch back — atomic batches are the
	// fix for orphan FK rows when a worker crashes between BatchCreate*
	// calls. Spans and logs must NOT be persisted when the trace insert
	// fails. Counterpart of TestPipeline_FailedSpansSkipsLogs.
	w := &fakeWriter{traceErr: errors.New("transient")}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 2, Workers: 1})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	t.Cleanup(p.Stop)

	if err := p.Submit(healthyBatch()); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return p.Stats().ProcessFailures > 0 }) {
		t.Fatalf("expected ProcessFailures > 0, got %d", p.Stats().ProcessFailures)
	}
	calls := w.snapshotOrder()
	for _, c := range calls {
		if c == "spans" || c == "logs" {
			t.Fatalf("spans/logs ran after trace failure — order=%v", calls)
		}
	}
}

func TestPipeline_DrainsOnStop(t *testing.T) {
	// Stop() must process remaining buffered batches before returning so
	// graceful shutdown doesn't lose in-flight ingest.
	w := &fakeWriter{}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 50, Workers: 2})
	for range 20 {
		_ = p.Submit(healthyBatch())
	}
	// Start AFTER submitting so the queue is pre-loaded — exercises the
	// drain path in worker().
	ctx := context.Background()
	p.Start(ctx)
	p.Stop()

	if got := p.Stats().Processed; got != 20 {
		t.Fatalf("after Stop: processed=%d, want 20 (drain path failed)", got)
	}
}

func TestPipeline_StopIsIdempotent(t *testing.T) {
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 4, Workers: 1})
	p.Start(context.Background())
	// First Stop drains. Second must not panic on closed channel.
	p.Stop()
	p.Stop()
}

func TestPipeline_ConcurrentSubmit(t *testing.T) {
	// 100 goroutines × 50 submits = 5000 submits. Pipeline must not
	// race; total of (enqueued + dropped + rejected) must equal 5000.
	w := &fakeWriter{}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 1024, Workers: 4})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	t.Cleanup(p.Stop)

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			for range 50 {
				_ = p.Submit(healthyBatch())
			}
		})
	}
	wg.Wait()

	// Wait for queue to drain so Processed catches up with Enqueued.
	if !waitFor(t, 5*time.Second, func() bool {
		s := p.Stats()
		return s.Processed == s.Enqueued && s.QueueDepth == 0
	}) {
		t.Fatalf("queue did not drain — stats=%+v", p.Stats())
	}

	stats := p.Stats()
	total := stats.Enqueued + stats.DroppedHealthy + stats.RejectedFull
	if total != 5000 {
		t.Fatalf("submits accounted=%d, want 5000 — race lost batches; stats=%+v", total, stats)
	}
}

func TestPipeline_DefaultsApplied(t *testing.T) {
	// Zero-value config must fall back to DefaultPipelineConfig().
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{})
	d := DefaultPipelineConfig()
	if p.cfg.Capacity != d.Capacity {
		t.Errorf("Capacity default not applied: got %d want %d", p.cfg.Capacity, d.Capacity)
	}
	if p.cfg.Workers != d.Workers {
		t.Errorf("Workers default not applied: got %d want %d", p.cfg.Workers, d.Workers)
	}
	if p.cfg.SoftThreshold != d.SoftThreshold {
		t.Errorf("SoftThreshold default not applied: got %v want %v", p.cfg.SoftThreshold, d.SoftThreshold)
	}
}

func TestPipeline_PerTenantCap_DropsExcessHealthy(t *testing.T) {
	// With Workers=0 and Capacity=10, queue absorbs everything; the per-
	// tenant cap kicks in independently of soft-backpressure. Tenant A is
	// capped at 3; the 4th healthy submission for A must be dropped while
	// tenant B's submissions land normally.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 0, SoftThreshold: 0.9})
	p.SetPerTenantCap(3)

	mkBatch := func(tenant string) *Batch {
		b := healthyBatch()
		b.Tenant = tenant
		return b
	}

	for range 3 {
		if err := p.Submit(mkBatch("a")); err != nil {
			t.Fatalf("submit under cap: %v", err)
		}
	}
	if err := p.Submit(mkBatch("a")); err != nil {
		t.Fatalf("4th submit returned err %v, want nil (silent drop)", err)
	}
	if err := p.Submit(mkBatch("b")); err != nil {
		t.Fatalf("tenant b under its cap should not be affected: %v", err)
	}

	stats := p.Stats()
	if stats.Enqueued != 4 { // 3 from a + 1 from b
		t.Fatalf("Enqueued=%d, want 4", stats.Enqueued)
	}
	if got := p.TenantDropped(); got != 1 {
		t.Fatalf("TenantDropped=%d, want 1", got)
	}
	if stats.DroppedHealthy != 0 {
		t.Fatalf("DroppedHealthy=%d, want 0 (tenant cap is a separate counter)", stats.DroppedHealthy)
	}
}

func TestPipeline_PerTenantCap_PriorityBypasses(t *testing.T) {
	// Errors and slow traces must always land — they bypass the per-tenant
	// cap the same way they bypass soft-backpressure.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 0, SoftThreshold: 0.9})
	p.SetPerTenantCap(2)

	mkErr := func(tenant string) *Batch {
		b := errorBatch()
		b.Tenant = tenant
		return b
	}

	for range 5 {
		if err := p.Submit(mkErr("noisy")); err != nil {
			t.Fatalf("priority submit blocked by tenant cap: %v", err)
		}
	}
	if got := p.TenantDropped(); got != 0 {
		t.Fatalf("TenantDropped=%d, want 0 (priority bypasses cap)", got)
	}
	if got := p.Stats().Enqueued; got != 5 {
		t.Fatalf("Enqueued=%d, want 5", got)
	}
}

func TestPipeline_PerTenantCap_ReleasedAfterProcess(t *testing.T) {
	// Once a worker drains and processes a batch, the tenant slot is
	// released so subsequent submissions from the same tenant are
	// accepted. Without release, the cap would be a one-shot per-tenant
	// quota for the lifetime of the process.
	w := &fakeWriter{}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 10, Workers: 1, SoftThreshold: 0.9})
	p.SetPerTenantCap(1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	t.Cleanup(p.Stop)

	mk := func() *Batch {
		b := healthyBatch()
		b.Tenant = "single"
		return b
	}

	// First batch fills the cap.
	if err := p.Submit(mk()); err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	// Wait for the worker to drain it (and release the slot). 5s tolerates
	// the race detector's overhead on slow CI runners — the test passes
	// locally in milliseconds.
	if !waitFor(t, 5*time.Second, func() bool { return p.Stats().Processed == 1 }) {
		t.Fatalf("worker did not process first batch")
	}
	// Second batch must succeed because the slot was released.
	if err := p.Submit(mk()); err != nil {
		t.Fatalf("submit 2 after release: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return p.Stats().Processed == 2 }) {
		t.Fatalf("worker did not process second batch")
	}
	if got := p.TenantDropped(); got != 0 {
		t.Fatalf("TenantDropped=%d, want 0 (no drops when slot is released between submits)", got)
	}
}

func TestPipeline_HardCapacityEvenForPriority(t *testing.T) {
	// Above hard capacity, priority batches are still rejected. The
	// caller is responsible for translating into RESOURCE_EXHAUSTED so
	// the OTLP client retries; better than silently losing errors.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 2, Workers: 0, SoftThreshold: 0.9})
	_ = p.Submit(errorBatch())
	_ = p.Submit(errorBatch())
	err := p.Submit(errorBatch())
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("hard cap with priority: got %v, want ErrQueueFull", err)
	}
}

func TestPipeline_PanicInCallbackRecovered(t *testing.T) {
	// A panicking callback must not kill the worker; processFailures
	// goes up but other batches still process.
	w := &fakeWriter{}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 4, Workers: 1})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	t.Cleanup(p.Stop)

	bad := healthyBatch()
	bad.SpanCallback = func(_ storage.Span) { panic("boom") }
	good := healthyBatch()

	if err := p.Submit(bad); err != nil {
		t.Fatalf("submit bad: %v", err)
	}
	if err := p.Submit(good); err != nil {
		t.Fatalf("submit good: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return p.Stats().Processed >= 2 }) {
		t.Fatalf("worker did not survive callback panic — Processed=%d", p.Stats().Processed)
	}
	if p.Stats().ProcessFailures == 0 {
		t.Errorf("expected ProcessFailures > 0 after callback panic")
	}
}
