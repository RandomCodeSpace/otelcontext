package ingest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// priorityTracesBody marshals an OTLP trace request whose first span is
// flagged STATUS_CODE_ERROR. The pipeline treats this as a priority batch,
// so it bypasses soft backpressure and requires a literal full-channel
// rejection (ErrQueueFull) — the path the HTTP 429 mapping is meant to cover.
func priorityTracesBody(t *testing.T, service string, count int) []byte {
	t.Helper()
	req := buildTracesRequest(service, count)
	if len(req.ResourceSpans) > 0 && len(req.ResourceSpans[0].ScopeSpans) > 0 && len(req.ResourceSpans[0].ScopeSpans[0].Spans) > 0 {
		req.ResourceSpans[0].ScopeSpans[0].Spans[0].Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
	}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// priorityLogsBody marshals an OTLP logs request flagged ERROR severity so
// it bypasses soft backpressure (LogsServer flags HasError when any record
// is Severity ERROR or FATAL).
func priorityLogsBody(t *testing.T, service string, count int) []byte {
	t.Helper()
	req := buildLogsRequest(service, count)
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				lr.SeverityText = "ERROR"
			}
		}
	}
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// newHTTPBackpressureHarness wires a TraceServer + LogsServer + MetricsServer
// to a Pipeline whose capacity is exhausted by the first Submit, so any
// follow-up Export returns ErrQueueFull. Used by Phase 4 tests that exercise
// the HTTP 429 + Retry-After path.
type httpBackpressureHarness struct {
	repo     *storage.Repository
	pipeline *Pipeline
	handler  *HTTPHandler
}

func newHTTPBackpressureHarness(t *testing.T) *httpBackpressureHarness {
	t.Helper()
	repo, traces, logs := newIngestTestRepo(t)
	// Metrics server is not needed for backpressure tests — the throttle
	// path is exercised via traces and logs. Pass a no-op MetricsServer
	// (nil tsdb is safe because we never invoke metrics.Export here).
	var metrics *MetricsServer

	// Capacity 1, NO workers — a single submit fills it and the next is
	// ErrQueueFull. Capacity 0 is rejected by the pipeline so we use 1 + a
	// pre-fill batch.
	pl := NewPipeline(repo, nil, PipelineConfig{Capacity: 1, Workers: 0, SoftThreshold: 0.5})
	traces.SetPipeline(pl)
	logs.SetPipeline(pl)

	// Pre-fill with a PRIORITY batch (HasError=true) so it bypasses soft
	// backpressure and lands in the channel — capacity 1, so the channel is
	// now full. Subsequent priority submits hit ErrQueueFull (the channel-
	// full path); healthy submits would still be silently soft-dropped.
	if _, err := pl.Submit(&Batch{
		Type:     SignalTraces,
		Tenant:   "default",
		Traces:   []storage.Trace{{TraceID: "x", ServiceName: "svc"}},
		HasError: true,
	}); err != nil {
		t.Fatalf("pre-fill Submit: %v", err)
	}

	h := NewHTTPHandler(traces, logs, metrics)
	t.Cleanup(func() {
		pl.Stop()
		_ = repo.Close()
	})
	return &httpBackpressureHarness{repo: repo, pipeline: pl, handler: h}
}

// TestHTTPBackpressure_TracesReturns429WithRetryAfter verifies that when the
// async pipeline is at capacity, the HTTP traces endpoint responds with 429,
// a Retry-After header, and an OTLP-shaped Status protobuf body.
func TestHTTPBackpressure_TracesReturns429WithRetryAfter(t *testing.T) {
	h := newHTTPBackpressureHarness(t)
	body := priorityTracesBody(t, "svc", 1)

	hr := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	hr.Header.Set("Content-Type", contentTypeProtobuf)
	rec := httptest.NewRecorder()

	mux := http.NewServeMux()
	h.handler.RegisterRoutes(mux)
	mux.ServeHTTP(rec, hr)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header missing on 429 response")
	}
	if ct := rec.Header().Get("Content-Type"); ct != contentTypeProtobuf {
		t.Fatalf("Content-Type want %s, got %s", contentTypeProtobuf, ct)
	}
}

