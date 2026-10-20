package aggregate

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// Adapted from the independent review's exact counterexample. Freeze each
// authoritative grouping before construction, including DurationCount == 0.
func checkSummaryArithmeticFallback(t *testing.T, values []float64, services, names []uint32, windows []int64, signal Signal) {
	t.Helper()
	s := rollupTestStore(t)
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var series []SeriesRow
	var ids []SeriesID
	var deltas []*AggregateDelta
	for i, value := range values {
		key := storeKey(names[i])
		key.ServiceID = services[i]
		key.Signal = signal
		series = append(series, SeriesRow{ID: SeriesID(i + 1), Key: key})
		ids = append(ids, SeriesID(i+1))
		deltas = append(deltas, &AggregateDelta{Count: 1, DurationSum: value})
	}
	seedSeries(t, tx, series)
	insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + rollupHour, Signal: signal}
	groups := []GroupBy{0, GroupByService, GroupByWindow, GroupByWindow | GroupByService}
	want := make(map[GroupBy][]SumRow)
	for _, by := range groups {
		want[by], err = s.SumBuckets(context.Background(), sel, by)
		if err != nil {
			t.Fatal(err)
		}
	}
	c := finishSummary(t, s, rollupTestHour, 1)
	var state int
	if err := s.reader.QueryRow(`SELECT state FROM aggregate_service_summary_hours WHERE window_start=?`, rollupTestHour).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != summaryArithmeticFallback {
		t.Fatalf("unproved arithmetic published: state=%d", state)
	}
	for _, by := range groups {
		got, err := s.SumBuckets(context.Background(), sel, by)
		if err != nil {
			t.Fatal(err)
		}
		assertTrafficSums(t, got, want[by])
		if _, used, err := readSummarySums(context.Background(), s.reader, sel, by); err != nil || used {
			t.Fatalf("unproved arithmetic used: by=%d used=%v err=%v", by, used, err)
		}
		t.Logf("authoritative fallback by=%d sums=%v", by, got)
	}
	// A rejected but finished source generation must not restart every worker pass.
	w := summaryWorker{sourceAfter: rollupTestHour, summaryAfter: rollupTestHour}
	active, err := s.summaryWorkerStep(context.Background(), time.Now(), &w, func(context.Context, int64, time.Duration) error { t.Fatal("unexpected source build"); return nil })
	if err != nil || active || w.build != nil {
		t.Fatal("rejected generation restarted", active, err)
	}
	if _, err := s.claimSummaryBuild(context.Background(), rollupTestHour); err != errRollupInvalidated {
		t.Fatal("reclaimed rejected source", err)
	}
	// A source change permits a new exact generation; state 4 is not permanent.
	if _, err := s.writer.Exec(`UPDATE aggregate_buckets SET duration_sum=1`); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	replacement := finishSummary(t, s, rollupTestHour, 1)
	if replacement.token == c.token {
		t.Fatal("replacement reused rejected token")
	}
	if ready, err := summaryReady(context.Background(), s.reader, rollupTestHour); err != nil || !ready {
		t.Fatal("safe replacement unavailable", ready, err)
	}
}

func TestReviewSummaryRegroupingByIdentityOrder(t *testing.T) {
	checkSummaryArithmeticFallback(t, []float64{1, 1, 1 << 53}, []uint32{2, 2, 2}, []uint32{101, 102, 100}, []int64{rollupTestHour, rollupTestHour, rollupTestHour}, SignalTraceOp)
}
func TestReviewSummaryDurationSumRegrouping(t *testing.T) {
	checkSummaryArithmeticFallback(t, []float64{1e12, .001, .001}, []uint32{2, 2, 2}, []uint32{100, 101, 102}, []int64{rollupTestHour, rollupTestHour, rollupTestHour}, SignalTraceOp)
}
func TestServiceSummaryArithmeticAcrossCells(t *testing.T) {
	for _, signal := range []Signal{SignalTraceOp, SignalLog, SignalServiceEdge} {
		t.Run(fmt.Sprint(signal), func(t *testing.T) {
			// Every service cell is individually <=2^52, yet regrouping services changes
			// 1+1+2^52+2^52 to 2^52+2^52+1+1. The hour-wide budget must reject it.
			checkSummaryArithmeticFallback(t, []float64{1, 1, 1 << 52, 1 << 52}, []uint32{4, 5, 2, 3}, []uint32{100, 101, 102, 103}, []int64{rollupTestHour, rollupTestHour, rollupTestHour, rollupTestHour}, signal)
		})
	}
}

func TestServiceSummaryArithmeticEligibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source float64
		deltas []float64
		used   bool
	}{
		{"ordinary", 100, []float64{1, 2}, true},
		{"both-budgets-exact", 1 << 52, []float64{1 << 52}, true},
		{"delta-over-budget", 0, []float64{1 << 52, 1}, false},
		{"delta-fractional-cancellation", 1, []float64{.25, .75}, false},
		{"delta-negative-cancellation", 1, []float64{-1, 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rollupTestStore(t)
			tx, err := s.writer.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			seedSeries(t, tx, []SeriesRow{{ID: 1, Key: storeKey(101)}, {ID: 2, Key: storeKey(100)}})
			insertRows(t, tx, "aggregate_buckets", []SeriesID{1}, []int64{rollupTestHour}, []*AggregateDelta{{Count: 1, DurationSum: tc.source}})
			var ids []SeriesID
			var windows []int64
			var deltas []*AggregateDelta
			for i, v := range tc.deltas {
				ids = append(ids, 2)
				windows = append(windows, rollupTestHour+int64(i)*300)
				deltas = append(deltas, &AggregateDelta{Count: 1, DurationSum: v})
			}
			insertRows(t, tx, "aggregate_delta_log", ids, windows, deltas)
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			finishRollups(t, s)
			sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + rollupHour, Signal: SignalTraceOp}
			groups := []GroupBy{0, GroupByService, GroupByWindow, GroupByService | GroupByWindow}
			want := make(map[GroupBy][]SumRow)
			for _, by := range groups {
				want[by], err = s.SumBuckets(context.Background(), sel, by)
				if err != nil {
					t.Fatal(err)
				}
			}
			finishSummary(t, s, rollupTestHour, 1)
			for _, by := range groups {
				got, used, err := readSummarySums(context.Background(), s.reader, sel, by)
				if err != nil || used != tc.used {
					t.Fatalf("by=%d used=%v want=%v err=%v", by, used, tc.used, err)
				}
				if used {
					assertTrafficSums(t, got, want[by])
				}
				got, err = s.SumBuckets(context.Background(), sel, by)
				if err != nil {
					t.Fatal(err)
				}
				assertTrafficSums(t, got, want[by])
			}
		})
	}
	t.Run("canonical-fractions-hidden-by-hourly-sum", func(t *testing.T) {
		s := rollupTestStore(t)
		tx, err := s.writer.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		seedSeries(t, tx, []SeriesRow{{ID: 1, Key: storeKey(100)}})
		insertRows(t, tx, "aggregate_buckets", []SeriesID{1, 1}, []int64{rollupTestHour, rollupTestHour + 300}, []*AggregateDelta{{Count: 1, DurationSum: .25}, {Count: 1, DurationSum: .75}})
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		finishRollups(t, s)
		finishSummary(t, s, rollupTestHour, 1)
		if ready, err := summaryReady(context.Background(), s.reader, rollupTestHour); err != nil || ready {
			t.Fatal("fractional windows certified by integral hourly sum", ready, err)
		}
	})
}

func TestServiceSummaryDeltaOverflowGrouping(t *testing.T) {
	s := rollupTestStore(t)
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	seedSeries(t, tx, []SeriesRow{{ID: 1, Key: storeKey(100)}})
	insertRows(t, tx, "aggregate_buckets", []SeriesID{1}, []int64{rollupTestHour}, []*AggregateDelta{{}})
	insertRows(t, tx, "aggregate_delta_log", []SeriesID{1, 1}, []int64{rollupTestHour, rollupTestHour + 300}, []*AggregateDelta{{Count: math.MaxInt64}, {Count: 1}})
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	sel := Selector{TenantID: 1, Signal: SignalTraceOp, Start: rollupTestHour, End: rollupTestHour + rollupHour}
	for _, by := range []GroupBy{0, GroupByService} {
		if _, err := s.SumBuckets(context.Background(), sel, by); err == nil || !strings.Contains(err.Error(), "integer overflow") {
			t.Fatal("authoritative overflow not exercised", by, err)
		}
	}
	finishSummary(t, s, rollupTestHour, 1)
	for _, by := range []GroupBy{0, GroupByService} {
		if _, used, err := readSummarySums(context.Background(), s.reader, sel, by); !used || err == nil || !strings.Contains(err.Error(), "integer overflow") {
			t.Fatal("accelerated overflow grouping changed", by, used, err)
		}
	}
	// GroupByWindow separates the two values, so it must succeed exactly.
	got, used, err := readSummarySums(context.Background(), s.reader, sel, GroupByWindow)
	if err != nil || !used {
		t.Fatal(used, err)
	}
	assertTrafficSums(t, got, originalRollupSums(t, s, sel, GroupByWindow))
}

