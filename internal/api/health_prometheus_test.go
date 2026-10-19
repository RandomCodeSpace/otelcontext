package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/prometheus"
)

func TestReadyRequiredPrometheusOutageAndRecovery(t *testing.T) {
	var unavailable atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/-/ready" {
			t.Errorf("unexpected probe %s", r.URL.Path)
		}
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer backend.Close()
	client, err := prometheus.NewClient(prometheus.Config{URL: backend.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	s := newTestServer(t)
	s.SetPrometheusReadinessProbe(client.Ready)
	if code, checks := readyChecks(t, s); code != http.StatusOK || checks["prometheus"] != "ok" {
		t.Fatalf("healthy backend: %d %v", code, checks)
	}
	unavailable.Store(true)
	if code, checks := readyChecks(t, s); code != http.StatusServiceUnavailable || checks["prometheus"] == "ok" {
		t.Fatalf("failed backend: %d %v", code, checks)
	}
	unavailable.Store(false)
	if code, checks := readyChecks(t, s); code != http.StatusOK || checks["prometheus"] != "ok" {
		t.Fatalf("recovered backend: %d %v", code, checks)
	}
	s.SetPrometheusReadinessProbe(nil)
	unavailable.Store(true)
	if code, checks := readyChecks(t, s); code != http.StatusOK || checks["prometheus"] != "not_required" {
		t.Fatalf("optional backend: %d %v", code, checks)
	}
}
