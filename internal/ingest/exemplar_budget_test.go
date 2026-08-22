package ingest

import (
	"fmt"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// Reservation lifecycle, synthesized-log metering and disk shedding (#201
// Q3/Q4/Q5). The recorder, offer helpers and admission scaffolding are shared
// with exemplar_test.go — these tests add cases, not a second harness.

// budgetPolicy builds a policy with the shared recorder and the caller's
// overrides applied on top of the frozen defaults.
func budgetPolicy(t *testing.T, mutate func(*ExemplarConfig)) (*ExemplarPolicy, *countingExemplarMetrics) {
	t.Helper()
	rec := newCountingExemplarMetrics()
	cfg := ExemplarConfig{
		LatencyThresholdMs: 500,
		Metrics:            rec,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewExemplarPolicy(cfg), rec
}

// admitOne selects one trace and returns its span input.
func admitOne(t *testing.T, p *ExemplarPolicy, traceID string, ts time.Time) ExemplarSpan {
	t.Helper()
	in := ExemplarSpan{
		Tenant:    storage.DefaultTenantID,
		Service:   "checkout",
		TraceID:   traceID,
		Operation: "GET /checkout",
		Status:    storage.StatusCodeError,
		Timestamp: ts,
	}
	if !p.AdmitSpan(in) {
		t.Fatalf("AdmitSpan(%s) refused a first error span", traceID)
	}
	return in
}

// TestReservationCommitMovesBytesFromReservedToCommitted is the base case of
// the lifecycle: reserved bytes bind the budget immediately, and commit is a
// state change rather than a second charge.
func TestReservationCommitMovesBytesFromReservedToCommitted(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, _ := budgetPolicy(t, nil)
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)

	res := p.NewReservation()
	if !p.ReserveSpan(res, in.Tenant, in.Service, in.TraceID, ts, 4096) {
		t.Fatal("ReserveSpan refused a span inside every budget")
	}
	committed, reserved := p.WindowBytes(ts)
	if committed != 0 || reserved != 4096 {
		t.Fatalf("after reserve: committed=%d reserved=%d, want 0/4096", committed, reserved)
	}
	if svcCommitted, svcReserved := p.ServiceWindowBytes(in.Tenant, in.Service, ts); svcCommitted != 0 || svcReserved != 4096 {
		t.Fatalf("service window after reserve: committed=%d reserved=%d, want 0/4096", svcCommitted, svcReserved)
	}

	res.Commit()
	committed, reserved = p.WindowBytes(ts)
	if committed != 4096 || reserved != 0 {
		t.Fatalf("after commit: committed=%d reserved=%d, want 4096/0", committed, reserved)
	}

	// Idempotent: a second Commit must not double-charge.
	res.Commit()
	if committed, _ = p.WindowBytes(ts); committed != 4096 {
		t.Fatalf("second Commit charged again: committed=%d, want 4096", committed)
	}
}

// TestReservationReleaseReturnsBytes covers the only legitimate refund: rows
// that reached no destination at all.
func TestReservationReleaseReturnsBytes(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, _ := budgetPolicy(t, nil)
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)

	res := p.NewReservation()
	if !p.ReserveSpan(res, in.Tenant, in.Service, in.TraceID, ts, 4096) {
		t.Fatal("ReserveSpan refused a span inside every budget")
	}
	res.Release()

	committed, reserved := p.WindowBytes(ts)
	if committed != 0 || reserved != 0 {
		t.Fatalf("after release: committed=%d reserved=%d, want 0/0", committed, reserved)
	}
	if svcCommitted, svcReserved := p.ServiceWindowBytes(in.Tenant, in.Service, ts); svcCommitted != 0 || svcReserved != 0 {
		t.Fatalf("service window after release: committed=%d reserved=%d, want 0/0", svcCommitted, svcReserved)
	}

	// Release after Release must not underflow the pools.
	res.Release()
	if committed, reserved = p.WindowBytes(ts); committed != 0 || reserved != 0 {
		t.Fatalf("double release moved the pools: committed=%d reserved=%d", committed, reserved)
	}
}

