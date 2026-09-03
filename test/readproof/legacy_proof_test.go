//go:build readproof

package readproof

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
	collectlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collecttracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Legacy shape: five services, two days of exemplars, generated
// deterministically and exported over OTLP HTTP into the exact binary.
//
// Every trace is the same four-span call chain, one trace every
// legacyTraceInterval across the two days, with one log record on the payment
// span. Every 25th trace fails at payment so the error surfaces have data.
const (
	legacyDays          = 2
	legacyTraceInterval = 4 * time.Second
	legacyTraces        = legacyDays * 24 * 60 * 60 / 4
	legacySpansPerTrace = 4
	legacyBatchTraces   = 250
	legacyRCAService    = "payment"
	legacyIngestTimeout = 6 * time.Minute
	legacyDrainTimeout  = 5 * time.Minute
)

var legacyServices = []string{"frontend", "checkout", "payment", "inventory", "shipping"}

func TestReadLatencyLegacy(t *testing.T) {
	binary := requireBinary(t)
	started := time.Now()
	// Latency from #281; RSS steady p95 from #283 (legacy, bounded SQLite,
	// five services: 512 MiB).
	objectives := Objectives{Requests: 200, WarmP99MS: 300, ColdMS: 1000, RSSSteadyP95Bytes: 512 * MiB}
	proof := newProof(t, "legacy", binary, objectives)
	proof.Prefill = Prefill{Services: len(legacyServices), Days: legacyDays, Traces: legacyTraces, Spans: legacyTraces * legacySpansPerTrace, Logs: legacyTraces}

	dir := stateDir(t)
	app := newAppProcess(t, binary, dir, "legacy")
	proof.ServerEnv = app.env
	// The history spans the two days ending at ingest time; one extra window
	// of margin keeps the oldest trace inside the range.
	fullEnd := time.Now().UTC()
	fullStart := fullEnd.Add(-legacyDays*24*time.Hour - 5*time.Minute)
	plan := endpoints(app, legacyRCAService, fullStart, fullEnd)
	plan = append(plan, endpoint{&Measurement{Name: "rest_system_graph", Kind: "rest", Path: "/api/system/graph", Asserted: true}, app.restRequest("/api/system/graph")})
	for _, ep := range plan {
		proof.Measurements = append(proof.Measurements, ep.m)
	}
	defer writeProof(t, proof, started)

	if err := app.start(); err != nil {
		markUnmeasured(proof, "server start failed: "+err.Error())
		t.Fatalf("start: %v", err)
	}
	defer app.stop()
	sampler := startRSSSampler(app)
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelReady()
	if err := app.waitReady(readyCtx); err != nil {
		proof.RSS = sampler.finish(0)
		markUnmeasured(proof, "server never became ready")
		t.Fatalf("ready: %v", err)
	}
	proof.ReadySeconds = round3(time.Since(app.started).Seconds())

	prefillStarted := time.Now()
	ingestCtx, cancelIngest := context.WithTimeout(context.Background(), legacyIngestTimeout)
	defer cancelIngest()
	if err := exportLegacyHistory(ingestCtx, app, time.Now().UTC()); err != nil {
		proof.Prefill.Error = err.Error()
		proof.RSS = sampler.finish(0)
		markUnmeasured(proof, "legacy ingest failed: "+err.Error())
		t.Fatalf("ingest: %v", err)
	}
	if err := waitLegacyDrain(filepath.Join(dir, "otelcontext.db"), legacyDrainTimeout); err != nil {
		proof.Prefill.Error = err.Error()
		proof.RSS = sampler.finish(0)
		markUnmeasured(proof, "legacy ingest did not drain: "+err.Error())
		t.Fatalf("drain: %v", err)
	}
	proof.Prefill.Seconds = round3(time.Since(prefillStarted).Seconds())
	proof.Prefill.MainDBByte = fileSize(filepath.Join(dir, "otelcontext.db")) + fileSize(filepath.Join(dir, "otelcontext.db-wal"))
	t.Logf("ingest: %d traces / %d spans / %d logs in %.1f s, main db %d bytes", legacyTraces, legacyTraces*legacySpansPerTrace, legacyTraces, proof.Prefill.Seconds, proof.Prefill.MainDBByte)

	sampler.settle(time.Now(), settleLoad(plan))
	measureFrom := time.Since(app.started).Seconds()
	sampler.sample()
	for _, ep := range plan {
		measure(t, ep.m, ep.fn, objectives)
	}
	proof.Memory = app.account()
	proof.RSS = sampler.finish(measureFrom)
	logMemory(t, proof)
}

