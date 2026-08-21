package aggregate

import (
	"testing"
	"time"
)

// otherNameStub stands in for the dictionary's __other__ entry.
const otherNameStub uint32 = 999999

func newTestLimiter(cfg LimiterConfig) *Limiter {
	cfg.OtherNameID = func(uint32, Signal) uint32 { return otherNameStub }
	return NewLimiter(cfg)
}

func capTraceKey(service, name uint32, m Method, status StatusClass) SeriesKey {
	return SeriesKey{
		TenantID:    1,
		ServiceID:   service,
		NameID:      name,
		Signal:      SignalTraceOp,
		StatusClass: status,
		Method:      m,
	}
}

const testWindow int64 = 1_000_000_000

// --- enforcement order: each tier must fire first in its own scenario ---

func TestEnforcementOrderTenantFirst(t *testing.T) {
	// Tenant fraction is the tightest: 2% of 100 = 2 series. Every other cap
	// is wide open, so only the tenant rule can trigger.
	l := newTestLimiter(LimiterConfig{
		MaxSeries: 100, MaxSeriesTraces: 100,
		MaxOperationsPerService: 100, MaxTraceSeriesPerService: 100,
		SeriesPerTenantFraction: 0.02,
	})
	l.Admit(capTraceKey(1, 1, MethodGet, StatusOK), testWindow)
	l.Admit(capTraceKey(1, 2, MethodGet, StatusOK), testWindow)

	adm := l.Admit(capTraceKey(1, 3, MethodGet, StatusOK), testWindow)
	if !adm.Overflowed || adm.Reason != OverflowTenant {
		t.Fatalf("admission = %+v, want overflow with reason tenant", adm)
	}
}

func TestEnforcementOrderServiceNamesBeforeServiceSeries(t *testing.T) {
	// Two operations allowed per service; the materialized per-service cap is
	// wide, so the distinct-name rule must be what fires.
	l := newTestLimiter(LimiterConfig{
		MaxSeries: 1000, MaxSeriesTraces: 1000,
		MaxOperationsPerService: 2, MaxTraceSeriesPerService: 1000,
	})
	l.Admit(capTraceKey(1, 1, MethodGet, StatusOK), testWindow)
	l.Admit(capTraceKey(1, 2, MethodGet, StatusOK), testWindow)

	// A third distinct operation overflows...
	adm := l.Admit(capTraceKey(1, 3, MethodGet, StatusOK), testWindow)
	if !adm.Overflowed || adm.Reason != OverflowServiceNames {
		t.Fatalf("admission = %+v, want overflow with reason service_names", adm)
	}
	// ...but another materialization of a KNOWN operation is admitted: the
	// name cap counts distinct names, not full keys (#159).
	known := l.Admit(capTraceKey(1, 1, MethodPost, StatusError), testWindow)
	if known.Overflowed {
		t.Fatalf("a new materialization of a known operation was rejected: %+v", known)
	}
}

func TestEnforcementOrderServiceSeriesBeforeSignal(t *testing.T) {
	// One operation, but only two materialized series per service. The name
	// cap can never fire here, and the signal sub-cap is far away.
	l := newTestLimiter(LimiterConfig{
		MaxSeries: 1000, MaxSeriesTraces: 1000,
		MaxOperationsPerService: 100, MaxTraceSeriesPerService: 2,
	})
	l.Admit(capTraceKey(1, 1, MethodGet, StatusOK), testWindow)
	l.Admit(capTraceKey(1, 1, MethodPost, StatusOK), testWindow)

	adm := l.Admit(capTraceKey(1, 1, MethodPut, StatusOK), testWindow)
	if !adm.Overflowed || adm.Reason != OverflowServiceSeries {
		t.Fatalf("admission = %+v, want overflow with reason service_series", adm)
	}
}

func TestEnforcementOrderSignalSubCap(t *testing.T) {
	// Per-service caps are wide; the signal sub-cap is two. Two different
	// services each take one slot, so no per-service rule can fire.
	l := newTestLimiter(LimiterConfig{
		MaxSeries: 1000, MaxSeriesTraces: 2, MaxSeriesLogs: 100,
		MaxOperationsPerService: 100, MaxTraceSeriesPerService: 100,
	})
	l.Admit(capTraceKey(1, 1, MethodGet, StatusOK), testWindow)
	l.Admit(capTraceKey(2, 1, MethodGet, StatusOK), testWindow)

	adm := l.Admit(capTraceKey(3, 1, MethodGet, StatusOK), testWindow)
	if !adm.Overflowed || adm.Reason != OverflowSignal {
		t.Fatalf("admission = %+v, want overflow with reason signal", adm)
	}

	// A different signal is unaffected: sub-caps are independent.
	logAdm := l.Admit(SeriesKey{TenantID: 1, ServiceID: 9, NameID: 1, Signal: SignalLog, StatusClass: SeverityTierInfo}, testWindow)
	if logAdm.Overflowed {
		t.Fatalf("log series overflowed on the trace sub-cap: %+v", logAdm)
	}
}

