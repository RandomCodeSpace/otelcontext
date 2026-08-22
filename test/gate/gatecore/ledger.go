package gatecore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// The ACK ledger.
//
// The recovery threshold is "no acknowledged aggregate loss", and the crash
// boundary permits committed-but-unacknowledged requests. Neither claim can be
// checked from the server side alone: only the client knows what it attempted
// and what came back with a nil error. So the load generator keeps a ledger of
// per-aggregate-window contributions, attempted and acknowledged, and flushes
// it to disk on an interval so a copy predating the kill always exists.
//
// Contributions are keyed by the window the DATA falls in, never by the window
// the Export happened in. internal/aggregate selects a span's window from its
// START time (SpanInput.Timestamp), which the load generator backdates by up
// to a batch interval plus the span duration. Keying the ledger on call time
// would therefore mis-attribute roughly a second of spans at every window
// boundary, and the exact-equality assertion for non-crash windows would fail
// on arithmetic that has nothing to do with durability.
//
// The ledger is written by test/loadsim (build tag loadtest) and read by the
// gate orchestrator. The types live here, in the untagged package, so there is
// exactly one definition of the on-disk shape.

// LedgerSchema is the on-disk schema marker.
const LedgerSchema = "otelcontext.ack-ledger/v1"

// LedgerCounts is one bucket of contribution accounting.
//
// Attempted is incremented before the Export call leaves; Acked only after it
// returns nil. Attempted >= Acked always, and the difference is exactly the
// ambiguity the at-least-once contract permits.
type LedgerCounts struct {
	AttemptedPoints   int64 `json:"attempted_points"`
	AckedPoints       int64 `json:"acked_points"`
	AttemptedRequests int64 `json:"attempted_requests"`
	AckedRequests     int64 `json:"acked_requests"`
}

// Add folds o into c.
func (c *LedgerCounts) Add(o LedgerCounts) {
	c.AttemptedPoints += o.AttemptedPoints
	c.AckedPoints += o.AckedPoints
	c.AttemptedRequests += o.AttemptedRequests
	c.AckedRequests += o.AckedRequests
}

// Exact reports whether the bucket carries no ambiguity: every attempted
// contribution was acknowledged, so the expected total is a single number
// rather than a range.
func (c LedgerCounts) Exact() bool { return c.AttemptedPoints == c.AckedPoints }

// LedgerWindow is one aggregate window's accounting.
type LedgerWindow struct {
	WindowStart int64                   `json:"window_start"`
	Counts      LedgerCounts            `json:"counts"`
	BySignal    map[string]LedgerCounts `json:"by_signal"`
}

// AckLedger is the on-disk document.
type AckLedger struct {
	Schema     string    `json:"schema"`
	StartedAt  time.Time `json:"started_at"`
	FlushedAt  time.Time `json:"flushed_at"`
	Final      bool      `json:"final"`
	WindowSecs int64     `json:"window_secs"`
	// FlushIntervalSec is how stale a non-final ledger can be. The gate needs
	// it to reason about a ledger recovered after a client-side crash.
	FlushIntervalSec float64                 `json:"flush_interval_sec"`
	Windows          []LedgerWindow          `json:"windows"`
	Totals           LedgerCounts            `json:"totals"`
	TotalsBySignal   map[string]LedgerCounts `json:"totals_by_signal"`
}

// WindowStartFor aligns t down to the containing window.
func WindowStartFor(t time.Time, windowSecs int64) int64 {
	if windowSecs <= 0 {
		windowSecs = WindowSecs
	}
	return (t.Unix() / windowSecs) * windowSecs
}

// Window returns the accounting for one window start, and whether it exists.
func (l *AckLedger) Window(start int64) (LedgerWindow, bool) {
	for i := range l.Windows {
		if l.Windows[i].WindowStart == start {
			return l.Windows[i], true
		}
	}
	return LedgerWindow{}, false
}

// SignalCounts returns one signal's accounting for one window.
func (w LedgerWindow) SignalCounts(signal string) LedgerCounts {
	if w.BySignal == nil {
		return LedgerCounts{}
	}
	return w.BySignal[signal]
}

// WindowsIn returns the windows whose start lies in [from, to), sorted.
func (l *AckLedger) WindowsIn(from, to int64) []LedgerWindow {
	out := make([]LedgerWindow, 0, len(l.Windows))
	for _, w := range l.Windows {
		if w.WindowStart >= from && w.WindowStart < to {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WindowStart < out[j].WindowStart })
	return out
}

// Summary reduces the ledger to what the report carries.
func (l *AckLedger) Summary(path string) LedgerSummary {
	s := LedgerSummary{
		Present:          true,
		Schema:           l.Schema,
		Final:            l.Final,
		FlushedAt:        l.FlushedAt,
		FlushIntervalSec: l.FlushIntervalSec,
		WindowSecs:       l.WindowSecs,
		Windows:          len(l.Windows),
		Totals:           l.Totals,
	}
	for i, w := range l.Windows {
		if i == 0 || w.WindowStart < s.FirstWindow {
			s.FirstWindow = w.WindowStart
		}
		if i == 0 || w.WindowStart > s.LastWindow {
			s.LastWindow = w.WindowStart
		}
	}
	return s
}

