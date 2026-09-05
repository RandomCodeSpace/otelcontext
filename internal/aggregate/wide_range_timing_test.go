package aggregate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestManualWideRangeTiming is a manual A/B measurement for #219, not a CI
// test: it seeds a wide-range store and times the old paged sketch drain
// (reproduced inline against ReadBuckets) against the streaming VisitSketches
// path. Run with WIDE_RANGE_TIMING=1.
func TestManualWideRangeTiming(t *testing.T) {
	if os.Getenv("WIDE_RANGE_TIMING") == "" {
		t.Skip("manual timing test; set WIDE_RANGE_TIMING=1")
	}
	const (
		seriesN  = 1000
		windowsN = 2016 // 7 days of 5-minute windows
	)
	now := mustTime(t, "2026-08-21T12:02:00Z")
	e := testEngine(t, now)
	store := newTestStoreAt(t, filepath.Join(t.TempDir(), "aggregate.db"), StoreConfig{})
	e.SetStore(store)

	tenantID, _ := e.TenantID("default")
	windowSecs := int64(WindowSize / time.Second)
	base := WindowStart(now) - int64(windowsN+2)*windowSecs

	series := make([]SeriesRow, 0, seriesN)
	deltas := make([]*AggregateDelta, 0, seriesN)
	for i := 0; i < seriesN; i++ {
		id := SeriesID(i + 1)
		series = append(series, SeriesRow{ID: id, Key: SeriesKey{
			TenantID:  tenantID,
			ServiceID: e.Cache().Intern(tenantID, KindService, fmt.Sprintf("svc-%03d", i%50)),
			NameID:    e.Cache().Intern(tenantID, KindOperation, fmt.Sprintf("op-%05d", i)),
			Signal:    SignalTraceOp,
			Variant:   SpanKindServer,
		}})
		d := &AggregateDelta{}
		d.ObserveSpan(float64(100+i%900), false, true)
		deltas = append(deltas, d)
	}

	tx, err := store.writer.Begin()
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	seedSeries(t, tx, series)
	ids := make([]SeriesID, seriesN)
	wins := make([]int64, seriesN)
	seedStart := time.Now()
	for w := 0; w < windowsN; w++ {
		for i := 0; i < seriesN; i++ {
			ids[i] = SeriesID(i + 1)
			wins[i] = base + int64(w)*windowSecs
		}
		insertRows(t, tx, "aggregate_buckets", ids, wins, deltas)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	t.Logf("seeded %d rows in %s", seriesN*windowsN, time.Since(seedStart).Round(time.Millisecond))

	sel := Selector{
		TenantID: tenantID,
		Start:    base,
		End:      base + int64(windowsN)*windowSecs,
		Signal:   SignalTraceOp,
	}

	// New path: one unordered streaming pass.
	newStart := time.Now()
	var newMerged *Sketch
	newRows := 0
	if err := e.mergeStoreSketches(context.Background(), sel, nil, func(sk *Sketch) {
		newRows++
		if newMerged == nil {
			newMerged = NewSketchAtScaleUnchecked(sk.Scale())
		}
		newMerged.Merge(sk)
	}); err != nil {
		t.Fatalf("mergeStoreSketches: %v", err)
	}
	newDur := time.Since(newStart)
	t.Logf("stream path: %d rows, count=%d, %s", newRows, newMerged.Count(), newDur.Round(time.Millisecond))

	// Old path: the paged, totally-ordered drain QueryDashboard used to run.
	pagedSel := sel
	pagedSel.SketchOnly = true
	oldStart := time.Now()
	var oldMerged *Sketch
	oldRows := 0
	for {
		page, err := store.ReadBuckets(context.Background(), pagedSel)
		if err != nil {
			t.Fatalf("ReadBuckets: %v", err)
		}
		for _, b := range page.Buckets {
			oldRows++
			if b.Delta.Sketch == nil {
				continue
			}
			if oldMerged == nil {
				oldMerged = NewSketchAtScaleUnchecked(b.Delta.Sketch.Scale())
			}
			oldMerged.Merge(b.Delta.Sketch)
		}
		if !page.Truncated {
			break
		}
		pagedSel.After = page.Next
	}
	oldDur := time.Since(oldStart)
	t.Logf("paged path: %d rows, count=%d, %s", oldRows, oldMerged.Count(), oldDur.Round(time.Millisecond))

	if newMerged.Count() != oldMerged.Count() {
		t.Fatalf("paths disagree: stream=%d paged=%d", newMerged.Count(), oldMerged.Count())
	}
	t.Logf("speedup: %.1fx", float64(oldDur)/float64(newDur))
}

// BenchmarkQueryDashboardSevenDay measures the production dashboard query
// over the same seven-day, 5-minute-window horizon used by the release gate.
// The signed-candidate gate owns the exact 6,000-series threshold; this focused
// benchmark keeps a stable 1,000-series local regression signal affordable.
func BenchmarkQueryDashboardSevenDay(b *testing.B) {
	const (
		seriesN  = 1000
		windowsN = 2016
	)
	now := mustTime(b, "2026-08-21T12:02:00Z")
	e := testEngine(b, now)
	store := newTestStoreAt(b, filepath.Join(b.TempDir(), "aggregate.db"), StoreConfig{})
	e.SetStore(store)

	tenantID, _ := e.TenantID("default")
	windowSecs := int64(WindowSize / time.Second)
	base := WindowStart(now) - int64(windowsN+2)*windowSecs
	series := make([]SeriesRow, 0, seriesN)
	deltas := make([]*AggregateDelta, 0, seriesN)
	for i := 0; i < seriesN; i++ {
		id := SeriesID(i + 1)
		series = append(series, SeriesRow{ID: id, Key: SeriesKey{
			TenantID:  tenantID,
			ServiceID: e.Cache().Intern(tenantID, KindService, fmt.Sprintf("svc-%03d", i%120)),
			NameID:    e.Cache().Intern(tenantID, KindOperation, fmt.Sprintf("op-%05d", i)),
			Signal:    SignalTraceOp,
			Variant:   SpanKindServer,
		}})
		d := &AggregateDelta{}
		d.ObserveSpan(float64(100+i%900), false, true)
		deltas = append(deltas, d)
	}

	tx, err := store.writer.Begin()
	if err != nil {
		b.Fatalf("begin seed tx: %v", err)
	}
	seedSeries(b, tx, series)
	ids := make([]SeriesID, seriesN)
	wins := make([]int64, seriesN)
	for w := 0; w < windowsN; w++ {
		for i := 0; i < seriesN; i++ {
			ids[i] = SeriesID(i + 1)
			wins[i] = base + int64(w)*windowSecs
		}
		insertRows(b, tx, "aggregate_buckets", ids, wins, deltas)
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit seed tx: %v", err)
	}

	q := Query{
		Tenant: "default",
		Start:  time.Unix(base, 0).UTC(),
		End:    time.Unix(base+int64(windowsN)*windowSecs, 0).UTC(),
	}
	b.ReportMetric(float64(seriesN*windowsN), "rows/query")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := e.QueryDashboard(context.Background(), q)
		if err != nil {
			b.Fatalf("QueryDashboard: %v", err)
		}
		if result.SpanCount != int64(seriesN*windowsN) {
			b.Fatalf("span count = %d, want %d", result.SpanCount, seriesN*windowsN)
		}
	}
}