// TestEnforcementOrderGlobalBackstop drives the last tier directly. In a
// VALID configuration the global cap is unreachable — LimiterConfig.Validate
// requires sum(sub-caps) <= global, so a signal sub-cap always fires first.
// That is the point of the backstop: it exists for the misconfigured and the
// overflow-inflated case, and it is evaluated last.
func TestEnforcementOrderGlobalBackstop(t *testing.T) {
	l := newTestLimiter(LimiterConfig{
		MaxSeries: 2, MaxSeriesTraces: 1000, MaxSeriesLogs: 1000,
		MaxOperationsPerService: 100, MaxTraceSeriesPerService: 100,
	})
	l.Admit(capTraceKey(1, 1, MethodGet, StatusOK), testWindow)
	l.Admit(capTraceKey(2, 1, MethodGet, StatusOK), testWindow)

	adm := l.Admit(capTraceKey(3, 1, MethodGet, StatusOK), testWindow)
	if !adm.Overflowed || adm.Reason != OverflowGlobal {
		t.Fatalf("admission = %+v, want overflow with reason global", adm)
	}
}

func TestLimiterConfigValidateRejectsImpossibleBudgets(t *testing.T) {
	over := LimiterConfig{MaxSeries: 100, MaxSeriesMetrics: 60, MaxSeriesTraces: 60}
	if err := over.Validate(); err == nil {
		t.Error("sum of sub-caps above the global cap was accepted")
	}
	badFraction := LimiterConfig{MaxSeries: 100, MaxSeriesTraces: 10, SeriesPerTenantFraction: 1.5}
	if err := badFraction.Validate(); err == nil {
		t.Error("tenant fraction above 1 was accepted")
	}
	ok := LimiterConfig{MaxSeries: 100, MaxSeriesTraces: 50, MaxSeriesLogs: 50}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid budget rejected: %v", err)
	}
}

func TestNewEngineRejectsImpossibleBudget(t *testing.T) {
	_, err := NewEngine(EngineConfig{
		Limiter: LimiterConfig{MaxSeries: 10, MaxSeriesTraces: 9, MaxSeriesLogs: 9},
	})
	if err == nil {
		t.Fatal("engine accepted a budget whose sub-caps exceed the global cap")
	}
}

// --- overflow behavior ---

func TestOverflowSeriesPreservesStatusAndCollapsesTheRest(t *testing.T) {
	l := newTestLimiter(LimiterConfig{
		MaxSeries: 1000, MaxSeriesTraces: 1000,
		MaxOperationsPerService: 1, MaxTraceSeriesPerService: 1000,
	})
	l.Admit(capTraceKey(1, 1, MethodGet, StatusOK), testWindow)

	errored := l.Admit(SeriesKey{
		TenantID: 1, ServiceID: 1, NameID: 42, Signal: SignalTraceOp,
		StatusClass: StatusError, Method: MethodPost, HTTPClass: HTTPClass5xx,
		Variant: SpanKindServer,
	}, testWindow)
	if !errored.Overflowed {
		t.Fatal("expected overflow")
	}
	// Error visibility must survive overflow.
	if errored.Key.StatusClass != StatusError {
		t.Errorf("overflow status = %d, want StatusError", errored.Key.StatusClass)
	}
	if errored.Key.Method != MethodOther {
		t.Errorf("overflow method = %v, want OTHER", errored.Key.Method)
	}
	if errored.Key.HTTPClass != HTTPClassNone {
		t.Errorf("overflow http class = %v, want none", errored.Key.HTTPClass)
	}
	if errored.Key.NameID != otherNameStub {
		t.Errorf("overflow name = %d, want the dictionary __other__ entry", errored.Key.NameID)
	}
	if errored.Key.Variant != SpanKindUnspecified || errored.Key.DimsID != 0 {
		t.Errorf("overflow key kept identity detail: %+v", errored.Key)
	}
	if err := errored.Key.Validate(); err != nil {
		t.Errorf("overflow key is not a valid series key: %v", err)
	}

	// A healthy overflow lands on a DIFFERENT overflow series than the error
	// one — collapsing errors into the healthy bucket would hide outages.
	healthy := l.Admit(capTraceKey(1, 43, MethodPut, StatusOK), testWindow)
	if healthy.Key == errored.Key {
		t.Error("error and healthy overflow collapsed into one series")
	}
}