// LedgerRecorder accumulates the ledger in memory. It is safe for concurrent
// use by every emitter goroutine.
type LedgerRecorder struct {
	windowSecs    int64
	flushInterval time.Duration
	startedAt     time.Time

	mu      sync.Mutex
	windows map[int64]*LedgerWindow
}

// NewLedgerRecorder returns a recorder aligned to windowSecs (0 means the
// platform default of 300).
func NewLedgerRecorder(windowSecs int64, flushInterval time.Duration, now time.Time) *LedgerRecorder {
	if windowSecs <= 0 {
		windowSecs = WindowSecs
	}
	return &LedgerRecorder{
		windowSecs:    windowSecs,
		flushInterval: flushInterval,
		startedAt:     now,
		windows:       make(map[int64]*LedgerWindow),
	}
}

// Contribution is one Export's point counts, keyed by the aligned window the
// DATA falls in. A single batch legitimately straddles two windows.
type Contribution map[int64]int64

// Add records one point landing in the window containing t.
func (c Contribution) Add(t time.Time, windowSecs int64) {
	c[WindowStartFor(t, windowSecs)]++
}

// Points is the total across every window.
func (c Contribution) Points() int64 {
	var n int64
	for _, v := range c {
		n += v
	}
	return n
}

// bucket returns the window bucket for an aligned start, creating it if
// needed. Caller holds the lock.
func (r *LedgerRecorder) bucket(start int64) *LedgerWindow {
	w, ok := r.windows[start]
	if !ok {
		w = &LedgerWindow{WindowStart: start, BySignal: make(map[string]LedgerCounts)}
		r.windows[start] = w
	}
	return w
}

// Attempt records the points of an Export that is about to be sent.
//
// The request counter is incremented in every window the batch contributed to,
// so "requests" here means "requests that touched this window", not a
// partition of the call count.
func (r *LedgerRecorder) Attempt(c Contribution, signal string) {
	r.record(c, signal, false)
}

// Ack records the points of an Export that returned nil. The caller passes the
// same Contribution it passed to Attempt, so the two sides of the bound can
// never land in different windows.
func (r *LedgerRecorder) Ack(c Contribution, signal string) {
	r.record(c, signal, true)
}

func (r *LedgerRecorder) record(c Contribution, signal string, acked bool) {
	if len(c) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for start, points := range c {
		if points <= 0 {
			continue
		}
		w := r.bucket(start)
		sig := w.BySignal[signal]
		if acked {
			w.Counts.AckedPoints += points
			w.Counts.AckedRequests++
			sig.AckedPoints += points
			sig.AckedRequests++
		} else {
			w.Counts.AttemptedPoints += points
			w.Counts.AttemptedRequests++
			sig.AttemptedPoints += points
			sig.AttemptedRequests++
		}
		w.BySignal[signal] = sig
	}
}

// Snapshot renders the current accounting as an on-disk document.
func (r *LedgerRecorder) Snapshot(now time.Time, final bool) AckLedger {
	r.mu.Lock()
	defer r.mu.Unlock()

	l := AckLedger{
		Schema:           LedgerSchema,
		StartedAt:        r.startedAt,
		FlushedAt:        now,
		Final:            final,
		WindowSecs:       r.windowSecs,
		FlushIntervalSec: r.flushInterval.Seconds(),
		Windows:          make([]LedgerWindow, 0, len(r.windows)),
		TotalsBySignal:   make(map[string]LedgerCounts),
	}
	for _, w := range r.windows {
		cp := LedgerWindow{WindowStart: w.WindowStart, Counts: w.Counts,
			BySignal: make(map[string]LedgerCounts, len(w.BySignal))}
		for k, v := range w.BySignal {
			cp.BySignal[k] = v
			t := l.TotalsBySignal[k]
			t.Add(v)
			l.TotalsBySignal[k] = t
		}
		l.Totals.Add(w.Counts)
		l.Windows = append(l.Windows, cp)
	}
	sort.Slice(l.Windows, func(i, j int) bool { return l.Windows[i].WindowStart < l.Windows[j].WindowStart })
	return l
}

// WriteLedger writes the document atomically and fsyncs both the file and its
// directory, so a copy that predates the kill is genuinely on the platter.
func WriteLedger(path string, l AckLedger) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ackledger-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	d, err := os.Open(dir) // #nosec G304 -- operator-supplied ledger directory
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// LoadLedger reads a ledger document.
func LoadLedger(path string) (AckLedger, error) {
	var l AckLedger
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied ledger path
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, fmt.Errorf("parse ack ledger %s: %w", path, err)
	}
	if l.Schema != LedgerSchema {
		return l, fmt.Errorf("ack ledger %s: schema %q, want %q", path, l.Schema, LedgerSchema)
	}
	if l.WindowSecs <= 0 {
		return l, fmt.Errorf("ack ledger %s: window_secs %d", path, l.WindowSecs)
	}
	return l, nil
}
