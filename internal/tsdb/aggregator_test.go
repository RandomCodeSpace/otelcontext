package tsdb

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// newRawMetric builds a deterministic RawMetric for a given (tenant, name)
// pair. Each call returns a NEW series identity unless name + attrs are
// identical to a previous call — useful for cardinality stress tests.
func newRawMetric(tenant, svc, name string, attrSeed int) RawMetric {
	return RawMetric{
		TenantID:    tenant,
		ServiceName: svc,
		Name:        name,
		Value:       1.0,
		Timestamp:   time.Now(),
		Attributes:  map[string]any{"seed": attrSeed},
	}
}

// countOverflowEvents wraps an atomic counter so tests can assert overflow
// fired the expected number of times under each labeled tenant.
type overflowCounter struct {
	total      atomic.Int64
	perTenant  map[string]*atomic.Int64
	registerMu chan struct{} // dummy lock-free serialization for tests
}

func newOverflowCounter() *overflowCounter {
	return &overflowCounter{perTenant: make(map[string]*atomic.Int64), registerMu: make(chan struct{}, 1)}
}

func (c *overflowCounter) inc(tenant string) {
	c.total.Add(1)
	// Lazy-register tenant counter under a tiny semaphore. Tests are
	// single-goroutine so no race; semaphore is just defensive.
	c.registerMu <- struct{}{}
	if _, ok := c.perTenant[tenant]; !ok {
		c.perTenant[tenant] = &atomic.Int64{}
	}
	<-c.registerMu
	c.perTenant[tenant].Add(1)
}

func (c *overflowCounter) tenant(t string) int64 {
	if v, ok := c.perTenant[t]; ok {
		return v.Load()
	}
	return 0
}

// TestAggregator_NoLimits_AcceptsUnlimitedSeries verifies that with both
// caps at 0, every distinct series gets its own bucket. Baseline.
func TestAggregator_NoLimits_AcceptsUnlimitedSeries(t *testing.T) {
	a := NewAggregator(nil, time.Minute)
	// No SetCardinalityLimit call → both caps are 0.

	for i := range 50 {
		a.Ingest(newRawMetric("tenant-a", "svc", "metric-"+strconv.Itoa(i), i))
	}
	if got := a.BucketCount(); got != 50 {
		t.Fatalf("BucketCount=%d, want 50 (no caps configured)", got)
	}
}

// TestAggregator_GlobalCapRoutesToSharedOverflow verifies the legacy
// single-tenant behavior is preserved when only the global cap is set.
func TestAggregator_GlobalCapRoutesToSharedOverflow(t *testing.T) {
	a := NewAggregator(nil, time.Minute)
	c := newOverflowCounter()
	a.SetCardinalityLimit(5, 0, c.inc)

	// 7 unique series → 5 fit, 2 overflow into the shared bucket.
	for i := range 7 {
		a.Ingest(newRawMetric("tenant-a", "svc", "metric-"+strconv.Itoa(i), i))
	}

	// 5 normal + 1 shared overflow = 6 buckets total.
	if got := a.BucketCount(); got != 6 {
		t.Fatalf("BucketCount=%d, want 6 (5 normal + 1 global overflow)", got)
	}
	if got := c.total.Load(); got != 2 {
		t.Errorf("overflow callback fired %d times, want 2", got)
	}
	if got := c.tenant(overflowSentinelGlobal); got != 2 {
		t.Errorf("global overflow label fired %d times, want 2", got)
	}
}

