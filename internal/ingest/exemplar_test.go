package ingest

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// countingExemplarMetrics is a test recorder. The policy's counters are the
// operator's only view of the completeness-vs-coverage gap, so the tests assert
// on them rather than trusting the drop paths silently.
type countingExemplarMetrics struct {
	mu         sync.Mutex
	eligible   map[string]int
	dropped    map[string]int
	evictions  int
	truncation int
}

func newCountingExemplarMetrics() *countingExemplarMetrics {
	return &countingExemplarMetrics{
		eligible: make(map[string]int),
		dropped:  make(map[string]int),
	}
}

func (m *countingExemplarMetrics) RecordExemplarEligible(signal, class string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eligible[signal+"/"+class]++
}

func (m *countingExemplarMetrics) RecordExemplarDropped(signal, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropped[signal+"/"+reason]++
}

func (m *countingExemplarMetrics) RecordExemplarEviction() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evictions++
}

func (m *countingExemplarMetrics) RecordExemplarTruncation() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.truncation++
}

func (m *countingExemplarMetrics) snapshot() (map[string]int, map[string]int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := make(map[string]int, len(m.eligible))
	for k, v := range m.eligible {
		e[k] = v
	}
	d := make(map[string]int, len(m.dropped))
	for k, v := range m.dropped {
		d[k] = v
	}
	return e, d, m.evictions, m.truncation
}

// exemplarOffer is one span offered to the policy in the unit-level tests.
type exemplarOffer struct {
	traceID    string
	operation  string
	status     string
	durationMs float64
}

func offerAll(p *ExemplarPolicy, service string, ts time.Time, offers []exemplarOffer) []string {
	admitted := make([]string, 0, len(offers))
	for _, o := range offers {
		in := ExemplarSpan{
			Tenant:     storage.DefaultTenantID,
			Service:    service,
			TraceID:    o.traceID,
			Operation:  o.operation,
			Status:     o.status,
			DurationMs: o.durationMs,
			Timestamp:  ts,
		}
		if p.AdmitSpan(in) {
			admitted = append(admitted, o.traceID)
			// Real callers always follow an admission with a charge; skipping
			// it here would leave the byte budget untouched and make the count
			// tests silently depend on charge-free admission.
			p.ChargeSpan(in.Tenant, service, o.traceID, ts, 512)
		}
	}
	return admitted
}

func sortedSelected(p *ExemplarPolicy, service string, ts time.Time) []string {
	got := p.SelectedTraces(storage.DefaultTenantID, service, ts)
	sort.Strings(got)
	return got
}

// TestExemplarSelectionIsOrderIndependentAndDuplicateSafe is the #161 selection
// contract: the retained set is a property of the traces offered in a window,
// not of the order they showed up in or how many times they were redelivered.
// Without this, #160's at-least-once retries and multi-batch traces would each
// produce a different exemplar set on every replay.
func TestExemplarSelectionIsOrderIndependentAndDuplicateSafe(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	operations := []string{"GET /a", "GET /b", "POST /c", "GET /d", "PUT /e"}

	base := make([]exemplarOffer, 0, 600)
	for i := 0; i < 400; i++ {
		base = append(base, exemplarOffer{
			traceID:    fmt.Sprintf("%032x", i),
			operation:  operations[i%len(operations)],
			status:     storage.StatusCodeError,
			durationMs: 5,
		})
	}
	// Healthy traces exercise the stateless hash-threshold branch alongside the
	// stratified top-K branch.
	for i := 400; i < 600; i++ {
		base = append(base, exemplarOffer{
			traceID:    fmt.Sprintf("%032x", i),
			operation:  operations[i%len(operations)],
			status:     "STATUS_CODE_OK",
			durationMs: 5,
		})
	}

	newPolicy := func() *ExemplarPolicy {
		return NewExemplarPolicy(ExemplarConfig{
			LatencyThresholdMs: 500,
			HealthyRate:        0.05, // raised so healthy eligibility is not vacuous at n=200
		})
	}

	reference := newPolicy()
	offerAll(reference, "checkout", ts, base)
	want := sortedSelected(reference, "checkout", ts)
	if len(want) == 0 {
		t.Fatal("reference run selected nothing — the test would be vacuous")
	}

	for run := 0; run < 5; run++ {
		shuffled := append([]exemplarOffer(nil), base...)
		rng := rand.New(rand.NewSource(int64(run) + 1)) // #nosec G404 -- deterministic test shuffle, not security
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		// Duplicate a third of the offers: a redelivery must re-select the same
		// slot, never consume a second one.
		for i := 0; i < len(base); i += 3 {
			shuffled = append(shuffled, base[i])
		}
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		p := newPolicy()
		offerAll(p, "checkout", ts, shuffled)
		got := sortedSelected(p, "checkout", ts)
		if len(got) != len(want) {
			t.Fatalf("run %d selected %d traces, reference selected %d", run, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("run %d retained set diverged at %d: got %s, want %s", run, i, got[i], want[i])
			}
		}
	}
}

