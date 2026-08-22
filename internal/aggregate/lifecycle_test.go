package aggregate

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Shared scaffolding for the #200 identity-lifecycle tests. Every test in this
// file builds on these four helpers rather than re-declaring a store/engine/
// writer stack — the duplication gate is set at 3% and a per-test stack is
// forty lines of identical wiring.

// lifecycleFixture is a store plus the identity stack that sits on it.
type lifecycleFixture struct {
	t      *testing.T
	path   string
	clock  *fixedClock
	store  Store
	sqlite *SQLiteStore
	reg    *DurableRegistrar
	eng    *Engine
	writer *Writer
}

// newLifecycleFixture opens a fresh store at a fresh path and wires a cold
// identity stack over it.
func newLifecycleFixture(t *testing.T, b Bounds) *lifecycleFixture {
	t.Helper()
	return openLifecycleFixture(t, filepath.Join(t.TempDir(), "aggregate.db"), b, nil)
}

// openLifecycleFixture wires a cold identity stack over the store at path,
// which is what a restart looks like: durable rows are there, every in-memory
// map starts empty. wrap, when set, decorates the store the writer sees.
func openLifecycleFixture(t *testing.T, path string, b Bounds, wrap func(*SQLiteStore) Store) *lifecycleFixture {
	t.Helper()
	sqlite := newTestStoreAt(t, path, StoreConfig{})
	var store Store = sqlite
	if wrap != nil {
		store = wrap(sqlite)
	}
	reg, err := NewDurableRegistrarWithBounds(store, b)
	if err != nil {
		t.Fatalf("NewDurableRegistrarWithBounds: %v", err)
	}
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	eng, err := NewEngine(EngineConfig{
		Mode:      ModeAggregate,
		Registrar: reg,
		Bounds:    b,
		Now:       clock.Now,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	f := &lifecycleFixture{t: t, path: path, clock: clock, store: store, sqlite: sqlite, reg: reg, eng: eng}
	f.writer = newTestWriter(t, store, eng, clock, WriterConfig{Registrar: reg})
	return f
}

// restart stops the writer, closes the store, and reopens a cold stack on the
// same file. This is the only way to observe what durable identity actually
// survives — a warm cache is a root and keeps everything alive by definition.
func (f *lifecycleFixture) restart(b Bounds) *lifecycleFixture {
	f.t.Helper()
	f.writer.Stop()
	if err := f.sqlite.Close(); err != nil {
		f.t.Fatalf("close store: %v", err)
	}
	return openLifecycleFixture(f.t, f.path, b, nil)
}

// collect runs one GC pass through the fixture's writer barrier.
func (f *lifecycleFixture) collect() GCStats {
	f.t.Helper()
	stats, err := f.writer.CollectIdentities()
	if err != nil {
		f.t.Fatalf("CollectIdentities: %v", err)
	}
	return stats
}

// countRows returns the row count of one aggregate table.
func countRows(t *testing.T, s *SQLiteStore, table string) int {
	t.Helper()
	var n int
	if err := s.reader.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// dictExists reports whether a dictionary row survived.
func dictExists(t *testing.T, s *SQLiteStore, id uint32) bool {
	t.Helper()
	var n int
	if err := s.reader.QueryRow(`SELECT COUNT(*) FROM aggregate_dict WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("probe dict %d: %v", id, err)
	}
	return n > 0
}

// seed commits one batch straight to the store, bypassing the reducer. The
// identity tests care about exactly which rows exist, not about how a span
// became one.
func seed(t *testing.T, store Store, b *GroupBatch) {
	t.Helper()
	if err := store.CommitGroup(b); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
}

// dictRow is a terse DictRow builder.
func dictRow(id, tenant uint32, kind Kind, value string) DictRow {
	return DictRow{ID: id, TenantID: tenant, Kind: kind, Value: []byte(value)}
}

// lifecycleTraceKey builds a trace-operation key from its dictionary IDs.
func lifecycleTraceKey(tenant, service, name, dims uint32) SeriesKey {
	return SeriesKey{
		TenantID:    tenant,
		ServiceID:   service,
		NameID:      name,
		DimsID:      dims,
		Signal:      SignalTraceOp,
		StatusClass: StatusOK,
	}
}

// --- Q3: bounds --------------------------------------------------------------

func TestBoundsOverLengthRoutesToOtherNeverTruncates(t *testing.T) {
	reg := NewMemRegistrar(nil)
	c := NewCacheWithBounds(reg, Bounds{MaxValueBytes: 16})

	long := strings.Repeat("s", 17)
	id := c.Intern(1, KindService, long)
	other := c.OtherID(1, KindService)
	if id != other {
		t.Fatalf("over-length service id = %d, want the __other__ id %d", id, other)
	}
	if entry, ok := reg.Lookup(id); !ok || string(entry.Value) != OtherValue {
		t.Fatalf("over-length value was registered as %q; it must never be truncated into the dictionary", entry.Value)
	}
	if stats := c.Stats(); stats.OverLength != 1 {
		t.Fatalf("OverLength = %d, want 1", stats.OverLength)
	}

	// Exactly at the cap is admitted: the bound is a maximum, not a margin.
	exact := c.Intern(1, KindService, strings.Repeat("s", 16))
	if exact == other || exact == 0 {
		t.Fatalf("value exactly at the cap was refused (id %d, other %d)", exact, other)
	}
}

func TestBoundsTenantIsRejectedNeverCollapsed(t *testing.T) {
	f := newLifecycleFixture(t, Bounds{MaxTenantBytes: 8, MaxTenants: 2})
	c := f.eng.Cache()

	for _, name := range []string{"", strings.Repeat("t", 9)} {
		if id, ok := c.InternTenant(name); ok {
			t.Fatalf("tenant %q admitted as %d; over-length and empty tenants must be refused", name, id)
		}
	}
	first, ok := c.InternTenant("alpha")
	if !ok {
		t.Fatal("first tenant refused")
	}
	if _, ok := c.InternTenant("beta"); !ok {
		t.Fatal("second tenant refused below the cap")
	}
	third, ok := c.InternTenant("gamma")
	if ok {
		t.Fatalf("third tenant admitted as %d past a cap of 2", third)
	}
	// The rejected tenant must NOT have been folded onto an existing one.
	if third == first {
		t.Fatal("a refused tenant collapsed onto an admitted tenant's identity")
	}
	if got := c.OtherID(GlobalTenant, KindTenant); got != 0 {
		t.Fatalf("tenant namespace minted an __other__ entry (%d); a shared overflow tenant is the merge the cap prevents", got)
	}
	if stats := c.Stats(); stats.TenantsRejected != 3 {
		t.Fatalf("TenantsRejected = %d, want 3", stats.TenantsRejected)
	}
}

func TestBoundsRejectedTenantDropsThePoint(t *testing.T) {
	f := newLifecycleFixture(t, Bounds{MaxTenantBytes: 4})
	r := f.eng.NewReducer(f.clock.Now())
	r.ReduceSpan(SpanInput{
		Tenant: strings.Repeat("t", 5), Service: "svc", SpanName: "op",
		Timestamp: f.clock.Now(), DurationMicros: 100,
	})
	if r.Len() != 0 {
		t.Fatalf("reducer emitted %d deltas for a rejected tenant, want 0", r.Len())
	}
	stats := r.Stats()
	if stats.TenantsRejected != 1 {
		t.Fatalf("TenantsRejected = %d, want 1", stats.TenantsRejected)
	}
	if stats.Accepted[SignalTraceOp] != 0 {
		t.Fatalf("a dropped point was counted as accepted (%d)", stats.Accepted[SignalTraceOp])
	}
	if stats.InputPoints[SignalTraceOp] != 1 {
		t.Fatalf("InputPoints = %d, want 1 — a dropped point is still offered telemetry", stats.InputPoints[SignalTraceOp])
	}
}

func TestBoundsPerTenantAndInstanceCaps(t *testing.T) {
	reg, err := NewDurableRegistrarWithBounds(nil, Bounds{
		PerTenantKind: map[Kind]int{KindService: 2},
		InstanceKind:  map[Kind]int{KindService: 3},
	})
	if err != nil {
		t.Fatalf("NewDurableRegistrarWithBounds: %v", err)
	}
	admit := func(tenant uint32, value string) bool {
		_, err := reg.Register(tenant, KindService, []byte(value))
		return err == nil
	}
	if !admit(1, "a") || !admit(1, "b") {
		t.Fatal("per-tenant cap bound below its limit")
	}
	if admit(1, "c") {
		t.Fatal("per-tenant cap of 2 admitted a third service")
	}
	// A second tenant has its own per-tenant budget but shares the instance
	// backstop, which is the whole point of having both.
	if !admit(2, "a") {
		t.Fatal("second tenant refused its first service")
	}
	if admit(2, "b") {
		t.Fatal("instance-wide backstop of 3 admitted a fourth service")
	}
}

func TestPreloadFailsFastAboveTheSupportedBound(t *testing.T) {
	prevDict, prevSeries := MaxDictRows, MaxSeriesRows
	MaxDictRows, MaxSeriesRows = 2, 2
	t.Cleanup(func() { MaxDictRows, MaxSeriesRows = prevDict, prevSeries })

	store := newTestStore(t)
	seed(t, store, &GroupBatch{Dicts: []DictRow{
		dictRow(1, GlobalTenant, KindTenant, "acme"),
		dictRow(2, 1, KindService, "svc"),
		dictRow(3, 1, KindOperation, "GET /a"),
	}})

	_, err := NewDurableRegistrarWithBounds(store, Bounds{})
	var preload *PreloadError
	if !errors.As(err, &preload) {
		t.Fatalf("registrar preload over the bound: got %v, want *PreloadError", err)
	}
	if preload.Table != "aggregate_dict" || preload.Max != 2 {
		t.Fatalf("PreloadError = %+v, want aggregate_dict at max 2", preload)
	}
	if !strings.Contains(preload.Error(), "aggregate_dict") {
		t.Fatalf("PreloadError message does not name the table: %s", preload.Error())
	}
}

// --- Q1/Q2: mark, sweep, watermarks, barrier ---------------------------------

func TestGCSweepsOnlyUnreferencedIdentities(t *testing.T) {
	f := newLifecycleFixture(t, Bounds{})
	seed(t, f.store, &GroupBatch{
		Dicts: []DictRow{
			dictRow(1, GlobalTenant, KindTenant, "acme"),
			dictRow(2, 1, KindService, "svc"),
			dictRow(3, 1, KindOperation, "GET /live"),
			dictRow(4, 1, KindOperation, "GET /orphan"),
		},
		Series: []SeriesRow{{ID: 1, Key: lifecycleTraceKey(1, 2, 3, 0)}},
		Deltas: []DeltaRow{{SeriesID: 1, WindowStart: WindowStart(f.clock.Now()), Delta: spanDelta(2, 100)}},
	})

	cold := f.restart(Bounds{})
	stats := cold.collect()

	if stats.DictSwept != 1 {
		t.Fatalf("DictSwept = %d, want 1 (the orphan operation only)", stats.DictSwept)
	}
	if stats.SeriesSwept != 0 {
		t.Fatalf("SeriesSwept = %d, want 0 — the series has a delta row", stats.SeriesSwept)
	}
	for _, id := range []uint32{1, 2, 3} {
		if !dictExists(t, cold.sqlite, id) {
			t.Fatalf("dictionary id %d was swept while a live series names it", id)
		}
	}
	if dictExists(t, cold.sqlite, 4) {
		t.Fatal("the orphan operation survived collection")
	}
}

func TestGCMarksDimTupleComponentsTransitively(t *testing.T) {
	f := newLifecycleFixture(t, Bounds{})
	// keyID 5, valueID 6 encoded canonically into tuple 7.
	tuple := AppendCanonicalDims(nil, []DimPair{{KeyID: 5, ValueID: 6}})
	seed(t, f.store, &GroupBatch{
		Dicts: []DictRow{
			dictRow(1, GlobalTenant, KindTenant, "acme"),
			dictRow(2, 1, KindService, "svc"),
			dictRow(3, 1, KindMetricName, "http.server.duration"),
			dictRow(5, 1, KindDimKey, "http.route"),
			dictRow(6, 1, KindDimValue, "/checkout"),
			{ID: 7, TenantID: 1, Kind: KindDimTuple, Value: tuple},
			dictRow(8, 1, KindDimKey, "orphan.key"),
		},
		Series: []SeriesRow{{ID: 1, Key: SeriesKey{TenantID: 1, ServiceID: 2, NameID: 3, DimsID: 7, Signal: SignalMetric}}},
		Deltas: []DeltaRow{{SeriesID: 1, WindowStart: WindowStart(f.clock.Now()), Delta: spanDelta(1, 10)}},
	})

	cold := f.restart(Bounds{})
	cold.collect()

	for _, id := range []uint32{5, 6, 7} {
		if !dictExists(t, cold.sqlite, id) {
			t.Fatalf("dictionary id %d was swept; a live dim tuple must mark the key and value inside it", id)
		}
	}
	if dictExists(t, cold.sqlite, 8) {
		t.Fatal("a dim key no tuple contains survived collection")
	}
}

func TestGCFollowsRetiredTemplateAliasChains(t *testing.T) {
	f := newLifecycleFixture(t, Bounds{})
	now := f.clock.Now().UnixNano()
	// 10 -> 11 -> 12: two retirements deep. Only 10 is named by a series, so
	// nothing but the transitive alias closure keeps 11 and 12 alive.
	seed(t, f.store, &GroupBatch{
		Dicts: []DictRow{
			dictRow(1, GlobalTenant, KindTenant, "acme"),
			dictRow(2, 1, KindService, "svc"),
			dictRow(10, 1, KindLogTemplate, "svc\x00retired one"),
			dictRow(11, 1, KindLogTemplate, "svc\x00retired two"),
			dictRow(12, 1, KindLogTemplate, "svc\x00survivor"),
			dictRow(13, 1, KindLogTemplate, "svc\x00unreachable"),
		},
		Templates: []TemplateRow{
			{ID: 10, Tenant: "acme", Service: "svc", Tokens: "retired\x00one", AliasOf: 11, LastSeen: now},
			{ID: 11, Tenant: "acme", Service: "svc", Tokens: "retired\x00two", AliasOf: 12, LastSeen: now},
			{ID: 12, Tenant: "acme", Service: "svc", Tokens: "survivor", LastSeen: now},
			{ID: 13, Tenant: "acme", Service: "svc", Tokens: "unreachable", LastSeen: now},
		},
		Series: []SeriesRow{{ID: 1, Key: SeriesKey{TenantID: 1, ServiceID: 2, NameID: 10, Signal: SignalLog, StatusClass: SeverityTierInfo}}},
		Deltas: []DeltaRow{{SeriesID: 1, WindowStart: WindowStart(f.clock.Now()), Delta: spanDelta(1, 10)}},
	})

	cold := f.restart(Bounds{})
	cold.collect()

	for _, id := range []uint32{10, 11, 12} {
		if !dictExists(t, cold.sqlite, id) {
			t.Fatalf("template %d was swept; every hop of a live alias chain stays", id)
		}
	}
	if dictExists(t, cold.sqlite, 13) {
		t.Fatal("a template no series and no alias reaches survived collection")
	}
	if countRows(t, cold.sqlite, "aggregate_log_template") != 3 {
		t.Fatalf("template rows = %d, want 3 — the swept dict id must take its miner row with it",
			countRows(t, cold.sqlite, "aggregate_log_template"))
	}
}

func TestGCHighWatermarkSurvivesSweepOfHighestID(t *testing.T) {
	f := newLifecycleFixture(t, Bounds{})
	seed(t, f.store, &GroupBatch{
		Dicts: []DictRow{
			dictRow(1, GlobalTenant, KindTenant, "acme"),
			dictRow(2, 1, KindService, "svc"),
			dictRow(900, 1, KindOperation, "GET /orphan"),
		},
		Series: []SeriesRow{{ID: 700, Key: lifecycleTraceKey(1, 2, 900, 0)}},
	})
	// The series row exists but nothing references it, so both the highest
	// dictionary ID and the highest series ID are collectible.
	cold := f.restart(Bounds{})
	stats := cold.collect()
	if stats.DictSwept == 0 || stats.SeriesSwept == 0 {
		t.Fatalf("expected the highest ids to be collected, got %+v", stats)
	}
	if dictExists(t, cold.sqlite, 900) {
		t.Fatal("the highest dictionary id survived collection")
	}

	// MAX(id)+1 would now reseed at 3 and 1. The watermarks must not.
	after := cold.restart(Bounds{})
	if next := after.reg.Next(); next <= 900 {
		t.Fatalf("dictionary reseeded at %d after sweeping id 900; a watermark must never decrease", next)
	}
	if next := after.writer.series.Next(); next <= 700 {
		t.Fatalf("series reseeded at %d after sweeping id 700; a watermark must never decrease", next)
	}
}

func TestBarrierFencedIDsAreNotHandedOut(t *testing.T) {
	reg, err := NewDurableRegistrarWithBounds(nil, Bounds{})
	if err != nil {
		t.Fatalf("NewDurableRegistrarWithBounds: %v", err)
	}
	c := NewCacheWithBounds(reg, Bounds{})
	id := c.Intern(1, KindService, "svc")
	if id == 0 {
		t.Fatal("service was not interned")
	}

	fence := map[uint32]struct{}{id: {}}
	reg.Fence(fence)
	c.Fence(fence)

	if got := c.Intern(1, KindService, "svc"); got == id {
		t.Fatal("a fenced dictionary id was handed to the hot path")
	} else if got != c.OtherID(1, KindService) {
		t.Fatalf("fenced lookup resolved to %d, want the __other__ id %d", got, c.OtherID(1, KindService))
	}
	if _, ok := reg.Lookup(id); ok {
		t.Fatal("a fenced dictionary id still reverses through the resolver")
	}

	// Releasing the fence restores the identity byte-for-byte.
	reg.Unfence()
	c.Unfence()
	if got := c.Intern(1, KindService, "svc"); got != id {
		t.Fatalf("after Unfence the value resolved to %d, want the original %d", got, id)
	}
}

func TestSweepFailureLeavesMemoryUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	boom := errors.New("sweep refused")
	seedStore := newTestStoreAt(t, path, StoreConfig{})
	seed(t, seedStore, &GroupBatch{Dicts: []DictRow{
		dictRow(1, GlobalTenant, KindTenant, "acme"),
		dictRow(2, 1, KindService, "orphan"),
	}})
	if err := seedStore.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	var failing *failingSweepStore
	f := openLifecycleFixture(t, path, Bounds{}, func(s *SQLiteStore) Store {
		failing = &failingSweepStore{SQLiteStore: s, err: boom}
		return failing
	})

	_, err := f.writer.CollectIdentities()
	if !errors.Is(err, boom) {
		t.Fatalf("CollectIdentities: got %v, want the sweep error", err)
	}
	if !dictExists(t, f.sqlite, 2) {
		t.Fatal("a failed sweep deleted rows anyway")
	}
	// The fence must be released and the identity must resolve exactly as it
	// did before the attempt.
	if _, ok := f.reg.Lookup(2); !ok {
		t.Fatal("a failed sweep left the reverse index fenced")
	}
	if got := f.eng.Cache().Intern(1, KindService, "orphan"); got != 2 {
		t.Fatalf("after a failed sweep the value resolved to %d, want the untouched id 2", got)
	}
}

// failingSweepStore is a store whose identity sweep always refuses.
type failingSweepStore struct {
	*SQLiteStore
	err error
}

func (s *failingSweepStore) SweepIdentities([]SeriesID, []uint32, []uint32) (SweepStats, error) {
	return SweepStats{}, s.err
}

// --- Q4/Q5: miner persistence ------------------------------------------------

func TestMinerIdentityRidesTheGroupCommit(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	inner := newTestStore(t)
	var seen []*GroupBatch
	hooked := &hookStore{Store: inner, beforeCommit: func(b *GroupBatch) {
		copyBatch := *b
		seen = append(seen, &copyBatch)
	}}
	reg, err := NewDurableRegistrarWithBounds(inner, Bounds{})
	if err != nil {
		t.Fatalf("registrar: %v", err)
	}
	eng, err := NewEngine(EngineConfig{Mode: ModeAggregate, Registrar: reg, Now: clock.Now})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	w := newTestWriter(t, hooked, eng, clock, WriterConfig{Registrar: reg})

	r := eng.NewReducer(clock.Now())
	r.ReduceLog(LogInput{
		Tenant: "acme", Service: "svc", Severity: "ERROR",
		Body: "connection refused to 10.0.0.1", Timestamp: clock.Now(),
	})
	if _, err := eng.ApplyReducerErr(r); err != nil {
		t.Fatalf("ApplyReducerErr: %v", err)
	}
	_ = w

	if len(seen) == 0 {
		t.Fatal("no group commit was observed")
	}
	batch := seen[len(seen)-1]
	if len(batch.Templates) == 0 {
		t.Fatal("the template identity did not ride the commit that used it")
	}
	if len(batch.Deltas) == 0 {
		t.Fatal("the batch carrying the template identity carried no delta")
	}
	if eng.Miner().PendingCount() != 0 {
		t.Fatalf("PendingCount = %d after a successful commit, want 0", eng.Miner().PendingCount())
	}
	if countRows(t, inner, "aggregate_log_template") == 0 {
		t.Fatal("no template row became durable")
	}
}

func TestMinerSurvivesCrashBetweenCommitAndSnapshot(t *testing.T) {
	f := newLifecycleFixture(t, Bounds{})
	r := f.eng.NewReducer(f.clock.Now())
	r.ReduceLog(LogInput{
		Tenant: "acme", Service: "svc", Severity: "ERROR",
		Body: "payment gateway timeout after 30s", Timestamp: f.clock.Now(),
	})
	if _, err := f.eng.ApplyReducerErr(r); err != nil {
		t.Fatalf("ApplyReducerErr: %v", err)
	}
	before, _ := f.eng.Miner().Mine("acme", "svc", "ERROR", "payment gateway timeout after 30s")
	if before == 0 {
		t.Fatal("template id is zero")
	}

	// A crash here: no periodic statistics write ever runs. The identity is
	// durable regardless, because it rode the group commit.
	f.writer.Stop()
	if err := f.sqlite.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cold := openLifecycleFixture(t, f.path, Bounds{}, nil)
	restored, err := RestoreMiner(cold.store, cold.eng.Miner())
	if err != nil {
		t.Fatalf("RestoreMiner: %v", err)
	}
	if restored == 0 {
		t.Fatal("no template state survived the crash")
	}
	after, isOther := cold.eng.Miner().Mine("acme", "svc", "ERROR", "payment gateway timeout after 30s")
	if isOther {
		t.Fatal("the reloaded miner routed a known pattern to overflow")
	}
	if after != before {
		t.Fatalf("template id changed across restart: %d -> %d", before, after)
	}
}

func TestMinerReloadRebuildsEquivalentState(t *testing.T) {
	f := newLifecycleFixture(t, Bounds{})
	bodies := []string{
		"GET /api/users/42 took 13ms",
		"GET /api/users/1337 took 91ms",
		"cache miss for key ab12cd34ef56ab78",
		"worker 3 finished batch",
	}
	want := make([]uint32, len(bodies))
	for i, body := range bodies {
		r := f.eng.NewReducer(f.clock.Now())
		r.ReduceLog(LogInput{Tenant: "acme", Service: "svc", Severity: "INFO", Body: body, Timestamp: f.clock.Now()})
		if _, err := f.eng.ApplyReducerErr(r); err != nil {
			t.Fatalf("ApplyReducerErr: %v", err)
		}
		want[i], _ = f.eng.Miner().Mine("acme", "svc", "INFO", body)
	}
	f.writer.SaveTemplateStats()

	cold := f.restart(Bounds{})
	if _, err := RestoreMiner(cold.store, cold.eng.Miner()); err != nil {
		t.Fatalf("RestoreMiner: %v", err)
	}
	rowsBefore := countRows(t, cold.sqlite, "aggregate_log_template")
	for i, body := range bodies {
		got, _ := cold.eng.Miner().Mine("acme", "svc", "INFO", body)
		if got != want[i] {
			t.Fatalf("body %q: template id %d after reload, want %d", body, got, want[i])
		}
	}
	if rowsAfter := countRows(t, cold.sqlite, "aggregate_log_template"); rowsAfter != rowsBefore {
		t.Fatalf("replaying known bodies minted new template rows: %d -> %d", rowsBefore, rowsAfter)
	}
}

// --- schema policy -----------------------------------------------------------

func TestStoreV4FileIsRefusedThenRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	store := newTestStoreAt(t, path, StoreConfig{})
	seed(t, store, &GroupBatch{Dicts: []DictRow{dictRow(1, GlobalTenant, KindTenant, "acme")}})
	if _, err := store.writer.Exec(
		`UPDATE aggregate_meta SET value = '4' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("stamp v4: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := OpenSQLiteStore(StoreConfig{Path: path})
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("opening a v4 file: got %v, want *SchemaError", err)
	}
	if schemaErr.Got != "4" || schemaErr.Want != "5" {
		t.Fatalf("SchemaError = %+v, want a 4 -> 5 mismatch", schemaErr)
	}

	rebuilt := newTestStoreAt(t, path, StoreConfig{AllowRebuild: true})
	if n := countRows(t, rebuilt, "aggregate_dict"); n != 0 {
		t.Fatalf("rebuild kept %d dictionary rows; the rebuild path destroys aggregate data by design", n)
	}
	if n := countRows(t, rebuilt, "aggregate_log_template"); n != 0 {
		t.Fatalf("rebuilt store has %d template rows, want 0", n)
	}
	dictWM, seriesWM, err := rebuilt.Watermarks()
	if err != nil {
		t.Fatalf("Watermarks: %v", err)
	}
	if dictWM != 1 || seriesWM != 1 {
		t.Fatalf("fresh watermarks = (%d, %d), want (1, 1)", dictWM, seriesWM)
	}
}
