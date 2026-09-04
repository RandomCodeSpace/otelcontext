package api

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/cache"
	"github.com/RandomCodeSpace/otelcontext/internal/graph"
	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/realtime"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

// Server handles HTTP API requests.
type Server struct {
	repo     *storage.Repository
	hub      *realtime.Hub
	eventHub *realtime.EventHub
	metrics  *telemetry.Metrics
	cache    *cache.TTLCache
	graph    *graph.Graph       // in-memory service dependency graph (may be nil before first build)
	graphRAG *graphrag.GraphRAG // layered GraphRAG for advanced queries
	// topology is selected once from AGGREGATE_MODE before listeners start.
	// It owns every service-map and system-graph read.
	topology topology.Provider

	// Saturation probes consulted by /ready. Each returns a fullness
	// fraction in [0.0, 1.0]; nil disables the corresponding check.
	// Decoupling via callbacks keeps the api package free of queue/ingest
	// imports and lets tests inject deterministic values.
	dlqSaturation      func() float64
	pipelineSaturation func() float64

	// aggregateEngine is the aggregate query facade. It is non-nil only in
	// AGGREGATE_MODE=aggregate: shadow mode writes aggregates but must not
	// serve them, and legacy mode has no engine at all. Every read migrated
	// to the engine branches on aggregateReads(), so legacy responses stay
	// byte-for-byte what they were.
	aggregateEngine *aggregate.Engine

	// aggregateRecovered reports whether the durable aggregate store has
	// finished startup recovery. /ready stays 503 until it returns true, so
	// no orchestrator routes traffic to a process whose shards are only
	// half-replayed (#173). nil means "no store configured" and is skipped.
	aggregateRecovered func() bool

	// diskPressure reports the disk watchdog's state (#201 Q5): a label for
	// the readiness breakdown and whether the process should still be
	// considered ready. At raw-off it is not — the platform still serves
	// reads and still accounts aggregates, but it can no longer retain the
	// diagnostics anyone comes here for, and an orchestrator should stop
	// routing fresh ingest at it. nil means "no watchdog" and is skipped.
	diskPressure func() (string, bool)

	// aggregateRuntime reports the aggregate engine's RUNTIME health — the
	// signals that say a process which finished recovery has since stopped
	// being able to serve (#194 finding 18). nil means "no aggregate store"
	// and every runtime probe is skipped.
	aggregateRuntime func() AggregateRuntime

	// aggregateDBPing is a cheap reachability check on the aggregate store's
	// READ pool. It takes a context so the probe carries its own deadline and
	// cannot park a readiness request behind a slow database.
	aggregateDBPing func(context.Context) error

	// readyThresholds are the runtime-probe limits. nil takes
	// DefaultReadinessThresholds; an explicitly-set zero struct disables
	// every runtime probe, which is what a 0 in each env knob means.
	readyThresholds *ReadinessThresholds

	// shuttingDown flips before listener admission stops. Readiness must fail
	// immediately so a load balancer cannot route fresh work into a process
	// that has started its quiescence barrier.
	shuttingDown atomic.Bool
}

// AggregateRuntime is the aggregate runtime health snapshot /ready consults.
// It is a plain value, sampled from counters the writer already maintains, so
// a readiness request never queries the store and never stacks behind the
// single SQLite writer.
type AggregateRuntime struct {
	// CommitFailureStreak and FinalizeFailureStreak are CONSECUTIVE failure
	// counts: any success resets them.
	CommitFailureStreak   uint64
	FinalizeFailureStreak uint64
	// AdmissionRatio is the group-commit writer's admission occupancy as a
	// fraction of its bounds — the fullest of pending bytes, pending deltas
	// and parked waiters.
	AdmissionRatio float64
	// DeltaLogAgeSeconds is the age of the oldest un-finalized window,
	// including the staleness of the sample it came from.
	DeltaLogAgeSeconds float64
	// DiskUsedBytes and DiskBudgetBytes are the aggregate tier's on-disk size
	// against its share of the data budget. A zero budget disables the check.
	DiskUsedBytes   int64
	DiskBudgetBytes int64
}

// DiskRatio is the aggregate tier's usage as a fraction of its budget.
func (a AggregateRuntime) DiskRatio() float64 {
	if a.DiskBudgetBytes <= 0 {
		return 0
	}
	return float64(a.DiskUsedBytes) / float64(a.DiskBudgetBytes)
}

