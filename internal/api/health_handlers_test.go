package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/ingest"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func TestReadyPipelineByteSaturation(t *testing.T) {
	p := ingest.NewPipeline(nil, nil, ingest.PipelineConfig{Capacity: 100, Workers: 1, MaxBytes: 1 << 20})
	s := newTestServer(t)
	s.SetPipelineSaturationProbe(func() float64 { return p.Stats().Saturation() })
	batch := func(n int) *ingest.Batch {
		return &ingest.Batch{Type: ingest.SignalLogs, HasError: true, Logs: []storage.Log{{Body: strings.Repeat("x", n)}}}
	}
	if _, err := p.Submit(batch(500 << 10)); err != nil {
		t.Fatal(err)
	}
	if code, checks := readyChecks(t, s); code != http.StatusOK {
		t.Fatalf("ready before byte saturation: code=%d checks=%v", code, checks)
	}
	if _, err := p.Submit(batch(500 << 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Submit(batch(100 << 10)); !errors.Is(err, ingest.ErrQueueFull) {
		t.Fatalf("submit beyond byte cap = %v, want ErrQueueFull", err)
	}
	if code, checks := readyChecks(t, s); code != http.StatusServiceUnavailable || !strings.HasPrefix(checks["pipeline"], "saturated") {
		t.Fatalf("ready with only two batches filling byte budget: code=%d checks=%v", code, checks)
	}
}

// newTestServer constructs a minimal Server for health-endpoint tests.
// GraphRAG is left nil; readiness treats a nil coordinator as "skipped".
func newTestServer(t *testing.T) *Server {
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
	return &Server{repo: repo}
}

func TestLiveAlwaysOK(t *testing.T) {
	s := &Server{} // no deps needed
	req := httptest.NewRequest(http.MethodGet, "/live", nil)
	rr := httptest.NewRecorder()

	s.handleLive(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "alive" {
		t.Fatalf("expected status=alive, got %q", body["status"])
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}
}

func TestReadyWithHealthyDB(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()
	s.handleReady(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Ready {
		t.Fatalf("expected ready=true, body=%s", rr.Body.String())
	}
	if body.Checks["database"] != "ok" {
		t.Fatalf("expected database=ok, got %q", body.Checks["database"])
	}
	// GraphRAG is nil in this test harness, so it is reported as skipped.
	if got := body.Checks["graphrag"]; got != "ok" && got != "skipped" {
		t.Fatalf("unexpected graphrag check: %q", got)
	}
}

func TestReadyWith_ClosedDB_Returns503(t *testing.T) {
	s := newTestServer(t)

	// Close the underlying sql.DB so Ping fails.
	sqlDB, err := s.repo.DB().DB()
	if err != nil {
		t.Fatalf("unwrap sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()
	s.handleReady(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Ready {
		t.Fatalf("expected ready=false")
	}
	if body.Checks["database"] == "ok" {
		t.Fatalf("expected database check to fail, got %q", body.Checks["database"])
	}
	// graphrag should still be present so operators can diagnose.
	if _, ok := body.Checks["graphrag"]; !ok {
		t.Fatalf("expected graphrag entry present")
	}
}
