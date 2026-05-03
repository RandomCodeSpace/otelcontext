package ingest

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

// SignalType identifies the OTLP signal a Batch carries. The label
// is exported on pipeline metrics so operators can attribute drops.
type SignalType uint8

const (
	SignalTraces SignalType = iota
	SignalLogs
)

// signalLabel returns the metric-label form of a SignalType.
func signalLabel(t SignalType) string {
	switch t {
	case SignalTraces:
		return "traces"
	case SignalLogs:
		return "logs"
	default:
		return "unknown"
	}
}

// Batch is the unit of work flowing through the async ingest Pipeline.
// One Batch corresponds to the persistable output of a single OTLP
// Export() call. Trace insertion ordering (Traces → Spans → Logs) is
// honored by the worker that processes the batch — packaging the three
// slices together preserves the FK invariant the synchronous path
// already enforces.
type Batch struct {
	Type   SignalType
	Tenant string

	Traces []storage.Trace
	Spans  []storage.Span
	Logs   []storage.Log

	// Priority flags. Errors and slow traces are protected from soft
	// backpressure drops — they may still be rejected at hard capacity.
	HasError bool
	HasSlow  bool

	// Optional per-record callbacks invoked after a successful DB write.
	// In production these feed GraphRAG ingestion. Nil callbacks are
	// skipped silently.
	SpanCallback func(storage.Span)
	LogCallback  func(storage.Log)

	enqueuedAt time.Time
}

// Priority reports whether the batch is protected from soft-backpressure
// drops. Used by Submit() to decide whether to enqueue at >= 90% fullness.
func (b *Batch) Priority() bool { return b.HasError || b.HasSlow }

// ErrQueueFull is returned by Submit when the queue is at hard capacity
// (100% full). Callers should map this to gRPC RESOURCE_EXHAUSTED or
// HTTP 429 with a Retry-After hint so OTLP clients back off cleanly.
var ErrQueueFull = errors.New("ingest pipeline at capacity")

// PipelineConfig holds the tunables for a Pipeline.
type PipelineConfig struct {
	Capacity      int     // total queue depth across all signal types
	Workers       int     // worker goroutines draining the queue
	SoftThreshold float64 // fullness fraction above which healthy batches are dropped (0.0–1.0)
}

// Defensive upper bounds on operator-supplied capacity/workers. Env-var
// inputs go directly into a make(chan ...) and into goroutine launches;
// without a sanity cap a typo like INGEST_PIPELINE_QUEUE_SIZE=10_000_000_000
// would OOM the process. These caps are well above any reasonable
// production deployment (50k is the default queue, 8 the default workers)
// while still keeping the allocation finite.
const (
	maxPipelineCapacity = 1_000_000
	maxPipelineWorkers  = 256
)

// DefaultPipelineConfig returns production-sized defaults.
func DefaultPipelineConfig() PipelineConfig {
	return PipelineConfig{
		Capacity:      50000,
		Workers:       8,
		SoftThreshold: 0.9,
	}
}

// pipelineWriter is the slice of *storage.Repository the Pipeline depends
// on. Defining it as an interface keeps the package layering clean and
// lets tests inject fakes without spinning up SQLite.
//
// The async pipeline drives only BatchCreateAll. The single-signal
// methods are kept on the interface for forward-compatibility with
// callers that may construct a writer directly (e.g. backfill tools);
// they aren't on the hot ingest path anymore.
type pipelineWriter interface {
	BatchCreateTraces(traces []storage.Trace) error
	BatchCreateSpans(spans []storage.Span) error
	BatchCreateLogs(logs []storage.Log) error
	// BatchCreateAll persists all three signal slices as a single atomic
	// transaction. A failure (or panic) anywhere in the chain rolls back
	// the entire batch, preventing orphan FK rows.
	BatchCreateAll(traces []storage.Trace, spans []storage.Span, logs []storage.Log) error
}

