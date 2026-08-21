package graphrag

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"gorm.io/gorm"
)

// fakeAggregateSource is a scripted AggregateSource. It counts snapshot renders
// so a test can prove the revision gate actually skips work rather than merely
// producing the same answer twice.
type fakeAggregateSource struct {
	epoch    uint64
	snaps    map[string]aggregate.TopologySnapshot
	renders  atomic.Int64
	prunes   atomic.Int64
	tenantsF func() []string
}

func (f *fakeAggregateSource) TopologyEpoch() uint64 { return f.epoch }

func (f *fakeAggregateSource) TopologyTenants() []string {
	if f.tenantsF != nil {
		return f.tenantsF()
	}
	out := make([]string, 0, len(f.snaps))
	for tenant := range f.snaps {
		out = append(out, tenant)
	}
	return out
}

func (f *fakeAggregateSource) TopologyRevision(tenant string) uint64 {
	return f.snaps[tenant].Revision
}

func (f *fakeAggregateSource) TopologySnapshot(tenant string) aggregate.TopologySnapshot {
	f.renders.Add(1)
	snap := f.snaps[tenant]
	snap.Epoch = f.epoch
	return snap
}

func (f *fakeAggregateSource) PruneTopology() { f.prunes.Add(1) }

// aggWindow builds one finalized topology window.
func aggWindow(start time.Time, count, errors uint64, p99Micros float64) aggregate.TopologyWindow {
	return aggregate.TopologyWindow{
		Start:             start,
		End:               start.Add(aggregate.WindowSize),
		Closed:            true,
		Final:             true,
		Elapsed:           aggregate.WindowSize,
		Count:             count,
		ErrorCount:        errors,
		DurationCount:     count,
		DurationSumMicros: float64(count) * 1000,
		P95Micros:         p99Micros * 0.8,
		P99Micros:         p99Micros,
	}
}

// aggregateGraphRAG builds a coordinator in aggregate mode wired to src.
func aggregateGraphRAG(t *testing.T, repo *storage.Repository, src AggregateSource) *GraphRAG {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Mode = aggregate.ModeAggregate
	g := New(repo, nil, nil, cfg)
	g.SetAggregateSource(src)
	t.Cleanup(g.Stop)
	return g
}

func oneServiceSnapshot(revision uint64, count, errors uint64) aggregate.TopologySnapshot {
	now := time.Now().UTC().Truncate(aggregate.WindowSize)
	return aggregate.TopologySnapshot{
		Tenant:   storage.DefaultTenantID,
		Revision: revision,
		Now:      now,
		Services: []aggregate.TopologyService{{
			Name:      "checkout",
			FirstSeen: now,
			LastSeen:  now.Add(aggregate.WindowSize),
			Windows:   []aggregate.TopologyWindow{aggWindow(now, count, errors, 4000)},
		}},
		Operations: []aggregate.TopologyOperation{{
			Service:   "checkout",
			Operation: "/pay",
			FirstSeen: now,
			LastSeen:  now.Add(aggregate.WindowSize),
			Windows:   []aggregate.TopologyWindow{aggWindow(now, count, errors, 4000)},
		}},
		Edges: []aggregate.TopologyEdge{{
			Caller:    "gateway",
			Callee:    "checkout",
			FirstSeen: now,
			LastSeen:  now.Add(aggregate.WindowSize),
			Windows:   []aggregate.TopologyWindow{aggWindow(now, count, errors, 4000)},
		}},
	}
}

// TestReconcileReplacesRatherThanAccumulates is the whole point of
// replacement-by-revision: re-applying the SAME snapshot must leave the
// counters where they were. The old rebuild multiplied them by the number of
// ticks a window survived.
func TestReconcileReplacesRatherThanAccumulates(t *testing.T) {
	src := &fakeAggregateSource{
		epoch: 7,
		snaps: map[string]aggregate.TopologySnapshot{
			storage.DefaultTenantID: oneServiceSnapshot(1, 100, 5),
		},
	}
	g := aggregateGraphRAG(t, newTestRepo(t), src)

	g.reconcileTopology()
	stores := g.storesForTenant(storage.DefaultTenantID)
	svc, ok := stores.service.GetService("checkout")
	if !ok || svc.CallCount != 100 || svc.ErrorCount != 5 {
		t.Fatalf("after first reconcile: %+v, want CallCount 100 / ErrorCount 5", svc)
	}

	// Force a re-render of the identical snapshot by bumping only the epoch's
	// revision bookkeeping on the consumer side.
	stores.topoRevision.Store(0)
	g.reconcileTopology()
	svc, _ = stores.service.GetService("checkout")
	if svc.CallCount != 100 || svc.ErrorCount != 5 {
		t.Fatalf("identical snapshot accumulated: CallCount=%d ErrorCount=%d, want 100/5",
			svc.CallCount, svc.ErrorCount)
	}
	if len(stores.service.AllEdges()) == 0 {
		t.Fatalf("edges were lost by the replacement")
	}
}

