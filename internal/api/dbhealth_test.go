package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type stubPinger struct {
	fail atomic.Bool
}

func (s *stubPinger) PingContext(_ context.Context) error {
	if s.fail.Load() {
		return errors.New("down")
	}
	return nil
}

func TestDBHealth_TogglesOnPingFailure(t *testing.T) {
	p := &stubPinger{}
	// Pass nil for metrics — DBHealth guards each metric access with a nil
	// check, and the global promauto registry would collide across tests.
	h := NewDBHealth(p, "sqlite", nil)
	h.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Start(ctx)
	defer h.Stop()

	if !h.Healthy() {
		t.Fatalf("expected initial healthy=true")
	}

	p.fail.Store(true)
	waitFor(t, 500*time.Millisecond, func() bool { return !h.Healthy() })

	mw := DBHealthMiddleware(h)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on /api/logs when unhealthy, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/live", nil)
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected /live passthrough, got %d", rec.Code)
	}

	p.fail.Store(false)
	waitFor(t, 500*time.Millisecond, func() bool { return h.Healthy() })
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after recovery, got %d", rec.Code)
	}
}

// TestDBHealth_SingleFailureDoesNotTripGate exercises the consecutive-failure
// threshold. With the legacy behaviour, a single ping failure flipped
// healthy=false and produced a 503 window for every /api/* request until
// the next successful poll up to 5s later. Under SQLite (MaxOpen=1) +
// real load, a poll competing with an in-flight write is enough to trip
// this — so a brief contended ping must NOT be observable to API clients.
func TestDBHealth_SingleFailureDoesNotTripGate(t *testing.T) {
	p := &stubPinger{}
	h := NewDBHealth(p, "sqlite", nil)
	// Drive ping() directly — no goroutine, no ticker — so we control the
	// exact failure pattern without timing flakes.
	ctx := context.Background()

	// One success then one failure; threshold is 3, so we must still be healthy.
	h.ping(ctx)
	if !h.Healthy() {
		t.Fatalf("expected healthy=true after first successful ping")
	}
	p.fail.Store(true)
	h.ping(ctx)
	if !h.Healthy() {
		t.Fatalf("single failure flipped healthy=false; threshold=%d should debounce",
			h.failureThreshold)
	}
	// Two failures total — still under threshold of 3.
	h.ping(ctx)
	if !h.Healthy() {
		t.Fatalf("two failures flipped healthy=false; threshold=%d should debounce",
			h.failureThreshold)
	}
}

// TestDBHealth_FlipsAtThreshold asserts that exactly N consecutive failed
// pings (default 3) are required before the gate trips and starts serving
// 503s. Anything less is treated as transient pool contention.
func TestDBHealth_FlipsAtThreshold(t *testing.T) {
	p := &stubPinger{fail: atomic.Bool{}}
	p.fail.Store(true)
	h := NewDBHealth(p, "sqlite", nil)
	ctx := context.Background()

	for i := 1; i < int(h.failureThreshold); i++ {
		h.ping(ctx)
		if !h.Healthy() {
			t.Fatalf("flipped after %d failures; expected only after %d",
				i, h.failureThreshold)
		}
	}
	// Threshold-th failure: must flip.
	h.ping(ctx)
	if h.Healthy() {
		t.Fatalf("did not flip after %d consecutive failures", h.failureThreshold)
	}
}

// TestDBHealth_SuccessResetsCounter asserts that the streak counter resets
// on the first success: two failures, one success, two more failures must
// NOT trip the gate, because there were never 3-in-a-row.
func TestDBHealth_SuccessResetsCounter(t *testing.T) {
	p := &stubPinger{}
	h := NewDBHealth(p, "sqlite", nil)
	ctx := context.Background()

	p.fail.Store(true)
	h.ping(ctx) // fail 1
	h.ping(ctx) // fail 2
	if !h.Healthy() {
		t.Fatalf("flipped before threshold")
	}
	p.fail.Store(false)
	h.ping(ctx) // success — counter reset
	if !h.Healthy() {
		t.Fatalf("expected healthy=true after success")
	}
	p.fail.Store(true)
	h.ping(ctx) // fail 1 (streak)
	h.ping(ctx) // fail 2 (streak)
	if !h.Healthy() {
		t.Fatalf("flipped after only 2 fresh failures; counter did not reset on success")
	}
}

// TestDBHealth_SetFailureThresholdNormalisesNonPositive confirms that a
// non-positive threshold collapses to 1 — restoring the legacy "any single
// failure trips the gate" behaviour for callers that explicitly opt out.
func TestDBHealth_SetFailureThresholdNormalisesNonPositive(t *testing.T) {
	p := &stubPinger{}
	h := NewDBHealth(p, "sqlite", nil)

	for _, n := range []int{0, -1, -100} {
		h.SetFailureThreshold(n)
		if got := atomic.LoadInt32(&h.failureThreshold); got != 1 {
			t.Fatalf("SetFailureThreshold(%d) → %d, want 1", n, got)
		}
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}
