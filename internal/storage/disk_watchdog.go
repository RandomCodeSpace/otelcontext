package storage

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

// Disk watchdog with staged shedding and hysteresis (#201 Q5).
//
// The 8 GiB data budget (#201 Q1) is a promise about a volume, not about a
// table. Per-component gauges attribute what is on disk; the ENFORCEMENT
// number is statfs on the data directory, because that is the only figure
// that also counts WAL frames, SQLite temp files, free pages the file has not
// given back, and anything else sharing the volume. A budget enforced against
// summed file sizes is a budget that reports 60% while write() returns ENOSPC.
//
// What shedding does NOT do: it never converts a successful aggregate Export
// into a retryable failure. Aggregate accounting is complete telemetry; raw
// exemplars are diagnostics. When the disk fills, the diagnostics go first.
// The single exception lives on the aggregate commit path — see
// aggregate.IsDiskFull.

// Shedding thresholds as a fraction of the enforcement ceiling. Entry and exit
// differ on purpose: a volume hovering at the line would otherwise flap raw
// retention on and off every tick, and every flap is a discontinuity in the
// exemplar coverage an operator is trying to read during an incident.
const (
	diskEnterErrorsOnly = 0.90
	diskEnterRawOff     = 0.95
	diskExitRawOff      = 0.90 // raw-off -> errors-only
	diskExitErrorsOnly  = 0.85 // errors-only -> none
)

// DefaultDiskWatchdogInterval is how often the volume is sampled. statfs is a
// single syscall; 30s is far below the time it takes to fill the remaining 5%
// of an 8 GiB volume at any ingest rate this platform survives.
const DefaultDiskWatchdogInterval = 30 * time.Second

// DiskUsage is one statfs sample.
type DiskUsage struct {
	TotalBytes int64
	AvailBytes int64
}

// UsedBytes is the allocated portion of the volume.
func (d DiskUsage) UsedBytes() int64 {
	if d.TotalBytes <= 0 {
		return 0
	}
	used := d.TotalBytes - d.AvailBytes
	if used < 0 {
		return 0
	}
	return used
}

// DiskWatchdogConfig configures a watchdog. Stat defaults to the platform
// statfs; tests inject a fake.
type DiskWatchdogConfig struct {
	// Path is any path on the data volume. statfs reports the filesystem, not
	// the path, so the data directory and the DB file give the same answer.
	Path string
	// BudgetBytes is DATA_DISK_BUDGET_MB expressed in bytes.
	BudgetBytes int64
	// Interval between samples. <= 0 uses DefaultDiskWatchdogInterval.
	Interval time.Duration
	// Stat overrides the platform statfs. nil uses the real one.
	Stat func(path string) (DiskUsage, error)
	// Metrics receives the disk gauges. nil is a no-op.
	Metrics *telemetry.Metrics
	// OnRawOff fires once on every transition INTO SheddingRawOff. It is where
	// the immediate expired-exemplar purge and the WAL checkpoint are wired.
	// Runs on the watchdog goroutine; keep it bounded.
	OnRawOff func()
}

// diskComponent is one attribution gauge: a named byte sampler plus the
// highest value it has ever reported.
type diskComponent struct {
	name      string
	sample    func() int64
	highWater int64
}

// DiskWatchdog samples the data volume and publishes a shedding state.
type DiskWatchdog struct {
	path     string
	budget   int64
	interval time.Duration
	stat     func(string) (DiskUsage, error)
	metrics  *telemetry.Metrics
	onRawOff func()

	state atomic.Int32 // SheddingState
	ratio atomic.Uint64
	used  atomic.Int64

	mu         sync.Mutex
	components []*diskComponent
	observers  []func(SheddingState)

	started atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewDiskWatchdog constructs a watchdog. It does not sample until Sample() or
// Start() is called.
func NewDiskWatchdog(cfg DiskWatchdogConfig) *DiskWatchdog {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultDiskWatchdogInterval
	}
	if cfg.Stat == nil {
		cfg.Stat = statfsUsage
	}
	if cfg.Path == "" {
		cfg.Path = "."
	}
	return &DiskWatchdog{
		path:     cfg.Path,
		budget:   cfg.BudgetBytes,
		interval: cfg.Interval,
		stat:     cfg.Stat,
		metrics:  cfg.Metrics,
		onRawOff: cfg.OnRawOff,
		done:     make(chan struct{}),
	}
}

// AddComponent registers a per-component byte sampler. The value is published
// as a gauge and its running maximum as a high-water gauge, so the seven-day
// gate (#202) can check each tier against its share of the budget table
// instead of against a number someone remembers from a demo.
//
// Attribution only: components never drive the shedding decision.
func (w *DiskWatchdog) AddComponent(name string, sample func() int64) {
	if sample == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.components = append(w.components, &diskComponent{name: name, sample: sample})
}

// AddObserver registers a callback fired on every state CHANGE, with the new
// state. Called synchronously on the sampling goroutine.
func (w *DiskWatchdog) AddObserver(fn func(SheddingState)) {
	if fn == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.observers = append(w.observers, fn)
}

// State returns the current shedding state. Safe on a nil receiver.
func (w *DiskWatchdog) State() SheddingState {
	if w == nil {
		return SheddingNone
	}
	return SheddingState(w.state.Load())
}

// Healthy reports whether readiness should pass. False at SheddingRawOff:
// the process is still serving reads and still accounting aggregates, but it
// can no longer retain the diagnostics an operator would come here for, and
// an orchestrator should stop routing fresh ingest at it.
func (w *DiskWatchdog) Healthy() bool { return w.State() < SheddingRawOff }

