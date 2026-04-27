package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pipelineHarness wires a real TraceServer + LogsServer to an in-memory
// SQLite repository through the async Pipeline. Each test gets its own
// harness so they don't share queue state.
type pipelineHarness struct {
	repo     *storage.Repository
	traces   *TraceServer
	logs     *LogsServer
	pipeline *Pipeline
}

func newPipelineHarness(t *testing.T, cap, workers int) *pipelineHarness {
	t.Helper()
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")

	cfg := &config.Config{
		IngestMinSeverity:          "DEBUG",
		SamplingLatencyThresholdMs: 500,
	}
	traces := NewTraceServer(repo, nil, cfg)
	logs := NewLogsServer(repo, nil, cfg)

	pl := NewPipeline(repo, nil, PipelineConfig{Capacity: cap, Workers: workers, SoftThreshold: 0.9})
	if workers > 0 {
		pl.Start(context.Background())
	}
	traces.SetPipeline(pl)
	logs.SetPipeline(pl)

	t.Cleanup(func() {
		pl.Stop()
		_ = repo.Close()
	})
	return &pipelineHarness{repo: repo, traces: traces, logs: logs, pipeline: pl}
}

// TestPipelineE2E_TracesPersistThroughPipeline verifies that a trace
// Export call submitted via the pipeline lands in the DB once workers
// drain the queue. Confirms the Trace→Span→Log insert path holds end-
// to-end through async persistence.
func TestPipelineE2E_TracesPersistThroughPipeline(t *testing.T) {
	h := newPipelineHarness(t, 16, 2)
	req := buildTracesRequest("svc-a", 5)

	if _, err := h.traces.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}

	if !waitFor(t, 3*time.Second, func() bool {
		return countSpans(t, h.repo) >= 5
	}) {
		t.Fatalf("spans did not land in DB through pipeline within deadline (got %d, want >=5)", countSpans(t, h.repo))
	}
}

// TestPipelineE2E_LogsPersistThroughPipeline same as above for logs.
func TestPipelineE2E_LogsPersistThroughPipeline(t *testing.T) {
	h := newPipelineHarness(t, 16, 2)
	req := buildLogsRequest("svc-a", 7)

	if _, err := h.logs.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}

	if !waitFor(t, 3*time.Second, func() bool {
		got, err := h.repo.GetRecentLogs(context.Background(), 100)
		if err != nil {
			t.Fatalf("GetRecentLogs: %v", err)
		}
		return countByService(got, "svc-a") >= 7
	}) {
		t.Fatalf("logs did not land in DB through pipeline within deadline")
	}
}

// TestPipelineE2E_HardCapacityReturnsResourceExhausted validates the
// gRPC error-code mapping. With workers=0 and cap=1 priority traffic,
// the second Export must surface RESOURCE_EXHAUSTED so OTLP clients
// back off rather than retry tighter.
func TestPipelineE2E_HardCapacityReturnsResourceExhausted(t *testing.T) {
	// workers=0 → nothing drains → after 1 priority submit, queue is full.
	h := newPipelineHarness(t, 1, 0)

	// First Export fills the queue. Build with an error span so it
	// bypasses soft backpressure (which kicks in at >=90% but a 1-cap
	// queue is degenerate — any non-empty submit is at 100%).
	primer := buildTracesRequest("svc-a", 1)
	primer.ResourceSpans[0].ScopeSpans[0].Spans[0].Status = errorStatusForTest()
	if _, err := h.traces.Export(context.Background(), primer); err != nil {
		t.Fatalf("primer Export: %v", err)
	}
	if got := h.pipeline.Stats().Enqueued; got != 1 {
		t.Fatalf("primer not enqueued (got %d, want 1)", got)
	}

	// Second Export overflows. Use a priority batch so it can't be
	// silently dropped by soft backpressure either.
	overflow := buildTracesRequest("svc-a", 1)
	overflow.ResourceSpans[0].ScopeSpans[0].Spans[0].Status = errorStatusForTest()
	_, err := h.traces.Export(context.Background(), overflow)
	if err == nil {
		t.Fatalf("Export at hard capacity returned nil error, want RESOURCE_EXHAUSTED")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("Export error %v is not a grpc status error", err)
	}
	if st.Code() != codes.ResourceExhausted {
		t.Fatalf("Export error code=%s, want %s", st.Code(), codes.ResourceExhausted)
	}
	if h.pipeline.Stats().RejectedFull == 0 {
		t.Fatalf("RejectedFull counter did not increment after hard-capacity reject")
	}
}

// TestPipelineE2E_PriorityBatchProtected verifies that under sustained
// soft-backpressure conditions, a batch flagged as priority (error span
// or slow span) is never silently dropped — it either enqueues or
// receives an explicit hard-capacity rejection.
func TestPipelineE2E_PriorityBatchProtected(t *testing.T) {
	// cap=10 workers=0 → fill to soft threshold first, then submit
	// healthy (drop) and priority (must still enqueue) batches.
	h := newPipelineHarness(t, 10, 0)
	for range 9 {
		_ = h.pipeline.Submit(healthyBatch())
	}

	// Healthy at >=90% should drop silently.
	healthyReq := buildTracesRequest("svc-h", 1)
	if _, err := h.traces.Export(context.Background(), healthyReq); err != nil {
		t.Fatalf("healthy Export at soft threshold returned %v, want nil", err)
	}
	if h.pipeline.Stats().DroppedHealthy < 1 {
		t.Fatalf("healthy batch was not soft-dropped — DroppedHealthy=%d", h.pipeline.Stats().DroppedHealthy)
	}

	// Priority must enqueue (occupying the 10th slot).
	prioReq := buildTracesRequest("svc-p", 1)
	prioReq.ResourceSpans[0].ScopeSpans[0].Spans[0].Status = errorStatusForTest()
	if _, err := h.traces.Export(context.Background(), prioReq); err != nil {
		t.Fatalf("priority Export at soft threshold returned %v, want nil", err)
	}
	if got := h.pipeline.Stats().Enqueued; got != 10 {
		t.Fatalf("priority batch not enqueued (Enqueued=%d, want 10)", got)
	}
}

// errorStatusForTest is a small helper to flag a span as ERROR for
// priority-batch tests.
func errorStatusForTest() *tracepb.Status {
	return &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
}
