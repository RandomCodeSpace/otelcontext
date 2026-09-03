package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
	"github.com/RandomCodeSpace/otelcontext/internal/tsdb"
	"github.com/prometheus/client_golang/prometheus"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// #280: hostmetrics carry host.name and no service.name. They used to land
// under unknown-service and merge across hosts; they now land under
// host/<name>, one entity per host, in legacy and aggregate modes alike.

const (
	hostCPUTime        = "system.cpu.time"
	hostCPUUtilization = "system.cpu.utilization"
)

// hostmetricsResource mimics one hostmetrics-receiver resource batch: a
// cumulative monotonic Sum and a Gauge, per-cpu point attributes, and only
// the given resource attributes.
func hostmetricsResource(ts time.Time, attrs ...*commonpb.KeyValue) *metricspb.ResourceMetrics {
	point := func(v float64) *metricspb.NumberDataPoint {
		return &metricspb.NumberDataPoint{
			StartTimeUnixNano: uint64(ts.Add(-time.Hour).UnixNano()), // #nosec G115 -- test timestamps are positive
			TimeUnixNano:      uint64(ts.UnixNano()),                 // #nosec G115 -- test timestamps are positive
			Value:             &metricspb.NumberDataPoint_AsDouble{AsDouble: v},
			Attributes:        []*commonpb.KeyValue{stringAttr("cpu", "cpu0"), stringAttr("state", "user")},
		}
	}
	return &metricspb.ResourceMetrics{
		Resource: &resourcepb.Resource{Attributes: attrs},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: hostCPUTime, Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				IsMonotonic:            true,
				DataPoints:             []*metricspb.NumberDataPoint{point(1234.5)},
			}}},
			{Name: hostCPUUtilization, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{point(0.42)},
			}}},
		}}},
	}
}

func hostmetricsRequest(ts time.Time, hosts ...string) *colmetricspb.ExportMetricsServiceRequest {
	req := &colmetricspb.ExportMetricsServiceRequest{}
	for _, h := range hosts {
		req.ResourceMetrics = append(req.ResourceMetrics, hostmetricsResource(ts, stringAttr("host.name", h)))
	}
	return req
}

// legacyServiceNames exports req through a legacy-wired MetricsServer and
// returns the RawMetric count per service name.
func legacyServiceNames(t *testing.T, req *colmetricspb.ExportMetricsServiceRequest) map[string]int {
	t.Helper()
	srv := NewMetricsServer(nil, nil, nil, aggTestConfig())
	seen := make(map[string]int)
	srv.SetMetricCallback(func(m tsdb.RawMetric) { seen[m.ServiceName]++ })
	if _, err := srv.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}
	return seen
}

func TestHostOnlyResourceLandsUnderHostEntityLegacy(t *testing.T) {
	seen := legacyServiceNames(t, hostmetricsRequest(time.Now(), "node-a", "node-b"))
	want := map[string]int{topology.HostPrefix + "node-a": 2, topology.HostPrefix + "node-b": 2}
	if len(seen) != len(want) {
		t.Fatalf("services = %v, want %v", seen, want)
	}
	for svc, n := range want {
		if seen[svc] != n {
			t.Errorf("service %q saw %d metrics, want %d (all: %v)", svc, seen[svc], n, seen)
		}
	}
}

func TestHostIdentityResolution(t *testing.T) {
	cases := []struct {
		name  string
		attrs []*commonpb.KeyValue
		want  string
	}{
		{"service.name wins over host", []*commonpb.KeyValue{stringAttr("host.name", "node-a"), stringAttr("service.name", "checkout")}, "checkout"},
		{"host.name preferred over host.id", []*commonpb.KeyValue{stringAttr("host.id", "i-0abc"), stringAttr("host.name", "node-a")}, topology.HostPrefix + "node-a"},
		{"host.id when no host.name", []*commonpb.KeyValue{stringAttr("host.id", "i-0abc")}, topology.HostPrefix + "i-0abc"},
		{"neither keeps unknown-service", []*commonpb.KeyValue{stringAttr("os.type", "linux")}, "unknown-service"},
		{"empty resource keeps unknown-service", nil, "unknown-service"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{hostmetricsResource(time.Now(), c.attrs...)}}
			seen := legacyServiceNames(t, req)
			if len(seen) != 1 || seen[c.want] != 2 {
				t.Fatalf("services = %v, want %q x2", seen, c.want)
			}
		})
	}
}

// hostServices returns the set of services the aggregate topology carries
// for metric name.
func hostServices(snap aggregate.TopologySnapshot, name string) map[string]bool {
	out := make(map[string]bool)
	for _, m := range snap.Metrics {
		if m.Metric == name {
			out[m.Service] = true
		}
	}
	return out
}

