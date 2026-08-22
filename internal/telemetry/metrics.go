package telemetry

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric naming convention: historical metrics use the PascalCase
// `OtelContext_*` prefix; all metrics added during the backend-robustness
// initiative (and later) use the Prometheus-idiomatic lowercase
// `otelcontext_*` prefix. Both are kept for backward compatibility —
// operators querying by prefix should match `(?i)^otelcontext_`. Migrate
// the legacy names when a major version bump is acceptable.

// Metrics holds all internal Prometheus metrics for OtelContext self-monitoring.
type Metrics struct {
	// --- Existing ---
	IngestionRate     prometheus.Counter
	ActiveConnections prometheus.Gauge
	DBLatency         prometheus.Histogram
	DLQSize           prometheus.Gauge

	// IngestDurationSeconds is the per-Export E2E latency observed inside
	// the OTLP servers (gRPC + HTTP), labeled by signal {traces,logs,metrics}.
	// Drives ingest SLOs: alert on p99 / error budget burn rather than on the
	// blunt OtelContext_grpc_request_duration_seconds aggregate.
	IngestDurationSeconds *prometheus.HistogramVec

	// --- gRPC ---
	GRPCRequestsTotal   *prometheus.CounterVec
	GRPCRequestDuration *prometheus.HistogramVec
	GRPCBatchSize       prometheus.Histogram

	// --- HTTP ---
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec

	// --- TSDB ---
	TSDBIngestTotal         prometheus.Counter
	TSDBFlushDuration       prometheus.Histogram
	TSDBBatchesDropped      prometheus.Counter
	TSDBCardinalityOverflow prometheus.Counter
	// TSDBCardinalityOverflowByTenant labels overflow events with the tenant ID
	// that triggered them, or the sentinel "__global__" when the global cap
	// (not a per-tenant cap) was the trigger. Use this to identify noisy
	// tenants: sum by (tenant_id) (rate(otelcontext_tsdb_cardinality_overflow_by_tenant_total[5m]))
	TSDBCardinalityOverflowByTenant *prometheus.CounterVec

	// --- WebSocket ---
	WSMessagesSent       *prometheus.CounterVec
	WSSlowClientsRemoved prometheus.Counter

	// --- DLQ ---
	DLQEnqueuedTotal prometheus.Counter
	DLQReplaySuccess prometheus.Counter
	DLQReplayFailure prometheus.Counter
	DLQDiskBytes     prometheus.Gauge

	// --- Storage ---
	HotDBSizeBytes prometheus.Gauge

	// --- Retention ---
	RetentionRowsPurgedTotal       *prometheus.CounterVec
	RetentionPurgeDurationSeconds  *prometheus.HistogramVec
	RetentionVacuumDurationSeconds *prometheus.HistogramVec
	RetentionRowsBehindGauge       *prometheus.GaugeVec

	// --- Postgres partitioning (DB_POSTGRES_PARTITIONING=daily) ---
	// PartitionsDropped counts daily logs partitions dropped during the
	// retention pass. Each drop is a near-instant DDL — alert when this
	// counter is flat for >1.5 retention periods (indicates a stuck loop).
	PartitionsDropped prometheus.Counter
	// PartitionsActive gauges the live partitions attached to logs.
	// Healthy steady-state ~ HOT_RETENTION_DAYS + DB_PARTITION_LOOKAHEAD_DAYS + 1.
	PartitionsActive prometheus.Gauge

	// --- Runtime ---
	GoGoroutines     prometheus.Gauge
	GoHeapAllocBytes prometheus.Gauge

	// --- Operational (Fix 6) ---
	PanicsRecoveredTotal          *prometheus.CounterVec
	MCPToolInvocationsTotal       *prometheus.CounterVec
	APIAuthFailuresTotal          *prometheus.CounterVec
	GraphRAGEventBufferDepth      prometheus.Gauge
	RetentionLastSuccessTimestamp *prometheus.GaugeVec
	RetentionConsecutiveFailures  *prometheus.GaugeVec
	DBUp                          *prometheus.GaugeVec

	// --- GraphRAG overflow ---
	GraphRAGEventsDroppedTotal *prometheus.CounterVec

	// GraphRAGTenantsEvictedTotal counts tenant store slices evicted after
	// exceeding GRAPHRAG_TENANT_IDLE_TTL. The default tenant is never
	// evicted; a steady non-zero rate on a single-tenant install means
	// rogue tenant IDs are reaching ingest.
	GraphRAGTenantsEvictedTotal prometheus.Counter

	// --- In-memory store census (OOM-survival work) ---
	// GraphRAGStoreEntities — live node counts per entity kind across tenants
	// (tenants|services|operations|traces|spans|log_clusters|metrics|anomalies).
	// GraphRAGStoreEdges — live edge counts per store (service|trace|signal|anomaly).
	// Together with the ring/drain gauges these attribute RSS growth to a
	// specific structure before a heap profile is needed.
	GraphRAGStoreEntities *prometheus.GaugeVec
	GraphRAGStoreEdges    *prometheus.GaugeVec
	TSDBRingSeriesActive  prometheus.Gauge
	// TSDBRingSeriesRejected — points refused a NEW ring series at the
	// tenant-scoped series cap (existing series keep recording).
	TSDBRingSeriesRejected prometheus.Counter
	DrainTemplatesActive   prometheus.Gauge

	// --- Async ingest pipeline (Phase 1 robustness work) ---
	// IngestPipelineQueueDepth — current queue depth, sampled on every Submit.
	// Labeled by signal so spikes can be attributed to traces vs logs.
	IngestPipelineQueueDepth *prometheus.GaugeVec
	// IngestPipelineQueueBytes — approximate bytes held by queued batches.
	// Reserved at Submit, released when a worker finishes the batch; the
	// byte cap (INGEST_PIPELINE_MAX_BYTES) rejects submissions above it.
	IngestPipelineQueueBytes prometheus.Gauge
	// IngestPipelineDroppedTotal — batches that did NOT reach the DB.
	// reason="soft_backpressure" — healthy batch dropped at >=90% fullness.
	// reason="queue_full"        — batch rejected at 100% capacity (client got 429/RESOURCE_EXHAUSTED).
	// reason="bytes_full"        — batch rejected at the byte cap (even priority batches).
	IngestPipelineDroppedTotal *prometheus.CounterVec
	// IngestPipelineDLQTotal — batches handed to the Dead Letter Queue after
	// the persist transaction failed, instead of being dropped silently.
	// result="enqueued"      — the complete batch is on disk awaiting replay.
	// result="enqueue_failed" — the DLQ itself rejected the write (disk full,
	//                           permissions); the batch IS lost.
	// result="no_sink"       — no DLQ wired into the pipeline; the batch IS lost.
	IngestPipelineDLQTotal *prometheus.CounterVec

	// ExemplarSubmitTotal — outcome of every raw-exemplar batch submission in
	// AGGREGATE_MODE=aggregate, where the durable aggregate commit is the
	// Export ACK and raw exemplar storage is bounded best-effort (#196).
	// outcome="queued" reason="none"        — the batch entered the raw pipeline.
	// outcome="dlq"    reason="queue_full"  — pipeline saturated, DLQ accepted
	//                                         the batch. Deferred, NOT lost.
	// outcome="lost"   reason="dlq_full"    — the DLQ refused it for capacity.
	// outcome="lost"   reason="dlq_error"   — no DLQ wired, or its write failed.
	// lost{reason="queue_full"} is never emitted: on the lost outcome the
	// reason names why the DLQ could not hold the batch, not why the primary
	// queue refused it. Intentional soft-backpressure drops are not counted
	// here — they are already on IngestPipelineDroppedTotal.
	ExemplarSubmitTotal *prometheus.CounterVec
	// ExemplarSubmitLostTotal — dedicated counter for the permanent-loss
	// subset of ExemplarSubmitTotal, so an alert can target loss without a
	// label matcher. reason="dlq_full"|"dlq_error".
	ExemplarSubmitLostTotal *prometheus.CounterVec

	// HTTPOTLPThrottledTotal — count of HTTP 429s issued by the OTLP HTTP
	// receiver when the async ingest pipeline is full. Mirrors the gRPC
	// RESOURCE_EXHAUSTED path so operators see a single throttling signal
	// across both transports. Label `signal` is one of traces|logs|metrics.
	HTTPOTLPThrottledTotal *prometheus.CounterVec

	// --- DB pool (sampled every 5s from sql.DB.Stats) ---
	DBPoolOpenConnections prometheus.Gauge
	DBPoolInUse           prometheus.Gauge
	DBPoolIdle            prometheus.Gauge
	DBPoolWaitCount       prometheus.Gauge
	DBPoolWaitDuration    prometheus.Gauge // cumulative seconds

	// --- DLQ eviction (Task 8) ---
	DLQEvictedTotal      prometheus.Counter
	DLQEvictedBytesTotal prometheus.Counter

	// --- Dashboard p99 (Task 10) ---
	DashboardP99RowCapHitsTotal prometheus.Counter

	// --- Aggregate engine (AGGREGATE_MODE != legacy) ---
	// AggregateInputPointsTotal — points offered to the request-local reducer
	// per signal, counted BEFORE the sampler and severity gates. This is
	// accepted telemetry, not persisted telemetry.
	AggregateInputPointsTotal *prometheus.CounterVec
	// AggregateDeltasTotal — series deltas emitted by reduction. The gap
	// between this and input points is the whole value of the engine.
	AggregateDeltasTotal *prometheus.CounterVec
	// AggregateReductionRatio — input points per emitted delta, per Export
	// request. A ratio collapsing toward 1 means cardinality is exploding.
	AggregateReductionRatio *prometheus.HistogramVec
	// AggregateLatePointsTotal — points excluded from aggregates because they
	// fell outside the mutable-window horizon. reason="late" (older than the
	// allowed lateness) or reason="future" (beyond the tolerated skew).
	AggregateLatePointsTotal *prometheus.CounterVec
	// AggregateSeriesActive — budgeted series present in at least one mutable
	// window, per signal. This is what the AGGREGATE_MAX_SERIES* caps bound,
	// and it never exceeds them: the __other__ series a cap mints when it
	// binds are the reserve, counted by AggregateOverflowSeriesActive instead.
	AggregateSeriesActive *prometheus.GaugeVec
	// AggregateOverflowSeriesActive — live __other__ series per signal. This
	// is the unbudgeted reserve the caps spend; it is bounded by
	// (services x signals x status classes), not by AGGREGATE_MAX_SERIES*.
	AggregateOverflowSeriesActive *prometheus.GaugeVec
	// AggregateOverflowTotal — admissions rerouted to an __other__ series,
	// labeled by the cap that triggered it (tenant|service_names|
	// service_series|signal|global). Totals are preserved; identity is not.
	AggregateOverflowTotal *prometheus.CounterVec
	// AggregateShadowAcceptedTotal — telemetry accounted on the aggregate
	// side per signal. In shadow mode this is compared against the legacy
	// path's accepted counts; it must not move with the sampling rate.
	AggregateShadowAcceptedTotal *prometheus.CounterVec
	// AggregateShadowErrorsTotal — errors accounted on the aggregate side per
	// service. Cheap invariant only (#165): no per-series comparison.
	AggregateShadowErrorsTotal *prometheus.CounterVec

	// --- Durable aggregate store (#173) ---
	// AggregateCommitDurationSeconds — group-commit wall time. This IS the
	// ACK latency floor: an Export cannot return before its commit does.
	AggregateCommitDurationSeconds *prometheus.HistogramVec
	// AggregateCommitDeltas — delta rows per group commit. The pre-merge
	// ratio and the coalescing behaviour both show up here.
	AggregateCommitDeltas prometheus.Histogram
	// AggregateCommitsTotal — commits by result (ok|error).
	AggregateCommitsTotal *prometheus.CounterVec
	// AggregateCommitBytesTotal — delta payload written to the store.
	AggregateCommitBytesTotal prometheus.Counter
	// AggregateAdmissionRejectedTotal — ErrSaturated refusals by the bound
	// that tripped (bytes|waiters|deltas). Non-zero at sustained load is a
	// release-gate failure, not a tuning hint.
	AggregateAdmissionRejectedTotal *prometheus.CounterVec
	// AggregateFinalizeDurationSeconds — window finalization wall time.
	AggregateFinalizeDurationSeconds prometheus.Histogram
	// AggregateFinalizeRowsTotal — rows materialized/deleted by finalization,
	// by kind (buckets|deltas).
	AggregateFinalizeRowsTotal *prometheus.CounterVec
	// AggregatePurgeDurationSeconds — retention purge wall time on the
	// aggregate DB.
	AggregatePurgeDurationSeconds prometheus.Histogram
	// AggregatePurgeRowsTotal — rows purged by kind (buckets|deltas|baselines).
	AggregatePurgeRowsTotal *prometheus.CounterVec
	// AggregateDeltaLogRows and AggregateDeltaLogAgeSeconds are the delta-log
	// backlog health bounds from #160: alert when either climbs.
	AggregateDeltaLogRows       prometheus.Gauge
	AggregateDeltaLogAgeSeconds prometheus.Gauge
	// AggregateRecoveryDurationSeconds and AggregateRecoveryRows describe the
	// last startup recovery. The gate allows 30s.
	AggregateRecoveryDurationSeconds prometheus.Gauge
	AggregateRecoveryRows            *prometheus.GaugeVec
	// --- Bounded exemplar retention (AGGREGATE_MODE=aggregate) ---
	// ExemplarEligibleTotal — telemetry that qualified for raw retention,
	// per signal and priority class. Eligible is not retained: the gap between
	// this and the drop counter is what makes aggregate completeness and raw
	// diagnostic coverage distinguishable during a storm (#161).
	ExemplarEligibleTotal *prometheus.CounterVec
	// ExemplarDroppedTotal — eligible telemetry refused raw persistence,
	// reason=budget_count|budget_bytes|stratum.
	ExemplarDroppedTotal *prometheus.CounterVec
	// ExemplarEvictionTotal — selected exemplars displaced by a better-ranked
	// trace. Each eviction is one trace's worth of bounded OVER-retention:
	// already-persisted spans are never deleted.
	ExemplarEvictionTotal prometheus.Counter
	// ExemplarTruncatedTotal — retained traces forced past their max spans or
	// max bytes. These persist truncated=true plus retained/observed counts,
	// and causal-analysis tools report partial coverage for them (#163).
	ExemplarTruncatedTotal prometheus.Counter

	// Atomic counters for JSON health endpoint (avoids scraping Prometheus)
	totalIngested  atomic.Int64
	activeConns    atomic.Int64
	dlqFileCount   atomic.Int64
	dbLatencyP99Ms atomic.Int64
	startTime      time.Time
}

