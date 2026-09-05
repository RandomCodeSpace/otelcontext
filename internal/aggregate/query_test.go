package aggregate

import (
	"context"
	"math"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/latency"
)

// stubStore is a Store that serves a fixed set of store-owned rows. It exists
// so the query facade's ownership rules can be tested without a live SQLite
// file: what matters here is WHICH source a window is read from, not how the
// bytes got there.
//
// Its ReadBuckets and SumBuckets are also the NAIVE REFERENCE the completeness
// tests compare the SQLite implementation against: sort in Go, page in Go, sum
// in Go, no SQL involved.
type stubStore struct {
	buckets []Bucket
	infos   map[SeriesID]SeriesKey
	reads   int
	// readRanges records every [Start,End) the facade asked for, so a test can
	// assert that a memory-owned window was never requested from the store.
	readRanges [][2]int64
}

func newStubStore() *stubStore {
	return &stubStore{infos: make(map[SeriesID]SeriesKey)}
}

// put records one finalized bucket under a stable series ID.
func (s *stubStore) put(id SeriesID, key SeriesKey, windowStart int64, d *AggregateDelta) {
	s.infos[id] = key
	s.buckets = append(s.buckets, Bucket{WindowStart: windowStart, SeriesID: id, Delta: d})
}

func (s *stubStore) CommitGroup(*GroupBatch) error { return nil }
func (s *stubStore) FinalizeWindow(int64) (FinalizeStats, error) {
	return FinalizeStats{}, nil
}
func (s *stubStore) FinalizableWindows(int64, int) ([]int64, error) { return nil, nil }
func (s *stubStore) PurgeBefore(int64) (PurgeStats, error)          { return PurgeStats{}, nil }

// matching returns the selector's rows in (window, series, source) order.
func (s *stubStore) matching(sel Selector) []Bucket {
	out := make([]Bucket, 0, len(s.buckets))
	for _, b := range s.buckets {
		if b.WindowStart < sel.Start || b.WindowStart >= sel.End {
			continue
		}
		key := s.infos[b.SeriesID]
		if key.TenantID != sel.TenantID {
			continue
		}
		if sel.Signal != SignalUnspecified && key.Signal != sel.Signal {
			continue
		}
		if len(sel.Signals) > 0 && !slices.Contains(sel.Signals, key.Signal) {
			continue
		}
		if sel.SketchOnly && (b.Delta == nil || b.Delta.Sketch == nil) {
			continue
		}
		if !sel.After.zero() && !sel.After.After(b.WindowStart, b.SeriesID, b.Source) {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WindowStart != out[j].WindowStart {
			return out[i].WindowStart < out[j].WindowStart
		}
		if out[i].SeriesID != out[j].SeriesID {
			return out[i].SeriesID < out[j].SeriesID
		}
		return out[i].Source < out[j].Source
	})
	return out
}

func (s *stubStore) ReadBuckets(ctx context.Context, sel Selector) (BucketPage, error) {
	if err := ctx.Err(); err != nil {
		return BucketPage{}, err
	}
	limit, err := sel.Validate()
	if err != nil {
		return BucketPage{}, err
	}
	s.reads++
	s.readRanges = append(s.readRanges, [2]int64{sel.Start, sel.End})
	out := s.matching(sel)
	page := BucketPage{Limit: limit}
	if len(out) > limit {
		out = out[:limit]
		page.Truncated = true
		last := out[len(out)-1]
		page.Next = BucketCursor{WindowStart: last.WindowStart, SeriesID: last.SeriesID, Source: last.Source}
	}
	page.Buckets = out
	return page, nil
}

func (s *stubStore) VisitSketches(ctx context.Context, sel Selector, visit func(uint32, *Sketch) error) error {
	if _, err := sel.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.reads++
	s.readRanges = append(s.readRanges, [2]int64{sel.Start, sel.End})
	sel.SketchOnly = true
	for _, b := range s.matching(sel) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(s.infos[b.SeriesID].ServiceID, b.Delta.Sketch); err != nil {
			return err
		}
	}
	return nil
}

