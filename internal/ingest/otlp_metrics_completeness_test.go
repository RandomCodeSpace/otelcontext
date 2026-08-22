package ingest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// #199 Q5: the OTLP metrics Export tells the client exactly how many data
// points it refused. The shared builders keep every case to its own payload.

// metricsExportServer returns a MetricsServer wired to a shadow-mode aggregate
// engine and nothing else: these tests are about the aggregate path's response,
// not about the TSDB or the DB.
func metricsExportServer(t *testing.T, now time.Time) *MetricsServer {
	t.Helper()
	s := NewMetricsServer(nil, nil, nil, aggTestConfig())
	s.SetAggregateEngine(newAggregateEngine(t, now))
	return s
}

// metricsRequest wraps metrics in the one resource these tests use.
func metricsRequest(metrics ...*metricspb.Metric) *colmetricspb.ExportMetricsServiceRequest {
	return &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}},
			}}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: metrics}},
		}},
	}
}

// histogramMetric builds an explicit-bounds Histogram metric with one point.
func histogramMetric(name string, temporality metricspb.AggregationTemporality, ts time.Time, counts []uint64) *metricspb.Metric {
	var total uint64
	for _, c := range counts {
		total += c
	}
	sum := 42.0
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
			AggregationTemporality: temporality,
			DataPoints: []*metricspb.HistogramDataPoint{{
				TimeUnixNano:   uint64(ts.UnixNano()), // #nosec G115 -- test timestamps are positive
				Count:          total,
				Sum:            &sum,
				ExplicitBounds: []float64{1, 2, 4},
				BucketCounts:   counts,
			}},
		}},
	}
}

// expHistogramMetric builds an ExponentialHistogram metric with one point.
func expHistogramMetric(name string, scale int32, ts time.Time, counts []uint64) *metricspb.Metric {
	var total uint64
	for _, c := range counts {
		total += c
	}
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			DataPoints: []*metricspb.ExponentialHistogramDataPoint{{
				TimeUnixNano: uint64(ts.UnixNano()), // #nosec G115 -- test timestamps are positive
				Count:        total,
				Scale:        scale,
				Positive:     &metricspb.ExponentialHistogramDataPoint_Buckets{Offset: 0, BucketCounts: counts},
			}},
		}},
	}
}

// summaryMetric builds a Summary metric with n points.
func summaryMetric(name string, ts time.Time, n int) *metricspb.Metric {
	points := make([]*metricspb.SummaryDataPoint, n)
	for i := range points {
		points[i] = &metricspb.SummaryDataPoint{
			TimeUnixNano: uint64(ts.UnixNano()), // #nosec G115 -- test timestamps are positive
			Count:        7,
		}
	}
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: points}},
	}
}

// TestExportReportsRejectedDataPoints pins #199 Q5 end to end: Summary,
// cumulative Histogram and a malformed ExponentialHistogram are counted
// exactly, named in the message, and reported alongside a SUCCESSFUL export of
// the accepted points.
func TestExportReportsRejectedDataPoints(t *testing.T) {
	now := time.Now().UTC()
	s := metricsExportServer(t, now)

	// count disagrees with the bucket totals: malformed.
	malformed := expHistogramMetric("bad.exp", 4, now, []uint64{1, 2})
	malformed.GetExponentialHistogram().DataPoints[0].Count = 99

	resp, err := s.Export(context.Background(), metricsRequest(
		histogramMetric("ok.hist", metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, now, []uint64{0, 5, 7, 0}),
		histogramMetric("cumulative.hist", metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, now, []uint64{0, 5, 7, 0}),
		malformed,
		summaryMetric("legacy.summary", now, 2),
	))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if resp.PartialSuccess == nil {
		t.Fatal("no partial success reported; three point types were refused")
	}
	if got := resp.PartialSuccess.RejectedDataPoints; got != 4 {
		t.Errorf("rejected_data_points = %d, want 4 (1 cumulative + 1 malformed + 2 summary)", got)
	}
	msg := resp.PartialSuccess.ErrorMessage
	for _, want := range []string{
		"summary/" + aggregate.ReasonUnsupportedType,
		"histogram/" + aggregate.ReasonCumulativeTemporality,
		"exponential_histogram/" + aggregate.ReasonMalformedPoint,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not name %q", msg, want)
		}
	}
}

// TestExportWithoutRejectionsLeavesPartialSuccessUnset pins the other half of
// Q5: a zero rejected count is reserved for warning-only responses, so a clean
// metrics export must not fabricate one.
func TestExportWithoutRejectionsLeavesPartialSuccessUnset(t *testing.T) {
	now := time.Now().UTC()
	s := metricsExportServer(t, now)

	resp, err := s.Export(context.Background(), metricsRequest(
		histogramMetric("ok.hist", metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, now, []uint64{0, 5, 7, 0}),
		expHistogramMetric("ok.exp", 6, now, []uint64{3, 4, 5}),
	))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if resp.PartialSuccess != nil {
		t.Fatalf("partial success set on a clean export: %+v", resp.PartialSuccess)
	}
}

// TestExportSkipsNoRecordedValuePoints pins that a point flagged
// NO_RECORDED_VALUE is neither aggregated nor reported as a rejection: the
// producer is stating there was nothing to record.
func TestExportSkipsNoRecordedValuePoints(t *testing.T) {
	now := time.Now().UTC()
	s := metricsExportServer(t, now)

	m := histogramMetric("gap.hist", metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, now, []uint64{0, 0, 0, 0})
	m.GetHistogram().DataPoints[0].Flags = otlpDataPointNoRecordedValue

	resp, err := s.Export(context.Background(), metricsRequest(m))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if resp.PartialSuccess != nil {
		t.Fatalf("partial success set for a no-recorded-value point: %+v", resp.PartialSuccess)
	}
}

// TestExportInLegacyModeIgnoresDistributionPoints pins the mode boundary: with
// no aggregate engine wired there is no aggregate accounting to be honest
// about, and the response stays exactly what it was before #199.
func TestExportInLegacyModeIgnoresDistributionPoints(t *testing.T) {
	now := time.Now().UTC()
	s := NewMetricsServer(nil, nil, nil, aggTestConfig())

	resp, err := s.Export(context.Background(), metricsRequest(
		summaryMetric("legacy.summary", now, 2),
		histogramMetric("cumulative.hist", metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, now, []uint64{0, 1, 0, 0}),
	))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if resp.PartialSuccess != nil {
		t.Fatalf("legacy mode reported partial success: %+v", resp.PartialSuccess)
	}
}

// TestRejectionMessageIsBounded pins that the message names at most
// maxRejectionReasons pairs while the COUNT stays exact.
func TestRejectionMessageIsBounded(t *testing.T) {
	var m metricRejections
	for i := 0; i < maxRejectionReasons+3; i++ {
		m.add(pointTypeHistogram, string(rune('a'+i)), uint64(i+1))
	}
	msg := m.message()
	if !strings.Contains(msg, "more reasons") {
		t.Errorf("message %q did not truncate", msg)
	}
	if strings.Count(msg, pointTypeHistogram+"/") != maxRejectionReasons {
		t.Errorf("message named %d reasons, want %d", strings.Count(msg, pointTypeHistogram+"/"), maxRejectionReasons)
	}
	var want uint64
	for i := 0; i < maxRejectionReasons+3; i++ {
		want += uint64(i + 1)
	}
	if m.total != want {
		t.Errorf("total = %d, want %d", m.total, want)
	}
}
