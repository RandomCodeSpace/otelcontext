package ingest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
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
	// Sync on the assertion target — the per-signal call sequence — rather
	// than Stats().Processed, which can bump between BatchCreate calls under
	// the race detector and trip the length check on a partial slice.
	if !waitFor(t, 5*time.Second, func() bool { return len(w.snapshotOrder()) >= 3 }) {
		t.Fatalf("worker did not record 3 calls within deadline (got %v)", w.snapshotOrder())
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
	if !waitFor(t, 5*time.Second, func() bool { return spanHits.Load() == 1 && logHits.Load() == 1 }) {
		t.Fatalf("callbacks did not fire (span=%d log=%d, want 1/1)", spanHits.Load(), logHits.Load())
	}
}

// runFailureSkipsCheck wires up a 1-worker pipeline with the configured
// fakeWriter, submits a healthy batch, waits for the failure to surface,
// then asserts that none of the forbidden BatchCreate* calls fired.
// Shared by the trace-fails and span-fails skip tests so the boilerplate
// (pipeline lifecycle + waitFor) lives in one place.
func runFailureSkipsCheck(t *testing.T, w *fakeWriter, forbidden ...string) {
	t.Helper()
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 2, Workers: 1})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	t.Cleanup(p.Stop)

	if err := p.Submit(healthyBatch()); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return p.Stats().ProcessFailures > 0 }) {
		t.Fatalf("expected ProcessFailures > 0, got %d", p.Stats().ProcessFailures)
	}
	calls := w.snapshotOrder()
	for _, c := range calls {
		for _, f := range forbidden {
			if c == f {
				t.Fatalf("%s ran after upstream failure — order=%v", f, calls)
			}
		}
	}
}

func TestPipeline_FailedSpansSkipsLogs(t *testing.T) {
	// When BatchCreateSpans fails, BatchCreateLogs must NOT run for that
	// batch — preserves the invariant that orphan logs aren't persisted
	// without their span. Mirrors the synchronous path's behavior of
	// returning the span error before log insert.
	runFailureSkipsCheck(t, &fakeWriter{spanErr: errors.New("span db down")}, "logs")
}

func TestPipeline_FailedTracesAbortsBatch(t *testing.T) {
	// Trace failures roll the entire batch back — atomic batches are the
	// fix for orphan FK rows when a worker crashes between BatchCreate*
	// calls. Spans and logs must NOT be persisted when the trace insert
	// fails. Counterpart of TestPipeline_FailedSpansSkipsLogs.
	runFailureSkipsCheck(t, &fakeWriter{traceErr: errors.New("transient")}, "spans", "logs")
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
	if p.cfg.MaxBytes != d.MaxBytes {
		t.Errorf("MaxBytes default not applied: got %d want %d", p.cfg.MaxBytes, d.MaxBytes)
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
	if !waitFor(t, 5*time.Second, func() bool { return p.Stats().Processed >= 2 }) {
		t.Fatalf("worker did not survive callback panic — Processed=%d", p.Stats().Processed)
	}
	if p.Stats().ProcessFailures == 0 {
		t.Errorf("expected ProcessFailures > 0 after callback panic")
	}
}

// TestPipeline_StoreMinSeverity_DropsBelowThresholdFromPersist verifies that
// when SetStoreMinSeverity is configured, logs below the threshold are
// dropped from BatchCreateAll — but the LogCallback still fires for them
// so in-memory enrichers (vectordb, GraphRAG) keep seeing every log that
// passed IngestMinSeverity at the receiver.
func TestPipeline_StoreMinSeverity_DropsBelowThresholdFromPersist(t *testing.T) {
	t.Parallel()
	w := &fakeWriter{}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 10, Workers: 1, SoftThreshold: 0.9})
	// Threshold = WARN (rank 30); INFO (20) is below, ERROR (40) is above.
	p.SetStoreMinSeverity(ParseSeverity("WARN"))
	p.Start(context.Background())
	defer p.Stop()

	var callbackSeen []string
	var cbMu sync.Mutex
	cb := func(l storage.Log) {
		cbMu.Lock()
		defer cbMu.Unlock()
		callbackSeen = append(callbackSeen, l.Severity)
	}

	b := &Batch{
		Type:   SignalLogs,
		Tenant: "t1",
		Logs: []storage.Log{
			{Body: "info-row", Severity: "INFO"},
			{Body: "warn-row", Severity: "WARN"},
			{Body: "err-row", Severity: "ERROR"},
		},
		LogCallback: cb,
	}
	if err := p.Submit(b); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if !waitFor(t, 5*time.Second, func() bool { return p.Stats().Processed >= 1 }) {
		t.Fatalf("batch never processed")
	}

	// Persist: only WARN + ERROR should reach the writer.
	w.mu.Lock()
	persistedCount := 0
	persistedSeverities := []string{}
	for _, call := range w.logsCalls {
		for _, l := range call {
			persistedCount++
			persistedSeverities = append(persistedSeverities, l.Severity)
		}
	}
	w.mu.Unlock()
	if persistedCount != 2 {
		t.Fatalf("expected 2 logs persisted (WARN+ERROR), got %d: %v", persistedCount, persistedSeverities)
	}
	for _, sev := range persistedSeverities {
		if sev == "INFO" {
			t.Errorf("INFO log was persisted but should have been gated by store-min-severity")
		}
	}

	// Callback: must fire for ALL THREE logs (INFO included), since the
	// in-memory enrichment path is independent of the persist gate.
	cbMu.Lock()
	defer cbMu.Unlock()
	if len(callbackSeen) != 3 {
		t.Fatalf("expected LogCallback to fire 3 times (incl. gated INFO), got %d: %v", len(callbackSeen), callbackSeen)
	}
	infoCb := false
	for _, sev := range callbackSeen {
		if sev == "INFO" {
			infoCb = true
		}
	}
	if !infoCb {
		t.Errorf("INFO log did not reach LogCallback — in-memory enrichment path broken: %v", callbackSeen)
	}

	// Stats: storeFiltered should report exactly 1 (the INFO drop).
	if got := p.Stats().StoreFiltered; got != 1 {
		t.Errorf("Stats().StoreFiltered = %d, want 1", got)
	}
}