// Pipeline decouples OTLP Export() from synchronous DB writes. It holds a
// bounded buffered channel of Batches, a worker pool that drains the
// channel into the Repository, and Prometheus instruments that surface
// queue depth, drop counts, and rejection counts.
//
// Lifecycle:
//
//	p := NewPipeline(repo, metrics, cfg)
//	p.Start(ctx)
//	defer p.Stop()       // drains in-flight before returning
//	p.Submit(batch)
type Pipeline struct {
	writer  pipelineWriter
	metrics *telemetry.Metrics

	cfg   PipelineConfig
	queue chan *Batch

	// Stats — exported via accessors for tests and for the /metrics path
	// that doesn't already cover pipeline counters.
	enqueuedTotal   atomic.Int64
	processedTotal  atomic.Int64
	droppedHealthy  atomic.Int64
	rejectedFull    atomic.Int64
	processFailures atomic.Int64
	tenantDropped   atomic.Int64

	// Per-tenant in-flight cap — bounds the queue slots a single tenant
	// can consume so a noisy tenant cannot starve siblings of fresh
	// healthy submissions when fullness is below the soft threshold.
	// Priority batches (errors/slow) bypass the cap because diagnostic
	// data must always land. perTenantCap == 0 disables the check.
	perTenantCap   int
	tenantMu       sync.Mutex
	tenantInFlight map[string]int

	// storeMinSeverity is the second-tier severity gate applied at persist
	// time inside process(). Logs in a Batch with severity below this
	// threshold are dropped from the BatchCreateAll write but still feed
	// the LogCallback (so vectordb / GraphRAG / Drain mining still see
	// them). 0 disables the second tier — every log that survived
	// IngestMinSeverity at the receiver is also persisted.
	storeMinSeverity int
	storeFiltered    atomic.Int64

	stopCh chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

// NewPipeline constructs a Pipeline with the given config, falling back
// to DefaultPipelineConfig() values for non-positive fields. The
// Pipeline does NOT start workers — call Start(ctx) when ready.
func NewPipeline(writer pipelineWriter, metrics *telemetry.Metrics, cfg PipelineConfig) *Pipeline {
	d := DefaultPipelineConfig()
	if cfg.Capacity <= 0 {
		cfg.Capacity = d.Capacity
	}
	if cfg.Workers <= 0 {
		cfg.Workers = d.Workers
	}
	// Sanitize operator-supplied capacity/workers BEFORE the make()/Workers
	// loop. CodeQL's taint-tracking treats env-var-derived values as untrusted
	// for go/uncontrolled-allocation-size; only an explicit comparison guard
	// is recognized as a BarrierGuard sanitizer. Both ceilings are well above
	// any reasonable deployment (50k default queue, 8 default workers) but
	// keep the allocation bounded against a misconfigured env var.
	capacity := cfg.Capacity
	if capacity > maxPipelineCapacity {
		slog.Warn("ingest pipeline: capacity clamped to defensive ceiling",
			"requested", capacity,
			"max", maxPipelineCapacity,
		)
		capacity = maxPipelineCapacity
	}
	workers := cfg.Workers
	if workers > maxPipelineWorkers {
		slog.Warn("ingest pipeline: workers clamped to defensive ceiling",
			"requested", workers,
			"max", maxPipelineWorkers,
		)
		workers = maxPipelineWorkers
	}
	cfg.Capacity = capacity
	cfg.Workers = workers
	// Zero-value config falls back to defaults — the field is internal
	// (no env-var surface) and TestPipeline_DefaultsApplied enforces this.
	// Priority-only mode (always-soft-drop) is not a supported configuration
	// via PipelineConfig{SoftThreshold:0}.
	if cfg.SoftThreshold <= 0 || cfg.SoftThreshold >= 1.0 {
		cfg.SoftThreshold = d.SoftThreshold
	}
	return &Pipeline{
		writer:         writer,
		metrics:        metrics,
		cfg:            cfg,
		queue:          make(chan *Batch, capacity),
		tenantInFlight: make(map[string]int),
		stopCh:         make(chan struct{}),
	}
}

// SetPerTenantCap configures the maximum in-flight batches one tenant may
// hold in the queue (and currently being processed). 0 disables the cap.
// Once a tenant hits the cap, further healthy submissions from that tenant
// are dropped at Submit() time with reason "tenant_backpressure". Priority
// batches (errors/slow traces) bypass the cap.
//
// Sized as a fraction of Capacity, e.g. Capacity/4 keeps any single tenant
// to 25% of queue capacity. Operators tune via INGEST_PIPELINE_PER_TENANT_CAP.
// Startup-only — call before Start().
func (p *Pipeline) SetPerTenantCap(n int) {
	if n < 0 {
		n = 0
	}
	p.perTenantCap = n
}

// SetStoreMinSeverity configures the second-tier severity gate applied at
// persist time. Logs below `level` are dropped from the BatchCreateAll write
// but still flow through the LogCallback so in-memory consumers (vectordb,
// GraphRAG Drain mining, anomaly correlation) keep working. 0 disables the
// second tier — every log surviving IngestMinSeverity at the receiver is
// also persisted (legacy behavior).
//
// `level` is the integer rank from parseSeverity ("DEBUG"=10 .. "FATAL"=50).
// Startup-only — call before Start().
func (p *Pipeline) SetStoreMinSeverity(level int) {
	if level < 0 {
		level = 0
	}
	p.storeMinSeverity = level
}

// TenantDropped reports the cumulative number of healthy submissions
// rejected because the submitting tenant was at the per-tenant cap.
// Distinct from RejectedFull (queue at hard capacity) and
// DroppedHealthy (soft-backpressure across the whole queue).
func (p *Pipeline) TenantDropped() int64 { return p.tenantDropped.Load() }

// Start spawns the worker pool. Safe to call once. Subsequent calls are
// no-ops; tests rely on this for reset semantics.
func (p *Pipeline) Start(ctx context.Context) {
	for range p.cfg.Workers {
		p.wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("ingest pipeline worker panic",
						"panic", r,
						"stack", string(debug.Stack()),
					)
					if p.metrics != nil && p.metrics.PanicsRecoveredTotal != nil {
						p.metrics.PanicsRecoveredTotal.WithLabelValues("ingest_pipeline").Inc()
					}
				}
			}()
			p.worker(ctx)
		})
	}
}

