package aggregate

import (
	"context"
	"testing"
	"time"
)

// The runtime-readiness surface (#194 finding 18). /ready consults these
// counters to decide whether a process that finished recovery can still
// serve, so what they say has to be exact: streaks that reset on success,
// an admission ratio that tracks the fullest bound, and a delta-log age that
// keeps growing when the finalize loop stops refreshing it.

func TestCommitFailureStreakResetsOnSuccess(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	store := &failableStore{Store: base}
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, store, eng, clock, WriterConfig{})

	if got := w.Stats().CommitFailureStreak; got != 0 {
		t.Fatalf("streak = %d before any commit, want 0", got)
	}

	store.failCommit.Store(true)
	for i := 1; i <= 3; i++ {
		if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), uint32(i), 1)); err == nil {
			t.Fatalf("apply %d succeeded while commits are refused", i)
		}
		if got := w.Stats().CommitFailureStreak; got != uint64(i) {
			t.Fatalf("streak after %d failures = %d, want %d", i, got, i)
		}
	}

	store.failCommit.Store(false)
	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 9, 1)); err != nil {
		t.Fatalf("ApplyDeltasErr after recovery: %v", err)
	}
	st := w.Stats()
	if st.CommitFailureStreak != 0 {
		t.Fatalf("streak = %d after a successful commit, want 0", st.CommitFailureStreak)
	}
	if st.CommitErrors != 3 {
		t.Fatalf("cumulative CommitErrors = %d, want 3", st.CommitErrors)
	}
}

func TestFinalizeFailureStreakResetsOnSuccess(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	store := &failableStore{Store: base}
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, store, eng, clock, WriterConfig{})

	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 5)); err != nil {
		t.Fatalf("ApplyDeltasErr: %v", err)
	}
	clock.Advance(WindowSize + AllowedLateness + time.Minute)

	store.failFinalize.Store(true)
	for i := 1; i <= 2; i++ {
		if n := w.FinalizeDue(clock.Now()); n != 0 {
			t.Fatalf("finalized %d windows while finalization is refused", n)
		}
		if got := w.Stats().FinalizeFailureStreak; got != uint64(i) {
			t.Fatalf("finalize streak after %d failures = %d, want %d", i, got, i)
		}
	}

	store.failFinalize.Store(false)
	if n := w.FinalizeDue(clock.Now()); n != 1 {
		t.Fatalf("finalized %d windows after recovery, want 1", n)
	}
	st := w.Stats()
	if st.FinalizeFailureStreak != 0 {
		t.Fatalf("finalize streak = %d after a successful pass, want 0", st.FinalizeFailureStreak)
	}
	if st.FinalizeErrors != 2 {
		t.Fatalf("cumulative FinalizeErrors = %d, want 2", st.FinalizeErrors)
	}
}

func TestAdmissionRatioTracksTheFullestBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats WriterStats
		want  float64
	}{
		{"empty", WriterStats{MaxPendingBytes: 100, MaxPendingDeltas: 10, MaxWaiters: 4}, 0},
		{"bytes", WriterStats{PendingBytes: 90, MaxPendingBytes: 100, MaxPendingDeltas: 10, MaxWaiters: 4}, 0.9},
		{"deltas", WriterStats{PendingDeltas: 8, MaxPendingBytes: 100, MaxPendingDeltas: 10, MaxWaiters: 4}, 0.8},
		{"waiters", WriterStats{Waiters: 3, MaxPendingBytes: 100, MaxPendingDeltas: 10, MaxWaiters: 4}, 0.75},
		{"fullest wins", WriterStats{PendingBytes: 50, PendingDeltas: 9, Waiters: 1, MaxPendingBytes: 100, MaxPendingDeltas: 10, MaxWaiters: 4}, 0.9},
		{"unbounded", WriterStats{PendingBytes: 50, PendingDeltas: 9, Waiters: 1}, 0},
	} {
		if got := tc.stats.AdmissionRatio(); got != tc.want {
			t.Errorf("%s: AdmissionRatio = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDeltaLogAgeCarriesSampleStaleness(t *testing.T) {
	now := time.Unix(3_000_000, 0).UTC()

	empty := WriterStats{BacklogSampledAt: now.Add(-time.Hour)}
	if got := empty.DeltaLogAge(now); got != 0 {
		t.Fatalf("empty backlog ages to %v, want 0", got)
	}

	fresh := WriterStats{DeltaLogAgeSeconds: 120, BacklogSampledAt: now}
	if got := fresh.DeltaLogAge(now); got != 120 {
		t.Fatalf("fresh sample = %v, want 120", got)
	}

	// A wedged finalize loop stops refreshing the sample. The age has to keep
	// growing anyway, or a stuck writer reads as healthy forever.
	stale := WriterStats{DeltaLogAgeSeconds: 120, BacklogSampledAt: now.Add(-10 * time.Minute)}
	if got := stale.DeltaLogAge(now); got != 720 {
		t.Fatalf("stale sample = %v, want 720", got)
	}

	unsampled := WriterStats{DeltaLogAgeSeconds: 120}
	if got := unsampled.DeltaLogAge(now); got != 120 {
		t.Fatalf("never-sampled clock = %v, want 120", got)
	}
}

func TestBacklogSampleIsCachedForReadinessProbes(t *testing.T) {
	clock := newClock(time.Unix(3_000_000, 0).UTC())
	base := newTestStore(t)
	eng := newTestEngine(t, clock, nil)
	w := newTestWriter(t, base, eng, clock, WriterConfig{})

	if st := w.Stats(); !st.BacklogSampledAt.IsZero() || st.DeltaLogRows != 0 {
		t.Fatalf("stats before the first sample = %+v, want an unsampled backlog", st)
	}

	if _, err := eng.ApplyDeltasErr(deltaFor(clock.Now(), 1, 5)); err != nil {
		t.Fatalf("ApplyDeltasErr: %v", err)
	}
	clock.Advance(2 * time.Minute)
	w.publishBacklog(clock.Now())

	st := w.Stats()
	if st.DeltaLogRows != 1 {
		t.Fatalf("DeltaLogRows = %d, want 1", st.DeltaLogRows)
	}
	if st.DeltaLogAgeSeconds != 120 {
		t.Fatalf("DeltaLogAgeSeconds = %v, want 120", st.DeltaLogAgeSeconds)
	}
	if !st.BacklogSampledAt.Equal(clock.Now()) {
		t.Fatalf("BacklogSampledAt = %v, want %v", st.BacklogSampledAt, clock.Now())
	}
}

func TestPingContextReportsStoreReachability(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.PingContext(ctx); err != nil {
		t.Fatalf("PingContext on an open store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.PingContext(ctx); err == nil {
		t.Fatal("PingContext on a closed store returned nil")
	}
}
