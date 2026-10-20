package aggregate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"
)

type summaryFitnessQuery struct {
	label, sql string
	args       []any
}
type summaryFitnessConnector struct {
	base        driver.Driver
	path, label string
	queries     []summaryFitnessQuery
	original    *sql.DB
}

func (c *summaryFitnessConnector) Driver() driver.Driver { return c.base }
func (c *summaryFitnessConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.path)
	if err != nil {
		return nil, err
	}
	return &summaryFitnessConn{Conn: conn, c: c}, nil
}

type summaryFitnessConn struct {
	driver.Conn
	c *summaryFitnessConnector
}

func (c *summaryFitnessConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}
func (c *summaryFitnessConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	values := make([]any, len(args))
	for i, v := range args {
		values[i] = v.Value
	}
	c.c.queries = append(c.c.queries, summaryFitnessQuery{c.c.label, q, values})
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func captureSummaryFitness(t *testing.T, s *SQLiteStore) *summaryFitnessConnector {
	t.Helper()
	original := s.reader
	c := &summaryFitnessConnector{base: original.Driver(), path: s.path, original: original}
	reader := sql.OpenDB(c)
	s.reader = reader
	reader.SetMaxOpenConns(1)
	t.Cleanup(func() {
		reader.Close()
		if s.reader == reader {
			s.reader = original
		}
	})
	return c
}
func (c *summaryFitnessConnector) cells(label string, sketch bool) int {
	n := 0
	for _, q := range c.queries {
		if q.label == label && strings.Contains(q.sql, "FROM aggregate_service_summary ") && strings.Contains(q.sql, "sketch_high") == sketch {
			n++
		}
	}
	return n
}

// Same duration mechanism as aggprefill, with only six identities, four hours
// and three observations per canonical bucket. The source generator is intact.
func fractionalSummaryFixture(t *testing.T, late bool) (*Engine, *SQLiteStore, Query, []uint32) {
	t.Helper()
	s := rollupTestStore(t)
	e := testEngine(t, time.Unix(rollupTestHour+10*rollupHour, 0))
	tenant, _ := e.TenantID("default")
	var dict []DictRow
	intern := func(kind Kind, name string) uint32 {
		id := e.cache.Intern(tenant, kind, name)
		dict = append(dict, DictRow{ID: id, TenantID: tenant, Kind: kind, Value: []byte(name)})
		return id
	}
	services := []uint32{intern(KindService, "fraction-a"), intern(KindService, "fraction-b")}
	var series []SeriesRow
	for i, service := range services {
		for op := 0; op < 2; op++ {
			name := intern(KindOperation, []string{"op-one", "op-two"}[op]+string(rune('a'+i)))
			series = append(series, SeriesRow{ID: SeriesID(len(series) + 1), Key: SeriesKey{TenantID: tenant, ServiceID: service, NameID: name, Signal: SignalTraceOp}})
		}
		name := intern(KindOperation, []string{"fraction-b", "fraction-a"}[i])
		series = append(series, SeriesRow{ID: SeriesID(len(series) + 1), Key: SeriesKey{TenantID: tenant, ServiceID: service, NameID: name, Signal: SignalServiceEdge}})
	}
	if err := s.CommitGroup(&GroupBatch{Dicts: dict, Series: series}); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(17, 29))
	observe := func(i int, trace bool) *AggregateDelta {
		d := &AggregateDelta{}
		mu, sigma := math.Log(float64(100+i*5)), 0.12
		for k := 0; k < 3; k++ {
			v := math.Exp(mu + sigma*rng.NormFloat64())
			if v < 1 {
				v = 1
			} else if v > 60e6 {
				v = 60e6
			}
			d.ObserveSpan(v, k == i%3, trace)
		}
		if d.DurationCount != 3 || math.Trunc(d.DurationSum) == d.DurationSum {
			t.Fatal("fixture did not retain fractional durations", d)
		}
		return d
	}
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var ids []SeriesID
	var windows []int64
	var deltas []*AggregateDelta
	for w := 0; w < 48; w++ {
		for i, row := range series {
			ids = append(ids, row.ID)
			windows = append(windows, rollupTestHour+int64(w)*300)
			deltas = append(deltas, observe(i, row.Key.Signal == SignalTraceOp))
		}
	}
	insertRows(t, tx, "aggregate_buckets", ids, windows, deltas)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Exercise the real worker step, source builder and summary continuation.
	if err := s.discoverRollupHours(context.Background()); err != nil {
		t.Fatal(err)
	}
	worker := summaryWorker{}
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("tiny fractional worker did not finish")
		}
		active, err := s.summaryWorkerStep(context.Background(), time.Now(), &worker, s.buildRollupHour)
		if err != nil {
			t.Fatal(err)
		}
		if !active {
			break
		}
	}
	var rejected int
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_service_summary_hours WHERE state=4`).Scan(&rejected); err != nil || rejected != 4 {
		t.Fatal("arithmetic rejection not exercised", rejected, err)
	}
	if late {
		var rows []DeltaRow
		for h := 0; h < 4; h++ {
			for i, row := range series {
				rows = append(rows, DeltaRow{SeriesID: row.ID, WindowStart: rollupTestHour + int64(h)*rollupHour + 300, Delta: observe(i, row.Key.Signal == SignalTraceOp)})
			}
		}
		if err := s.CommitGroup(&GroupBatch{Deltas: rows}); err != nil {
			t.Fatal(err)
		}
	}
	e.SetStore(s)
	return e, s, Query{Tenant: "default", Start: time.Unix(rollupTestHour, 0), End: time.Unix(rollupTestHour+4*rollupHour, 0)}, services
}

func TestServiceSummaryFractionalFitness(t *testing.T) {
	for _, late := range []bool{false, true} {
		label := "source"
		if late {
			label = "late-deltas"
		}
		t.Run(label, func(t *testing.T) {
			e, s, q, services := fractionalSummaryFixture(t, late)
			ctx := context.Background()
			for _, names := range [][]string{nil, {"fraction-a"}, {"fraction-a", "fraction-b"}} {
				q.Services = names
				// Freeze endpoint oracles using the same completed hourly source but no
				// eligible service summaries. The test DB is the only database touched.
				if _, err := s.writer.Exec(`UPDATE aggregate_service_summary_hours SET state=1`); err != nil {
					t.Fatal(err)
				}
				wantTraffic, err := e.QueryBuckets(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				wantDash, err := e.QueryDashboard(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				wantTopo, err := e.QueryTopology(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				sel := Selector{TenantID: 1, Start: q.Start.Unix(), End: q.End.Unix(), Signal: SignalTraceOp}
				if len(names) == 1 {
					sel.ServiceIDs = services[:1]
				} else if len(names) > 1 {
					sel.ServiceIDs = services
				}
				wantScalars, err := s.SumBuckets(ctx, sel, GroupByService)
				if err != nil {
					t.Fatal(err)
				}
				edgeSel := sel
				edgeSel.Signal = SignalServiceEdge
				wantEdges, err := s.SumBuckets(ctx, edgeSel, GroupByService|GroupByName)
				if err != nil {
					t.Fatal(err)
				}
				wantSketches := originalRollupSketches(t, s, sel)
				if _, err := s.writer.Exec(`UPDATE aggregate_service_summary_hours SET state=4`); err != nil {
					t.Fatal(err)
				}
				capture := captureSummaryFitness(t, s)
				capture.label = "traffic"
				gotTraffic, err := e.QueryBuckets(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotTraffic, wantTraffic) {
					t.Fatal("traffic count/coverage oracle changed", gotTraffic, wantTraffic)
				}
				capture.label = "dashboard"
				gotDash, err := e.QueryDashboard(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotDash, wantDash) {
					t.Fatal("dashboard oracle changed", gotDash, wantDash)
				}
				capture.label = "topology"
				gotTopo, err := e.QueryTopology(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotTopo, wantTopo) {
					t.Fatal("topology oracle changed", gotTopo, wantTopo)
				}
				capture.label = "full-scalars"
				gotScalars, err := s.SumBuckets(ctx, sel, GroupByService)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotScalars, wantScalars) {
					t.Fatal("full scalar arithmetic changed", gotScalars, wantScalars)
				}
				gotEdges, err := s.SumBuckets(ctx, edgeSel, GroupByService|GroupByName)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotEdges, wantEdges) {
					t.Fatal("full edge arithmetic changed", gotEdges, wantEdges)
				}
				capture.label = "sketch"
				gotSketches := map[uint32]*Sketch{}
				if err := s.VisitSketches(ctx, sel, func(id uint32, sk *Sketch) error {
					if gotSketches[id] == nil {
						gotSketches[id] = NewSketch()
					}
					gotSketches[id].Merge(sk)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				for id, want := range wantSketches {
					got := gotSketches[id]
					if got == nil {
						t.Fatal("missing service sketch", id)
					}
					sketchAssertSameContent(t, want, got)
					if want.saturations != got.saturations || want.Quantile(.99) != got.Quantile(.99) {
						t.Fatal("sketch provenance/quantile changed")
					}
					// Existing sketch merge contract uses relative 1e-12; scalar outputs
					// above remain bitwise equal, without changing assertTrafficSums.
					if math.Abs(got.Sum()-want.Sum())/want.Sum() > 1e-12 {
						t.Fatal("sketch sum changed", got.Sum(), want.Sum())
					}
					t.Logf("services=%v sketch service=%d count=%d oracleSum=%.17g gotSum=%.17g p99=%.17g bins=%v", names, id, got.Count(), want.Sum(), got.Sum(), got.Quantile(.99), sketchBins(got))
				}
				trafficJSON, _ := json.Marshal(gotTraffic)
				scalarJSON, _ := json.Marshal(gotScalars)
				edgeJSON, _ := json.Marshal(gotEdges)
				t.Logf("services=%v traffic SHA256=%x scalars=%s edges=%s", names, sha256.Sum256(trafficJSON), scalarJSON, edgeJSON)
				traffic, sketch, dashSketch, topoSketch := capture.cells("traffic", false), capture.cells("sketch", true), capture.cells("dashboard", true), capture.cells("topology", true)
				t.Logf("state4 path decisions: traffic_summary=%d sketch_summary=%d dashboard_sketch_summary=%d topology_sketch_summary=%d full_scalar_summary=%d", traffic, sketch, dashSketch, topoSketch, capture.cells("full-scalars", false))
				if capture.cells("full-scalars", false) != 0 || capture.cells("dashboard", false) != 0 || capture.cells("topology", false) != 0 {
					t.Error("DurationSum consumer used uncertified arithmetic")
				}
				if traffic == 0 || sketch == 0 || dashSketch == 0 || topoSketch == 0 {
					t.Error("fractional source disabled independent count/sketch acceleration")
				}
				// Close each capture before the next selector so logs remain independent.
				s.reader.Close()
				s.reader = capture.original
			}
		})
	}
}

func TestServiceSummaryFractionalPlans(t *testing.T) {
	e, s, q, _ := fractionalSummaryFixture(t, true)
	q.Services = []string{"fraction-a", "fraction-b"}
	capture := captureSummaryFitness(t, s)
	capture.label = "traffic"
	if _, err := e.QueryBuckets(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	capture.label = "topology"
	if _, err := e.QueryTopology(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, q := range capture.queries {
		if q.label == "traffic" && strings.Contains(q.sql, "MIN(t.duration_sum") {
			t.Fatal("count projection still probes duration arithmetic")
		}
		if !strings.Contains(q.sql, "FROM aggregate_service_summary ") {
			continue
		}
		sketch := strings.Contains(q.sql, "sketch_high")
		kind := "counts"
		if sketch {
			kind = "sketch"
		}
		if seen[kind] {
			continue
		}
		seen[kind] = true
		plan, ops := summaryReviewPlan(t, capture.original, q.sql, q.args)
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "hour_start=? AND resolution=? AND tenant_id=? AND signal=? AND service_id=?") {
			t.Fatal("missing service-prefixed summary seek", joined)
		}
		t.Logf("actual %s SQL: %s args=%v", kind, q.sql, q.args)
		if sketch {
			continue
		}
		if strings.Contains(q.sql, "t.duration_sum") || strings.Contains(q.sql, "SUM(duration_sum)") {
			t.Fatal("count query reads DurationSum")
		}
		if !strings.Contains(joined, "SEARCH s USING INTEGER PRIMARY KEY (rowid=?)") || strings.Contains(joined, "SCAN s\n") {
			t.Fatal("count delta identity join is not keyed", joined)
		}
		// Inspect the actual bytecode too: neither durable source's duration_sum
		// column may be loaded by the count-only query.
		roots := map[int]int{}
		for _, table := range []string{"aggregate_service_summary", "aggregate_delta_log"} {
			var root, cid int
			if err := capture.original.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name=?`, table).Scan(&root); err != nil {
				t.Fatal(err)
			}
			if err := capture.original.QueryRow(`SELECT cid FROM pragma_table_info(?) WHERE name='duration_sum'`, table).Scan(&cid); err != nil {
				t.Fatal(err)
			}
			roots[root] = cid
		}
		cursors := map[int]int{}
		membership, identity := false, false
		for _, op := range ops {
			if op.op == "OpenRead" {
				if cid, ok := roots[op.p2]; ok {
					cursors[op.p1] = cid
				}
			}
			if cid, ok := cursors[op.p1]; ok && op.op == "Column" && op.p2 == cid {
				t.Fatal("count projection loads a source DurationSum", op)
			}
			if op.op == "NotFound" && op.p4 == "1" {
				membership = true
				t.Logf("keyed count membership: %+v", op)
			}
			if op.op == "SeekRowid" {
				identity = true
			}
		}
		if !membership || !identity {
			t.Fatal("missing keyed count delta bytecode", membership, identity)
		}
	}
	if !seen["counts"] || !seen["sketch"] {
		t.Fatal("missing accelerated production query", seen)
	}
}

