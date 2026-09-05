package aggregate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore opens a store in a temp directory and closes it on cleanup.
func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	return newTestStoreAt(t, filepath.Join(t.TempDir(), "aggregate.db"), StoreConfig{})
}

func newTestStoreAt(t testing.TB, path string, cfg StoreConfig) *SQLiteStore {
	t.Helper()
	cfg.Path = path
	store, err := OpenSQLiteStore(cfg)
	if err != nil {
		t.Fatalf("OpenSQLiteStore(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// storeKey builds a distinct trace-operation series key.
func storeKey(n uint32) SeriesKey {
	return SeriesKey{
		TenantID:    1,
		ServiceID:   2,
		NameID:      n,
		Signal:      SignalTraceOp,
		StatusClass: StatusOK,
		HTTPClass:   HTTPClass2xx,
		Method:      MethodGet,
		Variant:     SpanKindServer,
	}
}

// spanDelta builds a delta carrying counters and a sketch.
func spanDelta(count int, micros float64) *AggregateDelta {
	d := &AggregateDelta{}
	for i := 0; i < count; i++ {
		d.ObserveSpan(micros, i%3 == 0, true)
	}
	return d
}

func TestStoreCreateVerifyReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	store := newTestStoreAt(t, path, StoreConfig{})
	if store.UUID() == "" {
		t.Fatal("store uuid is empty")
	}
	first := store.UUID()
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := newTestStoreAt(t, path, StoreConfig{})
	if reopened.UUID() != first {
		t.Fatalf("uuid changed across reopen: %q -> %q", first, reopened.UUID())
	}
}

func TestStoreVersionMismatchFailsStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	store := newTestStoreAt(t, path, StoreConfig{})
	if _, err := store.writer.Exec(
		`UPDATE aggregate_meta SET value = ? WHERE key = 'schema_version'`, StoreSchemaVersion+1); err != nil {
		t.Fatalf("tamper meta: %v", err)
	}
	_ = store.Close()

	_, err := OpenSQLiteStore(StoreConfig{Path: path})
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("open with mismatched schema_version: got %v, want *SchemaError", err)
	}
	if schemaErr.Key != "schema_version" {
		t.Fatalf("mismatch key = %q, want schema_version", schemaErr.Key)
	}

	rebuilt, err := OpenSQLiteStore(StoreConfig{Path: path, AllowRebuild: true})
	if err != nil {
		t.Fatalf("open with AGGREGATE_ALLOW_REBUILD=true: %v", err)
	}
	defer func() { _ = rebuilt.Close() }()
	if rebuilt.UUID() == store.UUID() {
		t.Fatal("rebuild kept the old store uuid; it must mint a new identity")
	}
}

func TestStorePartialSchemaFailsStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	store := newTestStoreAt(t, path, StoreConfig{})
	if _, err := store.writer.Exec(`DROP TABLE aggregate_buckets`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	_ = store.Close()

	_, err := OpenSQLiteStore(StoreConfig{Path: path})
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) || schemaErr.Reason != "partial_schema" {
		t.Fatalf("open with partial schema: got %v, want partial_schema *SchemaError", err)
	}

	rebuilt, err := OpenSQLiteStore(StoreConfig{Path: path, AllowRebuild: true})
	if err != nil {
		t.Fatalf("rebuild partial schema: %v", err)
	}
	_ = rebuilt.Close()
}

func TestStoreRejectsBadSynchronous(t *testing.T) {
	_, err := OpenSQLiteStore(StoreConfig{
		Path:        filepath.Join(t.TempDir(), "aggregate.db"),
		Synchronous: "OFF",
	})
	if err == nil {
		t.Fatal("synchronous=OFF was accepted; the ACK contract forbids it")
	}
}

