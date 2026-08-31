package graphrag

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
)

// Aggregate-mode anomaly detection (#163's "source change plus math
// correction", implemented by #174).
//
// The legacy detector hard-coded a 2% error baseline, thresholded on average
// latency above 500 ms, and called a min/max range calculation a z-score. None
// of that survives here. What replaces it:
//
//   - reads happen only when the topology revision has moved;
//   - error detection compares a window's error RATE against the rate of the
//     prior FINALIZED windows, and fires only when the excess clears three
//     standard errors of that baseline proportion — so a baseline of 0.4% and
//     a baseline of 12% both behave correctly, and neither is written down
//     anywhere;
//   - latency detection reads p95/p99 out of the window's sketch, never an
//     average;
//   - metric anomalies use a real rolling mean and variance over a bounded
//     finalized-window baseline;
//   - the current, still-filling window has to clear an elapsed-time AND a
//     count guard before it is compared to anything, which is what stops a
//     near-empty window from producing a storm.
//
// The baseline horizon is whatever the engine's topology projection retains
// (30 minutes by default). Startup never loads seven days of history because
// there is no history to load: the projection is built from live traffic.

const (
	// minWindowRequests is the smallest request count a window must carry
	// before its error rate or latency means anything at all.
	minWindowRequests = 20
	// minBaselineRequests is the smallest total request count the prior
	// finalized windows must carry to serve as a baseline.
	minBaselineRequests = 100
	// minBaselineWindows is the smallest number of finalized windows a
	// baseline is computed from. Two windows cannot describe variance.
	minBaselineWindows = 2
	// minPartialWindowFraction is how much of a still-open window must have
	// elapsed before it is evaluated. A window one second old holds one
	// second of traffic and says nothing about a five-minute rate.
	minPartialWindowFraction = 0.25
	// anomalySigma is how many standard errors (error rate) or standard
	// deviations (latency, metrics) the current value must clear.
	anomalySigma = 3.0
)

// detectAnomaliesFromTopology is the aggregate-mode detection pass for one
// tenant. It reads the last applied topology snapshot and nothing else — no
// database, no aggregate store.
func (g *GraphRAG) detectAnomaliesFromTopology(ctx context.Context, tenant string, stores *tenantStores) {
	snapPtr := stores.lastTopology.Load()
	if snapPtr == nil {
		return
	}
	snap := *snapPtr
	// Revision gate: an unchanged topology cannot have produced a new
	// anomaly, and re-running detection on it would only re-stamp timestamps.
	if stores.anomalyRevision.Load() == snap.Revision {
		return
	}
	stores.anomalyRevision.Store(snap.Revision)

	now := time.Now()
	for _, svc := range snap.Services {
		g.detectServiceAnomalies(ctx, tenant, stores, svc, now)
	}
	for _, m := range snap.Metrics {
		detectMetricAnomaly(stores, m, now)
	}
}

// detectServiceAnomalies evaluates one service's error rate and latency.
func (g *GraphRAG) detectServiceAnomalies(ctx context.Context, tenant string, stores *tenantStores, svc aggregate.TopologyService, now time.Time) {
	current, baseline, ok := splitWindows(svc.Windows, now)
	if !ok {
		return
	}

	if a, fired := errorRateAnomaly(svc.Name, current, baseline, now); fired {
		stores.anomalies.AddAnomaly(a)
		correlateWithRecent(stores, a)

		// Investigation trigger, unchanged from legacy: an error chain over
		// the retained exemplars plus the service's recent anomalies.
		chains := g.ErrorChain(ctx, svc.Name, now.Add(-5*time.Minute), 5)
		if len(chains) > 0 {
			anomalies := stores.anomalies.AnomaliesForService(svc.Name, now.Add(-1*time.Minute))
			g.PersistInvestigation(tenant, svc.Name, chains, anomalies)
		}
	}

	if a, fired := latencyAnomaly(svc.Name, current, baseline, now); fired {
		stores.anomalies.AddAnomaly(a)
		correlateWithRecent(stores, a)
	}
}

