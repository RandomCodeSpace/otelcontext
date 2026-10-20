package aggregate

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"
)

// Preserve the parent scalar SQL, streaming scratch decoder and engine fanout
// for the diagnostic baseline. This wrapper never writes a database.
type originalRollupQueryStore struct {
	Store
	sqlite *SQLiteStore
	t      *testing.T
}

func (s originalRollupQueryStore) SumBuckets(_ context.Context, sel Selector, by GroupBy) ([]SumRow, error) {
	return originalRollupSums(s.t, s.sqlite, sel, by), nil
}
func (s originalRollupQueryStore) VisitSketches(ctx context.Context, sel Selector, visit func(uint32, *Sketch) error) error {
	return visitOriginalRollupSketches(ctx, s.sqlite, sel, visit)
}

// Opt-in, bounded diagnostic only. This is 200 services and 24 hours, not the
// parent 12M-row fixture or a constrained ingestion/latency acceptance run.
func TestManualRollupHistoryTiming(t *testing.T) {
	if os.Getenv("AGGREGATE_ROLLUP_TIMING") == "" {
		t.Skip("set AGGREGATE_ROLLUP_TIMING=1")
	}
	s := rollupTestStore(t)
	e := testEngine(t, time.Unix(rollupTestHour+25*3600, 0))
	e.SetStore(s)
	tenant, _ := e.TenantID("default")
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var series []SeriesRow
	var ids []SeriesID
	var windows []int64
	var deltas []*AggregateDelta
	for svc := 0; svc < 200; svc++ {
		service := e.Cache().Intern(tenant, KindService, fmt.Sprintf("svc-%03d", svc))
		for _, signal := range []Signal{SignalTraceOp, SignalServiceEdge, SignalLog} {
			key := SeriesKey{TenantID: tenant, ServiceID: service, Signal: signal}
			if signal == SignalServiceEdge {
				key.NameID = e.Cache().Intern(tenant, KindService, fmt.Sprintf("svc-%03d", (svc+1)%200))
			} else {
				kind, _ := NameKind(signal)
				key.NameID = e.Cache().Intern(tenant, kind, "operation")
			}
			id := SeriesID(len(series) + 1)
			series = append(series, SeriesRow{ID: id, Key: key})
			ids = append(ids, id)
			windows = append(windows, rollupTestHour)
			d := spanDelta(7, float64(100+svc))
			if signal == SignalLog {
				d = &AggregateDelta{Count: 7, LogCount: 7, ErrorCount: 1}
			}
			deltas = append(deltas, d)
		}
	}
	seedSeries(t, tx, series)
	insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
	for filled := 1; filled < 288; {
		n := min(filled, 288-filled)
		if _, err := tx.Exec(`INSERT INTO aggregate_buckets SELECT window_start+?,series_id,`+deltaColumnList+` FROM aggregate_buckets WHERE window_start<?`, filled*300, rollupTestHour+int64(n*300)); err != nil {
			t.Fatal(err)
		}
		filled += n
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	checkpoint := func() int64 {
		t.Helper()
		if _, err := s.writer.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(s.Path())
		if err != nil {
			t.Fatal(err)
		}
		return st.Size()
	}
	// Measure installing the extension on the populated v5 schema, including
	// verification/open costs. Only these owned temporary tables are removed.
	path, uuid := s.Path(), s.UUID()
	for _, event := range []string{"insert", "update", "delete"} {
		if _, err := s.writer.Exec(`DROP TRIGGER aggregate_rollup_` + event); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"aggregate_hourly", "aggregate_rollup_hours", "aggregate_rollup_meta"} {
		if _, err := s.writer.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opened := time.Now()
	s = newTestStoreAt(t, path, StoreConfig{})
	openTime := time.Since(opened)
	s.stopRollups()
	e.SetStore(s)
	if s.UUID() != uuid {
		t.Fatal("migration changed UUID")
	}
	t.Logf("populated_v5_extension_open=%s", openTime)
	beforeBytes := checkpoint()
	sel := Selector{TenantID: tenant, Start: rollupTestHour, End: rollupTestHour + 24*3600, Signal: SignalTraceOp}
	q := Query{Tenant: "default", Start: time.Unix(sel.Start, 0), End: time.Unix(sel.End, 0)}
	measure := func(label string, fn func()) {
		t.Helper()
		fn()
		var times []time.Duration
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := 0; i < 3; i++ {
			start := time.Now()
			fn()
			times = append(times, time.Since(start))
		}
		runtime.ReadMemStats(&after)
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		t.Logf("%s median=%s allocated_per_read=%d", label, times[1], (after.TotalAlloc-before.TotalAlloc)/3)
	}
	scalarOriginal := func() { _ = originalRollupSums(t, s, sel, GroupByService) }
	sketchOriginal := func() { _ = originalRollupSketches(t, s, sel) }
	scalarRolled := func() {
		if _, err := s.SumBuckets(context.Background(), sel, GroupByService); err != nil {
			t.Fatal(err)
		}
	}
	sketchRolled := func() {
		merged := map[uint32]*Sketch{}
		if err := s.VisitSketches(context.Background(), sel, func(id uint32, sk *Sketch) error {
			if merged[id] == nil {
				merged[id] = NewSketchAtScaleUnchecked(sk.Scale())
			}
			merged[id].Merge(sk)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	measure("original_scalar", scalarOriginal)
	measure("original_sketch", sketchOriginal)
	e.SetStore(originalRollupQueryStore{Store: s, sqlite: s, t: t})
	var originalDashboard *DashboardResult
	originalDashboard, err = e.QueryDashboard(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	originalTopology, err := e.QueryTopology(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	measure("original_dashboard", func() {
		if _, err := e.QueryDashboard(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	})
	measure("original_topology", func() {
		if _, err := e.QueryTopology(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	})
	e.SetStore(s)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	if err := s.discoverRollupHours(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.buildPendingRollups(context.Background(), time.Now(), 25*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	buildTime := time.Since(start)
	runtime.ReadMemStats(&after)
	afterBytes := checkpoint()
	var rowCount, hourCount int
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_hourly`).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_rollup_hours WHERE state=2`).Scan(&hourCount); err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture=172800_rows_200_services_24h rollup_rows=%d completed_hours=%d build=%s build_allocated=%d heap_after=%d before_bytes=%d after_bytes=%d extra_bytes=%d", rowCount, hourCount, buildTime, after.TotalAlloc-before.TotalAlloc, after.HeapAlloc, beforeBytes, afterBytes, afterBytes-beforeBytes)
	measure("rolled_scalar", scalarRolled)
	measure("rolled_sketch", sketchRolled)
	measure("rolled_dashboard", func() {
		got, err := e.QueryDashboard(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if got.SpanCount != originalDashboard.SpanCount || got.RequestCount != originalDashboard.RequestCount || got.TotalLogs != originalDashboard.TotalLogs || got.P99LatencyMicros != originalDashboard.P99LatencyMicros {
			t.Fatal("dashboard counts/percentile changed")
		}
	})
	measure("rolled_topology", func() {
		got, err := e.QueryTopology(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Nodes) != len(originalTopology.Nodes) || len(got.Edges) != len(originalTopology.Edges) {
			t.Fatal("topology cardinality changed")
		}
	})

	// Isolate trigger work from the identical finalizer on fresh bounded stores.
	// Each sample finalizes 600 series; no application or ingestion load runs.
	for _, triggers := range []bool{true, false} {
		writeStore := rollupTestStore(t)
		seed(t, writeStore, &GroupBatch{Series: series})
		if !triggers {
			for _, event := range []string{"insert", "update", "delete"} {
				if _, err := writeStore.writer.Exec(`DROP TRIGGER aggregate_rollup_` + event); err != nil {
					t.Fatal(err)
				}
			}
		}
		var durations []time.Duration
		for i := 0; i < 4; i++ {
			window := rollupTestHour + int64(i*300)
			batch := &GroupBatch{}
			for j, id := range ids {
				batch.Deltas = append(batch.Deltas, DeltaRow{SeriesID: id, WindowStart: window, Delta: deltas[j]})
			}
			seed(t, writeStore, batch)
			start := time.Now()
			if _, err := writeStore.FinalizeWindow(window); err != nil {
				t.Fatal(err)
			}
			if i > 0 {
				durations = append(durations, time.Since(start))
			}
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		t.Logf("finalize_600_series invalidation_triggers=%t median=%s", triggers, durations[1])
	}
	query, args := sumHistoryQuery(sel, GroupByService, true)
	t.Logf("rolled scalar plan:\n%s", explainHistoryQuery(t, s, query, args))
}
