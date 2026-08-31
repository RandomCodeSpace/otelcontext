package ingest

import (
	"context"
	"errors"
	"fmt"
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

	// Reservation carries the exemplar bytes reserved for the rows in this
	// batch (#201 Q4). submitExemplars commits it when a destination accepts
	// the batch and releases it when none does. Nil outside aggregate mode,
	// and every method on it is nil-safe.
	Reservation *ExemplarReservation

	enqueuedAt time.Time

	// sizeBytes is the approxBytes() estimate computed once at Submit time
	// and released by process() — keeping the reservation and the release
	// reading the same number even if the slices are mutated in between.
	sizeBytes int64
}

// approxBytes estimates the heap footprint of the batch without marshaling.
// Per-record fixed costs approximate struct overhead (time.Time fields,
// GORM bookkeeping, slice/string headers); variable costs are the lengths
// of the dominant string payloads — Body and AttributesJSON are what make
// a batch fat. AttributesJSON/AIInsight are storage.CompressedText (string
// kind), so len() over the string cast is O(1). Whole walk is O(records).
func (b *Batch) approxBytes() int64 {
	var n int64
	for i := range b.Traces {
		t := &b.Traces[i]
		n += 128 + int64(len(t.TraceID)+len(t.ServiceName)+len(t.Status)+len(t.Operation))
	}
	for i := range b.Spans {
		s := &b.Spans[i]
		n += 256 + int64(len(s.OperationName)+len(string(s.AttributesJSON))+len(s.ServiceName)+len(s.Status))
	}
	for i := range b.Logs {
		l := &b.Logs[i]
		n += 192 + int64(len(l.Body)+len(string(l.AttributesJSON))+len(string(l.AIInsight))+len(l.ServiceName)+len(l.Severity))
	}
	return n
}

// Priority reports whether the batch is protected from soft-backpressure
// drops. Used by Submit() to decide whether to enqueue at >= 90% fullness.
func (b *Batch) Priority() bool { return b.HasError || b.HasSlow }

// ErrQueueFull is returned by Submit when the queue is at hard capacity
// (100% full). Callers should map this to gRPC RESOURCE_EXHAUSTED or
// HTTP 429 with a Retry-After hint so OTLP clients back off cleanly.
var ErrQueueFull = errors.New("ingest pipeline at capacity")

// ErrNoDLQ is returned by OfferToDLQ when no DLQ sink is wired. The batch is
// gone; the caller must count it as permanent loss.
var ErrNoDLQ = errors.New("no dead letter queue configured")

// ErrDLQFull is the sentinel a BatchSink returns when it refuses a batch
// because its bounded budget is exhausted, as opposed to the write itself
// failing. *queue.DeadLetterQueue evicts FIFO rather than refusing, so this
// is produced only by sinks that choose strict bounding — it exists so the
// exemplar loss metric can tell "nowhere to put it" from "the write broke".
var ErrDLQFull = errors.New("dead letter queue at capacity")

// SubmitOutcome is the success value of Submit. A hard rejection stays an
// error (ErrQueueFull); this type distinguishes the two ways Submit can
// succeed without the caller having to infer them from counters.
//
// The distinction is load-bearing for the aggregate-shadow ordering state
// machine (#196 Q4): both outcomes are non-retry Export results, so both
// permit the shadow aggregate to be applied exactly once, but only
// SubmitEnqueued means the rows will actually reach the database.
type SubmitOutcome uint8

const (
	// SubmitEnqueued — the batch holds a queue slot and a worker will
	// persist it. Also returned for a nil or empty batch, which needs no
	// slot and loses nothing.
	SubmitEnqueued SubmitOutcome = iota
	// SubmitSoftDropped — the batch was intentionally shed at the door by
	// soft backpressure or the per-tenant admission cap. Already counted on
	// otelcontext_ingest_pipeline_dropped_total; the Export still succeeds.
	SubmitSoftDropped
)

