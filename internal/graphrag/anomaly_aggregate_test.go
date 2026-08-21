package graphrag

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// baselineWindows builds n finalized windows each carrying count requests and
// errors errors, ending just before the current window.
func baselineWindows(base time.Time, n int, count, errors uint64, p99 float64) []aggregate.TopologyWindow {
	out := make([]aggregate.TopologyWindow, 0, n)
	for i := n; i > 0; i-- {
		out = append(out, aggWindow(base.Add(-time.Duration(i)*aggregate.WindowSize), count, errors, p99))
	}
	return out
}

// openWindow builds the still-filling current window.
func openWindow(start time.Time, elapsed time.Duration, count, errors uint64, p99 float64) aggregate.TopologyWindow {
	return aggregate.TopologyWindow{
		Start:             start,
		End:               start.Add(aggregate.WindowSize),
		Closed:            false,
		Final:             false,
		Elapsed:           elapsed,
		Count:             count,
		ErrorCount:        errors,
		DurationCount:     count,
		DurationSumMicros: float64(count) * 1000,
		P95Micros:         p99 * 0.8,
		P99Micros:         p99,
	}
}

// runDetector installs snap as the tenant's topology and runs one aggregate
// detection pass, returning the anomalies it produced.
func runDetector(t *testing.T, snap aggregate.TopologySnapshot) []*AnomalyNode {
	t.Helper()
	src := &fakeAggregateSource{epoch: 1, snaps: map[string]aggregate.TopologySnapshot{}}
	g := aggregateGraphRAG(t, newTestRepo(t), src)
	stores := g.storesForTenant(storage.DefaultTenantID)
	stores.lastTopology.Store(&snap)
	g.detectAnomaliesFromTopology(context.Background(), storage.DefaultTenantID, stores)
	return stores.anomalies.AnomaliesSince(time.Now().Add(-time.Hour))
}

func serviceSnapshot(revision uint64, windows []aggregate.TopologyWindow) aggregate.TopologySnapshot {
	return aggregate.TopologySnapshot{
		Tenant:   storage.DefaultTenantID,
		Revision: revision,
		Services: []aggregate.TopologyService{{Name: "checkout", Windows: windows}},
	}
}

func findAnomaly(anomalies []*AnomalyNode, kind AnomalyType) *AnomalyNode {
	for _, a := range anomalies {
		if a.Type == kind {
			return a
		}
	}
	return nil
}

// TestErrorSpikeFiresAgainstFinalizedBaseline proves the detector compares a
// window's error rate against the PRIOR FINALIZED windows and fires when the
// excess clears three standard errors — with no 2% constant anywhere.
func TestErrorSpikeFiresAgainstFinalizedBaseline(t *testing.T) {
	now := time.Now().UTC().Truncate(aggregate.WindowSize)
	windows := baselineWindows(now, 4, 1000, 10, 3000) // 1% baseline
	windows = append(windows, aggWindow(now, 500, 100, 3000))

	got := runDetector(t, serviceSnapshot(1, windows))
	a := findAnomaly(got, AnomalyErrorSpike)
	if a == nil {
		t.Fatalf("no error-spike anomaly for 20%% vs a 1%% baseline: %+v", got)
	}
	if a.Service != "checkout" {
		t.Fatalf("anomaly service = %q", a.Service)
	}
	if a.Evidence == "" {
		t.Fatalf("anomaly carries no evidence")
	}
}

// TestErrorSpikeRespectsMinimumRequestCount proves a window too small to mean
// anything cannot fire, no matter how bad its rate looks.
func TestErrorSpikeRespectsMinimumRequestCount(t *testing.T) {
	now := time.Now().UTC().Truncate(aggregate.WindowSize)
	windows := baselineWindows(now, 4, 1000, 10, 3000)
	windows = append(windows, aggWindow(now, minWindowRequests-1, minWindowRequests-1, 3000)) // 100% errors

	if got := runDetector(t, serviceSnapshot(1, windows)); findAnomaly(got, AnomalyErrorSpike) != nil {
		t.Fatalf("a %d-request window fired an anomaly: %+v", minWindowRequests-1, got)
	}
}

// TestPartialWindowGuardSuppressesNearEmptyWindow proves a window that has
// barely opened is not compared to anything — the "anomaly storm from a nearly
// empty window" this replaces.
func TestPartialWindowGuardSuppressesNearEmptyWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(aggregate.WindowSize)
	windows := baselineWindows(now, 4, 1000, 10, 3000)
	windows = append(windows, openWindow(now, 5*time.Second, 400, 200, 3000))

	if got := runDetector(t, serviceSnapshot(1, windows)); findAnomaly(got, AnomalyErrorSpike) != nil {
		t.Fatalf("a 5s-old window fired an anomaly: %+v", got)
	}

	// The same window, once enough of it has elapsed, does fire.
	windows[len(windows)-1] = openWindow(now, 3*time.Minute, 400, 200, 3000)
	if got := runDetector(t, serviceSnapshot(1, windows)); findAnomaly(got, AnomalyErrorSpike) == nil {
		t.Fatalf("a 3m-old window with 50%% errors did not fire: %+v", got)
	}
}

