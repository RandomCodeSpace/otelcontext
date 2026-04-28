package tsdb

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// RawMetric represents an incoming single metric data point before aggregation.
type RawMetric struct {
	Name        string
	ServiceName string
	Value       float64
	Timestamp   time.Time
	Attributes  map[string]any
	// TenantID identifies the owning tenant for this point. When empty the
	// DB default ("default") applies at persist time.
	TenantID string
}

// Aggregator manages in-memory tumbling windows for metrics.
type Aggregator struct {
	repo           *storage.Repository
	windowSize     time.Duration
	buckets        map[string]*storage.MetricBucket
	mu             sync.Mutex
	stopChan       chan struct{}
	flushChan chan []storage.MetricBucket
	pool      sync.Pool
	// droppedBatches is incremented in flush() (under no lock other than
	// the outer mu — but the increment path runs after the unlock) and
	// read by DroppedBatches() concurrently from telemetry scrape paths.
	// atomic.Int64 makes the unsynchronized read safe under the Go memory
	// model.
	droppedBatches atomic.Int64

	// Cardinality controls.
	//
	// maxCardinality is the GLOBAL series budget across all tenants;
	// when exceeded, new series go to a single shared overflow bucket
	// keyed by overflowKey ("__cardinality_overflow__"). Preserves
	// backward compatibility for single-tenant deployments.
	//
	// perTenantCardinality is the PER-TENANT series budget. When set
	// (>0), it is checked first: a tenant exceeding its own cap routes
	// to a tenant-specific overflow bucket so a noisy tenant cannot
	// starve siblings of fresh series. seriesPerTenant counts unique
	// (non-overflow) bucket keys per tenant and is reset by flush().
	maxCardinality       int                    // 0 = unlimited
	perTenantCardinality int                    // 0 = unlimited (global cap still applies)
	cardinalityOverflow  func(tenantID string)  // labeled per overflow event for Prometheus
	seriesPerTenant      map[string]int         //nolint:unused // touched only via mu
	overflowKey          string                 // constant key for the global overflow bucket

	// Ring buffer accelerator (optional)
	ring *RingBuffer

	// Metric callbacks
	onIngest  func() // TSDBIngestTotal.Inc()
	onDropped func() // TSDBBatchesDropped.Inc()
}

const persistenceWorkers = 3

// overflowSentinelGlobal is the label value emitted on the per-tenant
// overflow counter when the GLOBAL cap (not a per-tenant cap) is the
// one that triggered. Distinguishes "this tenant got too noisy" from
// "the whole instance is at series budget".
const overflowSentinelGlobal = "__global__"

// NewAggregator creates a new TSDB aggregator.
func NewAggregator(repo *storage.Repository, windowSize time.Duration) *Aggregator {
	a := &Aggregator{
		repo:            repo,
		windowSize:      windowSize,
		buckets:         make(map[string]*storage.MetricBucket),
		seriesPerTenant: make(map[string]int),
		stopChan:        make(chan struct{}),
		flushChan:       make(chan []storage.MetricBucket, 500),
		overflowKey:     "__cardinality_overflow__",
	}
	a.pool.New = func() any {
		return make([]storage.MetricBucket, 0, 100)
	}
	return a
}

