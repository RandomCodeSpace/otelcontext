package aggregate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func assertDictionaryValueIndex(t *testing.T, db *sql.DB) {
	t.Helper()
	var columns, expected, indexes int
	if err := db.QueryRow(`SELECT COUNT(*),SUM(
		(seqno=0 AND cid=3 AND name='value' AND desc=0 AND coll='BINARY' AND key=1)
		OR (seqno=1 AND cid=-1 AND name IS NULL AND key=0))
		FROM pragma_index_xinfo('idx_aggregate_dict_value')`).Scan(&columns, &expected); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_index_list('aggregate_dict')
		WHERE name='idx_aggregate_dict_value' AND "unique"=0 AND partial=0 AND origin='c'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if columns != 2 || expected != 2 || indexes != 1 {
		t.Fatal("expected nonunique value index with covering rowid", columns, expected, indexes)
	}
}

// Hash every authoritative row, including metadata and blobs, rather than
// treating unchanged row counts as evidence that an upgrade retained data.
func dictionaryIndexDataHash(t *testing.T, db *sql.DB) string {
	t.Helper()
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, table := range aggregateTables {
		fmt.Fprintln(h, table)
		rows, err := db.Query(`SELECT * FROM ` + table + ` ORDER BY 1,2`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		values, targets := make([]any, len(cols)), make([]any, len(cols))
		for i := range values {
			targets[i] = &values[i]
		}
		for rows.Next() {
			if err := rows.Scan(targets...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if err := enc.Encode(values); err != nil {
				rows.Close()
				t.Fatal(err)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestDictionaryValueIndexUpgrade(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			s := rollupTestStore(t)
			assertDictionaryValueIndex(t, s.reader) // New stores get the same index.
			populateRollupFixture(t, s, 8, 1)
			if _, err := s.writer.Exec(`DROP INDEX idx_aggregate_dict_value`); err != nil {
				t.Fatal(err)
			}
			if conflict {
				if _, err := s.writer.Exec(`CREATE TABLE idx_aggregate_dict_value(x)`); err != nil {
					t.Fatal(err)
				}
			}
			before, uuid, path := dictionaryIndexDataHash(t, s.reader), s.UUID(), s.Path()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			for reopen := 0; reopen < 2; reopen++ {
				opened, err := OpenSQLiteStore(StoreConfig{Path: path, AllowRebuild: true})
				if conflict {
					if err == nil {
						opened.Close()
						t.Fatal("accepted conflicting index name")
					}
					if !strings.Contains(err.Error(), "add dictionary value index") {
						t.Fatal("wrong startup failure", err)
					}
					db, err := openAggregateDB(path, "NORMAL", 1024, false)
					if err != nil {
						t.Fatal(err)
					}
					got := dictionaryIndexDataHash(t, db)
					db.Close()
					if got != before {
						t.Fatal("index failure rebuilt or changed authoritative data")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				opened.stopRollups()
				assertDictionaryValueIndex(t, opened.reader)
				if opened.UUID() != uuid || dictionaryIndexDataHash(t, opened.reader) != before {
					opened.Close()
					t.Fatal("index upgrade changed UUID, versions or authoritative rows")
				}
				if err := opened.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestDictionaryValueIndexScalarOrder(t *testing.T) {
	s := rollupTestStore(t)
	if _, err := s.writer.Exec(`DROP INDEX idx_aggregate_dict_value`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Series order and identity order give different SQLite SUM bits. The
	// dictionary lookup index must preserve each authoritative selector's order.
	values := []float64{1.4210854715202004e-14, 7.663350976776343e-47, 1.313717310304516e-46, 1.5777218104420236e-30, 40, 120}
	var series []SeriesRow
	var ids []SeriesID
	var windows []int64
	var deltas []*AggregateDelta
	for i, v := range values {
		series = append(series, SeriesRow{ID: SeriesID(i + 1), Key: storeKey(uint32(106 - i))})
		ids = append(ids, SeriesID(i+1))
		windows = append(windows, rollupTestHour)
		deltas = append(deltas, &AggregateDelta{Count: 1, DurationCount: 1, DurationSum: v})
	}
	seedSeries(t, tx, series)
	insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	type result struct {
		rows []SumRow
		plan string
	}
	read := func(services []uint32) result {
		sel := Selector{TenantID: 1, Signal: SignalTraceOp, ServiceIDs: services, Start: rollupTestHour, End: rollupTestHour + rollupHour}
		q, args := sumHistoryQuery(sel, GroupByService, true)
		rows, err := readSumQuery(context.Background(), s.reader, q, args)
		if err != nil {
			t.Fatal(err)
		}
		return result{rows, explainHistoryQuery(t, s, q, args)}
	}
	filters := [][]uint32{nil, {2}, {0}, {2, 3}}
	before := make([]result, len(filters))
	for i, filter := range filters {
		before[i] = read(filter)
	}
	if math.Float64bits(before[0].rows[0].DurationSum) != 0x4064000000000001 || math.Float64bits(before[1].rows[0].DurationSum) != 0x4064000000000000 {
		t.Fatal("order-sensitive counterexample was not exercised", before)
	}
	if _, err := s.writer.Exec(dictionaryValueIndexDDL); err != nil {
		t.Fatal(err)
	}
	for i, filter := range filters {
		after := read(filter)
		if !reflect.DeepEqual(before[i].rows, after.rows) {
			t.Fatal("dictionary index changed scalar arithmetic", filter, before[i].rows, after.rows)
		}
		if strings.Count(before[i].plan, "SCAN aggregate_dict") != 6 || strings.Count(after.plan, "SEARCH aggregate_dict USING COVERING INDEX idx_aggregate_dict_value (value=?)") != 6 {
			t.Fatal("dictionary scans were not replaced by covering seeks", after.plan)
		}
		oldPlan := strings.ReplaceAll(before[i].plan, "SCAN aggregate_dict", "DICT_LOOKUP")
		newPlan := strings.ReplaceAll(after.plan, "SEARCH aggregate_dict USING COVERING INDEX idx_aggregate_dict_value (value=?)", "DICT_LOOKUP")
		if oldPlan != newPlan {
			t.Fatalf("source scan/group order changed:\nbefore:\n%s\nafter:\n%s", before[i].plan, after.plan)
		}
	}
}