// TestNoBaselineNoAnomaly proves the detector refuses to invent a baseline: a
// service with too little finalized history produces nothing.
func TestNoBaselineNoAnomaly(t *testing.T) {
	now := time.Now().UTC().Truncate(aggregate.WindowSize)
	windows := []aggregate.TopologyWindow{
		aggWindow(now.Add(-aggregate.WindowSize), 30, 0, 3000),
		aggWindow(now, 500, 400, 3000),
	}
	if got := runDetector(t, serviceSnapshot(1, windows)); len(got) != 0 {
		t.Fatalf("anomalies produced without a baseline: %+v", got)
	}
}

// TestLatencySpikeUsesSketchPercentiles proves latency detection reads p99 and
// not an average: a window whose AVERAGE is unremarkable but whose p99 has
// blown out still fires, and the reverse does not.
func TestLatencySpikeUsesSketchPercentiles(t *testing.T) {
	now := time.Now().UTC().Truncate(aggregate.WindowSize)
	windows := baselineWindows(now, 4, 1000, 0, 20_000) // p99 20ms
	spike := aggWindow(now, 1000, 0, 400_000)           // p99 400ms
	// Average duration is identical to the baseline windows by construction.
	windows = append(windows, spike)

	got := runDetector(t, serviceSnapshot(1, windows))
	if findAnomaly(got, AnomalyLatencySpike) == nil {
		t.Fatalf("p99 20ms -> 400ms did not fire: %+v", got)
	}

	// A steady p99 does not fire even at an absolute value the retired
	// avg>500ms rule would have flagged.
	steady := baselineWindows(now, 4, 1000, 0, 900_000)
	steady = append(steady, aggWindow(now, 1000, 0, 900_000))
	if got := runDetector(t, serviceSnapshot(1, steady)); findAnomaly(got, AnomalyLatencySpike) != nil {
		t.Fatalf("a steady 900ms p99 fired: %+v", got)
	}
}

// TestMetricZScoreUsesRollingMeanAndVariance proves the metric detector is a
// real z-score over finalized window means, and that a baseline with no spread
// produces nothing rather than dividing by zero.
func TestMetricZScoreUsesRollingMeanAndVariance(t *testing.T) {
	now := time.Now().UTC().Truncate(aggregate.WindowSize)
	mkMetric := func(means []float64, current float64) aggregate.TopologySnapshot {
		windows := make([]aggregate.TopologyWindow, 0, len(means)+1)
		for i, m := range means {
			windows = append(windows, aggregate.TopologyWindow{
				Start:      now.Add(-time.Duration(len(means)-i) * aggregate.WindowSize),
				Final:      true,
				Closed:     true,
				ValueCount: 10,
				ValueSum:   m * 10,
			})
		}
		windows = append(windows, aggregate.TopologyWindow{
			Start: now, Closed: true, Final: false, Elapsed: aggregate.WindowSize,
			ValueCount: 10, ValueSum: current * 10,
		})
		return aggregate.TopologySnapshot{
			Tenant:   storage.DefaultTenantID,
			Revision: 1,
			Metrics: []aggregate.TopologyMetric{{
				Service: "checkout", Metric: "queue_depth", Windows: windows,
			}},
		}
	}

	got := runDetector(t, mkMetric([]float64{10, 11, 9, 10, 11}, 90))
	if findAnomaly(got, AnomalyMetricZScore) == nil {
		t.Fatalf("a 9-sigma metric excursion did not fire: %+v", got)
	}

	// Zero variance: no spread means no z-score, not an infinite one.
	if got := runDetector(t, mkMetric([]float64{10, 10, 10, 10}, 10)); findAnomaly(got, AnomalyMetricZScore) != nil {
		t.Fatalf("a flat baseline fired a z-score anomaly: %+v", got)
	}
}

// TestAggregateDetectionIsRevisionGated proves an unchanged topology revision
// does no detection work at all.
func TestAggregateDetectionIsRevisionGated(t *testing.T) {
	now := time.Now().UTC().Truncate(aggregate.WindowSize)
	windows := baselineWindows(now, 4, 1000, 10, 3000)
	windows = append(windows, aggWindow(now, 500, 100, 3000))
	snap := serviceSnapshot(5, windows)

	src := &fakeAggregateSource{epoch: 1, snaps: map[string]aggregate.TopologySnapshot{}}
	g := aggregateGraphRAG(t, newTestRepo(t), src)
	stores := g.storesForTenant(storage.DefaultTenantID)
	stores.lastTopology.Store(&snap)

	ctx := context.Background()
	g.detectAnomaliesFromTopology(ctx, storage.DefaultTenantID, stores)
	first := findAnomaly(stores.anomalies.AnomaliesSince(now.Add(-time.Hour)), AnomalyErrorSpike)
	if first == nil {
		t.Fatalf("first pass produced no anomaly")
	}
	stamp := first.Timestamp

	// Second pass at the same revision must not re-stamp the anomaly.
	time.Sleep(2 * time.Millisecond)
	g.detectAnomaliesFromTopology(ctx, storage.DefaultTenantID, stores)
	again := findAnomaly(stores.anomalies.AnomaliesSince(now.Add(-time.Hour)), AnomalyErrorSpike)
	if !again.Timestamp.Equal(stamp) {
		t.Fatalf("unchanged revision re-ran detection: %v -> %v", stamp, again.Timestamp)
	}
}