func TestCommitGroupRoundTrip(t *testing.T) {
	store := newTestStore(t)
	key := storeKey(10)
	batch := &GroupBatch{
		Dicts:  []DictRow{{ID: 10, TenantID: 1, Kind: KindOperation, Value: []byte("GET /orders")}},
		Series: []SeriesRow{{ID: 1, Key: key}},
		Deltas: []DeltaRow{{SeriesID: 1, WindowStart: 300, Delta: spanDelta(6, 1500)}},
		Baselines: []BaselineRow{{
			SeriesID: 1,
			Producer: ProducerID(0x1122334455667788),
			Baseline: Baseline{
				StartTime:     time.Unix(100, 0).UTC(),
				LastTimestamp: time.Unix(200, 0).UTC(),
				Value:         42,
			},
		}},
	}
	if err := store.CommitGroup(batch); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	rows, err := store.ReplayMutable(0)
	if err != nil {
		t.Fatalf("ReplayMutable: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("replayed %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.SeriesID != 1 || got.WindowStart != 300 {
		t.Fatalf("row identity = (%d,%d), want (1,300)", got.SeriesID, got.WindowStart)
	}
	want := batch.Deltas[0].Delta
	if got.Delta.Count != want.Count || got.Delta.ErrorCount != want.ErrorCount {
		t.Fatalf("counters = (%d,%d), want (%d,%d)",
			got.Delta.Count, got.Delta.ErrorCount, want.Count, want.ErrorCount)
	}
	if got.Delta.Sketch == nil {
		t.Fatal("sketch did not survive the round trip")
	}
	if got.Delta.Sketch.Count() != want.Sketch.Count() {
		t.Fatalf("sketch count = %d, want %d", got.Delta.Sketch.Count(), want.Sketch.Count())
	}

	infos, err := store.ResolveSeries([]SeriesID{1})
	if err != nil {
		t.Fatalf("ResolveSeries: %v", err)
	}
	if len(infos) != 1 || infos[0].Key != key {
		t.Fatalf("ResolveSeries = %+v, want key %+v", infos, key)
	}

	baselines, err := store.LoadBaselines(0)
	if err != nil {
		t.Fatalf("LoadBaselines: %v", err)
	}
	if len(baselines) != 1 || baselines[0].Producer != ProducerID(0x1122334455667788) {
		t.Fatalf("LoadBaselines = %+v", baselines)
	}
	if baselines[0].Baseline.Value != 42 {
		t.Fatalf("baseline value = %v, want 42", baselines[0].Baseline.Value)
	}

	dicts, err := store.LoadDict(0)
	if err != nil {
		t.Fatalf("LoadDict: %v", err)
	}
	if len(dicts) != 1 || string(dicts[0].Value) != "GET /orders" {
		t.Fatalf("LoadDict = %+v", dicts)
	}
}

func TestResolveSeriesRoundTripsEverySignal(t *testing.T) {
	store := newTestStore(t)
	keys := []SeriesKey{
		{TenantID: 1, ServiceID: 2, NameID: 3, Signal: SignalTraceOp, StatusClass: StatusError, HTTPClass: HTTPClass5xx, Method: MethodPost, Variant: SpanKindClient},
		{TenantID: 1, ServiceID: 2, NameID: 4, Signal: SignalServiceEdge, StatusClass: StatusOK, Variant: SpanKindServer},
		{TenantID: 1, ServiceID: 2, NameID: 5, Signal: SignalLog, StatusClass: SeverityTierError},
		{TenantID: 1, ServiceID: 2, NameID: 6, Signal: SignalMetric},
	}
	batch := &GroupBatch{}
	ids := make([]SeriesID, 0, len(keys))
	for i, k := range keys {
		id := SeriesID(i + 1)
		batch.Series = append(batch.Series, SeriesRow{ID: id, Key: k})
		ids = append(ids, id)
	}
	if err := store.CommitGroup(batch); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	infos, err := store.ResolveSeries(ids)
	if err != nil {
		t.Fatalf("ResolveSeries: %v", err)
	}
	byID := map[SeriesID]SeriesKey{}
	for _, info := range infos {
		byID[info.ID] = info.Key
	}
	for i, k := range keys {
		got, ok := byID[SeriesID(i+1)]
		if !ok {
			t.Fatalf("series %d missing from resolve", i+1)
		}
		if got != k {
			t.Fatalf("series %d round trip = %+v, want %+v", i+1, got, k)
		}
	}
}

// TestCommitGroupAllOrNothing proves the batch is one transaction: a failure in
// the FIRST phase (dictionary) must leave the series, delta and baseline rows
// of the SAME batch absent.
func TestCommitGroupAllOrNothing(t *testing.T) {
	store := newTestStore(t)
	bad := &GroupBatch{
		// The schema's length CHECK rejects an empty dictionary value; the
		// durable registrar never mints one, so this is the failure injection
		// a corrupt row would produce.
		Dicts:     []DictRow{{ID: 1, TenantID: 1, Kind: KindOperation, Value: []byte{}}},
		Series:    []SeriesRow{{ID: 1, Key: storeKey(1)}},
		Deltas:    []DeltaRow{{SeriesID: 1, WindowStart: 300, Delta: spanDelta(3, 500)}},
		Baselines: []BaselineRow{{SeriesID: 1, Producer: 7, Baseline: Baseline{Value: 1}}},
	}
	if err := store.CommitGroup(bad); err == nil {
		t.Fatal("CommitGroup accepted a NULL dictionary value")
	}
	assertEmpty(t, store, "aggregate_dict")
	assertEmpty(t, store, "aggregate_series")
	assertEmpty(t, store, "aggregate_delta_log")
	assertEmpty(t, store, "aggregate_baseline")

	// The store is still usable afterwards.
	good := &GroupBatch{
		Dicts:  []DictRow{{ID: 1, TenantID: 1, Kind: KindOperation, Value: []byte("ok")}},
		Series: []SeriesRow{{ID: 1, Key: storeKey(1)}},
		Deltas: []DeltaRow{{SeriesID: 1, WindowStart: 300, Delta: spanDelta(3, 500)}},
	}
	if err := store.CommitGroup(good); err != nil {
		t.Fatalf("CommitGroup after rollback: %v", err)
	}
	assertCount(t, store, "aggregate_delta_log", 1)
}

func TestFinalizeWindowMaterializesAndDeletes(t *testing.T) {
	store := newTestStore(t)
	seed := &GroupBatch{Series: []SeriesRow{{ID: 1, Key: storeKey(1)}, {ID: 2, Key: storeKey(2)}}}
	if err := store.CommitGroup(seed); err != nil {
		t.Fatalf("seed series: %v", err)
	}
	// Three commits touching two series in one window. The delta log is keyed
	// (window, series), so the three commits merge into two rows rather than
	// six — that is what the finalizer's transaction is sized by (#173).
	for i := 0; i < 3; i++ {
		batch := &GroupBatch{Deltas: []DeltaRow{
			{SeriesID: 1, WindowStart: 600, Delta: spanDelta(2, 100)},
			{SeriesID: 2, WindowStart: 600, Delta: spanDelta(1, 900)},
		}}
		if err := store.CommitGroup(batch); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	assertCount(t, store, "aggregate_delta_log", 2)

	stats, err := store.FinalizeWindow(600)
	if err != nil {
		t.Fatalf("FinalizeWindow: %v", err)
	}
	if stats.Buckets != 2 || stats.DeltaRows != 2 {
		t.Fatalf("finalize stats = %+v, want 2 buckets / 2 delta rows", stats)
	}
	assertCount(t, store, "aggregate_delta_log", 0)
	assertCount(t, store, "aggregate_buckets", 2)

	page, err := store.ReadBuckets(context.Background(), Selector{TenantID: 1, Start: 300, End: 900})
	if err != nil {
		t.Fatalf("ReadBuckets: %v", err)
	}
	buckets := page.Buckets
	if len(buckets) != 2 {
		t.Fatalf("read %d buckets, want 2", len(buckets))
	}
	for _, b := range buckets {
		switch b.SeriesID {
		case 1:
			if b.Delta.Count != 6 {
				t.Fatalf("series 1 count = %d, want 6", b.Delta.Count)
			}
		case 2:
			if b.Delta.Count != 3 {
				t.Fatalf("series 2 count = %d, want 3", b.Delta.Count)
			}
		}
		if b.Delta.Sketch == nil {
			t.Fatalf("series %d lost its sketch through finalize", b.SeriesID)
		}
	}
}

// TestFinalizeWindowMergesIntoExistingBucket covers the second finalize of the
// same window — which is what a late delta arriving after a downtime-expiry
// finalization produces.
func TestFinalizeWindowMergesIntoExistingBucket(t *testing.T) {
	store := newTestStore(t)
	if err := store.CommitGroup(&GroupBatch{
		Series: []SeriesRow{{ID: 1, Key: storeKey(1)}},
		Deltas: []DeltaRow{{SeriesID: 1, WindowStart: 600, Delta: spanDelta(4, 100)}},
	}); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if _, err := store.FinalizeWindow(600); err != nil {
		t.Fatalf("first finalize: %v", err)
	}
	if err := store.CommitGroup(&GroupBatch{
		Deltas: []DeltaRow{{SeriesID: 1, WindowStart: 600, Delta: spanDelta(3, 200)}},
	}); err != nil {
		t.Fatalf("late commit: %v", err)
	}
	if _, err := store.FinalizeWindow(600); err != nil {
		t.Fatalf("second finalize: %v", err)
	}
	page, err := store.ReadBuckets(context.Background(), Selector{TenantID: 1, Start: 600, End: 900})
	if err != nil {
		t.Fatalf("ReadBuckets: %v", err)
	}
	buckets := page.Buckets
	if len(buckets) != 1 || buckets[0].Delta.Count != 7 {
		t.Fatalf("merged bucket = %+v, want count 7", buckets)
	}
}

// TestFinalizeWindowAtomicity kills the write half of the transaction (by
// closing the writer pool) and proves the delta rows survive un-deleted and no
// bucket was written: materialize+delete is one transaction or neither.
func TestFinalizeWindowAtomicity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aggregate.db")
	store := newTestStoreAt(t, path, StoreConfig{})
	if err := store.CommitGroup(&GroupBatch{
		Series: []SeriesRow{{ID: 1, Key: storeKey(1)}},
		Deltas: []DeltaRow{{SeriesID: 1, WindowStart: 600, Delta: spanDelta(4, 100)}},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := store.writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if _, err := store.FinalizeWindow(600); err == nil {
		t.Fatal("FinalizeWindow succeeded with a closed writer")
	}
	_ = store.Close()

	reopened := newTestStoreAt(t, path, StoreConfig{})
	assertCount(t, reopened, "aggregate_delta_log", 1)
	assertCount(t, reopened, "aggregate_buckets", 0)
}

func TestReadBucketsEnforcesBounds(t *testing.T) {
	store := newTestStore(t)
	cases := []struct {
		name string
		sel  Selector
	}{
		{"empty", Selector{}},
		{"no range", Selector{TenantID: 1}},
		{"backwards range", Selector{TenantID: 1, Start: 900, End: 600}},
		{"span too wide", Selector{TenantID: 1, Start: 0 + 1, End: MaxReadWindowSpan + 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.ReadBuckets(context.Background(), tc.sel); !errors.Is(err, ErrSelectorUnbounded) {
				t.Fatalf("ReadBuckets(%+v) = %v, want ErrSelectorUnbounded", tc.sel, err)
			}
		})
	}
}

func TestReadBucketsClampsLimitAndScopesTenant(t *testing.T) {
	store := newTestStore(t)
	batch := &GroupBatch{}
	for i := 1; i <= 5; i++ {
		key := storeKey(uint32(i))
		if i > 3 {
			key.TenantID = 2
		}
		batch.Series = append(batch.Series, SeriesRow{ID: SeriesID(i), Key: key})
		batch.Deltas = append(batch.Deltas, DeltaRow{SeriesID: SeriesID(i), WindowStart: 600, Delta: spanDelta(1, 10)})
	}
	if err := store.CommitGroup(batch); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	if _, err := store.FinalizeWindow(600); err != nil {
		t.Fatalf("FinalizeWindow: %v", err)
	}

	got, err := store.ReadBuckets(context.Background(), Selector{TenantID: 1, Start: 600, End: 900})
	if err != nil {
		t.Fatalf("ReadBuckets: %v", err)
	}
	if len(got.Buckets) != 3 {
		t.Fatalf("tenant 1 read %d buckets, want 3 (tenant 2 rows must not leak)", len(got.Buckets))
	}
	if got.Truncated {
		t.Fatal("an unlimited read of 3 rows reported truncation")
	}

	limited, err := store.ReadBuckets(context.Background(), Selector{TenantID: 1, Start: 600, End: 900, Limit: 2})
	if err != nil {
		t.Fatalf("ReadBuckets(limit): %v", err)
	}
	if len(limited.Buckets) != 2 {
		t.Fatalf("limit 2 returned %d rows", len(limited.Buckets))
	}
	// #194 blocker 4: a clamped read must SAY it was clamped.
	if !limited.Truncated || limited.Limit != 2 {
		t.Fatalf("limited read = {truncated:%v limit:%d}, want {true 2}", limited.Truncated, limited.Limit)
	}
	last := limited.Buckets[len(limited.Buckets)-1]
	if limited.Next.WindowStart != last.WindowStart || limited.Next.SeriesID != last.SeriesID {
		t.Fatalf("resume cursor %+v does not point at the last returned row %+v", limited.Next, last)
	}

	signalScoped, err := store.ReadBuckets(context.Background(), Selector{TenantID: 1, Start: 600, End: 900, Signal: SignalLog})
	if err != nil {
		t.Fatalf("ReadBuckets(signal): %v", err)
	}
	if len(signalScoped.Buckets) != 0 {
		t.Fatalf("log-scoped read returned %d trace buckets", len(signalScoped.Buckets))
	}
}

func TestResolveSeriesRejectsOversizedInput(t *testing.T) {
	store := newTestStore(t)
	ids := make([]SeriesID, MaxReadRows+1)
	if _, err := store.ResolveSeries(ids); !errors.Is(err, ErrSelectorUnbounded) {
		t.Fatalf("ResolveSeries(oversized) = %v, want ErrSelectorUnbounded", err)
	}
}

func TestReplayMutableSkipsFinalizedWindows(t *testing.T) {
	store := newTestStore(t)
	if err := store.CommitGroup(&GroupBatch{
		Series: []SeriesRow{{ID: 1, Key: storeKey(1)}},
		Deltas: []DeltaRow{
			{SeriesID: 1, WindowStart: 300, Delta: spanDelta(1, 10)},
			{SeriesID: 1, WindowStart: 1200, Delta: spanDelta(2, 10)},
		},
	}); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	rows, err := store.ReplayMutable(1200)
	if err != nil {
		t.Fatalf("ReplayMutable: %v", err)
	}
	if len(rows) != 1 || rows[0].WindowStart != 1200 {
		t.Fatalf("ReplayMutable(1200) = %+v, want only window 1200", rows)
	}
}

func TestPurgeBeforeDropsHistory(t *testing.T) {
	store := newTestStore(t)
	batch := &GroupBatch{Series: []SeriesRow{{ID: 1, Key: storeKey(1)}}}
	for _, w := range []int64{300, 600, 900} {
		batch.Deltas = append(batch.Deltas, DeltaRow{SeriesID: 1, WindowStart: w, Delta: spanDelta(1, 10)})
	}
	batch.Baselines = []BaselineRow{{
		SeriesID: 1,
		Producer: 5,
		Baseline: Baseline{LastTimestamp: time.Unix(300, 0).UTC(), Value: 1},
	}}
	if err := store.CommitGroup(batch); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	for _, w := range []int64{300, 600, 900} {
		if _, err := store.FinalizeWindow(w); err != nil {
			t.Fatalf("FinalizeWindow(%d): %v", w, err)
		}
	}

	stats, err := store.PurgeBefore(900)
	if err != nil {
		t.Fatalf("PurgeBefore: %v", err)
	}
	if stats.Buckets != 2 {
		t.Fatalf("purged %d buckets, want 2", stats.Buckets)
	}
	if stats.Baselines != 1 {
		t.Fatalf("purged %d baselines, want 1", stats.Baselines)
	}
	assertCount(t, store, "aggregate_buckets", 1)
}

func TestBacklogReportsOldestWindow(t *testing.T) {
	store := newTestStore(t)
	if err := store.CommitGroup(&GroupBatch{
		Series: []SeriesRow{{ID: 1, Key: storeKey(1)}},
		Deltas: []DeltaRow{
			{SeriesID: 1, WindowStart: 900, Delta: spanDelta(1, 10)},
			{SeriesID: 1, WindowStart: 600, Delta: spanDelta(1, 10)},
		},
	}); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	backlog, err := store.Backlog()
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if backlog.Rows != 2 || backlog.OldestWindow != 600 {
		t.Fatalf("backlog = %+v, want 2 rows / oldest 600", backlog)
	}
}

func TestFinalizableWindowsRespectsCutoff(t *testing.T) {
	store := newTestStore(t)
	if err := store.CommitGroup(&GroupBatch{
		Series: []SeriesRow{{ID: 1, Key: storeKey(1)}},
		Deltas: []DeltaRow{
			{SeriesID: 1, WindowStart: 600, Delta: spanDelta(1, 10)},
			{SeriesID: 1, WindowStart: 1200, Delta: spanDelta(1, 10)},
		},
	}); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	windows, err := store.FinalizableWindows(600, 0)
	if err != nil {
		t.Fatalf("FinalizableWindows: %v", err)
	}
	if len(windows) != 1 || windows[0] != 600 {
		t.Fatalf("FinalizableWindows(600) = %v, want [600]", windows)
	}
}

func TestProducerIDRoundTrip(t *testing.T) {
	for _, p := range []ProducerID{0, 1, 0xdeadbeefcafef00d, ^ProducerID(0)} {
		if got := producerFromBytes(producerBytes(p)); got != p {
			t.Fatalf("producer round trip %d -> %d", p, got)
		}
	}
}

// assertCount fails unless the table holds exactly n rows.
func assertCount(t *testing.T, s *SQLiteStore, table string, n int) {
	t.Helper()
	var got int
	if err := s.reader.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != n {
		t.Fatalf("%s holds %d rows, want %d", table, got, n)
	}
}

func assertEmpty(t *testing.T, s *SQLiteStore, table string) {
	t.Helper()
	assertCount(t, s, table, 0)
}

// TestSumBucketsGroupsByNameWithinASignal covers the grouping QueryTopology's
// edge half rests on: one row per (service, name) under a single-signal
// selector. The name namespace is the signal's, so the selector pins the signal
// and the grouping never joins a NameID across two of them.
func TestSumBucketsGroupsByNameWithinASignal(t *testing.T) {
	store := newTestStore(t)
	edge := func(caller, callee uint32) SeriesKey {
		return SeriesKey{
			TenantID: 1, ServiceID: caller, NameID: callee,
			Signal: SignalServiceEdge, StatusClass: StatusOK, Variant: SpanKindClient,
		}
	}
	window := int64(1_800_000)
	if err := store.CommitGroup(&GroupBatch{
		Series: []SeriesRow{
			{ID: 1, Key: edge(10, 20)},
			{ID: 2, Key: edge(10, 30)},
			{ID: 3, Key: storeKey(20)},
		},
		Deltas: []DeltaRow{
			{SeriesID: 1, WindowStart: window, Delta: spanDelta(4, 100)},
			{SeriesID: 1, WindowStart: window + int64(WindowSize/time.Second), Delta: spanDelta(2, 100)},
			{SeriesID: 2, WindowStart: window, Delta: spanDelta(3, 100)},
			{SeriesID: 3, WindowStart: window, Delta: spanDelta(9, 100)},
		},
	}); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	sums, err := store.SumBuckets(context.Background(), Selector{
		TenantID: 1,
		Start:    window,
		End:      window + 4*int64(WindowSize/time.Second),
		Signal:   SignalServiceEdge,
	}, GroupByService|GroupByName)
	if err != nil {
		t.Fatalf("SumBuckets: %v", err)
	}
	got := make(map[uint32]uint64, len(sums))
	for _, r := range sums {
		if r.ServiceID != 10 {
			t.Fatalf("row %+v carries a service the selector excluded", r)
		}
		if r.WindowStart != 0 {
			t.Errorf("row %+v carries a window start without GroupByWindow", r)
		}
		got[r.NameID] += r.Count
	}
	if len(got) != 2 {
		t.Fatalf("grouped rows = %v, want one per callee", got)
	}
	// Both windows of the first edge collapse into its single group row.
	if got[20] != 6 || got[30] != 3 {
		t.Fatalf("grouped counts = %v, want {20: 6, 30: 3}", got)
	}
}
