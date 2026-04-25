package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"
)

// TestSimilarHandler_TenantIsolation is the RAN-20 acceptance bar for the HTTP
// surface. Two tenants with distinct corpora query /api/logs/similar; each
// sees ZERO rows belonging to the other tenant.
func TestSimilarHandler_TenantIsolation(t *testing.T) {
	idx := vectordb.New(1_000)
	idx.Add(101, "acme", "checkout", "ERROR", "payment gateway timeout charging customer")
	idx.Add(102, "acme", "checkout", "ERROR", "payment gateway refused charge insufficient funds")
	idx.Add(201, "globex", "auth", "ERROR", "payment gateway token expired for session")
	idx.Add(202, "globex", "auth", "ERROR", "payment gateway 500 internal error while authenticating")

	srv := &Server{vectorIdx: idx}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/logs/similar", srv.handleGetSimilarLogs)
	handler := TenantMiddleware(&config.Config{DefaultTenant: "default"})(mux)

	acmeIDs := map[float64]bool{101: true, 102: true}
	globexIDs := map[float64]bool{201: true, 202: true}

	q := url.Values{}
	q.Set("q", "payment gateway")
	q.Set("limit", "50")
	path := "/api/logs/similar?" + q.Encode()

	// Tenant A
	aRec := httptest.NewRecorder()
	aReq := httptest.NewRequest(http.MethodGet, path, nil)
	aReq.Header.Set(TenantHeader, "acme")
	handler.ServeHTTP(aRec, aReq)
	if aRec.Code != http.StatusOK {
		t.Fatalf("acme: want 200, got %d body=%q", aRec.Code, aRec.Body.String())
	}
	acme := decodeResults(t, aRec)
	if len(acme) == 0 {
		t.Fatalf("acme got zero hits despite matching corpus")
	}
	for _, r := range acme {
		if !acmeIDs[r.ID] {
			t.Fatalf("acme leaked cross-tenant id=%v tenant=%q body=%q", r.ID, r.Tenant, r.Body)
		}
	}

	// Tenant B
	gRec := httptest.NewRecorder()
	gReq := httptest.NewRequest(http.MethodGet, path, nil)
	gReq.Header.Set(TenantHeader, "globex")
	handler.ServeHTTP(gRec, gReq)
	if gRec.Code != http.StatusOK {
		t.Fatalf("globex: want 200, got %d", gRec.Code)
	}
	globex := decodeResults(t, gRec)
	if len(globex) == 0 {
		t.Fatalf("globex got zero hits despite matching corpus")
	}
	for _, r := range globex {
		if !globexIDs[r.ID] {
			t.Fatalf("globex leaked cross-tenant id=%v tenant=%q body=%q", r.ID, r.Tenant, r.Body)
		}
	}
}

// TestSimilarHandler_UnknownTenantReturnsEmpty confirms a request bearing an
// unknown tenant header returns zero results — the handler must not silently
// fall back to another tenant's rows.
func TestSimilarHandler_UnknownTenantReturnsEmpty(t *testing.T) {
	idx := vectordb.New(100)
	idx.Add(1, "acme", "svc", "ERROR", "database connection refused upstream")

	srv := &Server{vectorIdx: idx}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/logs/similar", srv.handleGetSimilarLogs)
	handler := TenantMiddleware(&config.Config{DefaultTenant: "default"})(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs/similar?q=database+connection", nil)
	req.Header.Set(TenantHeader, "initech")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if r := decodeResults(t, rec); len(r) != 0 {
		t.Fatalf("unknown tenant saw %d cross-tenant hits", len(r))
	}
}

type similarResult struct {
	ID          float64 `json:"LogID"`
	Tenant      string  `json:"Tenant"`
	ServiceName string  `json:"ServiceName"`
	Severity    string  `json:"Severity"`
	Body        string  `json:"Body"`
	Score       float64 `json:"Score"`
}

func decodeResults(t *testing.T, rec *httptest.ResponseRecorder) []similarResult {
	t.Helper()
	var env struct {
		Results []similarResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, rec.Body.String())
	}
	return env.Results
}