func (s *stubStore) SumBuckets(ctx context.Context, sel Selector, by GroupBy) ([]SumRow, error) {
	if _, err := sel.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	groups := make(map[SumRow]*SumRow)
	var order []SumRow
	for _, b := range s.matching(sel) {
		key := s.infos[b.SeriesID]
		var g SumRow
		if by&GroupByWindow != 0 {
			g.WindowStart = b.WindowStart
		}
		if by&GroupByService != 0 {
			g.ServiceID = key.ServiceID
		}
		if by&GroupByName != 0 {
			g.NameID = key.NameID
		}
		if by&GroupBySignal != 0 {
			g.Signal = key.Signal
		}
		acc := groups[g]
		if acc == nil {
			acc = &SumRow{WindowStart: g.WindowStart, ServiceID: g.ServiceID, NameID: g.NameID, Signal: g.Signal}
			groups[g] = acc
			order = append(order, g)
		}
		acc.Count += b.Delta.Count
		acc.ErrorCount += b.Delta.ErrorCount
		acc.RequestCount += b.Delta.RequestCount
		acc.ErrorRequestCount += b.Delta.ErrorRequestCount
		acc.DurationCount += b.Delta.DurationCount
		acc.DurationSum += b.Delta.DurationSum
		acc.LogCount += b.Delta.LogCount
	}
	out := make([]SumRow, 0, len(order))
	for _, g := range order {
		out = append(out, *groups[g])
	}
	return out, nil
}

func (s *stubStore) ReplayMutable(int64) ([]DeltaRow, error) { return nil, nil }

// ReadFinalizedSince is the naive reference: filter by window and signal in
// Go, newest window first, cap with the same limit+1 probe the SQLite store
// uses.
func (s *stubStore) ReadFinalizedSince(since int64, signals []Signal, limit int) (FinalizedPage, error) {
	if limit <= 0 || limit > MaxReadRows {
		limit = MaxReadRows
	}
	want := make(map[Signal]struct{}, len(signals))
	for _, sig := range signals {
		want[sig] = struct{}{}
	}
	out := make([]Bucket, 0, len(s.buckets))
	for _, b := range s.buckets {
		if b.WindowStart < since || b.Source != SourceFinalized {
			continue
		}
		if len(want) > 0 {
			if _, ok := want[s.infos[b.SeriesID].Signal]; !ok {
				continue
			}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WindowStart != out[j].WindowStart {
			return out[i].WindowStart > out[j].WindowStart
		}
		return out[i].SeriesID < out[j].SeriesID
	})
	page := FinalizedPage{}
	if len(out) > limit {
		out = out[:limit]
		page.Truncated = true
	}
	page.Buckets = out
	return page, nil
}

func (s *stubStore) LoadBaselines(int) ([]BaselineRow, error) { return nil, nil }
func (s *stubStore) ResolveSeries(ids []SeriesID) ([]SeriesInfo, error) {
	out := make([]SeriesInfo, 0, len(ids))
	for _, id := range ids {
		if key, ok := s.infos[id]; ok {
			out = append(out, SeriesInfo{ID: id, Key: key})
		}
	}
	return out, nil
}
func (s *stubStore) LoadDict(int) ([]DictRow, error)     { return nil, nil }
func (s *stubStore) LoadSeries(int) ([]SeriesRow, error) { return nil, nil }
func (s *stubStore) Backlog() (BacklogStats, error)      { return BacklogStats{}, nil }
func (s *stubStore) Close() error                        { return nil }

var _ Store = (*stubStore)(nil)

// queryFixture builds an engine with a fixed clock and named services.
type queryFixture struct {
	engine   *Engine
	now      time.Time
	tenantID uint32
}

func newQueryFixture(t *testing.T) *queryFixture {
	t.Helper()
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e := testEngine(t, now)
	tenantID, _ := e.TenantID("default")
	return &queryFixture{engine: e, now: now, tenantID: tenantID}
}