// TestPipeline_StoreMinSeverity_Disabled_PersistsAllLogs verifies the legacy
// path: when SetStoreMinSeverity is NOT called (or set to 0), every log in a
// Batch is persisted regardless of severity.
func TestPipeline_StoreMinSeverity_Disabled_PersistsAllLogs(t *testing.T) {
	t.Parallel()
	w := &fakeWriter{}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 10, Workers: 1, SoftThreshold: 0.9})
	// No SetStoreMinSeverity call → gate disabled.
	p.Start(context.Background())
	defer p.Stop()

	b := &Batch{
		Type:   SignalLogs,
		Tenant: "t1",
		Logs: []storage.Log{
			{Body: "info-row", Severity: "INFO"},
			{Body: "debug-row", Severity: "DEBUG"},
			{Body: "err-row", Severity: "ERROR"},
		},
	}
	if err := p.Submit(b); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return p.Stats().Processed >= 1 }) {
		t.Fatalf("batch never processed")
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	total := 0
	for _, call := range w.logsCalls {
		total += len(call)
	}
	if total != 3 {
		t.Fatalf("expected all 3 logs persisted with gate disabled, got %d", total)
	}
	if got := p.Stats().StoreFiltered; got != 0 {
		t.Errorf("Stats().StoreFiltered = %d, want 0 with gate disabled", got)
	}
}

// ===== Byte-bounded queue (P1.2) =====

// fatLogBatch builds a single-log batch whose Body is `bodyLen` bytes —
// the byte-cap tests size their payloads through this.
func fatLogBatch(bodyLen int) *Batch {
	return &Batch{
		Type:   SignalLogs,
		Tenant: "t1",
		Logs:   []storage.Log{{Body: strings.Repeat("x", bodyLen)}},
	}
}

func TestBatch_ApproxBytes_Formula(t *testing.T) {
	// Pins the per-record estimate: fixed struct overhead + the lengths of
	// the dominant string payloads. A drive-by change to the formula must
	// consciously update this test.
	b := &Batch{
		Traces: []storage.Trace{{TraceID: "abcd", ServiceName: "svc", Status: "OK", Operation: "op"}},
		Spans: []storage.Span{{
			OperationName:  "GET /x",
			AttributesJSON: storage.CompressedText(`{"k":"v"}`),
			ServiceName:    "svc",
			Status:         "OK",
		}},
		Logs: []storage.Log{{
			Body:           "hello",
			AttributesJSON: storage.CompressedText("{}"),
			AIInsight:      storage.CompressedText("ai"),
			ServiceName:    "svc",
			Severity:       "INFO",
		}},
	}
	wantTrace := int64(128 + 4 + 3 + 2 + 2)   // TraceID + ServiceName + Status + Operation
	wantSpan := int64(256 + 6 + 9 + 3 + 2)    // OperationName + AttributesJSON + ServiceName + Status
	wantLog := int64(192 + 5 + 2 + 2 + 3 + 4) // Body + AttributesJSON + AIInsight + ServiceName + Severity
	if got, want := b.approxBytes(), wantTrace+wantSpan+wantLog; got != want {
		t.Fatalf("approxBytes() = %d, want %d", got, want)
	}
}