// SetCardinalityLimit configures the global and per-tenant series caps.
//
//	global       — total distinct series across all tenants; 0 = unlimited.
//	perTenant    — distinct series per tenant; 0 = unlimited (global only).
//	onOverflow   — called once per overflow event with the tenant ID
//	               that exceeded its cap, or overflowSentinelGlobal when
//	               the global cap (not per-tenant) is the trigger.
//
// Pass nil for onOverflow to disable the callback. Either cap may be 0
// independently.
func (a *Aggregator) SetCardinalityLimit(global, perTenant int, onOverflow func(tenantID string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.maxCardinality = global
	a.perTenantCardinality = perTenant
	a.cardinalityOverflow = onOverflow
}

// SetRingBuffer attaches a RingBuffer that receives every ingested data point.
func (a *Aggregator) SetRingBuffer(rb *RingBuffer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ring = rb
}

// SetMetrics wires Prometheus metric callbacks.
func (a *Aggregator) SetMetrics(onIngest, onDropped func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onIngest = onIngest
	a.onDropped = onDropped
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
	// Include tenant in the key so points from different tenants never merge.
	key := fmt.Sprintf("%s|%s|%s|%s", m.TenantID, m.ServiceName, m.Name, string(attrJSON))

	// Feed ring buffer and metric counter outside the lock (both are thread-safe).
	if a.ring != nil {
		a.ring.Record(m.Name, m.ServiceName, m.Value, m.Timestamp)
	}
	if a.onIngest != nil {
		a.onIngest()
	}

	// Capture overflow tenant under the lock; fire the callback AFTER
	// unlock so a slow callback can't serialize Ingest. Currently the
	// callback is a Prometheus increment (atomic, fast) but holding the
	// lock across an external function call is a footgun for future
	// changes.
	var overflowTenant string

	a.mu.Lock()

	bucket, exists := a.buckets[key]
	if !exists {
		// Cardinality enforcement order:
		//   1. Per-tenant cap — checked first so a noisy tenant gets
		//      bounded BEFORE the global pool is touched. Routes to a
		//      tenant-specific overflow bucket so each tenant's overflow
		//      stats stay separate.
		//   2. Global cap — backstop. When triggered, routes to the
		//      single shared overflow bucket and labels the metric with
		//      the __global__ sentinel.
		overTenantCap := a.perTenantCardinality > 0 && a.seriesPerTenant[m.TenantID] >= a.perTenantCardinality
		overGlobalCap := a.maxCardinality > 0 && len(a.buckets) >= a.maxCardinality

		switch {
		case overTenantCap:
			overflowTenant = m.TenantID
			key = a.overflowKey + "|" + m.TenantID
			bucket = a.buckets[key]
			if bucket == nil {
				windowStart := m.Timestamp.Truncate(a.windowSize)
				bucket = &storage.MetricBucket{
					TenantID:    m.TenantID,
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
		case overGlobalCap:
			overflowTenant = overflowSentinelGlobal
			key = a.overflowKey
			bucket = a.buckets[key]
			if bucket == nil {
				windowStart := m.Timestamp.Truncate(a.windowSize)
				bucket = &storage.MetricBucket{
					TenantID:    m.TenantID,
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
		default:
			windowStart := m.Timestamp.Truncate(a.windowSize)
			bucket = &storage.MetricBucket{
				TenantID:       m.TenantID,
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
			a.seriesPerTenant[m.TenantID]++
			a.mu.Unlock()
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
	a.mu.Unlock()

	if overflowTenant != "" && a.cardinalityOverflow != nil {
		a.cardinalityOverflow(overflowTenant)
	}
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
	return a.droppedBatches.Load()
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
	// Per-tenant counts track only non-overflow series in the live
	// buckets map. Reset alongside it so the next window starts fresh.
	a.seriesPerTenant = make(map[string]int)
	a.mu.Unlock()

	select {
	case a.flushChan <- batch:
	default:
		dropped := a.droppedBatches.Add(1)
		if a.onDropped != nil {
			a.onDropped()
		}
		slog.Warn("⚠️ TSDB flush channel full, dropping metric batch", "count", len(batch), "total_dropped", dropped)
		batch = batch[:0]
		a.pool.Put(batch) //nolint:staticcheck // SA6002: []T in sync.Pool is intentional; proper refactor would change channel semantics
	}
}

// persistenceWorker drains the flush channel and writes batches to the database.
func (a *Aggregator) persistenceWorker(ctx context.Context) {
	for {
		select {
		case batch := <-a.flushChan:
			if len(batch) == 0 {
				a.pool.Put(batch[:0]) //nolint:staticcheck // SA6002: see flush() for rationale
				continue
			}
			err := a.repo.BatchCreateMetrics(batch)
			if err != nil {
				slog.Error("❌ Failed to persist metric batch", "error", err, "count", len(batch))
			} else {
				slog.Debug("💾 TSDB persisted metric batch", "count", len(batch))
			}
			batch = batch[:0]
			a.pool.Put(batch) //nolint:staticcheck // SA6002: see flush() for rationale
		case <-ctx.Done():
			return
		}
	}
}
