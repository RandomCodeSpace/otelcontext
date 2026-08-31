package telemetry

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/latency"
	"github.com/prometheus/client_golang/prometheus"
)

func testLatencyMetrics() *Metrics {
	return &Metrics{
		DBLatency: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "test_db_latency_seconds"}),
		startTime: time.Now(),
	}
}

func TestDBLatencyObservationBoundaries(t *testing.T) {
	for _, count := range []int{0, 1, 99, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			m := testLatencyMetrics()
			for i := 0; i < count; i++ {
				m.ObserveDBLatency(float64(i+1) / 1000)
			}
			stats := m.GetHealthStats()
			p99 := stats.LatencyProvenance.P99
			if count == 0 {
				if stats.DBLatencyP99Ms != 0 || p99.Status != latency.StatusUnavailable || p99.Reason != latency.ReasonNoObservations {
					t.Fatalf("empty stats=%+v provenance=%+v", stats, p99)
				}
				return
			}
			if p99.Status != latency.StatusMeasured || p99.Method != latency.MethodRollingObservationWindow || p99.SampleCount != uint64(count) || p99.LowSample != (count < 100) {
				t.Fatalf("count=%d provenance=%+v", count, p99)
			}
			if stats.DBLatencyLastMs != float64(count) {
				t.Fatalf("last=%v, want %d", stats.DBLatencyLastMs, count)
			}
		})
	}
}

func TestDBLatencyRingBecomesBoundedAfterWrap(t *testing.T) {
	m := testLatencyMetrics()
	for i := 0; i < 1025; i++ {
		m.ObserveDBLatency(float64(i+1) / 1000)
	}
	stats := m.GetHealthStats()
	p99 := stats.LatencyProvenance.P99
	if p99.Status != latency.StatusBounded || p99.SampleCount != 1024 || p99.PopulationCount != 1025 || p99.SampleLimit != 1024 {
		t.Fatalf("provenance=%+v", p99)
	}
	if stats.DBLatencyLastMs != 1025 {
		t.Fatalf("last=%v, want 1025", stats.DBLatencyLastMs)
	}
	if math.Abs(stats.DBLatencyP99Ms-1015) > 1e-9 {
		t.Fatalf("p99=%v, want nearest-rank 1015", stats.DBLatencyP99Ms)
	}
}
