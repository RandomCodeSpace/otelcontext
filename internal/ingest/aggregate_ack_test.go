package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// These tests pin the #196 frozen ACK contract, i.e. release blocker 1 of #194:
//
//	aggregate mode : the durable aggregate commit is the ACK, so exemplar
//	                 saturation degrades to DLQ-deferral or counted loss and
//	                 never makes the Export retryable.
//	shadow mode    : the legacy raw path is the ACK, so the shadow aggregate is
//	                 applied only after that path reaches a non-retry outcome.

// captureDLQ is a BatchSink that records what it was handed, or refuses with a
// configured error so the permanent-loss branch can be exercised.
type captureDLQ struct {
	mu         sync.Mutex
	envs       []DLQBatchEnvelope
	refuseWith error
}

func (c *captureDLQ) Enqueue(batch any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuseWith != nil {
		return c.refuseWith
	}
	env, ok := batch.(DLQBatchEnvelope)
	if !ok {
		return fmt.Errorf("unexpected DLQ payload %T", batch)
	}
	c.envs = append(c.envs, env)
	return nil
}

func (c *captureDLQ) snapshot() []DLQBatchEnvelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]DLQBatchEnvelope(nil), c.envs...)
}

// saturatedPipeline returns an UNSTARTED pipeline whose single queue slot is
// already occupied, so the next priority Submit hits hard capacity. Unstarted
// is what makes it deterministic: no worker can drain the slot mid-test.
func saturatedPipeline(t *testing.T, m *telemetry.Metrics, sink BatchSink) *Pipeline {
	t.Helper()
	p := NewPipeline(&fakeWriter{}, m, PipelineConfig{Capacity: 1, Workers: 1})
	if sink != nil {
		p.SetDLQ(sink)
	}
	if _, err := p.Submit(errorBatch()); err != nil {
		t.Fatalf("priming submit: %v", err)
	}
	if _, err := p.Submit(errorBatch()); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("pipeline is not saturated: %v", err)
	}
	return p
}

// drainOne removes one batch from the queue so a simulated client retry finds
// room. The pipeline is never started, so this is the only consumer.
func drainOne(t *testing.T, p *Pipeline) {
	t.Helper()
	select {
	case <-p.queue:
	default:
		t.Fatal("queue was empty; nothing to drain")
	}
}

// ackTestMetrics builds a Metrics carrying only the exemplar counters, under
// test-local names so the global promauto registry is untouched.
func ackTestMetrics(name string) *telemetry.Metrics {
	return &telemetry.Metrics{
		// Intake instruments Export touches unconditionally when metrics are
		// non-nil. Test-local so the global promauto registry stays clean.
		IngestionRate: prometheus.NewCounter(prometheus.CounterOpts{Name: "test_ingestion_rate_" + name}),
		GRPCBatchSize: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "test_grpc_batch_size_" + name}),
		ExemplarSubmitTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_exemplar_submit_total_" + name,
		}, []string{"signal", "outcome", "reason"}),
		ExemplarSubmitLostTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_exemplar_submit_lost_total_" + name,
		}, []string{"signal", "reason"}),
	}
}

// ackEngine builds an engine in the requested mode with the default (direct)
// applier, so Snapshot().Totals reflects exactly what was applied.
func ackEngine(t *testing.T, mode string, now time.Time) *aggregate.Engine {
	t.Helper()
	e, err := aggregate.NewEngine(aggregate.EngineConfig{Mode: mode, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewEngine(%s): %v", mode, err)
	}
	return e
}

// ackErrorSpanRequest builds an export of `n` ERROR spans on one service. ERROR
// makes the resulting batch priority, which is what bypasses soft backpressure
// and forces the HARD ErrQueueFull the contract is about.
func ackErrorSpanRequest(n int, ts time.Time) *coltracepb.ExportTraceServiceRequest {
	spans := make([]*tracepb.Span, 0, n)
	for i := range n {
		spans = append(spans, &tracepb.Span{
			TraceId:           []byte(fmt.Sprintf("%032d", i)),
			SpanId:            []byte(fmt.Sprintf("%016d", i)),
			Name:              "GET /checkout",
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: uint64(ts.UnixNano()), // #nosec G115 -- test timestamps are positive
			EndTimeUnixNano:   uint64(ts.Add(time.Millisecond).UnixNano()),
			Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "boom"},
		})
	}
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}}},
		}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}}
}

