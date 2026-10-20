package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestInstanceGraphBeforeSamplingAndEnvironmentPriority(t *testing.T) {
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := db.DB()
	t.Cleanup(func() { _ = conn.Close() })
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	g, err := topology.NewInstanceGraph(ctx, db, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	traces := NewTraceServer(repo, nil, &config.Config{DefaultTenant: "default"})
	traces.SetSampler(NewSampler(0, false, 1<<30))
	traces.SetInstanceGraph(g)
	parent := registryTraceRequest(registryResource("same", stringAttr("service.instance.id", "one"),
		stringAttr("deployment.environment", "legacy"), stringAttr("deployment.environment.name", "dev")))
	child := registryTraceRequest(registryResource("same", stringAttr("service.instance.id", "two"),
		stringAttr("deployment.environment.name", "dev"), stringAttr("deployment.environment", "legacy")))
	p := parent.ResourceSpans[0].ScopeSpans[0].Spans[0]
	c := child.ResourceSpans[0].ScopeSpans[0].Spans[0]
	c.ParentSpanId = append([]byte(nil), p.SpanId...)
	c.SpanId = []byte{2, 2, 2, 2, 2, 2, 2, 2}
	c.Kind = tracepb.Span_SPAN_KIND_SERVER
	// Out-of-order Export calls, even though raw sampling drops everything.
	if _, err := traces.Export(ctx, child); err != nil {
		t.Fatal(err)
	}
	if _, err := traces.Export(ctx, parent); err != nil {
		t.Fatal(err)
	}
	env := "dev"
	graph, err := g.Snapshot(ctx, "default", topology.InstanceQuery{Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Second),
		Environment: &env, Mode: "instances", Limit: 40, Hops: 1})
	if err != nil || len(graph.Nodes) != 2 || len(graph.Edges) != 1 {
		t.Fatalf("pre-sampling identity graph: %+v, %v", graph, err)
	}
	var count int64
	if err := db.Model(&storage.Span{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("test did not drop raw spans: %d, %v", count, err)
	}
}