// TestExemplarPriorityFillDisplacesHealthyBeforeErrors: under budget pressure
// the cheap signal goes first. An operator triaging an incident must not find
// their error exemplars evicted by healthy traffic.
func TestExemplarPriorityFillDisplacesHealthyBeforeErrors(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p := NewExemplarPolicy(ExemplarConfig{
		TracesPerServiceWindow: 4,
		HealthyRate:            1.0, // every healthy trace is eligible; the budget is the only bound
		LatencyThresholdMs:     500,
		StratumTopK:            10,
	})

	// Fill the window with healthy traces.
	healthy := make([]exemplarOffer, 0, 4)
	for i := 0; i < 4; i++ {
		healthy = append(healthy, exemplarOffer{
			traceID: fmt.Sprintf("h%031x", i), operation: "GET /a", status: "STATUS_CODE_OK", durationMs: 5,
		})
	}
	if got := offerAll(p, "checkout", ts, healthy); len(got) != 4 {
		t.Fatalf("healthy fill admitted %d, want 4", len(got))
	}

	// Now offer errors. Each must take a slot by displacing a healthy trace.
	errors := make([]exemplarOffer, 0, 4)
	for i := 0; i < 4; i++ {
		errors = append(errors, exemplarOffer{
			traceID: fmt.Sprintf("e%031x", i), operation: "GET /a", status: storage.StatusCodeError, durationMs: 5,
		})
	}
	if got := offerAll(p, "checkout", ts, errors); len(got) != 4 {
		t.Fatalf("error fill admitted %d, want 4 — errors must displace healthy", len(got))
	}

	selected := sortedSelected(p, "checkout", ts)
	if len(selected) != 4 {
		t.Fatalf("window holds %d traces, want the 4-trace budget", len(selected))
	}
	for _, id := range selected {
		if !strings.HasPrefix(id, "e") {
			t.Fatalf("healthy trace %s survived error pressure; selected=%v", id, selected)
		}
	}

	// And the reverse must NOT happen: healthy offered against a full error
	// window is refused, not swapped in.
	more := []exemplarOffer{{traceID: fmt.Sprintf("h%031x", 99), operation: "GET /a", status: "STATUS_CODE_OK", durationMs: 5}}
	if got := offerAll(p, "checkout", ts, more); len(got) != 0 {
		t.Fatalf("healthy trace displaced an error exemplar: %v", got)
	}
}

