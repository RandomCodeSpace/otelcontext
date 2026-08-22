package aggregate

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// The #197 read contract, end to end (#194 blockers 4 and 5).
//
// Every totals assertion here is against a NAIVE REFERENCE built in the test:
// the same seed data summed by plain Go loops, with no store, no SQL and no row
// cap anywhere near it. If the engine and the reference disagree, the engine is
// wrong — that is the entire point of writing the reference twice.
//
// Row volume is the expensive part: the pure-Go SQLite driver is ~30x slower
// under -race, so the seeded rows are inserted with direct prepared statements
// rather than through CommitGroup + FinalizeWindow (which read-modify-write
// every row), and the whole store is built ONCE and shared by subtests.

// Seed shape. 860 series x 24 windows is 20,640 rows: past the 20,000-row read
// cap that used to truncate this query in silence, and not much past it —
// race-instrumented SQLite charges about half a millisecond per inserted row.
const (
	seedServices    = 20
	seedOpsPerSvc   = 42
	seedWindows     = 24
	seedFinalizedTo = 20 // windows [0,20) are materialized; [20,24) stay in the delta log
)

// seedDelta is the deterministic per-row trace contribution. Both bases move
// independently so a query that confuses them cannot accidentally pass.
func seedDelta(i int) *AggregateDelta {
	spans := uint64(3 + i%5)
	return &AggregateDelta{
		Count:             spans,
		ErrorCount:        uint64(i % 3),
		RequestCount:      uint64(1 + i%2),
		ErrorRequestCount: uint64(i % 2),
		DurationCount:     spans,
		DurationSum:       float64(spans) * float64(100+i%17),
	}
}

// seedLogDelta is the per-row log contribution, so TotalLogs is exercised by
// the same SQL aggregation as the trace counters.
func seedLogDelta(i int) *AggregateDelta {
	n := uint64(1 + i%4)
	return &AggregateDelta{Count: n, LogCount: n, ErrorCount: uint64(i % 2)}
}

func svcName(i int) string { return fmt.Sprintf("svc-%02d", i) }

// refTotals is the naive reference's answer.
type refTotals struct {
	requests, errRequests uint64
	spans, spanErrors     uint64
	logs                  uint64
	durCount              uint64
	durSum                float64
	services              map[string]struct{}
	rows                  int
	perWindowReq          []int64
	perWindowSpans        []int64
}

// buildReference walks the seed specification with plain loops and sums it.
// keep reports whether a service is inside the query's service filter.
func buildReference(keep func(string) bool) *refTotals {
	ref := &refTotals{
		services:       map[string]struct{}{},
		perWindowReq:   make([]int64, seedWindows),
		perWindowSpans: make([]int64, seedWindows),
	}
	i := 0
	for w := 0; w < seedWindows; w++ {
		for s := 0; s < seedServices; s++ {
			for op := 0; op < seedOpsPerSvc; op++ {
				d := seedDelta(i)
				i++
				ref.rows++
				if !keep(svcName(s)) {
					continue
				}
				ref.services[svcName(s)] = struct{}{}
				ref.requests += d.RequestCount
				ref.errRequests += d.ErrorRequestCount
				ref.spans += d.Count
				ref.spanErrors += d.ErrorCount
				ref.durCount += d.DurationCount
				ref.durSum += d.DurationSum
				ref.perWindowReq[w] += int64(d.RequestCount)
				ref.perWindowSpans[w] += int64(d.Count)
			}
			ld := seedLogDelta(i)
			i++
			ref.rows++
			if !keep(svcName(s)) {
				continue
			}
			ref.logs += ld.LogCount
		}
	}
	return ref
}

// seedFixture is an engine with a live SQLite store holding the seeded rows.
type seedFixture struct {
	engine *Engine
	store  *SQLiteStore
	base   int64
}