// traceKey builds a trace-operation series key for a named service.
func (f *queryFixture) traceKey(service, op string) SeriesKey {
	return SeriesKey{
		TenantID:    f.tenantID,
		ServiceID:   f.engine.Cache().Intern(f.tenantID, KindService, service),
		NameID:      f.engine.Cache().Intern(f.tenantID, KindOperation, op),
		Signal:      SignalTraceOp,
		StatusClass: StatusOK,
		Variant:     SpanKindServer,
	}
}

// edgeKey builds a service-edge series key for a caller/callee pair. The
// CALLER is the series' service and the CALLEE is its name, interned in the
// operation namespace — the identity ReduceEdge writes.
func (f *queryFixture) edgeKey(caller, callee string) SeriesKey {
	return SeriesKey{
		TenantID:    f.tenantID,
		ServiceID:   f.engine.Cache().Intern(f.tenantID, KindService, caller),
		NameID:      f.engine.Cache().Intern(f.tenantID, KindOperation, callee),
		Signal:      SignalServiceEdge,
		StatusClass: StatusOK,
		Variant:     SpanKindClient,
	}
}

// logKey builds a log series key for a named service.
func (f *queryFixture) logKey(service, template string) SeriesKey {
	return SeriesKey{
		TenantID:  f.tenantID,
		ServiceID: f.engine.Cache().Intern(f.tenantID, KindService, service),
		NameID:    f.engine.Cache().Intern(f.tenantID, KindLogTemplate, template),
		Signal:    SignalLog,
	}
}

// apply folds one delta into the engine's mutable window set.
func (f *queryFixture) apply(key SeriesKey, windowStart int64, d *AggregateDelta) {
	f.engine.ApplyCommitted(DeltaMap{{Key: key, WindowStart: windowStart}: d})
}

// window is the fixture's current window start.
func (f *queryFixture) window() int64 { return WindowStart(f.now) }

// rangeQuery is a query covering the fixture's whole mutable horizon.
func (f *queryFixture) rangeQuery() Query {
	return Query{
		Tenant: "default",
		Start:  f.now.Add(-30 * time.Minute),
		End:    f.now.Add(time.Minute),
	}
}

func TestQueryDashboardFromMutableMemory(t *testing.T) {
	f := newQueryFixture(t)
	f.apply(f.traceKey("checkout", "POST /pay"), f.window(), spanDelta(9, 1000))
	f.apply(f.traceKey("cart", "GET /cart"), f.window(), spanDelta(3, 4000))
	logs := &AggregateDelta{}
	logs.ObserveLog(f.now, true)
	logs.ObserveLog(f.now, false)
	f.apply(f.logKey("checkout", "payment failed <*>"), f.window(), logs)

	res, err := f.engine.QueryDashboard(context.Background(), f.rangeQuery())
	if err != nil {
		t.Fatalf("QueryDashboard: %v", err)
	}
	// spanDelta marks every observation a request entry point, so both bases
	// agree here; the bases-diverge case has its own test.
	if res.RequestCount != 12 || res.SpanCount != 12 {
		t.Errorf("RequestCount/SpanCount = %d/%d, want 12/12", res.RequestCount, res.SpanCount)
	}
	if res.TotalLogs != 2 {
		t.Errorf("TotalLogs = %d, want 2", res.TotalLogs)
	}
	// spanDelta marks every third observation an error: 3 of 9 plus 1 of 3.
	if res.ErrorRequestCount != 4 || res.SpanErrorCount != 4 {
		t.Errorf("ErrorRequestCount/SpanErrorCount = %d/%d, want 4/4",
			res.ErrorRequestCount, res.SpanErrorCount)
	}
	if res.ActiveServices != 2 {
		t.Errorf("ActiveServices = %d, want 2", res.ActiveServices)
	}
	wantAvg := (9*1000.0 + 3*4000.0) / 12 / 1000.0
	if math.Abs(res.AvgLatencyMs-wantAvg) > 1e-9 {
		t.Errorf("AvgLatencyMs = %v, want %v", res.AvgLatencyMs, wantAvg)
	}
	if res.Coverage != CoverageFull {
		t.Errorf("Coverage = %q, want %q", res.Coverage, CoverageFull)
	}
	if res.Epoch != f.engine.Epoch() {
		t.Errorf("Epoch = %q, want %q", res.Epoch, f.engine.Epoch())
	}
	if len(res.TopFailing) != 2 {
		t.Fatalf("TopFailing = %d entries, want 2", len(res.TopFailing))
	}
	if res.TopFailing[0].Service != "checkout" {
		t.Errorf("top failing service = %q, want checkout", res.TopFailing[0].Service)
	}
}

