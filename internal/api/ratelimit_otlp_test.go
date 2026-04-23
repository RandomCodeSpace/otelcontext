package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
