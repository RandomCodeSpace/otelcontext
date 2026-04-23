package graphrag

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/tsdb"
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"
)

// panicMetrics is an optional hook for incrementing the panics-recovered
// metric from GraphRAG worker goroutines. Assigned via SetPanicMetrics.
var panicMetrics *telemetry.Metrics

// SetPanicMetrics wires the telemetry metrics so GraphRAG worker recovery
// closures can increment OtelContext_panics_recovered_total{subsystem="graphrag"}.
// Safe to leave unset in tests.
func SetPanicMetrics(m *telemetry.Metrics) { panicMetrics = m }

// guardWorker is a tiny helper that recovers from panics in a worker
// goroutine, logs the stack, and increments the metric.
func guardWorker(name string) {
	if r := recover(); r != nil {
		slog.Error("graphrag worker panic",
			"worker", name,
			"panic", r,
			"stack", string(debug.Stack()),
		)
		if panicMetrics != nil && panicMetrics.PanicsRecoveredTotal != nil {
			panicMetrics.PanicsRecoveredTotal.WithLabelValues("graphrag").Inc()
		}
	}
}

const (
	defaultWorkerCount   = 4
	defaultChannelSize   = 10000
	defaultTraceTTL      = 1 * time.Hour
	defaultRefreshEvery  = 60 * time.Second
	defaultSnapshotEvery = 15 * time.Minute
	defaultAnomalyEvery  = 10 * time.Second
)

// spanEvent is sent through the ingestion channel.
type spanEvent struct {
	Span    storage.Span
	TraceID string
	Status  string
}

// logEvent is sent through the ingestion channel.
type logEvent struct {
	Log storage.Log
}

// metricEvent is sent through the ingestion channel.
type metricEvent struct {
	Metric tsdb.RawMetric
}

// event wraps one of the above event types.
type event struct {
	span   *spanEvent
	log    *logEvent
	metric *metricEvent
}

// GraphRAG is the main coordinator for the layered graph system.
type GraphRAG struct {
	ServiceStore *ServiceStore
	TraceStore   *TraceStore
	SignalStore  *SignalStore
	AnomalyStore *AnomalyStore

	repo      *storage.Repository
	vectorIdx *vectordb.Index
	tsdbAgg   *tsdb.Aggregator
	ringBuf   *tsdb.RingBuffer

	drain *Drain // Drain log-template miner (see drain.go)

	eventCh chan event
	stopCh  chan struct{}

	// Configuration
	traceTTL      time.Duration
	refreshEvery  time.Duration
	snapshotEvery time.Duration
	anomalyEvery  time.Duration
}

// Config holds GraphRAG configuration.
type Config struct {
	TraceTTL      time.Duration
	RefreshEvery  time.Duration
	SnapshotEvery time.Duration
	AnomalyEvery  time.Duration
	WorkerCount   int
	ChannelSize   int
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		TraceTTL:      defaultTraceTTL,
		RefreshEvery:  defaultRefreshEvery,
		SnapshotEvery: defaultSnapshotEvery,
		AnomalyEvery:  defaultAnomalyEvery,
		WorkerCount:   defaultWorkerCount,
		ChannelSize:   defaultChannelSize,
	}
}

