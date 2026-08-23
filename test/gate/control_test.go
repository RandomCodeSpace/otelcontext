//go:build gate

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/test/gate/gatecore"
)

// hangingServer answers nothing until the test ends: every request blocks
// until the server is closed.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })
	return srv
}

func gateAgainst(t *testing.T, srv *httptest.Server, ctl time.Duration) *gate {
	t.Helper()
	return &gate{
		cfg:  gatecore.Config{HTTPAddr: srv.Listener.Addr().String()},
		http: &http.Client{Timeout: 600 * time.Second},
		ctl:  &http.Client{Timeout: ctl},
	}
}

// TestWaitReadyBoundedByDeadline pins the review contract: waitReady must
// return within its own budget even when /ready never answers — the long
// query-client timeout must not leak into the readiness loop.
func TestWaitReadyBoundedByDeadline(t *testing.T) {
	srv := hangingServer(t)
	g := gateAgainst(t, srv, 5*time.Second)

	started := time.Now()
	_, err := g.waitReady(2 * time.Second)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("waitReady succeeded against a server that never answers")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("waitReady took %s against a hung /ready, want return near its 2s budget", elapsed)
	}
}

// TestSamplerShutdownBoundedWithStuckScrape: a scrape stuck on a hung
// endpoint must not hold sampler shutdown past the control-client timeout.
func TestSamplerShutdownBoundedWithStuckScrape(t *testing.T) {
	srv := hangingServer(t)
	g := gateAgainst(t, srv, 500*time.Millisecond)
	g.cfg.Sampling.IntervalSec = 0.05

	s := newSampler(g)
	go s.run()
	time.Sleep(100 * time.Millisecond) // let a tick enter the stuck scrape

	started := time.Now()
	s.shutdown()
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("sampler shutdown took %s with a stuck scrape, want a bound near the 500ms control timeout", elapsed)
	}
}
