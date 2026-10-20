package aggregate

import (
	"math"
	"testing"
	"time"
)

// Non-finite samples must never reach the shared durable commit. SQLite binds
// NaN as NULL, so one such metric used to roll back valid traces and logs too.
func TestNonFiniteMetricsDoNotPoisonCommit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		temporality Temporality
		monotonic   bool
	}{
		{"gauge", TemporalityUnspecified, false},
		{"updown", TemporalityCumulative, false},
		{"delta", TemporalityDelta, true},
		{"cumulative", TemporalityCumulative, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock(time.Unix(3_000_000, 0).UTC())
			store := newTestStore(t)
			eng := newTestEngine(t, clock, nil)
			writer := newTestWriter(t, store, eng, clock, WriterConfig{})
			r := eng.NewReducer(clock.Now())
			in := MetricInput{Tenant: "default", Service: "checkout", Name: "value", Timestamp: clock.Now(), Temporality: tc.temporality, Monotonic: tc.monotonic}
			// Reject before any baseline/identity mutation, including a bad first point.
			for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
				in.Value = v
				res := r.ReduceMetricPoint(in)
				if !res.Rejected() || res.Reason != ReasonNonFiniteValue {
					t.Errorf("value %v: verdict %+v", v, res)
				}
			}
			if len(r.Deltas()) != 0 {
				t.Errorf("non-finite inputs created deltas: %v", r.Deltas())
			}
			in.Timestamp = in.Timestamp.Add(time.Second)
			in.Value = 42
			r.ReduceMetricPoint(in)
			in.Timestamp = in.Timestamp.Add(time.Second)
			in.Value = 50
			r.ReduceMetricPoint(in)
			r.ReduceLog(LogInput{Tenant: "default", Service: "checkout", Body: "ok", Timestamp: clock.Now()})
			r.ReduceSpan(SpanInput{Tenant: "default", Service: "checkout", SpanName: "GET /", Timestamp: clock.Now(), DurationMicros: 100})
			if _, err := eng.ApplyDeltasErr(r.Deltas()); err != nil {
				t.Fatalf("commit mixed valid telemetry: %v", err)
			}
			writer.Stop()
			path := store.path
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := newTestStoreAt(t, path, StoreConfig{})
			rows, err := reopened.ReplayMutable(0)
			if err != nil {
				t.Fatal(err)
			}
			var metrics, logs, traces uint64
			for _, row := range rows {
				d := row.Delta
				metrics += d.GaugeCount
				logs += d.LogCount
				traces += d.DurationCount
				if IsGaugeLike(tc.temporality, tc.monotonic) && d.GaugeCount > 0 {
					if d.GaugeCount != 2 || d.GaugeSum != 92 || d.GaugeMin != 42 || d.GaugeMax != 50 || d.GaugeLast != 50 {
						t.Fatalf("finite gauge changed: %+v", d)
					}
				}
				if d.CounterDelta != 0 {
					want := 92.0
					if tc.temporality == TemporalityCumulative {
						want = 8
					}
					if d.Count != 2 || d.CounterDelta != want {
						t.Fatalf("finite counter changed: %+v, want %v", d, want)
					}
					metrics += d.Count
				}
			}
			if metrics != 2 || logs != 1 || traces != 1 {
				t.Fatalf("durable counts metric/log/trace = %d/%d/%d", metrics, logs, traces)
			}
		})
	}
}