func TestOverflowSeriesCreationIsImmuneToEveryCap(t *testing.T) {
	// Every cap is at its floor: one series globally, one per signal, one per
	// service, one operation, and a tenant fraction that resolves to one.
	l := newTestLimiter(LimiterConfig{
		MaxSeries: 1, MaxSeriesTraces: 1,
		MaxOperationsPerService: 1, MaxTraceSeriesPerService: 1,
		SeriesPerTenantFraction: 0.01,
	})
	l.Admit(capTraceKey(1, 1, MethodGet, StatusOK), testWindow)

	adm := l.Admit(capTraceKey(1, 2, MethodGet, StatusOK), testWindow)
	if !adm.Overflowed {
		t.Fatal("expected overflow")
	}
	if !l.IsOverflowSeries(adm.Key) {
		t.Fatal("the __other__ series was not created: a cap blocked the series meant to absorb cap violations")
	}
	if got := l.Stats().OverflowSeries; got != 1 {
		t.Errorf("live overflow series = %d, want 1", got)
	}
}

func TestOverflowPreservesTotals(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e, err := NewEngine(EngineConfig{
		Mode: ModeShadow,
		Now:  func() time.Time { return now },
		Limiter: LimiterConfig{
			MaxSeries: 1000, MaxSeriesTraces: 500, MaxSeriesLogs: 100,
			MaxSeriesMetrics: 100, MaxSeriesEdges: 100, MaxSeriesSystem: 100,
			// Three operations per service: everything past that overflows.
			MaxOperationsPerService: 3, MaxTraceSeriesPerService: 1000,
		},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	const operations = 50
	const perOperation = 7
	r := e.NewReducer(now)
	wantErrors := uint64(0)
	for op := 0; op < operations; op++ {
		for i := 0; i < perOperation; i++ {
			isErr := op%5 == 0
			status := int32(1)
			if isErr {
				status = 2
				wantErrors++
			}
			r.ReduceSpan(SpanInput{
				Tenant: "t", Service: "svc",
				SpanName:   string(rune('a'+op%26)) + string(rune('a'+op/26)),
				Timestamp:  now,
				StatusCode: status, DurationMicros: float64(i + 1),
			})
		}
	}
	e.ApplyReducer(r)

	snap := e.Snapshot()
	count, errors := snap.Totals(SignalTraceOp)
	if count != operations*perOperation {
		t.Errorf("total count = %d, want %d — overflow must never lose a point", count, operations*perOperation)
	}
	if errors != wantErrors {
		t.Errorf("total errors = %d, want %d", errors, wantErrors)
	}
	if snap.Overflow[OverflowServiceNames] == 0 {
		t.Error("no service_names overflow was recorded despite exceeding the operation cap")
	}
	// Identity detail collapsed: far fewer series than operations.
	if snap.ActiveSeries >= operations {
		t.Errorf("active series = %d, want well under %d — overflow must collapse identity", snap.ActiveSeries, operations)
	}
}

// TestActiveSeriesReleasedAcrossWindows proves budget accounting is per-series,
// not per-(series, window): a series in two windows costs one slot, and giving
// up one window does not free it.
func TestActiveSeriesCountedOncePerSeries(t *testing.T) {
	l := newTestLimiter(LimiterConfig{MaxSeries: 100, MaxSeriesTraces: 100})
	key := capTraceKey(1, 1, MethodGet, StatusOK)

	l.Admit(key, testWindow)
	l.Admit(key, testWindow+300)
	if got := l.Stats().Active; got != 1 {
		t.Fatalf("active = %d, want 1 — one series in two windows is one series", got)
	}

	l.Release(key, testWindow)
	if got := l.Stats().Active; got != 1 {
		t.Fatalf("active = %d after releasing one of two windows, want 1", got)
	}
	l.Release(key, testWindow+300)
	if got := l.Stats().Active; got != 0 {
		t.Fatalf("active = %d after releasing every window, want 0", got)
	}
}

func TestLogTemplateCapDrivesLogSeriesOverflow(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:00:00Z")
	e, err := NewEngine(EngineConfig{
		Mode: ModeShadow,
		Now:  func() time.Time { return now },
		Limiter: LimiterConfig{
			MaxSeries: 1000, MaxSeriesLogs: 500, MaxSeriesTraces: 100,
			MaxSeriesMetrics: 100, MaxSeriesEdges: 100, MaxSeriesSystem: 100,
			MaxLogTemplatesPerService: 2,
		},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	r := e.NewReducer(now)
	bodies := []string{
		"alpha connection refused",
		"beta disk quota exceeded",
		"gamma certificate expired",
		"delta upstream unavailable",
		"epsilon handshake failed",
	}
	for _, b := range bodies {
		r.ReduceLog(LogInput{Tenant: "t", Service: "svc", Severity: "ERROR", Body: b, Timestamp: now})
	}
	e.ApplyReducer(r)

	count, errors := e.Snapshot().Totals(SignalLog)
	if count != uint64(len(bodies)) || errors != uint64(len(bodies)) {
		t.Errorf("log totals = (%d, %d), want (%d, %d) — totals survive template overflow",
			count, errors, len(bodies), len(bodies))
	}
	// The miner caps templates per service; the __other__ template absorbs the
	// rest, so the series count stays bounded.
	if got := e.Snapshot().ActiveSeries; got > 3 {
		t.Errorf("active log series = %d, want at most 3 (2 templates + __other__)", got)
	}
}

// TestSignalSubCapBindsUnderSaturation is the #173 regression. The wave-5 run
// measured aggregate_series_active{signal="log"} at 1,005 against a 500 log
// sub-cap: 500 admitted series plus 505 __other__ series, because every
// overflow series a cap minted was charged back to the same cap it was minted
// to relieve. The reserve is now reported apart from the census, so the census
// a cap is compared against is one a cap can actually bind.
func TestSignalSubCapBindsUnderSaturation(t *testing.T) {
	const (
		cap      = 50
		services = 30
		names    = 40
	)
	l := newTestLimiter(LimiterConfig{
		MaxSeries: 1000, MaxSeriesLogs: cap,
		// Every per-service cap is wide open so only the log sub-cap can bind.
		MaxLogTemplatesPerService: 10000,
	})

	// 1,200 distinct log series across 30 services — 24x the sub-cap, spread
	// so each service mints its own __other__ series once it overflows.
	for svc := uint32(1); svc <= services; svc++ {
		for name := uint32(1); name <= names; name++ {
			l.Admit(SeriesKey{
				TenantID: 1, ServiceID: svc, NameID: name,
				Signal: SignalLog, StatusClass: SeverityTierInfo,
			}, testWindow)
		}
	}

	stats := l.Stats()
	if got := stats.ActiveBySignal[SignalLog]; got > cap {
		t.Fatalf("active log series = %d, want <= %d — the sub-cap does not bind", got, cap)
	}
	if got := stats.ActiveBySignal[SignalLog]; got != cap {
		t.Fatalf("active log series = %d, want exactly %d — the budget is not being spent", got, cap)
	}
	// The reserve exists and is visible, it is just not charged to the cap.
	// Service 1's 40 names and 10 of service 2's fill the 50-series budget, so
	// service 1 is the one service that never overflows and never mints an
	// __other__ series.
	if got := stats.OverflowSeriesBySignal[SignalLog]; got != services-1 {
		t.Fatalf("live __other__ log series = %d, want %d (one per overflowing service)", got, services-1)
	}
	if stats.Overflow[OverflowSignal] == 0 {
		t.Fatal("no admission was attributed to the signal sub-cap")
	}

	// Releasing every window returns the budget AND the reserve: the two
	// censuses must not drift apart.
	for svc := uint32(1); svc <= services; svc++ {
		for name := uint32(1); name <= names; name++ {
			l.Release(SeriesKey{
				TenantID: 1, ServiceID: svc, NameID: name,
				Signal: SignalLog, StatusClass: SeverityTierInfo,
			}, testWindow)
		}
		l.Release(l.overflowKey(SeriesKey{
			TenantID: 1, ServiceID: svc, Signal: SignalLog, StatusClass: SeverityTierInfo,
		}), testWindow)
	}
	if stats := l.Stats(); stats.Active != 0 || stats.OverflowSeries != 0 {
		t.Fatalf("after releasing every window: active = %d, overflow series = %d, want 0/0",
			stats.Active, stats.OverflowSeries)
	}
}