// TestCommittedBytesSurvivePostAcceptanceEviction is the property #201 Q4 was
// written for: once a destination accepted the rows, displacing the trace
// must NOT hand the bytes back. Those bytes are on disk; refunding them would
// let the window write past its cap.
func TestCommittedBytesSurvivePostAcceptanceEviction(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	// One trace slot per service, so the second trace must displace the first.
	p, rec := budgetPolicy(t, func(c *ExemplarConfig) {
		c.TracesPerServiceWindow = 1
		c.StratumTopK = 1
	})

	// A high hash first so a lower one can displace it.
	var high, low string
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("%032x", i)
		if high == "" || exemplarHash(id) > exemplarHash(high) {
			high = id
		}
		if low == "" || exemplarHash(id) < exemplarHash(low) {
			low = id
		}
	}

	first := admitOne(t, p, high, ts)
	res := p.NewReservation()
	if !p.ReserveSpan(res, first.Tenant, first.Service, first.TraceID, ts, 8192) {
		t.Fatal("ReserveSpan refused the first span")
	}
	res.Commit() // the queue accepted the batch: the bytes are written

	if committed, _ := p.WindowBytes(ts); committed != 8192 {
		t.Fatalf("committed=%d before eviction, want 8192", committed)
	}
	slotsBefore := p.GlobalTraceSlots(ts)

	// Displace it with a strictly better-ranked trace.
	second := ExemplarSpan{
		Tenant: storage.DefaultTenantID, Service: "checkout", TraceID: low,
		Operation: "GET /checkout", Status: storage.StatusCodeError, Timestamp: ts,
	}
	if !p.AdmitSpan(second) {
		t.Fatal("a strictly better-ranked error trace was refused the full window")
	}
	if _, _, evictions, _ := rec.snapshot(); evictions == 0 {
		t.Fatal("no eviction recorded; the test did not exercise displacement")
	}

	committed, reserved := p.WindowBytes(ts)
	if committed != 8192 {
		t.Fatalf("eviction refunded committed bytes: committed=%d, want 8192 — bytes on disk are never refunded", committed)
	}
	if reserved != 0 {
		t.Fatalf("reserved=%d after eviction, want 0", reserved)
	}

	// Count slots, unlike bytes, ARE released on eviction: a slot is a seat,
	// not a byte. The evicted trace returned its seat and the newcomer took
	// one, so the instance-wide count is unchanged.
	if got := p.GlobalTraceSlots(ts); got != slotsBefore {
		t.Fatalf("global trace slots = %d after displacement, want %d (evicted trace must return its slot)", got, slotsBefore)
	}
}

// TestReservedBytesBindTheBudgetBeforeCommit: an uncommitted reservation is a
// row about to be written, so it must count against the cap. Otherwise one
// Export per window overshoots by the whole cap.
func TestReservedBytesBindTheBudgetBeforeCommit(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, rec := budgetPolicy(t, func(c *ExemplarConfig) {
		c.BytesGlobalWindow = 8192
		c.BytesPerServiceWindow = 8192
	})
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)

	res := p.NewReservation()
	if !p.ReserveSpan(res, in.Tenant, in.Service, in.TraceID, ts, 8192) {
		t.Fatal("first reservation should fit exactly")
	}
	// Nothing committed yet — and the budget must still be full.
	if p.ReserveSpan(res, in.Tenant, in.Service, in.TraceID, ts, 1) {
		t.Fatal("a second byte was reserved past a full window: reserved bytes are not binding the cap")
	}
	if _, dropped, _, _ := rec.snapshot(); dropped["traces/"+exemplarReasonBudgetBytes] == 0 {
		t.Error("over-budget reservation was not counted as a budget_bytes drop")
	}
}

// TestReservationMergeSettlesOnce: merged reservations settle exactly once, so
// the per-resource goroutines can be folded into one per-tenant batch.
func TestReservationMergeSettlesOnce(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, _ := budgetPolicy(t, nil)
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)

	a := p.NewReservation()
	b := p.NewReservation()
	if !p.ReserveSpan(a, in.Tenant, in.Service, in.TraceID, ts, 1024) {
		t.Fatal("reserve a")
	}
	if !p.ReserveSpan(b, in.Tenant, in.Service, in.TraceID, ts, 2048) {
		t.Fatal("reserve b")
	}
	a.Merge(b)
	if got := a.Bytes(); got != 3072 {
		t.Fatalf("merged reservation holds %d bytes, want 3072", got)
	}
	a.Commit()
	b.Release() // neutralized by the merge; must be a no-op

	committed, reserved := p.WindowBytes(ts)
	if committed != 3072 || reserved != 0 {
		t.Fatalf("after merge+commit: committed=%d reserved=%d, want 3072/0", committed, reserved)
	}
}

