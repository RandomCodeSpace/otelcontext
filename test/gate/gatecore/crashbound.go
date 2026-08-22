package gatecore

import "sort"

// The crash-interval bound (Q3).
//
// The aggregate write path is at-least-once across a crash: a request whose
// transaction committed but whose ACK never reached the client is a legitimate
// survivor. So the post-restart total for a crash-affected window is bounded,
// not fixed:
//
//	confirmed-ACKed contributions <= observed <= all attempted contributions
//
// Demanding equality there would contradict the documented contract and would
// fail runs that behaved correctly. Windows the crash never touched carry no
// ambiguity of their own: the ledger records attempted == acked for them, so
// the same rule collapses to an exact equality automatically. Prefill and
// non-crash phases are therefore exact by construction, not by a second code
// path that could disagree with this one.

// WindowBound is one window's comparison.
type WindowBound struct {
	WindowStart   int64  `json:"window_start"`
	CrashAffected bool   `json:"crash_affected"`
	Attempted     int64  `json:"attempted"`
	Acked         int64  `json:"acked"`
	Observed      int64  `json:"observed"`
	ObservedFound bool   `json:"observed_found"`
	Exact         bool   `json:"exact"`
	Pass          bool   `json:"pass"`
	Reason        string `json:"reason,omitempty"`
}

// CrashBoundReport is the whole per-window comparison.
type CrashBoundReport struct {
	Evaluated  bool   `json:"evaluated"`
	Error      string `json:"error,omitempty"`
	Signal     string `json:"signal"`
	WindowSecs int64  `json:"window_secs"`

	CrashStartUnix int64 `json:"crash_start_unix"`
	CrashEndUnix   int64 `json:"crash_end_unix"`

	CompareFromUnix int64 `json:"compare_from_unix"`
	CompareToUnix   int64 `json:"compare_to_unix"`

	Windows              []WindowBound `json:"windows"`
	WindowsCompared      int           `json:"windows_compared"`
	WindowsExact         int           `json:"windows_exact"`
	WindowsCrashAffected int           `json:"windows_crash_affected"`
	WindowsFailed        int           `json:"windows_failed"`
	// WindowsBelowLower and WindowsAboveUpper split the failures by which
	// side of the permitted interval they fell out of. Below the lower bound
	// is acknowledged loss; above the upper bound is contributions that were
	// never sent.
	WindowsBelowLower int `json:"windows_below_lower"`
	WindowsAboveUpper int `json:"windows_above_upper"`
	// WindowsMissing counts windows the query surface returned no point for.
	WindowsMissing int `json:"windows_missing"`

	TotalAttempted int64 `json:"total_attempted"`
	TotalAcked     int64 `json:"total_acked"`
	TotalObserved  int64 `json:"total_observed"`
	// AmbiguityPoints is attempted - acked across the crash-affected windows:
	// the width of the interval the contract permits.
	AmbiguityPoints int64 `json:"ambiguity_points"`
	// AckedLossPoints is how far below the lower bound the observation fell.
	// Anything above zero is acknowledged loss and fails the gate.
	AckedLossPoints int64 `json:"acknowledged_loss_points"`

	Pass bool `json:"pass"`
}

// EvaluateCrashBounds compares observed per-window totals against the ledger.
//
// observed maps an aligned window start to the total the query surface
// reported for that window. Only windows in [compareFrom, compareTo) are
// compared — the orchestrator excludes the boundary windows a neighbouring
// phase could have contributed to.
func EvaluateCrashBounds(l *AckLedger, signal string, observed map[int64]int64,
	crashStart, crashEnd, compareFrom, compareTo int64) CrashBoundReport {

	r := CrashBoundReport{
		Signal:          signal,
		CrashStartUnix:  crashStart,
		CrashEndUnix:    crashEnd,
		CompareFromUnix: compareFrom,
		CompareToUnix:   compareTo,
	}
	if l == nil {
		r.Error = "no ACK ledger: the recovery claim cannot be checked"
		return r
	}
	r.WindowSecs = l.WindowSecs

	windows := l.WindowsIn(compareFrom, compareTo)
	if len(windows) == 0 {
		r.Error = "ACK ledger holds no windows inside the comparison range"
		return r
	}

	r.Evaluated = true
	r.Pass = true
	for _, w := range windows {
		c := w.SignalCounts(signal)
		if c.AttemptedPoints == 0 && c.AckedPoints == 0 {
			continue
		}
		wb := WindowBound{
			WindowStart:   w.WindowStart,
			CrashAffected: windowOverlaps(w.WindowStart, l.WindowSecs, crashStart, crashEnd),
			Attempted:     c.AttemptedPoints,
			Acked:         c.AckedPoints,
			Exact:         c.Exact(),
		}
		v, ok := observed[w.WindowStart]
		wb.Observed, wb.ObservedFound = v, ok

		switch {
		case !ok:
			wb.Pass = false
			wb.Reason = "the query surface returned no point for this window"
			r.WindowsMissing++
		case wb.Observed < wb.Acked:
			wb.Pass = false
			wb.Reason = "below the acknowledged lower bound: acknowledged aggregate loss"
			r.AckedLossPoints += wb.Acked - wb.Observed
			r.WindowsBelowLower++
		case wb.Observed > wb.Attempted:
			wb.Pass = false
			wb.Reason = "above the attempted upper bound: contributions that were never sent"
			r.WindowsAboveUpper++
		default:
			wb.Pass = true
		}

		r.Windows = append(r.Windows, wb)
		r.WindowsCompared++
		if wb.Exact {
			r.WindowsExact++
		}
		if wb.CrashAffected {
			r.WindowsCrashAffected++
			r.AmbiguityPoints += wb.Attempted - wb.Acked
		}
		if !wb.Pass {
			r.WindowsFailed++
			r.Pass = false
		}
		r.TotalAttempted += wb.Attempted
		r.TotalAcked += wb.Acked
		r.TotalObserved += wb.Observed
	}

	if r.WindowsCompared == 0 {
		r.Evaluated = false
		r.Pass = false
		r.Error = "no window inside the comparison range carried any " + signal + " contribution"
		return r
	}
	sort.Slice(r.Windows, func(i, j int) bool { return r.Windows[i].WindowStart < r.Windows[j].WindowStart })
	return r
}

// windowOverlaps reports whether [start, start+width) intersects [from, to].
func windowOverlaps(start, width, from, to int64) bool {
	if width <= 0 {
		width = WindowSecs
	}
	return start < to+1 && start+width > from
}
