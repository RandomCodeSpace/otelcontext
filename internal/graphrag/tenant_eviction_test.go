package graphrag

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// staleNanos returns a unix-nano timestamp comfortably past any idle TTL
// used in these tests.
func staleNanos() int64 {
	return time.Now().Add(-48 * time.Hour).UnixNano()
}

// TestEvictIdleTenants_EvictsStaleKeepsDefault proves the three eviction
// invariants: an idle tenant is removed, the default tenant is immune no
// matter how stale, and a fresh tenant survives.
func TestEvictIdleTenants_EvictsStaleKeepsDefault(t *testing.T) {
	g := New(nil, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	stale := g.storesForTenant("tenant-idle")
	fresh := g.storesForTenant("tenant-active")
	def := g.storesForTenant(storage.DefaultTenantID)

	stale.lastAccess.Store(staleNanos())
	def.lastAccess.Store(staleNanos()) // default tenant must survive anyway
	_ = fresh                          // lastAccess just touched by storesForTenant

	if got := g.evictIdleTenants(); got != 1 {
		t.Fatalf("evictIdleTenants = %d, want 1", got)
	}

	g.tenantsMu.RLock()
	_, idleAlive := g.tenants["tenant-idle"]
	_, activeAlive := g.tenants["tenant-active"]
	_, defAlive := g.tenants[storage.DefaultTenantID]
	g.tenantsMu.RUnlock()

	if idleAlive {
		t.Errorf("idle tenant still resident after eviction")
	}
	if !activeAlive {
		t.Errorf("active tenant was evicted")
	}
	if !defAlive {
		t.Errorf("default tenant was evicted — must be immune")
	}
	if got := g.TenantsEvictedCount(); got != 1 {
		t.Errorf("TenantsEvictedCount = %d, want 1", got)
	}
}

// TestEvictIdleTenants_ReactivationCreatesFreshSlice proves eviction is
// self-healing: the next storesForTenant call re-creates the slice with a
// fresh idle window, so a returning tenant is not insta-evicted.
func TestEvictIdleTenants_ReactivationCreatesFreshSlice(t *testing.T) {
	g := New(nil, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	old := g.storesForTenant("tenant-a")
	old.lastAccess.Store(staleNanos())
	if got := g.evictIdleTenants(); got != 1 {
		t.Fatalf("evictIdleTenants = %d, want 1", got)
	}

	reborn := g.storesForTenant("tenant-a")
	if reborn == old {
		t.Fatalf("storesForTenant returned the evicted slice — expected a fresh one")
	}
	if g.evictIdleTenants() != 0 {
		t.Fatalf("freshly re-created tenant must not be evicted")
	}
}

// TestEvictIdleTenants_DisabledWhenNegative proves a negative TenantIdleTTL
// turns eviction off entirely.
func TestEvictIdleTenants_DisabledWhenNegative(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TenantIdleTTL = -1
	g := New(nil, nil, nil, cfg)
	t.Cleanup(g.Stop)

	g.storesForTenant("tenant-b").lastAccess.Store(staleNanos())
	if got := g.evictIdleTenants(); got != 0 {
		t.Fatalf("eviction ran with TenantIdleTTL<0: evicted %d", got)
	}
}

// TestRebuild_DoesNotRefreshIdleClock proves the 60s DB rebuild cannot keep
// a dormant tenant alive: rebuildFromDBForTenant goes through
// tenantStoresNoTouch, so lastAccess stays stale and the next eviction pass
// still fires. Without this, every known tenant would be touched every tick
// and GRAPHRAG_TENANT_IDLE_TTL would never elapse.
func TestRebuild_DoesNotRefreshIdleClock(t *testing.T) {
	repo := newTestRepo(t)
	g := New(repo, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	st := g.storesForTenant("tenant-dormant")
	stale := staleNanos()
	st.lastAccess.Store(stale)

	// Rebuild walks known tenants even when the DB has no rows for them.
	g.rebuildAllTenantsFromDB(context.Background())

	if got := st.lastAccess.Load(); got != stale {
		t.Fatalf("DB rebuild refreshed lastAccess (%d → %d) — dormant tenant would never be evicted", stale, got)
	}
	if got := g.evictIdleTenants(); got != 1 {
		t.Fatalf("evictIdleTenants after rebuild = %d, want 1", got)
	}
}

// TestStoresForTenant_RefreshesLastAccess proves the fast path (slice already
// resident) refreshes the idle clock on every call.
func TestStoresForTenant_RefreshesLastAccess(t *testing.T) {
	g := New(nil, nil, nil, DefaultConfig())
	t.Cleanup(g.Stop)

	st := g.storesForTenant("tenant-c")
	st.lastAccess.Store(staleNanos())

	g.storesForTenant("tenant-c") // fast path
	if age := time.Since(time.Unix(0, st.lastAccess.Load())); age > time.Minute {
		t.Fatalf("lastAccess not refreshed on fast path: %v old", age)
	}
}
