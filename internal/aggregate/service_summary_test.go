package aggregate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServiceSummarySketchCodec(t *testing.T) {
	for _, n := range []int{0, 1, 256, 257, 512} {
		for _, population := range []uint64{1, math.MaxUint32} {
			t.Run(fmt.Sprintf("bins%d/population%d", n, population), func(t *testing.T) {
				s := NewSketch()
				for i := 0; i < n; i++ {
					s.add(int32(i-250), population)
				}
				s.zeroCount = math.MaxUint64 - s.binTotal
				s.count = math.MaxUint64
				s.sum = 123456789.12345678
				low, high, safe := encodeSummarySketch(s)
				if !safe || (high != nil) != (n > 256) {
					t.Fatal("unexpected representation")
				}
				got, err := decodeSummarySketch(low, high)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(s, got) {
					t.Fatalf("round trip differs: %+v / %+v", s, got)
				}
				for _, q := range []float64{0, .5, .9, .99, 1} {
					if got.Quantile(q) != s.Quantile(q) {
						t.Fatal("quantile changed")
					}
				}
			})
		}
	}
	for _, s := range []*Sketch{nil, NewSketch(), {scale: SketchDefaultScale, zeroCount: 7, count: 7, sum: 0.25}} {
		low, high, ok := encodeSummarySketch(s)
		got, err := decodeSummarySketch(low, high)
		if !ok || err != nil || !reflect.DeepEqual(s, got) {
			t.Fatalf("absent/empty/zeros: %v %v", ok, err)
		}
	}
	sparse := NewSketch()
	sparse.add(-200, 2)
	sparse.add(311, 3)
	sparse.count = 5
	sparse.sum = 3.141592653589793
	low, high, ok := encodeSummarySketch(sparse)
	got, err := decodeSummarySketch(low, high)
	if !ok || err != nil || !reflect.DeepEqual(got, sparse) {
		t.Fatal("sparse roundtrip", err)
	}
	for name, mutate := range map[string]func(*Sketch){
		"scale": func(s *Sketch) { s.scale-- }, "deficit": func(s *Sketch) { s.count++ },
		"wrap": func(s *Sketch) { s.zeroCount = math.MaxUint64 }, "collapsed": func(s *Sketch) { s.collapsed = true },
		"saturated": func(s *Sketch) { s.saturations = 1 }, "span": func(s *Sketch) { s.maxIdx++ },
		"sum": func(s *Sketch) { s.sum = math.Inf(1) }, "population": func(s *Sketch) { s.binTotal++ },
	} {
		t.Run(name, func(t *testing.T) {
			s := *sparse
			mutate(&s)
			if _, _, ok := encodeSummarySketch(&s); ok {
				t.Fatal("accepted unsafe sketch")
			}
		})
	}
	for _, blobs := range [][2][]byte{{{}, nil}, {nil, {1}}, {append(append([]byte{}, low...), 0), nil}, {low, low}} {
		if _, err := decodeSummarySketch(blobs[0], blobs[1]); err == nil {
			t.Fatal("accepted corrupt payload")
		}
	}
}

