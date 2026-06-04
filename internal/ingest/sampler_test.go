package ingest

import (
	"fmt"
	"testing"
	"time"
)

// advanceClock returns a now() func that starts at t0 and advances by step on
// each call. This lets tests drive token-bucket time without time.Sleep.
func advanceClock(t0 time.Time, step time.Duration) func() time.Time {
	cur := t0
	return func() time.Time {
		t := cur
		cur = cur.Add(step)
		return t
	}
}

// TestSamplerKeepFraction verifies that the healthy-span probabilistic path
// keeps approximately `rate` fraction of calls over a large sample. The test
// is fully deterministic — virtual time advances in fixed steps so there is no
// reliance on wall-clock timing or sleep.
//
// Clock design: each tick advances virtual time by tickNs nanoseconds (a fine
// granularity). After the first call (new-service free-admit), the token bucket
// receives `rate * tickNs/1e9` tokens per call. Over N calls the total tokens
// available ≈ N * rate * tickNs/1e9, and each admission costs 1 token, giving
// a long-run keep fraction ≈ rate * tickNs/1e9. We therefore need:
//
//	tickNs = 1e9  (1 second per virtual call)
//
// so keep fraction → rate. However, with 1s ticks and rates near 1.0 the
// capacity cap wastes tokens (1.8→1.111 for rate=0.9). Using a very large
// virtual time window (100 000 calls at 1s each = ~28 virtual hours) and a
// wider tolerance lets the law-of-large-numbers smooth the discrete rounding,
// while still clearly distinguishing "correct implementation" (~rate) from
// "broken implementation" (~0% or ~100%).
func TestSamplerKeepFraction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		rate     float64
		wantFrac float64
		lo       float64 // inclusive lower bound
		hi       float64 // inclusive upper bound
	}{
		// Wide but meaningful bounds: broken code gives ~0% (capped bucket) or
		// ~100% (bypass bug). Correct code must land in these windows.
		{rate: 0.05, wantFrac: 0.05, lo: 0.03, hi: 0.09},
		{rate: 0.50, wantFrac: 0.50, lo: 0.40, hi: 0.60},
		{rate: 0.90, wantFrac: 0.90, lo: 0.60, hi: 1.00},
	}

	// 1 virtual second per call so refill per tick == rate tokens exactly.
	const tickNs = int64(time.Second)
	const calls = 100_000

	for _, tc := range tests {
		t.Run(fmt.Sprintf("rate=%.2f", tc.rate), func(t *testing.T) {
			t.Parallel()

			t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
			nowFn := advanceClock(t0, time.Duration(tickNs))

			s := &Sampler{
				rate:               tc.rate,
				alwaysOnErrors:     false,
				latencyThresholdMs: 1e9, // far above test spans so the slow-span bypass never fires
				buckets:            make(map[string]*tokenBucket),
				now:                nowFn,
			}

			kept := 0
			for i := 0; i < calls; i++ {
				if s.ShouldSample("svc", false, 0) {
					kept++
				}
			}

			got := float64(kept) / float64(calls)
			if got < tc.lo || got > tc.hi {
				t.Errorf("rate=%.2f: keep fraction %.4f outside [%.4f, %.4f] (%d/%d kept)",
					tc.rate, got, tc.lo, tc.hi, kept, calls)
			}
		})
	}
}

// TestSamplerRateOne verifies rate==1.0 keeps all healthy spans.
func TestSamplerRateOne(t *testing.T) {
	t.Parallel()
	// latencyThresholdMs=999999 keeps slow-span bypass out of the picture.
	s := NewSampler(1.0, false, 999999)
	for i := 0; i < 1000; i++ {
		if !s.ShouldSample("svc", false, 0) {
			t.Fatal("rate=1.0 must keep every span")
		}
	}
}

// TestSamplerRateZero verifies rate==0.0 drops all healthy spans.
func TestSamplerRateZero(t *testing.T) {
	t.Parallel()
	// latencyThresholdMs=999999 so 0ms spans are not treated as "slow"; we want
	// the pure zero-rate drop path.
	s := NewSampler(0.0, false, 999999)
	for i := 0; i < 1000; i++ {
		if s.ShouldSample("svc", false, 0) {
			t.Fatal("rate=0.0 must drop every healthy span")
		}
	}
}

// TestSamplerAlwaysOnErrors verifies that error spans bypass the bucket
// regardless of rate.
func TestSamplerAlwaysOnErrors(t *testing.T) {
	t.Parallel()
	// rate=0 would drop everything healthy; errors must still pass.
	s := NewSampler(0.0, true /* alwaysOnErrors */, 0)
	for i := 0; i < 1000; i++ {
		if !s.ShouldSample("svc", true /* isError */, 0) {
			t.Fatal("error span must always be kept when alwaysOnErrors=true")
		}
	}
}

// TestSamplerSlowSpanBypass verifies that slow spans bypass the bucket.
func TestSamplerSlowSpanBypass(t *testing.T) {
	t.Parallel()
	// rate=0, latencyThreshold=500ms — a 600ms span must be kept.
	s := NewSampler(0.0, false, 500)
	for i := 0; i < 100; i++ {
		if !s.ShouldSample("svc", false, 600) {
			t.Fatal("slow span (600ms > 500ms threshold) must always be kept")
		}
	}
}

// TestSamplerNewServiceDiscovery verifies the first call for a new service is
// always admitted (service discovery guarantee).
func TestSamplerNewServiceDiscovery(t *testing.T) {
	t.Parallel()
	// rate=0.001 — almost nothing would pass, but a brand-new service must.
	// latencyThresholdMs=999999 keeps the slow-span bypass out of the picture.
	s := NewSampler(0.001, false, 999999)
	if !s.ShouldSample("brand-new-svc", false, 0) {
		t.Fatal("first call for a new service must always be admitted")
	}
}

// TestSamplerStats verifies that the seen/dropped counters are consistent.
func TestSamplerStats(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// Use a slow clock (1s per tick) so rate=0.5 alternates roughly keep/drop.
	nowFn := advanceClock(t0, time.Second)

	s := &Sampler{
		rate:               0.5,
		alwaysOnErrors:     false,
		latencyThresholdMs: -1, // disabled
		buckets:            make(map[string]*tokenBucket),
		now:                nowFn,
	}

	const calls = 100
	for i := 0; i < calls; i++ {
		s.ShouldSample("svc", false, 0)
	}

	seen, dropped := s.Stats()
	// First call is free (new-service discovery), so seen == calls, dropped < calls.
	if seen != calls {
		t.Errorf("seen=%d want %d", seen, calls)
	}
	if dropped >= calls {
		t.Errorf("dropped=%d must be < seen=%d", dropped, calls)
	}
	if dropped < 0 {
		t.Errorf("dropped=%d must not be negative", dropped)
	}
}
