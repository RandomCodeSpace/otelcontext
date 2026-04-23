package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRateLimiter_ExemptsOTLPPaths pins the /v1/* exemption contract.
//
// OTLP collectors (Otel SDK, Collector, Alloy, vector, etc.) batch aggressively
// and a single agent routinely exceeds the 100 RPS/IP default that protects
// /api/*. Without this exemption, legitimate ingestion traffic — the exact data
// this platform exists to capture — would get 429'd and dropped. This test
// locks in the carve-out so a future refactor doesn't silently re-enable
// throttling on /v1/* and regress ingestion.
//
// It also asserts the inverse — that /api/* on the *same* IP is still
// throttled — so the exemption remains narrow (path-prefix, not blanket).
func TestRateLimiter_ExemptsOTLPPaths(t *testing.T) {
	rl := NewRateLimiter(1) // 1 RPS per IP — draconian
	handler := rl.MiddlewareExcept(func(path string) bool {
		return strings.HasPrefix(path, "/v1/")
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 10 rapid OTLP requests should all 200.
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("POST", "/v1/traces", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("OTLP request %d got %d, expected 200 (exempt path)", i, w.Code)
		}
	}

	// But /api/* on the same IP should get throttled after the 1st.
	hits := 0
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest("GET", "/api/logs", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code == 200 {
			hits++
		}
	}
	if hits >= 5 {
		t.Fatalf("expected /api throttling, got %d/5 passes", hits)
	}
}