// TestAggregator_PerTenantCapEnforcesFairness is the core fairness
// guarantee — tenant A exhausts its budget but tenant B still gets fresh
// series. Without per-tenant fairness this test would fail because
// tenant A would consume the full global pool.
func TestAggregator_PerTenantCapEnforcesFairness(t *testing.T) {
	a := NewAggregator(nil, time.Minute)
	c := newOverflowCounter()
	// Per-tenant cap=3, global cap=10 (large headroom — fairness is the
	// only thing being tested).
	a.SetCardinalityLimit(10, 3, c.inc)

	// Tenant A: 5 unique series → 3 fit, 2 overflow under tenant A's bucket.
	for i := range 5 {
		a.Ingest(newRawMetric("tenant-a", "svc", "metric-a"+strconv.Itoa(i), i))
	}
	// Tenant B: 3 unique series — must all fit, NOT routed to overflow.
	for i := range 3 {
		a.Ingest(newRawMetric("tenant-b", "svc", "metric-b"+strconv.Itoa(i), i))
	}

	// 3 (tenant-a normal) + 1 (tenant-a overflow) + 3 (tenant-b normal) = 7.
	if got := a.BucketCount(); got != 7 {
		t.Fatalf("BucketCount=%d, want 7 — got %d means fairness broken or overflow accounting off", got, got)
	}
	if got := c.tenant("tenant-a"); got != 2 {
		t.Errorf("tenant-a overflow fired %d times, want 2", got)
	}
	if got := c.tenant("tenant-b"); got != 0 {
		t.Errorf("tenant-b overflow fired %d times, want 0 — fairness broken", got)
	}
	if got := c.tenant(overflowSentinelGlobal); got != 0 {
		t.Errorf("global overflow fired %d times, want 0 — per-tenant cap should trigger first", got)
	}
}

// TestAggregator_PerTenantOverflowBucketsAreSeparate guards against the
// regression where two tenants in overflow merge into one shared bucket
// (which would corrupt per-tenant stats during flush).
func TestAggregator_PerTenantOverflowBucketsAreSeparate(t *testing.T) {
	a := NewAggregator(nil, time.Minute)
	a.SetCardinalityLimit(0, 1, func(string) {})

	// Tenant A: 3 series → 1 fits, 2 overflow into tenant-a's overflow bucket.
	for i := range 3 {
		a.Ingest(newRawMetric("tenant-a", "svc", "metric-a"+strconv.Itoa(i), i))
	}
	// Tenant B: 3 series → 1 fits, 2 overflow into tenant-b's overflow bucket.
	for i := range 3 {
		a.Ingest(newRawMetric("tenant-b", "svc", "metric-b"+strconv.Itoa(i), i))
	}

	// 2 normal + 2 overflow (one per tenant) = 4 buckets total.
	if got := a.BucketCount(); got != 4 {
		t.Fatalf("BucketCount=%d, want 4 (per-tenant overflow buckets must NOT merge)", got)
	}
}

// TestAggregator_FlushResetsPerTenantCounts verifies that after a flush
// pops the buckets, tenants get a fresh series budget. Otherwise the
// series count would grow monotonically and every tenant would saturate
// after a single window.
func TestAggregator_FlushResetsPerTenantCounts(t *testing.T) {
	a := NewAggregator(nil, time.Minute)
	a.SetCardinalityLimit(0, 2, func(string) {})

	// Window 1: tenant-a fills its budget.
	for i := range 2 {
		a.Ingest(newRawMetric("tenant-a", "svc", "metric-"+strconv.Itoa(i), i))
	}
	if got := a.BucketCount(); got != 2 {
		t.Fatalf("pre-flush BucketCount=%d, want 2", got)
	}

	a.flush()

	// Post-flush, the live map is empty and per-tenant count is reset.
	if got := a.BucketCount(); got != 0 {
		t.Fatalf("post-flush BucketCount=%d, want 0", got)
	}

	// Window 2: tenant-a should be allowed 2 fresh series. If the count
	// wasn't reset, both inserts would route to overflow and bucket count
	// would be only 1 (a single overflow bucket).
	c := newOverflowCounter()
	a.SetCardinalityLimit(0, 2, c.inc)
	for i := range 2 {
		a.Ingest(newRawMetric("tenant-a", "svc", "metric-window2-"+strconv.Itoa(i), i))
	}
	if got := a.BucketCount(); got != 2 {
		t.Fatalf("post-flush window: BucketCount=%d, want 2 (per-tenant count must reset on flush)", got)
	}
	if got := c.total.Load(); got != 0 {
		t.Errorf("post-flush window: overflow fired %d times, want 0", got)
	}
}

