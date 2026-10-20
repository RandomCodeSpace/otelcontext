package aggregate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const rollupTestHour = int64(3_600_000)

func rollupTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s := newTestStore(t)
	s.stopRollups()
	return s
}

func populateRollupFixture(t *testing.T, s *SQLiteStore, seriesN, hours int) {
	t.Helper()
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var series []SeriesRow
	var ids []SeriesID
	var windows []int64
	var deltas []*AggregateDelta
	for i := 1; i <= seriesN; i++ {
		key := storeKey(uint32(i + 100))
		key.ServiceID = uint32(2 + i%4)
		if i%7 == 0 {
			key.TenantID = 9
		}
		key.Signal = []Signal{SignalTraceOp, SignalServiceEdge, SignalMetric, SignalLog}[i%4]
		series = append(series, SeriesRow{ID: SeriesID(i), Key: key})
		for w := 0; w < hours*12; w++ {
			if i%5 == 0 && w%3 == 0 {
				continue
			} // sparse series
			d := spanDelta(1+w%3, float64(100+i+w))
			if i%4 == 2 {
				d.GaugeCount = 2
				d.GaugeSum = float64(w + 10)
				d.GaugeMin = -float64(w)
				d.GaugeMax = float64(w + 10)
				d.GaugeLast = float64(w)
				d.GaugeLastTime = time.Unix(rollupTestHour+int64(w*300), 0)
				d.CounterDelta = float64(w)
				d.ResetCount = 1
				d.HistogramCount = d.DurationCount
				d.HistogramSum = d.DurationSum
				d.HistogramMin = d.DurationMin
				d.HistogramMax = d.DurationMax
				d.HistogramFlags = HistHasMin | HistHasMax | HistUnboundedTail
				d.HistogramSourceError = 0.25
				d.HistogramTailBound = 1000 - float64(w)
				d.HistogramTailCount = 2
			}
			if i%4 == 3 {
				d.Sketch = nil
				d.DurationCount = 0
				d.LogCount = d.Count
			}
			d.FirstTimestamp = time.Unix(rollupTestHour+int64(w*300), 0)
			d.LastTimestamp = d.FirstTimestamp.Add(time.Second)
			ids = append(ids, SeriesID(i))
			windows = append(windows, rollupTestHour+int64(w*300))
			deltas = append(deltas, d)
		}
	}
	seedSeries(t, tx, series)
	insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
	// Service and name overflow must survive every grouping and filter.
	if _, err := tx.Exec(`INSERT INTO aggregate_dict(id,tenant_id,kind,value) VALUES (2,1,?,?), (105,1,?,?)`, KindService, []byte(OtherValue), KindOperation, []byte(OtherValue)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func finishRollups(t *testing.T, s *SQLiteStore) {
	t.Helper()
	if err := s.discoverRollupHours(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.buildPendingRollups(context.Background(), time.Now(), 0); err != nil {
		t.Fatal(err)
	}
}

func originalRollupSums(t *testing.T, s *SQLiteStore, sel Selector, by GroupBy) []SumRow {
	t.Helper()
	query, args := sumBucketsQuery(sel, by)
	rows, err := s.reader.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []SumRow
	for rows.Next() {
		var r SumRow
		if err := rows.Scan(&r.WindowStart, &r.ServiceID, &r.NameID, &r.Signal, &r.Count, &r.ErrorCount, &r.RequestCount, &r.ErrorRequestCount, &r.DurationCount, &r.DurationSum, &r.LogCount, &r.IdentityCount); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sortRollupSums(rows []SumRow) {
	sort.Slice(rows, func(i, j int) bool {
		return fmt.Sprint(rows[i].WindowStart, rows[i].ServiceID, rows[i].NameID, rows[i].Signal) < fmt.Sprint(rows[j].WindowStart, rows[j].ServiceID, rows[j].NameID, rows[j].Signal)
	})
}

func visitOriginalRollupSketches(ctx context.Context, s *SQLiteStore, sel Selector, visit func(uint32, *Sketch) error) error {
	var sb strings.Builder
	sel.SketchOnly = true
	args := appendBucketUnion(&sb, sel, `s.service_id,t.sketch`, true, nil)
	rows, err := s.reader.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var scratch Sketch
	var blob sql.RawBytes
	for rows.Next() {
		var src int
		var id uint32
		if err := rows.Scan(&src, &id, &blob); err != nil {
			return err
		}
		if err := DecodeSketchInto(&scratch, blob); err != nil {
			return err
		}
		if err := visit(id, &scratch); err != nil {
			return err
		}
	}
	return rows.Err()
}

func originalRollupSketches(t *testing.T, s *SQLiteStore, sel Selector) map[uint32]*Sketch {
	t.Helper()
	out := map[uint32]*Sketch{}
	err := visitOriginalRollupSketches(context.Background(), s, sel, func(id uint32, sk *Sketch) error {
		if out[id] == nil {
			out[id] = NewSketchAtScaleUnchecked(sk.Scale())
		}
		out[id].Merge(sk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertRollupParity(t *testing.T, s *SQLiteStore, sel Selector) {
	t.Helper()
	for _, by := range []GroupBy{0, GroupByService, GroupByName, GroupBySignal, GroupByService | GroupByName | GroupBySignal, GroupByWindow, GroupByWindow | GroupByService} {
		want := originalRollupSums(t, s, sel, by)
		got, err := s.SumBuckets(context.Background(), sel, by)
		if err != nil {
			t.Fatal(err)
		}
		sortRollupSums(want)
		sortRollupSums(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("by=%d sel=%+v\ngot=%+v\nwant=%+v", by, sel, got, want)
		}
	}
	want := originalRollupSketches(t, s, sel)
	got := map[uint32]*Sketch{}
	err := s.VisitSketches(context.Background(), sel, func(id uint32, sk *Sketch) error {
		if got[id] == nil {
			got[id] = NewSketchAtScaleUnchecked(sk.Scale())
		}
		got[id].Merge(sk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("sketch groups: got %d want %d", len(got), len(want))
	}
	for id, expected := range want {
		if !reflect.DeepEqual(got[id], expected) {
			t.Fatalf("service %d sketch mismatch: got count=%d p99=%g accuracy=%+v, want count=%d p99=%g accuracy=%+v", id, got[id].Count(), got[id].Quantile(.99), AccuracyFromSketch(got[id]), expected.Count(), expected.Quantile(.99), AccuracyFromSketch(expected))
		}
	}
}

func TestRollupMixedRangesFiltersAndPayload(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 16, 4)
	finishRollups(t, s)
	// A missing hour uses exactly the same authoritative fallback as an
	// invalidated build, while adjacent hours still use their rollups.
	if _, err := s.writer.Exec(`DELETE FROM aggregate_rollup_hours WHERE window_start=?`, rollupTestHour+3600); err != nil {
		t.Fatal(err)
	}
	selectors := []Selector{
		{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 4*3600},
		{TenantID: 1, Start: rollupTestHour + 301, End: rollupTestHour + 4*3600 - 299},
		{TenantID: 1, Start: rollupTestHour + 3600, End: rollupTestHour + 7200},
		{TenantID: 9, Start: rollupTestHour, End: rollupTestHour + 4*3600},
		{TenantID: 99, Start: rollupTestHour, End: rollupTestHour + 4*3600},
	}
	for _, base := range selectors {
		assertRollupParity(t, s, base)
		only := base
		only.SketchOnly = true
		assertRollupParity(t, s, only)
		for _, sig := range []Signal{SignalTraceOp, SignalServiceEdge, SignalMetric, SignalLog} {
			sel := base
			sel.Signal = sig
			assertRollupParity(t, s, sel)
		}
		sel := base
		sel.Signals = []Signal{SignalTraceOp, SignalLog}
		sel.ServiceIDs = []uint32{2, 3}
		assertRollupParity(t, s, sel)
		sel = base
		sel.SeriesIDs = []SeriesID{1, 3, 5}
		assertRollupParity(t, s, sel)
		sel = base
		sel.ServiceIDs = []uint32{0}
		assertRollupParity(t, s, sel)
	}
	// Full payload parity, including extrema, histogram provenance and gauge ties.
	page, err := s.ReadBuckets(context.Background(), Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3600, SeriesIDs: []SeriesID{2}})
	if err != nil {
		t.Fatal(err)
	}
	want := &AggregateDelta{}
	for _, bucket := range page.Buckets {
		want.Merge(bucket.Delta)
	}
	d, err := scanDelta(s.reader.QueryRow(`SELECT `+deltaColumnList+` FROM aggregate_hourly WHERE window_start=? AND series_id=2`, rollupTestHour).Scan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("hourly payload differs: got %+v want %+v", d, want)
	}
	if !reflect.DeepEqual(AccuracyFromHistogramDelta(d), AccuracyFromHistogramDelta(want)) {
		t.Fatal("histogram provenance changed")
	}

}

func TestRollupLateFinalizePurgeAndOldWriter(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 8, 2)
	finishRollups(t, s)
	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 7200}
	seed(t, s, &GroupBatch{Deltas: []DeltaRow{{SeriesID: 1, WindowStart: rollupTestHour + 300, Delta: spanDelta(3, 250)}}})
	assertRollupParity(t, s, sel)
	for i := 0; i < 2; i++ {
		if _, err := s.FinalizeWindow(rollupTestHour + 300); err != nil {
			t.Fatal(err)
		}
	}
	var state int
	if err := s.reader.QueryRow(`SELECT state FROM aggregate_rollup_hours WHERE window_start=?`, rollupTestHour).Scan(&state); !errors.Is(err, sql.ErrNoRows) && (err != nil || state != 0) {
		t.Fatalf("state=%d err=%v", state, err)
	}
	assertRollupParity(t, s, sel)
	finishRollups(t, s)
	assertRollupParity(t, s, sel)
	// SQL executed through an independent connection models a v5 writer that
	// does not know about the extension, including changing the window key.
	old, err := openAggregateDB(s.Path(), "NORMAL", 1024, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	if _, err := old.Exec(`UPDATE aggregate_buckets SET point_count=point_count+5 WHERE window_start=? AND series_id=1`, rollupTestHour); err != nil {
		t.Fatal(err)
	}
	assertRollupParity(t, s, sel)
	finishRollups(t, s)
	if _, err := old.Exec(`UPDATE aggregate_buckets SET window_start=? WHERE window_start=? AND series_id=1`, rollupTestHour-300, rollupTestHour); err != nil {
		t.Fatal(err)
	}
	assertRollupParity(t, s, sel)
	finishRollups(t, s)
	for i := 0; i < 2; i++ {
		if _, err := s.PurgeBefore(rollupTestHour + 900); err != nil {
			t.Fatal(err)
		}
	}
	assertRollupParity(t, s, sel)
	finishRollups(t, s)
	assertRollupParity(t, s, sel)
	if _, err := s.PurgeBefore(sel.End); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	for _, table := range []string{"aggregate_hourly", "aggregate_rollup_hours"} {
		var n int
		if err := s.reader.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s count=%d err=%v", table, n, err)
		}
	}
}

func TestRollupSketchReadKeepsSnapshotDuringPurgeAndFinalize(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 8, 3)
	finishRollups(t, s)
	seed(t, s, &GroupBatch{Deltas: []DeltaRow{{SeriesID: 1, WindowStart: rollupTestHour + 3600, Delta: spanDelta(3, 250)}}})
	sel := Selector{TenantID: 1, Start: rollupTestHour + 300, End: rollupTestHour + 3*3600}
	want := originalRollupSketches(t, s, sel)
	var expected uint64
	for _, sk := range want {
		expected += sk.Count()
	}
	var got uint64
	err := s.VisitSketches(context.Background(), sel, func(_ uint32, sk *Sketch) error {
		if got == 0 {
			if _, err := s.PurgeBefore(rollupTestHour + 7200); err != nil {
				return err
			}
			if _, err := s.FinalizeWindow(rollupTestHour + 3600); err != nil {
				return err
			}
		}
		got += sk.Count()
		return nil
	})
	if err != nil || got != expected {
		t.Fatalf("snapshot count=%d want=%d err=%v", got, expected, err)
	}
	assertRollupParity(t, s, sel)
}

func waitRollupPartial(t *testing.T, s *SQLiteStore) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_hourly`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("build did not write its first bounded batch")
}

func TestRollupInterruptedBuildsNeverPublish(t *testing.T) {
	for _, mutation := range []string{"cancel", "update", "purge", "failed_write"} {
		t.Run(mutation, func(t *testing.T) {
			s := rollupTestStore(t)
			populateRollupFixture(t, s, 260, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- s.buildRollupHour(ctx, rollupTestHour, 40*time.Millisecond) }()
			waitRollupPartial(t, s)
			switch mutation {
			case "cancel":
				cancel()
			case "update":
				if _, err := s.writer.Exec(`UPDATE aggregate_buckets SET point_count=point_count+1 WHERE window_start=? AND series_id=1`, rollupTestHour); err != nil {
					t.Fatal(err)
				}
			case "purge":
				if _, err := s.PurgeBefore(rollupTestHour + 300); err != nil {
					t.Fatal(err)
				}
			case "failed_write":
				if _, err := s.writer.Exec(`CREATE TRIGGER fail_rollup BEFORE INSERT ON aggregate_hourly BEGIN SELECT RAISE(ABORT,'injected rollup failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err := <-done
			if err == nil {
				t.Fatal("interrupted build succeeded")
			}
			if mutation == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			var state int
			if err := s.reader.QueryRow(`SELECT state FROM aggregate_rollup_hours WHERE window_start=?`, rollupTestHour).Scan(&state); !errors.Is(err, sql.ErrNoRows) && (err != nil || state == 2) {
				t.Fatalf("state=%d err=%v", state, err)
			}
			sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3600}
			assertRollupParity(t, s, sel)
			if mutation == "failed_write" {
				if _, err := s.writer.Exec(`DROP TRIGGER fail_rollup`); err != nil {
					t.Fatal(err)
				}
			}
			path, uuid := s.Path(), s.UUID()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := newTestStoreAt(t, path, StoreConfig{})
			reopened.stopRollups()
			if reopened.UUID() != uuid {
				t.Fatal("restart changed UUID")
			}
			finishRollups(t, reopened)
			assertRollupParity(t, reopened, sel)
		})
	}
}

func TestRollupSchemaIsAdditiveAndFailureDoesNotRebuild(t *testing.T) {
	for _, fault := range []string{"unchanged", "old_schema", "missing_hourly", "missing_trigger", "bad_version", "index_failure", "new_uuid"} {
		t.Run(fault, func(t *testing.T) {
			s := rollupTestStore(t)
			populateRollupFixture(t, s, 8, 1)
			finishRollups(t, s)
			uuid, path := s.UUID(), s.Path()
			switch fault {
			case "old_schema":
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
			case "missing_hourly":
				if _, err := s.writer.Exec(`DROP TABLE aggregate_hourly`); err != nil {
					t.Fatal(err)
				}
			case "missing_trigger":
				if _, err := s.writer.Exec(`DROP TRIGGER aggregate_rollup_update`); err != nil {
					t.Fatal(err)
				}
				if _, err := s.writer.Exec(`UPDATE aggregate_buckets SET point_count=point_count+1`); err != nil {
					t.Fatal(err)
				}
			case "bad_version":
				if _, err := s.writer.Exec(`UPDATE aggregate_rollup_meta SET value='999' WHERE key='version'`); err != nil {
					t.Fatal(err)
				}
			case "index_failure":
				if _, err := s.writer.Exec(`DROP INDEX idx_aggregate_hourly_summary`); err != nil {
					t.Fatal(err)
				}
				if _, err := s.writer.Exec(`CREATE TABLE idx_aggregate_hourly_summary(x)`); err != nil {
					t.Fatal(err)
				}
			case "new_uuid":
				if _, err := s.writer.Exec(`UPDATE aggregate_meta SET value='new-store' WHERE key='store_uuid'`); err != nil {
					t.Fatal(err)
				}
				uuid = "new-store"
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSQLiteStore(StoreConfig{Path: path, AllowRebuild: true})
			if fault == "bad_version" || fault == "index_failure" {
				if err == nil {
					_ = reopened.Close()
					t.Fatal("broken extension accepted")
				}
				db, err := openAggregateDB(path, "NORMAL", 1024, false)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				var got string
				if err := db.QueryRow(`SELECT value FROM aggregate_meta WHERE key='store_uuid'`).Scan(&got); err != nil || got != uuid {
					t.Fatalf("UUID changed after extension failure: %s %v", got, err)
				}
				var n int
				if err := db.QueryRow(`SELECT COUNT(*) FROM aggregate_buckets`).Scan(&n); err != nil || n == 0 {
					t.Fatalf("authoritative rows lost: %d %v", n, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			reopened.stopRollups()
			if reopened.UUID() != uuid {
				t.Fatal("additive migration changed UUID")
			}
			if fault == "unchanged" {
				var complete int
				if err := reopened.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_rollup_hours WHERE state=2`).Scan(&complete); err != nil || complete != 1 {
					t.Fatalf("reopen invalidated complete rollup: %d %v", complete, err)
				}
			}
			assertRollupParity(t, reopened, Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3600})
			finishRollups(t, reopened)
		})
	}
}

func TestRollupUnsafeSketchFallsBack(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 4, 1)
	huge := spanDelta(1, 100)
	huge.Sketch.ObserveN(100, math.MaxUint32)
	// A collapsed source and a saturated source must not be made more lossy by
	// hourly merging. Scalars can still use the complete hour.
	blob := huge.Sketch.AppendTo(nil)
	if _, err := s.writer.Exec(`UPDATE aggregate_buckets SET sketch=? WHERE series_id=1`, blob); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	var safe int
	if err := s.reader.QueryRow(`SELECT sketch_safe FROM aggregate_rollup_hours`).Scan(&safe); err != nil || safe != 0 {
		t.Fatalf("sketch_safe=%d err=%v", safe, err)
	}
	assertRollupParity(t, s, Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3600})
}

func TestRollupNegativeHourAndCancelledRead(t *testing.T) {
	for input, want := range map[int64]int64{-3601: -7200, -3600: -3600, -1: -3600, 0: 0, 1: 0, 3599: 0, 3600: 3600} {
		if got := hourStart(input); got != want {
			t.Fatalf("hour(%d)=%d want=%d", input, got, want)
		}
	}
	s := rollupTestStore(t)
	seed(t, s, &GroupBatch{Series: []SeriesRow{{ID: 1, Key: storeKey(1)}}, Deltas: []DeltaRow{{SeriesID: 1, WindowStart: -300, Delta: spanDelta(1, 100)}}})
	if _, err := s.FinalizeWindow(-300); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	var hour int64
	if err := s.reader.QueryRow(`SELECT window_start FROM aggregate_hourly`).Scan(&hour); err != nil || hour != -3600 {
		t.Fatalf("hour=%d err=%v", hour, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3600}
	if _, err := s.SumBuckets(ctx, sel, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.VisitSketches(ctx, sel, func(uint32, *Sketch) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRollupWorkerBuildsAndCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	s := newTestStoreAt(t, path, StoreConfig{})
	populateRollupFixture(t, s, 4, 1)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var state int
		err := s.reader.QueryRow(`SELECT state FROM aggregate_rollup_hours WHERE window_start=?`, rollupTestHour).Scan(&state)
		if err == nil && state == 2 {
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background worker did not build the closed hour")
}

func TestRollupMixedSketchScalesAndIncompleteHistogram(t *testing.T) {
	s := rollupTestStore(t)
	series := []SeriesRow{{ID: 1, Key: storeKey(1)}, {ID: 2, Key: storeKey(2)}}
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	seedSeries(t, tx, series)
	var ids []SeriesID
	var windows []int64
	var deltas []*AggregateDelta
	for w := 0; w < 36; w++ {
		for i := 1; i <= 2; i++ {
			d := spanDelta(1, 100+float64(w))
			if w%4 == 0 {
				d.Sketch = NewSketchAtScaleUnchecked(1)
				d.Sketch.Observe(100 + float64(w))
			}
			if w%7 == 0 {
				d.Sketch = nil
				d.HistogramFlags = HistPercentilesUnavailable
			}
			ids = append(ids, SeriesID(i))
			windows = append(windows, rollupTestHour+int64(w*300))
			deltas = append(deltas, d)
		}
	}
	insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	for _, only := range []bool{false, true} {
		assertRollupParity(t, s, Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3*3600, SketchOnly: only})
	}
}

func TestRollupPublicationDoesNotReplaceMutableOwnership(t *testing.T) {
	s := rollupTestStore(t)
	now := time.Unix(rollupTestHour+2*3600+120, 0)
	e := testEngine(t, now)
	e.SetStore(s)
	tenant, _ := e.TenantID("default")
	key := SeriesKey{TenantID: tenant, ServiceID: e.Cache().Intern(tenant, KindService, "svc"), NameID: e.Cache().Intern(tenant, KindOperation, "op"), Signal: SignalTraceOp}
	seed(t, s, &GroupBatch{Series: []SeriesRow{{ID: 1, Key: key}}, Deltas: []DeltaRow{{SeriesID: 1, WindowStart: rollupTestHour, Delta: spanDelta(2, 100)}}})
	if _, err := s.FinalizeWindow(rollupTestHour); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	live := WindowStart(now)
	e.ApplyDeltas(DeltaMap{{Key: key, WindowStart: live}: spanDelta(3, 200)})
	// The durable copy of the memory-owned delta must be excluded by plan.
	seed(t, s, &GroupBatch{Deltas: []DeltaRow{{SeriesID: 1, WindowStart: live, Delta: spanDelta(3, 200)}}})
	q := Query{Tenant: "default", Start: time.Unix(rollupTestHour, 0), End: now}
	got, err := e.QueryDashboard(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got.SpanCount != 5 {
		t.Fatalf("mutable ownership counted %d spans, want 5", got.SpanCount)
	}
}

func TestRollupSnapshotCannotDeadlockFinalizationWithOneReader(t *testing.T) {
	s := newTestStoreAt(t, filepath.Join(t.TempDir(), "aggregate.db"), StoreConfig{ReadPoolSize: 1})
	s.stopRollups()
	populateRollupFixture(t, s, 260, 1)
	seed(t, s, &GroupBatch{Deltas: []DeltaRow{{SeriesID: 1, WindowStart: rollupTestHour + 3600, Delta: spanDelta(1, 100)}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.buildRollupHour(ctx, rollupTestHour, 200*time.Millisecond) }()
	// The builder owns the single reader during this pause. Querying the read
	// pool here would itself block, so observe its bounded write on the writer.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := s.writer.QueryRow(`SELECT COUNT(*) FROM aggregate_hourly`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("builder did not reach snapshot")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := s.FinalizeWindow(rollupTestHour + 3600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("finalization blocked rollup snapshot: %v", err)
	}
	assertRollupParity(t, s, Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3900})
}

func TestRollupLogHeavySeriesCoverage(t *testing.T) {
	s := rollupTestStore(t)
	// Two complete service profiles at the measured 45 severity/template pairs
	// per service. Cardinality caps belong to the parent identity workstream.
	var series []SeriesRow
	var ids []SeriesID
	var windows []int64
	var deltas []*AggregateDelta
	for svc := 0; svc < 2; svc++ {
		for pair := 0; pair < 45; pair++ {
			id := SeriesID(len(series) + 1)
			key := SeriesKey{TenantID: 1, ServiceID: uint32(2 + svc), NameID: uint32(100 + pair%20), Signal: SignalLog, StatusClass: StatusClass(1 + pair/20)}
			series = append(series, SeriesRow{ID: id, Key: key})
			for w := 0; w < 24; w++ {
				ids = append(ids, id)
				windows = append(windows, rollupTestHour+int64(w*300))
				deltas = append(deltas, &AggregateDelta{Count: 7, LogCount: 7, ErrorCount: uint64(pair % 3)})
			}
		}
	}
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	seedSeries(t, tx, series)
	insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	for _, start := range []int64{rollupTestHour, rollupTestHour + 300} {
		assertRollupParity(t, s, Selector{TenantID: 1, Start: start, End: rollupTestHour + 7200, Signal: SignalLog})
	}
	rows, err := s.SumBuckets(context.Background(), Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 7200, Signal: SignalLog}, GroupByService)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, r := range rows {
		if r.LogCount != 45*24*7 {
			t.Fatalf("service log count=%d", r.LogCount)
		}
	}
}

func TestRollupPreservesSaturationProvenanceAcrossHours(t *testing.T) {
	s := rollupTestStore(t)
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	seedSeries(t, tx, []SeriesRow{{ID: 1, Key: storeKey(1)}})
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
	finishRollups(t, s)
	assertRollupParity(t, s, Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3*3600, Signal: SignalTraceOp})
}

func TestRollupWaitsForHourLatenessAndDropsEmptyHours(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 4, 1)
	if err := s.discoverRollupHours(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed := time.Unix(rollupTestHour+3600, 0).Add(AllowedLateness)
	if err := s.buildPendingRollups(context.Background(), closed.Add(-time.Second), 0); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_hourly`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("hour built before lateness horizon: %d %v", n, err)
	}
	if err := s.buildPendingRollups(context.Background(), closed, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_hourly`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("closed hour rows=%d err=%v", n, err)
	}
	if err := s.buildRollupHour(context.Background(), rollupTestHour-3600, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_rollup_hours WHERE window_start=?`, rollupTestHour-3600).Scan(&n); err != nil || n != 0 {
		t.Fatalf("empty hour left a marker: %d %v", n, err)
	}
}

func TestRollupNewWindowCopyStillValidatesAndRollsBack(t *testing.T) {
	s := rollupTestStore(t)
	seed(t, s, &GroupBatch{Series: []SeriesRow{{ID: 1, Key: storeKey(1)}}, Deltas: []DeltaRow{{SeriesID: 1, WindowStart: rollupTestHour, Delta: spanDelta(1, 100)}}})
	if _, err := s.writer.Exec(`UPDATE aggregate_delta_log SET sketch=?`, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinalizeWindow(rollupTestHour); err == nil {
		t.Fatal("corrupt sketch was finalized")
	}
	assertCount(t, s, "aggregate_delta_log", 1)
	assertCount(t, s, "aggregate_buckets", 0)
	if _, err := s.writer.Exec(`UPDATE aggregate_delta_log SET sketch=?`, spanDelta(1, 100).Sketch.AppendTo(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writer.Exec(`CREATE TRIGGER fail_bucket BEFORE INSERT ON aggregate_buckets BEGIN SELECT RAISE(ABORT,'injected copy failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinalizeWindow(rollupTestHour); err == nil {
		t.Fatal("failed copy committed")
	}
	assertCount(t, s, "aggregate_delta_log", 1)
	assertCount(t, s, "aggregate_buckets", 0)
	if _, err := s.writer.Exec(`DROP TRIGGER fail_bucket`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinalizeWindow(rollupTestHour); err != nil {
		t.Fatal(err)
	}
	assertCount(t, s, "aggregate_delta_log", 0)
	assertCount(t, s, "aggregate_buckets", 1)
}
