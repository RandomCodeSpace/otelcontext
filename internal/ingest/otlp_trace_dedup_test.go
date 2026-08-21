package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"gorm.io/gorm"
)

// newSyncTraceServer builds a TraceServer on an in-memory SQLite repo with the
// synchronous persist path (no pipeline, no sampler) so a single Export() call
// leaves everything readable in the DB when it returns.
func newSyncTraceServer(t *testing.T) (*TraceServer, *gorm.DB) {
	t.Helper()

	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })

	cfg := &config.Config{IngestMinSeverity: "DEBUG", DefaultTenant: storage.DefaultTenantID}
	return NewTraceServer(repo, nil, cfg), db
}

// dedupTraceID is the single trace all spans in these tests belong to.
var dedupTraceID = []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}

func dedupResource(svc string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
		Key:   "service.name",
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: svc}},
	}}}
}

// dedupSpan builds a span on dedupTraceID with the given status code and
// duration in milliseconds.
func dedupSpan(name string, spanID byte, code tracepb.Status_StatusCode, startNano uint64, durMs uint64) *tracepb.Span {
	return &tracepb.Span{
		TraceId:           dedupTraceID,
		SpanId:            []byte{spanID, spanID, spanID, spanID, spanID, spanID, spanID, spanID},
		Name:              name,
		StartTimeUnixNano: startNano,
		EndTimeUnixNano:   startNano + durMs*uint64(time.Millisecond),
		Status:            &tracepb.Status{Code: code},
	}
}

func exportSpans(t *testing.T, srv *TraceServer, svc string, spans ...*tracepb.Span) {
	t.Helper()
	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource:   dedupResource(svc),
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
		}},
	}
	if _, err := srv.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}
}

// TestExport_ErrorSpans_SynthesizeExactlyOneErrorLogEach guards the behavior of
// the removed O(n^2) rescan: every ERROR-status span with no exception event
// still gets exactly one synthesized ERROR log, and no span gets two.
func TestExport_ErrorSpans_SynthesizeExactlyOneErrorLogEach(t *testing.T) {
	srv, db := newSyncTraceServer(t)
	now := uint64(time.Now().UnixNano())

	exportSpans(t, srv, "checkout",
		dedupSpan("/a", 0xaa, tracepb.Status_STATUS_CODE_ERROR, now, 5),
		dedupSpan("/b", 0xbb, tracepb.Status_STATUS_CODE_ERROR, now+1, 5),
		dedupSpan("/c", 0xcc, tracepb.Status_STATUS_CODE_UNSET, now+2, 5),
	)

	var logs []storage.Log
	if err := db.Order("span_id").Find(&logs).Error; err != nil {
		t.Fatalf("query logs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("synthesized logs = %d, want 2 (one per ERROR span): %+v", len(logs), logs)
	}
	perSpan := map[string]int{}
	for _, l := range logs {
		if l.Severity != "ERROR" {
			t.Fatalf("severity = %q, want ERROR", l.Severity)
		}
		perSpan[l.SpanID]++
	}
	if perSpan["aaaaaaaaaaaaaaaa"] != 1 || perSpan["bbbbbbbbbbbbbbbb"] != 1 {
		t.Fatalf("expected exactly one ERROR log per error span, got %+v", perSpan)
	}
	if perSpan["cccccccccccccccc"] != 0 {
		t.Fatal("UNSET span must not synthesize an ERROR log")
	}
}

// TestExport_ExceptionEvent_SuppressesStatusLog asserts the status-derived log
// is skipped when the span's own events already produced an ERROR log — the
// property the removed rescan was enforcing.
func TestExport_ExceptionEvent_SuppressesStatusLog(t *testing.T) {
	srv, db := newSyncTraceServer(t)
	now := uint64(time.Now().UnixNano())

	span := dedupSpan("/boom", 0xaa, tracepb.Status_STATUS_CODE_ERROR, now, 5)
	span.Status.Message = "status message"
	span.Events = []*tracepb.Span_Event{{
		Name:         "exception",
		TimeUnixNano: now + 1,
		Attributes: []*commonpb.KeyValue{{
			Key:   "exception.message",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "kaboom"}},
		}},
	}}
	exportSpans(t, srv, "checkout", span)

	var logs []storage.Log
	if err := db.Find(&logs).Error; err != nil {
		t.Fatalf("query logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1 (event log only): %+v", len(logs), logs)
	}
	if logs[0].Body != "kaboom" {
		t.Fatalf("body = %q, want the exception message", logs[0].Body)
	}
}