func TestHostOnlyResourceLandsUnderHostEntityAggregate(t *testing.T) {
	now := time.Now().UTC()
	engine := ackEngine(t, aggregate.ModeAggregate, now)
	srv := NewMetricsServer(nil, nil, nil, aggTestConfig())
	srv.SetAggregateEngine(engine)

	if _, err := srv.Export(context.Background(), hostmetricsRequest(now, "node-a", "node-b")); err != nil {
		t.Fatalf("Export: %v", err)
	}

	snap := engine.TopologySnapshot(storage.DefaultTenantID)
	got := hostServices(snap, hostCPUUtilization)
	if len(got) != 2 || !got[topology.HostPrefix+"node-a"] || !got[topology.HostPrefix+"node-b"] {
		t.Fatalf("%s services = %v, want host/node-a and host/node-b", hostCPUUtilization, got)
	}
	for _, m := range snap.Metrics {
		if m.Service == "unknown-service" {
			t.Fatalf("metric %q still lands under unknown-service", m.Metric)
		}
	}
}

// hostDimsEngine builds an aggregate-mode engine with the given
// AGGREGATE_METRIC_DIMS-shaped config.
func hostDimsEngine(t *testing.T, now time.Time, dims aggregate.DimsConfig) *aggregate.Engine {
	t.Helper()
	e, err := aggregate.NewEngine(aggregate.EngineConfig{
		Mode:       aggregate.ModeAggregate,
		Now:        func() time.Time { return now },
		MetricDims: dims,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

// dimsIDsPerSeries exports two hosts through an engine built with dims and
// returns the DimsID of every active metric series.
func dimsIDsPerSeries(t *testing.T, dims aggregate.DimsConfig) []uint32 {
	t.Helper()
	now := time.Now().UTC()
	engine := hostDimsEngine(t, now, dims)
	srv := NewMetricsServer(nil, nil, nil, aggTestConfig())
	srv.SetAggregateEngine(engine)
	if _, err := srv.Export(context.Background(), hostmetricsRequest(now, "node-a", "node-b")); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var ids []uint32
	for key := range engine.ActiveSeriesKeys() {
		if key.Signal == aggregate.SignalMetric {
			ids = append(ids, key.DimsID)
		}
	}
	return ids
}

// TestHostDimsSplitPerHostFromResource: with
// AGGREGATE_METRIC_DIMS=system.cpu.utilization:host.name the point lacks
// host.name and the resource supplies it, so the gauge series carry one
// distinct non-zero DimsID per host. Without the config every series is
// DimsID 0.
func TestHostDimsSplitPerHostFromResource(t *testing.T) {
	withDims := dimsIDsPerSeries(t, aggregate.DimsConfig{hostCPUUtilization: {"host.name"}})
	distinct := make(map[uint32]bool)
	for _, id := range withDims {
		if id != 0 {
			distinct[id] = true
		}
	}
	if len(distinct) != 2 {
		t.Fatalf("configured host.name dim produced DimsIDs %v, want two distinct non-zero values (one per host)", withDims)
	}

	for _, id := range dimsIDsPerSeries(t, nil) {
		if id != 0 {
			t.Fatalf("no configured dims yet a series carries DimsID %d", id)
		}
	}
}

func TestReservedHostPrefixCounted(t *testing.T) {
	m := &telemetry.Metrics{
		IngestionRate: prometheus.NewCounter(prometheus.CounterOpts{Name: "test_ingestion_rate_reserved_prefix"}),
		GRPCBatchSize: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "test_grpc_batch_size_reserved_prefix"}),
		IngestReservedServicePrefixTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_ingest_reserved_service_prefix_total",
		}, []string{"signal"}),
	}
	srv := NewMetricsServer(nil, m, nil, aggTestConfig())
	seen := make(map[string]int)
	srv.SetMetricCallback(func(rm tsdb.RawMetric) { seen[rm.ServiceName]++ })

	now := time.Now()
	req := &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{
		hostmetricsResource(now, stringAttr("service.name", "host/minted"), stringAttr("host.name", "node-a")),
		hostmetricsResource(now, stringAttr("host.name", "node-a")),
		hostmetricsResource(now, stringAttr("service.name", "checkout")),
	}}
	if _, err := srv.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if seen["host/minted"] != 2 {
		t.Fatalf("client-declared host/ name was not accepted as sent: %v", seen)
	}
	if got := counterAt(t, m.IngestReservedServicePrefixTotal, "metrics"); got != 1 {
		t.Fatalf("reserved prefix counter = %v, want 1 (only the client-declared host/ name counts)", got)
	}
}