// String renders the outcome for logs and test failure messages.
func (o SubmitOutcome) String() string {
	switch o {
	case SubmitEnqueued:
		return "enqueued"
	case SubmitSoftDropped:
		return "soft_dropped"
	default:
		return "unknown"
	}
}

// DLQBatchType is the `type` discriminator of the typed DLQ envelope used
// for a whole failed pipeline batch. It extends the existing
// {"type":"logs|spans|traces|metrics","data":[...]} family with a fifth
// shape whose `data` is an object rather than an array, because the three
// signal slices of one Batch must be replayed through a single
// BatchCreateAll transaction to preserve the Trace->Span->Log FK ordering
// the pipeline enforces on the primary path.
const DLQBatchType = "batch"

// DLQBatchPayload is the `data` member of a DLQBatchType envelope.
type DLQBatchPayload struct {
	Tenant string          `json:"tenant,omitempty"`
	Signal string          `json:"signal,omitempty"`
	Traces []storage.Trace `json:"traces,omitempty"`
	Spans  []storage.Span  `json:"spans,omitempty"`
	Logs   []storage.Log   `json:"logs,omitempty"`
}

// DLQBatchEnvelope is the on-disk form of a batch the pipeline could not
// persist. main.go's replay handler decodes it and re-runs BatchCreateAll.
type DLQBatchEnvelope struct {
	Type string          `json:"type"`
	Data DLQBatchPayload `json:"data"`
}

// BatchSink is the slice of the Dead Letter Queue the Pipeline depends on.
// *queue.DeadLetterQueue satisfies it. Declaring it here (rather than
// importing internal/queue) keeps the package layering one-directional and
// lets tests inject a fake without touching the filesystem.
type BatchSink interface {
	Enqueue(batch any) error
}

// PipelineConfig holds the tunables for a Pipeline.
type PipelineConfig struct {
	Capacity      int     // total queue depth across all signal types
	Workers       int     // worker goroutines draining the queue
	SoftThreshold float64 // fullness fraction above which healthy batches are dropped (0.0–1.0)
	// MaxBytes caps the approximate bytes held by queued batches. Capacity
	// alone cannot bound memory — a single Batch may carry arbitrarily
	// large span/log payloads. At the cap, Submit rejects with ErrQueueFull
	// even for priority batches: a 429 is recoverable by the client's retry
	// loop, an OOM kill is not. <=0 falls back to the 512MB default.
	MaxBytes int64
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
	// minPipelineMaxBytes is the floor for the byte cap. A sub-1MB cap
	// would reject every gRPC batch outright (GRPC_MAX_RECV_MB default is
	// 16) — clamp up instead of letting a typo black-hole all ingest.
	minPipelineMaxBytes = 1 << 20
)

