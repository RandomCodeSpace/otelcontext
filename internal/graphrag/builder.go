package graphrag

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
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
	// Tenant is the tenant slice to route this event into. Populated by
	// OnSpanIngested from storage.Span.TenantID; empty values are coerced
	// to storage.DefaultTenantID before processing.
	Tenant string
}

// logEvent is sent through the ingestion channel.
type logEvent struct {
	Log    storage.Log
	Tenant string
}

// metricEvent is sent through the ingestion channel.
type metricEvent struct {
	Metric tsdb.RawMetric
	Tenant string
}

// event wraps one of the above event types.
type event struct {
	span   *spanEvent
	log    *logEvent
	metric *metricEvent
}

// GraphRAG is the main coordinator for the layered graph system.
//
// Every in-memory store is partitioned by tenant. The coordinator holds a map
// of tenant ID → *tenantStores and a reader/writer mutex that protects only
// the outer map; per-tenant stores keep their own RWMutexes for fine-grained
// concurrent access. All event ingestion and queries route through
// storesFor(ctx) / storesForTenant(tenant) — there is no "global" slice.
type GraphRAG struct {
	// tenants maps tenant ID → per-tenant store composite. Access via
	// storesFor / storesForTenant / snapshotTenants, not directly.
	tenants   map[string]*tenantStores
	tenantsMu sync.RWMutex

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
	workerCount   int // 0 = defaultWorkerCount (set by New from Config)

	// Event drop counters. Atomic so OnSpanIngested/OnLogIngested/
	// OnMetricIngested can record overflows without taking any lock —
	// the channel-full path must stay hot-path cheap.
	droppedSpans   atomic.Int64
	droppedLogs    atomic.Int64
	droppedMetrics atomic.Int64

	// metrics is an optional Prometheus hook for exporting event drops.
	// Assigned via SetMetrics; nil-safe at call sites.
	metrics *telemetry.Metrics

	// invCooldown suppresses repeat PersistInvestigation calls for the same
	// (trigger_service, root_service, root_operation) inside a sliding window.
	// Initialized in New; pruned from the refresh tick.
	invCooldown *investigationCooldown

	// invInserts counts cooldown-allowed PersistInvestigation calls.
	// Incremented BEFORE the DB write — see InvestigationInsertCount.
	invInserts atomic.Int64
}

// SetMetrics wires the Prometheus registry so GraphRAG event drops are
// observable via otelcontext_graphrag_events_dropped_total. Safe to call
// before Start; pass nil to disable Prometheus recording (atomic
// counters still tick).
func (g *GraphRAG) SetMetrics(m *telemetry.Metrics) { g.metrics = m }

// DroppedSpansCount reports the number of span events dropped because
// the ingestion channel was full. Exported for tests and readiness
// probes; atomic, safe from any goroutine.
func (g *GraphRAG) DroppedSpansCount() int64 { return g.droppedSpans.Load() }

// DroppedLogsCount reports the number of log events dropped because
// the ingestion channel was full.
func (g *GraphRAG) DroppedLogsCount() int64 { return g.droppedLogs.Load() }

// DroppedMetricsCount reports the number of metric events dropped
// because the ingestion channel was full.
func (g *GraphRAG) DroppedMetricsCount() int64 { return g.droppedMetrics.Load() }

// InvestigationInsertCount reports cooldown-allowed PersistInvestigation
// calls. Semantics: this counter increments when the cooldown check
// passes, BEFORE the DB write — so a subsequent DB failure still
// increments this. It is NOT a strict DB insert count. Intended for
// tests to assert cooldown behavior without requiring a live repo.
func (g *GraphRAG) InvestigationInsertCount() int64 { return g.invInserts.Load() }