func TestServiceSummaryFractionalEligibilityFences(t *testing.T) {
	e, s, q, _ := fractionalSummaryFixture(t, false)
	ctx := context.Background()
	capture := captureSummaryFitness(t, s)
	for _, change := range []struct{ name, mutate, restore string }{
		{"building", `UPDATE aggregate_service_summary_hours SET state=1`, `UPDATE aggregate_service_summary_hours SET state=4`},
		{"cleanup", `UPDATE aggregate_service_summary_hours SET state=3`, `UPDATE aggregate_service_summary_hours SET state=4`},
		{"source-token", `UPDATE aggregate_service_summary_hours SET source_build_id='stale'`, `UPDATE aggregate_service_summary_hours SET source_build_id=(SELECT build_id FROM aggregate_rollup_hours WHERE window_start=aggregate_service_summary_hours.window_start)`},
		{"uuid", `UPDATE aggregate_rollup_meta SET value='stale' WHERE key='service_summary_store_uuid'`, `UPDATE aggregate_rollup_meta SET value=(SELECT value FROM aggregate_meta WHERE key='store_uuid') WHERE key='service_summary_store_uuid'`},
	} {
		if _, err := s.writer.Exec(change.mutate); err != nil {
			t.Fatal(err)
		}
		capture.label = change.name
		if _, err := e.QueryBuckets(ctx, q); err != nil {
			t.Fatal(err)
		}
		if _, err := e.QueryTopology(ctx, q); err != nil {
			t.Fatal(err)
		}
		if capture.cells(change.name, false) != 0 || capture.cells(change.name, true) != 0 {
			t.Fatal("ineligible counts/sketches read", change.name)
		}
		if _, err := s.writer.Exec(change.restore); err != nil {
			t.Fatal(err)
		}
	}
	capture.label = "restored"
	if _, err := e.QueryBuckets(ctx, q); err != nil {
		t.Fatal(err)
	}
	if _, err := e.QueryTopology(ctx, q); err != nil {
		t.Fatal(err)
	}
	if capture.cells("restored", false) != 4 || capture.cells("restored", true) != 4 {
		t.Fatal("restored counts/sketches not eligible")
	}
	// Cleanup claims fence state 4 before deletion, just as they fence state 2.
	c, err := s.claimSummaryCleanup(ctx, rollupTestHour, rollupTestHour+300)
	if err != nil {
		t.Fatal(err)
	}
	if done, err := s.summaryCleanupSlice(ctx, c, 1); err != nil || done {
		t.Fatal("state4 bounded cleanup", done, err)
	}
	capture.label = "one-hour-cleaning"
	if _, err := e.QueryBuckets(ctx, q); err != nil {
		t.Fatal(err)
	}
	if capture.cells("one-hour-cleaning", false) != 3 {
		t.Fatal("cleanup hour still used counts")
	}
}