// TestReconcileSkipsUnchangedRevision proves an unchanged (epoch, revision)
// pair costs one map lookup and renders nothing.
func TestReconcileSkipsUnchangedRevision(t *testing.T) {
	src := &fakeAggregateSource{
		epoch: 7,
		snaps: map[string]aggregate.TopologySnapshot{
			storage.DefaultTenantID: oneServiceSnapshot(3, 10, 0),
		},
	}
	g := aggregateGraphRAG(t, newTestRepo(t), src)

	g.reconcileTopology()
	if got := src.renders.Load(); got != 1 {
		t.Fatalf("first reconcile rendered %d snapshots, want 1", got)
	}
	for i := 0; i < 5; i++ {
		g.reconcileTopology()
	}
	if got := src.renders.Load(); got != 1 {
		t.Fatalf("unchanged revision rendered %d snapshots, want 1", got)
	}

	// A moved revision is applied again.
	src.snaps[storage.DefaultTenantID] = oneServiceSnapshot(4, 20, 1)
	g.reconcileTopology()
	if got := src.renders.Load(); got != 2 {
		t.Fatalf("changed revision rendered %d snapshots, want 2", got)
	}
	svc, _ := g.storesForTenant(storage.DefaultTenantID).service.GetService("checkout")
	if svc.CallCount != 20 {
		t.Fatalf("CallCount = %d after revision 4, want 20", svc.CallCount)
	}
}

// TestReconcileHandlesEpochReset proves a restarted engine — new epoch, revision
// counter back to a LOWER number — is applied rather than skipped as "already
// seen".
func TestReconcileHandlesEpochReset(t *testing.T) {
	src := &fakeAggregateSource{
		epoch: 7,
		snaps: map[string]aggregate.TopologySnapshot{
			storage.DefaultTenantID: oneServiceSnapshot(99, 900, 9),
		},
	}
	g := aggregateGraphRAG(t, newTestRepo(t), src)
	g.reconcileTopology()
	stores := g.storesForTenant(storage.DefaultTenantID)
	if svc, _ := stores.service.GetService("checkout"); svc.CallCount != 900 {
		t.Fatalf("pre-reset CallCount = %d, want 900", svc.CallCount)
	}

	// Engine restarted: new epoch, revision restarts at 1 with fresh counters.
	src.epoch = 8
	src.snaps[storage.DefaultTenantID] = oneServiceSnapshot(1, 12, 0)
	g.reconcileTopology()

	if stores.topoEpoch.Load() != 8 || stores.topoRevision.Load() != 1 {
		t.Fatalf("consumer did not adopt the new epoch/revision: %d/%d",
			stores.topoEpoch.Load(), stores.topoRevision.Load())
	}
	svc, _ := stores.service.GetService("checkout")
	if svc.CallCount != 12 {
		t.Fatalf("post-reset CallCount = %d, want 12 (replaced, not merged)", svc.CallCount)
	}
}

// countSpanQueries registers a GORM callback that counts every query touching
// the spans table and returns the counter.
func countSpanQueries(t *testing.T, db *gorm.DB) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	err := db.Callback().Query().After("gorm:query").Register("test:count_span_scans", func(tx *gorm.DB) {
		sql := strings.ToLower(tx.Statement.SQL.String())
		if strings.Contains(sql, "`spans`") || strings.Contains(sql, " spans ") ||
			strings.Contains(sql, "\"spans\"") || strings.EqualFold(tx.Statement.Table, "spans") {
			n.Add(1)
		}
	})
	if err != nil {
		t.Fatalf("register query callback: %v", err)
	}
	return &n
}