// ReadinessThresholds are the limits the aggregate runtime probes compare
// against. A non-positive limit disables that probe: an operator who
// disagrees with a default switches it off rather than patching the binary.
type ReadinessThresholds struct {
	MaxCommitFailureStreak   uint64
	MaxFinalizeFailureStreak uint64
	MaxAdmissionRatio        float64
	MaxDeltaLogAgeSeconds    float64
	MaxAggregateDiskRatio    float64
}

// DefaultReadinessThresholds mirrors the config defaults so a Server built
// without explicit thresholds still probes rather than silently passing.
func DefaultReadinessThresholds() ReadinessThresholds {
	return ReadinessThresholds{
		MaxCommitFailureStreak:   3,
		MaxFinalizeFailureStreak: 3,
		MaxAdmissionRatio:        0.9,
		MaxDeltaLogAgeSeconds:    1800,
		MaxAggregateDiskRatio:    1.0,
	}
}

// thresholds resolves the configured limits, falling back to the defaults.
func (s *Server) thresholds() ReadinessThresholds {
	if s.readyThresholds == nil {
		return DefaultReadinessThresholds()
	}
	return *s.readyThresholds
}

// NewServer creates a new API server.
func NewServer(repo *storage.Repository, hub *realtime.Hub, eventHub *realtime.EventHub, metrics *telemetry.Metrics) *Server {
	s := &Server{
		repo:     repo,
		hub:      hub,
		eventHub: eventHub,
		metrics:  metrics,
		cache:    cache.New(),
	}
	metrics.RegisterReadCache("api_ttl", s.cache.Len)
	return s
}

// SetGraph wires the in-memory service graph into the API server.
func (s *Server) SetGraph(g *graph.Graph) {
	s.graph = g
}

// SetGraphRAG wires the GraphRAG instance for advanced queries.
func (s *Server) SetGraphRAG(g *graphrag.GraphRAG) {
	s.graphRAG = g
}

// SetTopologyProvider installs the construction-time mode owner used by both
// REST topology endpoints.
func (s *Server) SetTopologyProvider(provider topology.Provider) {
	s.topology = provider
}

func (s *Server) aggregateTopology() bool {
	return s.topology != nil && s.topology.Source() == topology.SourceAggregate
}

// SetDLQSaturationProbe registers a callback returning DLQ disk fullness as
// a fraction in [0.0, 1.0]. Used by /ready to flip to 503 when DLQ is at
// risk of FIFO-evicting unflushed batches. Pass nil to disable the check.
func (s *Server) SetDLQSaturationProbe(fn func() float64) {
	s.dlqSaturation = fn
}

// SetPipelineSaturationProbe registers a callback returning ingest pipeline
// queue fullness as a fraction in [0.0, 1.0]. Used by /ready to flip to 503
// when the pipeline is at hard capacity (already returning 429/RESOURCE_EXHAUSTED
// to clients). Pass nil to disable the check.
func (s *Server) SetPipelineSaturationProbe(fn func() float64) {
	s.pipelineSaturation = fn
}

// SetAggregateEngine wires the aggregate query facade into the read path. Pass
// the engine only in AGGREGATE_MODE=aggregate; a nil engine, or an engine in
// any other mode, leaves every handler on the legacy path.
func (s *Server) SetAggregateEngine(e *aggregate.Engine) {
	if e == nil || e.Mode() != aggregate.ModeAggregate {
		return
	}
	s.aggregateEngine = e
}

// aggregateReads reports whether this request should be served from the
// aggregate engine.
func (s *Server) aggregateReads() bool { return s.aggregateEngine != nil }

// SetAggregateRecoveryProbe registers a callback reporting whether the durable
// aggregate store has finished replaying its delta log. /ready returns 503
// until it does. Pass nil (the default) when no aggregate store is configured.
func (s *Server) SetAggregateRecoveryProbe(fn func() bool) {
	s.aggregateRecovered = fn
}

// SetDiskPressureProbe registers a callback returning the disk watchdog's
// state label and whether readiness should pass. Pass nil (the default) when
// no watchdog is configured.
func (s *Server) SetDiskPressureProbe(fn func() (string, bool)) {
	s.diskPressure = fn
}

// SetAggregateRuntimeProbe registers the aggregate runtime health sampler.
// Pass nil (the default) when no aggregate store is configured; every runtime
// check then reports "skipped" and readiness is unaffected.
func (s *Server) SetAggregateRuntimeProbe(fn func() AggregateRuntime) {
	s.aggregateRuntime = fn
}

