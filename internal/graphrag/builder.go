package graphrag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/tsdb"
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
func (g *GraphRAG) guardWorker(name string) {
	if r := recover(); r != nil {
		err := fmt.Errorf("graphrag worker %s panic: %v", name, r)
		g.recordShutdownError(err)
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
	defaultWorkerCount = 16
	defaultChannelSize = 100000
	// maxChannelSize bounds the event channel buffer at the allocation
	// site: each queued event embeds a Span/Log by value (~0.5-2 KB), so
	// 1M buffered events is already ~1-2 GB. Mirrors the config-boundary
	// range check on GRAPHRAG_EVENT_QUEUE_SIZE (config.Validate).
	maxChannelSize       = 1_000_000
	defaultTraceTTL      = 1 * time.Hour
	defaultRefreshEvery  = 60 * time.Second
	defaultSnapshotEvery = 15 * time.Minute
	defaultAnomalyEvery  = 10 * time.Second
	// defaultMaxSpansPerTenant caps the in-memory TraceStore per tenant
	// (~850 B/span ⇒ ~425 MB worst case). Overridable via
	// GRAPHRAG_MAX_SPANS_PER_TENANT.
	defaultMaxSpansPerTenant = 500000
	// defaultTenantIdleTTL evicts a tenant slice after this much time with
	// no ingest event or query. Overridable via GRAPHRAG_TENANT_IDLE_TTL.
	defaultTenantIdleTTL = 24 * time.Hour
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
	// tmpl carries an ingest-mined log template fact (shadow and aggregate
	// modes). It rides the same channel as everything else so the miner's
	// synchronous callback never does a store write on the OTLP goroutine.
	tmpl *aggregate.TemplateFact
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

	repo    *storage.Repository
	tsdbAgg *tsdb.Aggregator
	ringBuf *tsdb.RingBuffer

	drain *Drain // Drain log-template miner (see drain.go)

	eventCh chan event
	stopCh  chan struct{}

	startOnce   sync.Once
	stopOnce    sync.Once
	saveOnce    sync.Once
	lifecycleMu sync.Mutex
	started     bool
	workerWG    sync.WaitGroup
	workersDone chan struct{}
	admissionMu sync.RWMutex
	closed      bool
	shutdownMu  sync.Mutex
	shutdownErr error

	// Configuration
	traceTTL          time.Duration
	refreshEvery      time.Duration
	snapshotEvery     time.Duration
	anomalyEvery      time.Duration
	workerCount       int           // 0 = defaultWorkerCount (set by New from Config)
	maxSpansPerTenant int           // per-tenant TraceStore span cap; <=0 = unbounded
	tenantIdleTTL     time.Duration // idle window before tenant eviction; <=0 disables

	// Event drop counters. Atomic so OnSpanIngested/OnLogIngested/
	// OnMetricIngested can record overflows without taking any lock —
	// the channel-full path must stay hot-path cheap.
	droppedSpans   atomic.Int64
	droppedLogs    atomic.Int64
	droppedMetrics atomic.Int64
	// droppedSpanCap counts spans skipped because the tenant's TraceStore
	// hit MaxSpans (signal "span_capacity" on the Prometheus counter).
	droppedSpanCap atomic.Int64
	// tenantsEvicted counts tenant slices removed by evictIdleTenants.
	tenantsEvicted atomic.Int64

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

	// lastAnomalyScan is the unix-nano start time of the previous
	// detectAnomalies pass. Tenants whose lastEventAt predates it are
	// skipped — no events means their stats cannot have changed.
	lastAnomalyScan atomic.Int64

	// mode is the aggregate mode this coordinator runs under: one of
	// aggregate.ModeLegacy, ModeShadow or ModeAggregate. Legacy behaviour is
	// the default and is unchanged in every respect; everything the aggregate
	// phase adds is gated on this field (see aggregate.go).
	mode string

	// aggSource is the aggregate engine's topology projection, consulted only
	// in aggregate mode.
	aggSource AggregateSource
	// topologyMu serializes the periodic backstop with read-through freshness.
	// Without it, an older render could replace a newer revision under two
	// concurrent topology-dependent reads.
	topologyMu sync.Mutex
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

// SpanCapacityDropsCount reports the number of spans skipped because the
// tenant's TraceStore was at its MaxSpans cap. Atomic, safe from any
// goroutine; exported for tests and readiness probes.
func (g *GraphRAG) SpanCapacityDropsCount() int64 { return g.droppedSpanCap.Load() }

// TenantsEvictedCount reports the number of tenant store slices evicted
// for exceeding the idle TTL since startup.
func (g *GraphRAG) TenantsEvictedCount() int64 { return g.tenantsEvicted.Load() }

// InvestigationInsertCount reports cooldown-allowed PersistInvestigation
// calls. Semantics: this counter increments when the cooldown check
// passes, BEFORE the DB write — so a subsequent DB failure still
// increments this. It is NOT a strict DB insert count. Intended for
// tests to assert cooldown behavior without requiring a live repo.
func (g *GraphRAG) InvestigationInsertCount() int64 { return g.invInserts.Load() }

// RegisterAnomaly inserts an anomaly into the AnomalyStore for tenant.
// Mirrors PersistInvestigation's "tenant accepted explicitly" shape so
// out-of-band anomaly producers (synthetic detectors, integration tests,
// future external anomaly feeds) can land directly on the right tenant
// slice without going through the metric/error detection loops. Empty
// tenant collapses to storage.DefaultTenantID.
func (g *GraphRAG) RegisterAnomaly(tenant string, anomaly AnomalyNode) {
	if tenant == "" {
		tenant = storage.DefaultTenantID
	}
	g.storesForTenant(tenant).anomalies.AddAnomaly(anomaly)
}

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
	case "span_capacity":
		g.droppedSpanCap.Add(1)
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
	// MaxSpansPerTenant caps each tenant's in-memory TraceStore span map.
	// 0 = defaultMaxSpansPerTenant; negative disables the cap.
	MaxSpansPerTenant int
	// TenantIdleTTL evicts a tenant's store slice after this much time
	// without any ingest event or query. 0 = defaultTenantIdleTTL;
	// negative disables eviction.
	TenantIdleTTL time.Duration

	// Mode is the aggregate mode (AGGREGATE_MODE): aggregate.ModeLegacy,
	// ModeShadow or ModeAggregate. Empty means legacy. In shadow mode the only
	// change is that log templates come from the ingest-owned miner; in
	// aggregate mode the raw-span rebuild and the per-span topology upserts
	// are retired in favour of engine snapshots (#163, #174).
	Mode string
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		TraceTTL:          defaultTraceTTL,
		RefreshEvery:      defaultRefreshEvery,
		SnapshotEvery:     defaultSnapshotEvery,
		AnomalyEvery:      defaultAnomalyEvery,
		WorkerCount:       defaultWorkerCount,
		ChannelSize:       defaultChannelSize,
		MaxSpansPerTenant: defaultMaxSpansPerTenant,
		TenantIdleTTL:     defaultTenantIdleTTL,
		Mode:              aggregate.ModeLegacy,
	}
}

