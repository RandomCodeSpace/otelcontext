package graphrag

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// newTestRepo builds an in-memory SQLite Repository with all models migrated.
// Duplicates the fixture in internal/storage/testhelpers_test.go because that
// helper is in a different test package and thus not importable here.
func newTestRepo(t *testing.T) *storage.Repository {
	t.Helper()
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// newTestGraphRAG constructs a GraphRAG usable in tests without a repo or
// vectordb. The event workers are started so ingestion callbacks process
// events asynchronously; tests must call Stop() via t.Cleanup.
func newTestGraphRAG(t *testing.T) *GraphRAG {
	t.Helper()
	g := New(nil, nil, nil, nil, DefaultConfig())
	// Start only the event workers — the background refresh/snapshot/anomaly
	// loops require a repo, which this helper intentionally does not wire.
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < defaultWorkerCount; i++ {
		go g.eventWorker(ctx)
	}
	t.Cleanup(func() {
		cancel()
		g.Stop()
	})
	return g
}

// TestOnSpanIngested_PropagatesErrorStatus asserts that when the ingestion
// callback receives a span with status STATUS_CODE_ERROR, the GraphRAG
// ServiceStore's ErrorCount for that service increments. Before the fix,
// OnSpanIngested hardcoded status "OK" and the error was silently dropped.
func TestOnSpanIngested_PropagatesErrorStatus(t *testing.T) {
	g := newTestGraphRAG(t)

	errSpan := storage.Span{
		TenantID:      storage.DefaultTenantID,
		TraceID:       "trace-err",
		SpanID:        "span-err",
		OperationName: "/checkout",
		ServiceName:   "orders",
		Status:        "STATUS_CODE_ERROR",
		StartTime:     time.Now(),
		EndTime:       time.Now().Add(time.Millisecond),
	}
	g.OnSpanIngested(errSpan)

	// Event loop is async; poll briefly for the event to be processed.
	stores := g.storesForTenant(storage.DefaultTenantID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if svc, ok := stores.service.GetService("orders"); ok && svc.ErrorCount > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("ERROR span did not increment ServiceStore.ErrorCount — status was dropped")
}

// TestRefresh_PopulatesErrorCountFromDBStatus asserts that a persisted
// ERROR span is reflected in ServiceStore.ErrorCount after the refresh
// rebuild path runs. Before the fix, rebuildFromDB's SELECT omitted the
// status column so every reloaded span looked successful.
func TestRefresh_PopulatesErrorCountFromDBStatus(t *testing.T) {
	repo := newTestRepo(t)

	// Seed: one trace + one ERROR span under it, on the default tenant.
	now := time.Now()
	tr := storage.Trace{
		TenantID:    storage.DefaultTenantID,
		TraceID:     "trace-err-refresh",
		ServiceName: "orders",
		Duration:    1000,
		Status:      "STATUS_CODE_ERROR",
		Timestamp:   now,
	}
	if err := repo.DB().Create(&tr).Error; err != nil {
		t.Fatalf("seed trace: %v", err)
	}
	sp := storage.Span{
		TenantID:      storage.DefaultTenantID,
		TraceID:       "trace-err-refresh",
		SpanID:        "span-err-refresh",
		OperationName: "/checkout",
		ServiceName:   "orders",
		Status:        "STATUS_CODE_ERROR",
		StartTime:     now,
		EndTime:       now.Add(time.Millisecond),
		Duration:      1000,
	}
	if err := repo.DB().Create(&sp).Error; err != nil {
		t.Fatalf("seed span: %v", err)
	}

	// Build GraphRAG with the seeded repo, skip starting background loops;
	// invoke the rebuild path directly.
	g := New(repo, nil, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	g.rebuildAllTenantsFromDB(context.Background())

	stores := g.storesForTenant(storage.DefaultTenantID)
	svc, ok := stores.service.GetService("orders")
	if !ok {
		t.Fatalf("service 'orders' missing after rebuildAllTenantsFromDB")
	}
	if svc.ErrorCount < 1 {
		t.Fatalf("ErrorCount=%d after refresh, want >=1 — status not read from DB", svc.ErrorCount)
	}
}

// TestOnSpanIngested_DropsIncrementMetric asserts that when the event
// channel is full, OnSpanIngested records the drop via an atomic counter
// (and — when wired — the otelcontext_graphrag_events_dropped_total
// Prometheus metric).
func TestOnSpanIngested_DropsIncrementMetric(t *testing.T) {
	// Build a GraphRAG WITHOUT starting any event workers so the channel
	// fills up and overflows.
	g := New(nil, nil, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	// Fill the buffer beyond capacity. Use the package constant so the test
	// stays correct if defaultChannelSize is retuned.
	for range defaultChannelSize + 1000 {
		g.OnSpanIngested(storage.Span{
			TraceID:     "t",
			SpanID:      "s",
			ServiceName: "x",
			Status:      "STATUS_CODE_UNSET",
		})
	}
	if got := g.DroppedSpansCount(); got == 0 {
		t.Fatalf("expected drops > 0, got %d", got)
	}
}
