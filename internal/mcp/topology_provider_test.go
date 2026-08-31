package mcp

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

type fakeMCPTopologyProvider struct {
	epoch    string
	revision atomic.Uint64
	snapshot topology.Snapshot
	err      error
}

func (*fakeMCPTopologyProvider) Source() topology.Source { return topology.SourceAggregate }

func (p *fakeMCPTopologyProvider) Identity(context.Context) topology.Identity {
	return topology.Identity{Epoch: p.epoch, Revision: p.revision.Load()}
}

func (p *fakeMCPTopologyProvider) Snapshot(context.Context, topology.Query) (topology.Snapshot, error) {
	if p.err != nil {
		return topology.Snapshot{}, p.err
	}
	snapshot := p.snapshot
	snapshot.Meta.Source = topology.SourceAggregate
	snapshot.Meta.Epoch = p.epoch
	snapshot.Meta.Revision = p.revision.Load()
	return snapshot, nil
}

func TestMCPTopologySSEImmediatelyUsesProvider(t *testing.T) {
	provider := &fakeMCPTopologyProvider{
		epoch: "boot-a",
		snapshot: topology.Snapshot{
			Nodes: []topology.Node{{Name: "gateway"}, {Name: "aggregate-payments"}},
			Edges: []topology.Edge{{Source: "gateway", Target: "aggregate-payments"}},
			Meta:  topology.Metadata{Coverage: "full"},
		},
	}
	provider.revision.Store(7)
	server := New("default", nil, nil, provider)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest("GET", "/mcp", nil).WithContext(ctx)
	request.Header.Set("Last-Event-ID", "old:99")
	recorder := httptest.NewRecorder()
	server.handleSSE(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "notifications/resources/updated") || !strings.Contains(body, "aggregate-payments") {
		t.Fatalf("SSE did not send the current provider replacement immediately: %s", body)
	}
	if !strings.Contains(body, `\"epoch\":\"boot-a\"`) || !strings.Contains(body, `\"revision\":7`) || !strings.Contains(body, `\"reset\":true`) {
		t.Fatalf("SSE replacement identity/reset missing: %s", body)
	}
}

func TestMCPTopologyCacheScopeMovesWithProviderIdentity(t *testing.T) {
	provider := &fakeMCPTopologyProvider{epoch: "boot-a", snapshot: topology.Snapshot{Nodes: []topology.Node{}, Edges: []topology.Edge{}}}
	provider.revision.Store(1)
	server := New("default", nil, nil, provider)
	first := server.topologyCacheScope(context.Background(), "default", "get_service_map")
	provider.revision.Store(2)
	second := server.topologyCacheScope(context.Background(), "default", "get_service_map")
	if first == second {
		t.Fatalf("cache scope did not change with topology revision: %q", first)
	}
	if got := server.topologyCacheScope(context.Background(), "default", "trace_graph"); got != "default" {
		t.Fatalf("non-topology tool cache scope changed: %q", got)
	}
}

func TestMCPTopologyProviderErrorPublishesNothing(t *testing.T) {
	provider := &fakeMCPTopologyProvider{epoch: "boot-a", err: errors.New("boom")}
	server := New("default", nil, nil, provider)
	if _, _, ok := server.topologyNotification(context.Background(), topology.Identity{Epoch: "boot-a", Revision: 1}, true); ok {
		t.Fatal("provider error became an empty SSE replacement")
	}
}
