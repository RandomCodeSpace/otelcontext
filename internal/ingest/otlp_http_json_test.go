package ingest

import (
	"bytes"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestUnmarshalOTLPJSON_HexIDs covers the OTLP/JSON spec encoding: trace and
// span IDs are hex. protojson alone decoded them as base64 and dropped them.
func TestUnmarshalOTLPJSON_HexIDs(t *testing.T) {
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{
		"traceId":"5b8efff798038103d269b633813fc60c",
		"spanId":"eee19b7ec3c1b174",
		"parentSpanId":"eee19b7ec3c1b173",
		"name":"op","kind":2,
		"startTimeUnixNano":"1544712660000000000","endTimeUnixNano":"1544712661000000000"}]}]}]}`)

	var req coltracepb.ExportTraceServiceRequest
	if err := unmarshalOTLPJSON(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	span := req.ResourceSpans[0].ScopeSpans[0].Spans[0]
	wantTrace := []byte{0x5b, 0x8e, 0xff, 0xf7, 0x98, 0x03, 0x81, 0x03, 0xd2, 0x69, 0xb6, 0x33, 0x81, 0x3f, 0xc6, 0x0c}
	if !bytes.Equal(span.TraceId, wantTrace) {
		t.Fatalf("TraceId = %x, want %x", span.TraceId, wantTrace)
	}
	if got := span.SpanId; len(got) != 8 || got[0] != 0xee || got[7] != 0x74 {
		t.Fatalf("SpanId = %x, want eee19b7ec3c1b174", got)
	}
	if got := span.ParentSpanId; len(got) != 8 || got[7] != 0x73 {
		t.Fatalf("ParentSpanId = %x, want eee19b7ec3c1b173", got)
	}
}

// TestUnmarshalOTLPJSON_LegacyBase64IDs keeps clients that serialized with
// protojson (base64 IDs) working after the switch to the spec codec.
func TestUnmarshalOTLPJSON_LegacyBase64IDs(t *testing.T) {
	traces := buildTracesRequest("svc-legacy", 1)
	body, err := protojson.Marshal(traces)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var req coltracepb.ExportTraceServiceRequest
	if err := unmarshalOTLPJSON(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := traces.ResourceSpans[0].ScopeSpans[0].Spans[0].TraceId
	if got := req.ResourceSpans[0].ScopeSpans[0].Spans[0].TraceId; !bytes.Equal(got, want) {
		t.Fatalf("TraceId = %x, want %x", got, want)
	}

	logs := buildLogsRequest("svc-legacy", 1)
	body, err = protojson.Marshal(logs)
	if err != nil {
		t.Fatalf("marshal logs: %v", err)
	}
	var logReq collogspb.ExportLogsServiceRequest
	if err := unmarshalOTLPJSON(body, &logReq); err != nil {
		t.Fatalf("unmarshal logs: %v", err)
	}
	if n := len(logReq.ResourceLogs[0].ScopeLogs[0].LogRecords); n != 1 {
		t.Fatalf("log records = %d, want 1", n)
	}
}

// TestUnmarshalOTLPJSON_Invalid still rejects a body neither codec accepts.
func TestUnmarshalOTLPJSON_Invalid(t *testing.T) {
	var req coltracepb.ExportTraceServiceRequest
	if err := unmarshalOTLPJSON([]byte(`{"resourceSpans":"not-a-list"}`), &req); err == nil {
		t.Fatal("expected an error for a malformed body")
	}
}
