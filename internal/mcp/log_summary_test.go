package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

type fakeLogSummary struct {
	q   aggregate.Query
	err error
}

func (f *fakeLogSummary) QueryLogSummary(ctx context.Context, q aggregate.Query) (*aggregate.LogSummaryResult, error) {
	f.q = q
	if f.err != nil {
		return nil, f.err
	}
	return &aggregate.LogSummaryResult{Points: []aggregate.LogSummaryPoint{{WindowStart: q.Start, Count: 72}}, Coverage: aggregate.CoverageFull, ResolutionSeconds: 300}, nil
}
func TestLogSummaryToolRangeTenantAndErrors(t *testing.T) {
	backend := &fakeLogSummary{}
	srv := New("alpha", nil, nil, nil)
	srv.SetAggregateMode(true)
	srv.SetLogSummaryQuerier(backend)
	start := time.Now().UTC().Add(-6 * 24 * time.Hour).Truncate(time.Second)
	end := start.Add(time.Hour)
	args := map[string]any{"start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339), "service": "api"}
	res := srv.toolHandler(storage.WithTenantContext(context.Background(), "alpha"), "get_log_summary", args)
	if res.IsError || backend.q.Tenant != "alpha" || len(backend.q.Services) != 1 || backend.q.Services[0] != "api" || !backend.q.Start.Equal(start) || !backend.q.End.Equal(end) {
		t.Fatalf("incorrect query: %+v %+v", backend.q, res)
	}
	var body struct {
		Summary  aggregate.LogSummaryResult `json:"summary"`
		Coverage rangeCoverage              `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Summary.Points) != 1 || body.Summary.Points[0].Count != 72 || body.Coverage.Coverage != "full" {
		t.Fatalf("incorrect summary: %+v", body)
	}
	backend.err = errors.New("store unavailable")
	if res := srv.toolLogSummary(context.Background(), args); !res.IsError || !strings.Contains(res.Content[0].Text, "store unavailable") {
		t.Fatalf("lost store error: %+v", res)
	}
	srv.SetAggregateMode(false)
	if res := srv.toolLogSummary(context.Background(), args); !res.IsError {
		t.Fatal("aggregate log summary silently used in legacy mode")
	}
}

type limitedLogSummary struct{}

func (limitedLogSummary) QueryLogSummary(context.Context, aggregate.Query) (*aggregate.LogSummaryResult, error) {
	return &aggregate.LogSummaryResult{Coverage: aggregate.CoverageLimited, IdentityCoverage: aggregate.IdentityCoverage{LogPoints: 9}}, nil
}
func TestMCPIdentityCoveragePassesThrough(t *testing.T) {
	p := &fakeMCPTopologyProvider{snapshot: topology.Snapshot{Nodes: []topology.Node{}, Edges: []topology.Edge{}, Meta: topology.Metadata{Coverage: "limited", CoverageNote: aggregate.CoverageLimited.Note(), IdentityCoverage: aggregate.IdentityCoverage{TracePoints: 4}}}}
	srv := New("default", nil, nil, p)
	srv.SetAggregateMode(true)
	srv.SetLogSummaryQuerier(limitedLogSummary{})
	for _, tool := range []string{"get_service_map", "get_log_summary"} {
		res := srv.toolHandler(context.Background(), tool, nil)
		data, _ := json.Marshal(res)
		if res.IsError || !strings.Contains(string(data), "identity_coverage") || !strings.Contains(string(data), "limited") {
			t.Fatalf("%s lost identity coverage: %s", tool, data)
		}
	}
	notification, _, ok := srv.topologyNotification(context.Background(), topology.Identity{}, false)
	if !ok || !strings.Contains(notification, "identity_coverage") || !strings.Contains(notification, "limited") {
		t.Fatalf("SSE lost coverage: %s", notification)
	}
}