func TestBatch_ApproxBytes_GrowsWithPayload(t *testing.T) {
	// The estimate must scale with the variable-length payloads — Body and
	// AttributesJSON are what actually make a batch fat.
	small := fatLogBatch(1)
	big := fatLogBatch(1001)
	if diff := big.approxBytes() - small.approxBytes(); diff != 1000 {
		t.Errorf("Body growth: approxBytes diff = %d, want 1000", diff)
	}

	s1 := &Batch{Spans: []storage.Span{{SpanID: "s"}}}
	s2 := &Batch{Spans: []storage.Span{{SpanID: "s", AttributesJSON: storage.CompressedText(strings.Repeat("a", 500))}}}
	if diff := s2.approxBytes() - s1.approxBytes(); diff != 500 {
		t.Errorf("AttributesJSON growth: approxBytes diff = %d, want 500", diff)
	}
}

func TestPipeline_MaxBytes_DefaultAndClamp(t *testing.T) {
	// MaxBytes <= 0 falls back to the 512MB default.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 0})
	if got := p.Stats().MaxBytes; got != 512<<20 {
		t.Errorf("MaxBytes default: got %d, want %d", got, int64(512<<20))
	}
	// A sub-1MB cap would reject every gRPC batch (GRPC_MAX_RECV_MB default
	// is 16) — clamp up to the 1MB floor instead.
	p2 := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 0, MaxBytes: 1000})
	if got := p2.Stats().MaxBytes; got != 1<<20 {
		t.Errorf("sub-1MB clamp: got %d, want %d", got, int64(1<<20))
	}
	// Exactly the floor passes through unmodified.
	p3 := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 0, MaxBytes: 1 << 20})
	if got := p3.Stats().MaxBytes; got != 1<<20 {
		t.Errorf("1MB floor passthrough: got %d, want %d", got, int64(1<<20))
	}
}

func TestPipeline_ByteCap_RejectsEvenPriority(t *testing.T) {
	// Item count is nowhere near Capacity, yet the second fat batch must be
	// rejected on bytes — and priority gives no exemption: a 429 is
	// recoverable by the OTLP client's retry loop, an OOM kill is not.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 100, Workers: 0, MaxBytes: 1 << 20})

	first := fatLogBatch(700 * 1024)
	first.HasError = true
	if err := p.Submit(first); err != nil {
		t.Fatalf("first fat batch under the cap rejected: %v", err)
	}
	firstSize := first.approxBytes()

	second := fatLogBatch(700 * 1024)
	second.HasError = true
	if err := p.Submit(second); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("over-cap priority submit returned %v, want ErrQueueFull", err)
	}

	stats := p.Stats()
	if stats.RejectedBytes != 1 {
		t.Errorf("RejectedBytes = %d, want 1", stats.RejectedBytes)
	}
	if stats.RejectedFull != 0 {
		t.Errorf("RejectedFull = %d, want 0 (byte rejection is a separate counter)", stats.RejectedFull)
	}
	if stats.QueueBytes != firstSize {
		t.Errorf("QueueBytes = %d, want %d (rejected batch must not stay reserved)", stats.QueueBytes, firstSize)
	}
}

func TestPipeline_ByteCap_SingleOversizedBatchRejected(t *testing.T) {
	// A lone batch bigger than the whole cap is rejected outright and
	// leaves no residual reservation behind.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 100, Workers: 0, MaxBytes: 1 << 20})
	b := fatLogBatch(2 << 20)
	b.HasError = true
	if err := p.Submit(b); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("oversized submit returned %v, want ErrQueueFull", err)
	}
	if got := p.Stats().QueueBytes; got != 0 {
		t.Errorf("QueueBytes = %d after rejection, want 0", got)
	}
}

func TestPipeline_ByteCap_ReleasesTenantSlotOnReject(t *testing.T) {
	// A healthy batch reserves its tenant slot before the byte check; a
	// byte rejection must hand that slot back, or the tenant cap turns into
	// a slow leak. With cap 1, a leaked slot would make the follow-up
	// submission a tenant_backpressure drop.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 0, MaxBytes: 1 << 20})
	p.SetPerTenantCap(1)

	fat := fatLogBatch(2 << 20) // healthy → reserves the tenant slot first
	if err := p.Submit(fat); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("oversized submit returned %v, want ErrQueueFull", err)
	}
	if err := p.Submit(healthyBatch()); err != nil {
		t.Fatalf("follow-up submit after byte rejection: %v", err)
	}
	if got := p.TenantDropped(); got != 0 {
		t.Errorf("TenantDropped = %d, want 0 (slot leaked by byte rejection)", got)
	}
	if got := p.Stats().Enqueued; got != 1 {
		t.Errorf("Enqueued = %d, want 1", got)
	}
}