// TestExemplarOverlapConsumesOneSlot: a trace that is both slow and errored is
// one exemplar, not two (#161).
func TestExemplarOverlapConsumesOneSlot(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p := NewExemplarPolicy(ExemplarConfig{
		TracesPerServiceWindow: 2,
		LatencyThresholdMs:     500,
		StratumTopK:            10,
		HealthyRate:            0,
	})

	// One trace, error AND slow, offered across two spans.
	overlap := []exemplarOffer{
		{traceID: fmt.Sprintf("%032x", 1), operation: "GET /a", status: storage.StatusCodeError, durationMs: 900},
		{traceID: fmt.Sprintf("%032x", 1), operation: "GET /a", status: storage.StatusCodeError, durationMs: 900},
	}
	offerAll(p, "checkout", ts, overlap)

	if got := len(sortedSelected(p, "checkout", ts)); got != 1 {
		t.Fatalf("overlap trace occupies %d slots, want 1", got)
	}

	// The remaining slot must still be available to a different trace.
	other := []exemplarOffer{{traceID: fmt.Sprintf("%032x", 2), operation: "GET /b", status: storage.StatusCodeError, durationMs: 10}}
	if got := offerAll(p, "checkout", ts, other); len(got) != 1 {
		t.Fatalf("second trace refused — the overlap consumed both slots")
	}
}

// TestExemplarByteBudgetBindsIndependentlyOfCount: counts and bytes both bind
// and the first breach wins. A handful of enormous spans must not slip through
// just because the trace count is nowhere near its cap.
func TestExemplarByteBudgetBindsIndependentlyOfCount(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	rec := newCountingExemplarMetrics()
	p := NewExemplarPolicy(ExemplarConfig{
		TracesPerServiceWindow: 100, // count budget deliberately far from binding
		TracesGlobalWindow:     1000,
		BytesPerServiceWindow:  8192,
		BytesGlobalWindow:      1 << 20,
		MaxBytesPerTrace:       1 << 20,
		LatencyThresholdMs:     500,
		StratumTopK:            100,
		HealthyRate:            0,
		Metrics:                rec,
	})

	const spanBytes = 1024
	charged := 0
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("%032x", i)
		in := ExemplarSpan{
			Tenant: storage.DefaultTenantID, Service: "checkout", TraceID: id,
			Operation: fmt.Sprintf("op-%d", i), Status: storage.StatusCodeError, Timestamp: ts,
		}
		if !p.AdmitSpan(in) {
			continue
		}
		if p.ChargeSpan(in.Tenant, "checkout", id, ts, spanBytes) {
			charged++
		}
	}

	if want := 8192 / spanBytes; charged != want {
		t.Fatalf("charged %d spans, want %d — the byte budget did not bind", charged, want)
	}
	if selected := len(sortedSelected(p, "checkout", ts)); selected <= charged {
		t.Fatalf("selected %d traces but only %d were persisted; the count budget should not have bound", selected, charged)
	}
	_, dropped, _, truncations := rec.snapshot()
	if dropped["traces/"+exemplarReasonBudgetBytes] == 0 {
		t.Error("no budget_bytes drops recorded")
	}
	if dropped["traces/"+exemplarReasonBudgetCount] != 0 {
		t.Errorf("budget_count drops recorded (%d); only the byte budget should have bound", dropped["traces/"+exemplarReasonBudgetCount])
	}
	if truncations == 0 {
		t.Error("byte-budget refusal did not mark any trace truncated")
	}
}

// TestExemplarWarnLogsAreOptIn: WARN is off by default and budgeted when on.
// INFO/DEBUG are never raw in aggregate mode regardless.
func TestExemplarWarnLogsAreOptIn(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()

	admitN := func(p *ExemplarPolicy, severity string, n int) int {
		admitted := 0
		for i := 0; i < n; i++ {
			if p.AdmitLog(storage.DefaultTenantID, "checkout", severity, ts, 64) {
				admitted++
			}
		}
		return admitted
	}

	off := NewExemplarPolicy(ExemplarConfig{LogsWarnEnabled: false, LogsErrorPerServiceWindow: 50})
	if got := admitN(off, "WARN", 10); got != 0 {
		t.Fatalf("WARN admitted %d logs with the opt-in off, want 0", got)
	}

	on := NewExemplarPolicy(ExemplarConfig{LogsWarnEnabled: true, LogsWarnPerServiceWindow: 3, LogsErrorPerServiceWindow: 50})
	if got := admitN(on, "WARN", 10); got != 3 {
		t.Fatalf("WARN admitted %d logs, want the 3-log budget", got)
	}
	// The WARN budget is its own slot: it must not have eaten the ERROR budget.
	if got := admitN(on, "ERROR", 60); got != 50 {
		t.Fatalf("ERROR admitted %d logs, want the 50-log budget", got)
	}
	for _, sev := range []string{"INFO", "DEBUG", "TRACE"} {
		if got := admitN(on, sev, 5); got != 0 {
			t.Fatalf("%s admitted %d raw logs, want 0 — INFO/DEBUG are aggregate-only", sev, got)
		}
	}
}

