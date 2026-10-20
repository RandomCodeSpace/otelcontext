package aggregate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSketchSnapshotSurvivesConcurrentFinalization(t *testing.T) {
	s := newTestStoreAt(t, filepath.Join(t.TempDir(), "aggregate.db"), StoreConfig{})
	const start = int64(3_000_000)
	key := SeriesKey{TenantID: 1, ServiceID: 2, NameID: 3, Signal: SignalTraceOp}
	d := &AggregateDelta{}
	d.ObserveSpan(100, false, true)
	seed(t, s, &GroupBatch{Series: []SeriesRow{{ID: 1, Key: key}}, Deltas: []DeltaRow{{SeriesID: 1, WindowStart: start, Delta: d}}})
	if _, err := s.FinalizeWindow(start); err != nil {
		t.Fatal(err)
	}
	seed(t, s, &GroupBatch{Deltas: []DeltaRow{{SeriesID: 1, WindowStart: start + 300, Delta: d}}})
	var count uint64
	err := s.VisitSketches(context.Background(), Selector{TenantID: 1, Start: start, End: start + 600}, func(_ uint32, sk *Sketch) error {
		if count == 0 {
			if _, err := s.FinalizeWindow(start + 300); err != nil {
				return err
			}
		}
		count += sk.Count()
		return nil
	})
	if err != nil || count != 2 {
		t.Fatalf("count=%d err=%v; finalization must not hide the second sketch", count, err)
	}
}