// New creates and registers all OtelContext internal metrics.
func New() *Metrics {
	m := &Metrics{
		startTime: time.Now(),

		// Existing
		IngestionRate: promauto.NewCounter(prometheus.CounterOpts{
			Name: "OtelContext_ingestion_rate",
			Help: "Total number of spans and logs ingested.",
		}),
		ActiveConnections: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "OtelContext_active_connections",
			Help: "Number of active WebSocket client connections.",
		}),
		DBLatency: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "OtelContext_db_latency",
			Help:    "Database operation latency in seconds.",
			Buckets: prometheus.DefBuckets,
		}),
		DLQSize: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "OtelContext_dlq_size",
			Help: "Number of files currently in the Dead Letter Queue.",
		}),

		IngestDurationSeconds: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "otelcontext_ingest_duration_seconds",
			Help:    "End-to-end OTLP Export latency observed in the ingest server, by signal.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"signal"}),

		// gRPC
		GRPCRequestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "OtelContext_grpc_requests_total",
			Help: "Total gRPC requests by method and status.",
		}, []string{"method", "status"}),
		GRPCRequestDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "OtelContext_grpc_request_duration_seconds",
			Help:    "gRPC request latency in seconds.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"method"}),
		GRPCBatchSize: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "OtelContext_grpc_batch_size",
			Help:    "Number of spans/logs per OTLP Export call.",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500},
		}),

		// HTTP
		HTTPRequestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "OtelContext_http_requests_total",
			Help: "Total HTTP requests by method, path, and status.",
		}, []string{"method", "path", "status"}),
		HTTPRequestDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "OtelContext_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"method", "path"}),

		// TSDB
		TSDBIngestTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "OtelContext_tsdb_ingest_total",
			Help: "Total raw metric data points ingested into TSDB.",
		}),
		TSDBFlushDuration: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "OtelContext_tsdb_flush_duration_seconds",
			Help:    "Time taken to flush a TSDB window to disk.",
			Buckets: prometheus.DefBuckets,
		}),
		TSDBBatchesDropped: promauto.NewCounter(prometheus.CounterOpts{
			Name: "OtelContext_tsdb_batches_dropped_total",
			Help: "TSDB batches dropped due to full flush channel.",
		}),
		TSDBCardinalityOverflow: promauto.NewCounter(prometheus.CounterOpts{
			Name: "OtelContext_tsdb_cardinality_overflow_total",
			Help: "Metric points routed to overflow bucket due to cardinality limit.",
		}),
		TSDBCardinalityOverflowByTenant: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "otelcontext_tsdb_cardinality_overflow_by_tenant_total",
			Help: "Metric points routed to overflow bucket, labeled by the tenant_id that exceeded its cap (or __global__ when the global cap triggered).",
		}, []string{"tenant_id"}),

		// WebSocket
		WSMessagesSent: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "OtelContext_ws_messages_sent_total",
			Help: "Total WebSocket messages broadcast by type.",
		}, []string{"type"}),
		WSSlowClientsRemoved: promauto.NewCounter(prometheus.CounterOpts{
			Name: "OtelContext_ws_slow_clients_removed_total",
			Help: "WebSocket clients dropped due to slow consumption.",
		}),

		// DLQ
		DLQEnqueuedTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "OtelContext_dlq_enqueued_total",
			Help: "Total batches written to the Dead Letter Queue.",
		}),
		DLQReplaySuccess: promauto.NewCounter(prometheus.CounterOpts{
			Name: "OtelContext_dlq_replay_success_total",
			Help: "Successful DLQ replay attempts.",
		}),
		DLQReplayFailure: promauto.NewCounter(prometheus.CounterOpts{
			Name: "OtelContext_dlq_replay_failure_total",
			Help: "Failed DLQ replay attempts.",
		}),
		DLQDiskBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "OtelContext_dlq_disk_bytes",
			Help: "Total disk usage of the DLQ directory in bytes.",
		}),

		// Storage
		HotDBSizeBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "OtelContext_hot_db_size_bytes",
			Help: "Approximate hot database size in bytes.",
		}),

		// Retention
		RetentionRowsPurgedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "OtelContext_retention_rows_purged_total",
			Help: "Total rows purged by retention, by table and driver.",
		}, []string{"table", "driver"}),
		RetentionPurgeDurationSeconds: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "OtelContext_retention_purge_duration_seconds",
			Help:    "Wall-clock duration of a full retention purge pass, by driver.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 16),
		}, []string{"driver"}),
		RetentionVacuumDurationSeconds: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "OtelContext_retention_vacuum_duration_seconds",
			Help:    "Duration of per-table retention maintenance (VACUUM/ANALYZE/OPTIMIZE), by driver and table.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 16),
		}, []string{"driver", "table"}),
		RetentionRowsBehindGauge: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "otelcontext_retention_rows_behind",
			Help: "Rows older than retention cutoff that have not yet been purged. Climbing means purge cannot keep pace with ingest.",
		}, []string{"table", "driver"}),

		// Postgres partitioning
		PartitionsDropped: promauto.NewCounter(prometheus.CounterOpts{
			Name: "otelcontext_partitions_dropped_total",
			Help: "Total daily logs partitions dropped by the partition scheduler. Increments by `n` when n partitions are dropped on a single tick.",
		}),
		PartitionsActive: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "otelcontext_partitions_active",
			Help: "Live partitions attached to the logs parent. Steady-state ≈ HOT_RETENTION_DAYS + DB_PARTITION_LOOKAHEAD_DAYS + 1.",
		}),

		// Runtime
		GoGoroutines: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "OtelContext_go_goroutines",
			Help: "Current number of active goroutines.",
		}),
		GoHeapAllocBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "OtelContext_go_heap_alloc_bytes",
			Help: "Current Go heap allocations in bytes.",
		}),

		// Operational (Fix 6)
		PanicsRecoveredTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "OtelContext_panics_recovered_total",
			Help: "Panics recovered by subsystem (http|grpc|graphrag|retention|ingest).",
		}, []string{"subsystem"}),
		MCPToolInvocationsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "OtelContext_mcp_tool_invocations_total",
			Help: "MCP tool invocations by tool and status (ok|error).",
		}, []string{"tool", "status"}),
		APIAuthFailuresTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "OtelContext_api_auth_failures_total",
			Help: "API key auth failures by reason (missing_header|bad_scheme|bad_key).",
		}, []string{"reason"}),
		GraphRAGEventBufferDepth: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "OtelContext_graphrag_event_buffer_depth",
			Help: "Current depth of the GraphRAG ingestion event channel.",
		}),
		RetentionLastSuccessTimestamp: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "OtelContext_retention_last_success_timestamp",
			Help: "Unix timestamp of the last successful retention job (purge|maintenance).",
		}, []string{"job"}),
		RetentionConsecutiveFailures: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "OtelContext_retention_consecutive_failures",
			Help: "Consecutive failure count of the last retention job (purge|maintenance).",
		}, []string{"job"}),
		DBUp: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "OtelContext_db_up",
			Help: "Database reachability (1=up, 0=down) by driver.",
		}, []string{"driver"}),

		GraphRAGEventsDroppedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "otelcontext_graphrag_events_dropped_total",
			Help: "Events dropped because the GraphRAG event channel was full.",
		}, []string{"signal"}),
		GraphRAGTenantsEvictedTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "otelcontext_graphrag_tenants_evicted_total",
			Help: "Tenant store slices evicted after exceeding the idle TTL (GRAPHRAG_TENANT_IDLE_TTL). The default tenant is never evicted.",
		}),
		GraphRAGStoreEntities: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "otelcontext_graphrag_store_entities",
			Help: "Live GraphRAG node counts across tenants, by entity kind (tenants|services|operations|traces|spans|log_clusters|metrics|anomalies).",
		}, []string{"entity"}),
		GraphRAGStoreEdges: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "otelcontext_graphrag_store_edges",
			Help: "Live GraphRAG edge counts across tenants, by store (service|trace|signal|anomaly).",
		}, []string{"store"}),
		TSDBRingSeriesActive: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "otelcontext_tsdb_ring_series_active",
			Help: "Distinct metric series currently held in TSDB ring buffers.",
		}),
		TSDBRingSeriesRejected: promauto.NewCounter(prometheus.CounterOpts{
			Name: "otelcontext_tsdb_ring_series_rejected_total",
			Help: "Metric points refused a new TSDB ring series at the cardinality cap (existing series keep recording).",
		}),
		DrainTemplatesActive: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "otelcontext_drain_templates_active",
			Help: "Live Drain log templates (bounded by the 50k LRU cap).",
		}),

		IngestPipelineQueueDepth: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "otelcontext_ingest_pipeline_queue_depth",
			Help: "Current depth of the async ingest pipeline queue, by signal type.",
		}, []string{"signal"}),
		IngestPipelineQueueBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "otelcontext_ingest_pipeline_queue_bytes",
			Help: "Approximate bytes held by batches in the async ingest queue.",
		}),
		HTTPOTLPThrottledTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "otelcontext_http_otlp_throttled_total",
			Help: "OTLP HTTP requests rejected with 429 because the async ingest pipeline is at capacity, by signal type.",
		}, []string{"signal"}),
		IngestPipelineDroppedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "otelcontext_ingest_pipeline_dropped_total",
			Help: "Batches dropped by the async ingest pipeline. reason=soft_backpressure (>=90% queue, healthy) or queue_full (100% queue, rejected to client).",
		}, []string{"signal", "reason"}),
		IngestPipelineDLQTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "otelcontext_ingest_pipeline_dlq_total",
			Help: "Async ingest batches routed to the DLQ after a persist failure. result=enqueued (durable, awaiting replay), enqueue_failed or no_sink (batch lost).",
		}, []string{"signal", "result"}),
		ExemplarSubmitTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "otelcontext_exemplar_submit_total",
			Help: "Raw exemplar batch submissions in aggregate mode. outcome=queued (raw pipeline), dlq (deferred after queue_full), lost (dlq_full or dlq_error).",
		}, []string{"signal", "outcome", "reason"}),
		ExemplarSubmitLostTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "otelcontext_exemplar_submit_lost_total",
			Help: "Raw exemplar batches permanently lost in aggregate mode after the DLQ fallback failed. reason=dlq_full or dlq_error.",
		}, []string{"signal", "reason"}),

		// DB pool (Task 7 — visibility for DB_MAX_OPEN_CONNS sizing).
		DBPoolOpenConnections: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "otelcontext_db_pool_open_connections",
			Help: "Current number of open DB connections in the pool.",
		}),
		DBPoolInUse: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "otelcontext_db_pool_in_use",
			Help: "Current number of DB connections in use.",
		}),
		DBPoolIdle: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "otelcontext_db_pool_idle",
			Help: "Current number of idle DB connections.",
		}),
		DBPoolWaitCount: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "otelcontext_db_pool_wait_count",
			Help: "Cumulative connection waits since DB open (gauge-reported; compute rate() over this value).",
		}),
		DBPoolWaitDuration: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "otelcontext_db_pool_wait_duration_seconds",
			Help: "Cumulative wait duration for pool acquisition, in seconds (gauge-reported; compute rate() over this value).",
		}),
	}
	m.DLQEvictedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "otelcontext_dlq_evicted_total",
		Help: "DLQ files evicted to stay under MaxFiles/MaxDiskMB. Non-zero means backlog exceeds cap — investigate DB health.",
	})
	m.DLQEvictedBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "otelcontext_dlq_evicted_bytes_total",
		Help: "Total bytes evicted from DLQ. Rate indicates data-loss volume during backlog.",
	})
	m.DashboardP99RowCapHitsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "otelcontext_dashboard_p99_row_cap_hits_total",
		Help: "Number of dashboard p99 computations that hit the SQLite row cap (200k). Indicates the dataset is too large for in-memory p99 — use Postgres for prod.",
	})
	m.AggregateInputPointsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_input_points_total",
		Help: "Points offered to the aggregate reducer, per signal, counted before sampling and severity gates.",
	}, []string{"signal"})
	m.AggregateDeltasTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_deltas_total",
		Help: "Series deltas emitted by aggregate reduction, per signal.",
	}, []string{"signal"})
	m.AggregateReductionRatio = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "otelcontext_aggregate_reduction_ratio",
		Help:    "Input points per emitted delta, per Export request. Falling toward 1 means cardinality is exploding.",
		Buckets: []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000},
	}, []string{"signal"})
	m.AggregateLatePointsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_late_points_total",
		Help: "Points excluded from aggregates for falling outside the mutable-window horizon. reason=late|future.",
	}, []string{"signal", "reason"})
	m.AggregateSeriesActive = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "otelcontext_aggregate_series_active",
		Help: "Budgeted series present in at least one mutable window, per signal. Bounded by the AGGREGATE_MAX_SERIES* caps.",
	}, []string{"signal"})
	m.AggregateOverflowSeriesActive = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "otelcontext_aggregate_overflow_series_active",
		Help: "Live __other__ series per signal — the unbudgeted reserve a cap spends when it binds. Bounded by services x status classes, not by the caps.",
	}, []string{"signal"})
	m.AggregateOverflowTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_overflow_total",
		Help: "Admissions rerouted to an __other__ series, by the cap that triggered it. Totals are preserved; identity detail is not.",
	}, []string{"signal", "reason"})
	m.AggregateShadowAcceptedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_shadow_accepted_total",
		Help: "Telemetry accounted on the aggregate side, per signal. Must not move with SAMPLING_RATE.",
	}, []string{"signal"})
	m.AggregateShadowErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_shadow_errors_total",
		Help: "Errors accounted on the aggregate side, per service. Cheap shadow-mode invariant only.",
	}, []string{"service"})
	m.AggregateCommitDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "otelcontext_aggregate_commit_duration_seconds",
		Help:    "Group-commit wall time on the aggregate store. This is the floor of OTLP ACK latency under durable ACK.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
	}, []string{"result"})
	m.AggregateCommitDeltas = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "otelcontext_aggregate_commit_deltas",
		Help:    "Delta rows per group commit. Shows the pre-merge ratio and how much coalescing is happening.",
		Buckets: []float64{1, 10, 50, 100, 500, 1000, 2500, 5000, 10000},
	})
	m.AggregateCommitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_commits_total",
		Help: "Aggregate group commits by result (ok|error).",
	}, []string{"result"})
	m.AggregateCommitBytesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_commit_bytes_total",
		Help: "Approximate delta payload written to the aggregate delta log.",
	})
	m.AggregateAdmissionRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_admission_rejected_total",
		Help: "Group-commit admissions refused (RESOURCE_EXHAUSTED / 429), by the bound that tripped: bytes|waiters|deltas.",
	}, []string{"bound"})
	m.AggregateFinalizeDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "otelcontext_aggregate_finalize_duration_seconds",
		Help:    "Wall time of one window finalization (materialize buckets + delete incorporated deltas).",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})
	m.AggregateFinalizeRowsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_finalize_rows_total",
		Help: "Rows written or removed by window finalization, by kind (buckets|deltas).",
	}, []string{"kind"})
	m.AggregatePurgeDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "otelcontext_aggregate_purge_duration_seconds",
		Help:    "Wall time of one retention purge on the aggregate store.",
		Buckets: []float64{0.01, 0.1, 0.5, 1, 5, 15, 60, 300},
	})
	m.AggregatePurgeRowsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_aggregate_purge_rows_total",
		Help: "Rows purged from the aggregate store by kind (buckets|deltas|baselines).",
	}, []string{"kind"})
	m.AggregateDeltaLogRows = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "otelcontext_aggregate_delta_log_rows",
		Help: "Delta-log rows awaiting window finalization. Sustained growth means the finalizer is falling behind.",
	})
	m.AggregateDeltaLogAgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "otelcontext_aggregate_delta_log_age_seconds",
		Help: "Age of the oldest un-finalized window. Should stay near the window size plus allowed lateness.",
	})
	m.AggregateRecoveryDurationSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "otelcontext_aggregate_recovery_duration_seconds",
		Help: "Wall time of the last aggregate startup recovery. Readiness is held false for its duration.",
	})
	m.AggregateRecoveryRows = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "otelcontext_aggregate_recovery_rows",
		Help: "Rows handled by the last aggregate startup recovery, by kind (replayed|finalized_windows).",
	}, []string{"kind"})
	m.ExemplarEligibleTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_exemplar_eligible_total",
		Help: "Telemetry eligible for raw exemplar retention, by signal and priority class (error|slow|healthy|warn). Eligible is not retained.",
	}, []string{"signal", "class"})
	m.ExemplarDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otelcontext_exemplar_dropped_total",
		Help: "Eligible telemetry refused raw persistence by the exemplar budgets. reason=budget_count|budget_bytes|stratum.",
	}, []string{"signal", "reason"})
	m.ExemplarEvictionTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "otelcontext_exemplar_eviction_total",
		Help: "Selected exemplars displaced by a better-ranked trace. Bounded over-retention: already-persisted spans are not deleted.",
	})
	m.ExemplarTruncatedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "otelcontext_exemplar_truncated_total",
		Help: "Retained traces forced past max spans/bytes. Persisted with truncated=true plus retained/observed span counts.",
	})
	return m
}

