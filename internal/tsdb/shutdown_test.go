package tsdb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func TestShutdownPersistsFinalMetricAndIsIdempotent(t *testing.T) {
	db, err := storage.NewDatabase("sqlite", filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })

	a := NewAggregator(repo, time.Hour)
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go a.Start(runCtx)
	deadline := time.Now().Add(time.Second)
	for !a.started.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	a.Ingest(RawMetric{
		TenantID: storage.DefaultTenantID, ServiceName: "shutdown-fixture",
		Name: "last_accepted_metric", Value: 42, Timestamp: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}

	var count int64
	if err := repo.DB().Model(&storage.MetricBucket{}).
		Where("name = ?", "last_accepted_metric").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persisted metric buckets = %d, want 1", count)
	}
}

func TestShutdownBeforeStartIsSafe(t *testing.T) {
	a := NewAggregator(nil, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		a.Start(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("start after shutdown did not exit")
	}
}

func TestShutdownReturnsFinalPersistenceFailure(t *testing.T) {
	db, err := storage.NewDatabase("sqlite", filepath.Join(t.TempDir(), "failed-metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	a := NewAggregator(repo, time.Hour)
	go a.Start(context.Background())
	deadline := time.Now().Add(time.Second)
	for !a.started.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	a.Ingest(RawMetric{Name: "must-fail", ServiceName: "shutdown", Timestamp: time.Now(), Value: 1})
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err == nil {
		t.Fatal("shutdown reported success after final metric persistence failed")
	}
}
