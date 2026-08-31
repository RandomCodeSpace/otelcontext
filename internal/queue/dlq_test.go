package queue

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestDLQShutdownIsIdempotent(t *testing.T) {
	q, err := NewDLQ(t.TempDir(), time.Hour, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := q.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := q.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

// TestDLQ_ConcurrentEnqueue_NoFilenameCollision exercises 1000 parallel
// Enqueue() calls and verifies each one produced its own file. This guards
// against the previous regression where `batch_<unixnano>.json` could collide
// on coarse-resolution clocks or under heavy goroutine scheduling, silently
// overwriting failed batches.
func TestDLQ_ConcurrentEnqueue_NoFilenameCollision(t *testing.T) {
	dir := t.TempDir()

	// Never drain the queue during this test — we disable replay by pointing
	// the replay function at a no-op and setting a huge interval.
	noop := func([]byte) error { return nil }
	q, err := NewDLQWithLimits(dir, time.Hour, noop, 0, 0, 0)
	if err != nil {
		t.Fatalf("NewDLQ: %v", err)
	}
	defer q.Stop()

	const n = 1000
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if err := q.Enqueue(map[string]int{"i": i}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Enqueue failed: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var count int
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			count++
		}
	}
	if count != n {
		t.Fatalf("want %d files, got %d — collisions dropped %d batches", n, count, n-count)
	}
}
