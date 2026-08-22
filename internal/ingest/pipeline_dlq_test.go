package ingest

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/RandomCodeSpace/otelcontext/internal/queue"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

// recordingWriter is the replay target: it accumulates whatever
// BatchCreateAll receives so a test can assert the DLQ round-trip restored
// the complete batch.
type recordingWriter struct {
	mu     sync.Mutex
	traces []storage.Trace
	spans  []storage.Span
	logs   []storage.Log
}

func (r *recordingWriter) apply(t []storage.Trace, s []storage.Span, l []storage.Log) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.traces = append(r.traces, t...)
	r.spans = append(r.spans, s...)
	r.logs = append(r.logs, l...)
	return nil
}

func (r *recordingWriter) counts() (int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.traces), len(r.spans), len(r.logs)
}

// replayEnvelope mirrors main.go's DLQ replay handler for the batch envelope
// type. Kept in sync by construction: it decodes the same exported types the
// pipeline writes.
func replayEnvelope(data []byte, sink *recordingWriter) error {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Type != DLQBatchType {
		return errors.New("unexpected DLQ envelope type " + envelope.Type)
	}
	var payload DLQBatchPayload
	if err := json.Unmarshal(envelope.Data, &payload); err != nil {
		return err
	}
	return sink.apply(payload.Traces, payload.Spans, payload.Logs)
}

// TestPipeline_DBFailure_LandsInDLQAndReplays is the acceptance test for
// #194 finding 11: a forced BatchCreateAll failure must put the COMPLETE
// batch on the DLQ, the replay worker must restore every row, and none of
// the silent-drop counters may move.
func TestPipeline_DBFailure_LandsInDLQAndReplays(t *testing.T) {
	sink := &recordingWriter{}
	dlq, err := queue.NewDLQWithLimits(t.TempDir(), 25*time.Millisecond,
		func(data []byte) error { return replayEnvelope(data, sink) }, 0, 0, 0)
	if err != nil {
		t.Fatalf("NewDLQWithLimits: %v", err)
	}
	t.Cleanup(dlq.Stop)

	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_ingest_pipeline_dlq_total"},
		[]string{"signal", "result"})
	m := &telemetry.Metrics{IngestPipelineDLQTotal: c}

	w := &fakeWriter{traceErr: errors.New("db down")}
	p := NewPipeline(w, m, PipelineConfig{Capacity: 8, Workers: 1})
	p.SetDLQ(dlq)
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	b := healthyBatch() // 1 trace + 1 span + 1 log
	if _, err := p.Submit(b); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if !waitFor(t, 20*time.Second, func() bool { return p.Stats().DLQEnqueued == 1 }) {
		t.Fatalf("batch was not handed to the DLQ: %+v", p.Stats())
	}

	// Replay restores every row of the batch.
	if !waitFor(t, 20*time.Second, func() bool {
		tr, sp, lg := sink.counts()
		return tr == 1 && sp == 1 && lg == 1
	}) {
		tr, sp, lg := sink.counts()
		t.Fatalf("replay restored traces=%d spans=%d logs=%d, want 1/1/1", tr, sp, lg)
	}

	stats := p.Stats()
	if stats.ProcessFailures != 1 {
		t.Errorf("ProcessFailures=%d, want 1", stats.ProcessFailures)
	}
	if stats.DLQFailed != 0 {
		t.Errorf("DLQFailed=%d, want 0", stats.DLQFailed)
	}
	// No silent drop counter may increment — the batch was preserved, not lost.
	if stats.DroppedHealthy != 0 || stats.RejectedFull != 0 || stats.RejectedBytes != 0 {
		t.Errorf("silent drop counters moved: DroppedHealthy=%d RejectedFull=%d RejectedBytes=%d",
			stats.DroppedHealthy, stats.RejectedFull, stats.RejectedBytes)
	}
	if got := p.TenantDropped(); got != 0 {
		t.Errorf("TenantDropped=%d, want 0", got)
	}
	if got := testutil.ToFloat64(c.WithLabelValues("traces", "enqueued")); got != 1 {
		t.Errorf("dlq_total{signal=traces,result=enqueued}=%v, want 1", got)
	}
}

