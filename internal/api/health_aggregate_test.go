package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// readyChecks runs /ready and returns its status code and per-check breakdown.
func readyChecks(t *testing.T, s *Server) (int, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()
	s.handleReady(rr, req)
	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /ready body: %v", err)
	}
	return rr.Code, body.Checks
}

// TestReadyHeldDuringAggregateRecovery covers the #173 requirement that a
// process whose aggregate shards are only half-replayed must not be routed to.
func TestReadyHeldDuringAggregateRecovery(t *testing.T) {
	s := newTestServer(t)
	recovered := false
	s.SetAggregateRecoveryProbe(func() bool { return recovered })

	code, checks := readyChecks(t, s)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d during recovery, want 503", code)
	}
	if checks["aggregate_store"] != "recovering" {
		t.Fatalf("aggregate_store check = %q, want recovering", checks["aggregate_store"])
	}

	recovered = true
	code, checks = readyChecks(t, s)
	if code != http.StatusOK {
		t.Fatalf("/ready = %d after recovery, want 200", code)
	}
	if checks["aggregate_store"] != "ok" {
		t.Fatalf("aggregate_store check = %q, want ok", checks["aggregate_store"])
	}
}

// TestReadySkipsAggregateCheckWhenUnconfigured keeps AGGREGATE_MODE=legacy
// deployments unaffected.
func TestReadySkipsAggregateCheckWhenUnconfigured(t *testing.T) {
	s := newTestServer(t)
	code, checks := readyChecks(t, s)
	if code != http.StatusOK {
		t.Fatalf("/ready = %d without an aggregate store, want 200", code)
	}
	if checks["aggregate_store"] != "skipped" {
		t.Fatalf("aggregate_store check = %q, want skipped", checks["aggregate_store"])
	}
}
