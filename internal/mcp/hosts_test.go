package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

// setupHostsServer wires an MCP server over a started GraphRAG holding one
// gateway service, and a provider projecting gateway onto node-a and node-b
// plus a host entity on node-c.
func setupHostsServer(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	if err := graphrag.AutoMigrateGraphRAG(db); err != nil {
		t.Fatalf("AutoMigrateGraphRAG: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	cfg := graphrag.DefaultConfig()
	cfg.RefreshEvery, cfg.SnapshotEvery, cfg.AnomalyEvery = 24*time.Hour, 24*time.Hour, 24*time.Hour
	cfg.WorkerCount = 1
	g := graphrag.New(repo, nil, nil, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	go g.Start(ctx)
	now := time.Now().UTC()
	g.OnSpanIngested(storage.Span{TenantID: storage.DefaultTenantID, TraceID: "t", SpanID: "p", ServiceName: "gateway", OperationName: "op", StartTime: now, EndTime: now.Add(time.Millisecond), Duration: 1000, Status: "STATUS_CODE_OK"})
	deadline := time.Now().Add(2 * time.Second)
	for len(g.ServiceMap(storage.WithTenantContext(context.Background(), storage.DefaultTenantID), 0)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("gateway never reached the service store")
		}
		time.Sleep(10 * time.Millisecond)
	}

	provider := &fakeMCPTopologyProvider{epoch: "boot-a", hosts: topology.ProjectHosts([]topology.ResourceEntry{
		{ResourceKey: topology.ResourceKey{Tenant: storage.DefaultTenantID, Service: "gateway", Host: "node-a"}, Signals: topology.SignalTraces},
		{ResourceKey: topology.ResourceKey{Tenant: storage.DefaultTenantID, Service: "gateway", Host: "node-b"}, Signals: topology.SignalTraces},
		{ResourceKey: topology.ResourceKey{Tenant: storage.DefaultTenantID, Service: "host/node-c", Host: "node-c"}, Signals: topology.SignalMetrics},
	})}
	srv := New("", repo, nil, provider)
	srv.SetGraphRAG(g)
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		httpSrv.Close()
		cancel()
		g.Stop()
		_ = repo.Close()
	})
	return httpSrv
}

func TestGetServiceMapGroupByHostIsAdditiveAndSeparatelyCached(t *testing.T) {
	ts := setupHostsServer(t)

	_, plain := callTool(t, ts, "", "get_service_map", nil)
	if !strings.HasPrefix(plain, `[{"service":{`) || strings.Contains(plain, `"hosts"`) {
		t.Fatalf("plain get_service_map changed shape: %s", plain)
	}

	// The plain answer is now cached; group_by must not be served from it.
	_, grouped := callTool(t, ts, "", "get_service_map", map[string]any{"group_by": "host"})
	if !strings.HasPrefix(grouped, `{"services":[{"service":{`) {
		t.Fatalf("grouped get_service_map = %s", grouped)
	}
	wantHosts := `"hosts":[{"name":"node-a","service_count":1,"services":["gateway"],"last_seen":"0001-01-01T00:00:00Z","signals":["traces"]},` +
		`{"name":"node-b","service_count":1,"services":["gateway"],"last_seen":"0001-01-01T00:00:00Z","signals":["traces"]},` +
		`{"name":"node-c","service_count":0,"services":[],"last_seen":"0001-01-01T00:00:00Z","signals":["metrics"]}]}`
	if !strings.HasSuffix(grouped, wantHosts) {
		t.Fatalf("grouped hosts =\n%s\nwant suffix\n%s", grouped, wantHosts)
	}
	if plainAgain, _ := callTool(t, ts, "", "get_service_map", nil); plainAgain.IsError || strings.Contains(plainAgain.Content[0].Text, `"hosts"`) {
		t.Fatalf("plain answer polluted by grouped cache entry: %+v", plainAgain)
	}

	res, text := callTool(t, ts, "", "get_service_map", map[string]any{"group_by": "pod"})
	if !res.IsError || !strings.Contains(text, "unsupported group_by") {
		t.Fatalf("unsupported group_by accepted: %+v", res)
	}
}

func TestGetServiceHealthGainsHosts(t *testing.T) {
	ts := setupHostsServer(t)
	_, health := callTool(t, ts, "", "get_service_health", map[string]any{"service_name": "gateway"})
	if !strings.HasPrefix(health, `{"service":{`) || !strings.HasSuffix(health, `,"hosts":["node-a","node-b"]}`) {
		t.Fatalf("get_service_health = %s", health)
	}
}

func TestCacheKeyDistinguishesGroupBy(t *testing.T) {
	plain := cacheKey("default", "get_service_map", nil)
	grouped := cacheKey("default", "get_service_map", map[string]any{"group_by": "host"})
	if plain == grouped {
		t.Fatal("group_by does not change the cache key")
	}
}
