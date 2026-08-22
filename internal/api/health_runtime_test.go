package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Aggregate RUNTIME readiness probes (#194 finding 18). Recovery completing is
// a one-time gate; these cover the ways a recovered process later stops being
// able to serve. Every probe here must flip /ready at its threshold, recover
// when the signal does, honour its configured limit, and leave /live alone.

// runtimeCheckNames is the operator contract asserted by #202. Renaming one of
// these is a breaking change to every readiness dashboard and alert built on
// the payload.
var runtimeCheckNames = []string{
	"aggregate_db",
	"aggregate_commit",
	"aggregate_finalizer",
	"aggregate_admission",
	"aggregate_delta_log",
	"aggregate_disk",
}

// healthyRuntime is an aggregate runtime snapshot that passes every default
// threshold: no failures, a quarter-full writer, a draining delta log and an
// aggregate file at half its budget.
func healthyRuntime() AggregateRuntime {
	return AggregateRuntime{
		AdmissionRatio:     0.25,
		DeltaLogAgeSeconds: 300,
		DiskUsedBytes:      768 << 20,
		DiskBudgetBytes:    1536 << 20,
	}
}

func TestReadySkipsAggregateRuntimeWhenUnconfigured(t *testing.T) {
	s := newTestServer(t)
	code, checks := readyChecks(t, s)
	if code != http.StatusOK {
		t.Fatalf("/ready = %d without an aggregate store, want 200", code)
	}
	for _, name := range runtimeCheckNames {
		if checks[name] != "skipped" {
			t.Errorf("%s = %q without an aggregate store, want skipped", name, checks[name])
		}
	}
}

func TestReadyFlipsOnEachAggregateRuntimeProbe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		check   string
		breach  func(*AggregateRuntime)
		wantNum string
	}{
		{"commit failure streak", "aggregate_commit", func(rt *AggregateRuntime) { rt.CommitFailureStreak = 3 }, "3"},
		{"finalize failure streak", "aggregate_finalizer", func(rt *AggregateRuntime) { rt.FinalizeFailureStreak = 4 }, "4"},
		{"admission saturation", "aggregate_admission", func(rt *AggregateRuntime) { rt.AdmissionRatio = 0.91 }, "0.91"},
		{"delta log age", "aggregate_delta_log", func(rt *AggregateRuntime) { rt.DeltaLogAgeSeconds = 1801 }, "1801"},
		{"aggregate disk", "aggregate_disk", func(rt *AggregateRuntime) { rt.DiskUsedBytes = 1400 << 20 }, "0.91"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			rt := healthyRuntime()
			s.SetAggregateRuntimeProbe(func() AggregateRuntime { return rt })

			code, checks := readyChecks(t, s)
			if code != http.StatusOK {
				t.Fatalf("/ready = %d while healthy, want 200 (checks: %v)", code, checks)
			}
			if !strings.HasPrefix(checks[tc.check], "ok (") {
				t.Fatalf("%s = %q while healthy, want an ok reading", tc.check, checks[tc.check])
			}

			tc.breach(&rt)
			code, checks = readyChecks(t, s)
			if code != http.StatusServiceUnavailable {
				t.Fatalf("/ready = %d at the threshold, want 503 (checks: %v)", code, checks)
			}
			if !strings.Contains(checks[tc.check], tc.wantNum) {
				t.Fatalf("%s = %q, want the measured figure %s", tc.check, checks[tc.check], tc.wantNum)
			}

			rt = healthyRuntime()
			code, checks = readyChecks(t, s)
			if code != http.StatusOK {
				t.Fatalf("/ready = %d after recovery, want 200 (checks: %v)", code, checks)
			}
			if !strings.HasPrefix(checks[tc.check], "ok (") {
				t.Fatalf("%s = %q after recovery, want an ok reading", tc.check, checks[tc.check])
			}
		})
	}
}

