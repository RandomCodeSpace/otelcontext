package tsdb

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/RandomCodeSpace/argus/internal/storage"
)

// RawMetric represents an incoming single metric data point before aggregation.
type RawMetric struct {
	Name        string
	ServiceName string
	Value       float64
	Timestamp   time.Time
	Attributes  map[string]interface{}
}

// Aggregator manages in-memory tumbling windows for metrics.
type Aggregator struct {
	repo            *storage.Repository
	windowSize      time.Duration
	buckets         map[string]*storage.MetricBucket
	mu              sync.Mutex
	stopChan        chan struct{}
	flushChan       chan []storage.MetricBucket
	pool            sync.Pool
	droppedBatches  int64

	// Cardinality controls
	maxCardinality      int    // 0 = unlimited
	cardinalityOverflow func() // called when overflow bucket is used (for metrics)
	overflowKey         string // constant key for the overflow bucket

	// Ring buffer accelerator (optional)
	ring *RingBuffer
}

const persistenceWorkers = 3

// NewAggregator creates a new TSDB aggregator.
func NewAggregator(repo *storage.Repository, windowSize time.Duration) *Aggregator {
	a := &Aggregator{
		repo:        repo,
		windowSize:  windowSize,
		buckets:     make(map[string]*storage.MetricBucket),
		stopChan:    make(chan struct{}),
		flushChan:   make(chan []storage.MetricBucket, 500),
		overflowKey: "__cardinality_overflow__",
	}
	a.pool.New = func() interface{} {
		return make([]storage.MetricBucket, 0, 100)
	}
	return a
}

// SetCardinalityLimit configures the maximum number of distinct metric series.
// When exceeded, new series are routed to an overflow bucket and onOverflow is called.
func (a *Aggregator) SetCardinalityLimit(max int, onOverflow func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.maxCardinality = max
	a.cardinalityOverflow = onOverflow
}

// SetRingBuffer attaches a RingBuffer that receives every ingested data point.
// Dashboard queries for recent data (last 1h) can read from the ring directly.
func (a *Aggregator) SetRingBuffer(rb *RingBuffer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ring = rb
}

// Start begins the aggregation background processes.
func (a *Aggregator) Start(ctx context.Context) {
	ticker := time.NewTicker(a.windowSize)
	defer ticker.Stop()

	slog.Info("📈 TSDB Aggregator started", "window_size", a.windowSize, "workers", persistenceWorkers)

	for i := 0; i < persistenceWorkers; i++ {
		go a.persistenceWorker(ctx)
	}

	for {
		select {
		case <-ticker.C:
			a.flush()
		case <-a.stopChan:
			a.flush() // Final flush
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop stops the aggregator.
func (a *Aggregator) Stop() {
	close(a.stopChan)
}

// Ingest adds a raw metric point to the current aggregator window.
func (a *Aggregator) Ingest(m RawMetric) {
	// Pre-compute key outside the lock — json.Marshal is CPU-bound and must not hold mu.
	attrJSON, _ := json.Marshal(m.Attributes)
	key := fmt.Sprintf("%s|%s|%s", m.ServiceName, m.Name, string(attrJSON))

	// Feed ring buffer outside the lock (RingBuffer is independently thread-safe).
	if a.ring != nil {
		a.ring.Record(m.Name, m.ServiceName, m.Value, m.Timestamp)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	bucket, exists := a.buckets[key]
	if !exists {
		// Cardinality guard: if limit exceeded, route to overflow bucket.
		if a.maxCardinality > 0 && len(a.buckets) >= a.maxCardinality {
			if a.cardinalityOverflow != nil {
				a.cardinalityOverflow()
			}
			key = a.overflowKey
			bucket = a.buckets[key]
			if bucket == nil {
				windowStart := m.Timestamp.Truncate(a.windowSize)
				bucket = &storage.MetricBucket{
					Name:        "__overflow__",
					ServiceName: m.ServiceName,
					TimeBucket:  windowStart,
					Min:         m.Value,
					Max:         m.Value,
					Sum:         m.Value,
					Count:       1,
				}
				a.buckets[key] = bucket
			}
			// Fall through to update existing overflow bucket below.
		} else {
			windowStart := m.Timestamp.Truncate(a.windowSize)
			bucket = &storage.MetricBucket{
				Name:           m.Name,
				ServiceName:    m.ServiceName,
				TimeBucket:     windowStart,
				Min:            m.Value,
				Max:            m.Value,
				Sum:            m.Value,
				Count:          1,
				AttributesJSON: storage.CompressedText(attrJSON),
			}
			a.buckets[key] = bucket
			return
		}
	}

	if m.Value < bucket.Min {
		bucket.Min = m.Value
	}
	if m.Value > bucket.Max {
		bucket.Max = m.Value
	}
	bucket.Sum += m.Value
	bucket.Count++
}

// BucketCount returns the current number of in-memory buckets (for metrics/health).
func (a *Aggregator) BucketCount() int {
	a.mu.Lock()
	n := len(a.buckets)
	a.mu.Unlock()
	return n
}

// DroppedBatches returns the total number of batches dropped due to a full flush channel.
func (a *Aggregator) DroppedBatches() int64 {
	return a.droppedBatches
}

// flush moves the current buckets to the flush channel and resets the in-memory map.
func (a *Aggregator) flush() {
	a.mu.Lock()
	if len(a.buckets) == 0 {
		a.mu.Unlock()
		return
	}

	batch := a.pool.Get().([]storage.MetricBucket)
	for _, b := range a.buckets {
		batch = append(batch, *b)
	}
	a.buckets = make(map[string]*storage.MetricBucket)
	a.mu.Unlock()

	select {
	case a.flushChan <- batch:
	default:
		a.droppedBatches++
		slog.Warn("⚠️ TSDB flush channel full, dropping metric batch", "count", len(batch), "total_dropped", a.droppedBatches)
		batch = batch[:0]
		a.pool.Put(batch)
	}
}

// persistenceWorker drains the flush channel and writes batches to the database.
func (a *Aggregator) persistenceWorker(ctx context.Context) {
	for {
		select {
		case batch := <-a.flushChan:
			if len(batch) == 0 {
				a.pool.Put(batch[:0])
				continue
			}
			err := a.repo.BatchCreateMetrics(batch)
			if err != nil {
				slog.Error("❌ Failed to persist metric batch", "error", err, "count", len(batch))
			} else {
				slog.Debug("💾 TSDB persisted metric batch", "count", len(batch))
			}
			batch = batch[:0]
			a.pool.Put(batch)
		case <-ctx.Done():
			return
		}
	}
}
