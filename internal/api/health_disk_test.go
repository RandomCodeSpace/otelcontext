package api

import (
	"net/http"
	"testing"
)

// Readiness reflects disk pressure (#201 Q5). At raw-off the process still
// serves reads and still accounts aggregates, but it can no longer retain the
// diagnostics anyone comes here for; an orchestrator should stop aiming fresh
// ingest at it. Errors-only is degraded coverage, not an unready process.
//
// readyChecks is shared with health_aggregate_test.go.
func TestReadyReflectsDiskPressure(t *testing.T) {
	s := newTestServer(t)

	code, checks := readyChecks(t, s)
	if checks["disk"] != "skipped" {
		t.Fatalf("disk check = %q without a watchdog, want skipped", checks["disk"])
	}
	if code != http.StatusOK {
		t.Fatalf("/ready = %d with no watchdog, want 200", code)
	}

	state, healthy := "none", true
	s.SetDiskPressureProbe(func() (string, bool) { return state, healthy })

	for _, tc := range []struct {
		state   string
		healthy bool
		want    int
	}{
		{"none", true, http.StatusOK},
		{"errors_only", true, http.StatusOK},
		{"raw_off", false, http.StatusServiceUnavailable},
	} {
		state, healthy = tc.state, tc.healthy
		code, checks = readyChecks(t, s)
		if code != tc.want {
			t.Errorf("/ready = %d at disk state %q, want %d", code, tc.state, tc.want)
		}
		if checks["disk"] != tc.state {
			t.Errorf("disk check = %q, want %q", checks["disk"], tc.state)
		}
	}
}