// TestQueryDashboardPercentileWithinReportedBound is the accuracy contract:
// the p99 must land inside the relative-error bound the response advertises,
// and that bound must come from the merged sketch rather than a constant.
func TestQueryDashboardPercentileWithinReportedBound(t *testing.T) {
	f := newQueryFixture(t)
	d := &AggregateDelta{}
	for i := 1; i <= 1000; i++ {
		d.ObserveSpan(float64(i)*100, false, true)
	}
	f.apply(f.traceKey("checkout", "POST /pay"), f.window(), d)

	res, err := f.engine.QueryDashboard(context.Background(), f.rangeQuery())
	if err != nil {
		t.Fatalf("QueryDashboard: %v", err)
	}
	if !res.Accuracy.Approximate {
		t.Error("Accuracy.Approximate = false, want true for a sketch-derived percentile")
	}
	if res.Accuracy.SketchScale != SketchDefaultScale {
		t.Errorf("SketchScale = %d, want %d", res.Accuracy.SketchScale, SketchDefaultScale)
	}
	if res.Accuracy.RelativeErrorBound <= 0 {
		t.Fatalf("RelativeErrorBound = %v, want > 0", res.Accuracy.RelativeErrorBound)
	}
	p99 := res.LatencyProvenance.P99
	if p99 == nil || p99.Status != latency.StatusApproximate || p99.Method != latency.MethodDDSketch || p99.SampleCount != 1000 || p99.SketchScale != SketchDefaultScale || p99.Degraded {
		t.Fatalf("latency provenance = %+v", p99)
	}
	want := 99000.0 // the 990th of 1000 values, in microseconds
	rel := math.Abs(res.P99LatencyMicros-want) / want
	if rel > res.Accuracy.RelativeErrorBound {
		t.Errorf("p99 = %v (rel err %v) exceeds advertised bound %v",
			res.P99LatencyMicros, rel, res.Accuracy.RelativeErrorBound)
	}
}

// TestAccuracyMetadataTracksDownscaledMerge pins the reason the bound is
// computed and not hard-coded: merging a coarser sketch downscales the result,
// so the advertised error must GROW past the scale-4 2.17%.
func TestAccuracyMetadataTracksDownscaledMerge(t *testing.T) {
	f := newQueryFixture(t)

	fine := &AggregateDelta{}
	fine.ObserveSpan(1000, false, true)

	coarse := &AggregateDelta{}
	coarse.ObserveSpan(1000, false, true)
	sk, err := NewSketchAtScale(1)
	if err != nil {
		t.Fatalf("NewSketchAtScale: %v", err)
	}
	sk.Observe(1000)
	coarse.Sketch = sk

	f.apply(f.traceKey("a", "op"), f.window(), fine)
	f.apply(f.traceKey("b", "op"), f.window(), coarse)

	res, err := f.engine.QueryDashboard(context.Background(), f.rangeQuery())
	if err != nil {
		t.Fatalf("QueryDashboard: %v", err)
	}
	if res.Accuracy.SketchScale != 1 {
		t.Fatalf("SketchScale = %d, want 1 (the coarser of the merged pair)", res.Accuracy.SketchScale)
	}
	defaultBound := AccuracyFromSketch(NewSketch()).RelativeErrorBound
	if res.Accuracy.RelativeErrorBound <= defaultBound {
		t.Errorf("RelativeErrorBound = %v, want greater than the scale-%d bound %v",
			res.Accuracy.RelativeErrorBound, SketchDefaultScale, defaultBound)
	}
}

