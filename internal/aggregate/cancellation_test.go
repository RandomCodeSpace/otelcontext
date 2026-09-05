package aggregate

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type countingReadStore struct {
	*stubStore
	sumCalls atomic.Int32
}

func (s *countingReadStore) SumBuckets(ctx context.Context, sel Selector, by GroupBy) ([]SumRow, error) {
	s.sumCalls.Add(1)
	return s.stubStore.SumBuckets(ctx, sel, by)
}

func TestQueryDashboardCancelledBeforePlanningDoesNotReadStore(t *testing.T) {
	f := newQueryFixture(t)
	store := &countingReadStore{stubStore: newStubStore()}
	f.engine.SetStore(store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.engine.QueryDashboard(ctx, f.rangeQuery())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("QueryDashboard error = %v, want context.Canceled", err)
	}
	if got := store.sumCalls.Load(); got != 0 {
		t.Fatalf("SumBuckets calls = %d, want 0", got)
	}
}

type joinedFanoutStore struct {
	*stubStore
	want       int32
	failStart  int64
	failure    error
	allStarted chan struct{}
	started    atomic.Int32
	finished   atomic.Int32
}

func newJoinedFanoutStore(sel Selector, failure error) *joinedFanoutStore {
	return &joinedFanoutStore{
		stubStore:  newStubStore(),
		want:       int32(len(splitSelector(sel))),
		failStart:  sel.Start,
		failure:    failure,
		allStarted: make(chan struct{}),
	}
}

func (s *joinedFanoutStore) run(ctx context.Context, sel Selector) error {
	if s.started.Add(1) == s.want {
		close(s.allStarted)
	}
	defer s.finished.Add(1)

	select {
	case <-s.allStarted:
	case <-ctx.Done():
		return ctx.Err()
	}
	if sel.Start == s.failStart {
		return s.failure
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *joinedFanoutStore) SumBuckets(ctx context.Context, sel Selector, _ GroupBy) ([]SumRow, error) {
	return nil, s.run(ctx, sel)
}

func (s *joinedFanoutStore) VisitSketches(ctx context.Context, sel Selector, _ func(uint32, *Sketch) error) error {
	return s.run(ctx, sel)
}

func wideFanoutSelector() Selector {
	return Selector{
		TenantID: 1,
		Start:    0,
		End:      int64(storeReadFanout*storeReadChunkMinWindows) * int64(WindowSize/time.Second),
	}
}

func assertFanoutJoined(t *testing.T, store *joinedFanoutStore) {
	t.Helper()
	if got := store.started.Load(); got != store.want {
		t.Fatalf("started workers = %d, want %d", got, store.want)
	}
	if got := store.finished.Load(); got != store.want {
		t.Fatalf("finished workers at return = %d, want %d", got, store.want)
	}
}

func TestSumStoreCancelsAndJoinsSiblingReads(t *testing.T) {
	sel := wideFanoutSelector()
	boom := errors.New("sum failed")
	store := newJoinedFanoutStore(sel, boom)
	e := testEngine(t, time.Unix(sel.End, 0))
	e.SetStore(store)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := e.sumStore(ctx, sel, 0)
	if !errors.Is(err, boom) {
		t.Fatalf("sumStore error = %v, want %v", err, boom)
	}
	assertFanoutJoined(t, store)
}

func TestVisitStoreSketchesCancelsAndJoinsSiblingReads(t *testing.T) {
	sel := wideFanoutSelector()
	boom := errors.New("sketch scan failed")
	store := newJoinedFanoutStore(sel, boom)
	e := testEngine(t, time.Unix(sel.End, 0))
	e.SetStore(store)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := e.visitStoreSketches(ctx, sel, nil, func(uint32, *Sketch) {})
	if !errors.Is(err, boom) {
		t.Fatalf("visitStoreSketches error = %v, want %v", err, boom)
	}
	assertFanoutJoined(t, store)
}

func TestSQLiteSumBucketsPoolWaitHonorsCancellation(t *testing.T) {
	store := newTestStoreAt(t, filepath.Join(t.TempDir(), "aggregate.db"), StoreConfig{ReadPoolSize: 1})
	held, err := store.reader.Conn(context.Background())
	if err != nil {
		t.Fatalf("hold reader connection: %v", err)
	}
	defer func() { _ = held.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := store.SumBuckets(ctx, Selector{TenantID: 1, Start: 300, End: 600}, 0)
		result <- err
	}()

	waitDeadline := time.NewTimer(2 * time.Second)
	defer waitDeadline.Stop()
	waitTick := time.NewTicker(time.Millisecond)
	defer waitTick.Stop()
	for store.reader.Stats().WaitCount == 0 {
		select {
		case <-waitTick.C:
		case <-waitDeadline.C:
			t.Fatal("SumBuckets did not wait for the exhausted read pool")
		}
	}

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SumBuckets error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SumBuckets did not release the pool waiter after cancellation")
	}

	if err := held.Close(); err != nil {
		t.Fatalf("release held connection: %v", err)
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), time.Second)
	defer pingCancel()
	if err := store.reader.PingContext(pingCtx); err != nil {
		t.Fatalf("read pool after cancellation: %v", err)
	}
}

func TestSQLiteVisitSketchesCancellationStopsIterationAndReleasesConnection(t *testing.T) {
	store := newTestStoreAt(t, filepath.Join(t.TempDir(), "aggregate.db"), StoreConfig{ReadPoolSize: 1})
	if err := store.CommitGroup(&GroupBatch{
		Series: []SeriesRow{{ID: 1, Key: storeKey(1)}, {ID: 2, Key: storeKey(2)}},
		Deltas: []DeltaRow{
			{SeriesID: 1, WindowStart: 300, Delta: spanDelta(1, 100)},
			{SeriesID: 2, WindowStart: 300, Delta: spanDelta(1, 200)},
		},
	}); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	visits := 0
	err := store.VisitSketches(ctx, Selector{TenantID: 1, Start: 300, End: 600}, func(uint32, *Sketch) error {
		visits++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("VisitSketches error = %v, want context.Canceled", err)
	}
	if visits != 1 {
		t.Fatalf("visitor calls = %d, want 1", visits)
	}

	pingCtx, pingCancel := context.WithTimeout(context.Background(), time.Second)
	defer pingCancel()
	if err := store.reader.PingContext(pingCtx); err != nil {
		t.Fatalf("read pool after canceled sketch scan: %v", err)
	}
}
