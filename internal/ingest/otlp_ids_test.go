package ingest

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/prometheus/client_golang/prometheus"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const spanRefusedMsg = "spans refused: malformed trace_id/span_id"

// postTraces posts body to /v1/traces and decodes the response in the
// request's own encoding.
func postTraces(t *testing.T, h *e2eHarness, contentType string, body []byte) *coltracepb.ExportTraceServiceResponse {
	t.Helper()
	resp := postBody( //nolint:bodyclose // closed by readAllAndClose helper
		t, h.server.URL+"/v1/traces", contentType, "", body)
	rb := readAllAndClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%q", resp.StatusCode, rb)
	}
	out := &coltracepb.ExportTraceServiceResponse{}
	unmarshal := proto.Unmarshal
	if contentType == contentTypeJSON {
		unmarshal = protojson.Unmarshal
	}
	if err := unmarshal(rb, out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return out
}

func jsonTraces(spans ...string) []byte {
	now := time.Now().UnixNano()
	parts := make([]string, 0, len(spans))
	for _, s := range spans {
		parts = append(parts, fmt.Sprintf(`{%s,"name":"op","startTimeUnixNano":"%d","endTimeUnixNano":"%d"}`, s, now, now+int64(time.Millisecond)))
	}
	return []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"svc-ids"}}]},` +
		`"scopeSpans":[{"spans":[` + strings.Join(parts, ",") + `]}]}]}`)
}

func TestMalformedSpanIDsRefused(t *testing.T) {
	protobufBody := func(t *testing.T) []byte {
		req := buildTracesRequest("svc-ids", 5)
		spans := req.ResourceSpans[0].ScopeSpans[0].Spans
		spans[2].TraceId = spans[2].TraceId[:15]      // bad length
		spans[3].SpanId = make([]byte, spanIDLen)     // all-zero
		spans[4].ParentSpanId = []byte{1, 2, 3, 4, 5} // bad parent
		b, err := proto.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	jsonBody := func(*testing.T) []byte {
		return jsonTraces(
			`"traceId":"0123456789abcdef0123456789abcdef","spanId":"0123456789abcdef"`,
			`"traceId":"1123456789abcdef0123456789abcdef","spanId":"1123456789abcdef","parentSpanId":"0123456789abcdef"`,
			`"traceId":"0123456789abcdef0123456789abcd","spanId":"2123456789abcdef"`,
			`"traceId":"2123456789abcdef0123456789abcdef","spanId":"0000000000000000"`,
			`"traceId":"3123456789abcdef0123456789abcdef","spanId":"3123456789abcdef","parentSpanId":"abcd"`,
		)
	}
	for _, tc := range []struct {
		name, contentType string
		body              func(*testing.T) []byte
	}{
		{"protobuf", contentTypeProtobuf, protobufBody},
		{"json", contentTypeJSON, jsonBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newE2EHarness(t)
			out := postTraces(t, h, tc.contentType, tc.body(t))
			ps := out.GetPartialSuccess()
			if ps.GetRejectedSpans() != 3 {
				t.Fatalf("rejected_spans = %d, want 3", ps.GetRejectedSpans())
			}
			if want := "3 " + spanRefusedMsg; ps.GetErrorMessage() != want {
				t.Fatalf("error_message = %q, want %q", ps.GetErrorMessage(), want)
			}
			if c := h.spanCalls.Load(); c != 2 {
				t.Fatalf("stored spans = %d, want 2", c)
			}
		})
	}
}

func TestAllSpansRefusedStillSucceeds(t *testing.T) {
	h := newE2EHarness(t)
	req := buildTracesRequest("svc-ids", 2)
	for _, sp := range req.ResourceSpans[0].ScopeSpans[0].Spans {
		sp.TraceId = make([]byte, traceIDLen)
	}
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ps := postTraces(t, h, contentTypeProtobuf, b).GetPartialSuccess()
	if ps.GetRejectedSpans() != 2 || ps.GetErrorMessage() != "2 "+spanRefusedMsg {
		t.Fatalf("partial_success = %v, want 2 rejected", ps)
	}
	if c := h.spanCalls.Load(); c != 0 {
		t.Fatalf("stored spans = %d, want 0", c)
	}
	if n := countSpans(t, h.repo); n != 0 {
		t.Fatalf("trace rows = %d, want 0", n)
	}
}

func TestMalformedLinkDroppedSpanKept(t *testing.T) {
	h := newE2EHarness(t)
	req := buildTracesRequest("svc-ids", 1)
	sp := req.ResourceSpans[0].ScopeSpans[0].Spans[0]
	sp.Links = []*tracepb.Span_Link{
		{TraceId: bytes.Repeat([]byte{7}, traceIDLen), SpanId: bytes.Repeat([]byte{7}, spanIDLen)},
		{TraceId: bytes.Repeat([]byte{7}, traceIDLen), SpanId: []byte{7}},
	}

	clean, refused, links := sanitizeTraceRequest(req)
	if refused != 0 || links != 1 || len(clean.ResourceSpans[0].ScopeSpans[0].Spans[0].Links) != 1 {
		t.Fatalf("sanitize: refused=%d links=%d", refused, links)
	}
	if len(sp.Links) != 2 {
		t.Fatal("sanitize mutated the caller's request")
	}

	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ps := postTraces(t, h, contentTypeProtobuf, b).GetPartialSuccess()
	if ps.GetRejectedSpans() != 0 || ps.GetErrorMessage() != "1 span links dropped: malformed trace_id/span_id" {
		t.Fatalf("partial_success = %v", ps)
	}
	if c := h.spanCalls.Load(); c != 1 {
		t.Fatalf("stored spans = %d, want 1", c)
	}
}

func TestMalformedLogIDsRefused(t *testing.T) {
	h := newE2EHarness(t)
	req := buildLogsRequest("svc-ids", 4)
	recs := req.ResourceLogs[0].ScopeLogs[0].LogRecords
	// recs[0]: no ids (uncorrelated) — accepted.
	recs[1].TraceId, recs[1].SpanId = bytes.Repeat([]byte{1}, traceIDLen), bytes.Repeat([]byte{1}, spanIDLen)
	recs[2].TraceId = bytes.Repeat([]byte{2}, traceIDLen) // trace only — accepted
	recs[3].TraceId = bytes.Repeat([]byte{3}, 15)         // refused
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := postBody( //nolint:bodyclose // closed by readAllAndClose helper
		t, h.server.URL+"/v1/logs", contentTypeProtobuf, "", b)
	rb := readAllAndClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%q", resp.StatusCode, rb)
	}
	var out collogspb.ExportLogsServiceResponse
	if err := proto.Unmarshal(rb, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ps := out.GetPartialSuccess()
	if ps.GetRejectedLogRecords() != 1 || ps.GetErrorMessage() != "1 log records refused: malformed trace_id/span_id" {
		t.Fatalf("partial_success = %v", ps)
	}
	if c := h.logCalls.Load(); c != 3 {
		t.Fatalf("stored logs = %d, want 3", c)
	}
}

// Aggregate mode, saturated queue: the refused span reaches neither the
// reducer nor the DLQ, its count merges into the mode's own warning, and the
// rejection counter moves.
func TestMalformedIDsMergeWithAggregateWarningAndCount(t *testing.T) {
	now := time.Now().UTC()
	m := ackTestMetrics("invalid_ids")
	m.IngestRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_ingest_rejected_total_invalid_ids",
	}, []string{"signal", "reason"})
	sink := &captureDLQ{}
	eng := ackEngine(t, aggregate.ModeAggregate, now)
	srv := NewTraceServer(nil, m, aggTestConfig())
	srv.SetPipeline(saturatedPipeline(t, m, sink))
	srv.SetAggregateEngine(eng)

	req := ackErrorSpanRequest(3, now)
	bad := proto.Clone(req.ResourceSpans[0].ScopeSpans[0].Spans[0]).(*tracepb.Span)
	bad.TraceId = nil
	req.ResourceSpans[0].ScopeSpans[0].Spans = append(req.ResourceSpans[0].ScopeSpans[0].Spans, bad)

	resp, err := srv.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if count, _ := eng.Snapshot().Totals(aggregate.SignalTraceOp); count != 3 {
		t.Fatalf("aggregate count = %d, want 3", count)
	}
	ps := resp.GetPartialSuccess()
	want := "aggregate data accepted; 3 selected raw exemplars were deferred to DLQ 1 " + spanRefusedMsg
	if ps.GetRejectedSpans() != 1 || ps.GetErrorMessage() != want {
		t.Fatalf("partial_success = %v, want rejected=1 message %q", ps, want)
	}
	envs := sink.snapshot()
	if len(envs) != 1 || len(envs[0].Data.Spans) != 3 {
		t.Fatalf("DLQ envelopes = %+v, want one with 3 spans", envs)
	}
	if got := counterAt(t, m.IngestRejectedTotal, "traces", invalidIDReason); got != 1 {
		t.Fatalf("traces counter = %v, want 1", got)
	}

	logs := NewLogsServer(nil, m, aggTestConfig())
	logReq := ackErrorLogRequest(1, now)
	logReq.ResourceLogs[0].ScopeLogs[0].LogRecords[0].SpanId = make([]byte, spanIDLen)
	logResp, err := logs.Export(context.Background(), logReq)
	if err != nil {
		t.Fatalf("logs Export: %v", err)
	}
	if logResp.GetPartialSuccess().GetRejectedLogRecords() != 1 {
		t.Fatalf("logs partial_success = %v", logResp.GetPartialSuccess())
	}
	if got := counterAt(t, m.IngestRejectedTotal, "logs", invalidIDReason); got != 1 {
		t.Fatalf("logs counter = %v, want 1", got)
	}
}

// A retryable failure must not count: the client's retry would count the same
// refused records a second time.
func TestMalformedIDsCountedOnceAcrossRetry(t *testing.T) {
	now := time.Now().UTC()
	m := ackTestMetrics("invalid_ids_retry")
	m.IngestRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_ingest_rejected_total_invalid_ids_retry",
	}, []string{"signal", "reason"})
	p := saturatedPipeline(t, m, nil)
	srv := NewTraceServer(nil, m, aggTestConfig())
	srv.SetPipeline(p)

	req := ackErrorSpanRequest(1, now)
	bad := proto.Clone(req.ResourceSpans[0].ScopeSpans[0].Spans[0]).(*tracepb.Span)
	bad.SpanId = make([]byte, spanIDLen)
	req.ResourceSpans[0].ScopeSpans[0].Spans = append(req.ResourceSpans[0].ScopeSpans[0].Spans, bad)

	if _, err := srv.Export(context.Background(), req); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("first Export err = %v, want ResourceExhausted", err)
	}
	if got := counterAt(t, m.IngestRejectedTotal, "traces", invalidIDReason); got != 0 {
		t.Fatalf("counter after failed Export = %v, want 0", got)
	}
	drainOne(t, p)
	resp, err := srv.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("retry Export: %v", err)
	}
	if resp.GetPartialSuccess().GetRejectedSpans() != 1 {
		t.Fatalf("partial_success = %v, want rejected=1", resp.GetPartialSuccess())
	}
	if got := counterAt(t, m.IngestRejectedTotal, "traces", invalidIDReason); got != 1 {
		t.Fatalf("counter after retry = %v, want 1", got)
	}
}
