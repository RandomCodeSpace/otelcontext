package gatecore

import "testing"

// ledgerFor builds a ledger whose windows carry the given attempted/acked
// span contributions.
func ledgerFor(windowSecs int64, rows map[int64][2]int64) *AckLedger {
	l := &AckLedger{Schema: LedgerSchema, WindowSecs: windowSecs}
	for start, ab := range rows {
		c := LedgerCounts{AttemptedPoints: ab[0], AckedPoints: ab[1],
			AttemptedRequests: 1, AckedRequests: 1}
		l.Windows = append(l.Windows, LedgerWindow{
			WindowStart: start, Counts: c,
			BySignal: map[string]LedgerCounts{"spans": c},
		})
		l.Totals.Add(c)
	}
	return l
}

func TestCrashBoundExactWindowsMustMatchExactly(t *testing.T) {
	l := ledgerFor(300, map[int64][2]int64{
		1000: {500, 500},
		1300: {500, 500},
	})
	obs := map[int64]int64{1000: 500, 1300: 500}
	r := EvaluateCrashBounds(l, "spans", obs, 9_000, 9_100, 1000, 1600)
	if !r.Pass {
		t.Fatalf("exact windows that matched were failed: %+v", r.Windows)
	}
	if r.WindowsExact != 2 {
		t.Errorf("exact windows = %d, want 2", r.WindowsExact)
	}
	if r.AmbiguityPoints != 0 {
		t.Errorf("ambiguity = %d, want 0 outside the crash interval", r.AmbiguityPoints)
	}
}

func TestCrashBoundExactWindowOffByOneFails(t *testing.T) {
	l := ledgerFor(300, map[int64][2]int64{1000: {500, 500}})
	r := EvaluateCrashBounds(l, "spans", map[int64]int64{1000: 499}, 9_000, 9_100, 1000, 1300)
	if r.Pass {
		t.Fatal("a window one point short of its ACKed total must fail")
	}
	if r.AckedLossPoints != 1 {
		t.Errorf("acknowledged loss = %d, want 1", r.AckedLossPoints)
	}
	if r.WindowsBelowLower != 1 {
		t.Errorf("windows below lower bound = %d, want 1", r.WindowsBelowLower)
	}
}

func TestCrashBoundCrashWindowAcceptsTheAmbiguityRange(t *testing.T) {
	// The crash interval falls inside window 1300. 60 of its 500 attempted
	// contributions were never acknowledged, so anything in [440, 500] is a
	// legitimate post-restart total.
	l := ledgerFor(300, map[int64][2]int64{
		1000: {500, 500},
		1300: {500, 440},
		1600: {500, 500},
	})
	for _, observed := range []int64{440, 470, 500} {
		obs := map[int64]int64{1000: 500, 1300: observed, 1600: 500}
		r := EvaluateCrashBounds(l, "spans", obs, 1400, 1450, 1000, 1900)
		if !r.Pass {
			t.Errorf("observed %d inside [440,500] was rejected: %+v", observed, r.Windows)
		}
		if r.WindowsCrashAffected != 1 {
			t.Errorf("crash-affected windows = %d, want 1", r.WindowsCrashAffected)
		}
		if r.AmbiguityPoints != 60 {
			t.Errorf("ambiguity = %d, want 60", r.AmbiguityPoints)
		}
	}
}

func TestCrashBoundBelowAckedIsLossAboveAttemptedIsImpossible(t *testing.T) {
	l := ledgerFor(300, map[int64][2]int64{1300: {500, 440}})
	low := EvaluateCrashBounds(l, "spans", map[int64]int64{1300: 439}, 1400, 1450, 1300, 1600)
	if low.Pass || low.AckedLossPoints != 1 {
		t.Errorf("439 must be acknowledged loss: pass=%t loss=%d", low.Pass, low.AckedLossPoints)
	}
	high := EvaluateCrashBounds(l, "spans", map[int64]int64{1300: 501}, 1400, 1450, 1300, 1600)
	if high.Pass || high.WindowsAboveUpper != 1 {
		t.Errorf("501 exceeds what was ever attempted: pass=%t above=%d", high.Pass, high.WindowsAboveUpper)
	}
}

func TestCrashBoundMissingWindowFails(t *testing.T) {
	l := ledgerFor(300, map[int64][2]int64{1000: {500, 500}})
	r := EvaluateCrashBounds(l, "spans", map[int64]int64{}, 9000, 9100, 1000, 1300)
	if r.Pass {
		t.Fatal("a window the query surface never answered must fail, not be skipped")
	}
	if r.WindowsMissing != 1 {
		t.Errorf("missing windows = %d, want 1", r.WindowsMissing)
	}
}

func TestCrashBoundNoLedgerFails(t *testing.T) {
	r := EvaluateCrashBounds(nil, "spans", map[int64]int64{}, 0, 0, 0, 1)
	if r.Evaluated || r.Pass || r.Error == "" {
		t.Fatalf("a missing ledger must fail loudly: %+v", r)
	}
}

func TestCrashBoundIgnoresWindowsOutsideTheComparisonRange(t *testing.T) {
	l := ledgerFor(300, map[int64][2]int64{
		700:  {999, 999},
		1000: {500, 500},
		1900: {999, 999},
	})
	r := EvaluateCrashBounds(l, "spans", map[int64]int64{1000: 500}, 9000, 9100, 1000, 1300)
	if !r.Pass {
		t.Fatalf("boundary windows must be excluded, not compared: %+v", r.Windows)
	}
	if r.WindowsCompared != 1 {
		t.Errorf("compared %d windows, want 1", r.WindowsCompared)
	}
}

func TestWindowOverlaps(t *testing.T) {
	cases := []struct {
		name                   string
		start, width, from, to int64
		want                   bool
	}{
		{"crash inside window", 1200, 300, 1350, 1360, true},
		{"crash before window", 1200, 300, 900, 1000, false},
		{"crash after window", 1200, 300, 1600, 1700, false},
		{"crash spans window", 1200, 300, 1000, 1600, true},
		{"crash at window start", 1200, 300, 1200, 1200, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := windowOverlaps(c.start, c.width, c.from, c.to); got != c.want {
				t.Errorf("windowOverlaps(%d,%d,%d,%d) = %t, want %t",
					c.start, c.width, c.from, c.to, got, c.want)
			}
		})
	}
}