// insertRows writes rows straight into one aggregate table with a prepared
// statement. It deliberately bypasses CommitGroup: the write path has its own
// tests, and this one only needs the rows to exist.
//
// One row per Exec, not a multi-row VALUES: batching was measured here and is
// SLOWER, for the same reason the production commit path gives (store_sqlite.go
// mergeDeltas) — the cost is per-parameter binding, and a wide statement pays
// extra to compile on top of it.
func insertRows(t *testing.T, tx *sql.Tx, table string, ids []SeriesID, windows []int64, deltas []*AggregateDelta) {
	t.Helper()
	stmt, err := tx.Prepare(`INSERT INTO ` + table + ` (window_start, series_id, ` + deltaColumnList + `)
		VALUES (?,?,` + deltaValuePlaceholders + `)`)
	if err != nil {
		t.Fatalf("prepare %s insert: %v", table, err)
	}
	defer func() { _ = stmt.Close() }()
	var (
		scratch []byte
		args    = make([]any, 0, 22)
	)
	for i := range ids {
		var sketch []byte
		if deltas[i].Sketch != nil {
			scratch = deltas[i].Sketch.AppendTo(scratch[:0])
			sketch = scratch
		}
		args = append(args[:0], windows[i], int64(ids[i]))
		args = append(args, deltaArgs(deltas[i], sketch)...)
		if _, err := stmt.Exec(args...); err != nil {
			t.Fatalf("insert into %s: %v", table, err)
		}
	}
}

// seedSeries writes the series identities the seeded rows join against,
// through the store's own registration statement.
func seedSeries(t *testing.T, tx *sql.Tx, rows []SeriesRow) {
	t.Helper()
	if err := insertSeries(tx, rows); err != nil {
		t.Fatalf("insert series: %v", err)
	}
}

// newSeedFixture builds the store contents described by the seed constants.
// Windows below seedFinalizedTo live in aggregate_buckets; the rest live in the
// delta log, so every read here has to cover BOTH tables.
func newSeedFixture(t *testing.T) *seedFixture {
	t.Helper()
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e := testEngine(t, now)
	store := newTestStoreAt(t, filepath.Join(t.TempDir(), "aggregate.db"), StoreConfig{})
	e.SetStore(store)

	tenantID := e.TenantID("default")
	windowSecs := int64(WindowSize / time.Second)
	base := WindowStart(now) - int64(seedWindows+2)*windowSecs

	traceKey := func(s, op int) SeriesKey {
		return SeriesKey{
			TenantID:  tenantID,
			ServiceID: e.Cache().Intern(tenantID, KindService, svcName(s)),
			NameID:    e.Cache().Intern(tenantID, KindOperation, fmt.Sprintf("op-%02d", op)),
			Signal:    SignalTraceOp,
			Variant:   SpanKindServer,
		}
	}
	logKey := func(s int) SeriesKey {
		return SeriesKey{
			TenantID:  tenantID,
			ServiceID: e.Cache().Intern(tenantID, KindService, svcName(s)),
			NameID:    e.Cache().Intern(tenantID, KindLogTemplate, "template <*>"),
			Signal:    SignalLog,
		}
	}

	var (
		series   []SeriesRow
		traceIDs = make([][]SeriesID, seedServices)
		logIDs   = make([]SeriesID, seedServices)
		next     = SeriesID(1)
	)
	for s := 0; s < seedServices; s++ {
		traceIDs[s] = make([]SeriesID, seedOpsPerSvc)
		for op := 0; op < seedOpsPerSvc; op++ {
			series = append(series, SeriesRow{ID: next, Key: traceKey(s, op)})
			traceIDs[s][op] = next
			next++
		}
		series = append(series, SeriesRow{ID: next, Key: logKey(s)})
		logIDs[s] = next
		next++
	}

	// Group the rows by destination table, then write each in one pass.
	var (
		bucketIDs, deltaIDs         []SeriesID
		bucketWindows, deltaWindows []int64
		bucketDeltas, deltaDeltas   []*AggregateDelta
	)
	i := 0
	for w := 0; w < seedWindows; w++ {
		window := base + int64(w)*windowSecs
		finalized := w < seedFinalizedTo
		add := func(id SeriesID, d *AggregateDelta) {
			if finalized {
				bucketIDs, bucketWindows, bucketDeltas = append(bucketIDs, id), append(bucketWindows, window), append(bucketDeltas, d)
				return
			}
			deltaIDs, deltaWindows, deltaDeltas = append(deltaIDs, id), append(deltaWindows, window), append(deltaDeltas, d)
		}
		for s := 0; s < seedServices; s++ {
			for op := 0; op < seedOpsPerSvc; op++ {
				add(traceIDs[s][op], seedDelta(i))
				i++
			}
			add(logIDs[s], seedLogDelta(i))
			i++
		}
	}

	tx, err := store.writer.Begin()
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	seedSeries(t, tx, series)
	insertRows(t, tx, "aggregate_buckets", bucketIDs, bucketWindows, bucketDeltas)
	insertRows(t, tx, "aggregate_delta_log", deltaIDs, deltaWindows, deltaDeltas)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	return &seedFixture{engine: e, store: store, base: base}
}