// TestPipeline_DBFailure_NoDLQKeepsLegacyDrop pins the opt-out: with no sink
// wired the batch is still dropped (pre-#194 behaviour) but the loss is
// counted rather than silent.
func TestPipeline_DBFailure_NoDLQKeepsLegacyDrop(t *testing.T) {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_ingest_pipeline_dlq_nosink_total"},
		[]string{"signal", "result"})
	m := &telemetry.Metrics{IngestPipelineDLQTotal: c}

	p := NewPipeline(&fakeWriter{traceErr: errors.New("db down")}, m,
		PipelineConfig{Capacity: 8, Workers: 1})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	if _, err := p.Submit(healthyBatch()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !waitFor(t, 20*time.Second, func() bool { return p.Stats().ProcessFailures == 1 }) {
		t.Fatalf("batch was not processed: %+v", p.Stats())
	}
	if got := p.Stats().DLQEnqueued; got != 0 {
		t.Errorf("DLQEnqueued=%d, want 0 with no sink", got)
	}
	if !waitFor(t, 20*time.Second, func() bool {
		return testutil.ToFloat64(c.WithLabelValues("traces", "no_sink")) == 1
	}) {
		t.Errorf("dlq_total{result=no_sink}=%v, want 1",
			testutil.ToFloat64(c.WithLabelValues("traces", "no_sink")))
	}
}

// errSink is a BatchSink that always refuses the write.
type errSink struct{}

func (errSink) Enqueue(any) error { return errors.New("disk full") }

// TestPipeline_DBFailure_DLQEnqueueFailureCounted proves an unusable DLQ is
// loud: the batch is lost, but result=enqueue_failed says so.
func TestPipeline_DBFailure_DLQEnqueueFailureCounted(t *testing.T) {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_ingest_pipeline_dlq_err_total"},
		[]string{"signal", "result"})
	m := &telemetry.Metrics{IngestPipelineDLQTotal: c}

	p := NewPipeline(&fakeWriter{traceErr: errors.New("db down")}, m,
		PipelineConfig{Capacity: 8, Workers: 1})
	p.SetDLQ(errSink{})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	if _, err := p.Submit(healthyBatch()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !waitFor(t, 20*time.Second, func() bool { return p.Stats().DLQFailed == 1 }) {
		t.Fatalf("DLQFailed=%d, want 1", p.Stats().DLQFailed)
	}
	if got := testutil.ToFloat64(c.WithLabelValues("traces", "enqueue_failed")); got != 1 {
		t.Errorf("dlq_total{result=enqueue_failed}=%v, want 1", got)
	}
}

// TestPipeline_DLQEnvelope_CarriesStoreFilteredLogsOnly proves the envelope
// records what the transaction actually attempted: logs dropped by the
// store-severity gate must not reappear on replay.
func TestPipeline_DLQEnvelope_CarriesStoreFilteredLogsOnly(t *testing.T) {
	var got DLQBatchEnvelope
	captured := make(chan struct{}, 1)
	sink := sinkFunc(func(batch any) error {
		env, ok := batch.(DLQBatchEnvelope)
		if !ok {
			t.Errorf("sink received %T, want DLQBatchEnvelope", batch)
			return nil
		}
		got = env
		captured <- struct{}{}
		return nil
	})

	p := NewPipeline(&fakeWriter{traceErr: errors.New("db down")}, nil,
		PipelineConfig{Capacity: 8, Workers: 1})
	p.SetDLQ(sink)
	p.SetStoreMinSeverity(parseSeverity("WARN"))
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	b := healthyBatch()
	b.Tenant = "acme"
	b.Logs = []storage.Log{
		{Body: "debug-row", Severity: "DEBUG"},
		{Body: "error-row", Severity: "ERROR"},
	}
	if _, err := p.Submit(b); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	select {
	case <-captured:
	case <-time.After(20 * time.Second):
		t.Fatal("DLQ sink never saw the failed batch")
	}

	if got.Type != DLQBatchType {
		t.Errorf("envelope type=%q, want %q", got.Type, DLQBatchType)
	}
	if got.Data.Tenant != "acme" {
		t.Errorf("envelope tenant=%q, want acme", got.Data.Tenant)
	}
	if len(got.Data.Logs) != 1 || got.Data.Logs[0].Body != "error-row" {
		t.Errorf("envelope logs=%+v, want only the ERROR row", got.Data.Logs)
	}
	if len(got.Data.Traces) != 1 || len(got.Data.Spans) != 1 {
		t.Errorf("envelope traces=%d spans=%d, want 1/1", len(got.Data.Traces), len(got.Data.Spans))
	}
}

// sinkFunc adapts a function to BatchSink.
type sinkFunc func(batch any) error

func (f sinkFunc) Enqueue(batch any) error { return f(batch) }
