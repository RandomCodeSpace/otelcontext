package topology

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

func TestInstanceQueryScopeBoundsAndRestart(t *testing.T) {
	g := newTestInstanceGraph(t)
	ctx := context.Background()
	at := time.Now().Truncate(GraphWindow)
	var devIDs []string
	for _, tenant := range []string{"alpha", "beta"} {
		for _, env := range []string{"dev", "test", ""} {
			for i, name := range []string{"orders", "payments", "payments"} {
				id := g.Register(InstanceIdentity{Tenant: tenant, Environment: env, Service: name, Instance: fmt.Sprint(i), Host: fmt.Sprintf("host-%d", i)}, SignalTraces, at)
				if tenant == "alpha" && env == "dev" {
					devIDs = append(devIDs, id)
				}
				g.Observe(id, "11111111111111111111111111111111", fmt.Sprintf("%016x", len(g.entities)), "", at, 3000, true, false)
			}
		}
	}
	// Deliberate call between the same service's two copies.
	g.Observe(devIDs[1], "22222222222222222222222222222222", "1111111111111111", "", at, 4000, true, false)
	g.Observe(devIDs[2], "22222222222222222222222222222222", "2222222222222222", "1111111111111111", at, 5000, true, true)
	env := "dev"
	q := InstanceQuery{Start: at, End: at.Add(GraphWindow), Environment: &env, Mode: "instances", Hops: 1, Limit: 40}
	before, err := g.Snapshot(ctx, "alpha", q)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Nodes) != 3 || len(before.Edges) != 1 || before.Edges[0].Calls != 1 || before.Edges[0].Errors != 1 {
		t.Fatalf("wrong scoped graph: %+v", before)
	}
	for _, n := range before.Nodes {
		if n.Environment != "dev" {
			t.Fatal("environment leaked")
		}
	}
	if err := g.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewInstanceGraph(ctx, g.db, g.retention)
	if err != nil {
		t.Fatal(err)
	}
	after, err := reopened.Snapshot(ctx, "alpha", q)
	if err != nil || !reflect.DeepEqual(before.Nodes, after.Nodes) || !reflect.DeepEqual(before.Edges, after.Edges) {
		t.Fatalf("restart changed graph: %v\nbefore=%+v\nafter=%+v", err, before, after)
	}
	q.Mode = "services"
	grouped, err := g.Snapshot(ctx, "alpha", q)
	if err != nil || len(grouped.Nodes) != 2 || len(grouped.Edges) != 1 || grouped.Edges[0].Source != grouped.Edges[0].Target {
		t.Fatalf("same-service call must survive as a grouped self-edge: %+v %v", grouped, err)
	}
	q.ServiceID = serviceID(g.entities[devIDs[1]].row)
	focused, err := g.Snapshot(ctx, "alpha", q)
	if err != nil || len(focused.Nodes) != 1 || focused.Nodes[0].Name != "payments" {
		t.Fatalf("scope not applied: %+v %v", focused, err)
	}
	q.ServiceID, q.Limit = "", 1
	capped, err := g.Snapshot(ctx, "alpha", q)
	if err != nil || len(capped.Nodes) != 1 || !capped.Meta.Truncated {
		t.Fatalf("node cap: %+v %v", capped, err)
	}
	for _, e := range capped.Edges {
		if e.Source != capped.Nodes[0].ID || e.Target != capped.Nodes[0].ID {
			t.Fatal("dangling edge after truncation")
		}
	}
	catalog := g.Catalog("alpha", at, at.Add(GraphWindow))
	if len(catalog.Services) != 6 || catalog.Instances != 9 || len(catalog.Environments) != 3 {
		t.Fatalf("catalog scopes: %+v", catalog)
	}
	unknown := ""
	q.Environment, q.Limit = &unknown, 40
	snap, err := g.Snapshot(ctx, "alpha", q)
	if err != nil || len(snap.Nodes) != 2 || snap.Nodes[0].Environment != "" {
		t.Fatal("Unknown environment merged with named environments", err)
	}
}

func TestInstanceGraphRetentionAndQueryValidation(t *testing.T) {
	g := newTestInstanceGraph(t)
	ctx := context.Background()
	at := time.Now().Truncate(GraphWindow)
	id := g.Register(InstanceIdentity{Tenant: "alpha", Service: "old", Instance: "one"}, SignalTraces, at)
	g.Observe(id, "11111111111111111111111111111111", "1111111111111111", "", at, 10, true, false)
	if err := g.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return at.Add(8 * 24 * time.Hour) }
	q := InstanceQuery{Start: at, End: at.Add(time.Hour), Mode: "services", Hops: 1, Limit: 40}
	result, err := g.Snapshot(ctx, "alpha", q)
	if err != nil || len(result.Nodes) != 0 {
		t.Fatal("expired data visible before physical purge", err)
	}
	if err := g.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var count int64
	g.db.Model(&storage.TopologyBucket{}).Count(&count)
	if count != 0 || len(g.entities) != 0 {
		t.Fatal("expired topology not removed")
	}
	q.End = at.Add(25 * time.Hour)
	if _, err := g.Snapshot(ctx, "alpha", q); err == nil {
		t.Fatal("unbounded historical query allowed")
	}
}
