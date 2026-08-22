package aggregate

import "time"

// AggregateDelta is the compact aggregate contribution of one Export request to
// one series in one window (CONTEXT.md, "Delta"). It is what request-local
// reduction produces and what the engine applies; finalized buckets are built
// from deltas, never the other way round.
//
// Every field is additive or order-independent so two deltas for the same
// (series, window) merge without consulting the points that produced them. That
// is what lets the Phase 2 group-commit writer (#173) pre-merge deltas inside a
// transaction, and what lets Phase 1 apply them straight to the shards.
//
// A delta is not safe for concurrent use. Reducers are request-local and each
// owns its deltas until they are handed to the engine.
type AggregateDelta struct {
	// Count is the number of accepted points that contributed to this series:
	// spans for traces, log records for logs, data points for metrics. It is
	// accepted telemetry, never sampled telemetry (#153 §8).
	Count uint64
	// ErrorCount is the subset of Count classified as an error: span status
	// ERROR for traces, severity ERROR/FATAL for logs. Metrics never set it.
	ErrorCount uint64

	// RequestCount is the subset of Count that qualifies as a REQUEST: a span
	// that is a trace root (no parent span) or a SERVER-kind span. Either
	// qualifies and a span is counted at most once (#197 Q2).
	//
	// It exists because Count is per SPAN, and a dashboard that labels a span
	// total "traces" is lying by a factor of the average trace size. A
	// distributed trace with several entry points counts once per entry point:
	// a documented approximation, not a unique-trace-ID count, because trace
	// IDs are on the permanent banned list (#153, #159) and cannot be counted
	// without carrying them into aggregate identity.
	//
	// Only trace-shaped signals set it; logs and metrics leave it zero.
	RequestCount uint64
	// ErrorRequestCount is the error subset of RequestCount. It is the
	// numerator of the headline dashboard error rate (#197 Q3); per-operation
	// error rates stay span-based on ErrorCount/Count.
	ErrorRequestCount uint64

	// --- durations (traces) ---

	// DurationCount is the number of duration observations. It tracks Count
	// for trace series and stays zero elsewhere, so a merged delta can still
	// tell "no spans" from "spans with zero duration".
	DurationCount uint64
	// DurationSum is the sum of observed durations, in microseconds.
	DurationSum float64
	// DurationMin and DurationMax are the extremes of the observed durations
	// in microseconds. They are only meaningful when DurationCount > 0.
	DurationMin float64
	DurationMax float64
	// Sketch holds the quantile contribution of the observed durations. It is
	// allocated on the first duration observation and stays nil for series
	// that carry no latency (logs, metrics), which keeps the common delta
	// small — a Sketch is ~2 KiB of dense bins.
	Sketch *Sketch

	// --- gauge-like metrics ---
	//
	// Populated by gauges and by cumulative non-monotonic sums, which #166
	// aggregates gauge-like: last/min/max/sum-of-samples/count, never
	// reset-detected.

	GaugeCount uint64
	GaugeSum   float64
	GaugeMin   float64
	GaugeMax   float64
	// GaugeLast is the sample with the highest GaugeLastTime seen so far.
	// Merging picks the later timestamp so the result does not depend on the
	// order in which deltas were merged. Two samples of one series carrying
	// the IDENTICAL timestamp are ambiguous about which is last, and that tie
	// resolves by arrival order — no merge rule can fix a producer that
	// timestamps two values the same.
	GaugeLast     float64
	GaugeLastTime time.Time

	// --- cumulative counters ---

	// CounterDelta is the increase attributed to this window: the sum of
	// per-point deltas computed by the baseline tracker for cumulative sums,
	// or the raw values of delta-temporality points.
	CounterDelta float64
	// ResetCount is the number of counter resets observed in this window
	// (start-time change or value regression, per #166).
	ResetCount uint64

	// --- logs ---

	// LogCount is the number of log records in this series. It is redundant
	// with Count for log series and zero everywhere else; it exists so a
	// merged multi-signal view can still answer "how many logs" without
	// carrying the SeriesKey alongside.
	LogCount uint64
	// FirstTimestamp and LastTimestamp bound the log records that contributed.
	// Zero when LogCount is zero.
	FirstTimestamp time.Time
	LastTimestamp  time.Time
}

// ObserveSpan records one span: its count, its error classification, whether it
// is a request entry point, and its duration in microseconds. Negative
// durations are recorded in the counters but clamped to zero for the sketch,
// which is what Sketch.Observe does with them anyway (latency is non-negative
// by construction).
//
// isRequest is IsRequestSpan's verdict for the span. It is passed in rather
// than derived here because the delta never sees the span's parent or kind.
func (d *AggregateDelta) ObserveSpan(durationMicros float64, isError, isRequest bool) {
	d.Count++
	if isError {
		d.ErrorCount++
	}
	if isRequest {
		d.RequestCount++
		if isError {
			d.ErrorRequestCount++
		}
	}
	d.observeDuration(durationMicros)
}