func legacyResource(service string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
		{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}}},
		{Key: "host.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "node-" + service}}},
	}}
}

func legacyTraceID(n int) []byte {
	id := make([]byte, 16)
	binary.BigEndian.PutUint64(id[8:], uint64(n)+1)
	id[0] = 0x7e
	return id
}

func legacySpanID(n, span int) []byte {
	id := make([]byte, 8)
	binary.BigEndian.PutUint64(id, uint64(n)*legacySpansPerTrace+uint64(span)+1)
	return id
}

// legacyShippingSpanID sets the high bit so it never collides with the
// four chain span IDs of any trace.
func legacyShippingSpanID(n int) []byte {
	id := legacySpanID(n, 0)
	id[0] |= 0x80
	return id
}

// legacyBatch builds traces [from, to) as one trace export and one log
// export. Trace n starts at end - (legacyTraces-n)*legacyTraceInterval, so the
// history ends just before `end` and reaches two days back.
func legacyBatch(from, to int, end time.Time) (*collecttracepb.ExportTraceServiceRequest, *collectlogspb.ExportLogsServiceRequest) {
	chain := []struct {
		service, operation string
		kind               tracepb.Span_SpanKind
		parent             int
	}{
		{service: "frontend", operation: "GET /checkout", kind: tracepb.Span_SPAN_KIND_SERVER, parent: -1},
		{service: "checkout", operation: "POST /orders", kind: tracepb.Span_SPAN_KIND_SERVER, parent: 0},
		{service: "payment", operation: "POST /charge", kind: tracepb.Span_SPAN_KIND_SERVER, parent: 1},
		{service: "inventory", operation: "POST /reserve", kind: tracepb.Span_SPAN_KIND_SERVER, parent: 1},
	}
	perService := make(map[string]*tracepb.ScopeSpans, len(legacyServices))
	traces := &collecttracepb.ExportTraceServiceRequest{}
	for _, service := range legacyServices {
		scope := &tracepb.ScopeSpans{}
		perService[service] = scope
		traces.ResourceSpans = append(traces.ResourceSpans, &tracepb.ResourceSpans{Resource: legacyResource(service), ScopeSpans: []*tracepb.ScopeSpans{scope}})
	}
	logs := &collectlogspb.ExportLogsServiceRequest{}
	logScope := &logspb.ScopeLogs{}
	logs.ResourceLogs = append(logs.ResourceLogs, &logspb.ResourceLogs{Resource: legacyResource("payment"), ScopeLogs: []*logspb.ScopeLogs{logScope}})
	shippingScope := perService["shipping"]

	for n := from; n < to; n++ {
		traceStart := end.Add(-time.Duration(legacyTraces-n) * legacyTraceInterval)
		failed := n%25 == 24
		for i, hop := range chain {
			start := traceStart.Add(time.Duration(i) * 2 * time.Millisecond)
			duration := time.Duration(8+(n*7+i*3)%40) * time.Millisecond
			status := &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
			if failed && hop.service == "payment" {
				status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "card declined"}
				duration += 120 * time.Millisecond
			}
			span := &tracepb.Span{
				TraceId: legacyTraceID(n), SpanId: legacySpanID(n, i),
				Name: hop.operation, Kind: hop.kind,
				StartTimeUnixNano: uint64(start.UnixNano()), EndTimeUnixNano: uint64(start.Add(duration).UnixNano()),
				Status: status,
				Attributes: []*commonpb.KeyValue{
					{Key: "http.method", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "POST"}}},
					{Key: "http.status_code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 200}}},
				},
			}
			if hop.parent >= 0 {
				span.ParentSpanId = legacySpanID(n, hop.parent)
			}
			perService[hop.service].Spans = append(perService[hop.service].Spans, span)
		}
		// Every fourth trace also reaches shipping, so the map has five nodes.
		if n%4 == 0 {
			start := traceStart.Add(9 * time.Millisecond)
			shippingScope.Spans = append(shippingScope.Spans, &tracepb.Span{
				TraceId: legacyTraceID(n), SpanId: legacyShippingSpanID(n),
				ParentSpanId: legacySpanID(n, 1), Name: "POST /ship", Kind: tracepb.Span_SPAN_KIND_SERVER,
				StartTimeUnixNano: uint64(start.UnixNano()), EndTimeUnixNano: uint64(start.Add(15 * time.Millisecond).UnixNano()),
				Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
			})
		}
		when := uint64(traceStart.Add(5 * time.Millisecond).UnixNano())
		severity, text, body := logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO", fmt.Sprintf("charge authorized for order %d amount %d", n, 1000+n%500)
		if failed {
			severity, text, body = logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "ERROR", fmt.Sprintf("charge declined for order %d: card declined", n)
		}
		logScope.LogRecords = append(logScope.LogRecords, &logspb.LogRecord{
			TimeUnixNano: when, ObservedTimeUnixNano: when,
			SeverityNumber: severity, SeverityText: text,
			Body:    &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
			TraceId: legacyTraceID(n), SpanId: legacySpanID(n, 2),
		})
	}
	return traces, logs
}

