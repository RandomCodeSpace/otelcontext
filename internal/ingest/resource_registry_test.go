package ingest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func registryResource(service string, extra ...*commonpb.KeyValue) *resourcepb.Resource {
	attrs := make([]*commonpb.KeyValue, 0, 1+len(extra))
	attrs = append(attrs, &commonpb.KeyValue{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}}})
	return &resourcepb.Resource{Attributes: append(attrs, extra...)}
}

func stringAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func registryTraceRequest(res *resourcepb.Resource) *coltracepb.ExportTraceServiceRequest {
	now := uint64(time.Now().UnixNano())
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: res,
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId: bytes.Repeat([]byte{0xA1}, 16), SpanId: bytes.Repeat([]byte{0x11}, 8),
			Name: "op", StartTimeUnixNano: now, EndTimeUnixNano: now + uint64(time.Millisecond),
		}}}},
	}}}
}

func registryLogsRequest(res *resourcepb.Resource) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: res,
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: uint64(time.Now().UnixNano()), SeverityText: "ERROR",
			SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR,
			Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "boom"}},
		}}}},
	}}}
}

func registryMetricsRequest(res *resourcepb.Resource) *colmetricspb.ExportMetricsServiceRequest {
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: res,
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: "cpu", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
				TimeUnixNano: uint64(time.Now().UnixNano()), Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: 1},
			}}}},
		}}}},
	}}}
}

func postRegistryProto(t *testing.T, url string, msg proto.Message) {
	t.Helper()
	body, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/x-protobuf", bytes.NewReader(body)) // #nosec G107 -- test server URL
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d", url, resp.StatusCode)
	}
}

func TestResourceRegistryRegistersAllSignalsOnBothTransports(t *testing.T) {
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	cfg := &config.Config{IngestMinSeverity: "DEBUG", DefaultTenant: "default", SamplingRate: 1}

	// Drop every span so registration is provably pre-sample.
	sampler := NewSampler(0, false, 1<<30)
	reg := topology.NewRegistry(nil)
	traces := NewTraceServer(repo, nil, cfg)
	traces.SetSampler(sampler)
	traces.SetResourceRegistry(reg)
	logs := NewLogsServer(repo, nil, cfg)
	logs.SetResourceRegistry(reg)
	metrics := NewMetricsServer(repo, nil, nil, cfg)
	metrics.SetResourceRegistry(reg)

	mux := http.NewServeMux()
	NewHTTPHandler(traces, logs, metrics).RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	hostRes := func(service string) *resourcepb.Resource {
		return registryResource(service, stringAttr("host.name", "node-a"), stringAttr("host.id", "i-123"), stringAttr("container.id", "ctr-1"))
	}
	ctx := context.Background()
	// gRPC transport: the Export methods are the gRPC service implementation.
	if _, err := traces.Export(ctx, registryTraceRequest(hostRes("grpc-svc"))); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.Export(ctx, registryLogsRequest(hostRes("grpc-svc"))); err != nil {
		t.Fatal(err)
	}
	if _, err := metrics.Export(ctx, registryMetricsRequest(hostRes("grpc-svc"))); err != nil {
		t.Fatal(err)
	}
	// HTTP transport.
	postRegistryProto(t, srv.URL+"/v1/traces", registryTraceRequest(hostRes("http-svc")))
	postRegistryProto(t, srv.URL+"/v1/logs", registryLogsRequest(hostRes("http-svc")))
	postRegistryProto(t, srv.URL+"/v1/metrics", registryMetricsRequest(hostRes("http-svc")))
	// No host attribute at all: the service registers with an empty host.
	if _, err := traces.Export(ctx, registryTraceRequest(registryResource("hostless"))); err != nil {
		t.Fatal(err)
	}

	snap := reg.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot = %#v", snap)
	}
	byService := make(map[string]topology.ResourceEntry, len(snap))
	for _, e := range snap {
		byService[e.Service] = e
	}
	all := topology.SignalTraces | topology.SignalLogs | topology.SignalMetrics
	for _, service := range []string{"grpc-svc", "http-svc"} {
		e, ok := byService[service]
		if !ok || e.Tenant != "default" || e.Host != "i-123" || e.Workload != "ctr-1" || e.Kind != "container" || e.Signals != all {
			t.Fatalf("%s entry = %#v", service, e)
		}
	}
	if e := byService["hostless"]; e.Service != "hostless" || e.Host != "" || e.Workload != "" || e.Kind != "" || e.Signals != topology.SignalTraces {
		t.Fatalf("host-less entry = %#v", e)
	}
}