// New creates a new GraphRAG coordinator.
func New(repo *storage.Repository, vectorIdx *vectordb.Index, tsdbAgg *tsdb.Aggregator, ringBuf *tsdb.RingBuffer, cfg Config) *GraphRAG {
	if cfg.TraceTTL == 0 {
		cfg.TraceTTL = defaultTraceTTL
	}
	if cfg.RefreshEvery == 0 {
		cfg.RefreshEvery = defaultRefreshEvery
	}
	if cfg.SnapshotEvery == 0 {
		cfg.SnapshotEvery = defaultSnapshotEvery
	}
	if cfg.AnomalyEvery == 0 {
		cfg.AnomalyEvery = defaultAnomalyEvery
	}
	if cfg.WorkerCount == 0 {
		cfg.WorkerCount = defaultWorkerCount
	}
	if cfg.ChannelSize == 0 {
		cfg.ChannelSize = defaultChannelSize
	}

	g := &GraphRAG{
		ServiceStore:  newServiceStore(),
		TraceStore:    newTraceStore(cfg.TraceTTL),
		SignalStore:   newSignalStore(),
		AnomalyStore:  newAnomalyStore(),
		repo:          repo,
		vectorIdx:     vectorIdx,
		tsdbAgg:       tsdbAgg,
		ringBuf:       ringBuf,
		drain:         NewDrain(),
		eventCh:       make(chan event, cfg.ChannelSize),
		stopCh:        make(chan struct{}),
		traceTTL:      cfg.TraceTTL,
		refreshEvery:  cfg.RefreshEvery,
		snapshotEvery: cfg.SnapshotEvery,
		anomalyEvery:  cfg.AnomalyEvery,
	}

	// Restore persisted Drain templates so log clustering survives restarts.
	// A missing table (fresh install) or transient DB error is non-fatal —
	// ingestion will rebuild templates from scratch.
	if repo != nil && repo.DB() != nil {
		if tpls, err := LoadDrainTemplates(repo.DB()); err != nil {
			slog.Info("GraphRAG: drain template restore skipped", "reason", err)
		} else if len(tpls) > 0 {
			g.drain.LoadTemplates(tpls)
			slog.Info("GraphRAG: restored drain templates", "count", len(tpls))
		}
	}

	return g
}

// Start begins background goroutines: workers, refresh, snapshot, anomaly detection.
// Each goroutine is wrapped in a panic recovery so one misbehaving event
// can't take down the whole subsystem.
func (g *GraphRAG) Start(ctx context.Context) {
	// Start event workers
	for i := 0; i < defaultWorkerCount; i++ {
		go func() {
			defer guardWorker("eventWorker")
			g.eventWorker(ctx)
		}()
	}

	// Start background tasks
	go func() {
		defer guardWorker("refreshLoop")
		g.refreshLoop(ctx)
	}()
	go func() {
		defer guardWorker("snapshotLoop")
		g.snapshotLoop(ctx)
	}()
	go func() {
		defer guardWorker("anomalyLoop")
		g.anomalyLoop(ctx)
	}()

	slog.Info("GraphRAG started",
		"workers", defaultWorkerCount,
		"trace_ttl", g.traceTTL,
		"refresh_every", g.refreshEvery,
	)
}

// Stop signals all goroutines to exit.
func (g *GraphRAG) Stop() {
	// Best-effort final Drain template persistence — losing the most recent
	// updates on an unclean shutdown would force rebuilding from scratch.
	if g.repo != nil && g.repo.DB() != nil && g.drain != nil {
		if err := SaveDrainTemplates(g.repo.DB(), g.drain.Templates()); err != nil {
			slog.Warn("GraphRAG: final drain template save failed", "error", err)
		}
	}
	close(g.stopCh)
	slog.Info("GraphRAG stopped")
}

// EventBufferDepth returns the current number of events queued in the
// ingestion channel. Exported for telemetry polling; never blocks.
func (g *GraphRAG) EventBufferDepth() int {
	if g == nil || g.eventCh == nil {
		return 0
	}
	return len(g.eventCh)
}

// IsRunning reports whether the coordinator's stop channel has not been closed.
// Used by readiness probes to confirm the background workers are still live.
func (g *GraphRAG) IsRunning() bool {
	if g == nil {
		return false
	}
	select {
	case <-g.stopCh:
		return false
	default:
		return true
	}
}

// OnSpanIngested is the callback wired into the trace ingestion pipeline.
func (g *GraphRAG) OnSpanIngested(span storage.Span) {
	status := span.Status
	if status == "" {
		status = "STATUS_CODE_UNSET"
	}
	select {
	case g.eventCh <- event{span: &spanEvent{
		Span:    span,
		TraceID: span.TraceID,
		Status:  status,
	}}:
	default:
		// Channel full — graph is best-effort; DB is source of truth.
		// Task 2 will add a drop counter here.
	}
}

