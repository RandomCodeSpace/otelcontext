package storage

import (
	"sync/atomic"
	"testing"
)

// Disk watchdog thresholds and hysteresis (#201 Q5). A fake statfs makes the
// ladder deterministic; a real volume would make it a coin flip.

const testBudget = int64(8 * 1024 * 1024 * 1024) // 8 GiB

// fakeVolume is a settable statfs source sized to the budget, so a usage
// fraction maps directly onto the enforcement ratio.
type fakeVolume struct {
	total int64
	used  atomic.Int64
	err   error
}

func newFakeVolume(total int64) *fakeVolume { return &fakeVolume{total: total} }

func (f *fakeVolume) setRatio(r float64) {
	f.used.Store(int64(float64(f.total) * r))
}

func (f *fakeVolume) stat(string) (DiskUsage, error) {
	if f.err != nil {
		return DiskUsage{}, f.err
	}
	used := f.used.Load()
	return DiskUsage{TotalBytes: f.total, AvailBytes: f.total - used}, nil
}

// watchdogAt builds a watchdog over a fake volume at the given usage fraction.
func watchdogAt(ratio float64) (*DiskWatchdog, *fakeVolume) {
	vol := newFakeVolume(testBudget)
	vol.setRatio(ratio)
	w := NewDiskWatchdog(DiskWatchdogConfig{
		Path:        "/does/not/exist",
		BudgetBytes: testBudget,
		Stat:        vol.stat,
	})
	return w, vol
}

// TestNextStateLadder pins the entry and exit thresholds. The table IS the
// contract: 90/95 to enter, 90/85 to leave.
func TestNextStateLadder(t *testing.T) {
	cases := []struct {
		cur   SheddingState
		ratio float64
		want  SheddingState
	}{
		// Entry.
		{SheddingNone, 0.50, SheddingNone},
		{SheddingNone, 0.8999, SheddingNone},
		{SheddingNone, 0.90, SheddingErrorsOnly},
		{SheddingNone, 0.9499, SheddingErrorsOnly},
		{SheddingNone, 0.95, SheddingRawOff},
		{SheddingNone, 1.00, SheddingRawOff},
		// Hysteresis: raw-off holds until below 90%.
		{SheddingRawOff, 0.949, SheddingRawOff},
		{SheddingRawOff, 0.90, SheddingRawOff},
		{SheddingRawOff, 0.8999, SheddingErrorsOnly},
		// Hysteresis: errors-only holds until below 85%.
		{SheddingErrorsOnly, 0.899, SheddingErrorsOnly},
		{SheddingErrorsOnly, 0.85, SheddingErrorsOnly},
		{SheddingErrorsOnly, 0.8499, SheddingNone},
		// The gap between the two exits is the whole point: at 87% a
		// recovering raw-off lands on errors-only rather than none.
		{SheddingRawOff, 0.87, SheddingErrorsOnly},
	}
	for _, tc := range cases {
		if got := nextState(tc.cur, tc.ratio); got != tc.want {
			t.Errorf("nextState(%s, %.4f) = %s, want %s", tc.cur, tc.ratio, got, tc.want)
		}
	}
}

// TestWatchdogTransitionsAndHysteresis walks a volume up and back down and
// checks the observed sequence, which is what an operator sees.
func TestWatchdogTransitionsAndHysteresis(t *testing.T) {
	w, vol := watchdogAt(0.10)
	var seen []string
	w.AddObserver(func(s SheddingState) { seen = append(seen, s.String()) })
	var rawOffCalls int
	w.onRawOff = func() { rawOffCalls++ }

	steps := []struct {
		ratio float64
		want  SheddingState
	}{
		{0.10, SheddingNone},
		{0.91, SheddingErrorsOnly},
		{0.96, SheddingRawOff},
		{0.92, SheddingRawOff},     // above 90%: no recovery from raw-off
		{0.88, SheddingErrorsOnly}, // below 90%: one rung down only
		{0.86, SheddingErrorsOnly}, // above 85%: errors-only holds
		{0.20, SheddingNone},
	}
	for _, st := range steps {
		vol.setRatio(st.ratio)
		if got := w.Sample(); got != st.want {
			t.Fatalf("at %.2f: state = %s, want %s", st.ratio, got, st.want)
		}
	}

	want := []string{"errors_only", "raw_off", "errors_only", "none"}
	if len(seen) != len(want) {
		t.Fatalf("observed transitions %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("observed transitions %v, want %v", seen, want)
		}
	}
	if rawOffCalls != 1 {
		t.Fatalf("OnRawOff fired %d times, want exactly 1 (once per transition INTO raw-off)", rawOffCalls)
	}
}