// recordEventDrop increments the per-signal atomic counter and — when
// a telemetry registry is wired — the Prometheus counter vec.
func (g *GraphRAG) recordEventDrop(signal string) {
	switch signal {
	case "span":
		g.droppedSpans.Add(1)
	case "log":
		g.droppedLogs.Add(1)
	case "metric":
		g.droppedMetrics.Add(1)
	}
	if g.metrics != nil && g.metrics.GraphRAGEventsDroppedTotal != nil {
		g.metrics.GraphRAGEventsDroppedTotal.WithLabelValues(signal).Inc()
	}
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
		tenants:       make(map[string]*tenantStores),
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
		workerCount:   cfg.WorkerCount,
		invCooldown:   newInvestigationCooldown(5 * time.Minute),
	}

	// Bootstrap the default tenant slice so refresh/snapshot loops have a
	// baseline to iterate over before any ingest lands. Other tenants are
	// created lazily on first event via storesForTenant.
	g.storesForTenant(storage.DefaultTenantID)

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
	// Start event workers. Honor the configured worker count so operators
	// can scale up under sustained high ingest; fall back to the package
	// default when the constructor wasn't handed an override.
	workers := g.workerCount
	if workers <= 0 {
		workers = defaultWorkerCount
	}
	for i := 0; i < workers; i++ {
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
		"workers", workers,
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
// Tenant is taken straight from the persisted Span (already resolved upstream
// by the OTLP Export handlers) and carried on the event — the callback
// signature is intentionally unchanged so external wiring stays trivial.
func (g *GraphRAG) OnSpanIngested(span storage.Span) {
	status := span.Status
	if status == "" {
		status = "STATUS_CODE_UNSET"
	}
	tenant := span.TenantID
	if tenant == "" {
		tenant = storage.DefaultTenantID
	}
	select {
	case g.eventCh <- event{span: &spanEvent{
		Span:    span,
		TraceID: span.TraceID,
		Status:  status,
		Tenant:  tenant,
	}}:
	default:
		// Channel full — graph is best-effort; DB is source of truth.
		g.recordEventDrop("span")
	}
}

// OnLogIngested is the callback wired into the log ingestion pipeline.
func (g *GraphRAG) OnLogIngested(log storage.Log) {
	tenant := log.TenantID
	if tenant == "" {
		tenant = storage.DefaultTenantID
	}
	select {
	case g.eventCh <- event{log: &logEvent{Log: log, Tenant: tenant}}:
	default:
		// Channel full — graph is best-effort; DB is source of truth.
		g.recordEventDrop("log")
	}
}

// OnMetricIngested is the callback wired into the metric ingestion pipeline.
// tsdb.RawMetric already carries a resolved TenantID (set in ingest/otlp.go
// Export), so we read it here instead of adding a second argument — keeping
// the metric callback signature identical across TSDB and GraphRAG.
func (g *GraphRAG) OnMetricIngested(metric tsdb.RawMetric) {
	tenant := metric.TenantID
	if tenant == "" {
		tenant = storage.DefaultTenantID
	}
	select {
	case g.eventCh <- event{metric: &metricEvent{Metric: metric, Tenant: tenant}}:
	default:
		// Channel full — graph is best-effort; DB is source of truth.
		g.recordEventDrop("metric")
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

	stores := g.storesForTenant(ev.Tenant)

	// 1. Upsert ServiceNode
	stores.service.UpsertService(span.ServiceName, durationMs, isError, span.StartTime)

	// 2. Upsert OperationNode + EXPOSES edge
	if span.OperationName != "" {
		stores.service.UpsertOperation(span.ServiceName, span.OperationName, durationMs, isError, span.StartTime)
	}

	// 3. Create TraceNode + SpanNode + CONTAINS + CHILD_OF edges
	stores.traces.UpsertTrace(span.TraceID, span.ServiceName, ev.Status, durationMs, span.StartTime)
	stores.traces.UpsertSpan(SpanNode{
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
		if parentSpan, ok := stores.traces.GetSpan(span.ParentSpanID); ok {
			if parentSpan.Service != span.ServiceName {
				stores.service.UpsertCallEdge(parentSpan.Service, span.ServiceName, durationMs, isError, span.StartTime)
			}
		}
	}
}

func (g *GraphRAG) processLog(ev *logEvent) {
	log := ev.Log

	if log.ServiceName == "" {
		return
	}

	stores := g.storesForTenant(ev.Tenant)

	// Drain-based clustering (replaces hash+TF-IDF clustering). The Drain
	// miner is shared across tenants — its template tokens describe log shape,
	// not content, so same-shape logs from different tenants share a template
	// ID but land in their own tenant's SignalStore LogClusterNode entry.
	body := log.Body
	clusterID := g.clusterLog(stores, log.ServiceName, body, log.Severity, log.Timestamp)
	if clusterID == "" {
		return
	}

	// If log has trace_id + span_id, create LOGGED_DURING edge
	if log.SpanID != "" {
		stores.signals.AddLoggedDuringEdge(clusterID, log.SpanID, log.Timestamp)
	}
}

func (g *GraphRAG) processMetric(ev *metricEvent) {
	m := ev.Metric
	if m.ServiceName == "" {
		return
	}
	stores := g.storesForTenant(ev.Tenant)
	stores.signals.UpsertMetric(m.Name, m.ServiceName, m.Value, m.Timestamp)
}

// simpleHash produces a quick hash for log clustering.
func simpleHash(s string) uint32 {
	var h uint32
	for _, c := range s {
		h = h*31 + uint32(c) // #nosec G115 -- rune -> uint32 for hash is intentional
	}
	return h
}

// storesFor returns the tenantStores composite scoped to the tenant carried
// on ctx. A missing or empty tenant collapses to storage.DefaultTenantID,
// matching WithTenantContext semantics. Lazily creates the slice on first
// reference so a single-tenant install never carries empty maps for phantom
// tenants, and a new tenant does not require a restart.
func (g *GraphRAG) storesFor(ctx context.Context) *tenantStores {
	return g.storesForTenant(storage.TenantFromContext(ctx))
}

// storesForTenant is the tenant-string flavour of storesFor, used by event
// handlers that have already resolved the tenant (the callback path carries
// it on spanEvent / logEvent / metricEvent). Empty strings are coerced to
// storage.DefaultTenantID.
func (g *GraphRAG) storesForTenant(tenant string) *tenantStores {
	if tenant == "" {
		tenant = storage.DefaultTenantID
	}
	g.tenantsMu.RLock()
	slice, ok := g.tenants[tenant]
	g.tenantsMu.RUnlock()
	if ok {
		return slice
	}
	g.tenantsMu.Lock()
	defer g.tenantsMu.Unlock()
	if slice, ok = g.tenants[tenant]; ok {
		return slice
	}
	slice = newTenantStores(g.traceTTL)
	g.tenants[tenant] = slice
	return slice
}

// snapshotTenants returns a stable copy of the tenant → stores map suitable
// for iteration without holding the coordinator lock. Background loops call
// this once per tick and then operate on each slice under its own per-store
// lock, so a long-running refresh never blocks new-tenant ingestion.
func (g *GraphRAG) snapshotTenants() map[string]*tenantStores {
	g.tenantsMu.RLock()
	defer g.tenantsMu.RUnlock()
	out := make(map[string]*tenantStores, len(g.tenants))
	for k, v := range g.tenants {
		out[k] = v
	}
	return out
}
