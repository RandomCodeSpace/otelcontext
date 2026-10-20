package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

func TestInstanceGraphHTTPBoundsAndTenant(t *testing.T) {
	db, err := storage.NewDatabase("sqlite", filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&storage.TopologyEntity{}, &storage.TopologyBucket{}); err != nil {
		t.Fatal(err)
	}
	g, err := topology.NewInstanceGraph(context.Background(), db, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"alpha", "beta"} {
		g.Register(topology.InstanceIdentity{Tenant: tenant, Environment: "dev", Service: tenant, Instance: "one"}, topology.SignalMetrics, time.Now())
	}
	s := &Server{instanceGraph: g, topologyReads: make(chan struct{}, 4)}
	get := func(path string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(storage.WithTenantContext(req.Context(), "alpha"))
		w := httptest.NewRecorder()
		handler(w, req)
		return w
	}
	w := get("/api/topology/graph?environment=dev", s.handleInstanceGraph)
	var result topology.InstanceSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Nodes) != 1 || result.Nodes[0].Name != "alpha" {
		t.Fatalf("tenant graph: %d %s %v", w.Code, w.Body, err)
	}
	w = get("/api/topology/catalog", s.handleTopologyCatalog)
	var catalog topology.InstanceCatalog
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil || len(catalog.Services) != 1 || catalog.Services[0].Name != "alpha" {
		t.Fatalf("tenant catalog: %s %v", w.Body, err)
	}
	for _, query := range []string{"limit=101", "limit=-1", "hops=3", "mode=raw", "start=bad", "environment=" + url.QueryEscape(string(make([]byte, 256)))} {
		if w := get("/api/topology/graph?"+query, s.handleInstanceGraph); w.Code != 400 {
			t.Fatalf("%s returned %d", query, w.Code)
		}
	}
	for range 4 {
		s.topologyReads <- struct{}{}
	}
	if w := get("/api/topology/graph", s.handleInstanceGraph); w.Code != 429 {
		t.Fatalf("query admission: %d", w.Code)
	}
}

func TestRuntimeFootprintIncludesSidecarsAndUnknown(t *testing.T) {
	dir := t.TempDir()
	for name, size := range map[string]int{"main.db": 18, "main.db-wal": 5, "main.db-shm": 2, "aggregate.db": 112, "aggregate.db-wal": 9} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f := RuntimeFiles{MainDB: filepath.Join(dir, "main.db"), AggregateDB: filepath.Join(dir, "aggregate.db")}
	footprint := runtimeFootprint(f)
	if !footprint.Complete || *footprint.TotalBytes != 146 || *footprint.SidecarBytes != 16 {
		t.Fatalf("wrong total: %+v", footprint)
	}
	link := filepath.Join(dir, "main-link.db")
	if err := os.Symlink(f.MainDB, link); err == nil {
		f.MainDB = link
		if linked := runtimeFootprint(f); linked.TotalBytes == nil || *linked.TotalBytes != 146 {
			t.Fatal("sidecars resolved beside symlink instead of database")
		}
	}
	f.AggregateDB = filepath.Join(dir, "missing.db")
	missing := runtimeFootprint(f)
	if missing.Complete || missing.TotalBytes != nil || missing.AggregateBytes != nil {
		t.Fatal("missing database silently counted as zero")
	}
	f.AggregateDB = ""
	disabled := runtimeFootprint(f)
	if !disabled.Complete || *disabled.AggregateBytes != 0 || *disabled.TotalBytes != 25 {
		t.Fatal("disabled aggregate tier counted as missing")
	}
}
