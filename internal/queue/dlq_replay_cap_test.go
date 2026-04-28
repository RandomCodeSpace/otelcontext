package queue

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// errReplayFailed is the sentinel used by tests below to force replay failure.
var errReplayFailed = errors.New("simulated replay failure")

// TestDLQ_MaxReplayPerTick_BoundsAttempts verifies that SetMaxReplayPerTick
// caps replayFn invocations within a single processFiles call. Without
// this cap, a 10k-file backlog after an outage would replay every file in
// the first post-restart tick and hammer the just-restarted DB.
func TestDLQ_MaxReplayPerTick_BoundsAttempts(t *testing.T) {
	dir := t.TempDir()

	var attempts atomic.Int64
	failingReplay := func([]byte) error {
		attempts.Add(1)
		return errReplayFailed
	}

	// Long interval so the background ticker doesn't fire during the test;
	// we drive processFiles manually to make assertions deterministic.
	q, err := NewDLQWithLimits(dir, time.Hour, failingReplay, 0, 0, 0)
	if err != nil {
		t.Fatalf("NewDLQWithLimits: %v", err)
	}
	defer q.Stop()

	const total = 50
	const replayCap = 10
	q.SetMaxReplayPerTick(replayCap)

	for i := range total {
		if err := q.Enqueue(map[string]int{"i": i}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	q.processFiles()
	if got := attempts.Load(); got != int64(replayCap) {
		t.Fatalf("expected %d replay attempts (cap=%d), got %d", replayCap, replayCap, got)
	}
}

// TestDLQ_MaxReplayPerTick_DisabledByDefault verifies that with the cap
// unset (0) processFiles attempts every queued file in a single tick —
// preserving legacy behaviour for callers that don't opt in.
func TestDLQ_MaxReplayPerTick_DisabledByDefault(t *testing.T) {
	dir := t.TempDir()

	var attempts atomic.Int64
	failingReplay := func([]byte) error {
		attempts.Add(1)
		return errReplayFailed
	}

	q, err := NewDLQWithLimits(dir, time.Hour, failingReplay, 0, 0, 0)
	if err != nil {
		t.Fatalf("NewDLQWithLimits: %v", err)
	}
	defer q.Stop()

	const total = 25
	for i := range total {
		if err := q.Enqueue(map[string]int{"i": i}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	q.processFiles()
	if got := attempts.Load(); got != int64(total) {
		t.Fatalf("with no cap expected %d attempts, got %d", total, got)
	}
}

// TestDLQ_MaxReplayPerTick_NegativeNormalisesToZero ensures a negative
// argument is treated as "unlimited" rather than blocking all replay.
func TestDLQ_MaxReplayPerTick_NegativeNormalisesToZero(t *testing.T) {
	dir := t.TempDir()
	noop := func([]byte) error { return nil }
	q, err := NewDLQWithLimits(dir, time.Hour, noop, 0, 0, 0)
	if err != nil {
		t.Fatalf("NewDLQWithLimits: %v", err)
	}
	defer q.Stop()

	q.SetMaxReplayPerTick(-5)
	q.mu.Lock()
	got := q.maxReplayPerTick
	q.mu.Unlock()
	if got != 0 {
		t.Fatalf("expected -5 to clamp to 0 (unlimited), got %d", got)
	}
}
