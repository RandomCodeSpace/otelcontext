package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// newAPITestRepoWithFTS builds a fresh in-memory SQLite Repository with the
// FTS5 schema provisioned. Caller must have already set LOG_FTS_ENABLED=true
// before calling — otherwise the migrate path skips the FTS5 setup.
func newAPITestRepoWithFTS(t *testing.T) *storage.Repository {
	t.Helper()
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

// TestHandleDropFTS_Reclaims verifies that the admin endpoint drops the
// FTS5 virtual table + triggers and returns reclaimed_bytes >= 0.
//
// Test starts with LOG_FTS_ENABLED=true so the in-memory repo provisions
// the FTS5 schema, then flips the flag off so handleDropFTS will accept
// the call (it refuses while the flag is still on).
func TestHandleDropFTS_Reclaims(t *testing.T) {
	t.Setenv("LOG_FTS_ENABLED", "true")
	repo := newAPITestRepoWithFTS(t)
	// Seed a few rows so the FTS5 index has content (and reclaimable pages).
	for i := range 50 {
		_ = repo.DB().Create(&storage.Log{
			TenantID:    "default",
			Severity:    "ERROR",
			Body:        strings.Repeat("payload-", i+1),
			ServiceName: "svc",
		})
	}
	// Switch off so the handler accepts the drop request.
	t.Setenv("LOG_FTS_ENABLED", "false")

	srv := &Server{repo: repo}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/admin/drop_fts", srv.handleDropFTS)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/drop_fts", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%q", err, rec.Body.String())
	}
	if _, ok := body["reclaimed_bytes"]; !ok {
		t.Fatalf("response missing reclaimed_bytes: %v", body)
	}
	// Verify the FTS5 table is gone — querying it must error.
	if err := repo.DB().Exec("SELECT 1 FROM logs_fts LIMIT 1").Error; err == nil {
		t.Fatal("logs_fts should be dropped but still queryable")
	}
}

// TestHandleDropFTS_RefusesWhenEnabled verifies the safety gate: the
// endpoint refuses (405) if LOG_FTS_ENABLED is currently truthy, because
// dropping the triggers mid-operation would silently break FTS5 sync.
func TestHandleDropFTS_RefusesWhenEnabled(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *string
	}{
		{name: "unset defaults on"},
		{name: "explicit true", value: stringPtrAPI("true")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOG_FTS_ENABLED", "")
			if tc.value == nil {
				if err := os.Unsetenv("LOG_FTS_ENABLED"); err != nil {
					t.Fatalf("unset LOG_FTS_ENABLED: %v", err)
				}
			} else {
				t.Setenv("LOG_FTS_ENABLED", *tc.value)
			}
			repo := newAPITestRepoWithFTS(t)
			srv := &Server{repo: repo}
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/admin/drop_fts", srv.handleDropFTS)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/admin/drop_fts", nil)
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("want 405 with FTS enabled, got %d body=%q", rec.Code, rec.Body.String())
			}
			if err := repo.DB().Exec("SELECT 1 FROM logs_fts LIMIT 1").Error; err != nil {
				t.Fatalf("logs_fts should remain queryable when refused: %v", err)
			}
		})
	}
}

func stringPtrAPI(v string) *string { return &v }
