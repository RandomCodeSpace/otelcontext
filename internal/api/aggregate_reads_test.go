package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/cache"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

// rawTableCounter counts GORM reads that touch the raw telemetry tables. It is
// how "no raw trace/log table scans behind the dashboard in aggregate mode"
// stops being a claim and becomes an assertion.
type rawTableCounter struct{ hits []string }

var rawTables = map[string]bool{"traces": true, "logs": true, "spans": true}

func (c *rawTableCounter) install(t *testing.T, db *gorm.DB) {
	t.Helper()
	record := func(tx *gorm.DB) {
		table := tx.Statement.Table
		if table == "" {
			table = strings.ToLower(tx.Statement.SQL.String())
		}
		for raw := range rawTables {
			if strings.Contains(strings.ToLower(table), raw) {
				c.hits = append(c.hits, table)
				return
			}
		}
	}
	if err := db.Callback().Query().Before("gorm:query").Register("test:count_raw_reads", record); err != nil {
		t.Fatalf("register query callback: %v", err)
	}
	if err := db.Callback().Raw().Before("gorm:raw").Register("test:count_raw_reads", record); err != nil {
		t.Fatalf("register raw callback: %v", err)
	}
}

// aggregateTestServer builds an API server with a real repository and, when
// withEngine is set, an aggregate engine in aggregate mode.
func aggregateTestServer(t *testing.T, withEngine bool) (*Server, *rawTableCounter, *aggregate.Engine) {
	t.Helper()
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	c := cache.New()
	t.Cleanup(func() {
		c.Stop()
		_ = repo.Close()
	})
	s := &Server{repo: repo, cache: c}

	var engine *aggregate.Engine
	if withEngine {
		engine, err = aggregate.NewEngine(aggregate.EngineConfig{Mode: aggregate.ModeAggregate})
		if err != nil {
			t.Fatalf("NewEngine: %v", err)
		}
		s.SetAggregateEngine(engine)
		if !s.aggregateReads() {
			t.Fatal("aggregate engine was not adopted by the server")
		}
	}
	counter := &rawTableCounter{}
	counter.install(t, db)
	return s, counter, engine
}

// seedAggregate folds one service's worth of spans and logs into the engine's
// current window.
func seedAggregate(t *testing.T, e *aggregate.Engine, tenant, service string, spans int, micros float64) {
	t.Helper()
	tenantID := e.TenantID(tenant)
	key := aggregate.SeriesKey{
		TenantID:    tenantID,
		ServiceID:   e.Cache().Intern(tenantID, aggregate.KindService, service),
		NameID:      e.Cache().Intern(tenantID, aggregate.KindOperation, "GET /"+service),
		Signal:      aggregate.SignalTraceOp,
		StatusClass: aggregate.StatusOK,
		Variant:     aggregate.SpanKindServer,
	}
	d := &aggregate.AggregateDelta{}
	for i := 0; i < spans; i++ {
		d.ObserveSpan(micros, i%3 == 0, true)
	}
	logKey := aggregate.SeriesKey{
		TenantID:  tenantID,
		ServiceID: key.ServiceID,
		NameID:    e.Cache().Intern(tenantID, aggregate.KindLogTemplate, service+"|<*> failed"),
		Signal:    aggregate.SignalLog,
	}
	ld := &aggregate.AggregateDelta{}
	ld.ObserveLog(time.Now(), true)

	w := aggregate.WindowStart(time.Now())
	e.ApplyCommitted(aggregate.DeltaMap{
		{Key: key, WindowStart: w}:    d,
		{Key: logKey, WindowStart: w}: ld,
	})
}

// seedLegacy inserts equivalent raw rows so the legacy path has something to
// aggregate.
func seedLegacy(t *testing.T, s *Server, tenant, service string, spans int, micros int64) {
	t.Helper()
	now := time.Now().UTC()
	traces := make([]storage.Trace, 0, spans)
	for i := 0; i < spans; i++ {
		status := "STATUS_CODE_OK"
		if i%3 == 0 {
			status = storage.StatusCodeError
		}
		traces = append(traces, storage.Trace{
			TenantID: tenant, TraceID: service + "-" + string(rune('a'+i)),
			ServiceName: service, Timestamp: now, Duration: micros, Status: status,
		})
	}
	if err := s.repo.BatchCreateTraces(traces); err != nil {
		t.Fatalf("seed traces: %v", err)
	}
	if err := s.repo.BatchCreateLogs([]storage.Log{{
		TenantID: tenant, ServiceName: service, Severity: "ERROR",
		Body: "failed", Timestamp: now,
	}}); err != nil {
		t.Fatalf("seed logs: %v", err)
	}
}

