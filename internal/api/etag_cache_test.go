package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/cache"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// newCachedTestServer builds a Server backed by a fresh in-memory SQLite
// repo with the TTL cache wired — the shape the cached hot-poll handlers
// (system graph, dashboard stats, DB stats) need.
func newCachedTestServer(t *testing.T) *Server {
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

// pollCacheFlow drives the shared MISS → HIT → 304 assertion sequence for a
// cached+ETagged endpoint.
func pollCacheFlow(t *testing.T, handler http.HandlerFunc, path string) {
	t.Helper()

	do := func(inm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	first := do("")
	if first.Code != http.StatusOK {
		t.Fatalf("first: want 200, got %d body=%q", first.Code, first.Body.String())
	}
	if got := first.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("first: X-Cache = %q, want MISS", got)
	}
	etag := first.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("first: ETag = %q, want quoted non-empty", etag)
	}
	if ct := first.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("first: Content-Type = %q", ct)
	}

	second := do("")
	if got := second.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("second: X-Cache = %q, want HIT (within 10s TTL)", got)
	}
	if got := second.Header().Get("ETag"); got != etag {
		t.Errorf("second: ETag = %q, want %q", got, etag)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("second: cached body differs from first")
	}

	third := do(etag)
	if third.Code != http.StatusNotModified {
		t.Fatalf("third: want 304 with If-None-Match, got %d", third.Code)
	}
	if third.Body.Len() != 0 {
		t.Errorf("third: 304 body should be empty, got %q", third.Body.String())
	}
	if got := third.Header().Get("ETag"); got != etag {
		t.Errorf("third: 304 ETag = %q, want %q", got, etag)
	}
}

func TestHandleGetStats_CacheAndETag(t *testing.T) {
	s := newCachedTestServer(t)
	pollCacheFlow(t, s.handleGetStats, "/api/stats")
}

func TestHandleGetDashboardStats_CacheAndETag(t *testing.T) {
	s := newCachedTestServer(t)
	pollCacheFlow(t, s.handleGetDashboardStats, "/api/metrics/dashboard")
}

func TestHandleGetSystemGraph_CacheAndETag(t *testing.T) {
	// graph and graphRAG are nil → the handler exercises the DB fallback
	// path against the empty repo, which still yields a valid response.
	s := newCachedTestServer(t)
	pollCacheFlow(t, s.handleGetSystemGraph, "/api/system/graph")
}

// TestHandleGetDashboardStats_QueryScopedKey proves distinct query strings
// never share a cache entry.
func TestHandleGetDashboardStats_QueryScopedKey(t *testing.T) {
	s := newCachedTestServer(t)

	do := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.handleGetDashboardStats(rec, req)
		return rec
	}

	if got := do("/api/metrics/dashboard?service_name=a").Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("first ?service_name=a: X-Cache = %q, want MISS", got)
	}
	if got := do("/api/metrics/dashboard?service_name=b").Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("first ?service_name=b: X-Cache = %q, want MISS (different query must not share entry)", got)
	}
	if got := do("/api/metrics/dashboard?service_name=a").Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("second ?service_name=a: X-Cache = %q, want HIT", got)
	}
}

// TestHandleGetDashboardStats_LongQueryBypassesCache guards the cache-key
// cardinality bound: pathological query strings skip the cache entirely
// instead of growing the map.
func TestHandleGetDashboardStats_LongQueryBypassesCache(t *testing.T) {
	s := newCachedTestServer(t)
	path := "/api/metrics/dashboard?service_name=" + strings.Repeat("x", 300)

	for i := range 2 {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.handleGetDashboardStats(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d", i, rec.Code)
		}
		if got := rec.Header().Get("X-Cache"); got == "HIT" {
			t.Errorf("request %d: oversized query must bypass the cache, got X-Cache HIT", i)
		}
	}
}

// TestHandleGetDashboardStats_ExplicitTimeRange exercises the start/end
// query parsing alongside the cache: a parameterised window is served and
// cached under its own key.
func TestHandleGetDashboardStats_ExplicitTimeRange(t *testing.T) {
	s := newCachedTestServer(t)
	path := "/api/metrics/dashboard?start=2026-06-11T00:00:00Z&end=2026-06-11T01:00:00Z"

	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.handleGetDashboardStats(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("first: X-Cache = %q, want MISS", got)
	}

	rec2 := httptest.NewRecorder()
	s.handleGetDashboardStats(rec2, httptest.NewRequest(http.MethodGet, path, nil))
	if got := rec2.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("second: X-Cache = %q, want HIT", got)
	}
}

// Repo-error paths: a closed DB makes every repository call fail, which
// must surface as a 500 (graph handler: its DB fallback returns nil).
func TestCachedHandlers_DBErrorPaths(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		handler func(s *Server) http.HandlerFunc
	}{
		{"stats", "/api/stats", func(s *Server) http.HandlerFunc { return s.handleGetStats }},
		{"dashboard", "/api/metrics/dashboard", func(s *Server) http.HandlerFunc { return s.handleGetDashboardStats }},
		{"system graph", "/api/system/graph", func(s *Server) http.HandlerFunc { return s.handleGetSystemGraph }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newCachedTestServer(t)
			_ = s.repo.Close() // force every query to error

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			tt.handler(s)(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("want 500 on closed DB, got %d", rec.Code)
			}
		})
	}
}

func TestNewCachedJSON_MarshalError(t *testing.T) {
	if _, err := newCachedJSON(make(chan int)); err == nil {
		t.Fatal("want error for unmarshalable value")
	}
}

// TestHandleGetStats_TenantScopedKey proves two tenants never share a
// cached /api/stats payload.
func TestHandleGetStats_TenantScopedKey(t *testing.T) {
	s := newCachedTestServer(t)

	do := func(tenant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		req = req.WithContext(storage.WithTenantContext(req.Context(), tenant))
		rec := httptest.NewRecorder()
		s.handleGetStats(rec, req)
		return rec
	}

	if got := do("tenant-a").Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("tenant-a first: X-Cache = %q, want MISS", got)
	}
	if got := do("tenant-b").Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("tenant-b first: X-Cache = %q, want MISS (tenant isolation)", got)
	}
	if got := do("tenant-a").Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("tenant-a second: X-Cache = %q, want HIT", got)
	}
}