// RecordExemplarEligible implements ingest.ExemplarMetrics.
func (m *Metrics) RecordExemplarEligible(signal, class string) {
	m.ExemplarEligibleTotal.WithLabelValues(signal, class).Inc()
}

// RecordExemplarDropped implements ingest.ExemplarMetrics.
func (m *Metrics) RecordExemplarDropped(signal, reason string) {
	m.ExemplarDroppedTotal.WithLabelValues(signal, reason).Inc()
}

// RecordExemplarEviction implements ingest.ExemplarMetrics.
func (m *Metrics) RecordExemplarEviction() { m.ExemplarEvictionTotal.Inc() }

// RecordExemplarTruncation implements ingest.ExemplarMetrics.
func (m *Metrics) RecordExemplarTruncation() { m.ExemplarTruncatedTotal.Inc() }

// StartRuntimeMetrics samples Go runtime stats every 15 seconds.
func (m *Metrics) StartRuntimeMetrics() {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		var ms runtime.MemStats
		for range ticker.C {
			runtime.ReadMemStats(&ms)
			m.GoGoroutines.Set(float64(runtime.NumGoroutine()))
			m.GoHeapAllocBytes.Set(float64(ms.HeapAlloc))
		}
	}()
}

// SampleDBPoolStats writes the live pool stats into the DBPool* gauges. Safe
// to call from a ticker goroutine. A nil receiver or a nil *sql.DB is a no-op
// so callers don't need to guard at every call site.
//
// WaitCount and WaitDuration from sql.DBStats are cumulative values (always
// monotonically increasing) — operators should compute rate() over them.
func (m *Metrics) SampleDBPoolStats(sqlDB *sql.DB) {
	if m == nil || sqlDB == nil {
		return
	}
	s := sqlDB.Stats()
	m.DBPoolOpenConnections.Set(float64(s.OpenConnections))
	m.DBPoolInUse.Set(float64(s.InUse))
	m.DBPoolIdle.Set(float64(s.Idle))
	m.DBPoolWaitCount.Set(float64(s.WaitCount))
	m.DBPoolWaitDuration.Set(s.WaitDuration.Seconds())
}