// observeDuration folds one duration observation into the duration statistics
// and the sketch, allocating the sketch on first use.
func (d *AggregateDelta) observeDuration(micros float64) {
	if d.DurationCount == 0 || micros < d.DurationMin {
		d.DurationMin = micros
	}
	if d.DurationCount == 0 || micros > d.DurationMax {
		d.DurationMax = micros
	}
	d.DurationCount++
	d.DurationSum += micros
	if d.Sketch == nil {
		d.Sketch = NewSketch()
	}
	d.Sketch.Observe(micros)
}

// ObserveLog records one log record at ts. isError marks ERROR/FATAL severity.
func (d *AggregateDelta) ObserveLog(ts time.Time, isError bool) {
	d.Count++
	d.LogCount++
	if isError {
		d.ErrorCount++
	}
	if d.FirstTimestamp.IsZero() || ts.Before(d.FirstTimestamp) {
		d.FirstTimestamp = ts
	}
	if ts.After(d.LastTimestamp) {
		d.LastTimestamp = ts
	}
}

// ObserveGauge records one gauge-like sample at ts.
func (d *AggregateDelta) ObserveGauge(value float64, ts time.Time) {
	d.Count++
	if d.GaugeCount == 0 || value < d.GaugeMin {
		d.GaugeMin = value
	}
	if d.GaugeCount == 0 || value > d.GaugeMax {
		d.GaugeMax = value
	}
	d.GaugeCount++
	d.GaugeSum += value
	if d.GaugeLastTime.IsZero() || !ts.Before(d.GaugeLastTime) {
		d.GaugeLast = value
		d.GaugeLastTime = ts
	}
}

// ObserveCounter records one counter increase already converted to a delta by
// the baseline tracker. reset marks that the increase followed a counter reset.
func (d *AggregateDelta) ObserveCounter(delta float64, reset bool) {
	d.Count++
	d.CounterDelta += delta
	if reset {
		d.ResetCount++
	}
}

// Merge folds other into d. Every field is additive or order-independent, so
// merging is associative and commutative: the result depends only on the
// multiset of observations, never on the order they arrived in.
func (d *AggregateDelta) Merge(other *AggregateDelta) {
	if other == nil {
		return
	}
	d.Count += other.Count
	d.ErrorCount += other.ErrorCount
	d.RequestCount += other.RequestCount
	d.ErrorRequestCount += other.ErrorRequestCount

	if other.DurationCount > 0 {
		if d.DurationCount == 0 || other.DurationMin < d.DurationMin {
			d.DurationMin = other.DurationMin
		}
		if d.DurationCount == 0 || other.DurationMax > d.DurationMax {
			d.DurationMax = other.DurationMax
		}
		d.DurationCount += other.DurationCount
		d.DurationSum += other.DurationSum
	}
	if other.Sketch != nil {
		if d.Sketch == nil {
			d.Sketch = NewSketch()
		}
		d.Sketch.Merge(other.Sketch)
	}

	if other.GaugeCount > 0 {
		if d.GaugeCount == 0 || other.GaugeMin < d.GaugeMin {
			d.GaugeMin = other.GaugeMin
		}
		if d.GaugeCount == 0 || other.GaugeMax > d.GaugeMax {
			d.GaugeMax = other.GaugeMax
		}
		d.GaugeCount += other.GaugeCount
		d.GaugeSum += other.GaugeSum
		if d.GaugeLastTime.IsZero() || other.GaugeLastTime.After(d.GaugeLastTime) {
			d.GaugeLast = other.GaugeLast
			d.GaugeLastTime = other.GaugeLastTime
		}
	}

	d.CounterDelta += other.CounterDelta
	d.ResetCount += other.ResetCount

	d.LogCount += other.LogCount
	if !other.FirstTimestamp.IsZero() && (d.FirstTimestamp.IsZero() || other.FirstTimestamp.Before(d.FirstTimestamp)) {
		d.FirstTimestamp = other.FirstTimestamp
	}
	if other.LastTimestamp.After(d.LastTimestamp) {
		d.LastTimestamp = other.LastTimestamp
	}
}

// Clone returns a deep copy, including the sketch. Used by Snapshot so a caller
// can read a consistent view without holding a shard lock.
func (d *AggregateDelta) Clone() *AggregateDelta {
	if d == nil {
		return nil
	}
	cp := *d
	if d.Sketch != nil {
		s := *d.Sketch
		cp.Sketch = &s
	}
	return &cp
}
