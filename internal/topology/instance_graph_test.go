package topology

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

func newTestInstanceGraph(t *testing.T) *InstanceGraph {
	t.Helper()
	db := newRegistryDB(t)
	if err := db.AutoMigrate(&storage.TopologyEntity{}, &storage.TopologyBucket{}); err != nil {
		t.Fatal(err)
	}
	g, err := NewInstanceGraph(context.Background(), db, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestInstanceIdentityScopesAndFallback(t *testing.T) {
	now := time.Now()
	a := InstanceIdentity{Tenant: "alpha", Environment: "dev", Namespace: "web", Service: "orders", Instance: "one", Host: "host-a"}
	base, _ := a.entity(now)
	for _, field := range []string{"tenant", "environment", "namespace", "service", "instance"} {
		b := a
		switch field {
		case "tenant":
			b.Tenant = "beta"
		case "environment":
			b.Environment = "test"
		case "namespace":
			b.Namespace = "jobs"
		case "service":
			b.Service = "payments"
		case "instance":
			b.Instance = "two"
		}
		got, _ := b.entity(now)
		if got.ID == base.ID {
			t.Fatalf("%s merged distinct identities", field)
		}
	}
	a.Host = "host-b"
	if moved, _ := a.entity(now); moved.ID != base.ID {
		t.Fatal("explicit instance identity depends on placement")
	}
	a.Instance = ""
	row, _ := a.entity(now)
	if row.Identity != "host_only" {
		t.Fatal("host-only fallback must disclose ambiguity")
	}
	a.Workload = "container"
	row, _ = a.entity(now)
	if row.Identity != "host_workload" {
		t.Fatal("missing workload fallback")
	}
	a.Host, a.Workload = "", ""
	row, _ = a.entity(now)
	if row.Identity != "unresolved" || row.Environment != "dev" {
		t.Fatal("unknown identity lost its explicit environment")
	}
	a.Service = strings.Repeat("x", 256)
	if _, ok := a.entity(now); ok {
		t.Fatal("unbounded identity accepted")
	}
}

func TestInstanceGraphCorrelationPersistenceAndRetry(t *testing.T) {
	g := newTestInstanceGraph(t)
	ctx := context.Background()
	at := time.Now().Truncate(GraphWindow).Add(time.Second)
	a := g.Register(InstanceIdentity{Tenant: "alpha", Environment: "dev", Service: "same", Instance: "a"}, SignalTraces, at)
	b := g.Register(InstanceIdentity{Tenant: "alpha", Environment: "dev", Service: "same", Instance: "b"}, SignalTraces, at)
	otherTenant := g.Register(InstanceIdentity{Tenant: "beta", Environment: "dev", Service: "same", Instance: "a"}, SignalTraces, at)
	trace, parent, child := strings.Repeat("1", 32), strings.Repeat("2", 16), strings.Repeat("3", 16)
	// Reversed arrival, plus identical span ID in a different trace and tenant.
	g.Observe(b, trace, child, parent, at, 14000, true, true)
	g.Observe(otherTenant, trace, parent, "", at, 500, true, false)
	g.Observe(a, strings.Repeat("4", 32), parent, "", at, 700, true, false)
	g.Observe(a, trace, parent, "", at, 1000, true, false)
	g.Observe(b, trace, child, parent, at, 14000, true, true)
	var edge storage.TopologyEntity
	for _, e := range g.entities {
		if e.row.Source != "" {
			if edge.ID != "" {
				t.Fatal("trace/tenant collision created an extra call")
			}
			edge = e.row
		}
	}
	if edge.Source != a || edge.Target != b {
		t.Fatalf("same-name instance edge missing: %+v", edge)
	}
	if err := g.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewInstanceGraph(ctx, g.db, g.retention)
	if err != nil || reopened.entities[edge.ID] == nil {
		t.Fatalf("restart: %v", err)
	}
	var got storage.TopologyBucket
	if err := g.db.Where("entity_id = ?", edge.ID).First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 || got.Errors != 1 || got.DurationMicros != 14000 {
		t.Fatalf("edge counts: %+v", got)
	}
	// A failed flush must keep pending data, and retry must apply it once.
	g.Observe(b, trace, strings.Repeat("5", 16), parent, at, 3000, true, false)
	if err := g.db.Exec("PRAGMA query_only=ON").Error; err != nil {
		t.Fatal(err)
	}
	if err := g.Flush(ctx); err == nil || len(g.pending) == 0 {
		t.Fatal("failed flush discarded data")
	}
	if err := g.db.Exec("PRAGMA query_only=OFF").Error; err != nil {
		t.Fatal(err)
	}
	if err := g.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	got = storage.TopologyBucket{}
	if err := g.db.Where("entity_id = ?", edge.ID).First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.Requests != 2 || got.Errors != 1 || got.DurationMicros != 17000 {
		t.Fatalf("retry counters: %+v", got)
	}
}

func TestInstanceGraphPendingBoundsAndCancellation(t *testing.T) {
	g := newTestInstanceGraph(t)
	at := time.Now()
	for i := range graphMaxPending {
		g.pending[graphBucketKey{entity: "full", start: int64(i)}] = storage.TopologyBucket{}
	}
	g.mu.Lock()
	g.add("new", at, storage.TopologyBucket{Spans: 1})
	g.mu.Unlock()
	if len(g.pending) != graphMaxPending || g.dropped != 1 || g.drops[DropPendingCap] != 1 {
		t.Fatal("pending cardinality not bounded")
	}
	if err := g.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { <-g.gate }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Flush(ctx); err != context.Canceled {
		t.Fatalf("gate did not honor cancellation: %v", err)
	}
}

func TestInstanceGraphObservationDuringFlushIsNotLostOrDoubleCounted(t *testing.T) {
	g := newTestInstanceGraph(t)
	at := time.Now().Truncate(GraphWindow).Add(time.Second)
	identity := InstanceIdentity{Tenant: "alpha", Service: "orders", Instance: "one", Host: "old-host"}
	node := g.Register(identity, SignalTraces, at)
	trace := strings.Repeat("1", 32)
	g.Observe(node, trace, strings.Repeat("2", 16), "", at, 1000, true, false)

	// Pause the real flush after it has copied pending values and entered SQL.
	// Ingestion must remain available while the database write is in progress.
	entered, release := make(chan struct{}), make(chan struct{})
	if err := g.db.Callback().Create().Before("gorm:create").Register("test:pause_topology_flush", func(tx *gorm.DB) {
		if tx.Statement.Table == "topology_entities" {
			close(entered)
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	flushed := make(chan error, 1)
	go func() { flushed <- g.Flush(context.Background()) }()
	<-entered
	identity.Host = "new-host"
	g.Register(identity, SignalTraces, at.Add(time.Second))
	g.Observe(node, trace, strings.Repeat("3", 16), "", at, 3000, true, true)
	close(release)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	if err := g.db.Callback().Create().Remove("test:pause_topology_flush"); err != nil {
		t.Fatal(err)
	}
	q := InstanceQuery{Start: at.Add(-time.Second), End: at.Add(time.Minute), Mode: "instances", Hops: 1, Limit: 40}
	check := func(graph *InstanceGraph) {
		t.Helper()
		result, err := graph.Snapshot(context.Background(), "alpha", q)
		if err != nil || len(result.Nodes) != 1 {
			t.Fatalf("snapshot nodes=%d: %v", len(result.Nodes), err)
		}
		n := result.Nodes[0]
		if n.Requests != 2 || n.Spans != 2 || n.Errors != 1 || n.AvgLatencyMS != 2 ||
			len(n.Hosts) != 1 || n.Hosts[0] != "new-host" {
			t.Fatalf("pending/durable snapshot: %+v", n)
		}
	}
	check(g)
	if err := g.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewInstanceGraph(context.Background(), g.db, g.retention)
	if err != nil {
		t.Fatal(err)
	}
	check(reopened)
}

func TestInstanceGraphEvictsStaleEntitiesAtBound(t *testing.T) {
	g := newTestInstanceGraph(t)
	now := time.Now()
	g.now = func() time.Time { return now }
	fill := func(lastSeen time.Time, dirty bool) {
		clear(g.entities)
		for i := range graphMaxEntities {
			id := fmt.Sprintf("old-%05d", i)
			g.entities[id] = &graphEntity{row: storage.TopologyEntity{ID: id, TenantID: "alpha", LastSeen: lastSeen.Unix() + int64(i)}, dirty: dirty}
		}
	}
	// A live set at the bound is refused, never churned.
	fill(now, false)
	if id := g.Register(InstanceIdentity{Tenant: "alpha", Service: "orders", Instance: "fresh"}, SignalTraces, now); id != "" || g.drops[DropEntityCap] != 1 {
		t.Fatalf("live entities evicted: id=%q drops=%v", id, g.drops)
	}
	// Unflushed entities are never evicted either: their rows are not durable yet.
	fill(now.Add(-time.Hour), true)
	g.lastEvict = time.Time{}
	if id := g.Register(InstanceIdentity{Tenant: "alpha", Service: "orders", Instance: "fresh"}, SignalTraces, now); id != "" {
		t.Fatal("dirty entity evicted")
	}
	// Idle flushed entities make room, oldest first.
	fill(now.Add(-time.Hour), false)
	g.lastEvict = time.Time{}
	id := g.Register(InstanceIdentity{Tenant: "alpha", Service: "orders", Instance: "fresh"}, SignalTraces, now)
	if id == "" || len(g.entities) != graphMaxEntities-graphEvictBatch+1 {
		t.Fatalf("stale entities not evicted: id=%q entities=%d", id, len(g.entities))
	}
	if g.entities["old-00000"] != nil || g.entities[fmt.Sprintf("old-%05d", graphEvictBatch)] == nil {
		t.Fatal("eviction did not take the least recently seen first")
	}
}

func TestInstanceGraphReloadKeepsMostRecentEntities(t *testing.T) {
	g := newTestInstanceGraph(t)
	now := time.Now().Unix()
	rows := make([]storage.TopologyEntity, graphMaxEntities+5)
	for i := range rows {
		rows[i] = storage.TopologyEntity{ID: fmt.Sprintf("e-%05d", i), TenantID: "alpha", LastSeen: now - int64(len(rows)-i)}
	}
	if err := g.db.CreateInBatches(rows, 500).Error; err != nil {
		t.Fatal(err)
	}
	reopened, err := NewInstanceGraph(context.Background(), g.db, g.retention)
	if err != nil {
		t.Fatalf("retained rows past the bound must not refuse startup: %v", err)
	}
	if len(reopened.entities) != graphMaxEntities || reopened.entities["e-00004"] != nil || reopened.entities["e-00005"] == nil {
		t.Fatalf("reload kept %d entities, not the most recent", len(reopened.entities))
	}
}
