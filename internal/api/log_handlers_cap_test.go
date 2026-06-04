package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// TestHandleGetLogs_SearchCapRejectsOlderThan24h verifies that an explicit
// search query with a window entirely outside the 24h cap returns 400.
// Symmetric with the MCP search_logs cap so a direct HTTP caller cannot
// bypass via the alternate transport.
func TestHandleGetLogs_SearchCapRejectsOlderThan24h(t *testing.T) {
	repo := newAPITestRepoWithoutFTS(t)
	srv := &Server{repo: repo}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/logs", srv.handleGetLogs)

	q := url.Values{}
	q.Set("search", "panic")
	q.Set("start", time.Now().Add(-5*24*time.Hour).Format(time.RFC3339))
	q.Set("end", time.Now().Add(-4*24*time.Hour).Format(time.RFC3339))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs?"+q.Encode(), nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for out-of-cap window, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// TestHandleGetLogs_NoSearchSkipsCap verifies that a filtered listing with
// no search term keeps the full retention range — the cap fires only on
// keyword queries, where unbounded LIKE scans are the worst case.
func TestHandleGetLogs_NoSearchSkipsCap(t *testing.T) {
	repo := newAPITestRepoWithoutFTS(t)
	srv := &Server{repo: repo}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/logs", srv.handleGetLogs)

	q := url.Values{}
	// No search param — listing-only request with a 5-day-old window.
	q.Set("start", time.Now().Add(-5*24*time.Hour).Format(time.RFC3339))
	q.Set("end", time.Now().Add(-4*24*time.Hour).Format(time.RFC3339))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs?"+q.Encode(), nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("filtered listing without search must succeed, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// TestParsePaging_Clamp verifies that parsePaging enforces bounds on limit and
// offset, preventing GORM from receiving a negative Limit (treated as unlimited)
// or a negative offset.
func TestParsePaging_Clamp(t *testing.T) {
	cases := []struct {
		query        string
		defaultLimit int
		wantLimit    int
		wantOffset   int
	}{
		// Over-limit capped at 1000.
		{"limit=9999&offset=0", 50, 1000, 0},
		// Negative limit floored at 1.
		{"limit=-5&offset=0", 50, 1, 0},
		// Negative offset floored at 0.
		{"limit=10&offset=-99", 50, 10, 0},
		// Both negative.
		{"limit=-1&offset=-1", 50, 1, 0},
		// Default limit also clamped.
		{"", 9999, 1000, 0},
		// Valid values pass through unchanged.
		{"limit=100&offset=200", 50, 100, 200},
		// Exact cap boundary.
		{"limit=1000&offset=0", 50, 1000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/logs?"+tc.query, nil)
			gotLimit, gotOffset := parsePaging(req, tc.defaultLimit)
			if gotLimit != tc.wantLimit {
				t.Errorf("limit: got %d, want %d", gotLimit, tc.wantLimit)
			}
			if gotOffset != tc.wantOffset {
				t.Errorf("offset: got %d, want %d", gotOffset, tc.wantOffset)
			}
		})
	}
}

// newAPITestRepoWithoutFTS builds a fresh in-memory repo with FTS5 disabled.
// Used by cap tests since they only care about handler behavior, not the
// search backend.
func newAPITestRepoWithoutFTS(t *testing.T) *storage.Repository {
	t.Helper()
	t.Setenv("LOG_FTS_ENABLED", "false")
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}