func getJSON(t *testing.T, h http.HandlerFunc, target, tenant string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	ctx := storage.WithTenantContext(context.Background(), tenant)
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v (%s)", target, err, rr.Body.String())
	}
	return rr, body
}

// TestDashboardContractLegacyVsAggregate is the migration's contract: every
// field the legacy dashboard emits must exist in the aggregate response with
// the same JSON type. New fields are allowed; missing or retyped ones are not.
func TestDashboardContractLegacyVsAggregate(t *testing.T) {
	const tenant = "default"

	legacy, _, _ := aggregateTestServer(t, false)
	seedLegacy(t, legacy, tenant, "checkout", 9, 1000)
	_, legacyBody := getJSON(t, legacy.handleGetDashboardStats, "/api/metrics/dashboard", tenant)

	agg, _, engine := aggregateTestServer(t, true)
	seedAggregate(t, engine, tenant, "checkout", 9, 1000)
	_, aggBody := getJSON(t, agg.handleGetDashboardStats, "/api/metrics/dashboard", tenant)

	assertFieldCompatible(t, "dashboard", legacyBody, aggBody)

	if aggBody["coverage"] != string(aggregate.CoverageFull) {
		t.Errorf("aggregate coverage = %v, want %q", aggBody["coverage"], aggregate.CoverageFull)
	}
	accuracy, ok := aggBody["accuracy"].(map[string]any)
	if !ok {
		t.Fatalf("aggregate response carries no accuracy metadata: %v", aggBody)
	}
	if accuracy["approximate"] != true {
		t.Errorf("accuracy.approximate = %v, want true", accuracy["approximate"])
	}
	bound, _ := accuracy["relative_error_bound"].(float64)
	if bound <= 0 {
		t.Errorf("accuracy.relative_error_bound = %v, want > 0", accuracy["relative_error_bound"])
	}
	if _, ok := accuracy["sketch_scale"]; !ok {
		t.Error("accuracy carries no sketch_scale")
	}
	// Legacy must stay exactly what it was: no additive fields leak into it.
	for _, field := range []string{"coverage", "accuracy", "epoch", "revision", "coverage_note"} {
		if _, present := legacyBody[field]; present {
			t.Errorf("legacy dashboard response gained %q", field)
		}
	}
}

func TestServiceMapContractLegacyVsAggregate(t *testing.T) {
	const tenant = "default"

	legacy, _, _ := aggregateTestServer(t, false)
	seedServiceMapSpans(t, legacy, tenant, "x")
	_, legacyBody := getJSON(t, legacy.handleGetServiceMapMetrics, "/api/metrics/service-map", tenant)

	agg, _, engine := aggregateTestServer(t, true)
	seedAggregate(t, engine, tenant, "checkout", 6, 2000)
	_, aggBody := getJSON(t, agg.handleGetServiceMapMetrics, "/api/metrics/service-map", tenant)

	assertFieldCompatible(t, "service-map", legacyBody, aggBody)
	if aggBody["coverage"] != string(aggregate.CoverageSampled) {
		t.Errorf("service-map coverage = %v, want %q", aggBody["coverage"], aggregate.CoverageSampled)
	}
	if note, _ := aggBody["coverage_note"].(string); note == "" {
		t.Error("sampled service-map carries no coverage note")
	}
}

