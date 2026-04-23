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

// Metrics holds all internal Prometheus metrics for OtelContext self-monitoring.
type Metrics struct {
	// --- Existing ---
	IngestionRate     prometheus.Counter
	ActiveConnections prometheus.Gauge
	DBLatency         prometheus.Histogram
	DLQSize           prometheus.Gauge

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
	return m
}

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
