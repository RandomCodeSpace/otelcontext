package api

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/api/views"
	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
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
		start = clampAggregateRange(w, start, end)
		res, err := s.aggregateEngine.QueryBuckets(aggregate.Query{
			Tenant:   storage.TenantFromContext(r.Context()),
			Start:    start,
			End:      end,
			Services: serviceNames,
		})
		if err != nil {
			slog.Error("Failed to get aggregate traffic metrics", "error", err)
			http.Error(w, err.Error(), aggregateReadStatus(err))
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
	// Clamp BEFORE the cache lookup so the clamp headers are stamped on
	// cache hits too — the cached payload was produced from the clamped
	// range, and the headers must say so every time it is served (#217).
	if s.aggregateReads() {
		start = clampAggregateRange(w, start, end)
	}

	var cacheKey string
	if len(r.URL.RawQuery) <= maxCacheKeyQueryLen {
		cacheKey = "dashboard_stats:" + storage.TenantFromContext(r.Context()) + "?" + r.URL.RawQuery
		if cached, ok := s.cache.Get(cacheKey); ok {
			cached.(*cachedJSON).write(w, r, "HIT")
			return
		}
	}

	serviceNames := r.URL.Query()["service_name"]

	view, err := s.dashboardView(r, start, end, serviceNames)
	if err != nil {
		slog.Error("Failed to get dashboard stats", "error", err)
		http.Error(w, err.Error(), aggregateReadStatus(err))
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
	if s.aggregateTopology() {
		cacheKey += ":" + s.topology.Identity(r.Context()).String()
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
	// Clamp BEFORE the cache lookup so the clamp headers are stamped on
	// cache hits too (#217); the cache key is the raw start/end params, so
	// every hit was produced from the same clamped range.
	if s.aggregateReads() || s.aggregateTopology() {
		start = clampAggregateRange(w, start, end)
	}

	if cached, ok := s.cache.Get(cacheKey); ok {
		w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
		w.Header().Set("X-Cache", "HIT")
		_ = json.NewEncoder(w).Encode(cached)
		return
	}

	resp, err := s.serviceMapView(r, start, end)
	if err != nil {
		slog.Error("Failed to get service map metrics", "error", err)
		http.Error(w, err.Error(), aggregateReadStatus(err))
		return
	}
	s.cache.Set(cacheKey, resp, cacheTTL)
	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	w.Header().Set("X-Cache", "MISS")
	_ = json.NewEncoder(w).Encode(resp)
}

// serviceMapView produces the topology payload.
//
// In aggregate mode BOTH halves come from one engine query over one tenant and
// one range (#194 finding 15). Edges used to be supplemented from the GraphRAG
// topology store — a different range, a different retention rule, exemplar-fed
// counts — and the response said "sampled" to cover for it. The service-edge
// series the reducer emits made that side-channel unnecessary, so the coverage
// the engine reports is the coverage the response carries.
func (s *Server) serviceMapView(r *http.Request, start, end time.Time) (views.ServiceMapMetrics, error) {
	if s.topology != nil {
		snapshot, err := s.topology.Snapshot(r.Context(), topology.Query{Start: start, End: end})
		if err != nil {
			return views.ServiceMapMetrics{}, err
		}
		return views.ServiceMapMetricsFromTopology(snapshot), nil
	}
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
	return views.ServiceMapMetricsFromAggregate(res), nil
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

	// Aggregate mode has no metric_buckets rows to read: the legacy TSDB that
	// wrote them is not started (#194 finding 10). The series come from the
	// engine's topology projection instead.
	if s.aggregateReads() {
		snap := s.aggregateEngine.TopologySnapshot(storage.TenantFromContext(r.Context()))
		buckets, coverage := metricBucketsFromTopology(snap, name, serviceName, start, end)
		setCoverage(w, coverage)
		w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(buckets)
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

// metricBucketsFromTopology projects a tenant's aggregate topology snapshot
// into the /api/metrics wire shape. The contract is unchanged — same fields,
// same bare array, same time_bucket ASC ordering — but two properties of the
// data differ and neither is hidden from the caller:
//
//   - Bucket WIDTH is the engine's five-minute window, not the TSDB's 30s.
//   - HISTORY reaches back only as far as the projection retains (the mutable
//     set plus TopologySnapshot.Horizon), not HOT_RETENTION_DAYS. A request
//     reaching further back is reported as sampled coverage, never as full.
//
// ID is 0: an aggregate window has no row identity. AttributesJSON is empty:
// the projection keys metric series by (service, name) only, so there are no
// grouped attributes to restate. Both fields keep their place in the shape.
//
// The start/end filter is inclusive at both ends, matching the legacy
// `time_bucket BETWEEN ? AND ?` predicate exactly — including the degenerate
// zero-time case, where both paths return an empty array.
func metricBucketsFromTopology(
	snap aggregate.TopologySnapshot,
	name, service string,
	start, end time.Time,
) ([]views.MetricBucket, aggregate.Coverage) {
	out := make([]views.MetricBucket, 0, 8)
	for _, m := range snap.Metrics {
		if m.Metric != name {
			continue
		}
		if service != "" && m.Service != service {
			continue
		}
		for _, win := range m.Windows {
			// A window the metric was not observed in carries no value
			// statistics; emitting it would report a fabricated zero sample.
			if win.ValueCount == 0 {
				continue
			}
			if win.Start.Before(start) || win.Start.After(end) {
				continue
			}
			out = append(out, views.MetricBucket{
				Name:        m.Metric,
				ServiceName: m.Service,
				TimeBucket:  win.Start,
				Min:         win.ValueMin,
				Max:         win.ValueMax,
				Sum:         win.ValueSum,
				Count:       clampToInt64(win.ValueCount),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].TimeBucket.Equal(out[j].TimeBucket) {
			return out[i].TimeBucket.Before(out[j].TimeBucket)
		}
		return out[i].ServiceName < out[j].ServiceName
	})
	return out, metricSeriesCoverage(snap, start)
}

// metricSeriesCoverage reports whether a projected metric answer is complete.
// It is not when a projection cap refused metric facts, and it is not when the
// caller asked for history older than the projection retains: outside that
// horizon the engine holds nothing, and an empty stretch of chart must not read
// as "the metric was flat".
func metricSeriesCoverage(snap aggregate.TopologySnapshot, start time.Time) aggregate.Coverage {
	if snap.DroppedMetrics > 0 {
		return aggregate.CoverageSampled
	}
	if snap.Horizon > 0 && !snap.Now.IsZero() && start.Before(snap.Now.Add(-snap.Horizon)) {
		return aggregate.CoverageSampled
	}
	return aggregate.CoverageFull
}

// clampToInt64 narrows a projection counter for the wire shape. The counters
// are bounded by accepted data points inside one retained window, so the clamp
// is unreachable in practice and exists so the conversion is total.
func clampToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// handleGetMetricNames handles GET /api/metadata/metrics
func (s *Server) handleGetMetricNames(w http.ResponseWriter, r *http.Request) {
	serviceName := r.URL.Query().Get("service_name")

	// Aggregate mode: metric_buckets is not written, so the DISTINCT scan has
	// nothing to find. The names come from the engine's topology projection.
	if s.aggregateReads() {
		snap := s.aggregateEngine.TopologySnapshot(storage.TenantFromContext(r.Context()))
		// Always sampled, never full: the projection holds a bounded recent
		// horizon while the legacy answer spanned HOT_RETENTION_DAYS. A metric
		// that stopped reporting an hour ago is absent from this list, and the
		// header is what says so.
		setCoverage(w, aggregate.CoverageSampled)
		w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(metricNamesFromTopology(snap, serviceName))
		return
	}

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

// metricNamesFromTopology lists the distinct metric names the projection holds
// for a tenant, optionally narrowed to one service. Sorted ascending and never
// nil, matching the legacy DISTINCT ... ORDER BY name ASC answer.
func metricNamesFromTopology(snap aggregate.TopologySnapshot, service string) []string {
	seen := make(map[string]struct{}, len(snap.Metrics))
	names := make([]string, 0, len(snap.Metrics))
	for _, m := range snap.Metrics {
		if service != "" && m.Service != service {
			continue
		}
		if m.Metric == "" {
			continue
		}
		if _, dup := seen[m.Metric]; dup {
			continue
		}
		seen[m.Metric] = struct{}{}
		names = append(names, m.Metric)
	}
	sort.Strings(names)
	return names
}
