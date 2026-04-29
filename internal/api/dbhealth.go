package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

// DBPinger is the minimum DB interface DBHealth needs. *sql.DB satisfies it;
// tests can pass a stub.
type DBPinger interface {
	PingContext(ctx context.Context) error
}

// DBHealth periodically pings the database and exposes the result via an
// atomic boolean. The HTTP middleware below short-circuits /api/* traffic
// with a 503 when the flag is false, preventing goroutine pile-up on pool
// acquisition when the DB is unreachable.
//
// To avoid false-positive 503 windows under load — e.g. SQLite with
// MaxOpen=1 where a busy write transiently blocks the 2s health ping —
// the poller flips to unhealthy only after failureThreshold consecutive
// failed pings. A single successful ping clears the counter and restores
// healthy immediately.
type DBHealth struct {
	db               DBPinger
	driver           string
	interval         time.Duration
	healthy          atomic.Bool
	consecutiveFails atomic.Int32
	failureThreshold int32
	metrics          *telemetry.Metrics
	stopCh           chan struct{}
	doneCh           chan struct{}
}

// defaultFailureThreshold is the number of consecutive failed pings before
// the middleware flips into 503-serving mode. Three @ 5s = ~15s of real
// outage before the gate trips, which comfortably swallows transient
// connection-pool contention on single-writer drivers like SQLite.
const defaultFailureThreshold = 3

// NewDBHealth constructs a health poller. Default poll interval is 5s; ping
// timeout is 2s per attempt; failureThreshold defaults to 3 consecutive
// failed pings before the gate flips. Start() must be called to begin polling.
func NewDBHealth(db DBPinger, driver string, metrics *telemetry.Metrics) *DBHealth {
	h := &DBHealth{
		db:               db,
		driver:           driver,
		interval:         5 * time.Second,
		failureThreshold: defaultFailureThreshold,
		metrics:          metrics,
		stopCh:           make(chan struct{}),
		doneCh:           make(chan struct{}),
	}
	// Assume healthy until the first ping proves otherwise — avoids a
	// spurious 503 window at startup.
	h.healthy.Store(true)
	if metrics != nil && metrics.DBUp != nil {
		metrics.DBUp.WithLabelValues(driver).Set(1)
	}
	return h
}

// SetFailureThreshold overrides the number of consecutive failed pings
// before the middleware flips to 503. n <= 0 normalises to 1 (legacy
// behaviour: any single failure trips the gate).
func (h *DBHealth) SetFailureThreshold(n int) {
	if n <= 0 {
		n = 1
	}
	atomic.StoreInt32(&h.failureThreshold, int32(n)) // #nosec G115 -- n bounded above; in test wiring
}

// Start launches the background poller.
func (h *DBHealth) Start(ctx context.Context) {
	go h.loop(ctx)
}

// Stop signals the poller to exit and waits briefly for it to finish.
func (h *DBHealth) Stop() {
	select {
	case <-h.stopCh:
		// already stopped
	default:
		close(h.stopCh)
	}
	select {
	case <-h.doneCh:
	case <-time.After(2 * time.Second):
	}
}

// Healthy reports the most recent ping result.
func (h *DBHealth) Healthy() bool { return h.healthy.Load() }

func (h *DBHealth) loop(ctx context.Context) {
	defer close(h.doneCh)
	tick := time.NewTicker(h.interval)
	defer tick.Stop()
	h.ping(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stopCh:
			return
		case <-tick.C:
			h.ping(ctx)
		}
	}
}

func (h *DBHealth) ping(parent context.Context) {
	if h.db == nil {
		return
	}
	pctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	err := h.db.PingContext(pctx)
	if err == nil {
		// A single success clears the failure streak and restores healthy
		// immediately — recovery should not be debounced.
		h.consecutiveFails.Store(0)
		h.markHealthy(true)
		return
	}
	fails := h.consecutiveFails.Add(1)
	threshold := atomic.LoadInt32(&h.failureThreshold)
	if threshold < 1 {
		threshold = 1
	}
	if fails >= threshold {
		h.markHealthy(false)
	}
	// Below threshold: leave the gate where it was. A transient pool-contention
	// timeout under SQLite MaxOpen=1 must not produce a user-visible 503 window.
}

func (h *DBHealth) markHealthy(up bool) {
	h.healthy.Store(up)
	if h.metrics == nil || h.metrics.DBUp == nil {
		return
	}
	if up {
		h.metrics.DBUp.WithLabelValues(h.driver).Set(1)
	} else {
		h.metrics.DBUp.WithLabelValues(h.driver).Set(0)
	}
}

// dbHealthSkipPath mirrors the auth skip-list: probes, metrics, and the UI
// bundle must stay reachable even when the DB is down so operators can still
// see liveness and scraped metrics.
func dbHealthSkipPath(path string) bool {
	switch {
	case path == "/live", path == "/ready", path == "/health":
		return true
	case strings.HasPrefix(path, "/metrics"):
		return true
	case strings.HasPrefix(path, "/ws"):
		return true
	case path == "/api/health":
		return true
	}
	// Everything else under /api/ or /v1/ is DB-backed.
	if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/") {
		return false
	}
	// UI static bundle and all other paths — pass through.
	return true
}

// DBHealthMiddleware returns 503 immediately when the DB poller reports
// unhealthy, for DB-dependent paths. Health/metrics/UI paths bypass the gate.
func DBHealthMiddleware(h *DBHealth) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if h == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if dbHealthSkipPath(r.URL.Path) || h.Healthy() {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"database unavailable"}`))
		})
	}
}

// Compile-time assertion that *sql.DB satisfies DBPinger.
var _ DBPinger = (*sql.DB)(nil)
