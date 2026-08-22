package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/tsdb"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// #194 finding 10: in AGGREGATE_MODE=aggregate main.go constructs neither the
// TSDB aggregator nor the metric callback, and Export must then stop building
// tsdb.RawMetric values. RawMetric is not just a struct copy — it carries a
// per-point attribute map, which at 120 services is the largest remaining
// allocator on the metric path.
//
// The assertion is per-point allocation GROWTH rather than an absolute count:
// the fixed cost of one Export (response struct, tenant resolution) is
// irrelevant and would make the test a brittle transcription of the current
// implementation.

// gaugeMetricPoints builds one gauge metric carrying n data points, each with
// two attributes so a RawMetric build has a map to populate.
func gaugeMetricPoints(name string, n int, ts time.Time) *metricspb.Metric {
	points := make([]*metricspb.NumberDataPoint, 0, n)
	for i := 0; i < n; i++ {
		points = append(points, &metricspb.NumberDataPoint{
			TimeUnixNano: uint64(ts.UnixNano()), // #nosec G115 -- test timestamps are positive
			Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: float64(i)},
			Attributes: []*commonpb.KeyValue{
				{Key: "http.route", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "/cart"}}},
				{Key: "http.method", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "GET"}}},
			},
		})
	}
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: points}},
	}
}

// exportAllocsPerPoint returns the mean allocations of one Export of a gauge
// with n points.
func exportAllocsPerPoint(t *testing.T, s *MetricsServer, n int) float64 {
	t.Helper()
	ctx := context.Background()
	req := metricsRequest(gaugeMetricPoints("queue.depth", n, time.Now()))
	// Warm any lazily-built state (tenant interning, service list parse) so it
	// is not charged to the measured runs.
	if _, err := s.Export(ctx, req); err != nil {
		t.Fatalf("Export warmup: %v", err)
	}
	return testing.AllocsPerRun(20, func() {
		if _, err := s.Export(ctx, req); err != nil {
			t.Fatalf("Export: %v", err)
		}
	})
}

func TestExportBuildsNoRawMetricWithoutLegacyConsumers(t *testing.T) {
	const (
		few  = 4
		many = 260
	)

	// Aggregate mode's wiring: no TSDB aggregator, no metric callback.
	quiet := NewMetricsServer(nil, nil, nil, aggTestConfig())
	small := exportAllocsPerPoint(t, quiet, few)
	large := exportAllocsPerPoint(t, quiet, many)
	if large > small {
		t.Errorf("no-consumer Export allocates per data point: %d points = %.1f allocs, %d points = %.1f allocs",
			few, small, many, large)
	}

	// Legacy wiring: the callback alone is enough to require the RawMetric.
	loud := NewMetricsServer(nil, nil, nil, aggTestConfig())
	var seen int
	loud.SetMetricCallback(func(m tsdb.RawMetric) {
		seen++
		if m.Attributes["http.route"] == nil {
			t.Errorf("RawMetric lost its attributes: %+v", m)
		}
	})
	loudSmall := exportAllocsPerPoint(t, loud, few)
	loudLarge := exportAllocsPerPoint(t, loud, many)
	if seen == 0 {
		t.Fatal("metric callback was never invoked; the legacy comparison is meaningless")
	}
	if growth := loudLarge - loudSmall; growth < float64(many-few) {
		t.Errorf("legacy Export should allocate at least one object per extra data point, got %.1f for %d extra points",
			growth, many-few)
	}
}
