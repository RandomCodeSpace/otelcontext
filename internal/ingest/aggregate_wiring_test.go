package ingest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"gorm.io/gorm"
)

// newAggregateEngine builds a shadow-mode engine with the platform default
// budget and a fixed clock, so window placement in these tests is deterministic.
func newAggregateEngine(t *testing.T, now time.Time) *aggregate.Engine {
	t.Helper()
	e, err := aggregate.NewEngine(aggregate.EngineConfig{
		Mode: aggregate.ModeShadow,
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

// newAggregateTestRepo returns an in-memory repository on the synchronous
// persist path, so one Export() leaves everything readable when it returns.
func newAggregateTestRepo(t *testing.T) (*storage.Repository, *gorm.DB) {
	t.Helper()
	db, err := storage.NewDatabase("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	if err := storage.AutoMigrateModels(db, "sqlite"); err != nil {
		t.Fatalf("AutoMigrateModels: %v", err)
	}
	repo := storage.NewRepositoryFromDB(db, "sqlite")
	t.Cleanup(func() { _ = repo.Close() })
	return repo, db
}

func aggTestConfig() *config.Config {
	return &config.Config{
		IngestMinSeverity:          "INFO",
		DefaultTenant:              storage.DefaultTenantID,
		SamplingLatencyThresholdMs: 500,
	}
}

// aggSpanRequest builds an export request of healthy spans spread over a few
// services and operations. Every span is fast and OK, so the sampler's
// always-keep rules for errors and slow spans cannot rescue any of them —
// which is exactly what makes the sampling-invariant test meaningful.
func aggSpanRequest(spansPerService int, ts time.Time) *coltracepb.ExportTraceServiceRequest {
	services := []string{"checkout", "orders", "payments"}
	operations := []string{"GET /a", "GET /b", "POST /c"}

	req := &coltracepb.ExportTraceServiceRequest{}
	for si, svc := range services {
		spans := make([]*tracepb.Span, 0, spansPerService)
		for i := 0; i < spansPerService; i++ {
			status := &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
			spans = append(spans, &tracepb.Span{
				TraceId:           []byte(fmt.Sprintf("%016d%016d", si, i)),
				SpanId:            []byte(fmt.Sprintf("%08d", i)),
				Name:              operations[i%len(operations)],
				Kind:              tracepb.Span_SPAN_KIND_SERVER,
				StartTimeUnixNano: uint64(ts.UnixNano()), // #nosec G115 -- test timestamps are positive
				EndTimeUnixNano:   uint64(ts.Add(time.Millisecond).UnixNano()),
				Status:            status,
				Attributes: []*commonpb.KeyValue{
					{Key: "http.request.method", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "GET"}}},
					{Key: "http.response.status_code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 200}}},
				},
			})
		}
		req.ResourceSpans = append(req.ResourceSpans, &tracepb.ResourceSpans{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: svc}}},
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
		})
	}
	return req
}

func countAggSpans(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&storage.Span{}).Count(&n).Error; err != nil {
		t.Fatalf("count spans: %v", err)
	}
	return n
}

// TestAggregateCountsIdenticalAcrossSamplingRates is THE invariant of the
// aggregate-first design (#153 §8): the same input stream must produce the same
// aggregate counts at SAMPLING_RATE=1.0 and at 0.05. If this test ever fails,
// the reducer has drifted behind the sampler and the engine is measuring the
// sampling rate instead of the traffic.
func TestAggregateCountsIdenticalAcrossSamplingRates(t *testing.T) {
	now := time.Now().UTC() // Export captures arrival from the wall clock; test data must sit in the live window
	const spansPerService = 200

	run := func(rate float64) (aggregate.Snapshot, int64) {
		repo, db := newAggregateTestRepo(t)
		srv := NewTraceServer(repo, nil, aggTestConfig())
		srv.SetSampler(NewSampler(rate, true, 500))
		engine := newAggregateEngine(t, now)
		srv.SetAggregateEngine(engine)

		if _, err := srv.Export(context.Background(), aggSpanRequest(spansPerService, now)); err != nil {
			t.Fatalf("Export at rate %v: %v", rate, err)
		}
		return engine.Snapshot(), countAggSpans(t, db)
	}

	full, fullPersisted := run(1.0)
	sampled, sampledPersisted := run(0.05)

	const total = spansPerService * 3
	if fullPersisted != total {
		t.Fatalf("unsampled run persisted %d spans, want %d", fullPersisted, total)
	}
	// Guard against a vacuous test: sampling must actually be dropping spans.
	if sampledPersisted >= fullPersisted {
		t.Fatalf("sampling dropped nothing (%d vs %d persisted) — the invariant would be untested",
			sampledPersisted, fullPersisted)
	}
	t.Logf("persisted spans: rate=1.0 -> %d, rate=0.05 -> %d", fullPersisted, sampledPersisted)

	fullCount, fullErrors := full.Totals(aggregate.SignalTraceOp)
	sampledCount, sampledErrors := sampled.Totals(aggregate.SignalTraceOp)

	if fullCount != total || sampledCount != total {
		t.Fatalf("aggregate counts = %d (rate 1.0) and %d (rate 0.05), want %d for both",
			fullCount, sampledCount, total)
	}
	if fullErrors != sampledErrors {
		t.Errorf("aggregate error counts diverged: %d vs %d", fullErrors, sampledErrors)
	}
	if full.ActiveSeries != sampled.ActiveSeries {
		t.Errorf("active series diverged: %d vs %d", full.ActiveSeries, sampled.ActiveSeries)
	}
}