// --- Q3: synthesized-log metering -------------------------------------------

// TestSynthesizedLogsAreMetered: they ride the selected trace and they are not
// weightless. Bytes land on the same pools as spans.
func TestSynthesizedLogsAreMetered(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, _ := budgetPolicy(t, nil)
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)

	res := p.NewReservation()
	if !p.ReserveSynthesizedLog(res, in.Tenant, in.Service, in.TraceID, "span-1", "ERROR", ts, 100) {
		t.Fatal("ReserveSynthesizedLog refused a log inside every budget")
	}
	want := int64(100 + logRowFixedBytes)
	if _, reserved := p.WindowBytes(ts); reserved != want {
		t.Fatalf("synthesized log reserved %d bytes, want %d", reserved, want)
	}
	res.Commit()
	if committed, _ := p.WindowBytes(ts); committed != want {
		t.Fatalf("synthesized log committed %d bytes, want %d", committed, want)
	}
}

// TestSynthesizedLogsDoNotConsumeTheClientLogQuota: the ordinary log-exemplar
// budget exists for logs a client actually sent. Charging synthesized rows to
// it would silently evict real ones.
func TestSynthesizedLogsDoNotConsumeTheClientLogQuota(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, _ := budgetPolicy(t, func(c *ExemplarConfig) { c.LogsErrorPerServiceWindow = 2 })
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)

	res := p.NewReservation()
	for i := 0; i < 5; i++ {
		if !p.ReserveSynthesizedLog(res, in.Tenant, in.Service, in.TraceID, "span-1", "ERROR", ts, 10) {
			t.Fatalf("synthesized log %d refused below the per-span cap", i)
		}
	}
	// The client log quota must be untouched: two client ERROR logs still fit.
	for i := 0; i < 2; i++ {
		if !p.ReserveLog(res, in.Tenant, in.Service, "ERROR", ts, 10) {
			t.Fatalf("client log %d refused: synthesized logs ate the client quota", i)
		}
	}
	if p.ReserveLog(res, in.Tenant, in.Service, "ERROR", ts, 10) {
		t.Fatal("client log quota did not bind at 2")
	}
}

// TestSynthesizedLogPerSpanCap: one pathological span cannot write an
// unbounded number of log rows.
func TestSynthesizedLogPerSpanCap(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, rec := budgetPolicy(t, func(c *ExemplarConfig) { c.SynthLogsPerSpan = 3 })
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)

	res := p.NewReservation()
	admitted := 0
	for i := 0; i < 10; i++ {
		if p.ReserveSynthesizedLog(res, in.Tenant, in.Service, in.TraceID, "span-1", "ERROR", ts, 10) {
			admitted++
		}
	}
	if admitted != 3 {
		t.Fatalf("admitted %d synthesized logs for one span, want 3", admitted)
	}
	_, dropped, _, truncations := rec.snapshot()
	if dropped["logs/"+exemplarReasonSynthPerSpan] == 0 {
		t.Error("per-span refusals were not counted as synth_per_span")
	}
	if truncations == 0 {
		t.Error("per-span refusal did not mark the trace truncated")
	}
	stats, ok := p.TraceStats(in.Tenant, in.Service, in.TraceID, ts)
	if !ok || !stats.Truncated {
		t.Fatalf("TraceStats truncated=%v ok=%v, want a truncated trace", stats.Truncated, ok)
	}

	// A different span of the same trace gets its own per-span allowance.
	if !p.ReserveSynthesizedLog(res, in.Tenant, in.Service, in.TraceID, "span-2", "ERROR", ts, 10) {
		t.Fatal("a second span was refused its own per-span allowance")
	}
}