type summaryReviewQueryer struct {
	sqliteQueryer
	query string
	args  []any
}

func (q *summaryReviewQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.query, q.args = query, args
	return q.sqliteQueryer.QueryContext(ctx, query, args...)
}

func TestServiceSummaryDeltaGuardPlan(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 8, 1)
	for _, services := range [][]uint32{nil, {2, 3}, {0}} {
		q := &summaryReviewQueryer{sqliteQueryer: s.reader}
		sel := Selector{TenantID: 1, Signal: SignalTraceOp, Start: rollupTestHour, End: rollupTestHour + rollupHour, ServiceIDs: services}
		if safe, err := summaryDeltaSumSafe(context.Background(), q, sel); err != nil || !safe {
			t.Fatal(safe, err)
		}
		plan, ops := summaryReviewPlan(t, s.reader, q.query, q.args)
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "SEARCH t USING PRIMARY KEY (window_start>? AND window_start<?)") || strings.Contains(joined, "SCAN aggregate_series") {
			t.Fatal("delta arithmetic guard lost bounded membership", joined)
		}
		keyed := false
		for _, x := range ops {
			if x.op == "NotFound" && x.p4 == "1" {
				keyed = true
				t.Logf("arithmetic guard membership: %+v", x)
			}
		}
		if !keyed {
			t.Fatal("delta arithmetic guard is not a keyed membership lookup")
		}
	}
}

type summaryPlanOp struct {
	addr, p1, p2, p3 int
	op, p4           string
}

func summaryReviewPlan(t *testing.T, db *sql.DB, q string, args []any) ([]string, []summaryPlanOp) {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var a, b, c int
		var d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, d)
		t.Log("EQP", d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	rows, err = db.Query("EXPLAIN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	var ops []summaryPlanOp
	for rows.Next() {
		var x summaryPlanOp
		var p5 int
		var p4, comment sql.NullString
		if err := rows.Scan(&x.addr, &x.op, &x.p1, &x.p2, &x.p3, &p4, &p5, &comment); err != nil {
			t.Fatal(err)
		}
		x.p4 = p4.String
		ops = append(ops, x)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	return plan, ops
}

func TestReviewSummaryDeltaMembershipPlan(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 8, 1)
	var seriesRoot int
	if err := s.reader.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name='aggregate_series'`).Scan(&seriesRoot); err != nil {
		t.Fatal(err)
	}
	for _, services := range [][]uint32{nil, {2, 3}, {0}} {
		for _, by := range []GroupBy{0, GroupByWindow, GroupByService, GroupByService | GroupByName | GroupBySignal} {
			t.Run(fmt.Sprintf("services=%v/by=%d", services, by), func(t *testing.T) {
				signal := SignalTraceOp
				if by&GroupByName != 0 {
					signal = SignalServiceEdge
				}
				sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + rollupHour, Signal: signal, ServiceIDs: services}
				query, args := summarySumQuery(sel, by, summaryResolution(sel, by))
				plan, ops := summaryReviewPlan(t, s.reader, query, args)
				joined := strings.Join(plan, "\n")
				if !strings.Contains(joined, "SEARCH s USING INTEGER PRIMARY KEY (rowid=?)") || strings.Contains(joined, "SCAN s\n") || strings.HasSuffix(joined, "SCAN s") {
					t.Fatal("delta identity join is not keyed", joined)
				}
				if !strings.Contains(joined, "SEARCH t USING PRIMARY KEY (window_start>? AND window_start<?)") {
					t.Fatal("delta range seek missing", joined)
				}
				cursors := map[int]bool{}
				keyed := false
				membership := false
				for _, x := range ops {
					if x.op == "OpenRead" && x.p2 == seriesRoot {
						cursors[x.p1] = true
					}
					if cursors[x.p1] {
						if x.op == "SeekRowid" {
							keyed = true
							t.Logf("keyed delta identity: VDBE %+v", x)
						}
						if x.op == "Rewind" || x.op == "Next" {
							t.Fatalf("delta join scans physical identities: %+v", x)
						}
					}
					if x.op == "NotFound" && x.p4 == "1" {
						membership = true
						t.Logf("keyed membership: VDBE %+v", x)
					}
				}
				if !keyed || !membership {
					t.Fatal("missing actual keyed identity/membership VDBE", keyed, membership)
				}
			})
		}
	}
}