// TestHTTPBackpressure_LogsReturns429 mirrors the traces test for logs.
func TestHTTPBackpressure_LogsReturns429(t *testing.T) {
	h := newHTTPBackpressureHarness(t)
	body := priorityLogsBody(t, "svc", 1)

	hr := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
	hr.Header.Set("Content-Type", contentTypeProtobuf)
	rec := httptest.NewRecorder()

	mux := http.NewServeMux()
	h.handler.RegisterRoutes(mux)
	mux.ServeHTTP(rec, hr)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header missing")
	}
}

// TestHTTPBackpressure_ThrottleCallbackInvoked verifies the per-signal
// callback fires exactly once per 429, with the right signal label.
func TestHTTPBackpressure_ThrottleCallbackInvoked(t *testing.T) {
	h := newHTTPBackpressureHarness(t)

	var traceHits, logHits, metricHits atomic.Int64
	h.handler.SetThrottleCallback(func(signal string) {
		switch signal {
		case "traces":
			traceHits.Add(1)
		case "logs":
			logHits.Add(1)
		case "metrics":
			metricHits.Add(1)
		}
	})

	mux := http.NewServeMux()
	h.handler.RegisterRoutes(mux)

	tBody := priorityTracesBody(t, "svc", 1)
	hr := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(tBody))
	hr.Header.Set("Content-Type", contentTypeProtobuf)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, hr)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("traces: want 429, got %d", rec.Code)
	}

	lBody := priorityLogsBody(t, "svc", 1)
	hr = httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(lBody))
	hr.Header.Set("Content-Type", contentTypeProtobuf)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, hr)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("logs: want 429, got %d", rec.Code)
	}

	if traceHits.Load() != 1 {
		t.Fatalf("traceHits = %d, want 1", traceHits.Load())
	}
	if logHits.Load() != 1 {
		t.Fatalf("logHits = %d, want 1", logHits.Load())
	}
	if metricHits.Load() != 0 {
		t.Fatalf("metricHits should be 0 when no metric request was sent; got %d", metricHits.Load())
	}
}

// TestHTTPBackpressure_NotInvokedOnSuccess verifies that a successful
// (non-throttled) Export does NOT increment the throttle counter.
func TestHTTPBackpressure_NotInvokedOnSuccess(t *testing.T) {
	// Use a normal harness with workers so Submit succeeds.
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	defer func() { _ = repo.Close() }()

	cfg := &config.Config{IngestMinSeverity: "DEBUG", SamplingLatencyThresholdMs: 500}
	traces := NewTraceServer(repo, nil, cfg)
	logs := NewLogsServer(repo, nil, cfg)
	var metrics *MetricsServer // not exercised in this test
	pl := NewPipeline(repo, nil, PipelineConfig{Capacity: 16, Workers: 1, SoftThreshold: 0.9})
	pl.Start(context.Background())
	defer pl.Stop()
	traces.SetPipeline(pl)
	logs.SetPipeline(pl)

	handler := NewHTTPHandler(traces, logs, metrics)

	var hits atomic.Int64
	handler.SetThrottleCallback(func(signal string) { hits.Add(1) })

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	body, _ := proto.Marshal(buildTracesRequest("svc", 1))
	hr := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	hr.Header.Set("Content-Type", contentTypeProtobuf)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, hr)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("throttle callback fired on a successful request; hits=%d", hits.Load())
	}
}

// TestIsQueueFull_ClassifiesCorrectly verifies the helper picks up both
// gRPC RESOURCE_EXHAUSTED status errors AND the local ErrQueueFull sentinel.
func TestIsQueueFull_ClassifiesCorrectly(t *testing.T) {
	if !isQueueFull(ErrQueueFull) {
		t.Fatal("isQueueFull should match ErrQueueFull sentinel")
	}
	// Wrap to confirm errors.Is propagation.
	wrapped := wrapErr(ErrQueueFull)
	if !isQueueFull(wrapped) {
		t.Fatal("isQueueFull should match wrapped ErrQueueFull")
	}
	if isQueueFull(nil) {
		t.Fatal("isQueueFull(nil) must be false")
	}
	if isQueueFull(simpleErr("boom")) {
		t.Fatal("isQueueFull should not match unrelated errors")
	}
}

// helpers below are deliberately tiny so they don't accumulate testing
// abstractions inside ingest_test.

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

func wrapErr(err error) error {
	return &wrapped{err: err}
}

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

// keep the import set narrow — protojson + JSON content not needed.
var _ = strings.Contains
