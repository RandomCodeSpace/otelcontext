package ingest

import (
	"sync"
	"sync/atomic"
	"time"
)

// Sampler decides whether a trace/span should be ingested.
// Always keeps: error traces, slow traces (duration > latencyThresholdMs), new services.
// Samples healthy traces at the configured rate using a per-service token bucket.
type Sampler struct {
	rate               float64 // 0.0–1.0, fraction to keep
	alwaysOnErrors     bool
	latencyThresholdMs float64 // always keep traces slower than this
	mu                 sync.Mutex
	buckets            map[string]*tokenBucket
	totalSeen          atomic.Int64
	totalDropped       atomic.Int64
	now                func() time.Time // injectable for tests; defaults to time.Now
}

// NewSampler creates a Sampler with the given parameters.
func NewSampler(rate float64, alwaysOnErrors bool, latencyThresholdMs float64) *Sampler {
	if rate <= 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	return &Sampler{
		rate:               rate,
		alwaysOnErrors:     alwaysOnErrors,
		latencyThresholdMs: latencyThresholdMs,
		buckets:            make(map[string]*tokenBucket),
		now:                time.Now,
	}
}

// ShouldSample returns true if the trace should be ingested.
// isError: whether the trace/span has error status.
// durationMs: trace duration in milliseconds.
// serviceName: originating service.
func (s *Sampler) ShouldSample(serviceName string, isError bool, durationMs float64) bool {
	s.totalSeen.Add(1)

	// Always ingest errors.
	if s.alwaysOnErrors && isError {
		return true
	}

	// Always ingest slow traces.
	if durationMs >= s.latencyThresholdMs {
		return true
	}

	// Full ingestion rate — skip sampling.
	if s.rate >= 1.0 {
		return true
	}

	// Zero rate — drop everything (except errors/slow, handled above).
	if s.rate <= 0 {
		s.totalDropped.Add(1)
		return false
	}

	// Token bucket per service.
	s.mu.Lock()
	b, ok := s.buckets[serviceName]
	if !ok {
		b = newTokenBucket(s.rate, s.now)
		s.buckets[serviceName] = b
		// Always let first trace through (new service discovery).
		s.mu.Unlock()
		return true
	}
	allow := b.allow(s.now())
	s.mu.Unlock()

	if !allow {
		s.totalDropped.Add(1)
	}
	return allow
}

// Stats returns (seen, dropped) counters for metrics.
func (s *Sampler) Stats() (int64, int64) {
	return s.totalSeen.Load(), s.totalDropped.Load()
}

// tokenBucket throttles healthy-span ingestion per service. It is a RATE
// LIMITER, not a percentage sampler: it admits at most ~`rate` healthy spans
// per second per service (refill `rate` tokens/s, cost 1 token/span) with a
// startup burst of up to capacity = max(1, 1/rate) spans.
//
// The long-run keep FRACTION is therefore min(1, rate / offered_rate): a
// service emitting ≤ `rate` healthy spans/s keeps all of them; above that it
// keeps a shrinking fraction while the absolute admitted rate stays near
// `rate`/s. So rate=0.05 means "~0.05 healthy spans/s/service" (≈ one per 20s),
// NOT "5% of healthy spans". Errors and slow spans bypass this entirely in
// ShouldSample, so there is no data loss for the signals that matter; the
// absolute cap is what bounds the SQLite write rate under load spikes. If a
// true proportional keep-fraction is ever wanted, switch to probabilistic
// (hash-mod / rand) sampling — that is a separate design decision.
//
// (The previous implementation capped tokens at 1.0 but charged 1.0/rate per
// request; for any rate < 1.0 the charge exceeded the cap, so no healthy span
// could ever be admitted — the SQLite default rate of 0.05 dropped ~100%.)
type tokenBucket struct {
	rate     float64   // refill tokens/sec == max sustained admitted healthy spans/sec
	capacity float64   // max accumulated tokens (burst ceiling)
	tokens   float64   // current token count
	lastTick time.Time // wall-clock of last refill
}

func newTokenBucket(rate float64, now func() time.Time) *tokenBucket {
	// capacity = 1/rate tokens: one "admission interval" of burst so a freshly
	// seen service is not throttled immediately. Floored at 1.0 so a span can
	// always eventually be admitted.
	capacity := 1.0 / rate
	if capacity < 1.0 {
		capacity = 1.0
	}
	return &tokenBucket{
		rate:     rate,
		capacity: capacity,
		tokens:   capacity, // start full so new services are not immediately throttled
		lastTick: now(),
	}
}

// allow refills the bucket for elapsed time and admits the request if
// at least 1 token is available, consuming exactly 1 on admission.
func (b *tokenBucket) allow(now time.Time) bool {
	elapsed := now.Sub(b.lastTick).Seconds()
	b.lastTick = now

	b.tokens += elapsed * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true
	}
	return false
}