// ackErrorLogRequest builds an export of `n` ERROR log records on one service.
func ackErrorLogRequest(n int, ts time.Time) *collogspb.ExportLogsServiceRequest {
	records := make([]*logspb.LogRecord, 0, n)
	for i := range n {
		records = append(records, &logspb.LogRecord{
			TimeUnixNano: uint64(ts.UnixNano()), // #nosec G115 -- test timestamps are positive
			SeverityText: "ERROR",
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprintf("boom %d", i)}},
		})
	}
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}}},
		}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}},
	}}}
}

func counterAt(t *testing.T, v *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues%v: %v", labels, err)
	}
	return testutil.ToFloat64(c)
}

// exportSaturatedTraces wires a TraceServer over a saturated pipeline with
// sink, exports `spans` error spans in aggregate mode, asserts the Export
// succeeded with the aggregate counted exactly once, and returns the
// populated partial_success plus the metrics for further assertions.
func exportSaturatedTraces(t *testing.T, name string, sink BatchSink, spans int, now time.Time) (*coltracepb.ExportTraceServiceResponse, *telemetry.Metrics) {
	t.Helper()
	m := ackTestMetrics(name)
	p := saturatedPipeline(t, m, sink)
	eng := ackEngine(t, aggregate.ModeAggregate, now)
	srv := NewTraceServer(nil, m, aggTestConfig())
	srv.SetPipeline(p)
	srv.SetAggregateEngine(eng)

	resp, err := srv.Export(context.Background(), ackErrorSpanRequest(spans, now))
	if err != nil {
		t.Fatalf("Export must succeed once the aggregate commit landed: %v", err)
	}
	if count, _ := eng.Snapshot().Totals(aggregate.SignalTraceOp); count != uint64(spans) {
		t.Fatalf("aggregate count = %d, want %d (exactly one contribution)", count, spans)
	}
	if resp.GetPartialSuccess() == nil {
		t.Fatal("partial_success not populated")
	}
	return resp, m
}

// --- acceptance 1: aggregate mode, raw queue full -> deferred to DLQ ---------

func TestAggregateMode_QueueFull_DefersToDLQAndSucceeds(t *testing.T) {
	const spans = 3
	now := time.Now().UTC()
	sink := &captureDLQ{}
	resp, m := exportSaturatedTraces(t, "defer", sink, spans, now)

	// partial_success is a zero-rejected warning, not a rejection.
	ps := resp.GetPartialSuccess()
	if ps.RejectedSpans != 0 {
		t.Errorf("rejected_spans = %d, want 0 — the aggregate accepted every span", ps.RejectedSpans)
	}
	want := fmt.Sprintf("aggregate data accepted; %d selected raw exemplars were deferred to DLQ", spans)
	if ps.ErrorMessage != want {
		t.Errorf("error_message = %q, want %q", ps.ErrorMessage, want)
	}

	// The exemplar batch is in the DLQ, carrying the spans.
	envs := sink.snapshot()
	if len(envs) != 1 {
		t.Fatalf("DLQ received %d envelopes, want 1", len(envs))
	}
	if envs[0].Type != DLQBatchType {
		t.Errorf("DLQ envelope type = %q, want %q", envs[0].Type, DLQBatchType)
	}
	if got := len(envs[0].Data.Spans); got != spans {
		t.Errorf("DLQ envelope carries %d spans, want %d", got, spans)
	}

	if got := counterAt(t, m.ExemplarSubmitTotal, "traces", "dlq", "queue_full"); got != 1 {
		t.Errorf("exemplar_submit_total{dlq,queue_full} = %v, want 1", got)
	}
	// Deferred is not lost.
	if got := counterAt(t, m.ExemplarSubmitTotal, "traces", "lost", "queue_full"); got != 0 {
		t.Errorf("lost{reason=queue_full} = %v, want 0 — the DLQ accepted the batch", got)
	}
	if testutil.CollectAndCount(m.ExemplarSubmitLostTotal) != 0 {
		t.Error("loss counter moved on a deferred outcome")
	}
}

// --- acceptance 2: aggregate mode, queue full AND DLQ refuses ---------------