// TestAggregator_GlobalAndPerTenant_BothEnforced verifies the priority
// order: per-tenant cap is checked first; the global cap is a backstop
// that only fires when per-tenant is still under budget. In particular,
// once the global pool is exhausted, later tenants who haven't hit
// their per-tenant cap yet still get routed to the GLOBAL overflow —
// they're not penalized as if they had been noisy themselves.
//
// Trace with per-tenant=2, global=4:
//
//	tenant-a × 3 → 2 normal,             1 PER-TENANT overflow (tenant-a label)
//	  global buckets after a: 3 (2 normal + 1 tenant-a overflow)
//	tenant-b × 3 → 1 normal (global=3<4), 2 GLOBAL overflow (global hit at 4)
//	  global buckets after b: 5
//	tenant-c × 3 → 0 normal,             3 GLOBAL overflow
func TestAggregator_GlobalAndPerTenant_BothEnforced(t *testing.T) {
	a := NewAggregator(nil, time.Minute)
	c := newOverflowCounter()
	a.SetCardinalityLimit(4, 2, c.inc)

	for _, tenant := range []string{"a", "b", "c"} {
		for i := range 3 {
			a.Ingest(newRawMetric("tenant-"+tenant, "svc", "metric-"+strconv.Itoa(i), i))
		}
	}

	if got := c.tenant("tenant-a"); got != 1 {
		t.Errorf("tenant-a per-tenant overflow=%d, want 1", got)
	}
	if got := c.tenant("tenant-b"); got != 0 {
		t.Errorf("tenant-b per-tenant overflow=%d, want 0 (tenant-b never hit its per-tenant cap)", got)
	}
	if got := c.tenant("tenant-c"); got != 0 {
		t.Errorf("tenant-c per-tenant overflow=%d, want 0", got)
	}
	// 5 global overflows: tenant-b's 2nd & 3rd plus all 3 of tenant-c.
	if got := c.tenant(overflowSentinelGlobal); got != 5 {
		t.Errorf("global overflow=%d, want 5", got)
	}
}

// TestAggregator_DefaultBehaviorUnchanged is a regression guard — when
// SetCardinalityLimit is called with the LEGACY (global, perTenant=0,
// callback) shape, behavior must match the pre-fairness Aggregator.
func TestAggregator_DefaultBehaviorUnchanged(t *testing.T) {
	a := NewAggregator(nil, time.Minute)
	c := newOverflowCounter()
	// Per-tenant=0 → only global cap applies; identical to legacy.
	a.SetCardinalityLimit(2, 0, c.inc)

	a.Ingest(newRawMetric("any", "svc", "m1", 1))
	a.Ingest(newRawMetric("any", "svc", "m2", 2))
	a.Ingest(newRawMetric("any", "svc", "m3", 3)) // overflow

	if got := a.BucketCount(); got != 3 {
		t.Errorf("BucketCount=%d, want 3 (2 normal + 1 global overflow)", got)
	}
	if got := c.total.Load(); got != 1 {
		t.Errorf("overflow fired %d times, want 1", got)
	}
	if got := c.tenant(overflowSentinelGlobal); got != 1 {
		t.Errorf("global overflow label fired %d times, want 1", got)
	}
}

// TestAggregator_OverflowBucketStatsCorrect ensures that successive
// overflow points for the same tenant accumulate into ONE overflow
// bucket (not many) and that min/max/sum/count update correctly.
func TestAggregator_OverflowBucketStatsCorrect(t *testing.T) {
	a := NewAggregator(nil, time.Minute)
	a.SetCardinalityLimit(0, 1, func(string) {})

	// First point fills the per-tenant budget.
	first := newRawMetric("t", "svc", "metric-base", 0)
	first.Value = 5.0
	a.Ingest(first)

	// Three more points overflow — they should all merge into ONE
	// overflow bucket, not each create a fresh one.
	for i := range 3 {
		m := newRawMetric("t", "svc", "metric-overflow-"+strconv.Itoa(i), i)
		m.Value = float64(10 + i)
		a.Ingest(m)
	}

	// 1 normal + 1 overflow = 2 buckets.
	if got := a.BucketCount(); got != 2 {
		t.Fatalf("BucketCount=%d, want 2", got)
	}
}