// TestAggregateShadowModeDoesNotAlterPersistedOutput: turning the engine on
// must change nothing about what reaches the database.
func TestAggregateShadowModeDoesNotAlterPersistedOutput(t *testing.T) {
	now := time.Now().UTC() // Export captures arrival from the wall clock; test data must sit in the live window
	req := aggSpanRequest(25, now)

	legacyRepo, legacyDB := newAggregateTestRepo(t)
	legacy := NewTraceServer(legacyRepo, nil, aggTestConfig())
	if _, err := legacy.Export(context.Background(), req); err != nil {
		t.Fatalf("legacy Export: %v", err)
	}

	shadowRepo, shadowDB := newAggregateTestRepo(t)
	shadow := NewTraceServer(shadowRepo, nil, aggTestConfig())
	engine := newAggregateEngine(t, now)
	shadow.SetAggregateEngine(engine)
	if _, err := shadow.Export(context.Background(), req); err != nil {
		t.Fatalf("shadow Export: %v", err)
	}

	if got, want := countAggSpans(t, shadowDB), countAggSpans(t, legacyDB); got != want {
		t.Fatalf("shadow mode persisted %d spans, legacy persisted %d", got, want)
	}

	var legacyTraces, shadowTraces int64
	if err := legacyDB.Model(&storage.Trace{}).Count(&legacyTraces).Error; err != nil {
		t.Fatalf("count legacy traces: %v", err)
	}
	if err := shadowDB.Model(&storage.Trace{}).Count(&shadowTraces).Error; err != nil {
		t.Fatalf("count shadow traces: %v", err)
	}
	if legacyTraces != shadowTraces {
		t.Fatalf("shadow mode persisted %d traces, legacy persisted %d", shadowTraces, legacyTraces)
	}

	if count, _ := engine.Snapshot().Totals(aggregate.SignalTraceOp); count != 75 {
		t.Fatalf("aggregate count = %d, want 75", count)
	}
}

// TestLegacyModeRunsNoAggregateCode is the "nothing changed" guard: with no
// engine wired, Export takes the identical path it always did.
func TestLegacyModeRunsNoAggregateCode(t *testing.T) {
	now := time.Now().UTC() // Export captures arrival from the wall clock; test data must sit in the live window
	repo, db := newAggregateTestRepo(t)
	srv := NewTraceServer(repo, nil, aggTestConfig())
	if _, err := srv.Export(context.Background(), aggSpanRequest(10, now)); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := countAggSpans(t, db); got != 30 {
		t.Fatalf("persisted %d spans, want 30", got)
	}
}

// TestAggregateAccountsLogsBelowTheSeverityGate: a DEBUG log that is dropped
// before it ever reaches the database is still accepted telemetry and must be
// accounted.
func TestAggregateAccountsLogsBelowTheSeverityGate(t *testing.T) {
	now := time.Now().UTC() // Export captures arrival from the wall clock; test data must sit in the live window
	repo, db := newAggregateTestRepo(t)
	srv := NewLogsServer(repo, nil, aggTestConfig()) // INGEST_MIN_SEVERITY=INFO
	engine := newAggregateEngine(t, now)
	srv.SetAggregateEngine(engine)

	records := []struct {
		severity string
		body     string
	}{
		{"DEBUG", "cache lookup for key 91 took 3ms"},
		{"DEBUG", "cache lookup for key 92 took 4ms"},
		{"INFO", "request served in 12ms"},
		{"ERROR", "upstream payments timed out after 30s"},
	}
	req := &collogspb.ExportLogsServiceRequest{}
	logRecords := make([]*logspb.LogRecord, 0, len(records))
	for _, r := range records {
		logRecords = append(logRecords, &logspb.LogRecord{
			TimeUnixNano: uint64(now.UnixNano()), // #nosec G115 -- test timestamps are positive
			SeverityText: r.severity,
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: r.body}},
		})
	}
	req.ResourceLogs = append(req.ResourceLogs, &logspb.ResourceLogs{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}}},
		}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: logRecords}},
	})

	if _, err := srv.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}

	var persisted int64
	if err := db.Model(&storage.Log{}).Count(&persisted).Error; err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if persisted != 2 {
		t.Fatalf("persisted %d logs, want 2 (the DEBUG pair is gated out)", persisted)
	}

	count, errors := engine.Snapshot().Totals(aggregate.SignalLog)
	if count != 4 {
		t.Errorf("aggregate log count = %d, want 4 — accounting precedes the severity gate", count)
	}
	if errors != 1 {
		t.Errorf("aggregate log error count = %d, want 1", errors)
	}
}
