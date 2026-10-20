package aggregate

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReviewRollupFilterParameterBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		n        int
		sketches bool
	}{
		{"sketches_7000", 7000, true}, {"scalars_12000", 12000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rollupTestStore(t)
			sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3600,
				ServiceIDs: []uint32{2, 20000}, Signals: []Signal{SignalTraceOp, SignalServiceEdge}}
			for i := 1; i <= tc.n; i++ {
				sel.SeriesIDs = append(sel.SeriesIDs, SeriesID(i))
			}
			if _, err := sel.Validate(); err != nil {
				t.Fatal(err)
			}
			seed(t, s, &GroupBatch{Series: []SeriesRow{
				{ID: 1, Key: SeriesKey{TenantID: 1, ServiceID: 2, NameID: 3, Signal: SignalTraceOp}},
				{ID: 2, Key: SeriesKey{TenantID: 9, ServiceID: 2, NameID: 3, Signal: SignalTraceOp}},
				{ID: 3, Key: SeriesKey{TenantID: 1, ServiceID: 4, NameID: 3, Signal: SignalTraceOp}},
				{ID: 4, Key: SeriesKey{TenantID: 1, ServiceID: 2, NameID: 3, Signal: SignalMetric}},
				{ID: SeriesID(tc.n + 1), Key: SeriesKey{TenantID: 1, ServiceID: 2, NameID: 4, Signal: SignalTraceOp}},
			}, Deltas: []DeltaRow{
				{SeriesID: 1, WindowStart: sel.Start, Delta: spanDelta(2, 100)},
				{SeriesID: 2, WindowStart: sel.Start, Delta: spanDelta(7, 100)},
				{SeriesID: 3, WindowStart: sel.Start, Delta: spanDelta(7, 100)},
				{SeriesID: 4, WindowStart: sel.Start, Delta: spanDelta(7, 100)},
				{SeriesID: SeriesID(tc.n + 1), WindowStart: sel.Start, Delta: spanDelta(7, 100)},
			}})
			if _, err := s.FinalizeWindow(sel.Start); err != nil {
				t.Fatal(err)
			}
			seed(t, s, &GroupBatch{Deltas: []DeltaRow{
				{SeriesID: 1, WindowStart: sel.Start + 300, Delta: spanDelta(3, 200)},
				{SeriesID: 1, WindowStart: sel.End, Delta: spanDelta(7, 100)},
			}})
			for _, state := range []string{"cold", "warm", "unsafe"} {
				if state == "warm" {
					finishRollups(t, s)
				}
				if state == "unsafe" {
					if !tc.sketches {
						continue
					}
					coarse := NewSketchAtScaleUnchecked(SketchDefaultScale - 1)
					coarse.ObserveN(200, 3)
					if _, err := s.writer.Exec(`UPDATE aggregate_delta_log SET sketch=? WHERE series_id=1 AND window_start=?`, coarse.AppendTo(nil), sel.Start+300); err != nil {
						t.Fatal(err)
					}
				}
				for _, singleSignal := range []bool{false, true} {
					if singleSignal {
						sel.Signal = SignalTraceOp
						sel.Signals = nil
					}
					if tc.sketches {
						want := originalRollupSketches(t, s, sel)
						got := map[uint32]*Sketch{}
						err := s.VisitSketches(context.Background(), sel, func(id uint32, sk *Sketch) error {
							if got[id] == nil {
								got[id] = NewSketch()
							}
							got[id].Merge(sk)
							return nil
						})
						if err != nil || !reflect.DeepEqual(want, got) || len(got) != 1 || got[2].Count() != 5 {
							t.Fatalf("state=%s single_signal=%v sketches=%v err=%v", state, singleSignal, got, err)
						}
					} else {
						want := originalRollupSums(t, s, sel, GroupByService)
						got, err := s.SumBuckets(context.Background(), sel, GroupByService)
						if err != nil || !reflect.DeepEqual(want, got) || len(got) != 1 || got[0].Count != 5 {
							t.Fatalf("state=%s single_signal=%v sums=%+v err=%v", state, singleSignal, got, err)
						}
					}
				}
				sel.Signal, sel.Signals = SignalUnspecified, []Signal{SignalTraceOp, SignalServiceEdge}
			}

		})
	}
}

