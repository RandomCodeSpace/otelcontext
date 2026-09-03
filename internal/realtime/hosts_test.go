package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

// hostsProvider is a provider whose host projection a test fixes by hand.
type hostsProvider struct {
	fakeRefreshProvider
	source topology.Source
	nodes  []topology.Node
	hosts  topology.HostProjection
}

func (p *hostsProvider) Source() topology.Source                       { return p.source }
func (p *hostsProvider) Hosts(context.Context) topology.HostProjection { return p.hosts }
func (p *hostsProvider) Snapshot(context.Context, topology.Query) (topology.Snapshot, error) {
	return topology.Snapshot{Nodes: p.nodes, Edges: []topology.Edge{}, Meta: topology.Metadata{Source: p.source}}, nil
}

func fixtureHosts() topology.HostProjection {
	return topology.ProjectHosts([]topology.ResourceEntry{
		{ResourceKey: topology.ResourceKey{Tenant: storage.DefaultTenantID, Service: "gateway", Host: "node-a"}, Signals: topology.SignalTraces},
		{ResourceKey: topology.ResourceKey{Tenant: storage.DefaultTenantID, Service: "gateway", Host: "node-b"}, Signals: topology.SignalTraces},
	})
}

// TestLegacySnapshotStampsHostsOnServiceMap: the legacy live_snapshot keeps
// its repository-sourced nodes and gains kind/host_count/hosts from the
// provider's registry projection.
func TestLegacySnapshotStampsHostsOnServiceMap(t *testing.T) {
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	if err := repo.BatchCreateSpans([]storage.Span{
		{TenantID: storage.DefaultTenantID, TraceID: "t", SpanID: "p", ServiceName: "gateway", OperationName: "op", StartTime: now, EndTime: now.Add(time.Millisecond), Duration: 1000, Status: "STATUS_CODE_UNSET"},
		{TenantID: storage.DefaultTenantID, TraceID: "t", SpanID: "c", ParentSpanID: "p", ServiceName: "payments", OperationName: "op", StartTime: now, EndTime: now.Add(time.Millisecond), Duration: 1000, Status: "STATUS_CODE_UNSET"},
	}); err != nil {
		t.Fatalf("seed spans: %v", err)
	}

	hub := NewEventHub(repo, nil, nil)
	hub.SetTopologyProvider(&hostsProvider{source: topology.SourceLegacy, hosts: fixtureHosts()})
	snap := hub.computeSnapshot(storage.WithTenantContext(context.Background(), storage.DefaultTenantID), "")
	if snap == nil || snap.ServiceMap == nil {
		t.Fatalf("snapshot = %+v", snap)
	}
	got := ""
	for _, node := range snap.ServiceMap.Nodes {
		got += fmt.Sprintf("%s:%s:%d:%v ", node.Name, node.Kind, node.HostCount, node.Hosts)
	}
	if want := "gateway:service:2:[node-a node-b] payments:service:0:[] "; got != want {
		t.Fatalf("nodes = %q, want %q", got, want)
	}
	raw, _ := json.Marshal(snap.ServiceMap.Nodes[0])
	if want := `{"name":"gateway","total_traces":1,"error_count":0,"avg_latency_ms":1,"p99_latency_ms":1,"latency_provenance":{"p99":{"status":"measured","method":"ordered_rank",`; len(raw) < len(want) || string(raw[:len(want)]) != want {
		t.Fatalf("node prefix changed: %s", raw)
	}
	if suffix := `,"kind":"service","host_count":2,"hosts":["node-a","node-b"]}`; string(raw[len(raw)-len(suffix):]) != suffix {
		t.Fatalf("node suffix = %s, want %s", raw, suffix)
	}
}

// TestAggregateSnapshotCarriesProviderHosts: the engine publisher copies the
// provider's stamped host fields onto live_snapshot nodes.
func TestAggregateSnapshotCarriesProviderHosts(t *testing.T) {
	engine, err := aggregate.NewEngine(aggregate.EngineConfig{Mode: aggregate.ModeAggregate})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	provider := &hostsProvider{source: topology.SourceAggregate, nodes: []topology.Node{
		{Name: "gateway", Kind: "service", HostCount: 2, Hosts: []string{"node-a", "node-b"}},
		{Name: "host/node-c", Kind: "host", HostCount: 1, Hosts: []string{"node-c"}},
	}}
	provider.epoch.Store("epoch-1")
	pub := NewEnginePublisher(EnginePublisherConfig{Engine: engine, Topology: provider})
	if pub == nil {
		t.Fatal("publisher refused an aggregate provider")
	}
	snap := pub.Snapshot(context.Background(), "")
	if snap == nil || snap.ServiceMap == nil || len(snap.ServiceMap.Nodes) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if n := snap.ServiceMap.Nodes[1]; n.Kind != "host" || n.HostCount != 1 || fmt.Sprint(n.Hosts) != "[node-c]" {
		t.Fatalf("host node = %+v", n)
	}
	if n := snap.ServiceMap.Nodes[0]; n.Kind != "service" || fmt.Sprint(n.Hosts) != "[node-a node-b]" {
		t.Fatalf("service node = %+v", n)
	}
}