func TestServiceFilterSkipsUnrelatedSketchDecoding(t *testing.T) {
	s := newTestStoreAt(t, filepath.Join(t.TempDir(), "aggregate.db"), StoreConfig{})
	const start = int64(3_000_000)
	d := &AggregateDelta{}
	d.ObserveSpan(100, false, true)
	seed(t, s, &GroupBatch{Series: []SeriesRow{
		{ID: 1, Key: SeriesKey{TenantID: 1, ServiceID: 2, NameID: 3, Signal: SignalTraceOp}},
		{ID: 2, Key: SeriesKey{TenantID: 1, ServiceID: 4, NameID: 3, Signal: SignalTraceOp}},
		{ID: 3, Key: SeriesKey{TenantID: 9, ServiceID: 2, NameID: 3, Signal: SignalTraceOp}},
	}, Deltas: []DeltaRow{
		{SeriesID: 1, WindowStart: start, Delta: d}, {SeriesID: 2, WindowStart: start, Delta: d}, {SeriesID: 3, WindowStart: start, Delta: d},
	}})
	// A bad unrelated sketch proves filtering happens before decoding.
	if _, err := s.writer.Exec(`UPDATE aggregate_delta_log SET sketch = ? WHERE series_id IN (2,3)`, []byte{1}); err != nil {
		t.Fatal(err)
	}
	sel := Selector{TenantID: 1, Start: start, End: start + 300, Signal: SignalTraceOp, ServiceIDs: []uint32{2}}
	var count uint64
	if err := s.VisitSketches(context.Background(), sel, func(_ uint32, sk *Sketch) error { count += sk.Count(); return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count=%d want=1", count)
	}
	sums, err := s.SumBuckets(context.Background(), sel, GroupByService)
	if err != nil || len(sums) != 1 || sums[0].Count != 1 {
		t.Fatalf("sums=%+v err=%v", sums, err)
	}
	page, err := s.ReadBuckets(context.Background(), sel)
	if err != nil || len(page.Buckets) != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestHistorySummaryIndexUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	s := newTestStoreAt(t, path, StoreConfig{})
	// Model the current schema before this additive index existed.
	if _, err := s.writer.Exec(`DROP INDEX idx_aggregate_buckets_series_window`); err != nil {
		t.Fatal(err)
	}
	const start = int64(3_000_000)
	d := &AggregateDelta{}
	d.ObserveSpan(100, false, true)
	seed(t, s, &GroupBatch{Series: []SeriesRow{{ID: 1, Key: SeriesKey{TenantID: 1, ServiceID: 2, NameID: 3, Signal: SignalTraceOp}}},
		Deltas: []DeltaRow{{SeriesID: 1, WindowStart: start, Delta: d}}})
	if _, err := s.FinalizeWindow(start); err != nil {
		t.Fatal(err)
	}
	uuid := s.UUID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// A second open also proves the compatible upgrade is idempotent.
	for i := 0; i < 2; i++ {
		s = newTestStoreAt(t, path, StoreConfig{})
		if s.UUID() != uuid {
			t.Fatal("index upgrade changed store identity")
		}
		sel := Selector{TenantID: 1, Start: start, End: start + 300, Signal: SignalTraceOp}
		sums, err := s.SumBuckets(context.Background(), sel, GroupByService)
		if err != nil || len(sums) != 1 || sums[0].Count != 1 {
			t.Fatalf("reopen=%d sums=%+v err=%v", i, sums, err)
		}
		query, args := sumBucketsQuery(sel, GroupByService)
		plan := explainHistoryQuery(t, s, query, args)
		if !strings.Contains(plan, "COVERING INDEX idx_aggregate_buckets_series_window") {
			t.Fatalf("summary lost its covering scan:\n%s", plan)
		}
		var sb strings.Builder
		sel.SketchOnly = true
		args = appendBucketUnion(&sb, sel, `s.service_id AS service_id, t.sketch AS sketch`, true, nil)
		plan = explainHistoryQuery(t, s, sb.String(), args)
		if strings.Contains(plan, "idx_aggregate_buckets_series_window") || !strings.Contains(plan, "USING PRIMARY KEY") {
			t.Fatalf("sketch read lost its window scan:\n%s", plan)
		}
		var count uint64
		if err := s.VisitSketches(context.Background(), sel, func(_ uint32, sk *Sketch) error { count += sk.Count(); return nil }); err != nil || count != 1 {
			t.Fatalf("reopen=%d samples=%d err=%v", i, count, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func explainHistoryQuery(t *testing.T, s *SQLiteStore, query string, args []any) string {
	t.Helper()
	rows, err := s.reader.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

// This fixture retains every five-minute window for seven days. Sketches are
// copied across windows, so this measures read work and exact counts rather
// than forecasting the distribution of production latencies.
func historyQueryFixture(t *testing.T) (*Engine, *SQLiteStore, Query) {
	t.Helper()
	const seriesN, windowsN = 1000, 2016
	now := time.Unix(3_000_000, 0).UTC()
	e := testEngine(t, now)
	path := os.Getenv("AGGREGATE_QUERY_BENCH_PATH")
	if path == "" {
		path = filepath.Join(t.TempDir(), "aggregate.db")
	}
	s := newTestStoreAt(t, path, StoreConfig{})
	e.SetStore(s)
	tenantID, _ := e.TenantID("default")
	base := WindowStart(now) - int64((windowsN+2)*300)
	var series []SeriesRow
	var ids []SeriesID
	var windows []int64
	var deltas []*AggregateDelta
	for i := 0; i < seriesN; i++ {
		key := SeriesKey{TenantID: tenantID, ServiceID: e.Cache().Intern(tenantID, KindService, fmt.Sprintf("svc-%03d", i%200)), NameID: e.Cache().Intern(tenantID, KindOperation, fmt.Sprintf("op-%04d", i)), Signal: SignalTraceOp}
		id := SeriesID(i + 1)
		series = append(series, SeriesRow{ID: id, Key: key})
		ids = append(ids, id)
		windows = append(windows, base)
		d := &AggregateDelta{}
		d.ObserveSpan(float64(100+i%900), i%7 == 0, true)
		deltas = append(deltas, d)
	}
	var rows int
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_buckets`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		tx, err := s.writer.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		seedSeries(t, tx, series)
		insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
		// Copy the seeded block in powers of two. No per-bucket statement
		// preparation is included in the query measurement.
		for filled := 1; filled < windowsN; {
			n := min(filled, windowsN-filled)
			_, err := tx.Exec(`INSERT INTO aggregate_buckets SELECT window_start + ?, series_id, `+deltaColumnList+` FROM aggregate_buckets WHERE window_start < ?`, filled*300, base+int64(n*300))
			if err != nil {
				t.Fatal(err)
			}
			filled += n
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	} else if rows != seriesN*windowsN {
		t.Fatalf("unexpected fixture row count %d", rows)
	}
	return e, s, Query{Tenant: "default", Start: time.Unix(base, 0).UTC(), End: time.Unix(base+windowsN*300, 0).UTC()}
}

type unfilteredHistoryStore struct{ Store }

func (s unfilteredHistoryStore) SumBuckets(ctx context.Context, sel Selector, by GroupBy) ([]SumRow, error) {
	sel.ServiceIDs = nil
	return s.Store.SumBuckets(ctx, sel, by)
}
func (s unfilteredHistoryStore) VisitSketches(ctx context.Context, sel Selector, visit func(uint32, *Sketch) error) error {
	sel.ServiceIDs = nil
	return s.Store.VisitSketches(ctx, sel, visit)
}

func TestManualHistoryQueryPlan(t *testing.T) {
	if os.Getenv("AGGREGATE_QUERY_TIMING") == "" {
		t.Skip("set AGGREGATE_QUERY_TIMING=1")
	}
	e, s, q := historyQueryFixture(t)
	tenantID, _ := e.TenantID(q.Tenant)
	if os.Getenv("AGGREGATE_QUERY_PARTS") != "" {
		sel := Selector{TenantID: tenantID, Start: q.Start.Unix(), End: q.End.Unix(), Signal: SignalTraceOp}
		start := time.Now()
		sums, err := e.sumStore(context.Background(), sel, GroupByService)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("scalar duration=%s groups=%d", time.Since(start), len(sums))
		start = time.Now()
		var samples uint64
		if err := e.visitStoreSketches(context.Background(), sel, nil, func(_ uint32, sk *Sketch) { samples += sk.Count() }); err != nil {
			t.Fatal(err)
		}
		t.Logf("sketch duration=%s samples=%d", time.Since(start), samples)
		var sb strings.Builder
		sel = splitSelector(sel)[0]
		sel.SketchOnly = true
		sb.WriteString(`SELECT service_id, sketch FROM (`)
		args := appendBucketUnion(&sb, sel, `s.service_id AS service_id, t.sketch AS sketch`, true, nil)
		sb.WriteString(`)`)
		rows, err := s.reader.Query(`EXPLAIN QUERY PLAN `+sb.String(), args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			t.Log("sketch plan:", detail)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	sel := splitSelector(Selector{TenantID: tenantID, Start: q.Start.Unix(), End: q.End.Unix(), Signals: []Signal{SignalTraceOp, SignalLog}})[0]
	query, args := sumBucketsQuery(sel, GroupByService|GroupBySignal)
	rows, err := s.reader.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("current plan:\n%s", strings.Join(plan, "\n"))
	for _, filtered := range []bool{false, true} {
		if filtered {
			e.SetStore(s)
		} else {
			e.SetStore(unfilteredHistoryStore{s})
		}
		q.Services = []string{"svc-000"}
		start := time.Now()
		result, err := e.QueryDashboard(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if result.SpanCount != 5*2016 {
			t.Fatalf("filtered count=%d", result.SpanCount)
		}
		t.Logf("SQL service filter=%v rows=2016000 spans=%d dashboard=%s", filtered, result.SpanCount, time.Since(start))
	}
	e.SetStore(s)
	q.Services = nil
	measure := func(label string) {
		t.Helper()
		start := time.Now()
		dash, err := e.QueryDashboard(context.Background(), q)
		if err != nil || dash.SpanCount != 2016000 {
			t.Fatalf("dashboard=%+v err=%v", dash, err)
		}
		t.Logf("%s dashboard=%s", label, time.Since(start))
		start = time.Now()
		topo, err := e.QueryTopology(context.Background(), q)
		if err != nil || len(topo.Nodes) != 200 {
			t.Fatalf("topology nodes=%d err=%v", len(topo.Nodes), err)
		}
		t.Logf("%s topology=%s", label, time.Since(start))
	}
	measure("indexed history")
}
