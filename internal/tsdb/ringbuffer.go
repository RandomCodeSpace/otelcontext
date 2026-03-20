// Package tsdb provides an in-memory ring buffer for per-metric sliding windows
// with pre-computed aggregates. It accelerates dashboard queries for recent data
// so they never need to hit the relational DB.
package tsdb

import (
	"math"
	"sort"
	"sync"
	"time"
)

// WindowAgg is the pre-computed aggregate for one time window slot.
type WindowAgg struct {
	MetricName  string
	ServiceName string
	WindowStart time.Time
	Count       int64
	Sum         float64
	Min         float64
	Max         float64
	P50         float64
	P95         float64
	P99         float64
	samples     []float64 // raw samples, capped at maxSamples
}

const maxSamples = 256 // per-window sample cap for percentile computation

// ringSlot is a fixed-capacity circular slot in the ring buffer.
// Access is protected by the parent MetricRing's mu.
type ringSlot struct {
	agg WindowAgg
}

// MetricRing is a fixed-size circular buffer for a single metric+service key.
// Each slot represents one windowDuration-wide time bucket.
type MetricRing struct {
	mu           sync.RWMutex
	slots        []ringSlot
	size         int
	windowDur    time.Duration
	currentIdx   int
	currentStart time.Time
	metricName   string
	serviceName  string
}

func newMetricRing(metricName, serviceName string, slots int, windowDur time.Duration) *MetricRing {
	r := &MetricRing{
		slots:       make([]ringSlot, slots),
		size:        slots,
		windowDur:   windowDur,
		metricName:  metricName,
		serviceName: serviceName,
	}
	now := time.Now().UTC()
	r.currentStart = now.Truncate(windowDur)
	for i := range r.slots {
		r.slots[i].agg = newEmptyAgg(metricName, serviceName)
	}
	r.slots[0].agg.WindowStart = r.currentStart
	return r
}

func newEmptyAgg(metric, service string) WindowAgg {
	return WindowAgg{
		MetricName:  metric,
		ServiceName: service,
		Min:         math.MaxFloat64,
		Max:         -math.MaxFloat64,
	}
}

// Record adds a new data point, advancing the window if necessary.
func (r *MetricRing) Record(value float64, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	windowStart := at.UTC().Truncate(r.windowDur)
	if windowStart.After(r.currentStart) {
		// Advance one or more slots.
		steps := int(windowStart.Sub(r.currentStart) / r.windowDur)
		for i := 0; i < steps && i < r.size; i++ {
			r.currentIdx = (r.currentIdx + 1) % r.size
			r.currentStart = r.currentStart.Add(r.windowDur)
			s := &r.slots[r.currentIdx]
			s.agg = newEmptyAgg(r.metricName, r.serviceName)
			s.agg.WindowStart = r.currentStart
		}
	}

	s := &r.slots[r.currentIdx]
	s.agg.Count++
	s.agg.Sum += value
	if value < s.agg.Min {
		s.agg.Min = value
	}
	if value > s.agg.Max {
		s.agg.Max = value
	}
	if len(s.agg.samples) < maxSamples {
		s.agg.samples = append(s.agg.samples, value)
	}
}

// Windows returns up to `n` most-recent completed window aggregates.
// Takes a snapshot under the ring lock to avoid acquiring 120+ slot locks sequentially.
func (r *MetricRing) Windows(n int) []WindowAgg {
	r.mu.Lock()
	if n > r.size {
		n = r.size
	}
	// Snapshot all needed slots under a single lock
	type slotSnap struct {
		agg     WindowAgg
		samples []float64
	}
	snaps := make([]slotSnap, 0, n)
	for i := 0; i < n; i++ {
		idx := (r.currentIdx - i + r.size) % r.size
		s := &r.slots[idx]
		if s.agg.Count > 0 {
			samplesCopy := make([]float64, len(s.agg.samples))
			copy(samplesCopy, s.agg.samples)
			snaps = append(snaps, slotSnap{agg: s.agg, samples: samplesCopy})
		}
	}
	r.mu.Unlock()

	// Compute percentiles outside the lock
	out := make([]WindowAgg, 0, len(snaps))
	for _, snap := range snaps {
		agg := snap.agg
		agg.P50 = ringPercentile(snap.samples, 50)
		agg.P95 = ringPercentile(snap.samples, 95)
		agg.P99 = ringPercentile(snap.samples, 99)
		if agg.Min == math.MaxFloat64 {
			agg.Min = 0
		}
		if agg.Max == -math.MaxFloat64 {
			agg.Max = 0
		}
		agg.samples = nil // don't leak raw samples
		out = append(out, agg)
	}
	return out
}

// RingBuffer manages per-metric ring buffers, keyed by "service|metric".
type RingBuffer struct {
	mu        sync.RWMutex
	rings     map[string]*MetricRing
	slots     int
	windowDur time.Duration
}

// NewRingBuffer creates a RingBuffer.
// slots × windowDur = total retention (e.g. 120 × 30s = 1 hour).
func NewRingBuffer(slots int, windowDur time.Duration) *RingBuffer {
	return &RingBuffer{
		rings:     make(map[string]*MetricRing),
		slots:     slots,
		windowDur: windowDur,
	}
}

// Record records a value for the given metric+service at time t.
func (rb *RingBuffer) Record(metricName, serviceName string, value float64, at time.Time) {
	key := serviceName + "|" + metricName
	rb.mu.RLock()
	ring, ok := rb.rings[key]
	rb.mu.RUnlock()
	if !ok {
		rb.mu.Lock()
		ring, ok = rb.rings[key]
		if !ok {
			ring = newMetricRing(metricName, serviceName, rb.slots, rb.windowDur)
			rb.rings[key] = ring
		}
		rb.mu.Unlock()
	}
	ring.Record(value, at)
}

// QueryRecent returns aggregated windows for the given metric+service.
// Pass an empty serviceName to query across all services (returns first match).
func (rb *RingBuffer) QueryRecent(metricName, serviceName string, windowCount int) []WindowAgg {
	key := serviceName + "|" + metricName
	rb.mu.RLock()
	ring, ok := rb.rings[key]
	rb.mu.RUnlock()
	if !ok {
		return nil
	}
	return ring.Windows(windowCount)
}

// AllKeys returns all registered metric+service keys.
func (rb *RingBuffer) AllKeys() []string {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	keys := make([]string, 0, len(rb.rings))
	for k := range rb.rings {
		keys = append(keys, k)
	}
	return keys
}

// MetricCount returns the number of distinct metric series tracked.
func (rb *RingBuffer) MetricCount() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return len(rb.rings)
}

// ringPercentile computes the p-th percentile of a float64 slice.
func ringPercentile(data []float64, p float64) float64 {
	if len(data) == 0 {
		return 0
	}
	sorted := make([]float64, len(data))
	copy(sorted, data)
	sort.Float64s(sorted)
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