// SetAggregateDBProbe registers the aggregate store reachability check. The
// callback must honour the context deadline the probe passes it.
func (s *Server) SetAggregateDBProbe(fn func(context.Context) error) {
	s.aggregateDBPing = fn
}

// SetReadinessThresholds overrides the runtime-probe limits. A zero value in
// any field disables that probe.
func (s *Server) SetReadinessThresholds(t ReadinessThresholds) {
	s.readyThresholds = &t
}

// BeginShutdown removes this instance from readiness before admission stops.
// It is idempotent and leaves /live unchanged while the process drains.
func (s *Server) BeginShutdown() {
	s.shuttingDown.Store(true)
}

// RegisterRoutes registers API endpoints on the provided mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// Metadata & Discovery
	mux.HandleFunc("GET /api/metadata/services", s.handleGetServices)
	mux.HandleFunc("GET /api/metadata/metrics", s.handleGetMetricNames)

	// Metrics & Dashboard
	mux.HandleFunc("GET /api/metrics", s.handleGetMetricBuckets)
	mux.HandleFunc("GET /api/metrics/traffic", s.handleGetTrafficMetrics)
	mux.HandleFunc("GET /api/metrics/latency_heatmap", s.handleGetLatencyHeatmap)
	mux.HandleFunc("GET /api/metrics/dashboard", s.handleGetDashboardStats)
	mux.HandleFunc("GET /api/metrics/service-map", s.handleGetServiceMapMetrics)

	// System Graph (AI-consumable topology + health)
	mux.HandleFunc("GET /api/system/graph", s.handleGetSystemGraph)

	// Hosts (#288): the resource registry projected through the topology owner
	mux.HandleFunc("GET /api/hosts", s.handleGetHosts)
	mux.HandleFunc("GET /api/hosts/{host}", s.handleGetHost)

	// Traces
	mux.HandleFunc("GET /api/traces", s.handleGetTraces)
	mux.HandleFunc("GET /api/traces/{id}", s.handleGetTraceByID)

	// Logs
	mux.HandleFunc("GET /api/logs", s.handleGetLogs)
	mux.HandleFunc("GET /api/logs/context", s.handleGetLogContext)
	mux.HandleFunc("GET /api/logs/{id}/insight", s.handleGetLogInsight)

	// Admin & System
	mux.HandleFunc("GET /api/stats", s.handleGetStats)
	mux.HandleFunc("GET /api/health", s.metrics.HealthHandler())
	mux.HandleFunc("GET /live", s.handleLive)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.Handle("GET /metrics/prometheus", telemetry.PrometheusHandler())
	mux.HandleFunc("DELETE /api/admin/purge", s.handlePurge)
	mux.HandleFunc("POST /api/admin/vacuum", s.handleVacuum)
	mux.HandleFunc("POST /api/admin/drop_fts", s.handleDropFTS)

	// WebSockets
	mux.HandleFunc("/ws", s.hub.HandleWebSocket)
	mux.HandleFunc("/ws/health", s.metrics.HealthWSHandler())
	mux.HandleFunc("/ws/events", s.eventHub.HandleWebSocket)
}

const (
	pagingDefaultLimit = 50
	pagingMaxLimit     = 1000
)

// parsePaging reads "limit" and "offset" from the request query string and
// applies safety clamping so GORM never sees a negative or unbounded Limit.
//
//   - limit: floored at 1, capped at pagingMaxLimit (1000). When absent the
//     caller-supplied defaultLimit is used (also clamped).
//   - offset: floored at 0. When absent defaults to 0.
func parsePaging(r *http.Request, defaultLimit int) (limit, offset int) {
	limit = defaultLimit
	if limit < 1 {
		limit = 1
	}
	if limit > pagingMaxLimit {
		limit = pagingMaxLimit
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil {
			limit = v
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > pagingMaxLimit {
		limit = pagingMaxLimit
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil {
			offset = v
		}
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// parseTimeRange parses start and end times from request query parameters
func parseTimeRange(r *http.Request) (time.Time, time.Time, error) {
	var start, end time.Time

	if startStr := r.URL.Query().Get("start"); startStr != "" {
		if t, err := time.Parse(time.RFC3339, startStr); err == nil {
			start = t
		}
	}
	if endStr := r.URL.Query().Get("end"); endStr != "" {
		if t, err := time.Parse(time.RFC3339, endStr); err == nil {
			end = t
		}
	}

	return start, end, nil
}