// --- Export-path integration ---

// exemplarTraceRequest builds one export request of single-span traces for a
// service, all with the same status/duration.
func exemplarTraceRequest(service string, traces, operations int, status tracepb.Status_StatusCode, durationMs int, ts time.Time, seed int) *coltracepb.ExportTraceServiceRequest {
	spans := make([]*tracepb.Span, 0, traces)
	for i := 0; i < traces; i++ {
		spans = append(spans, &tracepb.Span{
			TraceId:           []byte(fmt.Sprintf("%08d%08d", seed, i)),
			SpanId:            []byte(fmt.Sprintf("%08d", i)),
			Name:              fmt.Sprintf("GET /op-%d", i%operations),
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: uint64(ts.UnixNano()), // #nosec G115 -- test timestamps are positive
			EndTimeUnixNano:   uint64(ts.Add(time.Duration(durationMs) * time.Millisecond).UnixNano()),
			Status:            &tracepb.Status{Code: status},
		})
	}
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}}},
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
		}},
	}
}

func exemplarTestPolicy(cfg ExemplarConfig) *ExemplarPolicy {
	if cfg.LatencyThresholdMs == 0 {
		cfg.LatencyThresholdMs = 500
	}
	return NewExemplarPolicy(cfg)
}

// TestExemplarErrorStormKeepsAggregateExactAndRawBounded is the acceptance
// criterion of #176 and the reason #161 exists at all: at 100% error traffic
// the aggregate error count must be exact while raw persistence stays flat at
// the caps. If raw persistence tracks the error rate, an outage has turned the
// platform into its own denial of service.
func TestExemplarErrorStormKeepsAggregateExactAndRawBounded(t *testing.T) {
	now := time.Now().UTC() // Export reads arrival from the wall clock
	repo, db := newAggregateTestRepo(t)
	srv := NewTraceServer(repo, nil, aggTestConfig())
	engine := newAggregateEngine(t, now)
	srv.SetAggregateEngine(engine)

	rec := newCountingExemplarMetrics()
	const perServiceBudget = 25
	srv.SetExemplarPolicy(exemplarTestPolicy(ExemplarConfig{
		TracesPerServiceWindow: perServiceBudget,
		TracesGlobalWindow:     1500,
		StratumTopK:            5,
		HealthyRate:            0.005,
		Metrics:                rec,
	}))

	// Ten batches of 500 errored traces: 5,000 error traces, 100% error rate.
	const batches, perBatch, operations = 10, 500, 10
	for b := 0; b < batches; b++ {
		req := exemplarTraceRequest("checkout", perBatch, operations, tracepb.Status_STATUS_CODE_ERROR, 5, now, b)
		if _, err := srv.Export(context.Background(), req); err != nil {
			t.Fatalf("Export batch %d: %v", b, err)
		}
	}

	const total = batches * perBatch

	// Aggregate accounting is exact — every error counted, no exceptions.
	count, errCount := engine.Snapshot().Totals(aggregate.SignalTraceOp)
	if count != total {
		t.Errorf("aggregate count = %d, want %d — accounting must precede retention", count, total)
	}
	if errCount != total {
		t.Errorf("aggregate error count = %d, want %d", errCount, total)
	}

	// Raw persistence is bounded. Over-retention from displacement is allowed
	// and counted; unbounded growth with the error rate is not.
	persistedSpans := countAggSpans(t, db)
	_, dropped, evictions, _ := rec.snapshot()
	maxRaw := int64(perServiceBudget + evictions)
	if persistedSpans > maxRaw {
		t.Fatalf("persisted %d spans; budget %d plus %d displacements allows at most %d",
			persistedSpans, perServiceBudget, evictions, maxRaw)
	}
	if persistedSpans >= total/10 {
		t.Fatalf("persisted %d of %d spans — raw retention is tracking the error rate", persistedSpans, total)
	}
	if dropped["traces/"+exemplarReasonStratum]+dropped["traces/"+exemplarReasonBudgetCount] == 0 {
		t.Error("no exemplar drops recorded during a 5,000-trace storm")
	}
	t.Logf("storm: aggregate=%d errors=%d persisted_spans=%d evictions=%d drops=%v",
		count, errCount, persistedSpans, evictions, dropped)
}