// New creates a new GraphRAG coordinator.
//
// The vectordb-backed semantic similarity path was removed on 2026-05-24
// along with the find_similar_logs MCP tool — log clustering now relies
// solely on the Drain template miner (see drain.go).
func New(repo *storage.Repository, tsdbAgg *tsdb.Aggregator, ringBuf *tsdb.RingBuffer, cfg Config) *GraphRAG {
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
	if cfg.ChannelSize <= 0 || cfg.ChannelSize > maxChannelSize {
		cfg.ChannelSize = defaultChannelSize
	}
	if cfg.MaxSpansPerTenant == 0 {
		cfg.MaxSpansPerTenant = defaultMaxSpansPerTenant
	}
	if cfg.TenantIdleTTL == 0 {
		cfg.TenantIdleTTL = defaultTenantIdleTTL
	}
	if cfg.Mode == "" {
		cfg.Mode = aggregate.ModeLegacy
	}

	g := &GraphRAG{
		tenants:           make(map[string]*tenantStores),
		repo:              repo,
		tsdbAgg:           tsdbAgg,
		ringBuf:           ringBuf,
		drain:             NewDrain(),
		eventCh:           make(chan event, cfg.ChannelSize),
		stopCh:            make(chan struct{}),
		workersDone:       make(chan struct{}),
		traceTTL:          cfg.TraceTTL,
		refreshEvery:      cfg.RefreshEvery,
		snapshotEvery:     cfg.SnapshotEvery,
		anomalyEvery:      cfg.AnomalyEvery,
		workerCount:       cfg.WorkerCount,
		maxSpansPerTenant: cfg.MaxSpansPerTenant,
		tenantIdleTTL:     cfg.TenantIdleTTL,
		mode:              cfg.Mode,
		invCooldown:       newInvestigationCooldown(5 * time.Minute),
	}
	if g.externalTemplates() {
		// Shadow and aggregate modes mine templates on the ingest path. A
		// second miner here would mint a second ID space for the same log
		// shapes (#163), so there is no Drain instance to construct, restore
		// or persist.
		g.drain = nil
	}

	// Bootstrap the default tenant slice so refresh/snapshot loops have a
	// baseline to iterate over before any ingest lands. Other tenants are
	// created lazily on first event via storesForTenant.
	g.storesForTenant(storage.DefaultTenantID)

	// Restore persisted Drain templates so log clustering survives restarts.
	// A missing table (fresh install) or transient DB error is non-fatal —
	// ingestion will rebuild templates from scratch.
	//
	// The Drain miner is currently a single shared instance, so we treat its
	// learned templates as belonging to DefaultTenantID. The persistence layer
	// is already keyed by (tenant_id, id) so a future per-tenant Drain miner
	// can load each tenant's slice without colliding cluster IDs.
	if repo != nil && repo.DB() != nil && g.drain != nil {
		if tpls, err := LoadDrainTemplates(repo.DB(), storage.DefaultTenantID); err != nil {
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
	g.startOnce.Do(func() {
		g.lifecycleMu.Lock()
		g.started = true
		g.lifecycleMu.Unlock()

		// Start event workers. Honor the configured worker count so operators
		// can scale up under sustained high ingest; fall back to the package
		// default when the constructor wasn't handed an override.
		workers := g.workerCount
		if workers <= 0 {
			workers = defaultWorkerCount
		}
		for i := 0; i < workers; i++ {
			g.workerWG.Add(1)
			go func() {
				defer g.workerWG.Done()
				defer g.guardWorker("eventWorker")
				g.eventWorker(ctx)
			}()
		}

		for name, loop := range map[string]func(context.Context){
			"refreshLoop":  g.refreshLoop,
			"snapshotLoop": g.snapshotLoop,
			"anomalyLoop":  g.anomalyLoop,
		} {
			g.workerWG.Add(1)
			go func() {
				defer g.workerWG.Done()
				defer g.guardWorker(name)
				loop(ctx)
			}()
		}
		go func() {
			g.workerWG.Wait()
			close(g.workersDone)
		}()

		slog.Info("GraphRAG started",
			"workers", workers,
			"trace_ttl", g.traceTTL,
			"refresh_every", g.refreshEvery,
		)
	})
}

// Shutdown closes event admission, drains accepted events, joins every worker,
// and only then persists the final Drain template state.
func (g *GraphRAG) Shutdown(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.admissionMu.Lock()
	g.closed = true
	g.stopOnce.Do(func() { close(g.stopCh) })
	g.admissionMu.Unlock()

	g.lifecycleMu.Lock()
	started := g.started
	g.lifecycleMu.Unlock()
	if !started {
		return nil
	}

	select {
	case <-g.workersDone:
	case <-ctx.Done():
		return ctx.Err()
	}

	g.saveOnce.Do(func() {
		if g.repo != nil && g.repo.DB() != nil && g.drain != nil {
			if err := SaveDrainTemplates(g.repo.DB(), storage.DefaultTenantID, g.drain.Templates()); err != nil {
				g.recordShutdownError(fmt.Errorf("persist final Drain templates: %w", err))
			}
		}
	})
	g.shutdownMu.Lock()
	err := g.shutdownErr
	g.shutdownMu.Unlock()
	if err == nil {
		slog.Info("GraphRAG stopped")
	}
	return err
}

// Stop preserves the prior internal lifecycle surface. Production shutdown
// uses Shutdown so deadline and persistence failures reach the process result.
func (g *GraphRAG) Stop() {
	if err := g.Shutdown(context.Background()); err != nil {
		slog.Error("GraphRAG shutdown failed", "error", err)
	}
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
	g.enqueueEvent(event{span: &spanEvent{
		Span:    span,
		TraceID: span.TraceID,
		Status:  status,
		Tenant:  tenant,
	}}, "span")
}

// OnLogIngested is the callback wired into the log ingestion pipeline.
func (g *GraphRAG) OnLogIngested(log storage.Log) {
	tenant := log.TenantID
	if tenant == "" {
		tenant = storage.DefaultTenantID
	}
	g.enqueueEvent(event{log: &logEvent{Log: log, Tenant: tenant}}, "log")
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
	g.enqueueEvent(event{metric: &metricEvent{Metric: metric, Tenant: tenant}}, "metric")
}

// eventWorker processes events from the channel.
func (g *GraphRAG) eventWorker(ctx context.Context) {
	for {
		select {
		case <-g.stopCh:
			for {
				select {
				case ev := <-g.eventCh:
					g.processEvent(ev)
				default:
					return
				}
			}
		case <-ctx.Done():
			g.recordShutdownError(fmt.Errorf("event worker stopped before shutdown barrier: %w", ctx.Err()))
			return
		case ev := <-g.eventCh:
			g.processEvent(ev)
		}
	}
}

func (g *GraphRAG) enqueueEvent(ev event, signal string) {
	if g == nil {
		return
	}
	g.admissionMu.RLock()
	defer g.admissionMu.RUnlock()
	if g.closed {
		return
	}
	select {
	case g.eventCh <- ev:
	default:
		// Channel full — graph is best-effort; DB is source of truth.
		g.recordEventDrop(signal)
	}
}

func (g *GraphRAG) processEvent(ev event) {
	if ev.span != nil {
		g.processSpan(ev.span)
	}
	if ev.log != nil {
		g.processLog(ev.log)
	}
	if ev.metric != nil {
		g.processMetric(ev.metric)
	}
	if ev.tmpl != nil {
		g.processTemplateFact(ev.tmpl)
	}
}

func (g *GraphRAG) recordShutdownError(err error) {
	if err == nil {
		return
	}
	g.shutdownMu.Lock()
	g.shutdownErr = errors.Join(g.shutdownErr, err)
	g.shutdownMu.Unlock()
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
	stores.lastEventAt.Store(time.Now().UnixNano())

	// 1./2./4. Service, operation and CALLS-edge aggregates.
	//
	// In aggregate mode these are owned by the engine's topology snapshot and
	// replaced per revision (see aggregate.go). Folding a retained exemplar's
	// span in here as well would double-count it against a snapshot that
	// already accounted for every accepted span, sampled or not. Only the
	// exemplar detail below is GraphRAG's to record.
	if !g.AggregateMode() {
		stores.service.UpsertService(span.ServiceName, durationMs, isError, span.StartTime)
		if span.OperationName != "" {
			stores.service.UpsertOperation(span.ServiceName, span.OperationName, durationMs, isError, span.StartTime)
		}
	}

	// 3. Create TraceNode + SpanNode + CONTAINS + CHILD_OF edges
	stores.traces.UpsertTrace(span.TraceID, span.ServiceName, ev.Status, durationMs, span.StartTime)
	if !stores.traces.UpsertSpan(SpanNode{
		ID:           span.SpanID,
		TraceID:      span.TraceID,
		ParentSpanID: span.ParentSpanID,
		Service:      span.ServiceName,
		Operation:    span.OperationName,
		Duration:     durationMs,
		StatusCode:   ev.Status,
		IsError:      isError,
		Timestamp:    span.StartTime,
	}) {
		// Tenant span cap reached — graph is best-effort; DB is source of
		// truth. Service/operation/trace stats above were still updated.
		g.recordEventDrop("span_capacity")
	}

	// 4. If parent span exists and belongs to different service, create CALLS edge
	if !g.AggregateMode() && span.ParentSpanID != "" {
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
	stores.lastEventAt.Store(time.Now().UnixNano())

	// Shadow and aggregate modes: templates are mined once, on the ingest
	// path, and arrive through OnTemplateFact. Mining here as well would mint
	// a second ID space for the same log shapes (#163). A template fact
	// carries no span ID, so the LOGGED_DURING correlation edge is not formed
	// in those modes; nothing in the 7-tool surface reads it.
	if g.externalTemplates() {
		return
	}

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
	stores.lastEventAt.Store(time.Now().UnixNano())
	// Aggregate mode: metric nodes are replaced from the engine snapshot per
	// revision, so a per-point upsert here would fight the projection.
	if g.AggregateMode() {
		return
	}
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
// storage.DefaultTenantID. Every call refreshes the tenant's idle-eviction
// clock — ingest and queries both count as activity.
func (g *GraphRAG) storesForTenant(tenant string) *tenantStores {
	slice := g.tenantStoresNoTouch(tenant)
	slice.lastAccess.Store(time.Now().UnixNano())
	return slice
}

// tenantStoresNoTouch returns (lazily creating) the tenant slice WITHOUT
// refreshing its idle-eviction clock. Background maintenance — the 60s DB
// rebuild in particular — goes through here so bookkeeping alone cannot keep
// a dormant tenant alive past tenantIdleTTL; only real ingest or queries do.
func (g *GraphRAG) tenantStoresNoTouch(tenant string) *tenantStores {
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
	slice = newTenantStores(g.traceTTL, g.maxSpansPerTenant)
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