// --- Existing helper methods ---

func (m *Metrics) RecordIngestion(count int) {
	m.IngestionRate.Add(float64(count))
	m.totalIngested.Add(int64(count))
}

// ObserveIngestDuration records an end-to-end OTLP Export latency for the
// given signal. Callers should pass time.Since(start) measured from the very
// start of the Export handler. Nil-safe so the OTLP servers can be wired
// without a Metrics instance during tests.
func (m *Metrics) ObserveIngestDuration(signal string, d time.Duration) {
	if m == nil || m.IngestDurationSeconds == nil {
		return
	}
	m.IngestDurationSeconds.WithLabelValues(signal).Observe(d.Seconds())
}

func (m *Metrics) SetActiveConnections(n int) {
	m.ActiveConnections.Set(float64(n))
	m.activeConns.Store(int64(n))
}

func (m *Metrics) IncrementActiveConns() {
	n := m.activeConns.Add(1)
	m.ActiveConnections.Set(float64(n))
}

func (m *Metrics) DecrementActiveConns() {
	n := m.activeConns.Add(-1)
	if n < 0 {
		n = 0
		m.activeConns.Store(0)
	}
	m.ActiveConnections.Set(float64(n))
}

func (m *Metrics) SetDLQSize(n int) {
	m.DLQSize.Set(float64(n))
	m.dlqFileCount.Store(int64(n))
}

