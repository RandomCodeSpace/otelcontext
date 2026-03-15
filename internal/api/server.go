package api

import (
	"net/http"
	"time"

	"github.com/RandomCodeSpace/argus/internal/cache"
	"github.com/RandomCodeSpace/argus/internal/graph"
	"github.com/RandomCodeSpace/argus/internal/realtime"
	"github.com/RandomCodeSpace/argus/internal/storage"
	"github.com/RandomCodeSpace/argus/internal/telemetry"
)

// Server handles HTTP API requests.
type Server struct {
	repo     *storage.Repository
	hub      *realtime.Hub
	eventHub *realtime.EventHub
	metrics  *telemetry.Metrics
	cache    *cache.TTLCache
	graph    *graph.Graph // in-memory service dependency graph (may be nil before first build)
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
// Called from main after the graph is created so startup order is flexible.
func (s *Server) SetGraph(g *graph.Graph) {
	s.graph = g
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
	mux.HandleFunc("GET /api/logs/{id}/insight", s.handleGetLogInsight)

	// Admin & System
	mux.HandleFunc("GET /api/stats", s.handleGetStats)
	mux.HandleFunc("GET /api/health", s.metrics.HealthHandler())
	mux.Handle("GET /metrics", telemetry.PrometheusHandler())
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