func TestServiceSummaryFractionalSketchFallback(t *testing.T) {
	e, s, q, _ := fractionalSummaryFixture(t, true)
	ctx := context.Background()
	sel := Selector{TenantID: 1, Start: q.Start.Unix(), End: q.End.Unix(), Signal: SignalTraceOp}
	collect := func() map[uint32]*Sketch {
		out := map[uint32]*Sketch{}
		if err := s.VisitSketches(ctx, sel, func(id uint32, sk *Sketch) error {
			if out[id] == nil {
				out[id] = NewSketch()
			}
			out[id].Merge(sk)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if _, err := s.writer.Exec(`UPDATE aggregate_service_summary_hours SET state=1`); err != nil {
		t.Fatal(err)
	}
	want := collect()
	if _, err := s.writer.Exec(`UPDATE aggregate_service_summary_hours SET state=4`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writer.Exec(`UPDATE aggregate_service_summary SET sketch_state=2 WHERE hour_start=? AND resolution=3600 AND signal=?`, rollupTestHour, SignalTraceOp); err != nil {
		t.Fatal(err)
	}
	capture := captureSummaryFitness(t, s)
	capture.label = "unsafe"
	got := collect()
	if !reflect.DeepEqual(got, want) {
		t.Fatal("unsafe state4 did not retain exact whole-chunk fallback/provenance")
	}
	summary, authoritative := 0, 0
	for _, q := range capture.queries {
		if strings.Contains(q.sql, "FROM aggregate_service_summary ") {
			summary++
		}
		if strings.Contains(q.sql, "FROM aggregate_hourly") || strings.Contains(q.sql, "FROM aggregate_buckets") {
			authoritative++
		}
	}
	if summary == 0 || authoritative == 0 {
		t.Fatal("unsafe path did not attempt and discard candidate", summary, authoritative)
	}
	if _, err := s.writer.Exec(`UPDATE aggregate_service_summary SET sketch_state=1,sketch=x'00',sketch_high=NULL WHERE hour_start=? AND resolution=3600 AND signal=?`, rollupTestHour, SignalTraceOp); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitSketches(ctx, sel, func(uint32, *Sketch) error { return nil }); err == nil {
		t.Fatal("corrupt state4 sketch accepted")
	}
	capture.label = "counts-ignore-sketch"
	if _, err := e.QueryBuckets(ctx, q); err != nil {
		t.Fatal("count path read unused sketch", err)
	}
	if capture.cells("counts-ignore-sketch", false) != 4 {
		t.Fatal("independent counts lost summary eligibility")
	}
}

// Count projection has its own arithmetic contract: integers and sentinel
// overlap remain exact even when fractional late sums reject full scalar reads.
func TestServiceSummaryFractionalCountArithmetic(t *testing.T) {
	for _, sourceSum := range []float64{1, .5} {
		t.Run(map[bool]string{true: "state2", false: "state4"}[sourceSum == 1], func(t *testing.T) {
			s := rollupTestStore(t)
			ctx := context.Background()
			tx, err := s.writer.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`INSERT INTO aggregate_dict VALUES (2,1,?,?),(4,1,?,?),(5,1,?,?)`, KindService, []byte(OtherValue), KindOperation, []byte(OtherValue), KindDimTuple, []byte(OtherValue)); err != nil {
				t.Fatal(err)
			}
			keys := []SeriesKey{storeKey(4), storeKey(4), storeKey(6)}
			keys[0].DimsID, keys[1].DimsID = 5, 5
			keys[1].HTTPClass = 1
			keys[2].ServiceID = 3
			var series []SeriesRow
			for i, key := range keys {
				series = append(series, SeriesRow{ID: SeriesID(i + 1), Key: key})
			}
			seedSeries(t, tx, series)
			insertRows(t, tx, "aggregate_buckets", []SeriesID{1, 2, 3}, []int64{rollupTestHour, rollupTestHour, rollupTestHour}, []*AggregateDelta{
				{Count: 1<<53 + 3, ErrorCount: 1<<53 + 1, RequestCount: 1<<53 + 2, ErrorRequestCount: 1 << 53, DurationCount: 1, DurationSum: sourceSum},
				{Count: 7, RequestCount: 3, DurationCount: 1, DurationSum: sourceSum},
				{Count: 11, RequestCount: 5, DurationCount: 1, DurationSum: sourceSum},
			})
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			finishRollups(t, s)
			finishSummary(t, s, rollupTestHour, 1)
			if err := s.CommitGroup(&GroupBatch{Deltas: []DeltaRow{
				{SeriesID: 1, WindowStart: rollupTestHour, Delta: &AggregateDelta{Count: 5, DurationCount: 1, DurationSum: .25}},
				{SeriesID: 2, WindowStart: rollupTestHour, Delta: &AggregateDelta{Count: 6, DurationCount: 1, DurationSum: .75}},
			}}); err != nil {
				t.Fatal(err)
			}
			sel := Selector{TenantID: 1, Start: rollupTestHour, End: rollupTestHour + rollupHour, Signal: SignalTraceOp}
			for _, services := range [][]uint32{nil, {2}, {2, 3}} {
				sel.ServiceIDs = services
				for _, by := range []GroupBy{GroupByWindow, GroupByWindow | GroupByService} {
					want := trafficRows(originalRollupSums(t, s, sel, by))
					got, used, err := readSummaryProjection(ctx, s.reader, sel, by, true)
					if err != nil || !used || !reflect.DeepEqual(trafficRows(got), want) {
						t.Fatal("count/identity projection", services, by, used, err, trafficRows(got), want)
					}
					var identity uint64
					for _, row := range got {
						identity += row.IdentityCount
					}
					if identity != 1<<53+21 {
						t.Fatal("sentinel overlap double-counted or rounded", identity)
					}
					if _, used, err := readSummarySums(ctx, s.reader, sel, by); err != nil || used {
						t.Fatal("fractional late sums did not retain full fallback", used, err)
					}
				}
			}
			if _, used, err := readSummaryProjection(ctx, s.reader, sel, GroupByService, true); err != nil || used {
				t.Fatal("count projection escaped its endpoint shape", used, err)
			}
			if err := s.CommitGroup(&GroupBatch{Deltas: []DeltaRow{{SeriesID: 3, WindowStart: rollupTestHour, Delta: &AggregateDelta{Count: math.MaxInt64}}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := readSumBuckets(ctx, s.reader, sel, GroupByWindow, false); err == nil || !strings.Contains(err.Error(), "integer overflow") {
				t.Fatal("authoritative overflow absent", err)
			}
			if _, used, err := readSummaryProjection(ctx, s.reader, sel, GroupByWindow, true); !used || err == nil || !strings.Contains(err.Error(), "integer overflow") {
				t.Fatal("count projection changed integer overflow", used, err)
			}
		})
	}
}
