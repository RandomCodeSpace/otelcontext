package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadyFailsImmediatelyOnceShutdownBegins(t *testing.T) {
	s := NewServer(nil, nil, nil, nil)
	s.BeginShutdown()
	rr := httptest.NewRecorder()
	s.handleReady(rr, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d, want 503", rr.Code)
	}
	if body := rr.Body.String(); body == "" || !containsAll(body, `"ready":false`, `"shutdown":"in_progress"`) {
		t.Fatalf("unexpected readiness body: %s", body)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
