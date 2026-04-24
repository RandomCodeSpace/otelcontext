package graphrag

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// TestGraphRAG_TenantIsolation_InMemoryStores ingests overlapping span data
// for two distinct tenants and asserts each tenant's query surface sees
// only its own services and traces. This is the core invariant of RAN-37:
// no in-memory state may leak across tenants.
func TestGraphRAG_TenantIsolation_InMemoryStores(t *testing.T) {
	g := newTestGraphRAG(t)

	now := time.Now()
	mk := func(tenant, service, traceID, spanID string) storage.Span {
		return storage.Span{
			TenantID:      tenant,
			TraceID:       traceID,
			SpanID:        spanID,
			OperationName: "/op",
			ServiceName:   service,
			Status:        "STATUS_CODE_OK",
			StartTime:     now,
			EndTime:       now.Add(time.Millisecond),
			Duration:      1000,
		}
	}

	// Tenant A: service "orders" under trace t-a-1.
	g.OnSpanIngested(mk("tenant-a", "orders", "t-a-1", "s-a-1"))
	// Tenant B: service "payments" under trace t-b-1 — different service so we
	// can prove A never sees B's service and vice versa.
	g.OnSpanIngested(mk("tenant-b", "payments", "t-b-1", "s-b-1"))

	ctxA := storage.WithTenantContext(context.Background(), "tenant-a")
	ctxB := storage.WithTenantContext(context.Background(), "tenant-b")

	// Poll briefly for the async event workers to finish both tenants.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mapA := g.ServiceMap(ctxA, 0)
		mapB := g.ServiceMap(ctxB, 0)
		if len(mapA) >= 1 && len(mapB) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mapA := g.ServiceMap(ctxA, 0)
	mapB := g.ServiceMap(ctxB, 0)

	if len(mapA) != 1 || mapA[0].Service.Name != "orders" {
		t.Fatalf("tenant-a ServiceMap = %v, want only [orders]", mapA)
	}
	if len(mapB) != 1 || mapB[0].Service.Name != "payments" {
		t.Fatalf("tenant-b ServiceMap = %v, want only [payments]", mapB)
	}

	// Dependency chain lookups must also stay inside the caller's tenant.
	if chain := g.DependencyChain(ctxA, "t-b-1"); len(chain) != 0 {
		t.Fatalf("tenant-a saw tenant-b trace t-b-1: %+v", chain)
	}
	if chain := g.DependencyChain(ctxB, "t-a-1"); len(chain) != 0 {
		t.Fatalf("tenant-b saw tenant-a trace t-a-1: %+v", chain)
	}
}

// TestGraphRAG_StoresFor_EmptyContextCollapsesToDefault confirms that a
// query without any tenant context lands on the DefaultTenantID slice —
// matching storage.TenantFromContext semantics so single-tenant installs
// behave as before.
func TestGraphRAG_StoresFor_EmptyContextCollapsesToDefault(t *testing.T) {
	g := newTestGraphRAG(t)

	viaEmpty := g.storesFor(context.Background())
	viaDefault := g.storesForTenant(storage.DefaultTenantID)
	if viaEmpty != viaDefault {
		t.Fatalf("empty-context lookup returned a different slice than DefaultTenantID")
	}
}
