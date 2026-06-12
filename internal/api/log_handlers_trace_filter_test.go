package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// TestHandleGetLogs_TraceIDFilter verifies that GET /api/logs honors the
// trace_id query param (exact match via storage.LogFilter.TraceID). The UI
// traces→logs cross-link depends on this being deterministic on every
// driver — body `search` only matches trace IDs on the LIKE fallback, not
// under FTS5.
func TestHandleGetLogs_TraceIDFilter(t *testing.T) {
	repo := newAPITestRepoWithoutFTS(t)
	now := time.Now().UTC()
	logs := []storage.Log{
		{TenantID: storage.DefaultTenantID, TraceID: "aaaa1111", Severity: "ERROR", Body: "boom", ServiceName: "svc-a", Timestamp: now},
		{TenantID: storage.DefaultTenantID, TraceID: "bbbb2222", Severity: "INFO", Body: "fine", ServiceName: "svc-b", Timestamp: now},
	}
	if err := repo.BatchCreateLogs(logs); err != nil {
		t.Fatalf("BatchCreateLogs: %v", err)
	}

	srv := &Server{repo: repo}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/logs", srv.handleGetLogs)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs?trace_id=aaaa1111", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data  []struct{ TraceID string `json:"trace_id"` } `json:"data"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total != 1 || len(resp.Data) != 1 {
		t.Fatalf("want exactly 1 row for trace_id filter, got total=%d len=%d", resp.Total, len(resp.Data))
	}
	if resp.Data[0].TraceID != "aaaa1111" {
		t.Fatalf("want trace_id aaaa1111, got %q", resp.Data[0].TraceID)
	}
}

// TestHandleGetLogs_TraceIDFilterSkipsSearchCap verifies that a trace_id
// filter alone (no search term) is NOT subject to the 24h keyword-search
// clamp — trace IDs are indexed exact matches, not LIKE scans.
func TestHandleGetLogs_TraceIDFilterSkipsSearchCap(t *testing.T) {
	repo := newAPITestRepoWithoutFTS(t)
	old := time.Now().UTC().Add(-5 * 24 * time.Hour)
	if err := repo.BatchCreateLogs([]storage.Log{
		{TenantID: storage.DefaultTenantID, TraceID: "cccc3333", Severity: "WARN", Body: "stale", ServiceName: "svc-c", Timestamp: old},
	}); err != nil {
		t.Fatalf("BatchCreateLogs: %v", err)
	}

	srv := &Server{repo: repo}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/logs", srv.handleGetLogs)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs?trace_id=cccc3333", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("5-day-old log must be reachable via trace_id, got total=%d", resp.Total)
	}
}
