package aggregate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
)

// Captures cleanup eligibility from the public path, then executes the same
// statements through the actual glebarez driver. Capture the replacement
// marker and boundary probes, not just the obsolete full-summary scan.
type lifecycleReviewQuery struct {
	sql        string
	args       []any
	writerHeld bool
}
type lifecycleReviewConnector struct {
	base    driver.Driver
	path    string
	store   *SQLiteStore
	queries []lifecycleReviewQuery
}

func (c *lifecycleReviewConnector) Driver() driver.Driver { return c.base }
func (c *lifecycleReviewConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.path + "?_txlock=immediate")
	if err != nil {
		return nil, err
	}
	return &lifecycleReviewConn{Conn: conn, c: c}, nil
}

type lifecycleReviewConn struct {
	driver.Conn
	c *lifecycleReviewConnector
}

func (c *lifecycleReviewConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *lifecycleReviewConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.HasPrefix(q, "SELECT m.state,") || strings.Contains(q, "EXISTS(SELECT 1 FROM aggregate_buckets") {
		values := make([]any, len(args))
		for i, v := range args {
			values[i] = v.Value
		}
		c.c.queries = append(c.c.queries, lifecycleReviewQuery{q, values, len(c.c.store.writeMu) == 1})
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

func TestReviewServiceSummaryCleanupEligibilityBounded(t *testing.T) {
	for _, mode := range []string{"retained", "boundary-no-expired", "resumed"} {
		t.Run(mode, func(t *testing.T) {
			s := rollupTestStore(t)
			tx, err := s.writer.Begin()
			if err != nil {
				t.Fatal(err)
			}
			var series []SeriesRow
			var ids []SeriesID
			var windows []int64
			var deltas []*AggregateDelta
			first := 0
			if mode == "boundary-no-expired" {
				first = 6
			}
			for i := 1; i <= 12; i++ {
				key := storeKey(uint32(i + 100))
				key.ServiceID = uint32(i + 200)
				series = append(series, SeriesRow{ID: SeriesID(i), Key: key})
				for w := first; w < 12; w++ {
					ids = append(ids, SeriesID(i))
					windows = append(windows, rollupTestHour+int64(w*300))
					deltas = append(deltas, &AggregateDelta{Count: 1})
				}
			}
			seedSeries(t, tx, series)
			insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			finishRollups(t, s)
			finishSummary(t, s, rollupTestHour, 4)
			var cells int
			if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_service_summary WHERE resolution=300`).Scan(&cells); err != nil {
				t.Fatal(err)
			}
			if cells != 12*(12-first) {
				t.Fatal("fixture size", cells)
			}
			original := s.writer
			capture := &lifecycleReviewConnector{base: original.Driver(), path: s.path, store: s}
			s.writer = sql.OpenDB(capture)
			s.writer.SetMaxOpenConns(1)
			defer func() { s.writer.Close(); s.writer = original }()
			cutoff := int64(rollupTestHour) + int64(first*300)
			if mode == "resumed" {
				// Reconstruct a persisted cleanup claim with retained source data. Every
				// continuation must short-circuit eligibility before examining data.
				if _, err := s.writer.Exec(`UPDATE aggregate_service_summary_hours SET state=3,build_id='prior-cleanup'`); err != nil {
					t.Fatal(err)
				}
				c, err := s.claimSummaryCleanup(context.Background(), rollupTestHour, rollupTestHour+300)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					done, err := s.summaryCleanupSlice(context.Background(), c, 1)
					if err != nil || done {
						t.Fatal("small cleanup continuation", done, err)
					}
				}
				// A later public purge claims a new token and finishes the same cleanup.
				if _, err := s.PurgeBefore(cutoff); err != nil {
					t.Fatal(err)
				}
				if _, err := s.summaryCleanupSlice(context.Background(), c, 1); err != errRollupInvalidated {
					t.Fatal("old cleanup was not fenced", err)
				}
				if n := summaryRowCount(t, s); n != 0 {
					t.Fatal("resumed cleanup incomplete", n)
				}
			} else {
				if _, err := s.PurgeBefore(cutoff); err != nil {
					t.Fatal(err)
				}
				if ready, err := summaryReady(context.Background(), s.reader, rollupTestHour); err != nil || !ready {
					t.Fatal("retained generation lost", ready, err)
				}
			}
			if len(capture.queries) == 0 {
				t.Fatal("production eligibility was not captured")
			}
			boundaryProbes := 0
			markerProbes := 0
			for _, q := range capture.queries {
				if !q.writerHeld {
					t.Fatal("not the serialized writer")
				}
				t.Logf("mode=%s cells=%d writer=%v SQL=%s args=%v", mode, cells, q.writerHeld, q.sql, q.args)
				plan, ops := summaryReviewPlan(t, s.reader, q.sql, q.args)
				for _, line := range plan {
					if strings.Contains(line, "aggregate_service_summary USING") {
						t.Fatal("eligibility probes summary cells", line)
					}
				}
				if strings.HasPrefix(q.sql, "SELECT m.state,") {
					markerProbes++
					seeks := 0
					for _, x := range ops {
						if x.op == "SeekRowid" {
							seeks++
							t.Logf("marker point seek: %+v", x)
						}
					}
					if seeks != 2 {
						t.Fatalf("marker/source check is not two point seeks: %d", seeks)
					}
				} else {
					boundaryProbes++
					var root int
					if err := s.reader.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name='aggregate_buckets'`).Scan(&root); err != nil {
						t.Fatal(err)
					}
					cursor := -1
					lo, hi, limit := false, false, false
					for _, x := range ops {
						if x.op == "OpenRead" && x.p2 == root {
							cursor = x.p1
						}
						if x.p1 == cursor {
							if x.op == "SeekGE" && x.p4 == "1" {
								lo = true
								t.Logf("boundary lower seek: %+v", x)
							}
							if x.op == "IdxGE" && x.p4 == "1" {
								hi = true
								t.Logf("boundary upper bound: %+v", x)
							}
							if x.op == "Column" {
								t.Fatal("boundary query rejects rows after the time seek", x)
							}
						}
						if x.op == "DecrJumpZero" {
							limit = true
						}
					}
					if !lo || !hi || !limit {
						t.Fatal("boundary probe is not one bounded time-prefix existence seek", lo, hi, limit)
					}
				}
			}
			if markerProbes == 0 {
				t.Fatal("missing marker point checks")
			}
			expected := 0
			if mode == "boundary-no-expired" {
				expected = 1
			}
			if boundaryProbes != expected {
				t.Fatalf("data probes=%d want=%d", boundaryProbes, expected)
			}
		})
	}
}