func TestAggregateMode_QueueFullAndDLQRefuses_SucceedsWithCountedLoss(t *testing.T) {
	const spans = 2
	now := time.Now().UTC()

	cases := []struct {
		name   string
		sink   BatchSink
		reason string
	}{
		{"dlq_full", &captureDLQ{refuseWith: ErrDLQFull}, "dlq_full"},
		{"dlq_error", &captureDLQ{refuseWith: errors.New("disk on fire")}, "dlq_error"},
		{"no_sink", nil, "dlq_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, m := exportSaturatedTraces(t, tc.name, tc.sink, spans, now)
			ps := resp.GetPartialSuccess()
			want := fmt.Sprintf("aggregate data accepted; %d selected raw exemplars could not be retained", spans)
			if ps.ErrorMessage != want {
				t.Errorf("error_message = %q, want %q", ps.ErrorMessage, want)
			}
			if got := counterAt(t, m.ExemplarSubmitTotal, "traces", "lost", tc.reason); got != 1 {
				t.Errorf("exemplar_submit_total{lost,%s} = %v, want 1", tc.reason, got)
			}
			if got := counterAt(t, m.ExemplarSubmitLostTotal, "traces", tc.reason); got != 1 {
				t.Errorf("exemplar_submit_lost_total{%s} = %v, want 1", tc.reason, got)
			}
			if got := counterAt(t, m.ExemplarSubmitTotal, "traces", "lost", "queue_full"); got != 0 {
				t.Errorf("lost{reason=queue_full} = %v, want 0 — never a valid reason", got)
			}
		})
	}
}

// --- acceptance 3: shadow mode, hard rejection -----------------------------

func TestShadowMode_HardRejection_DoesNotApplyShadowAggregate(t *testing.T) {
	now := time.Now().UTC()
	sink := &captureDLQ{}
	p := saturatedPipeline(t, nil, sink)

	eng := ackEngine(t, aggregate.ModeShadow, now)
	revBefore := eng.Revision()
	srv := NewTraceServer(nil, nil, aggTestConfig())
	srv.SetPipeline(p)
	srv.SetAggregateEngine(eng)

	_, err := srv.Export(context.Background(), ackErrorSpanRequest(4, now))
	st, ok := grpcstatus.FromError(err)
	if err == nil || !ok || st.Code() != codes.ResourceExhausted {
		t.Fatalf("Export error = %v, want RESOURCE_EXHAUSTED", err)
	}
	if count, _ := eng.Snapshot().Totals(aggregate.SignalTraceOp); count != 0 {
		t.Fatalf("shadow aggregate count = %d, want 0 — a retryable failure must contribute nothing", count)
	}
	if got := eng.Revision(); got != revBefore {
		t.Fatalf("engine revision moved %d -> %d on a rejected Export", revBefore, got)
	}
	if len(sink.snapshot()) != 0 {
		t.Fatal("shadow mode must not absorb a rejection into the DLQ — that is the aggregate-mode contract only")
	}
}

// --- acceptance 4: shadow mode, intentional soft-drop ----------------------

func TestShadowMode_SoftDrop_AppliesShadowAggregateExactlyOnce(t *testing.T) {
	const spans = 5
	now := time.Now().UTC()

	// Capacity 1 with the slot taken: fullness is 1.0, so a HEALTHY batch is
	// shed by soft backpressure rather than hard-rejected.
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 1, Workers: 1})
	if _, err := p.Submit(errorBatch()); err != nil {
		t.Fatalf("priming submit: %v", err)
	}

	eng := ackEngine(t, aggregate.ModeShadow, now)
	srv := NewTraceServer(nil, nil, aggTestConfig())
	srv.SetPipeline(p)
	srv.SetAggregateEngine(eng)

	// Healthy (OK, fast) spans — aggSpanRequest builds exactly that.
	req := aggSpanRequest(spans, now)
	before := p.Stats().DroppedHealthy
	resp, err := srv.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("an intentional soft-drop is a successful Export: %v", err)
	}
	if resp.GetPartialSuccess() != nil {
		t.Error("soft-drops are counted separately and must not raise a partial_success warning")
	}
	if got := p.Stats().DroppedHealthy - before; got == 0 {
		t.Fatal("no soft-drop happened; the test is vacuous")
	}
	// aggSpanRequest emits spansPerService spans across three services.
	if count, _ := eng.Snapshot().Totals(aggregate.SignalTraceOp); count != spans*3 {
		t.Fatalf("shadow aggregate count = %d, want %d (applied exactly once)", count, spans*3)
	}
}