func exportLegacyHistory(ctx context.Context, app *appProcess, end time.Time) error {
	for from := 0; from < legacyTraces; from += legacyBatchTraces {
		to := from + legacyBatchTraces
		if to > legacyTraces {
			to = legacyTraces
		}
		traces, logs := legacyBatch(from, to, end)
		if err := postProto(ctx, app.baseURL()+"/v1/traces", traces); err != nil {
			return fmt.Errorf("traces %d..%d: %w", from, to, err)
		}
		if err := postProto(ctx, app.baseURL()+"/v1/logs", logs); err != nil {
			return fmt.Errorf("logs %d..%d: %w", from, to, err)
		}
	}
	return nil
}

// postProto exports one OTLP request, honouring 429 + Retry-After from the
// async pipeline the way a collector would.
func postProto(ctx context.Context, endpoint string, message proto.Message) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/x-protobuf")
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		switch response.StatusCode {
		case http.StatusOK:
			return nil
		case http.StatusTooManyRequests:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		default:
			return fmt.Errorf("POST %s status=%d body=%s", endpoint, response.StatusCode, tail(string(payload), 300))
		}
	}
}

// waitLegacyDrain polls the main database until every exported row has been
// persisted by the async pipeline.
func waitLegacyDrain(dbPath string, timeout time.Duration) error {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	wantSpans := int64(legacyTraces*legacySpansPerTrace + (legacyTraces+3)/4)
	deadline := time.Now().Add(timeout)
	var spans, logs, traces int64
	for time.Now().Before(deadline) {
		_ = db.QueryRow("SELECT COUNT(*) FROM spans").Scan(&spans)
		_ = db.QueryRow("SELECT COUNT(*) FROM logs").Scan(&logs)
		_ = db.QueryRow("SELECT COUNT(*) FROM traces").Scan(&traces)
		if spans >= wantSpans && logs >= legacyTraces && traces >= legacyTraces {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("after %s: traces=%d/%d spans=%d/%d logs=%d/%d", timeout, traces, legacyTraces, spans, wantSpans, logs, legacyTraces)
}