// query spans every seeded window and nothing else.
func (f *seedFixture) query(services ...string) Query {
	return Query{
		Tenant:   "default",
		Start:    time.Unix(f.base, 0).UTC(),
		End:      time.Unix(f.base+int64(seedWindows)*int64(WindowSize/time.Second), 0).UTC(),
		Services: services,
	}
}

// selector is the store-level equivalent of query.
func (f *seedFixture) selector() Selector {
	return Selector{
		TenantID: f.engine.TenantID("default"),
		Start:    f.base,
		End:      f.base + int64(seedWindows)*int64(WindowSize/time.Second),
	}
}

// assertMatchesReference compares a dashboard answer against the reference.
func assertMatchesReference(t *testing.T, res *DashboardResult, ref *refTotals) {
	t.Helper()
	for _, c := range []struct {
		name      string
		got, want uint64
	}{
		{"RequestCount", uint64(res.RequestCount), ref.requests},
		{"ErrorRequestCount", uint64(res.ErrorRequestCount), ref.errRequests},
		{"SpanCount", uint64(res.SpanCount), ref.spans},
		{"SpanErrorCount", uint64(res.SpanErrorCount), ref.spanErrors},
		{"TotalLogs", uint64(res.TotalLogs), ref.logs},
		{"ActiveServices", uint64(res.ActiveServices), uint64(len(ref.services))},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, reference says %d", c.name, c.got, c.want)
		}
	}
	wantAvg := 0.0
	if ref.durCount > 0 {
		wantAvg = ref.durSum / float64(ref.durCount) / 1000.0
	}
	if math.Abs(res.AvgLatencyMs-wantAvg) > 1e-9 {
		t.Errorf("AvgLatencyMs = %v, reference says %v", res.AvgLatencyMs, wantAvg)
	}
	wantRate := 0.0
	if ref.requests > 0 {
		wantRate = float64(ref.errRequests) / float64(ref.requests) * 100
	}
	if math.Abs(res.RequestErrorRate-wantRate) > 1e-9 {
		t.Errorf("RequestErrorRate = %v, reference says %v", res.RequestErrorRate, wantRate)
	}
	if res.Coverage != CoverageFull {
		t.Errorf("Coverage = %q, want %q", res.Coverage, CoverageFull)
	}
}