// splitWindows separates the window to evaluate from the finalized windows that
// form its baseline.
//
// The evaluated window is the most recent one that clears the partial-window
// guard: fully closed windows always qualify, a still-open window only once
// enough of it has elapsed AND it carries enough requests. Baseline windows are
// strictly older and strictly finalized — a window still inside its lateness
// horizon can still gain points and is not yet a fact.
func splitWindows(windows []aggregate.TopologyWindow, now time.Time) (aggregate.TopologyWindow, []aggregate.TopologyWindow, bool) {
	var current aggregate.TopologyWindow
	idx := -1
	for i := len(windows) - 1; i >= 0; i-- {
		w := windows[i]
		if w.Count < minWindowRequests {
			continue
		}
		if !w.Closed && float64(w.Elapsed) < minPartialWindowFraction*float64(aggregate.WindowSize) {
			continue
		}
		current, idx = w, i
		break
	}
	if idx < 0 {
		return current, nil, false
	}
	baseline := make([]aggregate.TopologyWindow, 0, idx)
	for _, w := range windows[:idx] {
		if w.Final {
			baseline = append(baseline, w)
		}
	}
	return current, baseline, true
}

// baselineErrorRate pools the prior finalized windows into one proportion.
func baselineErrorRate(baseline []aggregate.TopologyWindow) (rate float64, n uint64, ok bool) {
	if len(baseline) < minBaselineWindows {
		return 0, 0, false
	}
	var errs uint64
	for _, w := range baseline {
		n += w.Count
		errs += w.ErrorCount
	}
	if n < minBaselineRequests {
		return 0, 0, false
	}
	return float64(errs) / float64(n), n, true
}

// errorRateAnomaly fires when a window's error rate exceeds the prior
// finalized baseline by more than anomalySigma standard errors.
//
// The standard error is that of the baseline proportion measured over the
// CURRENT window's sample size, which is what makes the test scale-free: a
// jump from 0.4% to 1.2% over 5000 requests is significant, the same jump over
// 25 requests is not, and no absolute threshold is written down anywhere.
func errorRateAnomaly(service string, current aggregate.TopologyWindow, baseline []aggregate.TopologyWindow, now time.Time) (AnomalyNode, bool) {
	p, baseN, ok := baselineErrorRate(baseline)
	if !ok {
		return AnomalyNode{}, false
	}
	rate := current.ErrorRate()
	if rate <= p {
		return AnomalyNode{}, false
	}
	stderr := math.Sqrt(p * (1 - p) / float64(current.Count))
	if stderr <= 0 {
		// A baseline of exactly zero errors has no spread; any error at all
		// in a window that cleared the count guard is the signal.
		if current.ErrorCount == 0 {
			return AnomalyNode{}, false
		}
	} else if rate < p+anomalySigma*stderr {
		return AnomalyNode{}, false
	}

	return AnomalyNode{
		// Stable per (service, type): each pass UPSERTS the same evolving
		// node rather than minting one per tick.
		ID:       fmt.Sprintf("anom_%s_err", service),
		Type:     AnomalyErrorSpike,
		Severity: relativeSeverity(rate, p),
		Service:  service,
		Evidence: fmt.Sprintf(
			"error rate %.2f%% over %d requests vs %.2f%% baseline over %d requests in %d finalized windows",
			rate*100, current.Count, p*100, baseN, len(baseline),
		),
		Timestamp: now,
	}, true
}

