package aggregate

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestRollupScalarFixedHourGuard(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 8, 3)
	finishRollups(t, s)
	for _, tc := range []struct {
		name       string
		start, end int64
	}{
		{"one_hour", rollupTestHour, rollupTestHour + 3600},
		{"multiple_hours", rollupTestHour, rollupTestHour + 10800},
		{"partial_start", rollupTestHour + 300, rollupTestHour + 3600},
		{"partial_end", rollupTestHour, rollupTestHour + 3300},
		{"unaligned_hour", rollupTestHour + 300, rollupTestHour + 3900},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := Selector{TenantID: 1, Start: tc.start, End: tc.end}
			want := originalRollupSums(t, s, sel, GroupByService|GroupBySignal)
			got, err := readSumBuckets(context.Background(), s.reader, sel, GroupByService|GroupBySignal, true)
			if err != nil {
				t.Fatal(err)
			}
			sortRollupSums(want)
			sortRollupSums(got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v; want %+v", got, want)
			}
		})
	}
}

func TestRollupScalarHourlyPlan(t *testing.T) {
	s := rollupTestStore(t)
	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3600,
		Signals: []Signal{SignalTraceOp, SignalLog}}
	q, args := sumHistoryQuery(sel, GroupByService|GroupBySignal, true)
	rows, err := s.reader.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	hourly := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		// Inspect only the hourly branch. The following delta branch must retain
		// its series membership filter and per-series reduction.
		if strings.Contains(detail, "UNION ALL") {
			break
		}
		if strings.Contains(detail, "idx_aggregate_hourly_summary") {
			hourly = true
		}
		if strings.Contains(detail, "idx_aggregate_series_scope") || detail == "CO-ROUTINE t" {
			t.Fatalf("hourly plan rebuilt series membership or reduced unique rows: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !hourly {
		t.Fatal("hourly covering index was not used")
	}
}

func TestRollupScalarPublicMultiHourExactCounts(t *testing.T) {
	s := rollupTestStore(t)
	const count = uint64(1<<53) + 7
	batch := &GroupBatch{
		Dicts: []DictRow{{ID: 50, TenantID: 1, Kind: KindOperation, Value: []byte(OtherValue)}},
		Series: []SeriesRow{
			{ID: 1, Key: SeriesKey{TenantID: 1, ServiceID: 2, NameID: 50, Signal: SignalTraceOp}},
			{ID: 2, Key: SeriesKey{TenantID: 1, ServiceID: 2, NameID: 51, Signal: SignalLog}},
			{ID: 3, Key: SeriesKey{TenantID: 9, ServiceID: 2, NameID: 52, Signal: SignalTraceOp}},
			{ID: 4, Key: SeriesKey{TenantID: 1, ServiceID: 3, NameID: 53, Signal: SignalTraceOp}},
		},
	}
	for h := int64(0); h < 3; h++ {
		w := rollupTestHour + h*3600 + 300
		batch.Deltas = append(batch.Deltas,
			DeltaRow{SeriesID: 1, WindowStart: w, Delta: &AggregateDelta{Count: count, ErrorCount: count - 1, RequestCount: count - 2, ErrorRequestCount: count - 3, DurationCount: 1, DurationSum: 100}},
			DeltaRow{SeriesID: 2, WindowStart: w, Delta: &AggregateDelta{Count: 11, LogCount: 11}},
			DeltaRow{SeriesID: 3, WindowStart: w, Delta: spanDelta(13, 100)},
			DeltaRow{SeriesID: 4, WindowStart: w, Delta: spanDelta(17, 100)})
	}
	seed(t, s, batch)
	for h := int64(0); h < 3; h++ {
		if _, err := s.FinalizeWindow(rollupTestHour + h*3600 + 300); err != nil {
			t.Fatal(err)
		}
	}
	finishRollups(t, s)
	base := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 10800}
	for _, phase := range []string{"completed", "missing_hour", "invalidated_hour"} {
		if phase == "missing_hour" {
			if _, err := s.writer.Exec(`DELETE FROM aggregate_rollup_hours WHERE window_start=?`, rollupTestHour+3600); err != nil {
				t.Fatal(err)
			}
		}
		if phase == "invalidated_hour" {
			// A prior-version writer invalidates the marker through the existing trigger.
			if _, err := s.writer.Exec(`UPDATE aggregate_buckets SET point_count=point_count+1 WHERE window_start=? AND series_id=4`, rollupTestHour+7500); err != nil {
				t.Fatal(err)
			}
		}
		t.Run(phase, func(t *testing.T) {
			selectors := []Selector{base,
				{TenantID: 1, Start: base.Start, End: base.End, Signal: SignalTraceOp, ServiceIDs: []uint32{2}, SeriesIDs: []SeriesID{1, 2, 3}},
				{TenantID: 1, Start: base.Start, End: base.End, Signals: []Signal{SignalTraceOp, SignalLog}, ServiceIDs: []uint32{2}, SeriesIDs: []SeriesID{1, 2, 3}},
				{TenantID: 9, Start: base.Start, End: base.End, Signal: SignalTraceOp, ServiceIDs: []uint32{2}},
				{TenantID: 1, Start: base.Start, End: base.End, SeriesIDs: []SeriesID{3}},
				{TenantID: 1, Start: base.Start, End: base.End, ServiceIDs: []uint32{999}},
				{TenantID: 1, Start: base.Start + 600, End: base.End - 3300},
			}
			for _, sel := range selectors {
				for _, by := range []GroupBy{GroupByService, GroupByService | GroupBySignal, GroupByService | GroupByName | GroupBySignal} {
					want := originalRollupSums(t, s, sel, by)
					got, err := s.SumBuckets(context.Background(), sel, by)
					if err != nil {
						t.Fatal(err)
					}
					sortRollupSums(want)
					sortRollupSums(got)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("selector %+v by %d: got %+v; want %+v", sel, by, got, want)
					}
				}
			}
			got, err := s.SumBuckets(context.Background(), selectors[1], GroupByService)
			if err != nil || len(got) != 1 || got[0].Count != 3*count || got[0].IdentityCount != 3*count || got[0].RequestCount != 3*(count-2) || got[0].ErrorRequestCount != 3*(count-3) {
				t.Fatalf("exact counters above 2^53 changed: %+v err=%v", got, err)
			}
		})
	}
}
