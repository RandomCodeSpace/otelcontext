package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// doClampGet issues one tenant-scoped GET against a handler and returns the
// recorder without asserting the status; clamp tests care about non-200 too.
func doClampGet(t *testing.T, h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := storage.WithTenantContext(context.Background(), "default")
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func rangeQueryString(start, end time.Time) string {
	return "?start=" + url.QueryEscape(start.Format(time.RFC3339)) +
		"&end=" + url.QueryEscape(end.Format(time.RFC3339))
}

// TestOverCapRangeClampsInsteadOfFailing is the #217 contract: a request
// whose range exceeds the engine's read-range cap is served from the clamped
// range with the clamp declared in headers — not refused with a 500.
func TestOverCapRangeClampsInsteadOfFailing(t *testing.T) {
	s, _, e := aggregateTestServer(t, true)
	seedAggregate(t, e, "default", "checkout", 6, 1500)

	end := time.Now().UTC().Truncate(time.Second)
	start := end.Add(-200 * time.Hour) // well past the ~168h read-range cap
	q := rangeQueryString(start, end)

	alignedEnd := aggregate.WindowStart(end.Add(aggregate.WindowSize - time.Second))
	wantEffective := time.Unix(alignedEnd-aggregate.MaxReadWindowSpan, 0).UTC().Format(time.RFC3339)

	handlers := map[string]http.HandlerFunc{
		"dashboard":   s.handleGetDashboardStats,
		"traffic":     s.handleGetTrafficMetrics,
		"service-map": s.handleGetServiceMapMetrics,
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			rr := doClampGet(t, h, "/api/metrics/"+name+q)
			if rr.Code != http.StatusOK {
				t.Fatalf("over-cap range = %d, want 200: %s", rr.Code, rr.Body.String())
			}
			if got, want := rr.Header().Get(RequestedStartHeader), start.Format(time.RFC3339); got != want {
				t.Errorf("%s = %q, want %q", RequestedStartHeader, got, want)
			}
			if got := rr.Header().Get(EffectiveStartHeader); got != wantEffective {
				t.Errorf("%s = %q, want %q", EffectiveStartHeader, got, wantEffective)
			}
		})
	}
}

// TestInCapRangeStampsNoClampHeaders proves the headers appear ONLY when a
// clamp happened: an ordinary in-cap request carries neither.
func TestInCapRangeStampsNoClampHeaders(t *testing.T) {
	s, _, e := aggregateTestServer(t, true)
	seedAggregate(t, e, "default", "checkout", 3, 900)

	end := time.Now().UTC().Truncate(time.Second)
	rr := doClampGet(t, s.handleGetTrafficMetrics,
		"/api/metrics/traffic"+rangeQueryString(end.Add(-30*time.Minute), end))
	if rr.Code != http.StatusOK {
		t.Fatalf("in-cap range = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	for _, h := range []string{RequestedStartHeader, EffectiveStartHeader} {
		if got := rr.Header().Get(h); got != "" {
			t.Errorf("%s = %q on an unclamped response, want absent", h, got)
		}
	}
}

// TestSelectorErrorIsClientError pins the backstop's status: a selector the
// engine still refuses after clamping (here a reversed range) is the client's
// mistake and maps to 400, not 500 (#217).
func TestSelectorErrorIsClientError(t *testing.T) {
	s, _, _ := aggregateTestServer(t, true)

	end := time.Now().UTC().Truncate(time.Second)
	start := end.Add(24 * time.Hour) // reversed: start after end
	rr := doClampGet(t, s.handleGetTrafficMetrics,
		"/api/metrics/traffic"+rangeQueryString(start, end))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("reversed range = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

// TestClampAggregateRangeBoundary pins the clamp threshold to the byte: a
// span of exactly MaxReadWindowSpan passes untouched; one second more clamps.
func TestClampAggregateRangeBoundary(t *testing.T) {
	// A window-aligned end makes the arithmetic exact.
	end := time.Unix(aggregate.WindowStart(time.Now()), 0).UTC()

	rr := httptest.NewRecorder()
	exact := end.Add(-time.Duration(aggregate.MaxReadWindowSpan) * time.Second)
	if got := clampAggregateRange(rr, exact, end); !got.Equal(exact) {
		t.Fatalf("exact-cap span clamped to %v, want untouched %v", got, exact)
	}
	if rr.Header().Get(EffectiveStartHeader) != "" {
		t.Fatal("exact-cap span stamped a clamp header")
	}

	rr = httptest.NewRecorder()
	over := exact.Add(-time.Second)
	got := clampAggregateRange(rr, over, end)
	if !got.Equal(exact) {
		t.Fatalf("over-cap span clamped to %v, want %v", got, exact)
	}
	if rr.Header().Get(RequestedStartHeader) == "" || rr.Header().Get(EffectiveStartHeader) == "" {
		t.Fatal("over-cap span did not stamp the clamp headers")
	}
}
