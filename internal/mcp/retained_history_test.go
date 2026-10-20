package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

func diskRepo(t *testing.T, path string) *storage.Repository {
	t.Helper()
	db, err := storage.NewDatabase("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}
func TestPersistedTraceAndLogsAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.db")
	repo := diskRepo(t, path)
	now := time.Now().UTC().Add(-5 * 24 * time.Hour)
	for _, tenant := range []string{"alpha", "beta"} {
		tr := storage.Trace{TenantID: tenant, TraceID: "persisted", Timestamp: now}
		if err := repo.DB().Create(&tr).Error; err != nil {
			t.Fatal(err)
		}
		spans := []storage.Span{
			{TenantID: tenant, TraceID: "persisted", SpanID: "root", ServiceName: tenant + "-gateway", StartTime: now, Duration: 2000, Status: "STATUS_CODE_OK"},
			{TenantID: tenant, TraceID: "persisted", SpanID: "child", ParentSpanID: "root", ServiceName: tenant + "-db", OperationName: "select", StartTime: now.Add(time.Millisecond), EndTime: now.Add(2 * time.Millisecond), Duration: 1000, Status: "STATUS_CODE_ERROR", AttributesJSON: `{"db.system":"postgresql"}`},
		}
		if err := repo.DB().Create(&spans).Error; err != nil {
			t.Fatal(err)
		}
		log := storage.Log{TenantID: tenant, TraceID: "persisted", SpanID: "child", Timestamp: now, ServiceName: tenant + "-db", Severity: "ERROR", Body: tenant + " error"}
		if err := repo.DB().Create(&log).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = diskRepo(t, path)
	srv := New("alpha", repo, nil, nil)
	srv.SetRawRetention(7 * 24 * time.Hour)
	srv.SetAggregateMode(true)
	ctx := storage.WithTenantContext(context.Background(), "alpha")
	result := srv.toolHandler(ctx, "trace_graph", map[string]any{"trace_id": "persisted"})
	decoded := decodeTraceGraph(t, result)
	if len(decoded.Spans) != 2 || !decoded.Coverage.Complete || decoded.Spans[1].ParentSpanID != "root" || !decoded.Spans[1].IsError || decoded.Spans[1].Duration != 1 {
		t.Fatalf("reconstructed tree: %+v", decoded)
	}
	body := result.Content[0].Resource.Text
	if !strings.Contains(body, `"db.system":"postgresql"`) || !strings.Contains(body, `"span_id":"child"`) || strings.Contains(body, "beta") {
		t.Fatalf("payload/tenant lost: %s", body)
	}
	logs := srv.toolSearchLogs(ctx, map[string]any{"start": now.Add(-time.Hour).Format(time.RFC3339), "end": now.Add(time.Hour).Format(time.RFC3339), "trace_id": "persisted"})
	if logs.IsError || !strings.Contains(logs.Content[0].Resource.Text, "alpha error") || strings.Contains(logs.Content[0].Resource.Text, "beta") {
		t.Fatalf("old correlation: %+v", logs)
	}
	var payload struct {
		Coverage rangeCoverage `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(logs.Content[0].Resource.Text), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Coverage.Clamped || payload.Coverage.Coverage != "exemplar" || payload.Coverage.Available.End.Sub(payload.Coverage.Available.Start) != 7*24*time.Hour {
		t.Fatalf("coverage: %+v", payload)
	}
}
func TestPersistedTraceIncompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		spans    []storage.Span
		observed int
	}{
		{name: "no payload", observed: 3},
		{name: "missing parent", spans: []storage.Span{{SpanID: "orphan", ParentSpanID: "missing"}}},
		{name: "cycle with root", spans: []storage.Span{{SpanID: "root"}, {SpanID: "a", ParentSpanID: "b"}, {SpanID: "b", ParentSpanID: "a"}}},
		{name: "missing sampled child", spans: []storage.Span{{SpanID: "root"}}, observed: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := diskRepo(t, filepath.Join(t.TempDir(), "raw.db"))
			tr := storage.Trace{TenantID: "default", TraceID: "trace"}
			if tc.observed > 0 {
				tr.ObservedSpanCount = &tc.observed
			}
			if err := repo.DB().Create(&tr).Error; err != nil {
				t.Fatal(err)
			}
			for _, sp := range tc.spans {
				sp.TenantID = "default"
				sp.TraceID = "trace"
				if err := repo.DB().Create(&sp).Error; err != nil {
					t.Fatal(err)
				}
			}
			srv := New("default", repo, nil, nil)
			out := decodeTraceGraph(t, srv.toolTraceGraph(context.Background(), map[string]any{"trace_id": "trace"}))
			if out.Coverage.Complete || out.Coverage.Source != graphrag.CoveragePartial {
				t.Fatalf("fabricated completeness: %+v", out.Coverage)
			}
			if out.Coverage.RetainedSpans != len(tc.spans) {
				t.Fatalf("retained count does not match payload: %+v", out.Coverage)
			}
		})
	}
}

type rangeTopology struct {
	fakeMCPTopologyProvider
	queries []topology.Query
	tenants []string
}

func (p *rangeTopology) Snapshot(ctx context.Context, q topology.Query) (topology.Snapshot, error) {
	p.queries = append(p.queries, q)
	p.tenants = append(p.tenants, storage.TenantFromContext(ctx))
	return topology.Snapshot{Nodes: []topology.Node{{Name: "old-service", TotalTraces: 42, ErrorCount: 2}}, Edges: []topology.Edge{}, Meta: topology.Metadata{Start: q.Start, End: q.End, Coverage: "sampled", CoverageNote: "historical fixture", Source: topology.SourceAggregate}}, nil
}
func TestHistoricalTopologyUsesRequestedSnapshot(t *testing.T) {
	for _, tool := range []string{"get_service_map", "get_service_health"} {
		t.Run(tool, func(t *testing.T) {
			p := &rangeTopology{}
			srv := New("alpha", nil, nil, p)
			srv.SetAggregateMode(true)
			start := time.Now().UTC().Add(-6 * 24 * time.Hour).Truncate(time.Second)
			end := start.Add(time.Hour)
			args := map[string]any{"start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339), "service_name": "old-service"}
			res := srv.toolHandler(storage.WithTenantContext(context.Background(), "alpha"), tool, args)
			if res.IsError || len(p.queries) != 1 || !p.queries[0].Start.Equal(start) || !p.queries[0].End.Equal(end) || p.tenants[0] != "alpha" {
				t.Fatalf("wrong query: %+v %+v", p, res)
			}
			if !strings.Contains(res.Content[0].Text, `"call_count":42`) || !strings.Contains(res.Content[1].Text, "historical fixture") || !strings.Contains(res.Content[1].Text, start.Format(time.RFC3339)) {
				t.Fatalf("payload and coverage mismatch: %+v", res)
			}
			srv.cache.Set("alpha", tool, args, res)
			if _, hit := srv.cache.Get("alpha", tool, args); hit {
				t.Fatal("historical snapshot cached against live identity")
			}
		})
	}
}
func TestHistoricalRangeAndPageValidation(t *testing.T) {
	srv := New("default", nil, nil, &rangeTopology{})
	srv.SetRawRetention(7 * 24 * time.Hour)
	now := time.Now().UTC()
	for _, args := range []map[string]any{
		{"start": "garbage"}, {"end": 123}, {"start": now.Format(time.RFC3339), "end": now.Add(-time.Hour).Format(time.RFC3339)},
		{"start": now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)},
		{"start": now.Add(-10 * 24 * time.Hour).Format(time.RFC3339), "end": now.Add(-9 * 24 * time.Hour).Format(time.RFC3339)},
	} {
		if res := srv.toolSearchLogs(context.Background(), args); !res.IsError {
			t.Fatalf("logs accepted %+v", args)
		}
		if res := srv.toolGetServiceMap(context.Background(), args); !res.IsError {
			t.Fatalf("topology accepted %+v", args)
		}
	}
	for _, args := range []map[string]any{{"limit": float64(-1)}, {"page": 1e99}, {"page": 0.5}, {"limit": float64(201)}, {"start": now.Add(-6 * 24 * time.Hour).Format(time.RFC3339), "query": "error"}} {
		if res := srv.toolSearchLogs(context.Background(), args); !res.IsError {
			t.Fatalf("logs accepted unbounded request %+v", args)
		}
	}
}
func TestHistoricalInvestigationEvidenceIsTenantScoped(t *testing.T) {
	srv, _, repo := honestyServer(t)
	if err := repo.DB().AutoMigrate(&graphrag.Investigation{}); err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC().Add(-5 * 24 * time.Hour)
	rows := []graphrag.Investigation{
		{ID: "old-alpha", TenantID: "alpha", CreatedAt: when, TriggerService: "api", ErrorMessage: "alpha history"},
		{ID: "old-beta", TenantID: "beta", CreatedAt: when, RootService: "api", ErrorMessage: "beta history"},
		{ID: "expired-alpha", TenantID: "alpha", CreatedAt: when.Add(-5 * 24 * time.Hour), TriggerService: "api"},
	}
	if err := repo.DB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	ctx := storage.WithTenantContext(context.Background(), "alpha")
	for _, tool := range []string{"get_anomaly_timeline", "root_cause_analysis"} {
		res := srv.toolHandler(ctx, tool, map[string]any{"service": "api", "since": when.Add(-time.Hour).Format(time.RFC3339), "time_range": "144h"})
		if res.IsError || len(res.Content) < 2 {
			t.Fatalf("missing evidence: %+v", res)
		}
		text := res.Content[1].Text
		if !strings.Contains(text, "alpha history") || strings.Contains(text, "beta history") || strings.Contains(text, "expired-alpha") || !strings.Contains(text, `"complete":false`) {
			t.Fatalf("invalid historical evidence: %s", text)
		}
	}
}

func TestPersistedTraceDatabaseFailureIsNotAbsence(t *testing.T) {
	repo := diskRepo(t, filepath.Join(t.TempDir(), "raw.db"))
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	srv := New("default", repo, nil, nil)
	if res := srv.toolTraceGraph(context.Background(), map[string]any{"trace_id": "any"}); !res.IsError {
		t.Fatal("database failure reported as an absent trace")
	}
}

func TestLogRetentionClampIsExplicit(t *testing.T) {
	repo := diskRepo(t, filepath.Join(t.TempDir(), "raw.db"))
	srv := New("default", repo, nil, nil)
	srv.SetRawRetention(7 * 24 * time.Hour)
	now := time.Now().UTC()
	args := map[string]any{"start": now.Add(-7*24*time.Hour - time.Hour).Format(time.RFC3339), "end": now.Add(-6 * 24 * time.Hour).Format(time.RFC3339), "service": "api"}
	res := srv.toolSearchLogs(context.Background(), args)
	if res.IsError {
		t.Fatalf("intersecting range rejected: %+v", res)
	}
	var payload struct {
		Coverage rangeCoverage `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Resource.Text), &payload); err != nil {
		t.Fatal(err)
	}
	c := payload.Coverage
	if !c.Clamped || !c.Requested.Start.Before(c.Available.Start) || !c.Queried.Start.Equal(c.Available.Start) {
		t.Fatalf("hidden retention clamp: %+v", c)
	}
}
