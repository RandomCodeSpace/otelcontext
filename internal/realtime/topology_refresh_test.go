package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/topology"
	"github.com/coder/websocket"
)

type fakeRefreshProvider struct {
	epoch atomic.Value
	rev   atomic.Uint64
}

func newFakeRefreshProvider(epoch string) *fakeRefreshProvider {
	provider := &fakeRefreshProvider{}
	provider.epoch.Store(epoch)
	return provider
}

func (*fakeRefreshProvider) Source() topology.Source { return topology.SourceAggregate }

func (p *fakeRefreshProvider) Identity(context.Context) topology.Identity {
	return topology.Identity{Epoch: p.epoch.Load().(string), Revision: p.rev.Load()}
}

func (p *fakeRefreshProvider) Snapshot(ctx context.Context, _ topology.Query) (topology.Snapshot, error) {
	id := p.Identity(ctx)
	return topology.Snapshot{
		Nodes: []topology.Node{},
		Edges: []topology.Edge{},
		Meta:  topology.Metadata{Source: topology.SourceAggregate, Epoch: id.Epoch, Revision: id.Revision},
	}, nil
}

func TestAggregateRawWSSendsCoalescedTopologyRefresh(t *testing.T) {
	hub := NewHub(nil)
	provider := newFakeRefreshProvider("boot-a")
	provider.rev.Store(1)
	hub.SetAggregateMode(true)
	hub.SetTopologyProvider(provider, 30*time.Millisecond)
	go hub.Run()

	server := httptest.NewServer(http.HandlerFunc(hub.HandleWebSocket))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	conn, response, err := websocket.Dial(ctx, "ws"+server.URL[len("http"):], nil)
	cancel()
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close(websocket.StatusNormalClosure, "test")
		hub.Stop()
		server.Close()
	})

	first := readTopologyRefresh(t, conn, 2*time.Second)
	if first.Epoch != "boot-a" || first.Revision != 1 || !first.Reset {
		t.Fatalf("initial refresh = %+v, want boot-a/1 reset", first)
	}

	provider.rev.Store(2)
	next := readTopologyRefresh(t, conn, 2*time.Second)
	if next.Epoch != "boot-a" || next.Revision != 2 || next.Reset {
		t.Fatalf("revision refresh = %+v, want boot-a/2 without reset", next)
	}
}

func TestAggregateRawWSNoClientsDoesNotConsumeRevision(t *testing.T) {
	hub := NewHub(nil)
	provider := newFakeRefreshProvider("boot-a")
	provider.rev.Store(9)
	hub.SetAggregateMode(true)
	hub.SetTopologyProvider(provider, time.Second)
	t.Cleanup(hub.Stop)

	hub.publishTopologyRefresh()

	if hub.topologyPublished {
		t.Fatal("revision was marked published with no clients")
	}
}

func readTopologyRefresh(t *testing.T, conn *websocket.Conn, timeout time.Duration) topologyRefresh {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, message, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read topology refresh: %v", err)
	}
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		t.Fatalf("decode refresh envelope: %v (%s)", err, message)
	}
	if envelope.Type != "topology_refresh" {
		t.Fatalf("message type = %q, want topology_refresh", envelope.Type)
	}
	var refresh topologyRefresh
	if err := json.Unmarshal(envelope.Data, &refresh); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	return refresh
}