// TestSynthesizedLogPerTraceCap: the per-trace cap binds across spans.
func TestSynthesizedLogPerTraceCap(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, rec := budgetPolicy(t, func(c *ExemplarConfig) {
		c.SynthLogsPerSpan = 2
		c.SynthLogsPerTrace = 5
	})
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)

	res := p.NewReservation()
	admitted := 0
	for span := 0; span < 10; span++ {
		for i := 0; i < 2; i++ {
			if p.ReserveSynthesizedLog(res, in.Tenant, in.Service, in.TraceID, fmt.Sprintf("span-%d", span), "ERROR", ts, 10) {
				admitted++
			}
		}
	}
	if admitted != 5 {
		t.Fatalf("admitted %d synthesized logs for one trace, want 5", admitted)
	}
	if _, dropped, _, _ := rec.snapshot(); dropped["logs/"+exemplarReasonSynthPerTrace] == 0 {
		t.Error("per-trace refusals were not counted as synth_per_trace")
	}
}

// TestSynthesizedLogByteBudgetMarksTruncated: byte refusals are counted as
// budget_bytes and stamp the trace, so the gap is in the data.
func TestSynthesizedLogByteBudgetMarksTruncated(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, rec := budgetPolicy(t, func(c *ExemplarConfig) {
		c.MaxBytesPerTrace = 1024
		c.SynthLogsPerSpan = 1000
		c.SynthLogsPerTrace = 1000
	})
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)

	res := p.NewReservation()
	admitted := 0
	for i := 0; i < 100; i++ {
		if p.ReserveSynthesizedLog(res, in.Tenant, in.Service, in.TraceID, "span-1", "ERROR", ts, 64) {
			admitted++
		}
	}
	perLog := int64(64 + logRowFixedBytes)
	if want := int(1024 / perLog); admitted != want {
		t.Fatalf("admitted %d synthesized logs, want %d — the per-trace byte budget did not bind", admitted, want)
	}
	_, dropped, _, truncations := rec.snapshot()
	if dropped["logs/"+exemplarReasonBudgetBytes] == 0 {
		t.Error("byte refusals were not counted as budget_bytes")
	}
	if truncations == 0 {
		t.Error("byte refusal did not mark the trace truncated")
	}
}

// TestSynthesizedLogsRefusedForUnselectedTrace: a synthesized log whose span
// is not retained would be dangling evidence.
func TestSynthesizedLogsRefusedForUnselectedTrace(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, _ := budgetPolicy(t, nil)
	if p.ReserveSynthesizedLog(nil, storage.DefaultTenantID, "checkout", "never-selected", "span-1", "ERROR", ts, 10) {
		t.Fatal("a synthesized log was admitted for a trace the policy never selected")
	}
}

// TestSynthesizedLogSeverityFloorUnchanged: INFO/DEBUG remain aggregate-only.
func TestSynthesizedLogSeverityFloorUnchanged(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, _ := budgetPolicy(t, nil)
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)
	for _, sev := range []string{"INFO", "DEBUG", "TRACE", "WARN"} {
		if p.ReserveSynthesizedLog(nil, in.Tenant, in.Service, in.TraceID, "span-1", sev, ts, 10) {
			t.Fatalf("severity %s was raw-retained; only ERROR/FATAL are (WARN is opt-in)", sev)
		}
	}
}

// --- Q5: shedding ------------------------------------------------------------

// TestSheddingErrorsOnlyKeepsErrors: at >=90% healthy and slow raw retention
// is off and errors still land.
func TestSheddingErrorsOnlyKeepsErrors(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, rec := budgetPolicy(t, func(c *ExemplarConfig) { c.HealthyRate = 1.0 })
	p.SetShedding(storage.SheddingErrorsOnly)

	healthy := ExemplarSpan{
		Tenant: storage.DefaultTenantID, Service: "checkout", TraceID: fmt.Sprintf("%032x", 7),
		Operation: "GET /checkout", Status: "STATUS_CODE_OK", Timestamp: ts,
	}
	if p.AdmitSpan(healthy) {
		t.Fatal("a healthy span was admitted while shedding to errors only")
	}
	slow := healthy
	slow.TraceID = fmt.Sprintf("%032x", 8)
	slow.DurationMs = 5000
	if p.AdmitSpan(slow) {
		t.Fatal("a slow span was admitted while shedding to errors only")
	}
	errSpan := healthy
	errSpan.TraceID = fmt.Sprintf("%032x", 9)
	errSpan.Status = storage.StatusCodeError
	if !p.AdmitSpan(errSpan) {
		t.Fatal("an error span was refused while shedding to errors only")
	}
	if _, dropped, _, _ := rec.snapshot(); dropped["traces/"+exemplarReasonShedErrorsOnly] != 2 {
		t.Fatalf("shed_errors_only drops = %d, want 2", dropped["traces/"+exemplarReasonShedErrorsOnly])
	}

	// WARN logs go with the healthy traffic; ERROR logs stay.
	pw, _ := budgetPolicy(t, func(c *ExemplarConfig) { c.LogsWarnEnabled = true })
	pw.SetShedding(storage.SheddingErrorsOnly)
	if pw.ReserveLog(nil, storage.DefaultTenantID, "checkout", "WARN", ts, 16) {
		t.Fatal("a WARN log was admitted while shedding to errors only")
	}
	if !pw.ReserveLog(nil, storage.DefaultTenantID, "checkout", "ERROR", ts, 16) {
		t.Fatal("an ERROR log was refused while shedding to errors only")
	}
}