// DefaultPipelineConfig returns production-sized defaults.
func DefaultPipelineConfig() PipelineConfig {
	return PipelineConfig{
		Capacity:      50000,
		Workers:       8,
		SoftThreshold: 0.9,
		MaxBytes:      512 << 20,
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
	rejectedBytes   atomic.Int64
	processFailures atomic.Int64
	tenantDropped   atomic.Int64
	dlqEnqueued     atomic.Int64
	dlqFailed       atomic.Int64

	// inFlightBytes tracks the approxBytes() sum of every batch currently
	// in the queue or being processed. Reserved in Submit before the
	// channel send, released by process() (deferred, so the panic path
	// releases too). Bounded by cfg.MaxBytes.
	inFlightBytes atomic.Int64

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

	// dlq receives batches whose persist transaction failed. Nil (the
	// default) keeps the historical behaviour — log, count, drop — which is
	// what the pipeline's own tests rely on.
	dlq BatchSink

	stopCh chan struct{}
	once   sync.Once
	wg     sync.WaitGroup

	startOnce   sync.Once
	lifecycleMu sync.Mutex
	started     bool
	workersDone chan struct{}
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
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = d.MaxBytes
	}
	if cfg.MaxBytes < minPipelineMaxBytes {
		slog.Warn("ingest pipeline: max bytes clamped up to floor — a sub-1MB cap would reject every gRPC batch",
			"requested", cfg.MaxBytes,
			"min", int64(minPipelineMaxBytes),
		)
		cfg.MaxBytes = minPipelineMaxBytes
	}
	// Zero-value config falls back to defaults — the field is internal
	// (no env-var surface) and TestPipeline_DefaultsApplied enforces this.
	// Priority-only mode (always-soft-drop) is not a supported configuration
	// via PipelineConfig{SoftThreshold:0}.
	if cfg.SoftThreshold <= 0 || cfg.SoftThreshold >= 1.0 {
		cfg.SoftThreshold = d.SoftThreshold
	}
	if metrics != nil && metrics.IngestPipelineDroppedTotal != nil {
		for _, signal := range []SignalType{SignalTraces, SignalLogs} {
			for _, reason := range []string{"soft_backpressure", "tenant_backpressure", "bytes_full", "queue_full"} {
				metrics.IngestPipelineDroppedTotal.WithLabelValues(signalLabel(signal), reason)
			}
		}
	}
	return &Pipeline{
		writer:         writer,
		metrics:        metrics,
		cfg:            cfg,
		queue:          make(chan *Batch, capacity),
		tenantInFlight: make(map[string]int),
		stopCh:         make(chan struct{}),
		workersDone:    make(chan struct{}),
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

// SetDLQ wires the Dead Letter Queue that receives batches whose persist
// transaction failed. Without it the pipeline logs the failure and drops the
// batch (the pre-#194 behaviour, still the default so tests and the
// synchronous fallback path are unaffected).
//
// Replay is at-least-once, not exactly-once. Traces and spans collapse
// duplicates on their composite unique indexes, so replaying an
// already-persisted batch is a no-op for them; logs have no stable OTLP
// identifier and a replay after a partially-visible commit can duplicate log
// rows. That trade is deliberate — losing the batch outright is worse.
//
// Startup-only — call before Start().
func (p *Pipeline) SetDLQ(sink BatchSink) {
	p.dlq = sink
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
	p.startOnce.Do(func() {
		p.lifecycleMu.Lock()
		p.started = true
		p.lifecycleMu.Unlock()
		for range p.cfg.Workers {
			p.wg.Go(func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("ingest pipeline worker panic",
							"panic", r,
							"stack", string(debug.Stack()),
						)
						p.processFailures.Add(1)
						if p.metrics != nil && p.metrics.PanicsRecoveredTotal != nil {
							p.metrics.PanicsRecoveredTotal.WithLabelValues("ingest_pipeline").Inc()
						}
					}
				}()
				p.worker(ctx)
			})
		}
		go func() {
			p.wg.Wait()
			close(p.workersDone)
		}()
	})
}

