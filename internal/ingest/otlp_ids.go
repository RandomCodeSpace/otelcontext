package ingest

import (
	"bytes"
	"fmt"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// OTLP id rules: trace_id is 16 bytes, span_id is 8 bytes, all-zero is invalid.
const (
	traceIDLen = 16
	spanIDLen  = 8
	// invalidIDReason labels otelcontext_ingest_rejected_total.
	invalidIDReason = "invalid_id"
)

var zeroID [traceIDLen]byte

func validID(id []byte, n int) bool {
	return len(id) == n && !bytes.Equal(id, zeroID[:n])
}

// optionalID accepts an absent id; a present one must be valid.
func optionalID(id []byte, n int) bool { return len(id) == 0 || validID(id, n) }

func validSpanIDs(s *tracepb.Span) bool {
	return validID(s.TraceId, traceIDLen) && validID(s.SpanId, spanIDLen) && optionalID(s.ParentSpanId, spanIDLen)
}

func validLinkIDs(l *tracepb.Span_Link) bool {
	return validID(l.TraceId, traceIDLen) && validID(l.SpanId, spanIDLen)
}

func validLogIDs(l *logspb.LogRecord) bool {
	return optionalID(l.TraceId, traceIDLen) && optionalID(l.SpanId, spanIDLen)
}

// sanitizeTraceRequest removes spans with malformed ids and links with
// malformed ids. The caller's request is never mutated: a clean request is
// returned as-is, otherwise a filtered copy is built.
func sanitizeTraceRequest(req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceRequest, int, int) {
	refused, links := 0, 0
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				if !validSpanIDs(sp) {
					refused++
					continue
				}
				for _, l := range sp.GetLinks() {
					if !validLinkIDs(l) {
						links++
					}
				}
			}
		}
	}
	if refused == 0 && links == 0 {
		return req, 0, 0
	}
	out := &coltracepb.ExportTraceServiceRequest{}
	for _, rs := range req.GetResourceSpans() {
		nrs := &tracepb.ResourceSpans{Resource: rs.GetResource(), SchemaUrl: rs.GetSchemaUrl()}
		for _, ss := range rs.GetScopeSpans() {
			nss := &tracepb.ScopeSpans{Scope: ss.GetScope(), SchemaUrl: ss.GetSchemaUrl()}
			for _, sp := range ss.GetSpans() {
				if !validSpanIDs(sp) {
					continue
				}
				kept := make([]*tracepb.Span_Link, 0, len(sp.GetLinks()))
				for _, l := range sp.GetLinks() {
					if validLinkIDs(l) {
						kept = append(kept, l)
					}
				}
				if len(kept) != len(sp.GetLinks()) {
					sp = proto.Clone(sp).(*tracepb.Span)
					sp.Links = kept
				}
				nss.Spans = append(nss.Spans, sp)
			}
			if len(nss.Spans) > 0 {
				nrs.ScopeSpans = append(nrs.ScopeSpans, nss)
			}
		}
		if len(nrs.ScopeSpans) > 0 {
			out.ResourceSpans = append(out.ResourceSpans, nrs)
		}
	}
	return out, refused, links
}

// sanitizeLogsRequest removes log records with malformed ids, copy-on-write
// like sanitizeTraceRequest.
func sanitizeLogsRequest(req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceRequest, int) {
	refused := 0
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, l := range sl.GetLogRecords() {
				if !validLogIDs(l) {
					refused++
				}
			}
		}
	}
	if refused == 0 {
		return req, 0
	}
	out := &collogspb.ExportLogsServiceRequest{}
	for _, rl := range req.GetResourceLogs() {
		nrl := &logspb.ResourceLogs{Resource: rl.GetResource(), SchemaUrl: rl.GetSchemaUrl()}
		for _, sl := range rl.GetScopeLogs() {
			nsl := &logspb.ScopeLogs{Scope: sl.GetScope(), SchemaUrl: sl.GetSchemaUrl()}
			for _, l := range sl.GetLogRecords() {
				if validLogIDs(l) {
					nsl.LogRecords = append(nsl.LogRecords, l)
				}
			}
			if len(nsl.LogRecords) > 0 {
				nrl.ScopeLogs = append(nrl.ScopeLogs, nsl)
			}
		}
		if len(nrl.ScopeLogs) > 0 {
			out.ResourceLogs = append(out.ResourceLogs, nrl)
		}
	}
	return out, refused
}

// invalidIDMessage is the bounded partial_success text for id refusals.
func invalidIDMessage(n int, what string) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d %s: malformed trace_id/span_id", n, what)
}