// TestSheddingRawOffRefusesEverything: at >=95% nothing raw is admitted, and
// the DLQ fallback closes with it.
func TestSheddingRawOffRefusesEverything(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, rec := budgetPolicy(t, nil)
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)
	p.SetShedding(storage.SheddingRawOff)

	next := in
	next.TraceID = fmt.Sprintf("%032x", 2)
	if p.AdmitSpan(next) {
		t.Fatal("an error span was admitted at raw-off")
	}
	if p.ReserveLog(nil, in.Tenant, in.Service, "ERROR", ts, 16) {
		t.Fatal("an ERROR log was admitted at raw-off")
	}
	if p.ReserveSynthesizedLog(nil, in.Tenant, in.Service, in.TraceID, "span-1", "ERROR", ts, 16) {
		t.Fatal("a synthesized log was admitted at raw-off")
	}
	if !p.DLQDisabled() {
		t.Fatal("the exemplar DLQ fallback is still open at raw-off")
	}
	_, dropped, _, _ := rec.snapshot()
	if dropped["traces/"+exemplarReasonShedRawOff] == 0 || dropped["logs/"+exemplarReasonShedRawOff] == 0 {
		t.Fatalf("raw-off drops were not counted: %v", dropped)
	}
}

// TestSheddingCutsAnAlreadySelectedTraceShort: shedding outranks the
// complete-retained-trace contract, and says so in the data.
func TestSheddingCutsAnAlreadySelectedTraceShort(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, _ := budgetPolicy(t, func(c *ExemplarConfig) { c.HealthyRate = 1.0 })

	healthy := ExemplarSpan{
		Tenant: storage.DefaultTenantID, Service: "checkout", TraceID: fmt.Sprintf("%032x", 7),
		Operation: "GET /checkout", Status: "STATUS_CODE_OK", Timestamp: ts,
	}
	if !p.AdmitSpan(healthy) {
		t.Fatal("healthy span refused before shedding")
	}
	p.SetShedding(storage.SheddingErrorsOnly)
	if p.AdmitSpan(healthy) {
		t.Fatal("a later span of a selected healthy trace was admitted while shedding")
	}
	stats, ok := p.TraceStats(healthy.Tenant, healthy.Service, healthy.TraceID, ts)
	if !ok || !stats.Truncated {
		t.Fatalf("TraceStats truncated=%v ok=%v: a trace cut short by shedding must be stamped", stats.Truncated, ok)
	}
}

// TestNilPolicyShedAccessorsAreSafe: legacy and shadow mode carry a nil
// policy and must not need a branch at every call site.
func TestNilPolicyShedAccessorsAreSafe(t *testing.T) {
	var p *ExemplarPolicy
	p.SetShedding(storage.SheddingRawOff)
	if p.Shedding() != storage.SheddingNone {
		t.Fatal("nil policy reported a shedding state")
	}
	if p.DLQDisabled() {
		t.Fatal("nil policy closed the DLQ")
	}
	if r := p.NewReservation(); r != nil {
		t.Fatal("nil policy handed out a reservation")
	}
	var r *ExemplarReservation
	r.Commit()
	r.Release()
	r.Merge(nil)
	if r.Bytes() != 0 || r.Len() != 0 {
		t.Fatal("nil reservation reported bytes")
	}
}