// TestReadsAreCompleteBeyondTheRowCap is #194 blocker 4's acceptance test. The
// seeded range is past MaxReadRows, so before the #197 read split every one of
// these answers was silently the first 20,000 rows ordered by (window, series)
// — reported as CoverageFull. One fixture, several subtests: the seed is the
// expensive part and it proves the same store for all of them.
func TestReadsAreCompleteBeyondTheRowCap(t *testing.T) {
	f := newSeedFixture(t)
	all := buildReference(func(string) bool { return true })
	if all.rows <= MaxReadRows {
		t.Fatalf("seed produced %d rows, which does not exercise the %d-row cap", all.rows, MaxReadRows)
	}

	t.Run("dashboard totals match the reference", func(t *testing.T) {
		res, err := f.engine.QueryDashboard(f.query())
		if err != nil {
			t.Fatalf("QueryDashboard: %v", err)
		}
		assertMatchesReference(t, res, all)
	})

	t.Run("filtered dashboard totals match the reference", func(t *testing.T) {
		keep := map[string]struct{}{svcName(0): {}, svcName(3): {}, svcName(11): {}, svcName(19): {}}
		names := make([]string, 0, len(keep))
		for n := range keep {
			names = append(names, n)
		}
		ref := buildReference(func(s string) bool { _, ok := keep[s]; return ok })
		res, err := f.engine.QueryDashboard(f.query(names...))
		if err != nil {
			t.Fatalf("QueryDashboard(filtered): %v", err)
		}
		assertMatchesReference(t, res, ref)

		// A filter matching nothing must produce zeros, not the unfiltered
		// answer — the cheapest way for a pushed-down filter to be wrong.
		empty, err := f.engine.QueryDashboard(f.query("no-such-service"))
		if err != nil {
			t.Fatalf("QueryDashboard(empty filter): %v", err)
		}
		if empty.RequestCount != 0 || empty.SpanCount != 0 || empty.TotalLogs != 0 {
			t.Fatalf("unmatched filter returned %+v, want zeros", empty)
		}
	})

	t.Run("traffic totals match the reference per window", func(t *testing.T) {
		res, err := f.engine.QueryBuckets(f.query())
		if err != nil {
			t.Fatalf("QueryBuckets: %v", err)
		}
		if len(res.Points) != seedWindows {
			t.Fatalf("points = %d, want one per seeded window (%d)", len(res.Points), seedWindows)
		}
		var sameOnBothBases bool
		for w, p := range res.Points {
			if p.RequestCount != all.perWindowReq[w] || p.SpanCount != all.perWindowSpans[w] {
				t.Fatalf("window %d = %d requests / %d spans, reference says %d / %d",
					w, p.RequestCount, p.SpanCount, all.perWindowReq[w], all.perWindowSpans[w])
			}
			sameOnBothBases = sameOnBothBases || p.RequestCount == p.SpanCount
		}
		if sameOnBothBases {
			t.Fatal("the seed made requests and spans identical in some window; the test cannot tell the bases apart")
		}
	})

	t.Run("topology nodes match the reference", func(t *testing.T) {
		res, err := f.engine.QueryTopology(f.query())
		if err != nil {
			t.Fatalf("QueryTopology: %v", err)
		}
		if len(res.Nodes) != seedServices {
			t.Fatalf("nodes = %d, want %d", len(res.Nodes), seedServices)
		}
		var spans, requests int64
		for _, n := range res.Nodes {
			spans += n.Count
			requests += n.RequestCount
		}
		if uint64(spans) != all.spans || uint64(requests) != all.requests {
			t.Fatalf("topology totals = %d spans / %d requests, reference says %d / %d",
				spans, requests, all.spans, all.requests)
		}
	})

	t.Run("generic reads page to completion and say when they do not", func(t *testing.T) {
		sel := f.selector()
		first, err := f.store.ReadBuckets(sel)
		if err != nil {
			t.Fatalf("ReadBuckets: %v", err)
		}
		if !first.Truncated {
			t.Fatalf("a %d-row range returned %d rows without reporting truncation",
				all.rows, len(first.Buckets))
		}
		if first.Limit != MaxReadRows {
			t.Fatalf("applied limit = %d, want the store cap %d", first.Limit, MaxReadRows)
		}

		seen := make(map[BucketCursor]struct{}, all.rows)
		page, pages := first, 1
		for {
			for _, b := range page.Buckets {
				c := BucketCursor{WindowStart: b.WindowStart, SeriesID: b.SeriesID, Source: b.Source}
				if _, dup := seen[c]; dup {
					t.Fatalf("paging returned row %+v twice", c)
				}
				seen[c] = struct{}{}
			}
			if !page.Truncated {
				break
			}
			sel.After = page.Next
			if page, err = f.store.ReadBuckets(sel); err != nil {
				t.Fatalf("ReadBuckets(page %d): %v", pages, err)
			}
			pages++
		}
		if len(seen) != all.rows {
			t.Fatalf("paging to completion saw %d rows, seeded %d", len(seen), all.rows)
		}
		if pages < 2 {
			t.Fatal("the whole range fitted in one page; truncation was never exercised")
		}
	})
}