func TestAggregateRuntimeThresholdsAreConfigurable(t *testing.T) {
	s := newTestServer(t)
	rt := healthyRuntime()
	rt.CommitFailureStreak = 2
	rt.AdmissionRatio = 0.5
	s.SetAggregateRuntimeProbe(func() AggregateRuntime { return rt })

	// A stricter commit threshold fails the same snapshot the default passes.
	th := DefaultReadinessThresholds()
	th.MaxCommitFailureStreak = 2
	s.SetReadinessThresholds(th)
	code, checks := readyChecks(t, s)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d at a stricter commit threshold, want 503 (checks: %v)", code, checks)
	}

	// Zero disables the probe outright — the escape hatch for an operator who
	// disagrees with the default.
	th.MaxCommitFailureStreak = 0
	s.SetReadinessThresholds(th)
	code, checks = readyChecks(t, s)
	if code != http.StatusOK {
		t.Fatalf("/ready = %d with the commit probe disabled, want 200 (checks: %v)", code, checks)
	}
	if checks["aggregate_commit"] != "skipped" {
		t.Fatalf("aggregate_commit = %q with a zero threshold, want skipped", checks["aggregate_commit"])
	}

	// A looser admission threshold passes a snapshot the default would fail.
	rt.AdmissionRatio = 0.93
	s.SetReadinessThresholds(DefaultReadinessThresholds())
	if code, _ = readyChecks(t, s); code != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d at 0.93 saturation under the default, want 503", code)
	}
	th = DefaultReadinessThresholds()
	th.MaxAdmissionRatio = 0.99
	s.SetReadinessThresholds(th)
	if code, checks = readyChecks(t, s); code != http.StatusOK {
		t.Fatalf("/ready = %d at 0.93 saturation under a 0.99 limit, want 200 (checks: %v)", code, checks)
	}
}

func TestReadyFlipsOnAggregateStoreUnreachable(t *testing.T) {
	s := newTestServer(t)
	var pingErr error
	deadlineSeen := false
	s.SetAggregateDBProbe(func(ctx context.Context) error {
		_, deadlineSeen = ctx.Deadline()
		return pingErr
	})

	code, checks := readyChecks(t, s)
	if code != http.StatusOK {
		t.Fatalf("/ready = %d with a reachable store, want 200 (checks: %v)", code, checks)
	}
	if checks["aggregate_db"] != "ok" {
		t.Fatalf("aggregate_db = %q with a reachable store, want ok", checks["aggregate_db"])
	}
	if !deadlineSeen {
		t.Fatal("aggregate_db probe ran without a deadline: a readiness request must never park on the store")
	}

	pingErr = errors.New("database is locked")
	code, checks = readyChecks(t, s)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d with an unreachable store, want 503", code)
	}
	if !strings.Contains(checks["aggregate_db"], "database is locked") {
		t.Fatalf("aggregate_db = %q, want the ping error", checks["aggregate_db"])
	}

	pingErr = nil
	if code, _ = readyChecks(t, s); code != http.StatusOK {
		t.Fatalf("/ready = %d after the store came back, want 200", code)
	}
}

// Liveness is process-only. Every runtime probe failing at once is a degraded
// process, not a dead one: killing it would throw away the in-memory shards
// and the delta log replay that come back with it.
func TestLiveUnaffectedByRuntimeProbeFailures(t *testing.T) {
	s := newTestServer(t)
	s.SetAggregateDBProbe(func(context.Context) error { return errors.New("unreachable") })
	s.SetAggregateRuntimeProbe(func() AggregateRuntime {
		return AggregateRuntime{
			CommitFailureStreak:   99,
			FinalizeFailureStreak: 99,
			AdmissionRatio:        1,
			DeltaLogAgeSeconds:    99999,
			DiskUsedBytes:         1536 << 20,
			DiskBudgetBytes:       1536 << 20,
		}
	})
	s.SetAggregateRecoveryProbe(func() bool { return false })
	s.SetDiskPressureProbe(func() (string, bool) { return "raw_off", false })

	if code, _ := readyChecks(t, s); code != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d with every probe failing, want 503", code)
	}

	req := httptest.NewRequest(http.MethodGet, "/live", nil)
	rr := httptest.NewRecorder()
	s.handleLive(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/live = %d with every readiness probe failing, want 200", rr.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /live body: %v", err)
	}
	if body["status"] != "alive" {
		t.Fatalf("/live status = %q, want alive", body["status"])
	}
}
