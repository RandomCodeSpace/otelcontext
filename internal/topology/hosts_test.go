package topology

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// hostFixture registers two tenants: acme has gateway on node-a and node-b,
// a host entity on node-c and a host-less checkout; beta has one host.
func hostFixture(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(nil)
	now := time.Unix(1_700_000_000, 0).UTC()
	for _, e := range []struct {
		tenant, service, host string
		signal                Signal
		at                    time.Time
	}{
		{"acme", "gateway", "node-a", SignalTraces, now},
		{"acme", "gateway", "node-b", SignalTraces, now.Add(time.Minute)},
		{"acme", "payments", "node-b", SignalLogs, now},
		{"acme", HostPrefix + "node-c", "node-c", SignalMetrics, now},
		{"acme", "checkout", "", SignalTraces, now},
		{"beta", "gateway", "node-z", SignalTraces, now},
	} {
		if !r.Register(e.tenant, e.service, e.host, "", "", e.signal, e.at) {
			t.Fatalf("register %+v refused", e)
		}
	}
	return r
}

func TestProjectHostsSortsScopesAndExcludesHostEntitiesFromServices(t *testing.T) {
	r := hostFixture(t)
	reader := &HostReader{}
	reader.SetRegistry(r)
	ctx := storage.WithTenantContext(context.Background(), "acme")
	p := reader.Hosts(ctx)

	got := fmt.Sprint(p.Hosts)
	want := "[{node-a 1 [gateway] 2023-11-14 22:13:20 +0000 UTC [traces]} {node-b 2 [gateway payments] 2023-11-14 22:14:20 +0000 UTC [logs traces]} {node-c 0 [] 2023-11-14 22:13:20 +0000 UTC [metrics]}]"
	if got != want {
		t.Fatalf("hosts =\n%s\nwant\n%s", got, want)
	}
	if count, hosts := p.ServiceHosts("gateway"); count != 2 || fmt.Sprint(hosts) != "[node-a node-b]" {
		t.Fatalf("gateway hosts = %d %v", count, hosts)
	}
	if count, hosts := p.ServiceHosts(HostPrefix + "node-c"); count != 1 || fmt.Sprint(hosts) != "[node-c]" {
		t.Fatalf("host entity hosts = %d %v", count, hosts)
	}
	if count, hosts := p.ServiceHosts("checkout"); count != 0 || hosts == nil || len(hosts) != 0 {
		t.Fatalf("host-less service = %d %v", count, hosts)
	}
	if host, ok := p.Host("node-b"); !ok || host.ServiceCount != 2 {
		t.Fatalf("Host(node-b) = %+v %v", host, ok)
	}
	if _, ok := p.Host("node-z"); ok {
		t.Fatal("beta's host leaked into acme")
	}
	if beta := reader.Hosts(storage.WithTenantContext(context.Background(), "beta")); len(beta.Hosts) != 1 || beta.Hosts[0].Name != "node-z" {
		t.Fatalf("beta hosts = %+v", beta.Hosts)
	}
	if empty := (&HostReader{}).Hosts(ctx); empty.Hosts == nil || len(empty.Hosts) != 0 {
		t.Fatalf("registry-less reader = %+v", empty.Hosts)
	}
}

func TestServiceHostsAreBoundedWhileCountIsNot(t *testing.T) {
	r := NewRegistry(nil)
	now := time.Now()
	for i := 0; i < MaxHostsPerNode+5; i++ {
		r.Register("acme", "gateway", fmt.Sprintf("node-%03d", i), "", "", SignalTraces, now)
	}
	p := ProjectHosts(r.TenantSnapshot("acme"))
	count, hosts := p.ServiceHosts("gateway")
	if count != MaxHostsPerNode+5 || len(hosts) != MaxHostsPerNode || hosts[0] != "node-000" || hosts[MaxHostsPerNode-1] != "node-019" {
		t.Fatalf("count=%d hosts=%v", count, hosts)
	}
}

func TestLegacyProviderStampsHostsAndDropsHostEntityEdges(t *testing.T) {
	repo := fakeLegacyRepository{result: &storage.ServiceMapMetrics{
		Nodes: []storage.ServiceMapNode{{Name: "gateway", TotalTraces: 4}, {Name: HostPrefix + "node-c"}, {Name: "checkout"}},
		Edges: []storage.ServiceMapEdge{{Source: "gateway", Target: "checkout", CallCount: 3}, {Source: "gateway", Target: HostPrefix + "node-c", CallCount: 1}},
	}}
	provider, err := NewLegacyProvider(repo, nil, nil)
	if err != nil {
		t.Fatalf("NewLegacyProvider: %v", err)
	}
	provider.SetRegistry(hostFixture(t))
	ctx := storage.WithTenantContext(context.Background(), "acme")
	snapshot, err := provider.Snapshot(ctx, Query{Start: time.Unix(100, 0), End: time.Unix(200, 0)})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := ""
	for _, node := range snapshot.Nodes {
		got += fmt.Sprintf("%s:%s:%d:%v ", node.Name, node.Kind, node.HostCount, node.Hosts)
	}
	if want := "checkout:service:0:[] gateway:service:2:[node-a node-b] host/node-c:host:1:[node-c] "; got != want {
		t.Fatalf("nodes = %q, want %q", got, want)
	}
	if len(snapshot.Edges) != 1 || snapshot.Edges[0].Target != "checkout" {
		t.Fatalf("edges = %+v, want only gateway -> checkout", snapshot.Edges)
	}
}

func TestAggregateProviderReadsTheSameRegistry(t *testing.T) {
	engine, err := aggregate.NewEngine(aggregate.EngineConfig{Mode: aggregate.ModeAggregate})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	provider, err := NewAggregateProvider(engine)
	if err != nil {
		t.Fatalf("NewAggregateProvider: %v", err)
	}
	registry := hostFixture(t)
	provider.SetRegistry(registry)
	ctx := storage.WithTenantContext(context.Background(), "acme")
	legacy, err := NewLegacyProvider(fakeLegacyRepository{}, nil, nil)
	if err != nil {
		t.Fatalf("NewLegacyProvider: %v", err)
	}
	legacy.SetRegistry(registry)
	if a, l := fmt.Sprint(provider.Hosts(ctx).Hosts), fmt.Sprint(legacy.Hosts(ctx).Hosts); a != l || len(provider.Hosts(ctx).Hosts) != 3 {
		t.Fatalf("aggregate hosts %s != legacy hosts %s", a, l)
	}
	if _, err := provider.Snapshot(ctx, Query{}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
}
