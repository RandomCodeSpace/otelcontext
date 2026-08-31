package aggregate

import (
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

func TestPrometheusRecordersExposeExplicitZeroDropSeries(t *testing.T) {
	metrics := telemetry.New()
	NewPrometheusRecorder(metrics)
	NewPrometheusStoreMetrics(metrics)

	for name, tc := range map[string]struct {
		collector prometheus.Collector
		want      int
	}{
		"late points":        {metrics.AggregateLatePointsTotal, 8},
		"identity overflow":  {metrics.AggregateIdentityOverflowTotal, 16},
		"admission rejected": {metrics.AggregateAdmissionRejectedTotal, 3},
	} {
		if got := collectedMetricCount(tc.collector); got != tc.want {
			t.Errorf("%s child series = %d, want %d explicit zero series", name, got, tc.want)
		}
	}
}

func collectedMetricCount(collector prometheus.Collector) int {
	metrics := make(chan prometheus.Metric, 64)
	collector.Collect(metrics)
	close(metrics)
	return len(metrics)
}