// TestAggregateModeIssuesNoSpanQueries is the acceptance criterion: in
// aggregate mode GraphRAG's refresh path must not touch the spans table at
// all. The counter is proven non-vacuous by running the same assertion in
// legacy mode, where it must fire.
func TestAggregateModeIssuesNoSpanQueries(t *testing.T) {
	ctx := context.Background()

	repo := newTestRepo(t)
	counter := countSpanQueries(t, repo.DB())
	src := &fakeAggregateSource{
		epoch: 1,
		snaps: map[string]aggregate.TopologySnapshot{
			storage.DefaultTenantID: oneServiceSnapshot(1, 50, 2),
		},
	}
	g := aggregateGraphRAG(t, repo, src)
	for i := 0; i < 10; i++ {
		g.refreshTopology(ctx)
	}
	if got := counter.Load(); got != 0 {
		t.Fatalf("aggregate mode issued %d spans-table queries, want 0", got)
	}
	if svc, ok := g.storesForTenant(storage.DefaultTenantID).service.GetService("checkout"); !ok || svc.CallCount != 50 {
		t.Fatalf("aggregate topology was not applied: %+v", svc)
	}

	// Non-vacuity: the same driver in legacy mode DOES scan spans.
	legacyRepo := newTestRepo(t)
	legacyCounter := countSpanQueries(t, legacyRepo.DB())
	legacy := New(legacyRepo, nil, nil, DefaultConfig())
	t.Cleanup(legacy.Stop)
	legacy.refreshTopology(ctx)
	if legacyCounter.Load() == 0 {
		t.Fatalf("legacy mode issued no spans-table queries; the counter is vacuous")
	}
}

// TestAggregateModeSkipsPerSpanTopologyUpserts proves an exemplar span still
// populates the trace store (so error chains work) without touching the
// service topology the snapshot owns.
func TestAggregateModeSkipsPerSpanTopologyUpserts(t *testing.T) {
	src := &fakeAggregateSource{epoch: 1, snaps: map[string]aggregate.TopologySnapshot{}}
	g := aggregateGraphRAG(t, newTestRepo(t), src)

	now := time.Now()
	g.processSpan(&spanEvent{
		Tenant:  storage.DefaultTenantID,
		TraceID: "t-1",
		Status:  storage.StatusCodeError,
		Span: storage.Span{
			TenantID:      storage.DefaultTenantID,
			TraceID:       "t-1",
			SpanID:        "s-1",
			ServiceName:   "checkout",
			OperationName: "/pay",
			StartTime:     now,
			Duration:      1000,
		},
	})

	stores := g.storesForTenant(storage.DefaultTenantID)
	if _, ok := stores.service.GetService("checkout"); ok {
		t.Fatalf("aggregate mode upserted a service node from a raw span")
	}
	if _, ok := stores.traces.GetSpan("s-1"); !ok {
		t.Fatalf("exemplar span did not reach the trace store")
	}
}

// TestOnTemplateFactPopulatesLogClusters proves the ingest-owned miner's facts
// land as log clusters and that GraphRAG runs no miner of its own outside
// legacy mode.
func TestOnTemplateFactPopulatesLogClusters(t *testing.T) {
	src := &fakeAggregateSource{epoch: 1, snaps: map[string]aggregate.TopologySnapshot{}}
	g := aggregateGraphRAG(t, newTestRepo(t), src)
	if g.drain != nil {
		t.Fatalf("aggregate mode constructed a GraphRAG-owned Drain miner")
	}

	now := time.Now()
	// The fact rides the event channel, so drain it on this goroutine rather
	// than starting the worker pool.
	g.OnTemplateFact(aggregate.TemplateFact{
		Tenant:     storage.DefaultTenantID,
		Service:    "checkout",
		Severity:   "ERROR",
		TemplateID: 42,
		Template:   "payment <*> declined",
		Timestamp:  now,
	})
	ev := <-g.eventCh
	if ev.tmpl == nil {
		t.Fatalf("template fact did not reach the event channel: %+v", ev)
	}
	g.processTemplateFact(ev.tmpl)

	clusters := g.storesForTenant(storage.DefaultTenantID).signals.LogClustersForService("checkout")
	if len(clusters) != 1 || clusters[0].Template != "payment <*> declined" {
		t.Fatalf("template fact did not become a log cluster: %+v", clusters)
	}
	if clusters[0].TemplateID != 42 {
		t.Fatalf("template ID = %d, want the ingest-minted 42", clusters[0].TemplateID)
	}
}