// Submit enqueues a batch for asynchronous persistence. It returns a
// SubmitOutcome plus a nil error when the batch is accepted (or
// intentionally shed under soft backpressure), and ErrQueueFull when the
// queue is at hard capacity. Nil batches are no-ops.
//
// Soft backpressure: when fullness >= SoftThreshold, healthy batches
// (Priority()==false) are dropped at the door and Submit returns
// (SubmitSoftDropped, nil) so the OTLP client sees a successful Export.
// Errors and slow traces always continue to the channel.
//
// Hard backpressure: when the channel send fails (buffer at 100%) or the
// byte cap would be exceeded, Submit returns ErrQueueFull regardless of
// priority. The caller should translate this into a backpressure signal so
// the client retries with exponential backoff rather than tighter loops —
// except in AGGREGATE_MODE=aggregate, where the durable aggregate commit is
// already the ACK and the caller absorbs the rejection (see submitExemplars).
func (p *Pipeline) Submit(b *Batch) (SubmitOutcome, error) {
	if b == nil {
		return SubmitEnqueued, nil
	}
	if len(b.Traces) == 0 && len(b.Spans) == 0 && len(b.Logs) == 0 {
		// Empty batch — nothing to persist, nothing lost. Skip the channel
		// entirely and report it as enqueued: there is no drop to account for.
		return SubmitEnqueued, nil
	}
	b.enqueuedAt = time.Now()
	b.sizeBytes = b.approxBytes()

	// Soft backpressure engages on whichever dimension is more saturated:
	// item count (many small batches) or bytes (few fat batches).
	itemFullness := float64(len(p.queue)) / float64(p.cfg.Capacity)
	byteFullness := float64(p.inFlightBytes.Load()) / float64(p.cfg.MaxBytes)
	if max(itemFullness, byteFullness) >= p.cfg.SoftThreshold && !b.Priority() {
		p.droppedHealthy.Add(1)
		p.observeDrop(b.Type, "soft_backpressure")
		return SubmitSoftDropped, nil
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
			return SubmitSoftDropped, nil
		}
		p.tenantInFlight[b.Tenant]++
		tenantReserved = true
		p.tenantMu.Unlock()
	}

	// Byte-cap reservation — after the soft and tenant checks so dropped
	// batches never reserve, before the channel send so the cap is never
	// overshot. Priority batches get NO exemption here: a 429 is
	// recoverable by the OTLP client's retry loop, an OOM kill is not.
	if newTotal := p.inFlightBytes.Add(b.sizeBytes); newTotal > p.cfg.MaxBytes {
		p.inFlightBytes.Add(-b.sizeBytes)
		if tenantReserved {
			p.releaseTenantSlot(b.Tenant)
		}
		p.rejectedBytes.Add(1)
		p.observeDrop(b.Type, "bytes_full")
		return SubmitEnqueued, ErrQueueFull
	}

	select {
	case p.queue <- b:
		p.enqueuedTotal.Add(1)
		p.observeQueueDepth(b.Type)
		p.observeQueueBytes()
		return SubmitEnqueued, nil
	default:
		p.inFlightBytes.Add(-b.sizeBytes)
		if tenantReserved {
			p.releaseTenantSlot(b.Tenant)
		}
		p.rejectedFull.Add(1)
		p.observeDrop(b.Type, "queue_full")
		return SubmitEnqueued, ErrQueueFull
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

// Shutdown signals workers to exit, drains accepted batches, and reports any
// state that did not reach the database or durable DLQ before the deadline.
func (p *Pipeline) Shutdown(ctx context.Context) error {
	p.once.Do(func() {
		close(p.stopCh)
	})
	p.lifecycleMu.Lock()
	started := p.started
	p.lifecycleMu.Unlock()
	if !started {
		return nil
	}
	select {
	case <-p.workersDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	stats := p.Stats()
	if stats.QueueDepth != 0 || stats.QueueBytes != 0 {
		return fmt.Errorf("ingest pipeline not drained: queue_depth=%d queue_bytes=%d", stats.QueueDepth, stats.QueueBytes)
	}
	if stats.DLQFailed > 0 {
		return fmt.Errorf("ingest pipeline lost %d batches after DLQ refusal", stats.DLQFailed)
	}
	if unprotected := stats.ProcessFailures - stats.DLQEnqueued; unprotected > 0 {
		return fmt.Errorf("ingest pipeline has %d failed batches without durable DLQ envelopes", unprotected)
	}
	return nil
}

// Stop preserves the prior internal lifecycle surface. Production shutdown
// uses Shutdown so deadline and durability failures reach the process result.
func (p *Pipeline) Stop() {
	if err := p.Shutdown(context.Background()); err != nil {
		slog.Error("ingest pipeline shutdown failed", "error", err)
	}
}

// Stats returns snapshot counters for tests and for telemetry that
// doesn't already use Prometheus instruments. The values are best-effort
// and not synchronized across atomics — sufficient for diagnostics.
//
// Processed is the one exception and is load-bearing: it is incremented in
// process()'s outermost defer, so observing Processed >= N guarantees that
// N batches finished completely — writes attempted, callbacks fired, tenant
// slots and byte reservations released. That makes it the correct barrier
// for a caller (a test, a drain check) that wants to read state process()
// produced. Every other counter is a plain progress tally.
func (p *Pipeline) Stats() PipelineStats {
	return PipelineStats{
		Enqueued:        p.enqueuedTotal.Load(),
		Processed:       p.processedTotal.Load(),
		DroppedHealthy:  p.droppedHealthy.Load(),
		RejectedFull:    p.rejectedFull.Load(),
		RejectedBytes:   p.rejectedBytes.Load(),
		ProcessFailures: p.processFailures.Load(),
		DLQEnqueued:     p.dlqEnqueued.Load(),
		DLQFailed:       p.dlqFailed.Load(),
		StoreFiltered:   p.storeFiltered.Load(),
		QueueDepth:      len(p.queue),
		Capacity:        p.cfg.Capacity,
		QueueBytes:      p.inFlightBytes.Load(),
		MaxBytes:        p.cfg.MaxBytes,
	}
}

// PipelineStats is a snapshot of pipeline counters.
type PipelineStats struct {
	Enqueued        int64
	Processed       int64 // batches whose process() ran to completion (see Stats)
	DroppedHealthy  int64
	RejectedFull    int64
	RejectedBytes   int64 // batches rejected because the byte cap was exceeded
	ProcessFailures int64
	DLQEnqueued     int64 // batches durably handed to the DLQ (persist failure or queue saturation)
	DLQFailed       int64 // batches the DLQ itself refused (data lost)
	StoreFiltered   int64 // logs dropped by STORE_MIN_SEVERITY at persist time
	QueueDepth      int
	Capacity        int
	QueueBytes      int64 // approx bytes currently reserved by in-flight batches
	MaxBytes        int64 // configured byte cap
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
// applyStoreSeverity returns the subset of logs that clears the second-tier
// STORE_MIN_SEVERITY gate, counting every rejection on storeFiltered. Shared
// by process() (the persist path) and OfferToDLQ (the saturation path) so a
// batch handed to the DLQ carries exactly the rows a successful write would
// have persisted — replay runs BatchCreateAll directly and never re-applies
// the gate. 0 (gate disabled) returns the input slice untouched.
func (p *Pipeline) applyStoreSeverity(logs []storage.Log) []storage.Log {
	if p.storeMinSeverity <= 0 || len(logs) == 0 {
		return logs
	}
	kept := make([]storage.Log, 0, len(logs))
	for _, l := range logs {
		if shouldIngestSeverity(l.Severity, p.storeMinSeverity) {
			kept = append(kept, l)
		} else {
			p.storeFiltered.Add(1)
		}
	}
	return kept
}

func (p *Pipeline) process(b *Batch) {
	if b == nil {
		return
	}
	// processedTotal is the pipeline's completion barrier and must therefore
	// be the LAST thing process() touches — hence registered as the FIRST
	// defer (defers run LIFO). By the time it increments, the batch's write
	// has committed or failed, the callbacks have run, the per-tenant slot is
	// released and the byte reservation is returned. It used to be a plain
	// increment at the top of the function, which made Stats().Processed mean
	// "a worker picked this batch up" — a barrier for nothing.
	defer p.processedTotal.Add(1)
	// Release the byte reservation taken at Submit time. Unconditional —
	// every batch that reached the channel reserved sizeBytes, priority or
	// not — and deferred so the panic path below releases too. Mirrors the
	// reservation exactly; an asymmetry here ratchets inFlightBytes up
	// until the cap rejects all traffic.
	defer func() {
		p.inFlightBytes.Add(-b.sizeBytes)
		p.observeQueueBytes()
	}()
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

	if len(b.Traces) == 0 && len(b.Spans) == 0 && len(b.Logs) == 0 {
		return
	}

	// Apply the second-tier store-severity gate. Logs below the threshold
	// are dropped from the persist set but still flow through the callback
	// so in-memory enrichers (vectordb, GraphRAG Drain) keep seeing them.
	logsToPersist := p.applyStoreSeverity(b.Logs)

	if err := p.writer.BatchCreateAll(b.Traces, b.Spans, logsToPersist); err != nil {
		slog.Error("ingest pipeline: BatchCreateAll failed", "error", err)
		p.processFailures.Add(1)
		// Hand the complete rolled-back batch to the DLQ rather than
		// dropping it. logsToPersist (not b.Logs) is what the transaction
		// attempted, so the store-severity gate is not re-litigated on replay.
		p.toDLQ(b, logsToPersist)
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

func (p *Pipeline) observeQueueBytes() {
	if p.metrics == nil || p.metrics.IngestPipelineQueueBytes == nil {
		return
	}
	p.metrics.IngestPipelineQueueBytes.Set(float64(p.inFlightBytes.Load()))
}

func (p *Pipeline) observeDrop(t SignalType, reason string) {
	if p.metrics == nil || p.metrics.IngestPipelineDroppedTotal == nil {
		return
	}
	p.metrics.IngestPipelineDroppedTotal.WithLabelValues(signalLabel(t), reason).Inc()
}

// toDLQ hands a batch whose persist transaction failed to the DLQ. The
// outcome is counted, never returned — process() has nobody to report to.
func (p *Pipeline) toDLQ(b *Batch, logs []storage.Log) {
	_ = p.enqueueDLQ(b, logs)
}

// OfferToDLQ hands a batch the primary queue REFUSED (ErrQueueFull) to the DLQ
// sink, so a hard rejection can degrade to deferred storage instead of loss.
// It is the second half of the aggregate-mode ACK contract (#196 Q2): once the
// durable aggregate commit has landed, the Export is already acknowledged and
// the raw exemplars must go somewhere other than the client's retry loop.
//
// Returns nil when the sink accepted the batch, ErrNoDLQ when no sink is
// wired, ErrDLQFull when the sink refused for capacity, and the sink's own
// error otherwise. A non-nil return means the batch is permanently lost.
//
// The batch never held a queue slot or a byte reservation (Submit released
// both before returning ErrQueueFull), so there is nothing to release here.
func (p *Pipeline) OfferToDLQ(b *Batch) error {
	if b == nil {
		return nil
	}
	return p.enqueueDLQ(b, p.applyStoreSeverity(b.Logs))
}

// enqueueDLQ serializes a batch into the typed DLQ envelope and enqueues it.
// Every outcome is counted on otelcontext_ingest_pipeline_dlq_total so a
// silent loss is impossible to miss: result=enqueued means the batch is
// durable, anything else means it is gone.
func (p *Pipeline) enqueueDLQ(b *Batch, logs []storage.Log) error {
	if p.dlq == nil {
		p.observeDLQ(b.Type, "no_sink")
		return ErrNoDLQ
	}
	env := DLQBatchEnvelope{
		Type: DLQBatchType,
		Data: DLQBatchPayload{
			Tenant: b.Tenant,
			Signal: signalLabel(b.Type),
			Traces: b.Traces,
			Spans:  b.Spans,
			Logs:   logs,
		},
	}
	if err := p.dlq.Enqueue(env); err != nil {
		slog.Error("ingest pipeline: DLQ enqueue failed, batch lost",
			"error", err,
			"signal", signalLabel(b.Type),
			"traces", len(b.Traces),
			"spans", len(b.Spans),
			"logs", len(logs),
		)
		p.dlqFailed.Add(1)
		p.observeDLQ(b.Type, "enqueue_failed")
		return err
	}
	p.dlqEnqueued.Add(1)
	p.observeDLQ(b.Type, "enqueued")
	return nil
}

func (p *Pipeline) observeDLQ(t SignalType, result string) {
	if p.metrics == nil || p.metrics.IngestPipelineDLQTotal == nil {
		return
	}
	p.metrics.IngestPipelineDLQTotal.WithLabelValues(signalLabel(t), result).Inc()
}