// TestExemplarRetiresSamplerInAggregateMode: with the exemplar policy wired,
// SAMPLING_RATE cannot influence what is persisted. Exactly one raw-retention
// policy governs the active path per mode (#161).
func TestExemplarRetiresSamplerInAggregateMode(t *testing.T) {
	now := time.Now().UTC()

	run := func(rate float64) int64 {
		repo, db := newAggregateTestRepo(t)
		srv := NewTraceServer(repo, nil, aggTestConfig())
		srv.SetAggregateEngine(newAggregateEngine(t, now))
		srv.SetExemplarPolicy(exemplarTestPolicy(ExemplarConfig{
			TracesPerServiceWindow: 25,
			StratumTopK:            5,
			HealthyRate:            0.005,
		}))
		// A sampler is deliberately wired alongside. In aggregate mode it must
		// never be consulted — this is the guard against a future refactor
		// quietly reinstating it.
		srv.SetSampler(NewSampler(rate, true, 500))
		req := exemplarTraceRequest("checkout", 400, 10, tracepb.Status_STATUS_CODE_ERROR, 5, now, 7)
		if _, err := srv.Export(context.Background(), req); err != nil {
			t.Fatalf("Export at rate %v: %v", rate, err)
		}
		return countAggSpans(t, db)
	}

	full := run(1.0)
	sampled := run(0.05)
	if full != sampled {
		t.Fatalf("SAMPLING_RATE changed exemplar retention: %d spans at 1.0 vs %d at 0.05", full, sampled)
	}
	if full == 0 {
		t.Fatal("no spans persisted at all — the test would be vacuous")
	}
}

