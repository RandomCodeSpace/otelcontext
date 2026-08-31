package graphrag

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func TestShutdownDrainsFinalEventAndPersistsTemplate(t *testing.T) {
	repo := newTestRepo(t)
	if err := repo.DB().AutoMigrate(&DrainTemplateRow{}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.WorkerCount = 1
	cfg.ChannelSize = 8
	cfg.RefreshEvery = time.Hour
	cfg.SnapshotEvery = time.Hour
	cfg.AnomalyEvery = time.Hour
	g := New(repo, nil, nil, cfg)
	g.Start(context.Background())
	g.OnLogIngested(storage.Log{
		TenantID: storage.DefaultTenantID, ServiceName: "shutdown-fixture",
		Severity: "ERROR", Body: "payment failed for order 123", Timestamp: time.Now(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := g.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}

	var count int64
	if err := repo.DB().Model(&DrainTemplateRow{}).
		Where("tenant_id = ?", storage.DefaultTenantID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("last accepted log template was not persisted")
	}
}

func TestShutdownBeforeStartIsSafe(t *testing.T) {
	g := New(nil, nil, nil, DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownReturnsFinalTemplatePersistenceFailure(t *testing.T) {
	repo := newTestRepo(t)
	if err := repo.DB().AutoMigrate(&DrainTemplateRow{}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.WorkerCount = 1
	cfg.RefreshEvery = time.Hour
	cfg.SnapshotEvery = time.Hour
	cfg.AnomalyEvery = time.Hour
	g := New(repo, nil, nil, cfg)
	g.Start(context.Background())
	g.OnLogIngested(storage.Log{ServiceName: "shutdown", Body: "template save must fail", Timestamp: time.Now()})
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.Shutdown(ctx); err == nil {
		t.Fatal("shutdown reported success after final template persistence failed")
	}
}
