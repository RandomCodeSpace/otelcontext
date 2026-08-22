package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/api/views"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// #194 finding 10: AGGREGATE_MODE=aggregate does not start the legacy TSDB, so
// metric_buckets is never written. GET /api/metrics and GET /api/metadata/metrics
// must therefore answer from the aggregate engine's topology projection — with
// the same wire shape and an honest coverage header — instead of scanning a
// table nothing fills.

// seedAggregateMetric folds gauge samples for one (service, metric) through a
// reducer. It must go through ApplyReducer rather than ApplyCommitted: only the
// reducer path carries the string identities the topology projection is keyed
// by, and the projection is what the metric endpoints read.
func seedAggregateMetric(t *testing.T, e *aggregate.Engine, tenant, service, name string, values ...float64) {
	t.Helper()
	r := e.NewReducer(time.Now())
	for _, v := range values {
		r.ReduceMetricPoint(aggregate.MetricInput{
			Tenant:      tenant,
			Service:     service,
			Name:        name,
			Value:       v,
			Timestamp:   time.Now(),
			Temporality: aggregate.TemporalityUnspecified,
		})
	}
	e.ApplyReducer(r)
}

// getArray issues a tenant-scoped GET and decodes the bare JSON array the
// metric endpoints return. getJSON cannot be reused: it decodes into an object.
func getArray[T any](t *testing.T, h http.HandlerFunc, target, tenant string) (*httptest.ResponseRecorder, []T) {
	t.Helper()
	ctx := storage.WithTenantContext(context.Background(), tenant)
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rr.Code, rr.Body.String())
	}
	var out []T
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v (%s)", target, err, rr.Body.String())
	}
	return rr, out
}

// metricWindowRange is a request window wide enough to contain the engine's
// current five-minute window and still sit inside the projection horizon, so
// the answer is expected to be full coverage.
func metricWindowRange() (string, string) {
	now := time.Now()
	return now.Add(-10 * time.Minute).UTC().Format(time.RFC3339),
		now.Add(10 * time.Minute).UTC().Format(time.RFC3339)
}

func TestMetricBucketsServedFromAggregateEngine(t *testing.T) {
	const (
		tenant = "default"
		metric = "queue.depth"
	)
	s, counter, engine := aggregateTestServer(t, true)
	seedAggregateMetric(t, engine, tenant, "checkout", metric, 4, 9, 2)
	seedAggregateMetric(t, engine, tenant, "orders", "other.metric", 1)

	start, end := metricWindowRange()
	rr, buckets := getArray[views.MetricBucket](t, s.handleGetMetricBuckets,
		"/api/metrics?name="+metric+"&start="+start+"&end="+end, tenant)

	if len(buckets) != 1 {
		t.Fatalf("want one projected window for %s, got %d: %+v", metric, len(buckets), buckets)
	}
	got := buckets[0]
	if got.Name != metric || got.ServiceName != "checkout" {
		t.Errorf("bucket identity = %q/%q, want %q/checkout", got.Name, got.ServiceName, metric)
	}
	if got.Count != 3 || got.Sum != 15 || got.Min != 2 || got.Max != 9 {
		t.Errorf("bucket stats = count %d sum %v min %v max %v, want 3/15/2/9",
			got.Count, got.Sum, got.Min, got.Max)
	}
	if got.TimeBucket.IsZero() {
		t.Error("bucket carries no time_bucket")
	}
	if cov := rr.Header().Get(aggregate.CoverageHeader); cov != string(aggregate.CoverageFull) {
		t.Errorf("coverage header = %q, want %q", cov, aggregate.CoverageFull)
	}
	if len(counter.hits) != 0 {
		t.Errorf("aggregate-mode metric read touched raw tables: %v", counter.hits)
	}
}

// A request reaching back past the projection horizon must not be presented as
// complete: outside the horizon the engine holds nothing, and a flat stretch of
// chart is not evidence the metric was flat.
func TestMetricBucketsBeyondHorizonReportSampled(t *testing.T) {
	const tenant = "default"
	s, _, engine := aggregateTestServer(t, true)
	seedAggregateMetric(t, engine, tenant, "checkout", "queue.depth", 1)

	start := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	rr, _ := getArray[views.MetricBucket](t, s.handleGetMetricBuckets,
		"/api/metrics?name=queue.depth&start="+start+"&end="+end, tenant)

	if cov := rr.Header().Get(aggregate.CoverageHeader); cov != string(aggregate.CoverageSampled) {
		t.Errorf("coverage header = %q, want %q", cov, aggregate.CoverageSampled)
	}
}

func TestMetricNamesServedFromAggregateEngine(t *testing.T) {
	const tenant = "default"
	s, counter, engine := aggregateTestServer(t, true)
	seedAggregateMetric(t, engine, tenant, "checkout", "queue.depth", 1)
	seedAggregateMetric(t, engine, tenant, "checkout", "cache.hits", 1)
	seedAggregateMetric(t, engine, tenant, "orders", "orders.pending", 1)

	rr, names := getArray[string](t, s.handleGetMetricNames, "/api/metadata/metrics", tenant)
	if len(names) != 3 || names[0] != "cache.hits" || names[1] != "orders.pending" || names[2] != "queue.depth" {
		t.Errorf("names = %v, want sorted [cache.hits orders.pending queue.depth]", names)
	}
	// Never "full": the projection retains a bounded recent horizon while the
	// legacy answer spanned HOT_RETENTION_DAYS.
	if cov := rr.Header().Get(aggregate.CoverageHeader); cov != string(aggregate.CoverageSampled) {
		t.Errorf("coverage header = %q, want %q", cov, aggregate.CoverageSampled)
	}

	_, scoped := getArray[string](t, s.handleGetMetricNames, "/api/metadata/metrics?service_name=orders", tenant)
	if len(scoped) != 1 || scoped[0] != "orders.pending" {
		t.Errorf("service-scoped names = %v, want [orders.pending]", scoped)
	}
	if len(counter.hits) != 0 {
		t.Errorf("aggregate-mode metric-name read touched raw tables: %v", counter.hits)
	}
}

// Legacy mode is untouched: both endpoints still read metric_buckets and neither
// stamps a coverage header.
func TestMetricEndpointsUnchangedInLegacyMode(t *testing.T) {
	const tenant = "default"
	s, _, _ := aggregateTestServer(t, false)
	now := time.Now().UTC()
	if err := s.repo.BatchCreateMetrics([]storage.MetricBucket{{
		TenantID: tenant, Name: "queue.depth", ServiceName: "checkout",
		TimeBucket: now, Min: 2, Max: 9, Sum: 15, Count: 3,
	}}); err != nil {
		t.Fatalf("seed metric bucket: %v", err)
	}

	start := now.Add(-10 * time.Minute).Format(time.RFC3339)
	end := now.Add(10 * time.Minute).Format(time.RFC3339)
	rr, buckets := getArray[views.MetricBucket](t, s.handleGetMetricBuckets,
		"/api/metrics?name=queue.depth&start="+start+"&end="+end, tenant)
	if len(buckets) != 1 || buckets[0].Count != 3 || buckets[0].Sum != 15 {
		t.Fatalf("legacy buckets = %+v, want the seeded row", buckets)
	}
	if cov := rr.Header().Get(aggregate.CoverageHeader); cov != "" {
		t.Errorf("legacy response stamped a coverage header: %q", cov)
	}

	_, names := getArray[string](t, s.handleGetMetricNames, "/api/metadata/metrics", tenant)
	if len(names) != 1 || names[0] != "queue.depth" {
		t.Errorf("legacy names = %v, want [queue.depth]", names)
	}
}