// TestAccuracyMetadataReportsCollapse covers the other way a sketch leaves its
// nominal bound: a collapsed sketch's low tail is outside it.
func TestAccuracyMetadataReportsCollapse(t *testing.T) {
	s := NewSketch()
	// A dynamic range wide enough to force a collapse into the lowest bin.
	s.Observe(1)
	s.Observe(1e12)
	if !s.Collapsed() {
		t.Skip("sketch did not collapse at this scale; nothing to assert")
	}
	acc := AccuracyFromSketch(s)
	if !acc.Degraded {
		t.Error("Degraded = false for a collapsed sketch, want true")
	}
}

// TestQueryBucketsOnePointPerWindow pins the traffic shape: one point per
// five-minute window, ordered, with error counts carried through.
func TestQueryBucketsOnePointPerWindow(t *testing.T) {
	f := newQueryFixture(t)
	cur := f.window()
	prev := cur - int64(WindowSize/time.Second)
	f.apply(f.traceKey("checkout", "op"), prev, spanDelta(3, 1000))
	f.apply(f.traceKey("checkout", "op"), cur, spanDelta(6, 1000))

	res, err := f.engine.QueryBuckets(context.Background(), f.rangeQuery())
	if err != nil {
		t.Fatalf("QueryBuckets: %v", err)
	}
	if len(res.Points) != 2 {
		t.Fatalf("points = %d, want 2", len(res.Points))
	}
	if !res.Points[0].WindowStart.Before(res.Points[1].WindowStart) {
		t.Error("points are not ordered oldest first")
	}
	if res.Points[0].RequestCount != 3 || res.Points[1].RequestCount != 6 {
		t.Errorf("request counts = %d,%d want 3,6", res.Points[0].RequestCount, res.Points[1].RequestCount)
	}
	if res.Points[0].SpanCount != 3 || res.Points[1].SpanCount != 6 {
		t.Errorf("span counts = %d,%d want 3,6", res.Points[0].SpanCount, res.Points[1].SpanCount)
	}
	if res.Points[1].ErrorRequestCount != 2 || res.Points[1].SpanErrorCount != 2 {
		t.Errorf("error counts = %d,%d want 2,2",
			res.Points[1].ErrorRequestCount, res.Points[1].SpanErrorCount)
	}
}

func TestQueryTopologyNodesFromAggregates(t *testing.T) {
	f := newQueryFixture(t)
	f.apply(f.traceKey("cart", "op"), f.window(), spanDelta(3, 2000))
	f.apply(f.traceKey("checkout", "op"), f.window(), spanDelta(6, 1000))

	res, err := f.engine.QueryTopology(context.Background(), f.rangeQuery())
	if err != nil {
		t.Fatalf("QueryTopology: %v", err)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(res.Nodes))
	}
	if res.Nodes[0].Service != "cart" || res.Nodes[1].Service != "checkout" {
		t.Errorf("nodes not sorted by service: %v", res.Nodes)
	}
	if res.Nodes[1].Count != 6 {
		t.Errorf("checkout count = %d, want 6", res.Nodes[1].Count)
	}
	if math.Abs(res.Nodes[1].AvgLatencyMs-1.0) > 1e-9 {
		t.Errorf("checkout avg latency = %v ms, want 1", res.Nodes[1].AvgLatencyMs)
	}
}