// OnLogIngested is the callback wired into the log ingestion pipeline.
func (g *GraphRAG) OnLogIngested(log storage.Log) {
	select {
	case g.eventCh <- event{log: &logEvent{Log: log}}:
	default:
	}
}

// OnMetricIngested is the callback wired into the metric ingestion pipeline.
func (g *GraphRAG) OnMetricIngested(metric tsdb.RawMetric) {
	select {
	case g.eventCh <- event{metric: &metricEvent{Metric: metric}}:
	default:
	}
}

// eventWorker processes events from the channel.
func (g *GraphRAG) eventWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stopCh:
			return
		case ev := <-g.eventCh:
			if ev.span != nil {
				g.processSpan(ev.span)
			}
			if ev.log != nil {
				g.processLog(ev.log)
			}
			if ev.metric != nil {
				g.processMetric(ev.metric)
			}
		}
	}
}

func (g *GraphRAG) processSpan(ev *spanEvent) {
	span := ev.Span
	durationMs := float64(span.Duration) / 1000.0 // microseconds → ms
	isError := span.OperationName != "" && ev.Status == "STATUS_CODE_ERROR"

	// Check for error status from the span data
	// The status comes from the trace data, propagated by the caller
	if span.ServiceName == "" {
		return
	}

	// 1. Upsert ServiceNode
	g.ServiceStore.UpsertService(span.ServiceName, durationMs, isError, span.StartTime)

	// 2. Upsert OperationNode + EXPOSES edge
	if span.OperationName != "" {
		g.ServiceStore.UpsertOperation(span.ServiceName, span.OperationName, durationMs, isError, span.StartTime)
	}

	// 3. Create TraceNode + SpanNode + CONTAINS + CHILD_OF edges
	g.TraceStore.UpsertTrace(span.TraceID, span.ServiceName, ev.Status, durationMs, span.StartTime)
	g.TraceStore.UpsertSpan(SpanNode{
		ID:           span.SpanID,
		TraceID:      span.TraceID,
		ParentSpanID: span.ParentSpanID,
		Service:      span.ServiceName,
		Operation:    span.OperationName,
		Duration:     durationMs,
		StatusCode:   ev.Status,
		IsError:      isError,
		Timestamp:    span.StartTime,
	})

	// 4. If parent span exists and belongs to different service, create CALLS edge
	if span.ParentSpanID != "" {
		if parentSpan, ok := g.TraceStore.GetSpan(span.ParentSpanID); ok {
			if parentSpan.Service != span.ServiceName {
				g.ServiceStore.UpsertCallEdge(parentSpan.Service, span.ServiceName, durationMs, isError, span.StartTime)
			}
		}
	}
}

func (g *GraphRAG) processLog(ev *logEvent) {
	log := ev.Log

	if log.ServiceName == "" {
		return
	}

	// Drain-based clustering (replaces hash+TF-IDF clustering).
	body := log.Body
	clusterID := g.clusterLog(log.ServiceName, body, log.Severity, log.Timestamp)
	if clusterID == "" {
		return
	}

	// If log has trace_id + span_id, create LOGGED_DURING edge
	if log.SpanID != "" {
		g.SignalStore.AddLoggedDuringEdge(clusterID, log.SpanID, log.Timestamp)
	}
}

func (g *GraphRAG) processMetric(ev *metricEvent) {
	m := ev.Metric
	if m.ServiceName == "" {
		return
	}
	g.SignalStore.UpsertMetric(m.Name, m.ServiceName, m.Value, m.Timestamp)
}

// simpleHash produces a quick hash for log clustering.
func simpleHash(s string) uint32 {
	var h uint32
	for _, c := range s {
		h = h*31 + uint32(c) // #nosec G115 -- rune -> uint32 for hash is intentional
	}
	return h
}