func TestServiceSummaryIdentityCursorPlan(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 48, 1)
	ctx := context.Background()
	var after *summaryIdentity
	seen := map[SeriesID]bool{}
	for {
		batch, err := summaryIdentities(ctx, s.reader, after, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
		for _, v := range batch {
			if seen[v.id] {
				t.Fatal("repeated identity", v.id)
			}
			seen[v.id] = true
		}
		v := batch[len(batch)-1]
		after = &v
	}
	if len(seen) != 48 {
		t.Fatal("cursor omitted identities", len(seen))
	}
	q, args := summaryIdentityQuery(after, 3)
	rows, err := s.reader.Query(`EXPLAIN QUERY PLAN `+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	var plans []string
	for rows.Next() {
		var a, b, c int
		var d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	plan := strings.Join(plans, "\n")
	t.Log(plan)
	if !strings.Contains(plan, "SEARCH aggregate_series USING COVERING INDEX idx_aggregate_series_identity") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("cursor does not seek covering identity index: %s", plan)
	}
	rows, err = s.reader.Query(`EXPLAIN `+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seek, limit, sorter := false, false, false
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var op string
		var p4, comment any
		if err := rows.Scan(&addr, &op, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			t.Fatal(err)
		}
		if op == "SeekGE" || op == "SeekGT" {
			seek = fmt.Sprint(p4) == "10"
		}
		if op == "DecrJumpZero" {
			limit = true
		}
		if strings.HasPrefix(op, "Sorter") {
			sorter = true
		}
		t.Log(addr, op, p1, p2, p3, p4)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seek || !limit || sorter {
		t.Fatal("unbounded cursor plan", seek, limit, sorter)
	}
}

func finishSummary(t *testing.T, s *SQLiteStore, hour int64, allowance int) *summaryBuild {
	t.Helper()
	c, err := s.claimSummaryBuild(context.Background(), hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		done, err := s.summaryBuildSlice(context.Background(), c, allowance)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return c
		}
	}
	t.Fatal("summary did not finish")
	return nil
}
func summaryRowCount(t *testing.T, s *SQLiteStore) int {
	t.Helper()
	var n int
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_service_summary`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestServiceSummaryResumesBoundedSlices(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 48, 1)
	finishRollups(t, s)
	ctx := context.Background()
	c, err := s.claimSummaryBuild(ctx, rollupTestHour)
	if err != nil {
		t.Fatal(err)
	}
	token := c.token
	var last *summaryIdentity
	yielded := 0
	for i := 0; i < 100; i++ {
		done, err := s.summaryBuildSlice(ctx, c, 1)
		if err != nil {
			t.Fatal(err)
		}
		if c.token != token {
			t.Fatal("yield replaced generation")
		}
		if c.after != nil {
			if last != nil && c.after.id == last.id && !done {
				t.Fatal("cursor failed to advance")
			}
			last = c.after
		}
		if s.reader.Stats().InUse != 0 || s.writer.Stats().InUse != 0 {
			t.Fatal("yield retained connection")
		}
		if done {
			break
		}
		yielded++
	}
	if !c.done || yielded < 48 {
		t.Fatalf("did not cross slices: done=%v yields=%d", c.done, yielded)
	}
	var state int
	var storedToken string
	if err := s.reader.QueryRow(`SELECT state,build_id FROM aggregate_service_summary_hours WHERE window_start=?`, rollupTestHour).Scan(&state, &storedToken); err != nil {
		t.Fatal(err)
	}
	if state != 2 || storedToken != token {
		t.Fatal("publication token", state, storedToken)
	}
	n := summaryRowCount(t, s)
	if n == 0 {
		t.Fatal("missing output")
	}
	if _, err := s.claimSummaryBuild(ctx, rollupTestHour); !errors.Is(err, errRollupInvalidated) {
		t.Fatal("published twice", err)
	}
	if summaryRowCount(t, s) != n {
		t.Fatal("repeat claim changed rows")
	}
	// Each cell's source scalars are checked independently against authoritative
	// SQL, including a service and edge name whose variants cross tiny slices.
	rows, err := s.reader.Query(`SELECT resolution,tenant_id,signal,service_id,name_id,window_start,` + summaryValueColumns + ` FROM aggregate_service_summary`)
	if err != nil {
		t.Fatal(err)
	}
	var cells []summaryCell
	for rows.Next() {
		var c summaryCell
		if err := rows.Scan(&c.resolution, &c.tenant, &c.signal, &c.service, &c.name, &c.window, &c.scalar.counts[0], &c.scalar.counts[1], &c.scalar.counts[2], &c.scalar.counts[3], &c.scalar.counts[4], &c.scalar.sum, &c.scalar.counts[5], &c.scalar.counts[6]); err != nil {
			t.Fatal(err)
		}
		cells = append(cells, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, c := range cells {
		sel := Selector{TenantID: uint32(c.tenant), Signal: Signal(c.signal), ServiceIDs: []uint32{uint32(c.service)}, Start: c.window, End: c.window + c.resolution}
		by := GroupByService
		if c.signal == int64(SignalServiceEdge) {
			by |= GroupByName
		}
		want := originalRollupSums(t, s, sel, by)
		var found bool
		for _, r := range want {
			if r.NameID == uint32(c.name) {
				found = true
				counts := [7]int64{int64(r.Count), int64(r.ErrorCount), int64(r.RequestCount), int64(r.ErrorRequestCount), int64(r.DurationCount), int64(r.LogCount), int64(r.IdentityCount)}
				if c.scalar.counts != counts || math.Abs(c.scalar.sum-r.DurationSum) > 1e-9 {
					t.Fatalf("cell differs %+v / %+v", c, r)
				}
			}
		}
		if !found {
			t.Fatal("extra cell", c)
		}
	}
}
func TestServiceSummaryBuildInvalidationAndRestart(t *testing.T) {
	for _, kind := range []string{"source", "summary", "uuid", "write", "restart"} {
		t.Run(kind, func(t *testing.T) {
			s := rollupTestStore(t)
			populateRollupFixture(t, s, 32, 1)
			finishRollups(t, s)
			ctx := context.Background()
			c, err := s.claimSummaryBuild(ctx, rollupTestHour)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 12; i++ {
				if _, err := s.summaryBuildSlice(ctx, c, 1); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "source":
				_, err = s.writer.Exec(`UPDATE aggregate_rollup_hours SET build_id='replacement' WHERE window_start=?`, rollupTestHour)
			case "summary":
				_, err = s.writer.Exec(`UPDATE aggregate_service_summary_hours SET build_id='replacement' WHERE window_start=?`, rollupTestHour)
			case "uuid":
				_, err = s.writer.Exec(`UPDATE aggregate_meta SET value='replacement' WHERE key='store_uuid'`)
			case "write":
				_, err = s.writer.Exec(`CREATE TRIGGER summary_fail BEFORE INSERT ON aggregate_service_summary BEGIN SELECT RAISE(ABORT,'injected summary failure'); END`)
			case "restart": // Drop the in-memory continuation, as after process loss.
				finishSummary(t, s, rollupTestHour, 2)
				if _, err := s.summaryBuildSlice(ctx, c, 1); !errors.Is(err, errRollupInvalidated) {
					t.Fatal("restart did not fence prior builder", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var failure error
			for i := 0; i < 100; i++ {
				_, failure = s.summaryBuildSlice(ctx, c, 1)
				if failure != nil {
					break
				}
			}
			if failure == nil || !c.failed {
				t.Fatal("invalidation/failure did not abandon continuation")
			}
			if kind == "write" {
				if _, err := s.writer.Exec(`DROP TRIGGER summary_fail`); err != nil {
					t.Fatal(err)
				}
				finishSummary(t, s, rollupTestHour, 2)
				var count int64
				if err := s.reader.QueryRow(`SELECT SUM(point_count) FROM aggregate_service_summary WHERE resolution=300`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				var want int64
				if err := s.reader.QueryRow(`SELECT SUM(b.point_count) FROM aggregate_buckets b JOIN aggregate_series s ON s.id=b.series_id WHERE s.signal=?`, SignalTraceOp).Scan(&want); err != nil {
					t.Fatal(err)
				}
				if count != want {
					t.Fatal("failure doubled output", count, want)
				}
			}
		})
	}
}
func TestServiceSummaryCleanupFencesReplacement(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 48, 1)
	finishRollups(t, s)
	finishSummary(t, s, rollupTestHour, 4)
	ctx := context.Background()
	// Remove one source window, leaving a retained boundary hour.
	if _, _, err := purgeWindow(s.writer, rollupTestHour); err != nil {
		t.Fatal(err)
	}
	c, err := s.claimSummaryCleanup(ctx, rollupTestHour, rollupTestHour+300)
	if err != nil {
		t.Fatal(err)
	}
	if done, err := s.summaryCleanupSlice(ctx, c, 1); err != nil || done {
		t.Fatal("first bounded cleanup", done, err)
	}
	finishRollups(t, s)
	replacement := finishSummary(t, s, rollupTestHour, 3)
	n := summaryRowCount(t, s)
	if _, err := s.summaryCleanupSlice(ctx, c, 128); !errors.Is(err, errRollupInvalidated) {
		t.Fatal("stale cleanup not fenced", err)
	}
	var token string
	var state int
	if err := s.reader.QueryRow(`SELECT state,build_id FROM aggregate_service_summary_hours WHERE window_start=?`, rollupTestHour).Scan(&state, &token); err != nil {
		t.Fatal(err)
	}
	if state != 2 || token != replacement.token || summaryRowCount(t, s) != n {
		t.Fatal("stale cleanup changed replacement")
	}
}
func TestServiceSummaryCleanupFencesBuilderAndOrphans(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 32, 1)
	finishRollups(t, s)
	ctx := context.Background()
	c, err := s.claimSummaryBuild(ctx, rollupTestHour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		if _, err := s.summaryBuildSlice(ctx, c, 1); err != nil {
			t.Fatal(err)
		}
	}
	// Purge all authoritative data. Derived rows must neither publish nor pin
	// identities. Restarted cleanup claims fence the abandoned cleanup owner.
	for window := rollupTestHour; window < rollupTestHour+rollupHour; window += 300 {
		if _, _, err := purgeWindow(s.writer, window); err != nil {
			t.Fatal(err)
		}
	}
	old, err := s.claimSummaryCleanup(ctx, rollupTestHour, rollupTestHour+rollupHour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.summaryBuildSlice(ctx, c, 1); !errors.Is(err, errRollupInvalidated) {
		t.Fatal("cleanup did not fence builder", err)
	}
	resumed, err := s.claimSummaryCleanup(ctx, rollupTestHour, old.cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.summaryCleanupSlice(ctx, old, 1); !errors.Is(err, errRollupInvalidated) {
		t.Fatal("restart did not fence cleanup", err)
	}
	steps := 0
	for ; steps < 200; steps++ {
		done, err := s.summaryCleanupSlice(ctx, resumed, 1)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if steps == 200 || summaryRowCount(t, s) != 0 {
		t.Fatal("orphan cleanup did not finish")
	}
	var state int
	if err := s.reader.QueryRow(`SELECT state FROM aggregate_service_summary_hours`).Scan(&state); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cleanup marker remains", err)
	}
	stats, err := s.SweepIdentities([]SeriesID{1}, []uint32{105}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("identity cleanup after orphan expiry: %+v", stats)
}

func TestServiceSummaryDeferredSourceDoesNotStarve(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 48, 1)
	finishRollups(t, s)
	if _, err := s.writer.Exec(`INSERT INTO aggregate_rollup_hours VALUES (?,0,'',0)`, rollupTestHour-rollupHour); err != nil {
		t.Fatal(err)
	}
	w := summaryWorker{sourceAfter: math.MinInt64, summaryAfter: math.MinInt64}
	calls := 0
	deferred := errors.New("injected source-hour timeout")
	attempt := func(context.Context, int64, time.Duration) error { calls++; return deferred }
	active, err := s.summaryWorkerStep(context.Background(), time.Now(), &w, attempt)
	if !active || !errors.Is(err, deferred) || w.build == nil {
		t.Fatal("deferred source suppressed ready summary", active, err)
	}
	token := w.build.token
	for i := 0; i < 10 && w.build != nil; i++ {
		if _, err := s.summaryWorkerStep(context.Background(), time.Now(), &w, attempt); err != nil {
			t.Fatal(err)
		}
	}
	var state int
	var got string
	if err := s.reader.QueryRow(`SELECT state,build_id FROM aggregate_service_summary_hours WHERE window_start=?`, rollupTestHour).Scan(&state, &got); err != nil {
		t.Fatal(err)
	}
	if state != 2 || got != token || calls != 1 {
		t.Fatal("source failure starved or restarted summary", state, got, calls)
	}
}

func TestServiceSummarySelectorParity(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 48, 3)
	finishRollups(t, s)
	for h := 0; h < 3; h++ {
		finishSummary(t, s, rollupTestHour+int64(h)*rollupHour, 3)
	}
	for _, signals := range [][]Signal{{SignalTraceOp}, {SignalLog}, {SignalServiceEdge}, {SignalTraceOp, SignalLog}, {SignalMetric}, nil, {SignalTraceOp, SignalServiceEdge}} {
		for _, service := range [][]uint32{nil, {2, 3}, {0}} {
			for by := GroupBy(0); by < 16; by++ {
				for _, bounds := range [][2]int64{{0, 3 * rollupHour}, {300, 2*rollupHour + 600}} {
					sel := Selector{TenantID: 1, Start: rollupTestHour + bounds[0], End: rollupTestHour + bounds[1], Signals: signals, ServiceIDs: service}
					// Freeze the authoritative planner output by making only the new claims
					// unavailable. Existing hourly arithmetic remains the exact fallback.
					if _, err := s.writer.Exec(`UPDATE aggregate_service_summary_hours SET state=1`); err != nil {
						t.Fatal(err)
					}
					want, err := s.SumBuckets(context.Background(), sel, by)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.writer.Exec(`UPDATE aggregate_service_summary_hours SET state=2`); err != nil {
						t.Fatal(err)
					}
					got, err := s.SumBuckets(context.Background(), sel, by)
					if err != nil {
						t.Fatalf("signals=%v services=%v by=%d: %v", signals, service, by, err)
					}
					assertTrafficSums(t, got, want)
				}
			}
		}
	}
	// Unsupported selectors keep the authoritative path despite a corrupt
	// derived scalar. Limit does not cap the supported grouped result.
	if _, err := s.writer.Exec(`UPDATE aggregate_service_summary SET point_count=-99`); err != nil {
		t.Fatal(err)
	}
	base := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + rollupHour, Signal: SignalTraceOp}
	for _, change := range []func(*Selector){func(v *Selector) { v.SeriesIDs = []SeriesID{4} }, func(v *Selector) { v.SketchOnly = true }, func(v *Selector) { v.After = BucketCursor{WindowStart: rollupTestHour, SeriesID: 4} }} {
		sel := base
		change(&sel)
		got, err := s.SumBuckets(context.Background(), sel, GroupByWindow)
		if err != nil {
			t.Fatal(err)
		}
		assertTrafficSums(t, got, originalRollupSums(t, s, sel, GroupByWindow))
	}
}
func TestServiceSummarySketchQueryParity(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 48, 3)
	finishRollups(t, s)
	for h := 0; h < 3; h++ {
		finishSummary(t, s, rollupTestHour+int64(h)*rollupHour, 4)
	}
	for _, bounds := range [][2]int64{{0, 3 * rollupHour}, {300, 2*rollupHour + 600}} {
		for _, services := range [][]uint32{nil, {2}, {0}} {
			sel := Selector{TenantID: 1, Start: rollupTestHour + bounds[0], End: rollupTestHour + bounds[1], Signal: SignalTraceOp, ServiceIDs: services}
			want := originalRollupSketches(t, s, sel)
			got := map[uint32]*Sketch{}
			err := s.VisitSketches(context.Background(), sel, func(id uint32, sk *Sketch) error {
				if got[id] == nil {
					got[id] = NewSketch()
				}
				got[id].Merge(sk)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("sketch mismatch for %v %v", bounds, services)
			}
		}
	}
	// An unsafe summary forces the entire original chunk path; malformed safe
	// payloads remain errors instead of silently becoming absent.
	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + 3*rollupHour, Signal: SignalTraceOp}
	if _, err := s.writer.Exec(`UPDATE aggregate_service_summary SET sketch_state=2 WHERE signal=? AND resolution=3600`, SignalTraceOp); err != nil {
		t.Fatal(err)
	}
	got := map[uint32]*Sketch{}
	if err := s.VisitSketches(context.Background(), sel, func(id uint32, sk *Sketch) error {
		if got[id] == nil {
			got[id] = NewSketch()
		}
		got[id].Merge(sk)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, originalRollupSketches(t, s, sel)) {
		t.Fatal("unsafe fallback changed sketch")
	}
	if _, err := s.writer.Exec(`UPDATE aggregate_service_summary SET sketch_state=1,sketch=x'00' WHERE signal=? AND resolution=3600`, SignalTraceOp); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitSketches(context.Background(), sel, func(uint32, *Sketch) error { return nil }); err == nil {
		t.Fatal("corrupt payload accepted")
	}
}
func TestServiceSummaryScalarArithmetic(t *testing.T) {
	s := rollupTestStore(t)
	ctx := context.Background()
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	keys := []SeriesRow{{ID: 1, Key: storeKey(100)}, {ID: 2, Key: storeKey(101)}}
	seedSeries(t, tx, keys)
	d := spanDelta(1, 5)
	d.Count = (1 << 53) + 3
	d.RequestCount = d.Count
	d.DurationCount = 0
	d.DurationSum = 7.25
	insertRows(t, tx, "aggregate_buckets", []SeriesID{1, 2}, []int64{rollupTestHour, rollupTestHour}, []*AggregateDelta{d, {}})
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	finishSummary(t, s, rollupTestHour, 1)
	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + rollupHour, Signal: SignalTraceOp}
	for _, by := range []GroupBy{0, GroupByWindow, GroupByService, GroupByService | GroupBySignal} {
		got, err := s.SumBuckets(ctx, sel, by)
		if err != nil {
			t.Fatal(err)
		}
		assertTrafficSums(t, got, originalRollupSums(t, s, sel, by))
	}
	// Individually safe persisted cells plus a delta must still raise SQLite's
	// signed SUM overflow, not overflow a Go merge between separate sources.
	// The fractional source above now deliberately falls back. Rebuild with an
	// exact integral source before testing the accelerated SQL's overflow path.
	if _, err := s.writer.Exec(`UPDATE aggregate_buckets SET duration_sum=7`); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	finishSummary(t, s, rollupTestHour, 1)
	if got, used, err := readSummarySums(ctx, s.reader, sel, GroupByWindow); err != nil || !used || got[0].Count != d.Count {
		t.Fatal("exact counter above 2^53 did not use summary", got, used, err)
	}
	if _, err := s.writer.Exec(`UPDATE aggregate_service_summary SET point_count=?`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitGroup(&GroupBatch{Deltas: []DeltaRow{{SeriesID: 1, WindowStart: rollupTestHour, Delta: &AggregateDelta{Count: 1}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SumBuckets(ctx, sel, GroupByWindow); err == nil || !strings.Contains(err.Error(), "integer overflow") {
		t.Fatal("summary plus delta overflow not preserved", err)
	}
	// Builder arithmetic rejects negative or unrepresentable derived totals.
	for _, v := range []summaryScalar{{counts: [7]int64{-1}}, {counts: [7]int64{math.MaxInt64}}} {
		a := summaryScalar{counts: [7]int64{1}}
		if err := a.add(v); err == nil {
			t.Fatal("accepted unsafe scalar")
		}
	}
}
func TestServiceSummaryReadPlans(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 8, 1)
	finishRollups(t, s)
	finishSummary(t, s, rollupTestHour, 2)
	for _, services := range [][]uint32{nil, {2, 3}} {
		for _, by := range []GroupBy{GroupByWindow, GroupByService} {
			resolution := int64(300)
			if by == GroupByService {
				resolution = 3600
			}
			q, args := summarySumQuery(Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + rollupHour, Signal: SignalTraceOp, ServiceIDs: services}, by, resolution)
			rows, err := s.reader.Query(`EXPLAIN QUERY PLAN `+q, args...)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for rows.Next() {
				var a, b, c int
				var d string
				if err := rows.Scan(&a, &b, &c, &d); err != nil {
					t.Fatal(err)
				}
				t.Log(d)
				if strings.Contains(d, "SEARCH t USING PRIMARY KEY (hour_start=? AND resolution=? AND tenant_id=? AND signal=?") {
					found = true
					if services != nil && !strings.Contains(d, "service_id=?") {
						t.Fatal("missing service prefix", d)
					}
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if !found {
				t.Fatal("summary prefix not used")
			}
		}
	}
}

// Wrap only the test reader's real SQLite driver. The endpoint still sees the
// concrete SQLiteStore, and pauses at EOF of its first scalar statement.
type summaryTestConnector struct {
	base  driver.Driver
	path  string
	once  sync.Once
	after func()
	match string
}

func (c *summaryTestConnector) Driver() driver.Driver { return c.base }
func (c *summaryTestConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.path)
	if err != nil {
		return nil, err
	}
	return &summaryTestConn{Conn: conn, c: c}, nil
}

type summaryTestConn struct {
	driver.Conn
	c *summaryTestConnector
}

func (c *summaryTestConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}
func (c *summaryTestConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	if strings.Contains(q, c.c.match) {
		return &summaryTestRows{Rows: rows, c: c.c}, nil
	}
	return rows, nil
}

type summaryTestRows struct {
	driver.Rows
	c *summaryTestConnector
}

func (r *summaryTestRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == io.EOF {
		r.c.once.Do(r.c.after)
	}
	return err
}
func interceptSummaryPhase(t *testing.T, s *SQLiteStore, after func()) {
	t.Helper()
	original := s.reader
	s.reader = sql.OpenDB(&summaryTestConnector{base: original.Driver(), path: s.path, after: after, match: "COALESCE(SUM(point_count)"})
	s.reader.SetMaxOpenConns(1)
	t.Cleanup(func() { s.reader.Close(); s.reader = original })
}
func summaryEndpointFixture(t *testing.T) (*Engine, *SQLiteStore, Query, []uint32) {
	t.Helper()
	s := rollupTestStore(t)
	e := testEngine(t, time.Unix(rollupTestHour+10*rollupHour, 0))
	tenant, _ := e.TenantID("default")
	service := e.cache.Intern(tenant, KindService, "caller")
	op := e.cache.Intern(tenant, KindOperation, "op")
	callee := e.cache.Intern(tenant, KindOperation, "callee-label")
	dict := []DictRow{{ID: service, TenantID: tenant, Kind: KindService, Value: []byte("caller")}, {ID: op, TenantID: tenant, Kind: KindOperation, Value: []byte("op")}, {ID: callee, TenantID: tenant, Kind: KindOperation, Value: []byte("callee-label")}}
	if err := s.CommitGroup(&GroupBatch{Dicts: dict, Series: []SeriesRow{{ID: 1, Key: SeriesKey{TenantID: tenant, ServiceID: service, NameID: op, Signal: SignalTraceOp}}, {ID: 2, Key: SeriesKey{TenantID: tenant, ServiceID: service, NameID: callee, Signal: SignalServiceEdge}}}, Deltas: []DeltaRow{{SeriesID: 1, WindowStart: rollupTestHour, Delta: spanDelta(1, 100)}, {SeriesID: 2, WindowStart: rollupTestHour, Delta: spanDelta(1, 100)}}}); err != nil {
		t.Fatal(err)
	}
	e.SetStore(s)
	q := Query{Tenant: "default", Start: time.Unix(rollupTestHour, 0), End: time.Unix(rollupTestHour+300, 0)}
	return e, s, q, []uint32{service, op, callee}
}
func TestServiceSummaryEndpointPhaseSnapshot(t *testing.T) {
	for _, endpoint := range []string{"topology", "dashboard"} {
		t.Run(endpoint, func(t *testing.T) {
			e, s, q, _ := summaryEndpointFixture(t)
			interceptSummaryPhase(t, s, func() {
				if err := s.CommitGroup(&GroupBatch{Deltas: []DeltaRow{{SeriesID: 1, WindowStart: rollupTestHour, Delta: spanDelta(1, 900)}, {SeriesID: 2, WindowStart: rollupTestHour, Delta: spanDelta(1, 900)}}}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.FinalizeWindow(rollupTestHour); err != nil {
					t.Fatal(err)
				}
			})
			for _, want := range []int64{1, 2} {
				if endpoint == "topology" {
					got, err := e.QueryTopology(context.Background(), q)
					if err != nil {
						t.Fatal(err)
					}
					if len(got.Nodes) != 1 || len(got.Edges) != 1 {
						t.Fatalf("shape: %+v", got)
					}
					node, edge := got.Nodes[0], got.Edges[0]
					if node.Count != want || node.LatencyProvenance.P99.SampleCount != uint64(want) || edge.CallCount != want {
						t.Fatalf("mixed endpoint snapshot: node=%d sketch=%d edge=%d want each %d", node.Count, node.LatencyProvenance.P99.SampleCount, edge.CallCount, want)
					}
				} else {
					got, err := e.QueryDashboard(context.Background(), q)
					if err != nil {
						t.Fatal(err)
					}
					if got.SpanCount != want || got.LatencyProvenance.P99.SampleCount != uint64(want) {
						t.Fatalf("mixed dashboard snapshot: scalar=%d sketch=%d want each %d", got.SpanCount, got.LatencyProvenance.P99.SampleCount, want)
					}
				}
			}
			if s.reader.Stats().InUse != 0 {
				t.Fatal("endpoint leaked reader")
			}
		})
	}
}
func TestServiceSummaryEndpointNamesSurviveGC(t *testing.T) {
	e, s, q, ids := summaryEndpointFixture(t)
	interceptSummaryPhase(t, s, func() {
		// The source is expired, with no mutable owner, baseline or remaining
		// authoritative reference. Complete the ordinary SQL/cache sweep steps.
		if _, _, err := purgeWindow(s.writer, rollupTestHour); err != nil {
			t.Fatal(err)
		}
		stats, err := s.SweepIdentities([]SeriesID{1, 2}, ids, nil)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Series != 2 || stats.Dict != 3 {
			t.Fatal("GC incomplete", stats)
		}
		forgotten := make(map[uint32]struct{})
		for _, id := range ids {
			forgotten[id] = struct{}{}
		}
		e.cache.Forget(forgotten)
	})
	got, err := e.QueryTopology(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].Service != "caller" || len(got.Edges) != 1 || got.Edges[0].Target != "callee-label" {
		t.Fatalf("snapshot lost durable names: %+v", got)
	}
	later, err := e.QueryTopology(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(later.Nodes) != 0 || len(later.Edges) != 0 {
		t.Fatal("later snapshot resurrected expired data")
	}
}

func TestServiceSummarySchemaAndRetention(t *testing.T) {
	for _, change := range []string{"none", "data", "marker", "uuid", "trigger"} {
		t.Run(change, func(t *testing.T) {
			s := rollupTestStore(t)
			populateRollupFixture(t, s, 16, 2)
			finishRollups(t, s)
			finishSummary(t, s, rollupTestHour, 3)
			uuid := s.UUID()
			var q string
			switch change {
			case "data":
				q = `DROP TABLE aggregate_service_summary`
			case "marker":
				q = `DROP TABLE aggregate_service_summary_hours`
			case "uuid":
				q = `UPDATE aggregate_rollup_meta SET value='old' WHERE key='service_summary_store_uuid'`
			case "trigger":
				q = `DROP TRIGGER aggregate_rollup_update`
			}
			if q != "" {
				if _, err := s.writer.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			path := s.Path()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSQLiteStore(StoreConfig{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			reopened.stopRollups()
			if reopened.UUID() != uuid {
				t.Fatal("extension rebuilt authoritative database")
			}
			ready, err := summaryReady(context.Background(), reopened.reader, rollupTestHour)
			if err != nil {
				t.Fatal(err)
			}
			if ready != (change == "none") {
				t.Fatal("wrong completion after reopen", change, ready)
			}
			if _, err := reopened.PurgeBefore(rollupTestHour + 300); err != nil {
				t.Fatal(err)
			}
			if ready, err := summaryReady(context.Background(), reopened.reader, rollupTestHour); err != nil || ready {
				t.Fatal("purge left complete stale summary", ready, err)
			}
			if n := summaryRowCount(t, reopened); n != 0 {
				t.Fatal("boundary summary rows retained", n)
			}
			finishRollups(t, reopened)
			finishSummary(t, reopened, rollupTestHour, 3)
			if _, err := reopened.PurgeBefore(rollupTestHour + 300); err != nil {
				t.Fatal(err)
			}
			if ready, err := summaryReady(context.Background(), reopened.reader, rollupTestHour); err != nil || !ready {
				t.Fatal("repeat purge invalidated retained generation", ready, err)
			}
			if _, err := reopened.PurgeBefore(rollupTestHour + 2*rollupHour); err != nil {
				t.Fatal(err)
			}
			if summaryRowCount(t, reopened) != 0 {
				t.Fatal("expiry left orphan summaries")
			}
		})
	}
	t.Run("extension_error_no_rebuild", func(t *testing.T) {
		s := rollupTestStore(t)
		populateRollupFixture(t, s, 2, 1)
		uuid := s.UUID()
		if _, err := s.writer.Exec(`DROP TABLE aggregate_service_summary; CREATE VIEW aggregate_service_summary AS SELECT 1`); err != nil {
			t.Fatal(err)
		}
		path := s.Path()
		s.Close()
		opened, err := OpenSQLiteStore(StoreConfig{Path: path, AllowRebuild: true})
		if err == nil {
			opened.Close()
			t.Fatal("accepted conflicting extension")
		}
		db, err := openAggregateDB(path, "NORMAL", 1024, false)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var got string
		var n int
		if err := db.QueryRow(`SELECT value FROM aggregate_meta WHERE key='store_uuid'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM aggregate_buckets`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if got != uuid || n == 0 {
			t.Fatal("extension failure rebuilt history")
		}
	})
}
func TestServiceSummaryWideSketchAndUnsafeFallback(t *testing.T) {
	for _, n := range []int{257, 512} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s := rollupTestStore(t)
			tx, err := s.writer.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			seedSeries(t, tx, []SeriesRow{{ID: 1, Key: storeKey(100)}, {ID: 2, Key: storeKey(101)}})
			a, b := NewSketch(), NewSketch()
			for i := 0; i < n; i++ {
				sk := a
				if i >= 256 {
					sk = b
				}
				sk.add(int32(i-200), 1)
				sk.count++
			}
			a.sum, b.sum = 123.456789, 234.567891
			insertRows(t, tx, "aggregate_buckets", []SeriesID{1, 2}, []int64{rollupTestHour, rollupTestHour}, []*AggregateDelta{{Count: a.count, DurationCount: a.count, DurationSum: a.sum, Sketch: a}, {Count: b.count, DurationCount: b.count, DurationSum: b.sum, Sketch: b}})
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			finishRollups(t, s)
			finishSummary(t, s, rollupTestHour, 1)
			var high []byte
			if err := s.reader.QueryRow(`SELECT sketch_high FROM aggregate_service_summary WHERE resolution=3600 AND signal=?`, SignalTraceOp).Scan(&high); err != nil {
				t.Fatal(err)
			}
			if high == nil {
				t.Fatal("wide sketch did not use two blobs")
			}
			sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + rollupHour, Signal: SignalTraceOp}
			want := originalRollupSketches(t, s, sel)
			got := map[uint32]*Sketch{}
			if err := s.VisitSketches(context.Background(), sel, func(id uint32, sk *Sketch) error {
				if got[id] == nil {
					got[id] = NewSketch()
				}
				got[id].Merge(sk)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatal("wide sketch query changed bins/count/sum")
			}
		})
	}
}
func TestServiceSummaryEndpointCancellationAndFilter(t *testing.T) {
	e, s, q, _ := summaryEndpointFixture(t)
	q.Services = []string{"caller", "callee-label"}
	got, err := e.QueryTopology(context.Background(), q)
	if err != nil || len(got.Nodes) != 1 || len(got.Edges) != 1 {
		t.Fatal("durable named filter", got, err)
	}
	q.Services = []string{"unknown"}
	empty, err := e.QueryBuckets(context.Background(), q)
	if err != nil || len(empty.Points) != 0 {
		t.Fatal("unmatched filter became unfiltered", empty, err)
	}
	s.reader.SetMaxOpenConns(1)
	held, err := s.reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.QueryDashboard(ctx, q); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled pool wait", err)
	}
	held.Rollback()
	ctx, cancel = context.WithCancel(context.Background())
	interceptSummaryPhase(t, s, cancel)
	if _, err := e.QueryTopology(ctx, q); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled pinned snapshot", err)
	}
	// database/sql may still be returning the connection from its cancellation
	// goroutine after the endpoint's Rollback sees ErrTxDone.
	pingCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := s.reader.PingContext(pingCtx); err != nil || s.reader.Stats().InUse != 0 {
		t.Fatalf("cancelled endpoint leaked reader: %v %+v", err, s.reader.Stats())
	}
}

type summaryCommitConnector struct {
	base    driver.Driver
	path    string
	failure error
	once    sync.Once
}

func (c *summaryCommitConnector) Driver() driver.Driver { return c.base }
func (c *summaryCommitConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.path + "?_txlock=immediate")
	if err != nil {
		return nil, err
	}
	return &summaryCommitConn{Conn: conn, c: c}, nil
}

type summaryCommitConn struct {
	driver.Conn
	c *summaryCommitConnector
}

func (c *summaryCommitConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &summaryCommitTx{Tx: tx, c: c.c}, nil
}

type summaryCommitTx struct {
	driver.Tx
	c *summaryCommitConnector
}

func (tx *summaryCommitTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	var err error
	tx.c.once.Do(func() { err = tx.c.failure })
	return err
}
func TestServiceSummaryUncertainOutputCommit(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 48, 1)
	finishRollups(t, s)
	ctx := context.Background()
	c, err := s.claimSummaryBuild(ctx, rollupTestHour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.summaryBuildSlice(ctx, c, 128); err != nil {
		t.Fatal(err)
	}
	original := s.writer
	uncertain := errors.New("injected committed-but-unacknowledged output")
	s.writer = sql.OpenDB(&summaryCommitConnector{base: original.Driver(), path: s.path, failure: uncertain})
	s.writer.SetMaxOpenConns(1)
	_, err = s.summaryBuildSlice(ctx, c, 128)
	s.writer.Close()
	s.writer = original
	if !errors.Is(err, uncertain) || !c.failed || summaryRowCount(t, s) == 0 {
		t.Fatal("did not exercise ambiguous committed output", err, c.failed)
	}
	replacement := finishSummary(t, s, rollupTestHour, 3)
	if replacement.token == c.token {
		t.Fatal("uncertain generation reused")
	}
	var got, want int64
	if err := s.reader.QueryRow(`SELECT SUM(point_count) FROM aggregate_service_summary WHERE resolution=300`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if err := s.reader.QueryRow(`SELECT SUM(b.point_count) FROM aggregate_buckets b JOIN aggregate_series s ON s.id=b.series_id WHERE s.signal=?`, SignalTraceOp).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("ambiguous commit doubled contributions", got, want)
	}
}
func TestServiceSummarySharedNameAndIdentityCoverage(t *testing.T) {
	s := rollupTestStore(t)
	ctx := context.Background()
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO aggregate_dict VALUES (2,1,?,?),(3,1,?,?),(4,1,?,?)`, KindService, []byte(OtherValue), KindOperation, []byte(OtherValue), KindDimTuple, []byte(OtherValue)); err != nil {
		t.Fatal(err)
	}
	var series []SeriesRow
	var ids []SeriesID
	var windows []int64
	var deltas []*AggregateDelta
	for i := 1; i <= 8; i++ {
		key := storeKey(100)
		key.Signal = SignalServiceEdge
		key.ServiceID = 8
		key.NameID = 9
		key.DimsID = uint32(20 + i)
		if i&1 != 0 {
			key.ServiceID = 2
		}
		if i&2 != 0 {
			key.NameID = 3
		}
		if i&4 != 0 {
			key.DimsID = 4
		}
		key.HTTPClass = HTTPClass(i)
		series = append(series, SeriesRow{ID: SeriesID(i), Key: key})
		ids = append(ids, SeriesID(i))
		windows = append(windows, rollupTestHour)
		deltas = append(deltas, spanDelta(i, 100))
	}
	// Two additional edge variants share the full collapsed caller/name group.
	for i := 9; i <= 10; i++ {
		key := storeKey(100)
		key.Signal = SignalServiceEdge
		key.ServiceID = 8
		key.NameID = 9
		key.DimsID = uint32(i + 20)
		series = append(series, SeriesRow{ID: SeriesID(i), Key: key})
		ids = append(ids, SeriesID(i))
		windows = append(windows, rollupTestHour)
		deltas = append(deltas, spanDelta(i, 100))
	}
	seedSeries(t, tx, series)
	insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	finishSummary(t, s, rollupTestHour, 1)
	// Repeat every sentinel overlap through the delta join, including all
	// three identities being Other; a point must still count only once.
	tx, err = s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insertRows(t, tx, "aggregate_delta_log", ids, windows, deltas)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + rollupHour, Signal: SignalServiceEdge}
	for _, by := range []GroupBy{0, GroupByService, GroupByService | GroupByName | GroupBySignal} {
		got, err := s.SumBuckets(ctx, sel, by)
		if err != nil {
			t.Fatal(err)
		}
		assertTrafficSums(t, got, originalRollupSums(t, s, sel, by))
		var identity uint64
		for _, row := range got {
			identity += row.IdentityCount
		}
		if identity != 56 {
			t.Fatal("sentinel overlap counted more than once", identity)
		}
	}
}
func TestServiceSummaryRejectsNoncanonicalSource(t *testing.T) {
	s := rollupTestStore(t)
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	key := storeKey(100)
	key.Signal = SignalLog
	seedSeries(t, tx, []SeriesRow{{ID: 1, Key: key}})
	insertRows(t, tx, "aggregate_buckets", []SeriesID{1}, []int64{rollupTestHour + 1}, []*AggregateDelta{{Count: 1, LogCount: 1}})
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	finishRollups(t, s)
	c, err := s.claimSummaryBuild(context.Background(), rollupTestHour)
	if err != nil {
		t.Fatal(err)
	}
	s.summaryBuildSlice(context.Background(), c, 1)
	if _, err := s.summaryBuildSlice(context.Background(), c, 1); err == nil || !strings.Contains(err.Error(), "noncanonical") {
		t.Fatal("off-grid log window published", err)
	}
	rows, err := s.reader.Query(`EXPLAIN QUERY PLAN SELECT window_start FROM aggregate_buckets WHERE window_start>? AND window_start<? LIMIT 1`, rollupTestHour, rollupTestHour+300)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b, c int
		var d string
		rows.Scan(&a, &b, &c, &d)
		t.Log(d)
		if !strings.Contains(d, "SEARCH aggregate_buckets USING PRIMARY KEY (window_start>? AND window_start<?)") {
			t.Fatal("canonical-window proof would scan source", d)
		}
	}
}