// TestPercentilePathReadsEverySketchBearingRow proves the p99 is not built from
// the first page: the merged sketch must have observed every seeded duration,
// across materialized buckets and un-finalized delta rows alike.
//
// The row cap is shrunk rather than the row count raised: what matters is that
// the read spans more pages than one, not the absolute number, and encoding
// 20,000 sketches through a race-instrumented SQLite costs minutes.
func TestPercentilePathReadsEverySketchBearingRow(t *testing.T) {
	restore := MaxReadRows
	MaxReadRows = 120
	t.Cleanup(func() { MaxReadRows = restore })

	const rows = 500 // four pages plus a remainder
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e := testEngine(t, now)
	store := newTestStoreAt(t, filepath.Join(t.TempDir(), "aggregate.db"), StoreConfig{})
	e.SetStore(store)

	tenantID := e.TenantID("default")
	windowSecs := int64(WindowSize / time.Second)
	base := WindowStart(now) - 4*windowSecs

	var (
		series                    []SeriesRow
		bucketIDs, deltaIDs       []SeriesID
		bucketWins, deltaWins     []int64
		bucketDeltas, deltaDeltas []*AggregateDelta
		want                      uint64
		withoutSketch             int
	)
	for i := 0; i < rows; i++ {
		id := SeriesID(i + 1)
		series = append(series, SeriesRow{ID: id, Key: SeriesKey{
			TenantID:  tenantID,
			ServiceID: e.Cache().Intern(tenantID, KindService, "svc"),
			NameID:    e.Cache().Intern(tenantID, KindOperation, fmt.Sprintf("op-%05d", i)),
			Signal:    SignalTraceOp,
			Variant:   SpanKindServer,
		}})
		d := &AggregateDelta{}
		// Every seventh row carries no duration at all. SketchOnly must skip
		// those without losing its place in the keyset.
		if i%7 == 6 {
			d.Count = 1
			d.RequestCount = 1
			withoutSketch++
		} else {
			d.ObserveSpan(float64(100+i%900), false, true)
			want += d.Sketch.Count()
		}
		// Half in a materialized window, half still in the delta log.
		if i%2 == 0 {
			bucketIDs, bucketWins, bucketDeltas = append(bucketIDs, id), append(bucketWins, base), append(bucketDeltas, d)
			continue
		}
		deltaIDs, deltaWins, deltaDeltas = append(deltaIDs, id), append(deltaWins, base+windowSecs), append(deltaDeltas, d)
	}
	tx, err := store.writer.Begin()
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	seedSeries(t, tx, series)
	insertRows(t, tx, "aggregate_buckets", bucketIDs, bucketWins, bucketDeltas)
	insertRows(t, tx, "aggregate_delta_log", deltaIDs, deltaWins, deltaDeltas)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	if withoutSketch == 0 {
		t.Fatal("seed produced no sketch-free rows; SketchOnly is not exercised")
	}

	var merged *Sketch
	visited := 0
	sel := Selector{TenantID: tenantID, Start: base, End: base + 3*windowSecs, Signal: SignalTraceOp, SketchOnly: true}
	if err := e.pageStore(sel, func(_ int64, _ SeriesKey, d *AggregateDelta) {
		visited++
		if d.Sketch == nil {
			t.Error("SketchOnly read returned a row with no sketch")
			return
		}
		if merged == nil {
			merged = NewSketchAtScaleUnchecked(d.Sketch.Scale())
		}
		merged.Merge(d.Sketch)
	}); err != nil {
		t.Fatalf("pageStore: %v", err)
	}
	if visited != rows-withoutSketch {
		t.Fatalf("paged read visited %d rows, want %d sketch-bearing rows", visited, rows-withoutSketch)
	}
	if merged == nil || merged.Count() != want {
		t.Fatalf("merged sketch observed %v of %d observations", merged, want)
	}
}

// --- blocker 5: request counting -------------------------------------------

// reduceTotals sums a reducer's deltas on both bases.
func reduceTotals(r *Reducer) (spans, spanErrors, requests, errRequests uint64) {
	for _, d := range r.Deltas() {
		spans += d.Count
		spanErrors += d.ErrorCount
		requests += d.RequestCount
		errRequests += d.ErrorRequestCount
	}
	return
}

// otlpStatusError is the OTLP STATUS_CODE_ERROR numeric value; otlpKind* are
// the OTLP SpanKind numeric values.
const (
	otlpStatusError  = 2
	otlpKindInternal = 1
	otlpKindServer   = 2
	otlpKindClient   = 3
)

// spanAt builds a SpanInput for the reducer's arrival time.
func spanAt(ts time.Time, op string, root bool, kind, status int32) SpanInput {
	return SpanInput{
		Tenant:         "default",
		Service:        "checkout",
		SpanName:       op,
		SpanKind:       kind,
		StatusCode:     status,
		Root:           root,
		Timestamp:      ts,
		DurationMicros: 1000,
	}
}

// TestOneTraceOfTwentySpansIsOneRequest is #194 blocker 5's acceptance test.
// The trace has one entry point and nineteen internal spans; traffic must read
// 1, and per-operation diagnostics must still see all 20.
func TestOneTraceOfTwentySpansIsOneRequest(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e := testEngine(t, now)
	r := e.NewReducer(now)
	r.ReduceSpan(spanAt(now, "POST /pay", true, otlpKindServer, 0))
	for i := 0; i < 19; i++ {
		r.ReduceSpan(spanAt(now, fmt.Sprintf("step-%d", i), false, otlpKindInternal, 0))
	}
	spans, _, requests, _ := reduceTotals(r)
	if spans != 20 {
		t.Errorf("spans = %d, want 20", spans)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}
}

