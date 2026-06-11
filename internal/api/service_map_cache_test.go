package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/cache"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// newServiceMapTestServer builds a Server with a real SQLite-backed repository
// and a live TTL cache — the two dependencies handleGetServiceMapMetrics uses.
func newServiceMapTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	c := cache.New()
	t.Cleanup(func() {
		c.Stop()
		_ = repo.Close()
	})
	return &Server{repo: repo, cache: c}
}

// seedServiceMapSpans inserts a parent/child span pair for the given tenant so
// the service-map endpoint has one edge and two nodes to report.
func seedServiceMapSpans(t *testing.T, s *Server, tenant, suffix string) {
	t.Helper()
	now := time.Now().UTC()
	spans := []storage.Span{
		{
			TenantID: tenant, TraceID: "trace-" + suffix, SpanID: "parent-" + suffix,
			OperationName: "op", StartTime: now, EndTime: now.Add(time.Millisecond),
			Duration: 1000, ServiceName: "svc-front-" + suffix, Status: "STATUS_CODE_UNSET",
		},
		{
			TenantID: tenant, TraceID: "trace-" + suffix, SpanID: "child-" + suffix,
			ParentSpanID: "parent-" + suffix, OperationName: "op", StartTime: now,
			EndTime: now.Add(time.Millisecond), Duration: 2000,
			ServiceName: "svc-back-" + suffix, Status: "STATUS_CODE_ERROR",
		},
	}
	if err := s.repo.BatchCreateSpans(spans); err != nil {
		t.Fatalf("seed spans: %v", err)
	}
}

func getServiceMap(t *testing.T, s *Server, ctx context.Context, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	s.handleGetServiceMapMetrics(rr, req)
	return rr
}

// TestServiceMapHandler_CacheMissThenHit verifies the 30s result cache: the
// first request computes and stores, the second is served from cache even if
// new spans landed in between.
func TestServiceMapHandler_CacheMissThenHit(t *testing.T) {
	s := newServiceMapTestServer(t)
	seedServiceMapSpans(t, s, "default", "a")
	ctx := context.Background()

	rr1 := getServiceMap(t, s, ctx, "/api/metrics/service-map")
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request: %d body=%s", rr1.Code, rr1.Body.String())
	}
	if got := rr1.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("first request X-Cache = %q, want MISS", got)
	}

	// New data must NOT appear within the TTL — proves the second request
	// never reached the repository.
	seedServiceMapSpans(t, s, "default", "b")

	rr2 := getServiceMap(t, s, ctx, "/api/metrics/service-map")
	if rr2.Code != http.StatusOK {
		t.Fatalf("second request: %d", rr2.Code)
	}
	if got := rr2.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("second request X-Cache = %q, want HIT", got)
	}
	if rr1.Body.String() != rr2.Body.String() {
		t.Fatalf("cached body diverged:\n first=%s\nsecond=%s", rr1.Body.String(), rr2.Body.String())
	}
}

// TestServiceMapHandler_CacheKeyPerTenant verifies two tenants never share a
// cache entry.
func TestServiceMapHandler_CacheKeyPerTenant(t *testing.T) {
	s := newServiceMapTestServer(t)
	seedServiceMapSpans(t, s, "default", "a")
	seedServiceMapSpans(t, s, "tenant-b", "b")

	rr1 := getServiceMap(t, s, context.Background(), "/api/metrics/service-map")
	if got := rr1.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("default tenant X-Cache = %q, want MISS", got)
	}

	ctxB := storage.WithTenantContext(context.Background(), "tenant-b")
	rr2 := getServiceMap(t, s, ctxB, "/api/metrics/service-map")
	if got := rr2.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("tenant-b first request X-Cache = %q, want MISS (cache leaked across tenants)", got)
	}
	if rr1.Body.String() == rr2.Body.String() {
		t.Fatalf("tenant responses identical — tenant scoping broken: %s", rr1.Body.String())
	}

	rr3 := getServiceMap(t, s, ctxB, "/api/metrics/service-map")
	if got := rr3.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("tenant-b second request X-Cache = %q, want HIT", got)
	}
}

// TestServiceMapHandler_CacheKeyPerWindow verifies explicit start/end windows
// are cached independently of the default rolling window and of each other.
func TestServiceMapHandler_CacheKeyPerWindow(t *testing.T) {
	s := newServiceMapTestServer(t)
	seedServiceMapSpans(t, s, "default", "a")
	ctx := context.Background()

	// Prime the default-window entry.
	if got := getServiceMap(t, s, ctx, "/api/metrics/service-map").Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("default window X-Cache = %q, want MISS", got)
	}

	win := "/api/metrics/service-map?start=2026-06-01T00:00:00Z&end=2026-06-01T01:00:00Z"
	if got := getServiceMap(t, s, ctx, win).Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("explicit window first request X-Cache = %q, want MISS", got)
	}
	if got := getServiceMap(t, s, ctx, win).Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("explicit window second request X-Cache = %q, want HIT", got)
	}

	other := "/api/metrics/service-map?start=2026-06-01T00:00:00Z&end=2026-06-01T02:00:00Z"
	if got := getServiceMap(t, s, ctx, other).Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("different window X-Cache = %q, want MISS", got)
	}
}

// TestServiceMapHandler_DBError500 verifies repository failures surface as 500
// and are never cached.
func TestServiceMapHandler_DBError500(t *testing.T) {
	s := newServiceMapTestServer(t)
	sqlDB, err := s.repo.DB().DB()
	if err != nil {
		t.Fatalf("unwrap sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rr := getServiceMap(t, s, context.Background(), "/api/metrics/service-map")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rr.Code, rr.Body.String())
	}
}