// TestTrafficUsesCoverageHeaderNotAnEnvelope pins the bare-array rule: the body
// shape is unchanged and coverage rides in the header.
func TestTrafficUsesCoverageHeaderNotAnEnvelope(t *testing.T) {
	const tenant = "default"
	agg, _, engine := aggregateTestServer(t, true)
	seedAggregate(t, engine, tenant, "checkout", 6, 1000)

	ctx := storage.WithTenantContext(context.Background(), tenant)
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/traffic", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	agg.handleGetTrafficMetrics(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("traffic = %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get(aggregate.CoverageHeader); got != string(aggregate.CoverageFull) {
		t.Errorf("%s = %q, want %q", aggregate.CoverageHeader, got, aggregate.CoverageFull)
	}
	var points []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &points); err != nil {
		t.Fatalf("traffic body is not a bare array: %v (%s)", err, rr.Body.String())
	}
	if len(points) == 0 {
		t.Fatal("traffic returned no points")
	}
	for _, field := range []string{"timestamp", "count", "error_count"} {
		if _, ok := points[0][field]; !ok {
			t.Errorf("traffic point is missing %q: %v", field, points[0])
		}
	}
}

// TestLatencyHeatmapDeclaresExemplarCoverage: an empty heatmap in aggregate
// mode must not read as "nothing happened".
func TestLatencyHeatmapDeclaresExemplarCoverage(t *testing.T) {
	agg, _, _ := aggregateTestServer(t, true)
	ctx := storage.WithTenantContext(context.Background(), "default")
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/latency_heatmap", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	agg.handleGetLatencyHeatmap(rr, req)
	if got := rr.Header().Get(aggregate.CoverageHeader); got != string(aggregate.CoverageExemplar) {
		t.Errorf("%s = %q, want %q", aggregate.CoverageHeader, got, aggregate.CoverageExemplar)
	}
}

// TestNoRawTableScansBehindDashboardInAggregateMode is the acceptance
// criterion: with the aggregate engine wired, the dashboard, traffic and
// topology endpoints must not read the traces, logs or spans tables at all.
func TestNoRawTableScansBehindDashboardInAggregateMode(t *testing.T) {
	const tenant = "default"
	agg, counter, engine := aggregateTestServer(t, true)
	seedAggregate(t, engine, tenant, "checkout", 12, 1500)

	ctx := storage.WithTenantContext(context.Background(), tenant)
	endpoints := []struct {
		target  string
		handler http.HandlerFunc
	}{
		{"/api/metrics/dashboard", agg.handleGetDashboardStats},
		{"/api/metrics/traffic", agg.handleGetTrafficMetrics},
		{"/api/metrics/service-map", agg.handleGetServiceMapMetrics},
	}
	for _, ep := range endpoints {
		req := httptest.NewRequest(http.MethodGet, ep.target, nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		ep.handler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", ep.target, rr.Code, rr.Body.String())
		}
	}
	if len(counter.hits) != 0 {
		t.Fatalf("aggregate-mode dashboard endpoints hit raw tables: %v", counter.hits)
	}

	// Control: the same endpoints in legacy mode DO scan the raw tables, so a
	// silently broken counter cannot make the assertion above vacuous.
	legacy, legacyCounter, _ := aggregateTestServer(t, false)
	seedLegacy(t, legacy, tenant, "checkout", 3, 1000)
	legacyCounter.hits = nil
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/dashboard", nil).WithContext(ctx)
	legacy.handleGetDashboardStats(httptest.NewRecorder(), req)
	if len(legacyCounter.hits) == 0 {
		t.Fatal("legacy dashboard hit no raw tables; the query counter is not wired")
	}
}

// assertFieldCompatible checks that every field of the legacy payload survives
// into the aggregate payload with the same JSON type.
func assertFieldCompatible(t *testing.T, name string, legacy, agg map[string]any) {
	t.Helper()
	for field, legacyVal := range legacy {
		aggVal, ok := agg[field]
		if !ok {
			t.Errorf("%s: aggregate response dropped field %q", name, field)
			continue
		}
		if legacyVal == nil || aggVal == nil {
			continue
		}
		if jsonKind(legacyVal) != jsonKind(aggVal) {
			t.Errorf("%s: field %q changed type: legacy %s, aggregate %s",
				name, field, jsonKind(legacyVal), jsonKind(aggVal))
		}
	}
}

// jsonKind names the decoded JSON kind of v.
func jsonKind(v any) string {
	switch v.(type) {
	case float64:
		return "number"
	case string:
		return "string"
	case bool:
		return "bool"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "null"
	}
}
