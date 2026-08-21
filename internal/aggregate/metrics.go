package aggregate

import (
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

// Metric plumbing. The Prometheus collectors themselves live in
// internal/telemetry with every other subsystem's metrics — that package owns
// registration against the default registry via promauto, and splitting the
// aggregate engine's metrics out into a second registration site would give
// operators two places to look and this package a duplicate-registration panic
// in tests.
//
// The indirection here is a recorder interface, not a second registry: it keeps
// the engine testable without a live Prometheus registry and keeps a nil
// *telemetry.Metrics (which every ingest test passes) from panicking.

// MetricsRecorder is the engine's view of the metric surface.
type MetricsRecorder interface {
	// RecordReduction publishes one Export request's reduction accounting:
	// input points, emitted deltas, the reduction ratio, late/future
	// exclusions, and the shadow-comparison counters.
	RecordReduction(stats ReducerStats, deltas map[Signal]uint64)
	// RecordOverflow counts one admission rerouted to an __other__ series.
	RecordOverflow(signal Signal, reason OverflowReason)
	// SetActiveSeries publishes the active-series census per signal.
	SetActiveSeries(active map[Signal]int)
}

// noopRecorder is the default when no metrics are wired.
type noopRecorder struct{}

func (noopRecorder) RecordReduction(ReducerStats, map[Signal]uint64) {}
func (noopRecorder) RecordOverflow(Signal, OverflowReason)           {}
func (noopRecorder) SetActiveSeries(map[Signal]int)                  {}

// promRecorder bridges the engine onto the platform's Prometheus metrics.
type promRecorder struct{ m *telemetry.Metrics }

// NewPrometheusRecorder returns a recorder backed by the platform metrics. A
// nil *telemetry.Metrics yields a no-op recorder rather than a panic.
func NewPrometheusRecorder(m *telemetry.Metrics) MetricsRecorder {
	if m == nil {
		return noopRecorder{}
	}
	return promRecorder{m: m}
}

// RecordReduction implements MetricsRecorder.
func (r promRecorder) RecordReduction(stats ReducerStats, deltas map[Signal]uint64) {
	for i := range stats.InputPoints {
		signal := Signal(i) // #nosec G115 -- loop index over a signal-sized array
		in := stats.InputPoints[i]
		if in == 0 && stats.LatePoints[i] == 0 && stats.FuturePoints[i] == 0 {
			continue
		}
		label := signal.String()
		if in > 0 {
			out := deltas[signal]
			r.m.AggregateInputPointsTotal.WithLabelValues(label).Add(float64(in))
			r.m.AggregateDeltasTotal.WithLabelValues(label).Add(float64(out))
			if out > 0 {
				r.m.AggregateReductionRatio.WithLabelValues(label).Observe(float64(in) / float64(out))
			}
		}
		if n := stats.LatePoints[i]; n > 0 {
			r.m.AggregateLatePointsTotal.WithLabelValues(label, PointLate.String()).Add(float64(n))
		}
		if n := stats.FuturePoints[i]; n > 0 {
			r.m.AggregateLatePointsTotal.WithLabelValues(label, PointFuture.String()).Add(float64(n))
		}
		if n := stats.Accepted[i]; n > 0 {
			r.m.AggregateShadowAcceptedTotal.WithLabelValues(label).Add(float64(n))
		}
	}
	for service, n := range stats.ErrorsByService {
		if n > 0 {
			r.m.AggregateShadowErrorsTotal.WithLabelValues(service).Add(float64(n))
		}
	}
}

// RecordOverflow implements MetricsRecorder.
func (r promRecorder) RecordOverflow(signal Signal, reason OverflowReason) {
	r.m.AggregateOverflowTotal.WithLabelValues(signal.String(), reason.String()).Inc()
}

// SetActiveSeries implements MetricsRecorder.
func (r promRecorder) SetActiveSeries(active map[Signal]int) {
	for sig := SignalTraceOp; sig <= signalMax; sig++ {
		r.m.AggregateSeriesActive.WithLabelValues(sig.String()).Set(float64(active[sig]))
	}
}

// promStoreRecorder bridges the durable store and the group-commit writer onto
// the platform's Prometheus metrics.
type promStoreRecorder struct{ m *telemetry.Metrics }

// NewPrometheusStoreMetrics returns a StoreMetrics backed by the platform
// metrics. A nil *telemetry.Metrics yields a no-op recorder rather than a
// panic, which is what every store unit test passes.
func NewPrometheusStoreMetrics(m *telemetry.Metrics) StoreMetrics {
	if m == nil {
		return noopStoreMetrics{}
	}
	return promStoreRecorder{m: m}
}

// result renders an error as a metric label value.
func result(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// RecordCommit implements StoreMetrics.
func (r promStoreRecorder) RecordCommit(d time.Duration, deltas int, bytes int64, err error) {
	label := result(err)
	r.m.AggregateCommitDurationSeconds.WithLabelValues(label).Observe(d.Seconds())
	r.m.AggregateCommitsTotal.WithLabelValues(label).Inc()
	r.m.AggregateCommitDeltas.Observe(float64(deltas))
	if bytes > 0 {
		r.m.AggregateCommitBytesTotal.Add(float64(bytes))
	}
}

// RecordAdmissionRejected implements StoreMetrics.
func (r promStoreRecorder) RecordAdmissionRejected(bound string) {
	r.m.AggregateAdmissionRejectedTotal.WithLabelValues(bound).Inc()
}

// RecordFinalize implements StoreMetrics.
func (r promStoreRecorder) RecordFinalize(stats FinalizeStats, err error) {
	r.m.AggregateFinalizeDurationSeconds.Observe(stats.Duration.Seconds())
	if err != nil {
		return
	}
	if stats.Buckets > 0 {
		r.m.AggregateFinalizeRowsTotal.WithLabelValues("buckets").Add(float64(stats.Buckets))
	}
	if stats.DeltaRows > 0 {
		r.m.AggregateFinalizeRowsTotal.WithLabelValues("deltas").Add(float64(stats.DeltaRows))
	}
}

// RecordPurge implements StoreMetrics.
func (r promStoreRecorder) RecordPurge(stats PurgeStats, err error) {
	r.m.AggregatePurgeDurationSeconds.Observe(stats.Duration.Seconds())
	if err != nil {
		return
	}
	for kind, n := range map[string]int64{
		"buckets":   stats.Buckets,
		"deltas":    stats.Deltas,
		"baselines": stats.Baselines,
	} {
		if n > 0 {
			r.m.AggregatePurgeRowsTotal.WithLabelValues(kind).Add(float64(n))
		}
	}
}

// SetBacklog implements StoreMetrics.
func (r promStoreRecorder) SetBacklog(rows int64, ageSeconds float64) {
	r.m.AggregateDeltaLogRows.Set(float64(rows))
	r.m.AggregateDeltaLogAgeSeconds.Set(ageSeconds)
}

// RecordRecovery implements StoreMetrics.
func (r promStoreRecorder) RecordRecovery(d time.Duration, replayed, finalized int) {
	r.m.AggregateRecoveryDurationSeconds.Set(d.Seconds())
	r.m.AggregateRecoveryRows.WithLabelValues("replayed").Set(float64(replayed))
	r.m.AggregateRecoveryRows.WithLabelValues("finalized_windows").Set(float64(finalized))
}