// latencyAnomaly fires when a window's p99 clears the mean plus anomalySigma
// standard deviations of the prior finalized windows' p99s. Falls back to a
// doubling test when the baseline has no spread at all.
func latencyAnomaly(service string, current aggregate.TopologyWindow, baseline []aggregate.TopologyWindow, now time.Time) (AnomalyNode, bool) {
	if current.P99Micros <= 0 {
		return AnomalyNode{}, false
	}
	samples := make([]float64, 0, len(baseline))
	for _, w := range baseline {
		if w.P99Micros > 0 && w.Count >= minWindowRequests {
			samples = append(samples, w.P99Micros)
		}
	}
	if len(samples) < minBaselineWindows {
		return AnomalyNode{}, false
	}
	mean, stddev := meanStdDev(samples)
	threshold := mean + anomalySigma*stddev
	if stddev == 0 {
		threshold = mean * 2
	}
	if current.P99Micros <= threshold || mean <= 0 {
		return AnomalyNode{}, false
	}
	accuracy := "DDSketch accuracy unavailable"
	if current.LatencyProvenance != nil && current.LatencyProvenance.P99 != nil {
		p99 := current.LatencyProvenance.P99
		if p99.Degraded {
			accuracy = "DDSketch degraded"
		} else if p99.RelativeErrorBound > 0 {
			accuracy = fmt.Sprintf("DDSketch ±%.1f%%", p99.RelativeErrorBound*100)
		}
	}
	return AnomalyNode{
		ID:       fmt.Sprintf("anom_%s_lat", service),
		Type:     AnomalyLatencySpike,
		Severity: relativeSeverity(current.P99Micros, mean),
		Service:  service,
		Evidence: fmt.Sprintf(
			"approx. p99 %.0fms (p95 %.0fms; %s) vs %.0fms baseline (sd %.0fms) over %d finalized windows",
			current.P99Micros/1000, current.P95Micros/1000, accuracy, mean/1000, stddev/1000, len(samples),
		),
		Timestamp: now,
	}, true
}

// detectMetricAnomaly applies a rolling mean/variance z-score to one metric
// series. This is a real z-score: mean and standard deviation of the prior
// finalized windows' means, not a min/max range ratio.
func detectMetricAnomaly(stores *tenantStores, m aggregate.TopologyMetric, now time.Time) {
	var current aggregate.TopologyWindow
	idx := -1
	for i := len(m.Windows) - 1; i >= 0; i-- {
		if m.Windows[i].ValueCount > 0 {
			current, idx = m.Windows[i], i
			break
		}
	}
	if idx < 0 {
		return
	}
	samples := make([]float64, 0, idx)
	for _, w := range m.Windows[:idx] {
		if w.Final && w.ValueCount > 0 {
			samples = append(samples, w.Mean())
		}
	}
	if len(samples) < minBaselineWindows {
		return
	}
	mean, stddev := meanStdDev(samples)
	if stddev <= 0 {
		return
	}
	z := (current.Mean() - mean) / stddev
	if math.Abs(z) < anomalySigma {
		return
	}
	anomaly := AnomalyNode{
		ID:       fmt.Sprintf("anom_%s_metric_%s", m.Service, m.Metric),
		Type:     AnomalyMetricZScore,
		Severity: SeverityWarning,
		Service:  m.Service,
		Evidence: fmt.Sprintf(
			"metric %s z-score %.1f (window mean %.3f vs baseline %.3f, sd %.3f over %d finalized windows)",
			m.Metric, z, current.Mean(), mean, stddev, len(samples),
		),
		Timestamp: now,
	}
	stores.anomalies.AddAnomaly(anomaly)
	correlateWithRecent(stores, anomaly)
}

// meanStdDev returns the sample mean and population standard deviation.
func meanStdDev(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))
	var sq float64
	for _, v := range values {
		d := v - mean
		sq += d * d
	}
	return mean, math.Sqrt(sq / float64(len(values)))
}

// relativeSeverity grades how far a measurement sits above its own baseline.
// It is deliberately relative: an absolute cut-off is exactly the hard-coded
// threshold #163 removed.
func relativeSeverity(current, baseline float64) AnomalySeverity {
	if baseline <= 0 {
		return SeverityWarning
	}
	switch ratio := current / baseline; {
	case ratio >= 4:
		return SeverityCritical
	case ratio >= 2:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}