func TestPipeline_ChannelFull_ReleasesBytes(t *testing.T) {
	// The hard-capacity default branch must undo the byte reservation the
	// same way it undoes the tenant slot.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 1, Workers: 0})
	first := errorBatch()
	if err := p.Submit(first); err != nil {
		t.Fatalf("priming submit: %v", err)
	}
	if err := p.Submit(errorBatch()); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("channel-full submit returned %v, want ErrQueueFull", err)
	}
	if got, want := p.Stats().QueueBytes, first.approxBytes(); got != want {
		t.Errorf("QueueBytes = %d, want %d (channel-full path leaked its reservation)", got, want)
	}
}

func TestPipeline_SoftDropViaByteFullness(t *testing.T) {
	// Item fullness is ~0.1% but bytes are at ~93% of the cap — the soft
	// check must fire on max(itemFullness, byteFullness) and drop the
	// healthy batch.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 1000, Workers: 0, SoftThreshold: 0.9, MaxBytes: 1 << 20})
	fat := fatLogBatch(950 * 1024) // priority → bypasses the soft check itself
	fat.HasError = true
	if err := p.Submit(fat); err != nil {
		t.Fatalf("priming fat priority submit: %v", err)
	}

	if err := p.Submit(healthyBatch()); err != nil {
		t.Fatalf("soft-dropped submit returned %v, want nil (silent drop)", err)
	}
	stats := p.Stats()
	if stats.DroppedHealthy != 1 {
		t.Errorf("DroppedHealthy = %d, want 1 (byteFullness should trip the soft check)", stats.DroppedHealthy)
	}
	if stats.Enqueued != 1 {
		t.Errorf("Enqueued = %d, want 1 (only the priming batch)", stats.Enqueued)
	}
}

func TestPipeline_ByteAccounting_ReservedWhileQueued(t *testing.T) {
	// With no workers draining, QueueBytes must equal the sum of the
	// enqueued batches' estimates.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 10, Workers: 0})
	b1, b2 := healthyBatch(), fatLogBatch(4096)
	if err := p.Submit(b1); err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	if err := p.Submit(b2); err != nil {
		t.Fatalf("submit 2: %v", err)
	}
	if got, want := p.Stats().QueueBytes, b1.approxBytes()+b2.approxBytes(); got != want {
		t.Errorf("QueueBytes = %d, want %d", got, want)
	}
}

func TestPipeline_ByteAccounting_ReturnsToZeroAfterProcess(t *testing.T) {
	// Every reservation taken at Submit must be released by process() —
	// including the panic path, or the counter ratchets up until the cap
	// rejects all traffic.
	w := &fakeWriter{}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 10, Workers: 1})
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
	if !waitFor(t, 5*time.Second, func() bool { return p.Stats().Processed >= 2 }) {
		t.Fatalf("worker did not process both batches — Processed=%d", p.Stats().Processed)
	}
	if !waitFor(t, 5*time.Second, func() bool { return p.Stats().QueueBytes == 0 }) {
		t.Fatalf("QueueBytes = %d after drain, want 0 (panic path leaked its reservation)", p.Stats().QueueBytes)
	}
}

func TestPipeline_QueueBytesGaugeTracksReservations(t *testing.T) {
	// The Prometheus gauge must follow the reservation lifecycle: up on
	// enqueue, back to zero after the worker drains. Built from a bare
	// prometheus.NewGauge (not telemetry.New()) so the default registry
	// isn't double-registered across tests.
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_ingest_pipeline_queue_bytes"})
	m := &telemetry.Metrics{IngestPipelineQueueBytes: g}
	p := NewPipeline(&fakeWriter{}, m, PipelineConfig{Capacity: 10, Workers: 1})

	b := healthyBatch()
	if err := p.Submit(b); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got, want := testutil.ToFloat64(g), float64(b.approxBytes()); got != want {
		t.Errorf("gauge after enqueue = %v, want %v", got, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	t.Cleanup(p.Stop)
	if !waitFor(t, 5*time.Second, func() bool { return testutil.ToFloat64(g) == 0 }) {
		t.Fatalf("gauge did not return to 0 after drain: %v", testutil.ToFloat64(g))
	}
}

func TestPipeline_ByteAccounting_ReleasedOnWriterFailure(t *testing.T) {
	// A failed BatchCreateAll drops the batch — its reservation must be
	// released all the same.
	w := &fakeWriter{traceErr: errors.New("db down")}
	p := NewPipeline(w, nil, PipelineConfig{Capacity: 4, Workers: 1})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	t.Cleanup(p.Stop)

	if err := p.Submit(healthyBatch()); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return p.Stats().ProcessFailures >= 1 }) {
		t.Fatalf("writer failure never surfaced")
	}
	if !waitFor(t, 5*time.Second, func() bool { return p.Stats().QueueBytes == 0 }) {
		t.Fatalf("QueueBytes = %d after failed process, want 0", p.Stats().QueueBytes)
	}
}
