package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/api/views"
	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// handleGetTrafficMetrics handles GET /api/metrics/traffic
func (s *Server) handleGetTrafficMetrics(w http.ResponseWriter, r *http.Request) {
	// Default to last 30 minutes if not specified
	end := time.Now()
	start := end.Add(-30 * time.Minute)

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

	serviceNames := r.URL.Query()["service_name"]

	// /api/metrics/traffic returns a BARE ARRAY. Wrapping it in an envelope to
	// carry coverage would silently break every existing client, so coverage
	// travels in the response header instead (#164).
	if s.aggregateReads() {
		res, err := s.aggregateEngine.QueryBuckets(aggregate.Query{
			Tenant:   storage.TenantFromContext(r.Context()),
			Start:    start,
			End:      end,
			Services: serviceNames,
		})
		if err != nil {
			slog.Error("Failed to get aggregate traffic metrics", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		setCoverage(w, res.Coverage)
		w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(trafficPointsFromAggregate(res))
		return
	}

	points, err := s.repo.GetTrafficMetrics(r.Context(), start, end, serviceNames)
	if err != nil {
		slog.Error("Failed to get traffic metrics", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(points)
}

// trafficPointsFromAggregate converts engine traffic buckets into the same
// wire shape the legacy repository produces. Only the bucket WIDTH changes:
// five-minute aggregate windows instead of one-minute row scans.
//
// count/error_count carry the REQUEST basis, matching what the legacy path
// counts (one point per trace row). Both bases are restated by name so a client
// never has to infer which one it is plotting (#197 Q3).
func trafficPointsFromAggregate(res *aggregate.BucketsResult) []storage.TrafficPoint {
	points := make([]storage.TrafficPoint, 0, len(res.Points))
	for _, p := range res.Points {
		points = append(points, storage.TrafficPoint{
			Timestamp:     p.WindowStart,
			Count:         p.RequestCount,
			ErrorCount:    p.ErrorRequestCount,
			Requests:      p.RequestCount,
			RequestErrors: p.ErrorRequestCount,
			Spans:         p.SpanCount,
			SpanErrors:    p.SpanErrorCount,
		})
	}
	return points
}

// handleGetLatencyHeatmap handles GET /api/metrics/latency_heatmap
func (s *Server) handleGetLatencyHeatmap(w http.ResponseWriter, r *http.Request) {
	end := time.Now()
	start := end.Add(-30 * time.Minute)

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

	serviceNames := r.URL.Query()["service_name"]

	points, err := s.repo.GetLatencyHeatmap(r.Context(), start, end, serviceNames)
	if err != nil {
		slog.Error("Failed to get latency heatmap", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// The heatmap plots individual span durations, which in aggregate mode
	// exist only as retained exemplars. The endpoint keeps its bare-array
	// shape and declares that honestly in the header: an empty heatmap is not
	// evidence that no spans ran.
	if s.aggregateReads() {
		setCoverage(w, aggregate.CoverageExemplar)
	}
	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(points)
}

// handleGetDashboardStats handles GET /api/metrics/dashboard.
// The rendered JSON is cached for 10s per (tenant, query) with an ETag —
// same pattern as handleGetSystemGraph — so steady-state dashboard polling
// becomes a hash compare instead of a SQLite aggregate + JSON encode. The
// key includes the raw query string so explicit start/end/service_name
// windows never share an entry; oversized queries skip the cache (see
// maxCacheKeyQueryLen).
func (s *Server) handleGetDashboardStats(w http.ResponseWriter, r *http.Request) {
	var cacheKey string
	if len(r.URL.RawQuery) <= maxCacheKeyQueryLen {
		cacheKey = "dashboard_stats:" + storage.TenantFromContext(r.Context()) + "?" + r.URL.RawQuery
		if cached, ok := s.cache.Get(cacheKey); ok {
			cached.(*cachedJSON).write(w, r, "HIT")
			return
		}
	}

	// Default to last 30 minutes if not specified
	end := time.Now()
	start := end.Add(-30 * time.Minute)

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

	serviceNames := r.URL.Query()["service_name"]

	view, err := s.dashboardView(r, start, end, serviceNames)
	if err != nil {
		slog.Error("Failed to get dashboard stats", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cj, err := newCachedJSON(view)
	if err != nil {
		http.Error(w, "failed to encode dashboard stats", http.StatusInternalServerError)
		return
	}
	if cacheKey != "" {
		s.cache.Set(cacheKey, cj, hotPollCacheTTL)
	}
	cj.write(w, r, "MISS")
}

// dashboardView produces the dashboard payload from whichever source owns the
// numbers in this mode. In aggregate mode every figure comes from engine
// queries — no COUNT/AVG/DISTINCT scan of the trace or log tables, and no
// sqliteP99RowCap sort: the p99 comes from the merged sketch with the accuracy
// bound that sketch justifies.
func (s *Server) dashboardView(r *http.Request, start, end time.Time, serviceNames []string) (views.DashboardStats, error) {
	if s.aggregateReads() {
		res, err := s.aggregateEngine.QueryDashboard(aggregate.Query{
			Tenant:   storage.TenantFromContext(r.Context()),
			Start:    start,
			End:      end,
			Services: serviceNames,
		})
		if err != nil {
			return views.DashboardStats{}, err
		}
		return views.DashboardStatsFromAggregate(res), nil
	}
	stats, err := s.repo.GetDashboardStats(r.Context(), start, end, serviceNames)
	if err != nil {
		return views.DashboardStats{}, err
	}
	return views.DashboardStatsFromModel(stats), nil
}

// handleGetServiceMapMetrics handles GET /api/metrics/service-map.
// Results are cached for 30s per (tenant, window) — the dashboard polls this
// endpoint and the underlying span aggregation is among the most expensive
// queries in the API surface. The key uses the raw start/end params so the
// default rolling window (no params) shares a single entry instead of being
// re-keyed on every request timestamp.
func (s *Server) handleGetServiceMapMetrics(w http.ResponseWriter, r *http.Request) {
	const cacheTTL = 30 * time.Second
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	cacheKey := "service_map:" + storage.TenantFromContext(r.Context()) + ":" + startStr + ":" + endStr

	if cached, ok := s.cache.Get(cacheKey); ok {
		w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
		w.Header().Set("X-Cache", "HIT")
		_ = json.NewEncoder(w).Encode(cached)
		return
	}

	end := time.Now()
	start := end.Add(-30 * time.Minute)
	if startStr != "" {
		if t, err := time.Parse(time.RFC3339, startStr); err == nil {
			start = t
		}
	}
	if endStr != "" {
		if t, err := time.Parse(time.RFC3339, endStr); err == nil {
			end = t
		}
	}

	resp, err := s.serviceMapView(r, start, end)
	if err != nil {
		slog.Error("Failed to get service map metrics", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.cache.Set(cacheKey, resp, cacheTTL)
	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	w.Header().Set("X-Cache", "MISS")
	_ = json.NewEncoder(w).Encode(resp)
}

// serviceMapView produces the topology payload.
//
// In aggregate mode the NODES come from engine queries and are exact for
// accepted telemetry. The EDGES do not: caller/callee identity is not part of
// a SeriesKey, so no reducer emits a service-edge series today and the edge
// metrics come from the GraphRAG topology store, which is fed by retained
// exemplars. That is why the response is marked "sampled" rather than "full" —
// the node counts are complete, the edges are not.
func (s *Server) serviceMapView(r *http.Request, start, end time.Time) (views.ServiceMapMetrics, error) {
	if !s.aggregateReads() {
		metrics, err := s.repo.GetServiceMapMetrics(r.Context(), start, end)
		if err != nil {
			return views.ServiceMapMetrics{}, err
		}
		return views.ServiceMapMetricsFromModel(metrics), nil
	}
	res, err := s.aggregateEngine.QueryTopology(aggregate.Query{
		Tenant: storage.TenantFromContext(r.Context()),
		Start:  start,
		End:    end,
	})
	if err != nil {
		return views.ServiceMapMetrics{}, err
	}
	return views.ServiceMapMetricsFromAggregate(res, s.topologyEdges(r.Context()), aggregate.CoverageSampled), nil
}

// topologyEdges reads caller/callee edges out of the GraphRAG service store.
// The edges themselves are observed for every span before any retention gate,
// but their call counts and latencies come from retained spans only.
func (s *Server) topologyEdges(ctx context.Context) []views.ServiceMapEdge {
	if s.graphRAG == nil {
		return nil
	}
	all := s.graphRAG.AllServiceEdges(ctx)
	edges := make([]views.ServiceMapEdge, 0, len(all))
	for _, e := range all {
		if e.Type != "CALLS" {
			continue
		}
		edges = append(edges, views.ServiceMapEdge{
			Source:       e.FromID,
			Target:       e.ToID,
			CallCount:    e.CallCount,
			AvgLatencyMs: e.AvgMs,
			ErrorRate:    e.ErrorRate,
		})
	}
	return edges
}

// handleGetMetricBuckets handles GET /api/metrics
func (s *Server) handleGetMetricBuckets(w http.ResponseWriter, r *http.Request) {
	start, end, err := parseTimeRange(r)
	if err != nil {
		http.Error(w, "invalid time range", http.StatusBadRequest)
		return
	}

	name := r.URL.Query().Get("name")
	serviceName := r.URL.Query().Get("service_name")

	// name is required for bucket queries
	if name == "" {
		http.Error(w, "metric name is required", http.StatusBadRequest)
		return
	}

	buckets, err := s.repo.GetMetricBuckets(r.Context(), start, end, serviceName, name)
	if err != nil {
		slog.Error("Failed to get metric buckets", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(views.MetricBucketsFromModels(buckets))
}

// handleGetMetricNames handles GET /api/metadata/metrics
func (s *Server) handleGetMetricNames(w http.ResponseWriter, r *http.Request) {
	serviceName := r.URL.Query().Get("service_name")

	names, err := s.repo.GetMetricNames(r.Context(), serviceName)
	if err != nil {
		slog.Error("Failed to get metric names", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(names)
}

// handleGetServices returns the list of services the caller's tenant has
// emitted any span for. Read from the in-memory GraphRAG ServiceStore so
// the dropdown matches /api/system/graph exactly — and so a service that
// only appears as a downstream callee (e.g. shipping-service deep in a
// fan-out) isn't silently dropped because some other span won the
// trace_id-uniqueness race for the legacy `traces` table query.
//
// Cold-start (first ~60s after restart, before the GraphRAG refresh loop
// rebuilds from DB) returns an empty list, which is correct: nothing has
// been ingested yet that the dropdown could meaningfully display.
func (s *Server) handleGetServices(w http.ResponseWriter, r *http.Request) {
	var services []string
	if s.graphRAG != nil {
		services = s.graphRAG.ServiceNames(r.Context())
	}
	if services == nil {
		services = []string{}
	}
	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(services)
}
