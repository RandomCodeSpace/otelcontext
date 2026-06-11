package tsdb

import (
	"strconv"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// newTestRing builds an unlimited RingBuffer with a wide window so every
// record in a test lands in the current slot.
func newTestRing() *RingBuffer {
	return NewRingBuffer(8, time.Minute, 0, nil)
}

// TestRingBuffer_Record_ScopedByTenant is the data-isolation guarantee —
// two tenants recording under the SAME service+metric must never see each
// other's points (modeled on internal/storage tenant_test.go naming).
func TestRingBuffer_Record_ScopedByTenant(t *testing.T) {
	rb := newTestRing()
	now := time.Now()

	rb.Record("tenant-a", "latency_ms", "checkout", 1.0, now)
	rb.Record("tenant-a", "latency_ms", "checkout", 2.0, now)
	rb.Record("tenant-b", "latency_ms", "checkout", 100.0, now)

	aggsA := rb.QueryRecent("tenant-a", "latency_ms", "checkout", 8)
	if len(aggsA) != 1 {
		t.Fatalf("tenant-a windows=%d, want 1", len(aggsA))
	}
	if aggsA[0].Count != 2 || aggsA[0].Sum != 3.0 {
		t.Errorf("tenant-a agg Count=%d Sum=%v, want Count=2 Sum=3 — tenant-b data leaked in?", aggsA[0].Count, aggsA[0].Sum)
	}

	aggsB := rb.QueryRecent("tenant-b", "latency_ms", "checkout", 8)
	if len(aggsB) != 1 {
		t.Fatalf("tenant-b windows=%d, want 1", len(aggsB))
	}
	if aggsB[0].Count != 1 || aggsB[0].Sum != 100.0 {
		t.Errorf("tenant-b agg Count=%d Sum=%v, want Count=1 Sum=100 — tenant-a data leaked in?", aggsB[0].Count, aggsB[0].Sum)
	}

	if got := rb.QueryRecent("tenant-c", "latency_ms", "checkout", 8); got != nil {
		t.Errorf("tenant-c QueryRecent=%v, want nil (tenant never recorded)", got)
	}
}

// TestRingBuffer_EmptyTenantCoercedToDefault — a point recorded without a
// tenant must be readable under storage.DefaultTenantID (and vice versa),
// matching the RawMetric.TenantID doc ("empty → the default applies").
func TestRingBuffer_EmptyTenantCoercedToDefault(t *testing.T) {
	rb := newTestRing()
	now := time.Now()

	rb.Record("", "latency_ms", "checkout", 7.0, now)

	aggs := rb.QueryRecent(storage.DefaultTenantID, "latency_ms", "checkout", 8)
	if len(aggs) != 1 || aggs[0].Sum != 7.0 {
		t.Fatalf("QueryRecent(DefaultTenantID)=%v, want the empty-tenant point", aggs)
	}
	aggs = rb.QueryRecent("", "latency_ms", "checkout", 8)
	if len(aggs) != 1 || aggs[0].Sum != 7.0 {
		t.Fatalf("QueryRecent(\"\")=%v, want the empty-tenant point", aggs)
	}
	if got := rb.MetricCount(); got != 1 {
		t.Errorf("MetricCount=%d, want 1 (\"\" and DefaultTenantID are the same series)", got)
	}
}

// TestRingBuffer_SeriesCapRejectsNewSeries — past maxSeries, NEW series are
// refused (telemetry fires, Record returns false) while EXISTING series
// keep recording.
func TestRingBuffer_SeriesCapRejectsNewSeries(t *testing.T) {
	rejected := 0
	rb := NewRingBuffer(8, time.Minute, 2, func() { rejected++ })
	now := time.Now()

	if !rb.Record("t", "m1", "svc", 1.0, now) {
		t.Fatal("first series refused below cap")
	}
	if !rb.Record("t", "m2", "svc", 1.0, now) {
		t.Fatal("second series refused below cap")
	}
	// Third DISTINCT series is past the cap → refused.
	if rb.Record("t", "m3", "svc", 1.0, now) {
		t.Error("third series accepted past cap=2")
	}
	if rejected != 1 {
		t.Errorf("rejection callback fired %d times, want 1", rejected)
	}
	if got := rb.MetricCount(); got != 2 {
		t.Errorf("MetricCount=%d, want 2 (cap must hold)", got)
	}

	// Existing series must still record at the cap.
	if !rb.Record("t", "m1", "svc", 2.0, now) {
		t.Fatal("existing series refused at cap — only NEW series may be rejected")
	}
	aggs := rb.QueryRecent("t", "m1", "svc", 8)
	if len(aggs) != 1 || aggs[0].Count != 2 || aggs[0].Sum != 3.0 {
		t.Errorf("existing series aggs=%v, want Count=2 Sum=3", aggs)
	}
	// The refused series holds no data.
	if got := rb.QueryRecent("t", "m3", "svc", 8); got != nil {
		t.Errorf("refused series QueryRecent=%v, want nil", got)
	}
	if rejected != 1 {
		t.Errorf("rejection callback fired %d times after existing-series record, want still 1", rejected)
	}
}

// TestRingBuffer_SeriesCapZeroIsUnlimited — maxSeries=0 preserves the
// legacy uncapped behavior.
func TestRingBuffer_SeriesCapZeroIsUnlimited(t *testing.T) {
	rb := NewRingBuffer(8, time.Minute, 0, func() { t.Error("rejection fired with cap=0") })
	now := time.Now()

	for i := range 50 {
		if !rb.Record("t", "metric-"+strconv.Itoa(i), "svc", 1.0, now) {
			t.Fatalf("series %d refused with cap=0", i)
		}
	}
	if got := rb.MetricCount(); got != 50 {
		t.Errorf("MetricCount=%d, want 50", got)
	}
}

// TestRingBuffer_WindowAdvanceKeepsTenantSeries — recording past the
// window boundary advances slots within the SAME tenant-scoped series
// (no new series is created) and both windows stay queryable.
func TestRingBuffer_WindowAdvanceKeepsTenantSeries(t *testing.T) {
	rb := newTestRing()
	now := time.Now()

	rb.Record("tenant-a", "latency_ms", "checkout", 1.0, now)
	rb.Record("tenant-a", "latency_ms", "checkout", 2.0, now.Add(2*time.Minute))

	if got := rb.MetricCount(); got != 1 {
		t.Fatalf("MetricCount=%d, want 1 — window advance must not mint a new series", got)
	}
	aggs := rb.QueryRecent("tenant-a", "latency_ms", "checkout", 8)
	if len(aggs) != 2 {
		t.Fatalf("windows=%d, want 2 (one per time bucket)", len(aggs))
	}
	// Most-recent window first.
	if aggs[0].Sum != 2.0 || aggs[0].Count != 1 {
		t.Errorf("newest window Sum=%v Count=%d, want Sum=2 Count=1", aggs[0].Sum, aggs[0].Count)
	}
	if aggs[1].Sum != 1.0 || aggs[1].Count != 1 {
		t.Errorf("older window Sum=%v Count=%d, want Sum=1 Count=1", aggs[1].Sum, aggs[1].Count)
	}
	if aggs[0].P50 != 2.0 || aggs[0].P99 != 2.0 {
		t.Errorf("newest window P50=%v P99=%v, want 2 for a single-sample window", aggs[0].P50, aggs[0].P99)
	}
}

// TestRingBuffer_MetricCount_CountsTenantScopedSeries — the same
// service+metric under two tenants is TWO series, not one.
func TestRingBuffer_MetricCount_CountsTenantScopedSeries(t *testing.T) {
	rb := newTestRing()
	now := time.Now()

	rb.Record("tenant-a", "latency_ms", "checkout", 1.0, now)
	rb.Record("tenant-b", "latency_ms", "checkout", 1.0, now)
	rb.Record("tenant-a", "latency_ms", "checkout", 2.0, now) // same series

	if got := rb.MetricCount(); got != 2 {
		t.Errorf("MetricCount=%d, want 2 tenant-scoped series", got)
	}
	if got := len(rb.AllKeys()); got != 2 {
		t.Errorf("AllKeys len=%d, want 2", got)
	}
}
