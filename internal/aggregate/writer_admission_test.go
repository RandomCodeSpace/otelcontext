package aggregate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWriterSynchronized600Exports(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	eng := newTestEngine(t, clock, nil)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	hooked := &hookStore{Store: base, beforeCommit: func(*GroupBatch) { <-release }}
	w := newTestWriter(t, hooked, eng, clock, WriterConfig{})
	// Unblock before newTestWriter's shutdown cleanup, including on failure.
	t.Cleanup(unblock)
	const callers = 200 * 3
	start := make(chan struct{})
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			<-start
			_, err := w.ApplyErr(deltaFor(clock.Now(), uint32(i+1), 1))
			results <- err
		}(i)
	}
	close(start)
	waitFor(t, func() bool { return w.Stats().Waiters == callers || len(results) != 0 })
	select {
	case err := <-results:
		t.Fatalf("export completed before commit release: %v; stats=%+v", err, w.Stats())
	default:
	}
	if got := w.Stats(); got.Waiters != callers || got.Rejections != 0 {
		t.Fatalf("synchronized burst not admitted: %+v", got)
	}
	if count, _ := eng.Snapshot().Totals(SignalTraceOp); count != 0 {
		t.Fatalf("applied %d points before durable commit", count)
	}

	// Shutdown must drain the accepted burst. Expiring its caller's deadline
	// must not cancel or falsely acknowledge the pending durable writes.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked shutdown = %v, want context.Canceled", err)
	}
	unblock()
	for i := 0; i < callers; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("export %d: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("accepted export did not drain")
		}
	}
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := base.ReplayMutable(0)
	if err != nil {
		t.Fatal(err)
	}
	var durable uint64
	for _, row := range rows {
		durable += row.Delta.Count
	}
	if count, _ := eng.Snapshot().Totals(SignalTraceOp); count != callers || durable != callers {
		t.Fatalf("applied=%d durable=%d, want %d each", count, durable, callers)
	}
	if got := w.Stats(); got.Waiters != 0 || got.PendingBytes != 0 || got.PendingDeltas != 0 || got.Rejections != 0 {
		t.Fatalf("writer not drained: %+v", got)
	}
}

func TestWriterConfiguredWaiterOverflow(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	eng := newTestEngine(t, clock, nil)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	hooked := &hookStore{Store: base, beforeCommit: func(*GroupBatch) { <-release }}
	const capacity = 640
	w := newTestWriter(t, hooked, eng, clock, WriterConfig{MaxWaiters: capacity})
	t.Cleanup(unblock)
	results := make(chan error, capacity)
	for i := 0; i < capacity; i++ {
		go func() {
			_, err := w.ApplyErr(deltaFor(clock.Now(), 1, 1))
			results <- err
		}()
	}
	waitFor(t, func() bool { return w.Stats().Waiters == capacity })
	for i := 0; i < 2; i++ {
		_, err := w.ApplyErr(deltaFor(clock.Now(), 2, 1))
		var sat *SaturationError
		if !errors.As(err, &sat) || sat.Bound != "waiters" || sat.Limit != capacity {
			t.Fatalf("overflow = %v, want waiter saturation at %d", err, capacity)
		}
	}
	st := w.Stats()
	if st.Waiters != capacity || st.PendingDeltas != capacity || st.Rejections != 2 || st.AdmissionRatio() != 1 {
		t.Fatalf("overflow changed admission accounting: %+v", st)
	}
	unblock()
	for i := 0; i < capacity; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("accepted export did not drain")
		}
	}
	if count, _ := eng.Snapshot().Totals(SignalTraceOp); count != capacity {
		t.Fatalf("applied %d points, want %d", count, capacity)
	}
}