// --- acceptance 5: shadow-mode retry does not double count -----------------

func TestShadowMode_RetryAfterResourceExhausted_DoesNotDoubleCount(t *testing.T) {
	const spans = 3
	now := time.Now().UTC()
	p := saturatedPipeline(t, nil, nil)

	eng := ackEngine(t, aggregate.ModeShadow, now)
	srv := NewTraceServer(nil, nil, aggTestConfig())
	srv.SetPipeline(p)
	srv.SetAggregateEngine(eng)

	req := ackErrorSpanRequest(spans, now)

	// Attempt 1: the pipeline is saturated -> RESOURCE_EXHAUSTED, no shadow.
	if _, err := srv.Export(context.Background(), req); err == nil {
		t.Fatal("attempt 1 should have been rejected")
	}
	if count, _ := eng.Snapshot().Totals(aggregate.SignalTraceOp); count != 0 {
		t.Fatalf("after rejection: shadow count = %d, want 0", count)
	}

	// The compliant client backs off and retries the identical Export.
	drainOne(t, p)
	if _, err := srv.Export(context.Background(), req); err != nil {
		t.Fatalf("attempt 2 (retry): %v", err)
	}
	if count, _ := eng.Snapshot().Totals(aggregate.SignalTraceOp); count != spans {
		t.Fatalf("after retry: shadow count = %d, want %d — the rejected attempt must not have contributed", count, spans)
	}
}

// --- symmetry: the log signal honours the identical contract ---------------

func TestAggregateMode_Logs_QueueFull_DefersToDLQAndSucceeds(t *testing.T) {
	const records = 4
	now := time.Now().UTC()
	m := ackTestMetrics("logs_defer")
	sink := &captureDLQ{}
	p := saturatedPipeline(t, m, sink)

	eng := ackEngine(t, aggregate.ModeAggregate, now)
	srv := NewLogsServer(nil, m, aggTestConfig())
	srv.SetPipeline(p)
	srv.SetAggregateEngine(eng)

	resp, err := srv.Export(context.Background(), ackErrorLogRequest(records, now))
	if err != nil {
		t.Fatalf("Export must succeed once the aggregate commit landed: %v", err)
	}
	if count, _ := eng.Snapshot().Totals(aggregate.SignalLog); count != records {
		t.Fatalf("aggregate log count = %d, want %d", count, records)
	}
	ps := resp.GetPartialSuccess()
	if ps == nil {
		t.Fatal("partial_success not populated on a deferred log Export")
	}
	if ps.RejectedLogRecords != 0 {
		t.Errorf("rejected_log_records = %d, want 0", ps.RejectedLogRecords)
	}
	want := fmt.Sprintf("aggregate data accepted; %d selected raw exemplars were deferred to DLQ", records)
	if ps.ErrorMessage != want {
		t.Errorf("error_message = %q, want %q", ps.ErrorMessage, want)
	}
	envs := sink.snapshot()
	if len(envs) != 1 || len(envs[0].Data.Logs) != records {
		t.Fatalf("DLQ envelopes = %d, logs = %v, want 1 envelope carrying %d logs", len(envs), envs, records)
	}
	if got := counterAt(t, m.ExemplarSubmitTotal, "logs", "dlq", "queue_full"); got != 1 {
		t.Errorf("exemplar_submit_total{logs,dlq,queue_full} = %v, want 1", got)
	}
}

func TestShadowMode_Logs_HardRejection_DoesNotApplyShadowAggregate(t *testing.T) {
	now := time.Now().UTC()
	p := saturatedPipeline(t, nil, nil)
	eng := ackEngine(t, aggregate.ModeShadow, now)
	srv := NewLogsServer(nil, nil, aggTestConfig())
	srv.SetPipeline(p)
	srv.SetAggregateEngine(eng)

	_, err := srv.Export(context.Background(), ackErrorLogRequest(3, now))
	st, ok := grpcstatus.FromError(err)
	if err == nil || !ok || st.Code() != codes.ResourceExhausted {
		t.Fatalf("Export error = %v, want RESOURCE_EXHAUSTED", err)
	}
	if count, _ := eng.Snapshot().Totals(aggregate.SignalLog); count != 0 {
		t.Fatalf("shadow log count = %d, want 0", count)
	}
}

// --- partial_success never describes synthesized logs ----------------------