func TestReviewRollupEngineSaturationParity(t *testing.T) {
	s := rollupTestStore(t)
	e := testEngine(t, time.Unix(rollupTestHour+4*3600, 0))
	tenant, _ := e.TenantID("default")
	key := SeriesKey{TenantID: tenant, ServiceID: e.Cache().Intern(tenant, KindService, "svc"), NameID: e.Cache().Intern(tenant, KindOperation, "op"), Signal: SignalTraceOp}
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	seedSeries(t, tx, []SeriesRow{{ID: 1, Key: key}})
	var ids []SeriesID
	var windows []int64
	var deltas []*AggregateDelta
	for h := 0; h < 3; h++ {
		for w := 0; w < 4; w++ {
			d := &AggregateDelta{Count: 500_000_000, DurationCount: 500_000_000, DurationSum: 50_000_000_000, DurationMin: 100, DurationMax: 100, Sketch: NewSketch()}
			d.Sketch.ObserveN(100, 500_000_000)
			ids = append(ids, 1)
			windows = append(windows, rollupTestHour+int64(h*3600+w*300))
			deltas = append(deltas, d)
		}
	}
	insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	q := Query{Tenant: "default", Start: time.Unix(rollupTestHour, 0), End: time.Unix(rollupTestHour+3*3600, 0)}
	e.SetStore(originalRollupQueryStore{Store: s, sqlite: s, t: t})
	want, err := e.QueryDashboard(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if want.SpanCount != 6_000_000_000 || want.LatencyProvenance.P99.Saturations != 2 {
		t.Fatalf("invalid reference fixture: %+v", want)
	}
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprint("warm=", warm), func(t *testing.T) {
			if warm {
				finishRollups(t, s)
			}
			e.SetStore(s)
			got, err := e.QueryDashboard(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("engine result changed: original=%+v rolled=%+v; provenance original=%+v rolled=%+v", want, got, want.LatencyProvenance.P99, got.LatencyProvenance.P99)
			}
		})
	}
	t.Run("complete_hour_chunks", func(t *testing.T) {
		// Four-hour bounds produce full-hour logical chunks, so the warm
		// path actually substitutes hourly sketches inside those groups.
		hourQuery := q
		hourQuery.End = hourQuery.End.Add(time.Hour)
		e.SetStore(originalRollupQueryStore{Store: s, sqlite: s, t: t})
		hourWant, err := e.QueryDashboard(context.Background(), hourQuery)
		if err != nil {
			t.Fatal(err)
		}
		e.SetStore(s)
		hourGot, err := e.QueryDashboard(context.Background(), hourQuery)
		if err != nil || !reflect.DeepEqual(hourWant, hourGot) {
			t.Fatalf("complete-hour engine parity: got=%+v want=%+v err=%v", hourGot, hourWant, err)
		}
	})
	t.Run("during_finalize", func(t *testing.T) {
		// Move the last hour to pending storage without changing its payload.
		move, err := s.writer.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer move.Rollback()
		if _, err := move.Exec(`INSERT INTO aggregate_delta_log SELECT * FROM aggregate_buckets WHERE window_start>=?`, rollupTestHour+7200); err != nil {
			t.Fatal(err)
		}
		if _, err := move.Exec(`DELETE FROM aggregate_buckets WHERE window_start>=?`, rollupTestHour+7200); err != nil {
			t.Fatal(err)
		}
		if err := move.Commit(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		var finalizeErr error
		go func() {
			defer close(done)
			for s.reader.Stats().InUse == 0 {
				if ctx.Err() != nil {
					finalizeErr = ctx.Err()
					return
				}
				time.Sleep(100 * time.Microsecond)
			}
			for _, window := range windows[8:] {
				if _, err := s.FinalizeWindow(window); err != nil {
					finalizeErr = err
					return
				}
			}
		}()
		defer func() { cancel(); <-done }()
		for i := 0; i < 8; i++ {
			got, err := e.QueryDashboard(ctx, q)
			if err != nil || !reflect.DeepEqual(want, got) {
				t.Fatalf("during finalize: got=%+v want=%+v err=%v", got, want, err)
			}
		}
		<-done
		if finalizeErr != nil {
			t.Fatal(finalizeErr)
		}
	})
	t.Run("snapshot_between_chunks", func(t *testing.T) {
		tx, err := s.reader.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		sel := Selector{TenantID: tenant, Start: q.Start.Unix(), End: q.End.Unix(), Signal: SignalTraceOp}
		merged := NewSketch()
		read := func(ctx context.Context, chunk Selector, visit func(uint32, *Sketch) error) error {
			return visitSQLiteSketches(ctx, tx, chunk, visit)
		}
		for i, chunk := range splitSelector(sel) {
			part, err := e.collectStoreSketches(context.Background(), read, chunk, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, sk := range part {
				merged.Merge(sk)
			}
			if i == 0 {
				if _, err := s.PurgeBefore(sel.End); err != nil {
					t.Fatal(err)
				}
				finishRollups(t, s)
			}
		}
		if !reflect.DeepEqual(PercentileFromSketch(merged), want.LatencyProvenance.P99) {
			t.Fatalf("chunk snapshot changed: got=%+v want=%+v", PercentileFromSketch(merged), want.LatencyProvenance.P99)
		}
	})

}

// queryHook wraps the same transaction used by production to inspect executed
// statements or commit a writer mutation between them.
type rollupQueryHook struct {
	sqliteQueryer
	before func(string, []any)
}

func (h rollupQueryHook) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	h.before(q, args)
	return h.sqliteQueryer.QueryContext(ctx, q, args...)
}