// TestWatchdogHealthyOnlyFalseAtRawOff: readiness flips at 95%, not at 90%.
// Errors-only is degraded coverage, not an unready process.
func TestWatchdogHealthyOnlyFalseAtRawOff(t *testing.T) {
	w, vol := watchdogAt(0.50)
	if w.Sample(); !w.Healthy() {
		t.Fatal("healthy volume reported unready")
	}
	vol.setRatio(0.91)
	if w.Sample(); !w.Healthy() {
		t.Fatal("errors-only reported unready; only raw-off should")
	}
	vol.setRatio(0.99)
	if w.Sample(); w.Healthy() {
		t.Fatal("raw-off reported ready")
	}
}

// TestWatchdogCeilingIsTheLowerOfBudgetAndVolume: a 4 GiB volume does not
// become 8 GiB because the config says so.
func TestWatchdogCeilingIsTheLowerOfBudgetAndVolume(t *testing.T) {
	small := int64(4 * 1024 * 1024 * 1024)
	vol := newFakeVolume(small)
	vol.setRatio(0.96) // 96% of the REAL volume, 48% of the configured budget
	w := NewDiskWatchdog(DiskWatchdogConfig{Path: "/x", BudgetBytes: testBudget, Stat: vol.stat})
	if got := w.Sample(); got != SheddingRawOff {
		t.Fatalf("state = %s at 96%% of a volume smaller than the budget, want raw_off", got)
	}
	if r := w.UsedRatio(); r < 0.95 {
		t.Fatalf("used ratio = %.3f, want >= 0.95 — the ceiling was not clamped to the volume", r)
	}
}

// TestWatchdogStatFailureHoldsState: shedding raw retention because a syscall
// failed would be an outage caused by the safety mechanism.
func TestWatchdogStatFailureHoldsState(t *testing.T) {
	w, vol := watchdogAt(0.96)
	if got := w.Sample(); got != SheddingRawOff {
		t.Fatalf("state = %s, want raw_off", got)
	}
	vol.err = errStatFailed
	if got := w.Sample(); got != SheddingRawOff {
		t.Fatalf("state = %s after a failed stat, want the held raw_off", got)
	}
	vol.err = nil
	vol.setRatio(0.10)
	if got := w.Sample(); got != SheddingNone {
		t.Fatalf("state = %s once stat recovered, want none", got)
	}
}

// TestWatchdogComponentHighWaterMarks: the budget table (#201 Q1) is validated
// against peaks, and an instantaneous gauge does not remember the peak.
func TestWatchdogComponentHighWaterMarks(t *testing.T) {
	w, _ := watchdogAt(0.10)
	var size int64 = 100
	w.AddComponent("main_db", func() int64 { return atomic.LoadInt64(&size) })
	w.AddComponent("negative", func() int64 { return -5 })

	w.Sample()
	atomic.StoreInt64(&size, 900)
	w.Sample()
	atomic.StoreInt64(&size, 300)
	w.Sample()

	if got := w.HighWater("main_db"); got != 900 {
		t.Fatalf("main_db high water = %d, want 900", got)
	}
	if got := w.HighWater("negative"); got != 0 {
		t.Fatalf("a negative sample became a high water mark of %d, want 0", got)
	}
	if got := w.HighWater("absent"); got != 0 {
		t.Fatalf("unknown component reported %d, want 0", got)
	}
}

// TestNilWatchdogAccessors: legacy deployments carry no watchdog.
func TestNilWatchdogAccessors(t *testing.T) {
	var w *DiskWatchdog
	if w.State() != SheddingNone {
		t.Fatal("nil watchdog reported a shedding state")
	}
	if w.UsedRatio() != 0 {
		t.Fatal("nil watchdog reported usage")
	}
}

// errStatFailed stands in for a statfs syscall failure.
var errStatFailed = errStatfsTest{}

type errStatfsTest struct{}

func (errStatfsTest) Error() string { return "statfs: simulated failure" }