// TestOwnershipIsExclusivePerWindow is the read-consistency contract of #164:
// a window owned by the engine is read ONLY from memory even when the store
// holds rows for it, and after finalization it is read ONLY from the store.
// Neither transition may omit or double-count.
func TestOwnershipIsExclusivePerWindow(t *testing.T) {
	f := newQueryFixture(t)
	st := newStubStore()
	f.engine.SetStore(st)

	key := f.traceKey("checkout", "op")
	w := f.window()
	f.apply(key, w, spanDelta(10, 1000))
	// A crash-recovery checkpoint row exists for the SAME window. Row presence
	// must not add a second count.
	st.put(1, key, w, spanDelta(10, 1000))

	res, err := f.engine.QueryDashboard(context.Background(), f.rangeQuery())
	if err != nil {
		t.Fatalf("QueryDashboard: %v", err)
	}
	if res.SpanCount != 10 {
		t.Fatalf("SpanCount = %d with a checkpointed mutable window, want 10", res.SpanCount)
	}
	for _, r := range st.readRanges {
		if w >= r[0] && w < r[1] {
			t.Fatalf("store was asked for memory-owned window %d (range %v)", w, r)
		}
	}

	own := f.engine.Ownership()
	if !own.OwnsInMemory(w) {
		t.Fatalf("window %d is not memory-owned before finalization", w)
	}

	// Finalization hands the window over. The count must stay 10 — now served
	// entirely from the store.
	f.engine.MarkFinalized(w)
	own = f.engine.Ownership()
	if own.OwnsInMemory(w) {
		t.Fatal("window is still memory-owned after MarkFinalized")
	}
	if own.FinalizedWatermark != w {
		t.Errorf("FinalizedWatermark = %d, want %d", own.FinalizedWatermark, w)
	}

	res, err = f.engine.QueryDashboard(context.Background(), f.rangeQuery())
	if err != nil {
		t.Fatalf("QueryDashboard after finalize: %v", err)
	}
	if res.SpanCount != 10 {
		t.Fatalf("SpanCount = %d after finalization, want 10", res.SpanCount)
	}
}

// TestQueryRejectsUnboundedRange keeps the store-side bound rule visible at the
// facade: a query without a forward range is refused, not clamped silently.
func TestQueryRejectsUnboundedRange(t *testing.T) {
	f := newQueryFixture(t)
	if _, err := f.engine.QueryDashboard(context.Background(), Query{Tenant: "default"}); err == nil {
		t.Fatal("QueryDashboard accepted an empty range")
	}
	if _, err := f.engine.QueryDashboard(context.Background(), Query{Start: f.now.Add(-time.Hour), End: f.now}); err == nil {
		t.Fatal("QueryDashboard accepted a query with no tenant")
	}
}

// TestFinalizeHandsOwnershipToTheStore pins #194 blocker 6: rollover closes an
// expired window but keeps it memory-owned, and only a committed finalize moves
// ownership and the watermark. Advancing at rollover routed reads to a store
// that had no buckets yet, so the window silently vanished from queries.
func TestFinalizeHandsOwnershipToTheStore(t *testing.T) {
	f := newQueryFixture(t)
	old := f.window() - int64((WindowSize+AllowedLateness+WindowSize)/time.Second)
	f.engine.own.mu.Lock()
	f.engine.own.mutable[old] = struct{}{}
	f.engine.own.mu.Unlock()

	f.engine.Rollover(f.now)
	own := f.engine.Ownership()
	if !own.OwnsInMemory(old) {
		t.Fatal("closed window lost memory ownership before it was finalized")
	}
	if own.FinalizedWatermark >= old {
		t.Errorf("FinalizedWatermark = %d, want below %d until finalize commits", own.FinalizedWatermark, old)
	}

	f.engine.MarkFinalized(old)
	own = f.engine.Ownership()
	if own.OwnsInMemory(old) {
		t.Fatal("finalized window is still memory-owned")
	}
	if own.FinalizedWatermark < old {
		t.Errorf("FinalizedWatermark = %d, want at least %d", own.FinalizedWatermark, old)
	}
}