// TestExemplarTruncationMetadataPersists checks the #163 contract end to end:
// a trace cut short by the per-trace span bound lands in the database with
// truncated=true and honest retained/observed counts.
func TestExemplarTruncationMetadataPersists(t *testing.T) {
	now := time.Now().UTC()
	repo, db := newAggregateTestRepo(t)
	srv := NewTraceServer(repo, nil, aggTestConfig())
	srv.SetAggregateEngine(newAggregateEngine(t, now))

	const maxSpans = 5
	rec := newCountingExemplarMetrics()
	srv.SetExemplarPolicy(exemplarTestPolicy(ExemplarConfig{
		TracesPerServiceWindow: 25,
		StratumTopK:            5,
		MaxSpansPerTrace:       maxSpans,
		HealthyRate:            0,
		Metrics:                rec,
	}))

	// One trace, 20 errored spans, delivered across two batches so the
	// truncation only becomes known on the second one.
	const totalSpans = 20
	traceID := []byte("0123456789abcdef")
	mkBatch := func(from, to int) *coltracepb.ExportTraceServiceRequest {
		spans := make([]*tracepb.Span, 0, to-from)
		for i := from; i < to; i++ {
			spans = append(spans, &tracepb.Span{
				TraceId:           traceID,
				SpanId:            []byte(fmt.Sprintf("%08d", i)),
				Name:              "GET /checkout",
				StartTimeUnixNano: uint64(now.UnixNano()), // #nosec G115 -- test timestamps are positive
				EndTimeUnixNano:   uint64(now.Add(time.Millisecond).UnixNano()),
				Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
			})
		}
		return &coltracepb.ExportTraceServiceRequest{
			ResourceSpans: []*tracepb.ResourceSpans{{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
					{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}}},
				}},
				ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
			}},
		}
	}
	for _, b := range [][2]int{{0, 3}, {3, totalSpans}} {
		if _, err := srv.Export(context.Background(), mkBatch(b[0], b[1])); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	var tr storage.Trace
	if err := db.Where("trace_id = ?", fmt.Sprintf("%x", traceID)).First(&tr).Error; err != nil {
		t.Fatalf("load trace: %v", err)
	}
	if tr.Truncated == nil || !*tr.Truncated {
		t.Fatalf("trace not marked truncated: %+v", tr.Truncated)
	}
	if tr.RetainedSpanCount == nil || *tr.RetainedSpanCount != maxSpans {
		t.Fatalf("retained_span_count = %v, want %d", tr.RetainedSpanCount, maxSpans)
	}
	if tr.ObservedSpanCount == nil || *tr.ObservedSpanCount != totalSpans {
		t.Fatalf("observed_span_count = %v, want %d", tr.ObservedSpanCount, totalSpans)
	}
	// The claim must match the rows actually on disk.
	var persisted int64
	if err := db.Model(&storage.Span{}).Where("trace_id = ?", fmt.Sprintf("%x", traceID)).Count(&persisted).Error; err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if persisted != int64(maxSpans) {
		t.Fatalf("persisted %d spans, but the trace claims %d retained", persisted, maxSpans)
	}
	if _, _, _, truncations := rec.snapshot(); truncations != 1 {
		t.Errorf("truncation counter = %d, want 1", truncations)
	}
}

// TestExemplarUntruncatedTraceLeavesMetadataNull: the truncation columns are a
// claim, not a default. A trace retained whole must leave them NULL so a reader
// can tell "not truncated" from "no claim made".
func TestExemplarUntruncatedTraceLeavesMetadataNull(t *testing.T) {
	now := time.Now().UTC()
	repo, db := newAggregateTestRepo(t)
	srv := NewTraceServer(repo, nil, aggTestConfig())
	srv.SetAggregateEngine(newAggregateEngine(t, now))
	srv.SetExemplarPolicy(exemplarTestPolicy(ExemplarConfig{
		TracesPerServiceWindow: 25, StratumTopK: 5, MaxSpansPerTrace: 500, HealthyRate: 0,
	}))

	req := exemplarTraceRequest("checkout", 3, 3, tracepb.Status_STATUS_CODE_ERROR, 5, now, 1)
	if _, err := srv.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}

	var traces []storage.Trace
	if err := db.Find(&traces).Error; err != nil {
		t.Fatalf("load traces: %v", err)
	}
	if len(traces) == 0 {
		t.Fatal("no traces persisted")
	}
	for _, tr := range traces {
		if tr.Truncated != nil || tr.RetainedSpanCount != nil || tr.ObservedSpanCount != nil {
			t.Fatalf("untruncated trace %s carries a truncation claim: %+v", tr.TraceID, tr)
		}
	}
}