// Assert against the actual raw-row VM loop, not a blanket ban on sorters:
// service grouping may sort reduced series or fixed-hour summary rows, and
// the unchanged delta-log branch may sort its pending rows.
func assertRollupScalarPlan(t *testing.T, db *sql.DB, q string, args []any, by GroupBy, rolled bool) {
	t.Helper()
	if err := checkRollupScalarPlan(t, db, q, args, by, rolled); err != nil {
		t.Fatal(err)
	}
}

func checkRollupScalarPlan(t *testing.T, db *sql.DB, q string, args []any, by GroupBy, rolled bool) error {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&plan, "%d %d %s\n", id, parent, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()

	// Only the flattened hourly branch may group unique summary rows directly.
	// Its first three bindings are the identity sentinels, then the hour bounds.
	fixedHourly := rolled && by&(GroupByService|GroupByName|GroupBySignal) != 0 && by&GroupByWindow == 0
	if fixedHourly {
		if len(args) < 5 {
			return fmt.Errorf("missing hourly bounds")
		}
		start, startOK := args[3].(int64)
		end, endOK := args[4].(int64)
		if !startOK || !endOK || start != hourStart(start) || end-start != rollupHour {
			return fmt.Errorf("hourly grouping is not fixed to one complete hour")
		}
		if !strings.Contains(q, "WHERE t.window_start = ? AND t.window_start < ?") {
			return fmt.Errorf("hourly grouping is missing fixed-hour equality")
		}
		if !strings.Contains(plan.String(), "USING COVERING INDEX idx_aggregate_hourly_summary (window_start=?") {
			return fmt.Errorf("missing fixed-hour covering index:\n%s", &plan)
		}
	}

	roots := make(map[int]string)
	for _, name := range []string{"aggregate_buckets", "idx_aggregate_buckets_series_window", "aggregate_hourly", "idx_aggregate_hourly_summary"} {
		var root int
		if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name=?`, name).Scan(&root); err != nil {
			t.Fatal(err)
		}
		roots[root] = name
	}
	rows, err = db.Query("EXPLAIN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	type op struct {
		addr, p1, p2, p3, p5 int
		code                 string
		p4, comment          sql.NullString
	}
	var ops []op
	cursors := make(map[int]string)
	hourlyReads := 0
	for rows.Next() {
		var o op
		if err := rows.Scan(&o.addr, &o.code, &o.p1, &o.p2, &o.p3, &o.p4, &o.p5, &o.comment); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, o)
		if o.code == "OpenRead" && o.p3 == 0 && roots[o.p2] != "" {
			cursors[o.p1] = roots[o.p2]
			if roots[o.p2] == "aggregate_hourly" || roots[o.p2] == "idx_aggregate_hourly_summary" {
				hourlyReads++
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	// The flattened query has one hourly source. Its fixed-hour lookup must
	// not exempt a separate, wider hourly read from the reduction checks.
	if fixedHourly && hourlyReads != 1 {
		return fmt.Errorf("fixed-hour grouping requires exactly one hourly read; got %d", hourlyReads)
	}
	loops, hourlyLoops := 0, 0
	for _, o := range ops {
		if o.code != "Next" || cursors[o.p1] == "" {
			continue
		}
		loops++
		if fixedHourly && cursors[o.p1] == "idx_aggregate_hourly_summary" {
			hourlyLoops++
			continue
		}
		aggregates := false
		for _, body := range ops[o.p2:o.addr] {
			if body.code == "SorterInsert" {
				return fmt.Errorf("retained-row loop %d..%d (%s) sorts before reduction at %d:\n%s", o.p2, o.addr, cursors[o.p1], body.addr, &plan)
			}
			aggregates = aggregates || body.code == "AggStep"
		}
		if !aggregates {
			return fmt.Errorf("retained-row loop %d..%d does not reduce rows", o.p2, o.addr)
		}
	}
	if loops == 0 {
		return fmt.Errorf("no retained-row loop inspected")
	}
	if by == GroupByService {
		index := "idx_aggregate_buckets_series_window"
		if rolled {
			index = "idx_aggregate_hourly_summary"
		}
		if !strings.Contains(plan.String(), "USING COVERING INDEX "+index) {
			return fmt.Errorf("missing streaming covering index %s:\n%s", index, &plan)
		}
	}
	t.Logf("by=%d rolled=%v: %d retained-row loop(s), %d fixed-hour summary loop(s); remaining loops reduce without SorterInsert\n%s", by, rolled, loops, hourlyLoops, &plan)
	return nil
}

func TestReviewRollupScalarPlanRejectsUnboundedSorts(t *testing.T) {
	s := rollupTestStore(t)
	// EXPLAIN covers the seven-day query shape without constructing a large
	// fixture or executing its unreduced retained-row sort.
	badBuckets := `SELECT s.service_id, SUM(t.point_count)
	 FROM aggregate_buckets t INDEXED BY sqlite_autoindex_aggregate_buckets_1
	 CROSS JOIN aggregate_series s ON s.id=t.series_id
	 WHERE t.window_start>=? AND t.window_start<? AND s.tenant_id=?
	 GROUP BY s.service_id`
	err := checkRollupScalarPlan(t, s.reader, badBuckets,
		[]any{rollupTestHour, rollupTestHour + 7*24*3600, 1}, GroupByService, false)
	if err == nil || !strings.Contains(err.Error(), "(aggregate_buckets) sorts before reduction") {
		t.Fatalf("unreduced authoritative sort was not rejected: %v", err)
	}

	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3600}
	q, args := sumHistoryQuery(sel, GroupByService, true)
	join := "JOIN aggregate_series s ON s.id = t.series_id"
	wideJoin := fmt.Sprintf("CROSS JOIN aggregate_hourly wide INDEXED BY idx_aggregate_hourly_summary ON wide.series_id = t.series_id AND wide.window_start >= %d AND wide.window_start < %d %s", sel.Start, sel.Start+7*24*3600, join)
	extraHourly := strings.Replace(q, join, wideJoin, 1)
	if extraHourly == q {
		t.Fatal("negative query did not add the seven-day hourly source")
	}
	for _, tc := range []struct {
		name, query, want string
		end               int64
	}{
		{"extra_hourly", extraHourly, "requires exactly one hourly read", sel.End},
		{"wide_hourly", q, "not fixed to one complete hour", sel.End + 3600},
		{"hourly_range", strings.Replace(q, "t.window_start = ?", "t.window_start >= ?", 1), "missing fixed-hour equality", sel.End},
		{"hourly_primary", strings.Replace(q, "idx_aggregate_hourly_summary", "sqlite_autoindex_aggregate_hourly_1", 1), "missing fixed-hour covering index", sel.End},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bound := append([]any(nil), args...)
			bound[4] = tc.end
			err := checkRollupScalarPlan(t, s.reader, tc.query, bound, GroupByService, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe hourly grouping was not rejected: %v", err)
			}
		})
	}
}

func TestReviewRollupColdQueryPlan(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 8, 4)
	var tempStore int
	if err := s.reader.QueryRow("PRAGMA temp_store").Scan(&tempStore); err != nil || tempStore != 2 {
		t.Fatalf("temp_store=%d err=%v; test must exercise MEMORY", tempStore, err)
	}
	sel := Selector{TenantID: 1, Start: rollupTestHour + 300, End: rollupTestHour + 4*3600 - 300}
	for _, state := range []string{"cold", "warm", "partial", "invalidated"} {
		t.Run(state, func(t *testing.T) {
			switch state {
			case "warm":
				finishRollups(t, s)
			case "partial":
				if _, err := s.writer.Exec(`UPDATE aggregate_rollup_hours SET state=1 WHERE window_start=?`, rollupTestHour+3600); err != nil {
					t.Fatal(err)
				}
			case "invalidated":
				if _, err := s.writer.Exec(`UPDATE aggregate_buckets SET point_count=point_count+1 WHERE window_start=?`, rollupTestHour+7200); err != nil {
					t.Fatal(err)
				}
			}
			for _, by := range []GroupBy{0, GroupByService} {
				want := originalRollupSums(t, s, sel, by)
				tx, err := s.reader.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				queries, hourly := 0, 0
				hook := rollupQueryHook{sqliteQueryer: tx, before: func(q string, args []any) {
					if !strings.HasPrefix(q, "SELECT ") {
						return // hour markers, not a bucket scan
					}
					rolled := strings.Contains(q, "FROM aggregate_hourly")
					queries++
					if rolled {
						hourly++
						if strings.Contains(q, "FROM aggregate_buckets") {
							t.Fatal("warm scalar statement still scans retained buckets")
						}
					}
					assertRollupScalarPlan(t, s.reader, q, args, by, rolled)
				}}
				got, err := sumRollupSnapshot(context.Background(), hook, sel, by)
				if err != nil {
					t.Fatal(err)
				}
				tx.Rollback()
				sortRollupSums(got)
				sortRollupSums(want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("counts changed: got=%+v want=%+v", got, want)
				}
				wantQueries, wantHourly := 1, 0
				if state == "warm" {
					wantQueries, wantHourly = 4, 2
				} else if state == "partial" {
					wantQueries, wantHourly = 3, 1
				}
				if queries != wantQueries || hourly != wantHourly {
					t.Fatalf("queries=%d hourly=%d, want %d/%d", queries, hourly, wantQueries, wantHourly)
				}
			}
		})
	}
}

func TestRollupScalarSnapshotBetweenStatements(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 8, 3)
	finishRollups(t, s)
	window := rollupTestHour + 3600
	seed(t, s, &GroupBatch{Deltas: []DeltaRow{{SeriesID: 1, WindowStart: window, Delta: spanDelta(3, 100)}}})
	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3*3600}
	want := originalRollupSums(t, s, sel, GroupByService)
	tx, err := s.reader.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mutated := false
	hook := rollupQueryHook{sqliteQueryer: tx, before: func(q string, _ []any) {
		if mutated || !strings.HasPrefix(q, "SELECT ") {
			return
		}
		mutated = true // marker selection has already pinned the snapshot
		if _, err := s.FinalizeWindow(window); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PurgeBefore(rollupTestHour + 7200); err != nil {
			t.Fatal(err)
		}
		finishRollups(t, s)
	}}
	got, err := sumRollupSnapshot(context.Background(), hook, sel, GroupByService)
	if err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	sortRollupSums(want)
	sortRollupSums(got)
	if !mutated || !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot changed: got=%+v want=%+v mutated=%v", got, want, mutated)
	}
	after, err := s.SumBuckets(context.Background(), sel, GroupByService)
	if err != nil || reflect.DeepEqual(after, got) {
		t.Fatalf("new snapshot did not observe purge: after=%+v err=%v", after, err)
	}
}
