package graphrag

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

var (
	testMetricsOnce sync.Once
	testMetrics     *telemetry.Metrics
)

// sharedTestMetrics returns a process-wide telemetry.Metrics. promauto
// registers on the default Prometheus registry and panics on duplicate
// registration, so every graphrag test shares this single instance.
func sharedTestMetrics() *telemetry.Metrics {
	testMetricsOnce.Do(func() { testMetrics = telemetry.New() })
	return testMetrics
}

// TestRefreshLoop_TickPrunesAndEvicts drives a real refreshLoop tick with a
// tiny RefreshEvery and proves the tick wiring end-to-end: expired spans
// pruned (TraceStore.Prune), stale metrics pruned (SignalStore.Prune) and
// idle tenants evicted — with Prometheus metrics wired so the counter
// branches are exercised too.
func TestRefreshLoop_TickPrunesAndEvicts(t *testing.T) {
	repo := newTestRepo(t)
	cfg := DefaultConfig()
	cfg.RefreshEvery = 20 * time.Millisecond
	cfg.TraceTTL = time.Minute
	g := New(repo, nil, nil, cfg)
	g.SetMetrics(sharedTestMetrics())
	t.Cleanup(g.Stop)

	now := time.Now()
	def := g.storesForTenant(storage.DefaultTenantID)
	def.traces.UpsertSpan(mkSpanNode("s-old", now.Add(-2*time.Minute))) // past TraceTTL
	def.signals.UpsertMetric("cpu", "svc", 1.0, now.Add(-48*time.Hour)) // past signalRetention
	g.storesForTenant("tenant-idle").lastAccess.Store(staleNanos())     // past TenantIdleTTL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.refreshLoop(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		def.traces.mu.RLock()
		_, spanAlive := def.traces.Spans["s-old"]
		def.traces.mu.RUnlock()
		def.signals.mu.RLock()
		_, metricAlive := def.signals.Metrics["cpu|svc"]
		def.signals.mu.RUnlock()
		g.tenantsMu.RLock()
		_, tenantAlive := g.tenants["tenant-idle"]
		g.tenantsMu.RUnlock()
		if !spanAlive && !metricAlive && !tenantAlive {
			if g.TenantsEvictedCount() == 0 {
				t.Fatalf("eviction happened but the counter did not tick")
			}
			return // all tick effects observed
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("refreshLoop tick did not prune spans/metrics or evict the idle tenant within 2s")
}

// TestRebuild_RowLimitMergesLimitedWindow seeds exactly rebuildRowLimit rows
// so the limit-hit branch fires (warning logged); the observable contract is
// that the pass still merges the limited window instead of bailing.
func TestRebuild_RowLimitMergesLimitedWindow(t *testing.T) {
	repo := newTestRepo(t)
	g := New(repo, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	now := time.Now()
	spans := make([]storage.Span, rebuildRowLimit)
	for i := range spans {
		spans[i] = storage.Span{
			TenantID:    storage.DefaultTenantID,
			TraceID:     "t-bulk",
			SpanID:      fmt.Sprintf("s-%d", i),
			ServiceName: "svc",
			Status:      "STATUS_CODE_OK",
			StartTime:   now,
			EndTime:     now,
		}
	}
	// Batch size bounded by SQLite's SQL-variable limit (rows × columns).
	if err := repo.DB().CreateInBatches(spans, 500).Error; err != nil {
		t.Fatalf("bulk seed: %v", err)
	}

	g.rebuildAllTenantsFromDB(context.Background())

	if _, ok := g.storesForTenant(storage.DefaultTenantID).service.GetService("svc"); !ok {
		t.Fatalf("rebuild with row-limit hit did not merge any services")
	}
}