// An ERROR span synthesizes an ERROR log that the client never sent. It rides
// in the same batch, so it reaches the DLQ — but OTLP must not count it as a
// rejected or deferred record (#196).
func TestPartialSuccessCountsSelectedSpansNotSynthesizedLogs(t *testing.T) {
	const spans = 2
	now := time.Now().UTC()
	sink := &captureDLQ{}
	p := saturatedPipeline(t, nil, sink)
	eng := ackEngine(t, aggregate.ModeAggregate, now)
	srv := NewTraceServer(nil, nil, aggTestConfig())
	srv.SetPipeline(p)
	srv.SetAggregateEngine(eng)

	resp, err := srv.Export(context.Background(), ackErrorSpanRequest(spans, now))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	envs := sink.snapshot()
	if len(envs) != 1 {
		t.Fatalf("DLQ envelopes = %d, want 1", len(envs))
	}
	if len(envs[0].Data.Logs) == 0 {
		t.Fatal("expected synthesized ERROR logs in the deferred batch; the test is vacuous")
	}
	want := fmt.Sprintf("aggregate data accepted; %d selected raw exemplars were deferred to DLQ", spans)
	if got := resp.GetPartialSuccess().GetErrorMessage(); got != want {
		t.Errorf("error_message = %q, want %q (synthesized logs must not be counted)", got, want)
	}
}

// --- the outcome type itself ----------------------------------------------

func TestSubmitOutcomes(t *testing.T) {
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 2, Workers: 1})

	if out, err := p.Submit(nil); err != nil || out != SubmitEnqueued {
		t.Errorf("Submit(nil) = (%v, %v), want (enqueued, nil)", out, err)
	}
	if out, err := p.Submit(&Batch{Type: SignalTraces}); err != nil || out != SubmitEnqueued {
		t.Errorf("Submit(empty) = (%v, %v), want (enqueued, nil)", out, err)
	}
	if out, err := p.Submit(healthyBatch()); err != nil || out != SubmitEnqueued {
		t.Errorf("Submit(healthy) = (%v, %v), want (enqueued, nil)", out, err)
	}
	// Slot 2 of 2 -> fullness 1.0 -> healthy batches are shed.
	if _, err := p.Submit(errorBatch()); err != nil {
		t.Fatalf("priming: %v", err)
	}
	if out, err := p.Submit(healthyBatch()); err != nil || out != SubmitSoftDropped {
		t.Errorf("Submit at capacity (healthy) = (%v, %v), want (soft_dropped, nil)", out, err)
	}
	if _, err := p.Submit(errorBatch()); !errors.Is(err, ErrQueueFull) {
		t.Errorf("Submit at capacity (priority) error = %v, want ErrQueueFull", err)
	}
	if got := SubmitSoftDropped.String(); got != "soft_dropped" {
		t.Errorf("SubmitSoftDropped.String() = %q", got)
	}
}

// --- OTLP/HTTP carries the same partial_success -----------------------------

// gRPC and HTTP share Export(), but the response still has to survive
// HTTPHandler.writeResponse. This pins that plumbing for both wire encodings.
func TestHTTPOTLPCarriesPartialSuccess(t *testing.T) {
	const spans = 3
	now := time.Now().UTC()
	sink := &captureDLQ{}
	p := saturatedPipeline(t, nil, sink)

	eng := ackEngine(t, aggregate.ModeAggregate, now)
	traces := NewTraceServer(nil, nil, aggTestConfig())
	traces.SetPipeline(p)
	traces.SetAggregateEngine(eng)
	h := NewHTTPHandler(traces, NewLogsServer(nil, nil, aggTestConfig()), nil)

	body, err := proto.Marshal(ackErrorSpanRequest(spans, now))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a deferred exemplar is not a client error", rec.Code)
	}
	var resp coltracepb.ExportTraceServiceResponse
	if err := proto.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	ps := resp.GetPartialSuccess()
	if ps == nil {
		t.Fatal("HTTP response dropped partial_success")
	}
	if ps.RejectedSpans != 0 {
		t.Errorf("rejected_spans = %d, want 0", ps.RejectedSpans)
	}
	want := fmt.Sprintf("aggregate data accepted; %d selected raw exemplars were deferred to DLQ", spans)
	if ps.ErrorMessage != want {
		t.Errorf("error_message = %q, want %q", ps.ErrorMessage, want)
	}
}