func (m *Metrics) ObserveDBLatency(seconds float64) {
	m.DBLatency.Observe(seconds)
	m.dbLatencyP99Ms.Store(int64(seconds * 1000))
}

// --- Health endpoint ---

// HealthStats is the JSON response for GET /api/health.
type HealthStats struct {
	IngestionRate  int64   `json:"ingestion_rate"`
	DLQSize        int64   `json:"dlq_size"`
	ActiveConns    int64   `json:"active_connections"`
	DBLatencyP99Ms float64 `json:"db_latency_p99_ms"`
	Goroutines     int     `json:"goroutines"`
	HeapAllocMB    float64 `json:"heap_alloc_mb"`
	UptimeSeconds  float64 `json:"uptime_seconds"`
}

func (m *Metrics) GetHealthStats() HealthStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return HealthStats{
		IngestionRate:  m.totalIngested.Load(),
		DLQSize:        m.dlqFileCount.Load(),
		ActiveConns:    m.activeConns.Load(),
		DBLatencyP99Ms: float64(m.dbLatencyP99Ms.Load()),
		Goroutines:     runtime.NumGoroutine(),
		HeapAllocMB:    float64(ms.HeapAlloc) / 1024 / 1024,
		UptimeSeconds:  time.Since(m.startTime).Seconds(),
	}
}

func (m *Metrics) HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.GetHealthStats())
	}
}

func PrometheusHandler() http.Handler {
	return promhttp.Handler()
}
