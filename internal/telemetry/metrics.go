package telemetry

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all internal Prometheus metrics for Argus self-monitoring.
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
	TSDBIngestTotal       prometheus.Counter
	TSDBFlushDuration     prometheus.Histogram
	TSDBBatchesDropped    prometheus.Counter
	TSDBCardinalityOverflow prometheus.Counter

	// --- WebSocket ---
	WSMessagesSent        *prometheus.CounterVec
	WSSlowClientsRemoved  prometheus.Counter

	// --- DLQ ---
	DLQEnqueuedTotal    prometheus.Counter
	DLQReplaySuccess    prometheus.Counter
	DLQReplayFailure    prometheus.Counter
	DLQDiskBytes        prometheus.Gauge

	// --- Archive ---
	ArchiveRecordsMoved *prometheus.CounterVec
	HotDBSizeBytes      prometheus.Gauge
	ColdStorageBytes    prometheus.Gauge

	// --- Runtime ---
	GoGoroutines   prometheus.Gauge
	GoHeapAllocBytes prometheus.Gauge

	// Atomic counters for JSON health endpoint (avoids scraping Prometheus)
	totalIngested   atomic.Int64
	activeConns     atomic.Int64
	dlqFileCount    atomic.Int64
	dbLatencyP99Ms  atomic.Int64
	startTime       time.Time
}

// New creates and registers all Argus internal metrics.
func New() *Metrics {
	m := &Metrics{
		startTime: time.Now(),

		// Existing
		IngestionRate: promauto.NewCounter(prometheus.CounterOpts{
			Name: "argus_ingestion_rate",
			Help: "Total number of spans and logs ingested.",
		}),
		ActiveConnections: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "argus_active_connections",
			Help: "Number of active WebSocket client connections.",
		}),
		DBLatency: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "argus_db_latency",
			Help:    "Database operation latency in seconds.",
			Buckets: prometheus.DefBuckets,
		}),
		DLQSize: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "argus_dlq_size",
			Help: "Number of files currently in the Dead Letter Queue.",
		}),

		// gRPC
		GRPCRequestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "argus_grpc_requests_total",
			Help: "Total gRPC requests by method and status.",
		}, []string{"method", "status"}),
		GRPCRequestDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "argus_grpc_request_duration_seconds",
			Help:    "gRPC request latency in seconds.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"method"}),
		GRPCBatchSize: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "argus_grpc_batch_size",
			Help:    "Number of spans/logs per OTLP Export call.",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500},
		}),

		// HTTP
		HTTPRequestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "argus_http_requests_total",
			Help: "Total HTTP requests by method, path, and status.",
		}, []string{"method", "path", "status"}),
		HTTPRequestDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "argus_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"method", "path"}),

		// TSDB
		TSDBIngestTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "argus_tsdb_ingest_total",
			Help: "Total raw metric data points ingested into TSDB.",
		}),
		TSDBFlushDuration: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "argus_tsdb_flush_duration_seconds",
			Help:    "Time taken to flush a TSDB window to disk.",
			Buckets: prometheus.DefBuckets,
		}),
		TSDBBatchesDropped: promauto.NewCounter(prometheus.CounterOpts{
			Name: "argus_tsdb_batches_dropped_total",
			Help: "TSDB batches dropped due to full flush channel.",
		}),
		TSDBCardinalityOverflow: promauto.NewCounter(prometheus.CounterOpts{
			Name: "argus_tsdb_cardinality_overflow_total",
			Help: "Metric points routed to overflow bucket due to cardinality limit.",
		}),

		// WebSocket
		WSMessagesSent: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "argus_ws_messages_sent_total",
			Help: "Total WebSocket messages broadcast by type.",
		}, []string{"type"}),
		WSSlowClientsRemoved: promauto.NewCounter(prometheus.CounterOpts{
			Name: "argus_ws_slow_clients_removed_total",
			Help: "WebSocket clients dropped due to slow consumption.",
		}),

		// DLQ
		DLQEnqueuedTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "argus_dlq_enqueued_total",
			Help: "Total batches written to the Dead Letter Queue.",
		}),
		DLQReplaySuccess: promauto.NewCounter(prometheus.CounterOpts{
			Name: "argus_dlq_replay_success_total",
			Help: "Successful DLQ replay attempts.",
		}),
		DLQReplayFailure: promauto.NewCounter(prometheus.CounterOpts{
			Name: "argus_dlq_replay_failure_total",
			Help: "Failed DLQ replay attempts.",
		}),
		DLQDiskBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "argus_dlq_disk_bytes",
			Help: "Total disk usage of the DLQ directory in bytes.",
		}),

		// Archive
		ArchiveRecordsMoved: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "argus_archive_records_moved_total",
			Help: "Records moved to cold storage by data type.",
		}, []string{"type"}),
		HotDBSizeBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "argus_hot_db_size_bytes",
			Help: "Approximate hot database size in bytes.",
		}),
		ColdStorageBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "argus_cold_storage_bytes",
			Help: "Total cold archive size on disk in bytes.",
		}),

		// Runtime
		GoGoroutines: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "argus_go_goroutines",
			Help: "Current number of active goroutines.",
		}),
		GoHeapAllocBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "argus_go_heap_alloc_bytes",
			Help: "Current Go heap allocations in bytes.",
		}),
	}
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
		json.NewEncoder(w).Encode(m.GetHealthStats())
	}
}

func PrometheusHandler() http.Handler {
	return promhttp.Handler()
}