// TestQueryTopologyEdgesShareTheNodeQuery is #194 finding 15: edges come out of
// the SAME plan as the nodes — same tenant, same range, same ownership
// snapshot — with the memory-owned and store-owned halves added rather than one
// of them silently winning.
func TestQueryTopologyEdgesShareTheNodeQuery(t *testing.T) {
	f := newQueryFixture(t)
	st := newStubStore()
	f.engine.SetStore(st)

	w := f.window()
	old := w - 3*int64(WindowSize/time.Second)

	f.apply(f.traceKey("cart", "op"), w, spanDelta(3, 2000))
	f.apply(f.traceKey("checkout", "op"), w, spanDelta(6, 1000))
	f.apply(f.edgeKey("cart", "checkout"), w, spanDelta(6, 1000))

	// Store-owned rows in the same range: they must ADD to the memory half.
	st.put(1, f.traceKey("cart", "op"), old, spanDelta(3, 2000))
	st.put(2, f.edgeKey("cart", "checkout"), old, spanDelta(3, 2000))

	// Another tenant's edge over the same window must never leak in.
	otherID, ok := f.engine.TenantID("other")
	if !ok {
		t.Fatal("second tenant was refused")
	}
	st.put(3, SeriesKey{
		TenantID:  otherID,
		ServiceID: f.engine.Cache().Intern(otherID, KindService, "cart"),
		NameID:    f.engine.Cache().Intern(otherID, KindOperation, "checkout"),
		Signal:    SignalServiceEdge,
	}, old, spanDelta(30, 1000))

	res, err := f.engine.QueryTopology(context.Background(), f.rangeQuery())
	if err != nil {
		t.Fatalf("QueryTopology: %v", err)
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %+v, want exactly one cart->checkout edge", res.Edges)
	}
	edge := res.Edges[0]
	if edge.Source != "cart" || edge.Target != "checkout" {
		t.Fatalf("edge = %s->%s, want cart->checkout", edge.Source, edge.Target)
	}
	if edge.CallCount != 9 {
		t.Errorf("edge call count = %d, want 9 (6 memory-owned + 3 store-owned)", edge.CallCount)
	}
	// spanDelta marks every third observation an error: 2 of 6 plus 1 of 3.
	if math.Abs(edge.ErrorRate-3.0/9.0) > 1e-9 {
		t.Errorf("edge error rate = %v, want %v", edge.ErrorRate, 3.0/9.0)
	}
	wantLatency := (6*1000.0 + 3*2000.0) / 9 / 1000.0
	if math.Abs(edge.AvgLatencyMs-wantLatency) > 1e-9 {
		t.Errorf("edge avg latency = %v ms, want %v", edge.AvgLatencyMs, wantLatency)
	}
	// The edge series must not be counted as a service node.
	if len(res.Nodes) != 2 {
		t.Fatalf("nodes = %+v, want cart and checkout only", res.Nodes)
	}
	if res.Coverage != CoverageFull {
		t.Errorf("coverage = %q, want %q", res.Coverage, CoverageFull)
	}
}

// TestQueryTopologyServiceFilterKeepsTheGraphClosed proves a filtered topology
// is a SUBGRAPH: an edge whose other end was filtered out is dropped rather
// than left hanging off a node the response does not contain.
func TestQueryTopologyServiceFilterKeepsTheGraphClosed(t *testing.T) {
	f := newQueryFixture(t)
	w := f.window()
	f.apply(f.traceKey("cart", "op"), w, spanDelta(3, 2000))
	f.apply(f.traceKey("checkout", "op"), w, spanDelta(6, 1000))
	f.apply(f.edgeKey("cart", "checkout"), w, spanDelta(6, 1000))

	q := f.rangeQuery()
	q.Services = []string{"checkout"}
	res, err := f.engine.QueryTopology(context.Background(), q)
	if err != nil {
		t.Fatalf("QueryTopology: %v", err)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].Service != "checkout" {
		t.Fatalf("nodes = %+v, want checkout only", res.Nodes)
	}
	if len(res.Edges) != 0 {
		t.Fatalf("edges = %+v, want none: cart was filtered out", res.Edges)
	}
}
