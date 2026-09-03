package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

// hostFixtureProjection is the projection of a fixed registry: gateway on
// node-a and node-b, a host entity on node-c.
func hostFixtureProjection() topology.HostProjection {
	seen := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	return topology.ProjectHosts([]topology.ResourceEntry{
		{ResourceKey: topology.ResourceKey{Tenant: "default", Service: "gateway", Host: "node-a"}, Signals: topology.SignalTraces, LastSeen: seen},
		{ResourceKey: topology.ResourceKey{Tenant: "default", Service: "gateway", Host: "node-b"}, Signals: topology.SignalTraces | topology.SignalLogs, LastSeen: seen.Add(time.Minute)},
		{ResourceKey: topology.ResourceKey{Tenant: "default", Service: "host/node-c", Host: "node-c"}, Signals: topology.SignalMetrics, LastSeen: seen},
	})
}

// hostFixtureSnapshot is what a provider hands the handlers once it has
// stamped hostFixtureProjection onto its nodes.
func hostFixtureSnapshot() topology.Snapshot {
	return topology.Snapshot{
		Nodes: []topology.Node{
			{Name: "gateway", Kind: "service", HostCount: 2, Hosts: []string{"node-a", "node-b"}},
			{Name: "host/node-c", Kind: "host", HostCount: 1, Hosts: []string{"node-c"}},
			{Name: "payments", Kind: "service", Hosts: []string{}},
		},
		Edges: []topology.Edge{{Source: "gateway", Target: "payments", CallCount: 4}},
		Meta:  topology.Metadata{Source: topology.SourceLegacy},
	}
}

func newHostsTestServer() *Server {
	server := NewServer(nil, nil, nil, nil)
	server.SetTopologyProvider(&fakeTopologyProvider{source: topology.SourceLegacy, snapshot: hostFixtureSnapshot(), hosts: hostFixtureProjection()})
	return server
}

func get(t *testing.T, handler http.HandlerFunc, target string, pathValues map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestHostsEndpointsGolden(t *testing.T) {
	server := newHostsTestServer()

	list := get(t, server.handleGetHosts, "/api/hosts", nil)
	wantList := `[{"name":"node-a","service_count":1,"services":["gateway"],"last_seen":"2026-09-03T08:00:00Z","signals":["traces"]},` +
		`{"name":"node-b","service_count":1,"services":["gateway"],"last_seen":"2026-09-03T08:01:00Z","signals":["logs","traces"]},` +
		`{"name":"node-c","service_count":0,"services":[],"last_seen":"2026-09-03T08:00:00Z","signals":["metrics"]}]` + "\n"
	if list.Code != http.StatusOK || list.Body.String() != wantList {
		t.Fatalf("GET /api/hosts = %d\n%s\nwant\n%s", list.Code, list.Body.String(), wantList)
	}

	one := get(t, server.handleGetHost, "/api/hosts/node-b", map[string]string{"host": "node-b"})
	wantOne := `{"name":"node-b","service_count":1,"services":["gateway"],"last_seen":"2026-09-03T08:01:00Z","signals":["logs","traces"]}` + "\n"
	if one.Code != http.StatusOK || one.Body.String() != wantOne {
		t.Fatalf("GET /api/hosts/node-b = %d\n%s\nwant\n%s", one.Code, one.Body.String(), wantOne)
	}

	if missing := get(t, server.handleGetHost, "/api/hosts/node-z", map[string]string{"host": "node-z"}); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown host = %d, want 404", missing.Code)
	}

	bare := NewServer(nil, nil, nil, nil)
	if none := get(t, bare.handleGetHosts, "/api/hosts", nil); none.Body.String() != "[]\n" {
		t.Fatalf("provider-less /api/hosts = %q, want []", none.Body.String())
	}
}

// TestServiceMapNodesGainOnlyHostFields pins the node wire shape: every
// pre-existing field first, unchanged, then kind/host_count/hosts appended.
func TestServiceMapNodesGainOnlyHostFields(t *testing.T) {
	server := newHostsTestServer()
	rec := get(t, server.handleGetServiceMapMetrics, "/api/metrics/service-map", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Nodes []json.RawMessage `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{
		`{"name":"gateway","total_traces":0,"error_count":0,"avg_latency_ms":0,"kind":"service","host_count":2,"hosts":["node-a","node-b"]}`,
		`{"name":"host/node-c","total_traces":0,"error_count":0,"avg_latency_ms":0,"kind":"host","host_count":1,"hosts":["node-c"]}`,
		`{"name":"payments","total_traces":0,"error_count":0,"avg_latency_ms":0,"kind":"service"}`,
	}
	for i, node := range body.Nodes {
		if string(node) != want[i] {
			t.Fatalf("node %d =\n%s\nwant\n%s", i, node, want[i])
		}
	}
	if len(body.Edges) != 1 || string(body.Edges[0]) != `{"source":"gateway","target":"payments","call_count":4,"avg_latency_ms":0,"error_rate":0}` {
		t.Fatalf("edges = %s", body.Edges)
	}
}

// TestSystemGraphNodesGainOnlyHostFields does the same for /api/system/graph
// and proves a host node never enters the service summary.
func TestSystemGraphNodesGainOnlyHostFields(t *testing.T) {
	server := newHostsTestServer()
	rec := get(t, server.handleGetSystemGraph, "/api/system/graph", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		System SystemSummary     `json:"system"`
		Nodes  []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.System.TotalServices != 2 || body.System.Healthy != 2 {
		t.Fatalf("summary counted the host node: %+v", body.System)
	}
	wantSuffix := []string{
		`"alerts":[],"kind":"service","host_count":2,"hosts":["node-a","node-b"]}`,
		`"alerts":[],"kind":"host","host_count":1,"hosts":["node-c"]}`,
		`"alerts":[],"kind":"service"}`,
	}
	for i, node := range body.Nodes {
		if !strings.HasPrefix(string(node), `{"id":"`) || !strings.Contains(string(node), `"type":"service"`) || !strings.HasSuffix(string(node), wantSuffix[i]) {
			t.Fatalf("node %d = %s, want suffix %s", i, node, wantSuffix[i])
		}
	}
}