// UsedRatio returns the last sampled usage as a fraction of the enforcement
// ceiling.
func (w *DiskWatchdog) UsedRatio() float64 {
	if w == nil {
		return 0
	}
	return math.Float64frombits(w.ratio.Load())
}

// ceiling is the enforcement ceiling: the lower of the configured budget and
// the usable volume capacity, where the latter is detectable. A 4 GiB volume
// does not become 8 GiB because the config says so.
func (w *DiskWatchdog) ceiling(u DiskUsage) int64 {
	ceiling := w.budget
	if u.TotalBytes > 0 && (ceiling <= 0 || u.TotalBytes < ceiling) {
		ceiling = u.TotalBytes
	}
	return ceiling
}

// nextState applies the threshold ladder with hysteresis to a usage ratio.
// Pure function of (current, ratio) so the transition table is testable
// without a filesystem.
func nextState(cur SheddingState, ratio float64) SheddingState {
	switch {
	case ratio >= diskEnterRawOff:
		return SheddingRawOff
	case ratio >= diskEnterErrorsOnly:
		// Already raw-off: stay there. Recovery from raw-off needs < 90%.
		if cur == SheddingRawOff {
			return SheddingRawOff
		}
		return SheddingErrorsOnly
	case ratio >= diskExitErrorsOnly:
		// 85%..90%: raw-off recovers to errors-only here; errors-only holds.
		if cur == SheddingNone {
			return SheddingNone
		}
		return SheddingErrorsOnly
	default:
		return SheddingNone
	}
}

// Sample takes one statfs reading, updates the gauges and applies the state
// ladder. Exported so tests drive transitions deterministically and so the
// startup path can establish a state before ingest opens.
//
// A failed stat holds the current state: shedding raw retention because a
// syscall failed would be an outage caused by the safety mechanism.
func (w *DiskWatchdog) Sample() SheddingState {
	u, err := w.stat(w.path)
	if err != nil {
		slog.Warn("disk watchdog: statfs failed, holding current shedding state",
			"path", w.path, "state", w.State().String(), "error", err)
		w.sampleComponents()
		return w.State()
	}

	ceiling := w.ceiling(u)
	used := u.UsedBytes()
	ratio := 0.0
	if ceiling > 0 {
		ratio = float64(used) / float64(ceiling)
	}
	w.used.Store(used)
	w.ratio.Store(math.Float64bits(ratio))
	w.publishVolume(ceiling, used, ratio)
	w.sampleComponents()

	cur := w.State()
	next := nextState(cur, ratio)
	if next == cur {
		return cur
	}
	w.state.Store(int32(next))
	slog.Warn("disk watchdog: shedding state changed",
		"from", cur.String(),
		"to", next.String(),
		"used_bytes", used,
		"ceiling_bytes", ceiling,
		"used_ratio", ratio,
	)
	if w.metrics != nil && w.metrics.DiskSheddingTransitionsTotal != nil {
		w.metrics.DiskSheddingTransitionsTotal.WithLabelValues(cur.String(), next.String()).Inc()
	}
	w.notify(next)
	if next == SheddingRawOff && w.onRawOff != nil {
		w.onRawOff()
	}
	return next
}

// notify fans the new state out to observers under a snapshot of the slice, so
// an observer that registers another observer cannot deadlock.
func (w *DiskWatchdog) notify(s SheddingState) {
	w.mu.Lock()
	obs := make([]func(SheddingState), len(w.observers))
	copy(obs, w.observers)
	w.mu.Unlock()
	for _, fn := range obs {
		fn(s)
	}
}

// publishVolume writes the volume-level gauges.
func (w *DiskWatchdog) publishVolume(ceiling, used int64, ratio float64) {
	m := w.metrics
	if m == nil {
		return
	}
	if m.DiskBudgetBytes != nil {
		m.DiskBudgetBytes.Set(float64(ceiling))
	}
	if m.DiskUsedBytes != nil {
		m.DiskUsedBytes.Set(float64(used))
	}
	if m.DiskUsedRatio != nil {
		m.DiskUsedRatio.Set(ratio)
	}
	if m.DiskSheddingState != nil {
		m.DiskSheddingState.Set(float64(w.state.Load()))
	}
}

// sampleComponents refreshes the per-component attribution and high-water
// gauges.
func (w *DiskWatchdog) sampleComponents() {
	w.mu.Lock()
	comps := make([]*diskComponent, len(w.components))
	copy(comps, w.components)
	w.mu.Unlock()

	m := w.metrics
	for _, c := range comps {
		n := c.sample()
		if n < 0 {
			n = 0
		}
		if n > c.highWater {
			c.highWater = n
		}
		if m == nil {
			continue
		}
		if m.DiskComponentBytes != nil {
			m.DiskComponentBytes.WithLabelValues(c.name).Set(float64(n))
		}
		if m.DiskComponentHighWaterBytes != nil {
			m.DiskComponentHighWaterBytes.WithLabelValues(c.name).Set(float64(c.highWater))
		}
	}
}

// HighWater returns the recorded high-water mark for a component. Test and
// diagnostic surface.
func (w *DiskWatchdog) HighWater(name string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.components {
		if c.name == name {
			return c.highWater
		}
	}
	return 0
}

// Start samples immediately and then on every tick until ctx is cancelled or
// Stop is called. Idempotent.
func (w *DiskWatchdog) Start(parent context.Context) {
	if !w.started.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	go func() {
		defer close(w.done)
		t := time.NewTicker(w.interval)
		defer t.Stop()
		w.Sample()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.Sample()
			}
		}
	}()
}

// Stop halts the sampling loop and waits for it to exit. No-op before Start.
func (w *DiskWatchdog) Stop() {
	if !w.started.Load() {
		return
	}
	if w.cancel != nil {
		w.cancel()
	}
	<-w.done
}
