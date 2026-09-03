package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/api/views"
	"github.com/RandomCodeSpace/otelcontext/internal/latency"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

type fakeTopologyProvider struct {
	source   topology.Source
	identity topology.Identity
	snapshot topology.Snapshot
	err      error
	hosts    topology.HostProjection
}

func (f *fakeTopologyProvider) Source() topology.Source { return f.source }

func (f *fakeTopologyProvider) Hosts(context.Context) topology.HostProjection { return f.hosts }

func (f *fakeTopologyProvider) Identity(context.Context) topology.Identity { return f.identity }

func (f *fakeTopologyProvider) Snapshot(context.Context, topology.Query) (topology.Snapshot, error) {
	return f.snapshot, f.err
}

func TestAggregateTopologyProviderOwnsBothRESTGraphs(t *testing.T) {
	provider := &fakeTopologyProvider{
		source:   topology.SourceAggregate,
		identity: topology.Identity{Epoch: "boot-a", Revision: 7},
		snapshot: topology.Snapshot{
			Nodes: []topology.Node{{Name: "gateway", P99LatencyMs: 1000, LatencyProvenance: &latency.Provenance{P99: &latency.Percentile{Status: latency.StatusApproximate, Method: latency.MethodDDSketch, SampleCount: 1000}}}, {Name: "aggregate-payments"}},
			Edges: []topology.Edge{{Source: "gateway", Target: "aggregate-payments", CallCount: 4}},
			Meta: topology.Metadata{
				Source:   topology.SourceAggregate,
				Coverage: "full",
				Epoch:    "boot-a",
				Revision: 7,
			},
		},
	}
	server := NewServer(nil, nil, nil, nil)
	server.SetTopologyProvider(provider)

	serviceReq := httptest.NewRequest("GET", "/api/metrics/service-map", nil)
	serviceRec := httptest.NewRecorder()
	server.handleGetServiceMapMetrics(serviceRec, serviceReq)
	if serviceRec.Code != 200 {
		t.Fatalf("service map status = %d, body=%s", serviceRec.Code, serviceRec.Body.String())
	}
	var serviceMap views.ServiceMapMetrics
	if err := json.Unmarshal(serviceRec.Body.Bytes(), &serviceMap); err != nil {
		t.Fatalf("decode service map: %v", err)
	}
	if len(serviceMap.Edges) != 1 || serviceMap.Edges[0].Target != "aggregate-payments" {
		t.Fatalf("service map mixed topology owners: %+v", serviceMap.Edges)
	}
	if serviceMap.Epoch != "boot-a" || serviceMap.Revision != 7 {
		t.Fatalf("service map identity = %q/%d, want boot-a/7", serviceMap.Epoch, serviceMap.Revision)
	}
	if serviceMap.Nodes[0].P99LatencyMs != 1000 || serviceMap.Nodes[0].LatencyProvenance == nil || serviceMap.Nodes[0].LatencyProvenance.P99.Status != latency.StatusApproximate {
		t.Fatalf("service map latency = %+v", serviceMap.Nodes[0])
	}

	systemReq := httptest.NewRequest("GET", "/api/system/graph", nil)
	systemRec := httptest.NewRecorder()
	server.handleGetSystemGraph(systemRec, systemReq)
	if systemRec.Code != 200 {
		t.Fatalf("system graph status = %d, body=%s", systemRec.Code, systemRec.Body.String())
	}
	var systemGraph SystemGraphResponse
	if err := json.Unmarshal(systemRec.Body.Bytes(), &systemGraph); err != nil {
		t.Fatalf("decode system graph: %v", err)
	}
	if len(systemGraph.Edges) != 1 || systemGraph.Edges[0].Target != "aggregate-payments" {
		t.Fatalf("system graph mixed topology owners: %+v", systemGraph.Edges)
	}
	if systemGraph.Epoch != "boot-a" || systemGraph.Revision != 7 {
		t.Fatalf("system graph identity = %q/%d, want boot-a/7", systemGraph.Epoch, systemGraph.Revision)
	}
	if systemGraph.Nodes[0].Metrics.P99LatencyMs != 1000 || systemGraph.Nodes[0].Metrics.LatencyProvenance == nil || systemGraph.Nodes[0].Metrics.LatencyProvenance.P99.SampleCount != 1000 {
		t.Fatalf("system graph latency = %+v", systemGraph.Nodes[0].Metrics)
	}
}

func TestAggregateTopologyCacheIdentityAndEmptyReplacement(t *testing.T) {
	provider := &fakeTopologyProvider{
		source:   topology.SourceAggregate,
		identity: topology.Identity{Epoch: "boot-a", Revision: 1},
		snapshot: topology.Snapshot{
			Nodes: []topology.Node{{Name: "stale"}},
			Edges: []topology.Edge{},
			Meta:  topology.Metadata{Source: topology.SourceAggregate, Epoch: "boot-a", Revision: 1},
		},
	}
	server := NewServer(nil, nil, nil, nil)
	server.SetTopologyProvider(provider)
	req := httptest.NewRequest("GET", "/api/system/graph", nil)

	first := httptest.NewRecorder()
	server.handleGetSystemGraph(first, req)
	if first.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first cache result = %q, want MISS", first.Header().Get("X-Cache"))
	}

	provider.identity.Revision = 2
	provider.snapshot = topology.Snapshot{
		Nodes: []topology.Node{},
		Edges: []topology.Edge{},
		Meta:  topology.Metadata{Source: topology.SourceAggregate, Epoch: "boot-a", Revision: 2},
	}
	second := httptest.NewRecorder()
	server.handleGetSystemGraph(second, req)
	if second.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("moved identity reused stale cache: %q", second.Header().Get("X-Cache"))
	}
	var got SystemGraphResponse
	if err := json.Unmarshal(second.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode empty graph: %v", err)
	}
	if got.Nodes == nil || got.Edges == nil || len(got.Nodes) != 0 || len(got.Edges) != 0 {
		t.Fatalf("empty replacement = nodes:%v edges:%v", got.Nodes, got.Edges)
	}
}
