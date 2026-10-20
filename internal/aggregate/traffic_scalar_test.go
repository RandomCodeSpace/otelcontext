package aggregate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func trafficScalarFixture(t *testing.T) *SQLiteStore {
	t.Helper()
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 8, 3)
	seed(t, s, &GroupBatch{Deltas: []DeltaRow{
		{SeriesID: 1, WindowStart: rollupTestHour + 3600, Delta: spanDelta(3, 0.1)},
		{SeriesID: 4, WindowStart: rollupTestHour + 7200, Delta: spanDelta(2, 0.2)},
	}})
	if _, err := s.writer.Exec(`UPDATE aggregate_buckets SET point_count=?, error_count=?, request_count=?,
	 error_request_count=?, duration_count=?, duration_sum=0.1, log_count=? WHERE series_id=1 AND window_start=?`,
		int64(1<<53)+3, int64(1<<53)+5, int64(1<<53)+7, int64(1<<53)+9,
		int64(1<<53)+11, int64(1<<53)+13, rollupTestHour); err != nil {
		t.Fatal(err)
	}
	// The shared fixture already covers service and name overflow.
	if _, err := s.writer.Exec(`INSERT INTO aggregate_dict(id,tenant_id,kind,value) VALUES (900,1,?,?)`, KindDimTuple, []byte(OtherValue)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writer.Exec(`UPDATE aggregate_series SET dims_id=900 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	return s
}

// The candidate changes only the bucket index in each SQL statement. Restore
// the original primary scan and execute the whole selector as the SQL oracle.
func originalTrafficSums(t *testing.T, s *SQLiteStore, sel Selector, by GroupBy) []SumRow {
	t.Helper()
	q, args := sumBucketsQuery(sel, by)
	q = strings.Replace(q, "idx_aggregate_buckets_series_window", "sqlite_autoindex_aggregate_buckets_1", 1)
	rows, err := s.reader.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []SumRow
	for rows.Next() {
		var r SumRow
		if err := rows.Scan(&r.WindowStart, &r.ServiceID, &r.NameID, &r.Signal, &r.Count, &r.ErrorCount,
			&r.RequestCount, &r.ErrorRequestCount, &r.DurationCount, &r.DurationSum, &r.LogCount, &r.IdentityCount); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertTrafficSums(t *testing.T, got, want []SumRow) {
	t.Helper()
	sortRollupSums(got)
	sortRollupSums(want)
	if len(got) != len(want) {
		t.Fatalf("groups=%d want=%d", len(got), len(want))
	}
	for i := range got {
		a, b := got[i], want[i]
		if math.Abs(a.DurationSum-b.DurationSum) > 1e-9 {
			t.Fatalf("float parity: %+v != %+v", a, b)
		}
		a.DurationSum, b.DurationSum = 0, 0
		if a != b {
			t.Fatalf("exact parity: %+v != %+v", a, b)
		}
	}
}

func TestWindowScalarParity(t *testing.T) {
	s := trafficScalarFixture(t)
	base := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3*3600}
	cases := map[string]func(*Selector){
		"all":          func(*Selector) {},
		"partial":      func(s *Selector) { s.Start++; s.End -= 137 },
		"half_open":    func(s *Selector) { s.Start += 3600; s.End = s.Start + 300 },
		"empty_range":  func(s *Selector) { s.Start = s.End; s.End += 3600 },
		"empty_tenant": func(s *Selector) { s.TenantID = 99 },
		"tenant":       func(s *Selector) { s.TenantID = 9 },
		"signal":       func(s *Selector) { s.Signal = SignalTraceOp },
		"signals":      func(s *Selector) { s.Signals = []Signal{SignalTraceOp, SignalLog} },
		"services":     func(s *Selector) { s.ServiceIDs = []uint32{2, 3} },
		"no_service":   func(s *Selector) { s.ServiceIDs = []uint32{0} },
		"series":       func(s *Selector) { s.SeriesIDs = []SeriesID{1, 4, 7} },
		"no_series":    func(s *Selector) { s.SeriesIDs = []SeriesID{999} },
		"intersection": func(s *Selector) {
			s.SeriesIDs = []SeriesID{1, 4, 7}
			s.ServiceIDs = []uint32{2}
			s.Signal = SignalTraceOp
		},
		"sketch_only": func(s *Selector) { s.SketchOnly = true },
		"sketch_partial": func(s *Selector) {
			s.SketchOnly = true
			s.End = s.Start + 600
		},
	}
	for by := GroupByWindow; by < 16; by += 2 {
		for name, change := range cases {
			t.Run(fmt.Sprintf("by=%d/%s", by, name), func(t *testing.T) {
				sel := base
				change(&sel)
				want := originalTrafficSums(t, s, sel, by)
				got, err := s.SumBuckets(context.Background(), sel, by)
				if err != nil {
					t.Fatal(err)
				}
				assertTrafficSums(t, got, want)
			})
		}
	}
	sel := base
	sel.End, sel.SeriesIDs = sel.Start+300, []SeriesID{1}
	got, err := s.SumBuckets(context.Background(), sel, GroupByWindow)
	want := []SumRow{{WindowStart: sel.Start, Count: 1<<53 + 3, ErrorCount: 1<<53 + 5,
		RequestCount: 1<<53 + 7, ErrorRequestCount: 1<<53 + 9, DurationCount: 1<<53 + 11,
		DurationSum: 0.1, LogCount: 1<<53 + 13, IdentityCount: 1<<53 + 3}}
	if err != nil {
		t.Fatal(err)
	}
	assertTrafficSums(t, got, want)
	for _, invalid := range []Selector{{}, {Start: 600, End: 300}, {Start: 1, End: MaxReadWindowSpan + 2},
		{Start: 300, End: 600, Signal: SignalTraceOp, Signals: []Signal{SignalLog}}} {
		if got, err := s.SumBuckets(context.Background(), invalid, GroupByWindow); !errors.Is(err, ErrSelectorUnbounded) || got != nil {
			t.Fatalf("invalid selector returned %v, %v", got, err)
		}
	}
}

func assertWindowScalarPlan(t *testing.T, s *SQLiteStore, q string, args []any) {
	t.Helper()
	plan := explainHistoryQuery(t, s, q, args)
	if !strings.Contains(plan, "USING COVERING INDEX idx_aggregate_buckets_series_window (series_id=? AND window_start>? AND window_start<?)") {
		t.Fatalf("missing covering range: %s", plan)
	}
	var root int
	if err := s.reader.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name='aggregate_buckets'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	rows, err := s.reader.Query("EXPLAIN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var code string
		var p4, comment sql.NullString
		if err := rows.Scan(&addr, &code, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			t.Fatal(err)
		}
		if code == "OpenRead" && p3 == 0 && p2 == root {
			t.Fatal("scalar scan opens bucket payload table")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowScalarPlansAndRanges(t *testing.T) {
	s := trafficScalarFixture(t)
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			if err := s.Analyze(); err != nil {
				t.Fatal(err)
			}
		}
		for by := GroupByWindow; by < 16; by += 2 {
			sel := Selector{TenantID: 1, Start: rollupTestHour + 1, End: rollupTestHour + 3600}
			q, args := sumBucketsQuery(sel, by)
			assertWindowScalarPlan(t, s, q, args)
			for _, change := range []func(*Selector){
				func(s *Selector) { s.End++ },
				func(s *Selector) { s.End += 7 * 24 * 3600 },
				func(s *Selector) { s.SketchOnly = true },
			} {
				guard := sel
				change(&guard)
				q, _ := sumBucketsQuery(guard, by)
				if !strings.Contains(q, "INDEXED BY sqlite_autoindex_aggregate_buckets_1") {
					t.Fatal("wide/sketch query bypassed primary guard")
				}
			}
		}
	}
	for _, bounds := range [][2]int64{
		{rollupTestHour, rollupTestHour + 7*24*3600},
		{rollupTestHour + 3599, rollupTestHour + 7201},
		{1, MaxReadWindowSpan + 1},
		{math.MaxInt64 - MaxReadWindowSpan, math.MaxInt64},
	} {
		t.Run(fmt.Sprint(bounds), func(t *testing.T) {
			sel := Selector{TenantID: 1, Start: bounds[0], End: bounds[1],
				SeriesIDs: []SeriesID{1, 2, 7}, ServiceIDs: []uint32{3, 4}, Signals: []Signal{SignalTraceOp, SignalLog}}
			_, fullArgs := sumBucketsQuery(sel, GroupByWindow)
			tx, err := s.reader.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			next, queries := sel.Start, 0
			hook := rollupQueryHook{sqliteQueryer: tx, before: func(q string, args []any) {
				if len(args) != len(fullArgs) || strings.Count(q, "?") != len(args) {
					t.Fatal("selector bindings multiplied across chunks")
				}
				start, end := args[3].(int64), args[4].(int64)
				branch := len(args) / 2
				if !reflect.DeepEqual(args[:branch], args[branch:]) {
					t.Fatal("bucket/delta filters differ")
				}
				if start != next || end <= start || end > sel.End || end-hourStart(start) > rollupHour {
					t.Fatalf("unbounded/noncontiguous [%d,%d) after %d", start, end, next)
				}
				if !strings.Contains(q, "INDEXED BY idx_aggregate_buckets_series_window") {
					t.Fatal("chunk lost covering index")
				}
				next = end
				queries++
			}}
			if _, err := sumWindowSnapshot(context.Background(), hook, sel, GroupByWindow); err != nil {
				t.Fatal(err)
			}
			if next != sel.End || queries != int((sel.End-1)/rollupHour-sel.Start/rollupHour+1) {
				t.Fatalf("incomplete ranges: end=%d queries=%d", next, queries)
			}
		})
	}
}

func TestWindowScalarSnapshotBetweenChunks(t *testing.T) {
	s := trafficScalarFixture(t)
	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3*3600}
	by := GroupByWindow | GroupByService | GroupByName | GroupBySignal
	want := originalTrafficSums(t, s, sel, by)
	tx, err := s.reader.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	queries := 0
	hook := rollupQueryHook{sqliteQueryer: tx, before: func(string, []any) {
		queries++
		if queries != 2 {
			return
		}
		done := make(chan error, 1)
		go func() {
			if _, err := s.FinalizeWindow(sel.Start + 3600); err != nil {
				done <- err
				return
			}
			_, err := s.PurgeBefore(sel.Start + 7200)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent finalization/purge blocked")
		}
	}}
	got, err := sumWindowSnapshot(context.Background(), hook, sel, by)
	if err != nil {
		t.Fatal(err)
	}
	assertTrafficSums(t, got, want)
	if queries != 3 {
		t.Fatalf("queries=%d want=3", queries)
	}
	tx.Rollback()
	after, err := s.SumBuckets(context.Background(), sel, by)
	if err != nil || len(after) >= len(got) {
		t.Fatalf("new snapshot missed purge: rows=%d old=%d err=%v", len(after), len(got), err)
	}
}

func TestWindowScalarCancellationAndFailure(t *testing.T) {
	s := trafficScalarFixture(t)
	s.reader.SetMaxOpenConns(1)
	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3*3600}
	for _, cancelRead := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel=%v", cancelRead), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			queries := 0
			got, err := func() ([]SumRow, error) {
				tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
				if err != nil {
					return nil, err
				}
				defer tx.Rollback()
				hook := rollupQueryHook{sqliteQueryer: tx, before: func(string, []any) {
					queries++
					if queries == 2 {
						if cancelRead {
							cancel()
						} else {
							tx.Rollback()
						}
					}
				}}
				return sumWindowSnapshot(ctx, hook, sel, GroupByWindow)
			}()
			want := sql.ErrTxDone
			if cancelRead {
				want = context.Canceled
			}
			if got != nil || !errors.Is(err, want) || queries != 2 {
				t.Fatalf("partial success/further chunks: rows=%v err=%v queries=%d", got, err, queries)
			}
			pingCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := s.reader.PingContext(pingCtx); err != nil || s.reader.Stats().InUse != 0 {
				t.Fatalf("reader not released: %v %+v", err, s.reader.Stats())
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := s.SumBuckets(ctx, sel, GroupByWindow); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled public call: %v %v", got, err)
	}
}
