package graphrag

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/tsdb"
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"
)

const (
	defaultWorkerCount  = 4
	defaultChannelSize  = 10000
	defaultTraceTTL     = 1 * time.Hour
	defaultRefreshEvery = 60 * time.Second
	defaultSnapshotEvery = 15 * time.Minute
	defaultAnomalyEvery  = 10 * time.Second
)

// spanEvent is sent through the ingestion channel.
type spanEvent struct {
	Span     storage.Span
	TraceID  string
	Status   string
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
	ServiceStore  *ServiceStore
	TraceStore    *TraceStore
	SignalStore   *SignalStore
	AnomalyStore  *AnomalyStore

	repo       *storage.Repository
	vectorIdx  *vectordb.Index
	tsdbAgg    *tsdb.Aggregator
	ringBuf    *tsdb.RingBuffer

	eventCh    chan event
	stopCh     chan struct{}

	// Configuration
	traceTTL       time.Duration
	refreshEvery   time.Duration
	snapshotEvery  time.Duration
	anomalyEvery   time.Duration
}

// Config holds GraphRAG configuration.
type Config struct {
	TraceTTL       time.Duration
	RefreshEvery   time.Duration
	SnapshotEvery  time.Duration
	AnomalyEvery   time.Duration
	WorkerCount    int
	ChannelSize    int
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		TraceTTL:       defaultTraceTTL,
		RefreshEvery:   defaultRefreshEvery,
		SnapshotEvery:  defaultSnapshotEvery,
		AnomalyEvery:   defaultAnomalyEvery,
		WorkerCount:    defaultWorkerCount,
		ChannelSize:    defaultChannelSize,
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

	return &GraphRAG{
		ServiceStore:  newServiceStore(),
		TraceStore:    newTraceStore(cfg.TraceTTL),
		SignalStore:   newSignalStore(),
		AnomalyStore:  newAnomalyStore(),
		repo:          repo,
		vectorIdx:     vectorIdx,
		tsdbAgg:       tsdbAgg,
		ringBuf:       ringBuf,
		eventCh:       make(chan event, cfg.ChannelSize),
		stopCh:        make(chan struct{}),
		traceTTL:      cfg.TraceTTL,
		refreshEvery:  cfg.RefreshEvery,
		snapshotEvery: cfg.SnapshotEvery,
		anomalyEvery:  cfg.AnomalyEvery,
	}
}

// Start begins background goroutines: workers, refresh, snapshot, anomaly detection.
func (g *GraphRAG) Start(ctx context.Context) {
	// Start event workers
	for i := 0; i < defaultWorkerCount; i++ {
		go g.eventWorker(ctx)
	}

	// Start background tasks
	go g.refreshLoop(ctx)
	go g.snapshotLoop(ctx)
	go g.anomalyLoop(ctx)

	slog.Info("GraphRAG started",
		"workers", defaultWorkerCount,
		"trace_ttl", g.traceTTL,
		"refresh_every", g.refreshEvery,
	)
}

// Stop signals all goroutines to exit.
func (g *GraphRAG) Stop() {
	close(g.stopCh)
	slog.Info("GraphRAG stopped")
}

// OnSpanIngested is the callback wired into the trace ingestion pipeline.
func (g *GraphRAG) OnSpanIngested(span storage.Span) {
	select {
	case g.eventCh <- event{span: &spanEvent{
		Span:    span,
		TraceID: span.TraceID,
		Status:  "OK",
	}}:
	default:
		// Channel full — graph is best-effort; DB is source of truth
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

	// Compute cluster ID from log body using a simple hash
	body := string(log.Body)
	clusterID := fmt.Sprintf("lc_%s_%x", log.ServiceName, simpleHash(body))

	g.SignalStore.UpsertLogCluster(clusterID, body, log.Severity, log.ServiceName, log.Timestamp)

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
		h = h*31 + uint32(c)
	}
	return h
}