// --- Q4 at the Export boundary ----------------------------------------------

// exportPolicy wires a TraceServer in aggregate mode over pipeline p with a
// live exemplar policy, and returns the policy so the byte pools can be read.
func exportPolicy(t *testing.T, p *Pipeline, now time.Time) (*TraceServer, *ExemplarPolicy) {
	t.Helper()
	pol := NewExemplarPolicy(ExemplarConfig{
		LatencyThresholdMs: 500,
		Metrics:            newCountingExemplarMetrics(),
	})
	srv := NewTraceServer(nil, nil, aggTestConfig())
	srv.SetPipeline(p)
	srv.SetAggregateEngine(ackEngine(t, "aggregate", now))
	srv.SetExemplarPolicy(pol)
	return srv, pol
}

// TestExportCommitsReservationsOnQueueAcceptance: the queue took the rows, so
// the bytes are a charge, not a reservation.
func TestExportCommitsReservationsOnQueueAcceptance(t *testing.T) {
	now := time.Now().UTC()
	p := NewPipeline(&fakeWriter{}, nil, PipelineConfig{Capacity: 16, Workers: 1})
	srv, pol := exportPolicy(t, p, now)

	if _, err := srv.Export(t.Context(), ackErrorSpanRequest(3, now)); err != nil {
		t.Fatalf("Export: %v", err)
	}
	committed, reserved := pol.WindowBytes(now)
	if committed == 0 {
		t.Fatal("nothing was committed after the queue accepted the batch")
	}
	if reserved != 0 {
		t.Fatalf("reserved=%d after acceptance, want 0 — the reservation never settled", reserved)
	}
}

// TestExportReleasesReservationsWhenNothingAcceptsTheBatch: the primary queue
// is full and there is no DLQ, so the rows reached nothing. Their bytes must
// go back, or the window is charged for writes that never happened.
func TestExportReleasesReservationsWhenNothingAcceptsTheBatch(t *testing.T) {
	now := time.Now().UTC()
	p := saturatedPipeline(t, nil, nil) // no DLQ sink
	srv, pol := exportPolicy(t, p, now)

	// Aggregate mode acknowledges the Export even though the raw rows are
	// lost — that is the #196 contract and it does not change here.
	resp, err := srv.Export(t.Context(), ackErrorSpanRequest(3, now))
	if err != nil {
		t.Fatalf("Export must still succeed once the aggregate commit landed: %v", err)
	}
	if resp.GetPartialSuccess() == nil {
		t.Fatal("partial_success not populated for permanently lost exemplars")
	}
	committed, reserved := pol.WindowBytes(now)
	if committed != 0 || reserved != 0 {
		t.Fatalf("committed=%d reserved=%d after a batch nothing accepted, want 0/0", committed, reserved)
	}
}

// TestReleaseReturnsSpanSlotsButNotSynthesizedOnes: releasing a reservation
// hands back the retained-SPAN count for spans and leaves it alone for
// synthesized logs, which never took one. Getting this wrong drives
// retained_span_count negative on the persisted trace row.
func TestReleaseReturnsSpanSlotsButNotSynthesizedOnes(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0).UTC()
	p, _ := budgetPolicy(t, nil)
	in := admitOne(t, p, fmt.Sprintf("%032x", 1), ts)

	keep := p.NewReservation()
	if !p.ReserveSpan(keep, in.Tenant, in.Service, in.TraceID, ts, 512) {
		t.Fatal("reserve the span that stays")
	}
	keep.Commit()

	drop := p.NewReservation()
	if !p.ReserveSpan(drop, in.Tenant, in.Service, in.TraceID, ts, 512) {
		t.Fatal("reserve the span that is dropped")
	}
	if !p.ReserveSynthesizedLog(drop, in.Tenant, in.Service, in.TraceID, "span-1", "ERROR", ts, 32) {
		t.Fatal("reserve the synthesized log that is dropped")
	}
	drop.Release()

	stats, ok := p.TraceStats(in.Tenant, in.Service, in.TraceID, ts)
	if !ok {
		t.Fatal("TraceStats missing for a selected trace")
	}
	if stats.Retained != 1 {
		t.Fatalf("retained = %d after releasing one span and one synthesized log, want 1", stats.Retained)
	}
}
