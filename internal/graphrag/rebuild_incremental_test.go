package graphrag

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

// seedSpan inserts one span row for the default tenant.
func seedSpan(t *testing.T, repo *storage.Repository, service, spanID string, start time.Time) {
	t.Helper()
	sp := storage.Span{
		TenantID:      storage.DefaultTenantID,
		TraceID:       "trace-" + spanID,
		SpanID:        spanID,
		OperationName: "/op",
		ServiceName:   service,
		Status:        "STATUS_CODE_OK",
		StartTime:     start,
		EndTime:       start.Add(time.Millisecond),
		Duration:      1000,
	}
	if err := repo.DB().Create(&sp).Error; err != nil {
		t.Fatalf("seed span %s: %v", spanID, err)
	}
}

// TestRebuild_IncrementalWindow proves the per-tenant high-water-mark: the
// second rebuild tick only re-reads spans newer than HWM minus the overlap,
// so a row inserted between ticks with an older start_time (late backfill
// outside the overlap) is not merged, while fresh rows are.
func TestRebuild_IncrementalWindow(t *testing.T) {
	repo := newTestRepo(t)
	g := New(repo, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	now := time.Now()
	ctx := context.Background()

	// Tick 1: full window (fresh store, HWM == 0) reads both rows.
	seedSpan(t, repo, "orders", "s-a", now.Add(-30*time.Minute))
	seedSpan(t, repo, "orders", "s-a2", now.Add(-10*time.Minute))
	g.rebuildAllTenantsFromDB(ctx)

	stores := g.storesForTenant(storage.DefaultTenantID)
	svc, ok := stores.service.GetService("orders")
	if !ok || svc.CallCount != 2 {
		t.Fatalf("tick 1: orders CallCount = %v, want 2 (full window)", svc)
	}
	hwm := stores.lastRebuildMax.Load()
	if hwm == 0 {
		t.Fatalf("tick 1 did not advance the high-water-mark")
	}

	// Between ticks: one backfilled row older than HWM-overlap (must be
	// skipped) and one fresh row (must be merged).
	seedSpan(t, repo, "ghost", "s-ghost", now.Add(-20*time.Minute))
	seedSpan(t, repo, "fresh-svc", "s-fresh", now)

	// Tick 2: incremental window = (HWM - 5min, now] = (now-15min, now].
	g.rebuildAllTenantsFromDB(ctx)

	if _, ok := stores.service.GetService("ghost"); ok {
		t.Errorf("tick 2 re-read the full window — ghost (start_time < HWM-5min) was merged")
	}
	if _, ok := stores.service.GetService("fresh-svc"); !ok {
		t.Errorf("tick 2 missed the fresh span — incremental window too narrow")
	}
	if got := stores.lastRebuildMax.Load(); got <= hwm {
		t.Errorf("HWM did not advance on tick 2: %d -> %d", hwm, got)
	}
}

// TestRebuild_HWMNeverRegresses proves a tick that reads only older rows
// (possible when the overlap re-reads the tail) cannot move the
// high-water-mark backwards.
func TestRebuild_HWMNeverRegresses(t *testing.T) {
	repo := newTestRepo(t)
	g := New(repo, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	now := time.Now()
	ctx := context.Background()

	seedSpan(t, repo, "orders", "s-1", now.Add(-2*time.Minute))
	g.rebuildAllTenantsFromDB(ctx)
	stores := g.storesForTenant(storage.DefaultTenantID)
	hwm := stores.lastRebuildMax.Load()
	if hwm == 0 {
		t.Fatalf("HWM not set on first rebuild")
	}

	// Second tick re-reads the same row via the overlap; HWM must hold.
	g.rebuildAllTenantsFromDB(ctx)
	if got := stores.lastRebuildMax.Load(); got != hwm {
		t.Fatalf("HWM changed without newer rows: %d -> %d", hwm, got)
	}
}

// TestRebuild_FullWindowAfterEviction proves the self-healing contract:
// a tenant evicted and re-discovered gets a fresh slice (HWM 0) and the
// next rebuild takes the full trailing window again.
func TestRebuild_FullWindowAfterEviction(t *testing.T) {
	repo := newTestRepo(t)
	g := New(repo, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	now := time.Now()
	ctx := context.Background()
	tctx := storage.WithTenantContext(ctx, "tenant-x")

	for i := 0; i < 3; i++ {
		sp := storage.Span{
			TenantID:    "tenant-x",
			TraceID:     fmt.Sprintf("t-%d", i),
			SpanID:      fmt.Sprintf("s-%d", i),
			ServiceName: "orders",
			Status:      "STATUS_CODE_OK",
			StartTime:   now.Add(-30 * time.Minute),
			EndTime:     now.Add(-30 * time.Minute).Add(time.Millisecond),
		}
		if err := repo.DB().Create(&sp).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	g.rebuildAllTenantsFromDB(ctx)
	if len(g.ServiceMap(tctx, 0)) == 0 {
		t.Fatalf("tenant-x not built on first rebuild")
	}

	// Evict, then rebuild: the fresh slice must take the full window and
	// recover the 30min-old spans.
	g.storesForTenant("tenant-x").lastAccess.Store(staleNanos())
	if g.evictIdleTenants() != 1 {
		t.Fatalf("eviction did not fire")
	}
	g.rebuildAllTenantsFromDB(ctx)
	if len(g.ServiceMap(tctx, 0)) == 0 {
		t.Fatalf("evicted tenant not rebuilt with full window")
	}
}

// TestRebuild_SQLiteUsesStartTimeIndex guards the plan, not just the result:
// on SQLite without ANALYZE stats the tenant_id index scanned every span the
// tenant ever kept, holding the single connection for seconds per tick. Both
// the tenant discovery and the per-tenant rebuild run on every tick.
func TestRebuild_SQLiteUsesStartTimeIndex(t *testing.T) {
	repo := newTestRepo(t)
	g := New(repo, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)
	seedSpan(t, repo, "orders", "s-plan", time.Now().Add(-time.Minute))

	plans := map[string]string{}
	if err := repo.DB().Callback().Query().After("gorm:query").Register("test:capture_rebuild_plan", func(db *gorm.DB) {
		sql := db.Statement.SQL.String()
		if !strings.Contains(sql, "FROM spans") && !strings.Contains(sql, "FROM `spans`") {
			return
		}
		var rows []struct{ Detail string }
		db.Session(&gorm.Session{NewDB: true, SkipHooks: true}).Raw("EXPLAIN QUERY PLAN "+sql, db.Statement.Vars...).Scan(&rows)
		for _, r := range rows {
			plans[sql] += r.Detail + "\n"
		}
	}); err != nil {
		t.Fatal(err)
	}

	g.rebuildAllTenantsFromDB(context.Background())
	if len(plans) < 2 {
		t.Fatalf("want tenant discovery and rebuild queries, captured %q", plans)
	}
	for sql, plan := range plans {
		if !strings.Contains(plan, "idx_spans_start_time") {
			t.Errorf("plan does not use idx_spans_start_time:\n%s\n%s", sql, plan)
		}
	}
}