// TestRequestCountingCountsOncePerEntryPoint covers the documented
// approximation: a distributed trace with several entry points counts once per
// entry point, and a span that is both root AND server counts once.
func TestRequestCountingCountsOncePerEntryPoint(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e := testEngine(t, now)
	r := e.NewReducer(now)
	// Root and SERVER at once: one span, one request.
	r.ReduceSpan(spanAt(now, "POST /pay", true, otlpKindServer, 0))
	// A downstream service's SERVER span: a second entry point.
	r.ReduceSpan(spanAt(now, "GET /stock", false, otlpKindServer, 0))
	// A root span that is not a server span still starts a request.
	r.ReduceSpan(spanAt(now, "cron tick", true, otlpKindInternal, 0))
	// Neither root nor server: not a request.
	r.ReduceSpan(spanAt(now, "db query", false, otlpKindClient, 0))

	spans, _, requests, _ := reduceTotals(r)
	if spans != 4 {
		t.Errorf("spans = %d, want 4", spans)
	}
	if requests != 3 {
		t.Errorf("requests = %d, want 3", requests)
	}
}

// TestRequestErrorRateReflectsEntryPointStatusOnly is #197 Q3: a failing
// internal span moves the SPAN error rate and must leave the headline request
// error rate alone.
func TestRequestErrorRateReflectsEntryPointStatusOnly(t *testing.T) {
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e := testEngine(t, now)

	r := e.NewReducer(now)
	r.ReduceSpan(spanAt(now, "POST /pay", true, otlpKindServer, 0))
	for i := 0; i < 4; i++ {
		r.ReduceSpan(spanAt(now, fmt.Sprintf("step-%d", i), false, otlpKindInternal, otlpStatusError))
	}
	spans, spanErrors, requests, errRequests := reduceTotals(r)
	if spans != 5 || spanErrors != 4 {
		t.Errorf("span basis = %d/%d, want 5/4", spans, spanErrors)
	}
	if requests != 1 || errRequests != 0 {
		t.Errorf("request basis = %d/%d, want 1/0 — internal failures are not failed requests", requests, errRequests)
	}

	// The entry point itself failing IS a failed request.
	r2 := e.NewReducer(now)
	r2.ReduceSpan(spanAt(now, "POST /pay", true, otlpKindServer, otlpStatusError))
	_, _, requests2, errRequests2 := reduceTotals(r2)
	if requests2 != 1 || errRequests2 != 1 {
		t.Errorf("failed entry point = %d/%d requests, want 1/1", requests2, errRequests2)
	}
}

// TestStoreRefusesAV2FileAndRebuildsOnDemand is #197 Q5's fail-closed policy at
// the v2 -> v3 bump: request_count cannot be derived from a v2 file, so the only
// two answers are "run the old binary" and "destroy and recreate". The wanted
// version is read from StoreSchemaVersion, not spelled out: the policy is what
// this test pins, and it does not change when a later bump adds columns.
func TestStoreRefusesAV2FileAndRebuildsOnDemand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	store := newTestStoreAt(t, path, StoreConfig{})
	original := store.UUID()
	if _, err := store.writer.Exec(
		`UPDATE aggregate_meta SET value = '2' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("tamper meta: %v", err)
	}
	_ = store.Close()

	_, err := OpenSQLiteStore(StoreConfig{Path: path})
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("open of a v2 file = %v, want *SchemaError", err)
	}
	want := fmt.Sprint(StoreSchemaVersion)
	if schemaErr.Got != "2" || schemaErr.Want != want {
		t.Fatalf("mismatch reported %s -> %s, want 2 -> %s", schemaErr.Got, schemaErr.Want, want)
	}

	rebuilt, err := OpenSQLiteStore(StoreConfig{Path: path, AllowRebuild: true})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	defer func() { _ = rebuilt.Close() }()
	if rebuilt.UUID() == original {
		t.Fatal("rebuild kept the old store uuid; it must mint a new identity")
	}
	// The rebuilt schema must actually carry the current columns.
	if err := rebuilt.CommitGroup(&GroupBatch{
		Series: []SeriesRow{{ID: 1, Key: storeKey(1)}},
		Deltas: []DeltaRow{{SeriesID: 1, WindowStart: 600, Delta: &AggregateDelta{Count: 3, RequestCount: 2, ErrorRequestCount: 1}}},
	}); err != nil {
		t.Fatalf("commit into rebuilt store: %v", err)
	}
	sums, err := rebuilt.SumBuckets(Selector{TenantID: 1, Start: 600, End: 900}, 0)
	if err != nil {
		t.Fatalf("SumBuckets: %v", err)
	}
	if len(sums) != 1 || sums[0].RequestCount != 2 || sums[0].ErrorRequestCount != 1 {
		t.Fatalf("rebuilt store summed %+v, want 2 requests / 1 error request", sums)
	}
}
