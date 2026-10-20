package aggregate

import (
	"context"
	"testing"
	"time"
)

func TestIdentityCoverageDictionaryOverflowAndRestart(t *testing.T) {
	bounds := Bounds{PerTenantKind: map[Kind]int{KindOperation: 1, KindMetricName: 1, KindLogTemplate: 1}}
	f := newLifecycleFixture(t, bounds)
	now := f.clock.Now()
	for _, name := range []string{"known", "new-first", "new-last"} {
		r := f.eng.NewReducer(now)
		reduceSpanAt(r, "acme", "svc", name, 0, 100, now)
		r.ReduceMetricPoint(MetricInput{Tenant: "acme", Service: "svc", Name: name, Value: 3, Timestamp: now})
		r.ReduceLog(LogInput{Tenant: "acme", Service: "svc", Body: name, Severity: "INFO", Timestamp: now})
		if _, err := f.eng.ApplyReducerErr(r); err != nil {
			t.Fatal(err)
		}
	}
	checkProjection := func(e *Engine) {
		t.Helper()
		snap := e.TopologySnapshot("acme")
		if snap.IdentityCoverage.TracePoints != 2 || snap.IdentityCoverage.MetricPoints != 2 || snap.IdentityCoverage.LogPoints != 2 {
			t.Fatalf("identity coverage = %+v", snap.IdentityCoverage)
		}
		if len(snap.Operations) != 2 || len(snap.Metrics) != 2 {
			t.Fatalf("overflow names were invented: operations=%+v metrics=%+v", snap.Operations, snap.Metrics)
		}
		for _, op := range snap.Operations {
			want := uint64(1)
			if op.Operation == OtherValue {
				want = 2
			} else if op.Operation != "known" {
				t.Fatalf("invented operation %q", op.Operation)
			}
			if count, _ := totalCount(op.Windows); count != want {
				t.Fatalf("%s count=%d want=%d", op.Operation, count, want)
			}
		}
	}
	checkProjection(f.eng)
	checkQuery := func(e *Engine) {
		t.Helper()
		q := Query{Tenant: "acme", Start: now.Add(-WindowSize), End: now.Add(WindowSize)}
		res, err := e.QueryDashboard(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if res.SpanCount != 3 || res.TotalLogs != 3 || res.Coverage != CoverageLimited || res.IdentityCoverage.TracePoints != 2 || res.IdentityCoverage.LogPoints != 2 {
			t.Fatalf("dashboard = %+v", res)
		}
		q.Tenant = "unrelated"
		empty, err := e.QueryDashboard(context.Background(), q)
		if err != nil || empty.SpanCount != 0 || empty.IdentityCoverage.Limited() {
			t.Fatalf("tenant isolation: %+v %v", empty, err)
		}
	}
	checkQuery(f.eng)
	// Both the materialized store and a cold registrar must preserve counts
	// and the evidence of identity loss, independently of process counters.
	f.clock.Advance(WindowSize + AllowedLateness + time.Minute)
	cold := f.restart(bounds)
	if _, err := Recover(cold.store, cold.eng, cold.writer, cold.clock.Now(), RecoverOptions{TopologyHorizon: DefaultTopologyHorizon}); err != nil {
		t.Fatal(err)
	}
	checkQuery(cold.eng)
	checkProjection(cold.eng)
	// The current window is present both in the durable delta log and in
	// memory. Ownership must count it once beside finalized history.
	r := cold.eng.NewReducer(cold.clock.Now())
	reduceSpanAt(r, "acme", "svc", "still-new", 0, 100, cold.clock.Now())
	if _, err := cold.eng.ApplyReducerErr(r); err != nil {
		t.Fatal(err)
	}
	q := Query{Tenant: "acme", Start: now, End: cold.clock.Now().Add(WindowSize), Services: []string{"svc"}}
	res, err := cold.eng.QueryDashboard(context.Background(), q)
	if err != nil || res.SpanCount != 4 || res.IdentityCoverage.TracePoints != 3 || res.TotalLogs != 3 {
		t.Fatalf("disk/mutable overlap: %+v %v", res, err)
	}
	topo, err := cold.eng.QueryTopology(context.Background(), q)
	if err != nil || len(topo.Nodes) != 1 || topo.Nodes[0].Count != 4 || topo.IdentityCoverage.TracePoints != 3 {
		t.Fatalf("topology overlap: %+v %v", topo, err)
	}
	buckets, err := cold.eng.QueryBuckets(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	var spans int64
	for _, point := range buckets.Points {
		spans += point.SpanCount
	}
	if spans != 4 || buckets.IdentityCoverage.TracePoints != 3 {
		t.Fatalf("buckets overlap: %+v", buckets)
	}
}

func TestFilteredHistoryDisclosesUnnamedServices(t *testing.T) {
	f := newLifecycleFixture(t, Bounds{PerTenantKind: map[Kind]int{KindService: 1}})
	now := f.clock.Now()
	r := f.eng.NewReducer(now)
	for _, service := range []string{"known", "unnamed"} {
		reduceSpanAt(r, "acme", service, "op", 0, 100, now)
	}
	if _, err := f.eng.ApplyReducerErr(r); err != nil {
		t.Fatal(err)
	}
	q := Query{Tenant: "acme", Start: now, End: now.Add(WindowSize), Services: []string{"unnamed"}}
	for _, disk := range []bool{false, true} {
		if disk {
			if _, err := f.sqlite.FinalizeWindow(WindowStart(now)); err != nil {
				t.Fatal(err)
			}
			f.eng.MarkFinalized(WindowStart(now))
		}
		pending := f.reg.PendingCount()
		res, err := f.eng.QueryDashboard(context.Background(), q)
		if err != nil || res.SpanCount != 0 || res.Coverage != CoverageLimited || res.IdentityCoverage.TracePoints != 1 {
			t.Fatalf("disk=%v result=%+v err=%v", disk, res, err)
		}
		if f.reg.PendingCount() != pending {
			t.Fatal("service query minted dictionary identities")
		}
	}
}

func TestIdentityCoverageLimiterOverflowAndExpiry(t *testing.T) {
	now := time.Unix(3_000_000, 0).UTC()
	clock := newClock(now)
	e, err := NewEngine(EngineConfig{Mode: ModeAggregate, Now: clock.Now, Limiter: LimiterConfig{MaxSeriesTraces: 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, names := range [][]string{{"known"}, {"new-first", "new-last"}} {
		r := e.NewReducer(now)
		for _, name := range names {
			reduceSpanAt(r, "acme", "svc", name, 0, 100, now)
		}
		if _, err := e.ApplyReducerErr(r); err != nil {
			t.Fatal(err)
		}
		var reducedCount uint64
		for _, d := range r.Deltas() {
			reducedCount += d.Count
		}
		if reducedCount != uint64(len(names)) {
			t.Fatalf("admission mutated reducer count: %d want %d", reducedCount, len(names))
		}
	}
	snap := e.TopologySnapshot("acme")
	if snap.IdentityCoverage.TracePoints != 2 || len(snap.Operations) != 2 {
		t.Fatalf("projection = %+v", snap)
	}
	for _, op := range snap.Operations {
		if op.Operation != "known" && op.Operation != OtherValue {
			t.Fatalf("unretained name %q", op.Operation)
		}
	}
	clock.Advance(time.Hour)
	e.PruneTopology()
	if e.TopologySnapshot("acme").IdentityCoverage.Limited() {
		t.Fatal("expired identity loss still reported")
	}
}

func TestIdentityCoverageDictionaryCollisionWithinOneReducer(t *testing.T) {
	now := time.Unix(3_000_000, 0).UTC()
	e, err := NewEngine(EngineConfig{Mode: ModeAggregate, Now: func() time.Time { return now }, Registrar: NewMemRegistrar(&MemRegistrarOptions{Limits: map[Kind]int{KindOperation: 1}})})
	if err != nil {
		t.Fatal(err)
	}
	r := e.NewReducer(now)
	for _, name := range []string{"known", "new-first", "new-last"} {
		reduceSpanAt(r, "acme", "svc", name, 0, 100, now)
	}
	if _, err := e.ApplyReducerErr(r); err != nil {
		t.Fatal(err)
	}
	snap := e.TopologySnapshot("acme")
	if snap.IdentityCoverage.TracePoints != 2 {
		t.Fatalf("coverage = %+v", snap.IdentityCoverage)
	}
	for _, op := range snap.Operations {
		if op.Operation == "new-last" || op.Operation == "new-first" {
			t.Fatalf("merged counts attributed to raw name %q", op.Operation)
		}
	}
}

func TestHistoricalTopologyDoesNotSpendActiveSeriesBudget(t *testing.T) {
	cold := restoreFixture(t, 20*time.Minute)
	// The retained fixture has two trace series, more than the new active
	// budget. Replaying history into the limiter would exhaust it.
	cold.eng.limiter = NewLimiter(LimiterConfig{MaxSeriesTraces: 1, OtherNameID: cold.eng.otherNameID})
	stats, err := Recover(cold.store, cold.eng, cold.writer, cold.clock.Now(), RecoverOptions{TopologyHorizon: DefaultTopologyHorizon})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ReplayedRows != 0 || stats.RestoredTopologyRows == 0 {
		t.Fatalf("recovery=%+v", stats)
	}
	if active := cold.eng.Limiter().Stats().Active; active != 0 {
		t.Fatalf("history consumed %d active slots", active)
	}
	r := cold.eng.NewReducer(cold.clock.Now())
	reduceSpanAt(r, "acme", "new-service", "new-operation", 0, 100, cold.clock.Now())
	if _, err := cold.eng.ApplyReducerErr(r); err != nil {
		t.Fatal(err)
	}
	if stats := cold.eng.Limiter().Stats(); stats.ActiveBySignal[SignalTraceOp] != 1 || stats.OverflowSeries != 0 {
		t.Fatalf("fresh live series lost identity: %+v", stats)
	}
}