// TestExport_OneTraceRowPerTraceID asserts a multi-span trace produces exactly
// one traces row, seeded from the first span.
func TestExport_OneTraceRowPerTraceID(t *testing.T) {
	srv, db := newSyncTraceServer(t)
	now := uint64(time.Now().UnixNano())

	exportSpans(t, srv, "checkout",
		dedupSpan("/root", 0xaa, tracepb.Status_STATUS_CODE_OK, now, 40),
		dedupSpan("/child1", 0xbb, tracepb.Status_STATUS_CODE_OK, now+1, 10),
		dedupSpan("/child2", 0xcc, tracepb.Status_STATUS_CODE_OK, now+2, 10),
		dedupSpan("/child3", 0xdd, tracepb.Status_STATUS_CODE_OK, now+3, 10),
	)

	var traces []storage.Trace
	if err := db.Find(&traces).Error; err != nil {
		t.Fatalf("query traces: %v", err)
	}
	if len(traces) != 1 {
		t.Fatalf("trace rows = %d, want 1", len(traces))
	}
	if got := traces[0].Duration; got != 40*int64(time.Millisecond/time.Microsecond) {
		t.Fatalf("duration = %d us, want the first span's 40ms", got)
	}
	if traces[0].Status != "STATUS_CODE_OK" {
		t.Fatalf("status = %q, want STATUS_CODE_OK", traces[0].Status)
	}

	var spanCount int64
	if err := db.Model(&storage.Span{}).Count(&spanCount).Error; err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if spanCount != 4 {
		t.Fatalf("span rows = %d, want 4 (dedup must not touch spans)", spanCount)
	}
}

// TestExport_TraceStatusUpgradesToError covers both directions of the
// upgrade-only rule inside one Export: a later ERROR span upgrades the trace,
// and a later healthy span cannot downgrade it.
func TestExport_TraceStatusUpgradesToError(t *testing.T) {
	srv, db := newSyncTraceServer(t)
	now := uint64(time.Now().UnixNano())

	exportSpans(t, srv, "checkout",
		dedupSpan("/root", 0xaa, tracepb.Status_STATUS_CODE_UNSET, now, 40),
		dedupSpan("/child-fails", 0xbb, tracepb.Status_STATUS_CODE_ERROR, now+1, 10),
		dedupSpan("/child-ok", 0xcc, tracepb.Status_STATUS_CODE_OK, now+2, 10),
	)

	var traces []storage.Trace
	if err := db.Find(&traces).Error; err != nil {
		t.Fatalf("query traces: %v", err)
	}
	if len(traces) != 1 {
		t.Fatalf("trace rows = %d, want 1", len(traces))
	}
	if traces[0].Status != storage.StatusCodeError {
		t.Fatalf("status = %q, want %q", traces[0].Status, storage.StatusCodeError)
	}
}

// TestExport_TraceStatusUpgradesAcrossExports covers the cross-batch case the
// DB upsert owns: the root span arrives UNSET in one export, the failing child
// arrives in a later export, and the persisted row must end up ERROR.
func TestExport_TraceStatusUpgradesAcrossExports(t *testing.T) {
	srv, db := newSyncTraceServer(t)
	now := uint64(time.Now().UnixNano())

	exportSpans(t, srv, "checkout", dedupSpan("/root", 0xaa, tracepb.Status_STATUS_CODE_UNSET, now, 40))
	exportSpans(t, srv, "payments", dedupSpan("/charge", 0xbb, tracepb.Status_STATUS_CODE_ERROR, now+1, 10))
	exportSpans(t, srv, "payments", dedupSpan("/refund", 0xcc, tracepb.Status_STATUS_CODE_OK, now+2, 10))

	var traces []storage.Trace
	if err := db.Find(&traces).Error; err != nil {
		t.Fatalf("query traces: %v", err)
	}
	if len(traces) != 1 {
		t.Fatalf("trace rows = %d, want 1", len(traces))
	}
	if traces[0].Status != storage.StatusCodeError {
		t.Fatalf("status = %q, want %q after cross-export upgrade", traces[0].Status, storage.StatusCodeError)
	}
	if traces[0].ServiceName != "checkout" {
		t.Fatalf("service = %q, want the first writer's service to survive", traces[0].ServiceName)
	}
}
