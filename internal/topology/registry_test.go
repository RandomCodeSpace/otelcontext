package topology

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

func newRegistryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := storage.NewDatabase("sqlite", filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := db.AutoMigrate(&storage.ResourceRegistryEntry{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func TestRegistryRegisterMergesSignalsAndKeepsEmptyHost(t *testing.T) {
	r := NewRegistry(nil)
	now := time.Unix(1_700_000_000, 0)
	if !r.Register("acme", "checkout", "", "", "", SignalTraces, now) {
		t.Fatal("host-less resource was refused")
	}
	if !r.Register("acme", "checkout", "", "", "", SignalLogs, now.Add(time.Second)) {
		t.Fatal("second signal was refused")
	}
	if !r.Register("acme", "checkout", "node-1", "pod-1", "pod", SignalMetrics, now) {
		t.Fatal("host-bearing resource was refused")
	}
	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot = %#v", snap)
	}
	if snap[0].Host != "" || snap[0].Signals != SignalTraces|SignalLogs || !snap[0].LastSeen.Equal(now.Add(time.Second)) {
		t.Fatalf("host-less entry = %#v", snap[0])
	}
	if snap[1].Host != "node-1" || snap[1].Workload != "pod-1" || snap[1].Kind != "pod" || snap[1].Signals != SignalMetrics {
		t.Fatalf("host entry = %#v", snap[1])
	}
}

func TestRegistryBoundsHoldUnder100kDistinctHosts(t *testing.T) {
	r := NewRegistry(nil)
	now := time.Now()
	accepted := 0
	for i := 0; i < 100_000; i++ {
		if r.Register("acme", "svc", fmt.Sprintf("host-%d", i), "", "", SignalTraces, now) {
			accepted++
		}
	}
	if accepted != RegistryMaxHostsPerTenant {
		t.Fatalf("accepted %d hosts, want %d", accepted, RegistryMaxHostsPerTenant)
	}
	if got := r.Overflow("acme", RegistryKindHost); got != 100_000-RegistryMaxHostsPerTenant {
		t.Fatalf("host overflow = %d", got)
	}
	// Known hosts still accept new services until the pair bound.
	pairs := 0
	for i := 0; pairs < RegistryMaxEntriesPerTenant*2 && i < RegistryMaxEntriesPerTenant*2; i++ {
		if r.Register("acme", fmt.Sprintf("svc-%d", i), "host-0", "", "", SignalLogs, now) {
			pairs++
		} else {
			break
		}
	}
	if pairs != RegistryMaxEntriesPerTenant-RegistryMaxHostsPerTenant {
		t.Fatalf("accepted %d extra pairs, want %d", pairs, RegistryMaxEntriesPerTenant-RegistryMaxHostsPerTenant)
	}
	if got := r.Overflow("acme", RegistryKindPair); got != 1 {
		t.Fatalf("pair overflow = %d", got)
	}
	if len(r.Snapshot()) != RegistryMaxEntriesPerTenant {
		t.Fatalf("snapshot size = %d", len(r.Snapshot()))
	}
	// Another tenant is unaffected, and nothing was merged into a shared host.
	if !r.Register("beta", "svc", "host-999999", "", "", SignalTraces, now) {
		t.Fatal("second tenant refused")
	}
	for _, e := range r.Snapshot() {
		if e.Host == "__other__" {
			t.Fatal("overflow merged into __other__")
		}
	}
}

func TestRegistryPersistsReloadsAndEvictsOnFirstTick(t *testing.T) {
	db := newRegistryDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	stale := now.Add(-RegistryIdleTTL - time.Hour)

	r := NewRegistry(nil)
	r.Register("acme", "checkout", "node-1", "ctr-1", "container", SignalTraces|SignalLogs, now)
	r.Register("acme", "legacy", "node-9", "", "", SignalMetrics, stale)
	if err := r.Flush(ctx, db); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	var rows int64
	if err := db.Table("resource_registry").Count(&rows).Error; err != nil || rows != 2 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}

	// Restart: reload keeps last_seen, then the first tick evicts the stale
	// entry and the flush deletes its row.
	reloaded := NewRegistry(nil)
	if n, err := reloaded.Load(ctx, db); err != nil || n != 2 {
		t.Fatalf("Load n=%d err=%v", n, err)
	}
	snap := reloaded.Snapshot()
	if len(snap) != 2 || !snap[0].LastSeen.Equal(now) || snap[0].Signals != SignalTraces|SignalLogs || snap[0].Kind != "container" || !snap[1].LastSeen.Equal(stale) {
		t.Fatalf("reloaded snapshot = %#v", snap)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reloaded.Run(runCtx, db, 20*time.Millisecond)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(reloaded.Snapshot()) != 1 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("first tick did not evict the expired entry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if err := reloaded.Flush(ctx, db); err != nil {
		t.Fatalf("shutdown Flush: %v", err)
	}
	var kept []storage.ResourceRegistryEntry
	if err := db.Find(&kept).Error; err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].ServiceName != "checkout" || kept[0].Host != "node-1" || !kept[0].LastSeen.Equal(now) {
		t.Fatalf("rows after eviction = %#v", kept)
	}
}

func BenchmarkRegistryRegisterPresent(b *testing.B) {
	r := NewRegistry(nil)
	now := time.Now()
	r.Register("acme", "checkout", "node-1", "pod-1", "pod", SignalTraces, now)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Register("acme", "checkout", "node-1", "pod-1", "pod", SignalTraces, now)
	}
}