// TestExemplarLogBudgetBindsOnExportPath wires the policy into LogsServer and
// checks the raw-log budget through the real persist path.
func TestExemplarLogBudgetBindsOnExportPath(t *testing.T) {
	now := time.Now().UTC()
	repo, db := newAggregateTestRepo(t)
	srv := NewLogsServer(repo, nil, aggTestConfig())
	engine := newAggregateEngine(t, now)
	srv.SetAggregateEngine(engine)

	const errorBudget = 6
	srv.SetExemplarPolicy(exemplarTestPolicy(ExemplarConfig{
		LogsErrorPerServiceWindow: errorBudget,
		LogsWarnEnabled:           false,
	}))

	const errorLogs, infoLogs = 50, 20
	records := make([]*logspb.LogRecord, 0, errorLogs+infoLogs)
	for i := 0; i < errorLogs; i++ {
		records = append(records, &logspb.LogRecord{
			TimeUnixNano: uint64(now.UnixNano()), // #nosec G115 -- test timestamps are positive
			SeverityText: "ERROR",
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprintf("upstream failed %d", i)}},
		})
	}
	for i := 0; i < infoLogs; i++ {
		records = append(records, &logspb.LogRecord{
			TimeUnixNano: uint64(now.UnixNano()), // #nosec G115 -- test timestamps are positive
			SeverityText: "INFO",
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprintf("served %d", i)}},
		})
	}
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}}},
		}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}},
	}}}

	if _, err := srv.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}

	var persisted int64
	if err := db.Model(&storage.Log{}).Count(&persisted).Error; err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if persisted != errorBudget {
		t.Fatalf("persisted %d logs, want the %d-log ERROR budget (INFO is aggregate-only)", persisted, errorBudget)
	}
	// Aggregate accounting still saw every record.
	if count, _ := engine.Snapshot().Totals(aggregate.SignalLog); count != errorLogs+infoLogs {
		t.Errorf("aggregate log count = %d, want %d", count, errorLogs+infoLogs)
	}
}

// TestExemplarInertInLegacyAndShadowModes: with no policy wired, nothing about
// the persisted output changes. This is the mode-ownership guard — legacy and
// aggregate-shadow keep the Sampler and see none of this code.
func TestExemplarInertInLegacyAndShadowModes(t *testing.T) {
	now := time.Now().UTC()
	req := exemplarTraceRequest("checkout", 40, 5, tracepb.Status_STATUS_CODE_ERROR, 5, now, 3)

	persistedIn := func(withEngine bool) int64 {
		repo, db := newAggregateTestRepo(t)
		srv := NewTraceServer(repo, nil, aggTestConfig())
		if withEngine {
			srv.SetAggregateEngine(newAggregateEngine(t, now))
		}
		if _, err := srv.Export(context.Background(), req); err != nil {
			t.Fatalf("Export: %v", err)
		}
		return countAggSpans(t, db)
	}

	legacy := persistedIn(false)
	shadow := persistedIn(true)
	if legacy != 40 || shadow != 40 {
		t.Fatalf("legacy persisted %d and shadow persisted %d spans, want 40 each — the exemplar policy leaked into a mode that does not own it", legacy, shadow)
	}
}

// TestExemplarConcurrentAdmissionIsRaceFree exercises the sharded state from
// many goroutines. Run under -race; the assertion is only that the budget still
// holds afterwards.
func TestExemplarConcurrentAdmissionIsRaceFree(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p := NewExemplarPolicy(ExemplarConfig{
		TracesPerServiceWindow: 25,
		TracesGlobalWindow:     100,
		StratumTopK:            5,
		HealthyRate:            0.005,
		LatencyThresholdMs:     500,
	})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				id := fmt.Sprintf("%016x%016x", g, i)
				in := ExemplarSpan{
					Tenant: storage.DefaultTenantID, Service: fmt.Sprintf("svc-%d", i%4), TraceID: id,
					Operation: fmt.Sprintf("op-%d", i%7), Status: storage.StatusCodeError, Timestamp: ts,
				}
				if p.AdmitSpan(in) {
					p.ChargeSpan(in.Tenant, in.Service, id, ts, 256)
					p.TraceStats(in.Tenant, in.Service, id, ts)
				}
			}
		}(g)
	}
	wg.Wait()

	for i := 0; i < 4; i++ {
		svc := fmt.Sprintf("svc-%d", i)
		if got := len(p.SelectedTraces(storage.DefaultTenantID, svc, ts)); got > 25 {
			t.Fatalf("%s holds %d exemplars, over the 25 budget", svc, got)
		}
	}
}