// Submit enqueues a batch for asynchronous persistence. Returns nil when
// the batch is accepted (or silently dropped under soft backpressure)
// and ErrQueueFull when the queue is at hard capacity. Nil batches are
// no-ops.
//
// Soft backpressure: when fullness >= SoftThreshold, healthy batches
// (Priority()==false) are dropped at the door and Submit returns nil so
// the OTLP client sees a successful Export. Errors and slow traces
// always continue to the channel.
//
// Hard backpressure: when the channel send fails (buffer at 100%),
// Submit returns ErrQueueFull regardless of priority. The caller should
// translate this into a backpressure signal so the client retries with
// exponential backoff rather than tighter loops.
func (p *Pipeline) Submit(b *Batch) error {
	if b == nil {
		return nil
	}
	if len(b.Traces) == 0 && len(b.Spans) == 0 && len(b.Logs) == 0 {
		// Empty batch — nothing to persist. Skip the channel entirely.
		return nil
	}
	b.enqueuedAt = time.Now()

	fullness := float64(len(p.queue)) / float64(p.cfg.Capacity)
	if fullness >= p.cfg.SoftThreshold && !b.Priority() {
		p.droppedHealthy.Add(1)
		p.observeDrop(b.Type, "soft_backpressure")
		return nil
	}

	// Per-tenant cap — only enforced for healthy batches (priority bypasses,
	// same as soft-backpressure). Reserve the slot under the lock so the
	// counter and the channel send are coherent: if the channel is full,
	// undo the reservation in the default branch below.
	tenantReserved := false
	if p.perTenantCap > 0 && b.Tenant != "" && !b.Priority() {
		p.tenantMu.Lock()
		if p.tenantInFlight[b.Tenant] >= p.perTenantCap {
			p.tenantMu.Unlock()
			p.tenantDropped.Add(1)
			p.observeDrop(b.Type, "tenant_backpressure")
			return nil
		}
		p.tenantInFlight[b.Tenant]++
		tenantReserved = true
		p.tenantMu.Unlock()
	}

	select {
	case p.queue <- b:
		p.enqueuedTotal.Add(1)
		p.observeQueueDepth(b.Type)
		return nil
	default:
		if tenantReserved {
			p.releaseTenantSlot(b.Tenant)
		}
		p.rejectedFull.Add(1)
		p.observeDrop(b.Type, "queue_full")
		return ErrQueueFull
	}
}

// releaseTenantSlot decrements the in-flight count for a tenant, removing
// the map entry when it hits zero so the map doesn't grow unboundedly with
// short-lived tenant IDs. Safe to call with an empty tenant or when the
// cap is disabled — both no-op.
func (p *Pipeline) releaseTenantSlot(tenant string) {
	if p.perTenantCap <= 0 || tenant == "" {
		return
	}
	p.tenantMu.Lock()
	n := p.tenantInFlight[tenant] - 1
	if n <= 0 {
		delete(p.tenantInFlight, tenant)
	} else {
		p.tenantInFlight[tenant] = n
	}
	p.tenantMu.Unlock()
}

// Stop signals workers to exit and blocks until in-flight batches have
// been drained from the channel. Idempotent.
func (p *Pipeline) Stop() {
	p.once.Do(func() {
		close(p.stopCh)
	})
	p.wg.Wait()
}

// Stats returns snapshot counters for tests and for telemetry that
// doesn't already use Prometheus instruments. The values are best-effort
// and not synchronized across atomics — sufficient for diagnostics.
func (p *Pipeline) Stats() PipelineStats {
	return PipelineStats{
		Enqueued:        p.enqueuedTotal.Load(),
		Processed:       p.processedTotal.Load(),
		DroppedHealthy:  p.droppedHealthy.Load(),
		RejectedFull:    p.rejectedFull.Load(),
		ProcessFailures: p.processFailures.Load(),
		StoreFiltered:   p.storeFiltered.Load(),
		QueueDepth:      len(p.queue),
		Capacity:        p.cfg.Capacity,
	}
}