func TestServiceSummaryCleanupFencesPublication(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 48, 1)
	finishRollups(t, s)
	ctx := context.Background()
	c, err := s.claimSummaryBuild(ctx, rollupTestHour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.summaryBuildSlice(ctx, c, 128); err != nil {
			t.Fatal(err)
		}
	}
	if summaryRowCount(t, s) == 0 {
		t.Fatal("missing partial output")
	}
	original := s.reader
	var cleanup *summaryCleanup
	s.reader = sql.OpenDB(&summaryTestConnector{base: original.Driver(), path: s.path, match: "SELECT id,tenant_id", after: func() {
		var err error
		cleanup, err = s.claimSummaryCleanup(ctx, rollupTestHour, rollupTestHour+300)
		if err != nil {
			t.Fatal(err)
		}
	}})
	s.reader.SetMaxOpenConns(1)
	_, err = s.summaryBuildSlice(ctx, c, 128)
	s.reader.Close()
	s.reader = original
	if cleanup == nil || !errors.Is(err, errRollupInvalidated) {
		t.Fatal("cleanup did not fence publication after source snapshot", err)
	}
	var state int
	var token string
	if err := s.reader.QueryRow(`SELECT state,build_id FROM aggregate_service_summary_hours WHERE window_start=?`, rollupTestHour).Scan(&state, &token); err != nil {
		t.Fatal(err)
	}
	if state != 3 || token != cleanup.token {
		t.Fatal("builder overwrote cleanup claim")
	}
}
func TestServiceSummaryRestartIncompleteStates(t *testing.T) {
	for _, phase := range []string{"building", "cleanup"} {
		t.Run(phase, func(t *testing.T) {
			s := rollupTestStore(t)
			populateRollupFixture(t, s, 48, 1)
			finishRollups(t, s)
			ctx := context.Background()
			c, err := s.claimSummaryBuild(ctx, rollupTestHour)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if _, err := s.summaryBuildSlice(ctx, c, 128); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "cleanup" {
				cleanup, err := s.claimSummaryCleanup(ctx, rollupTestHour, rollupTestHour+300)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.summaryCleanupSlice(ctx, cleanup, 1); err != nil {
					t.Fatal(err)
				}
			}
			path := s.Path()
			s.Close()
			reopened, err := OpenSQLiteStore(StoreConfig{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			reopened.stopRollups()
			if ready, err := summaryReady(ctx, reopened.reader, rollupTestHour); err != nil || ready {
				t.Fatal("restart exposed incomplete generation", ready, err)
			}
			finishSummary(t, reopened, rollupTestHour, 2)
			sel := Selector{TenantID: 1, Signal: SignalTraceOp, Start: rollupTestHour, End: rollupTestHour + rollupHour}
			got, err := reopened.SumBuckets(ctx, sel, GroupByWindow)
			if err != nil {
				t.Fatal(err)
			}
			assertTrafficSums(t, got, originalRollupSums(t, reopened, sel, GroupByWindow))
		})
	}
}
func TestServiceSummaryLateDataAndSelectorLimit(t *testing.T) {
	s := rollupTestStore(t)
	populateRollupFixture(t, s, 16, 1)
	finishRollups(t, s)
	finishSummary(t, s, rollupTestHour, 3)
	if err := s.CommitGroup(&GroupBatch{Deltas: []DeltaRow{{SeriesID: 4, WindowStart: rollupTestHour, Delta: spanDelta(3, 333)}, {SeriesID: 1, WindowStart: rollupTestHour, Delta: spanDelta(2, 444)}}}); err != nil {
		t.Fatal(err)
	}
	for _, signal := range []Signal{SignalTraceOp, SignalServiceEdge} {
		for _, by := range []GroupBy{GroupByWindow, GroupByService} {
			sel := Selector{TenantID: 1, Signal: signal, Start: rollupTestHour, End: rollupTestHour + rollupHour, Limit: 1}
			got, err := s.SumBuckets(context.Background(), sel, by)
			if err != nil {
				t.Fatal(err)
			}
			assertTrafficSums(t, got, originalRollupSums(t, s, sel, by))
		}
	}
	sel := Selector{TenantID: 1, Signal: SignalTraceOp, Start: rollupTestHour, End: rollupTestHour + rollupHour, ServiceIDs: make([]uint32, MaxReadRows)}
	for i := range sel.ServiceIDs {
		sel.ServiceIDs[i] = uint32(i + 1)
	}
	_, args := summarySumQuery(sel, GroupByWindow, 300)
	if len(args) > MaxReadRows+20 {
		t.Fatal("selector bindings duplicated", len(args))
	}
	got, err := s.SumBuckets(context.Background(), sel, GroupByWindow)
	if err != nil {
		t.Fatal(err)
	}
	sel.ServiceIDs = nil
	assertTrafficSums(t, got, originalRollupSums(t, s, sel, GroupByWindow))
	if _, err := s.FinalizeWindow(rollupTestHour); err != nil {
		t.Fatal(err)
	}
	if ready, err := summaryReady(context.Background(), s.reader, rollupTestHour); err != nil || ready {
		t.Fatal("late finalize left old summary eligible", ready, err)
	}
	finishRollups(t, s)
	finishSummary(t, s, rollupTestHour, 3)
	if _, err := s.FinalizeWindow(rollupTestHour); err != nil {
		t.Fatal(err)
	}
	if ready, err := summaryReady(context.Background(), s.reader, rollupTestHour); err != nil || !ready {
		t.Fatal("no-op finalize invalidated summary", ready, err)
	}
}

func TestServiceSummarySketchFallbackProvenance(t *testing.T) {
	for _, kind := range []string{"cross-service", "cross-hour", "scale", "collapse", "saturation", "deficit", "empty", "absent"} {
		t.Run(kind, func(t *testing.T) {
			s := rollupTestStore(t)
			tx, err := s.writer.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			key1, key2 := storeKey(100), storeKey(101)
			if kind == "cross-service" {
				key2.ServiceID = 9
			}
			seedSeries(t, tx, []SeriesRow{{ID: 1, Key: key1}, {ID: 2, Key: key2}})
			a, b := NewSketch(), NewSketch()
			a.ObserveN(100, 2_500_000_000)
			b.ObserveN(100, 2_500_000_000)
			switch kind {
			case "scale":
				a, _ = NewSketchAtScale(3)
				a.Observe(100)
			case "collapse":
				a.collapsed = true
			case "saturation":
				a.ObserveN(100, 2_500_000_000)
			case "deficit":
				a.count++
			case "empty":
				a, b = NewSketch(), NewSketch()
			case "absent":
				a, b = nil, nil
			}
			hour2 := rollupTestHour
			if kind == "cross-hour" {
				hour2 += rollupHour
			}
			da, db := &AggregateDelta{Count: 1, Sketch: a}, &AggregateDelta{Count: 1, Sketch: b}
			insertRows(t, tx, "aggregate_buckets", []SeriesID{1, 2}, []int64{rollupTestHour, hour2}, []*AggregateDelta{da, db})
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			finishRollups(t, s)
			finishSummary(t, s, rollupTestHour, 1)
			if hour2 != rollupTestHour {
				finishSummary(t, s, hour2, 1)
			}
			sel := Selector{TenantID: 1, Start: rollupTestHour, End: hour2 + rollupHour, Signal: SignalTraceOp}
			collect := func(read func(func(uint32, *Sketch) error) error) (map[uint32]*Sketch, *Sketch) {
				m := map[uint32]*Sketch{}
				whole := NewSketch()
				if err := read(func(id uint32, sk *Sketch) error {
					if m[id] == nil {
						m[id] = NewSketchAtScaleUnchecked(sk.Scale())
					}
					m[id].Merge(sk)
					whole.Merge(sk)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return m, whole
			}
			want, wholeWant := collect(func(visit func(uint32, *Sketch) error) error {
				return visitSQLiteSketches(context.Background(), s.reader, sel, visit)
			})
			got, wholeGot := collect(func(visit func(uint32, *Sketch) error) error {
				return s.VisitSketches(context.Background(), sel, visit)
			})
			if !reflect.DeepEqual(want, got) || !reflect.DeepEqual(wholeWant, wholeGot) {
				t.Fatalf("%s changed fallback bins or provenance", kind)
			}
			rejected := errors.New("visitor rejected sample")
			if kind != "absent" {
				if err := s.VisitSketches(context.Background(), sel, func(uint32, *Sketch) error { return rejected }); !errors.Is(err, rejected) {
					t.Fatal("visitor error lost", err)
				}
			}
			if s.reader.Stats().InUse != 0 {
				t.Fatal("visitor leaked snapshot")
			}
		})
	}
}
func TestServiceSummaryEndpointPoolWait(t *testing.T) {
	e, s, q, _ := summaryEndpointFixture(t)
	s.reader.SetMaxOpenConns(1)
	held, err := s.reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	before := s.reader.Stats().WaitCount
	go func() { _, err := e.QueryTopology(ctx, q); done <- err }()
	deadline := time.Now().Add(time.Second)
	for s.reader.Stats().WaitCount == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.reader.Stats().WaitCount == before {
		t.Fatal("endpoint never waited for the reader")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("endpoint did not cancel pool wait")
	}
	if s.reader.Stats().InUse != 1 {
		t.Fatal("waiter leaked connection")
	}
	held.Rollback()
	if s.reader.Stats().InUse != 0 {
		t.Fatal("held connection did not release")
	}
}

func TestServiceSummarySketchRejectsDisjointCorruption(t *testing.T) {
	low, high := NewSketch(), NewSketch()
	low.add(math.MinInt32+1, 1)
	low.count = 1
	high.add(math.MaxInt32-1, 1)
	high.count = 1
	if _, err := decodeSummarySketch(low.Encode(), high.Encode()); err == nil {
		t.Fatal("accepted overwide two-part payload")
	}
	high.maxIdx = math.MaxInt32
	if _, _, ok := encodeSummarySketch(high); ok {
		t.Fatal("accepted index whose codec loop overflows")
	}
	s := NewSketch()
	for i := 0; i < 257; i++ {
		s.add(int32(i), 1)
		s.count++
	}
	s.sum = math.Copysign(0, -1)
	a, b, ok := encodeSummarySketch(s)
	if !ok {
		t.Fatal("safe negative-zero sketch refused")
	}
	got, err := decodeSummarySketch(a, b)
	if err != nil || math.Float64bits(got.Sum()) != math.Float64bits(s.Sum()) {
		t.Fatal("changed exact Sum bits", err)
	}
}
