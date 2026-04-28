package api

import (
	"net/http"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/cache"
	"github.com/RandomCodeSpace/otelcontext/internal/graph"
	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/realtime"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"
)

// Server handles HTTP API requests.
type Server struct {
	repo      *storage.Repository
	hub       *realtime.Hub
	eventHub  *realtime.EventHub
	metrics   *telemetry.Metrics
	cache     *cache.TTLCache
	graph     *graph.Graph       // in-memory service dependency graph (may be nil before first build)
	graphRAG  *graphrag.GraphRAG // layered GraphRAG for advanced queries
	vectorIdx *vectordb.Index    // TF-IDF semantic log search index

	// Saturation probes consulted by /ready. Each returns a fullness
	// fraction in [0.0, 1.0]; nil disables the corresponding check.
	// Decoupling via callbacks keeps the api package free of queue/ingest
	// imports and lets tests inject deterministic values.
	dlqSaturation      func() float64
	pipelineSaturation func() float64
}

// NewServer creates a new API server.
func NewServer(repo *storage.Repository, hub *realtime.Hub, eventHub *realtime.EventHub, metrics *telemetry.Metrics) *Server {
	return &Server{
		repo:     repo,
		hub:      hub,
		eventHub: eventHub,
		metrics:  metrics,
		cache:    cache.New(),
	}
}

// SetGraph wires the in-memory service graph into the API server.
func (s *Server) SetGraph(g *graph.Graph) {
	s.graph = g
}

// SetGraphRAG wires the GraphRAG instance for advanced queries.
func (s *Server) SetGraphRAG(g *graphrag.GraphRAG) {
	s.graphRAG = g
}

// SetVectorIndex wires the TF-IDF vector index for semantic log search.
func (s *Server) SetVectorIndex(idx *vectordb.Index) {
	s.vectorIdx = idx
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

	// Traces
	mux.HandleFunc("GET /api/traces", s.handleGetTraces)
	mux.HandleFunc("GET /api/traces/{id}", s.handleGetTraceByID)

	// Logs
	mux.HandleFunc("GET /api/logs", s.handleGetLogs)
	mux.HandleFunc("GET /api/logs/context", s.handleGetLogContext)
	mux.HandleFunc("GET /api/logs/similar", s.handleGetSimilarLogs)
	mux.HandleFunc("GET /api/logs/{id}/insight", s.handleGetLogInsight)

	// Admin & System
	mux.HandleFunc("GET /api/stats", s.handleGetStats)
	mux.HandleFunc("GET /api/health", s.metrics.HealthHandler())
	mux.HandleFunc("GET /live", s.handleLive)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.Handle("GET /metrics/prometheus", telemetry.PrometheusHandler())
	mux.HandleFunc("DELETE /api/admin/purge", s.handlePurge)
	mux.HandleFunc("POST /api/admin/vacuum", s.handleVacuum)

	// WebSockets
	mux.HandleFunc("/ws", s.hub.HandleWebSocket)
	mux.HandleFunc("/ws/health", s.metrics.HealthWSHandler())
	mux.HandleFunc("/ws/events", s.eventHub.HandleWebSocket)
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