// PipelineStats is a snapshot of pipeline counters.
type PipelineStats struct {
	Enqueued        int64
	Processed       int64
	DroppedHealthy  int64
	RejectedFull    int64
	ProcessFailures int64
	StoreFiltered   int64 // logs dropped by STORE_MIN_SEVERITY at persist time
	QueueDepth      int
	Capacity        int
}

// worker drains the queue. Exits when stopCh closes (after draining
// remaining batches) or when ctx is canceled (immediate).
func (p *Pipeline) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-p.queue:
			p.process(b)
		case <-p.stopCh:
			// Drain remaining buffered batches synchronously so a
			// graceful shutdown doesn't lose in-flight ingest.
			for {
				select {
				case b := <-p.queue:
					p.process(b)
				default:
					return
				}
			}
		}
	}
}

// process persists a single batch in a single DB transaction. Trace→Span→Log
// ordering inside the transaction mirrors the FK invariant of the synchronous
// Export() path; atomicity prevents the orphan-row class of bugs where a
// panic between two BatchCreate* calls left a parent row with no children
// (or vice versa). Any failure rolls the entire batch back; the worker logs,
// increments processFailures, and drops the batch (DLQ is the redundancy
// story for sustained failures).
//
// Behavior change vs. the pre-tx implementation: trace insert errors are no
// longer "tolerated" with downstream spans/logs continuing — the whole batch
// is now atomic. This is intentional. Traces are idempotent (ON CONFLICT
// DO NOTHING), so a DLQ retry of the same envelope re-attempts cleanly.
func (p *Pipeline) process(b *Batch) {
	if b == nil {
		return
	}
	// Release the per-tenant slot reserved at Submit time. Registered as
	// a defer so it runs even if the batch panics. Priority batches don't
	// reserve at submit, so they don't release here either — the conditions
	// must mirror exactly to keep the in-flight count balanced.
	if !b.Priority() {
		defer p.releaseTenantSlot(b.Tenant)
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("ingest pipeline process panic",
				"panic", r,
				"stack", string(debug.Stack()),
			)
			p.processFailures.Add(1)
			if p.metrics != nil && p.metrics.PanicsRecoveredTotal != nil {
				p.metrics.PanicsRecoveredTotal.WithLabelValues("ingest_pipeline").Inc()
			}
		}
	}()
	p.processedTotal.Add(1)

	if len(b.Traces) == 0 && len(b.Spans) == 0 && len(b.Logs) == 0 {
		return
	}

	// Apply the second-tier store-severity gate. Logs below the threshold
	// are dropped from the persist set but still flow through the callback
	// so in-memory enrichers (vectordb, GraphRAG Drain) keep seeing them.
	logsToPersist := b.Logs
	if p.storeMinSeverity > 0 && len(b.Logs) > 0 {
		kept := make([]storage.Log, 0, len(b.Logs))
		for _, l := range b.Logs {
			if shouldIngestSeverity(l.Severity, p.storeMinSeverity) {
				kept = append(kept, l)
			} else {
				p.storeFiltered.Add(1)
			}
		}
		logsToPersist = kept
	}

	if err := p.writer.BatchCreateAll(b.Traces, b.Spans, logsToPersist); err != nil {
		slog.Error("ingest pipeline: BatchCreateAll failed", "error", err)
		p.processFailures.Add(1)
		return
	}

	// Callbacks fire only after the transaction commits successfully — a
	// rolled-back batch must not feed downstream consumers (GraphRAG etc.)
	// data that no longer exists in the DB. The LogCallback intentionally
	// iterates over the FULL b.Logs slice, not logsToPersist — even logs
	// dropped by the store-severity gate must reach in-memory enrichers.
	if b.SpanCallback != nil {
		for _, s := range b.Spans {
			b.SpanCallback(s)
		}
	}
	if b.LogCallback != nil {
		for _, l := range b.Logs {
			b.LogCallback(l)
		}
	}
}

func (p *Pipeline) observeQueueDepth(t SignalType) {
	if p.metrics == nil || p.metrics.IngestPipelineQueueDepth == nil {
		return
	}
	p.metrics.IngestPipelineQueueDepth.WithLabelValues(signalLabel(t)).Set(float64(len(p.queue)))
}

func (p *Pipeline) observeDrop(t SignalType, reason string) {
	if p.metrics == nil || p.metrics.IngestPipelineDroppedTotal == nil {
		return
	}
	p.metrics.IngestPipelineDroppedTotal.WithLabelValues(signalLabel(t), reason).Inc()
}
