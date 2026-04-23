package storage

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RetentionScheduler periodically enforces hot-DB retention and runs DB maintenance.
// On startup and hourly thereafter it deletes rows older than retentionDays.
// Daily it runs driver-appropriate maintenance (VACUUM ANALYZE / OPTIMIZE / VACUUM).
type RetentionScheduler struct {
	repo            *Repository
	retentionDays   int
	purgeInterval   time.Duration
	vacuumInterval  time.Duration
	purgeBatchSize  int
	purgeBatchSleep time.Duration

	// started is an atomic so a fast-path Stop() before Start() is lock-free.
	// mu serializes the Start/Stop transition itself (protects cancel + done).
	started atomic.Bool
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}

	// running prevents overlapping purge/maintenance passes. If a run exceeds
	// purgeInterval, the next tick is skipped with a warn rather than piling on
	// contention (and potentially holding a long-running DELETE behind another).
	running atomic.Bool

	// skippedRuns increments every time a tick is dropped because running==true.
	// Test hook; exported via SkippedRuns().
	skippedRuns atomic.Int64
}

// NewRetentionScheduler constructs a scheduler but does not start it.
// batchSize <= 0 defaults to 10_000; batchSleep < 0 defaults to 5ms.
func NewRetentionScheduler(repo *Repository, retentionDays, batchSize int, batchSleep time.Duration) *RetentionScheduler {
	if batchSize <= 0 {
		batchSize = 10_000
	}
	if batchSleep < 0 {
		batchSleep = 5 * time.Millisecond
	}
	return &RetentionScheduler{
		repo:            repo,
		retentionDays:   retentionDays,
		purgeInterval:   1 * time.Hour,
		vacuumInterval:  24 * time.Hour,
		purgeBatchSize:  batchSize,
		purgeBatchSleep: batchSleep,
		done:            make(chan struct{}),
	}
}

// SkippedRuns returns the number of purge/maintenance ticks that were dropped
// because a previous run was still executing. Intended for tests and telemetry.
func (r *RetentionScheduler) SkippedRuns() int64 { return r.skippedRuns.Load() }

// Start launches the scheduler goroutine. It runs an initial purge immediately.
// Idempotent and race-free: atomic CAS elects the first caller, and mu
// publishes cancel+done before any concurrent Stop can observe started=true.
func (r *RetentionScheduler) Start(parent context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started.Load() {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	go r.loop(ctx)
	r.started.Store(true)
}

// Stop signals the scheduler to exit and waits for the loop to return.
// No-op if Start was never called. Safe to call concurrently / repeatedly.
func (r *RetentionScheduler) Stop() {
	if !r.started.Load() {
		return
	}
	r.mu.Lock()
	cancel := r.cancel
	done := r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (r *RetentionScheduler) loop(ctx context.Context) {
	defer close(r.done)

	purgeTick := time.NewTicker(r.purgeInterval)
	defer purgeTick.Stop()
	vacuumTick := time.NewTicker(r.vacuumInterval)
	defer vacuumTick.Stop()

	// Run an initial purge pass at startup so a long-paused instance catches up quickly.
	r.runPurge(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-purgeTick.C:
			r.runPurge(ctx)
		case <-vacuumTick.C:
			r.runMaintenance(ctx)
		}
	}
}

func (r *RetentionScheduler) runPurge(ctx context.Context) {
	// Overlap guard: if a previous purge/maintenance is still in flight, skip.
	if !r.running.CompareAndSwap(false, true) {
		r.skippedRuns.Add(1)
		slog.Warn("retention: previous run still in progress, skipping this tick", "phase", "purge")
		return
	}
	defer r.running.Store(false)

	driver := strings.ToLower(r.repo.driver)
	if driver == "" {
		driver = "sqlite"
	}
	cutoff := time.Now().UTC().Add(-time.Duration(r.retentionDays) * 24 * time.Hour)

	// SQLite: single-writer, parallel purges would just contend on the DB lock.
	if driver == "sqlite" {
		r.runPurgeSerial(ctx, cutoff, driver)
		return
	}

	metrics := r.repo.metrics
	start := time.Now()

	// Observe rows-behind before we start — good for dashboards, costs a COUNT.
	// Only on Postgres/MySQL where the extra scan is cheap relative to the purge.
	r.observeRowsBehind(ctx, driver, cutoff)

	type result struct {
		kind string
		n    int64
		err  error
	}
	results := make(chan result, 3)

	go func() {
		n, err := r.repo.PurgeLogsBatched(ctx, cutoff, r.purgeBatchSize, r.purgeBatchSleep)
		results <- result{"logs", n, err}
	}()
	go func() {
		n, err := r.repo.PurgeTracesBatched(ctx, cutoff, r.purgeBatchSize, r.purgeBatchSleep)
		results <- result{"traces", n, err}
	}()
	go func() {
		n, err := r.repo.PurgeMetricBucketsBatched(ctx, cutoff, r.purgeBatchSize, r.purgeBatchSleep)
		results <- result{"metric_buckets", n, err}
	}()

	purgeFailed := false
	totals := map[string]int64{}
	for i := 0; i < 3; i++ {
		res := <-results
		if res.err != nil {
			slog.Error("retention: purge failed", "kind", res.kind, "error", res.err)
			purgeFailed = true
		}
		totals[res.kind] += res.n
		if metrics != nil && res.n > 0 {
			metrics.RetentionRowsPurgedTotal.WithLabelValues(res.kind, driver).Add(float64(res.n))
		}
	}

	if metrics != nil {
		metrics.RetentionPurgeDurationSeconds.WithLabelValues(driver).Observe(time.Since(start).Seconds())
		if purgeFailed {
			metrics.RetentionConsecutiveFailures.WithLabelValues("purge").Inc()
		} else {
			metrics.RetentionConsecutiveFailures.WithLabelValues("purge").Set(0)
			metrics.RetentionLastSuccessTimestamp.WithLabelValues("purge").Set(float64(time.Now().Unix()))
		}
	}

	slog.Info("retention purge complete",
		"driver", driver,
		"duration", time.Since(start),
		"logs_deleted", totals["logs"],
		"traces_deleted", totals["traces"],
		"metrics_deleted", totals["metric_buckets"],
	)
}

// runPurgeSerial is the SQLite path: running the three purges concurrently buys
// nothing because the driver holds a single writer lock, so we serialize them
// to keep the "running" gauge accurate and avoid goroutine launch cost.
func (r *RetentionScheduler) runPurgeSerial(ctx context.Context, cutoff time.Time, driver string) {
	metrics := r.repo.metrics
	start := time.Now()
	purgeFailed := false

	logs, err := r.repo.PurgeLogsBatched(ctx, cutoff, r.purgeBatchSize, r.purgeBatchSleep)
	if err != nil {
		slog.Error("retention: purge logs failed", "error", err)
		purgeFailed = true
	}
	if metrics != nil && logs > 0 {
		metrics.RetentionRowsPurgedTotal.WithLabelValues("logs", driver).Add(float64(logs))
	}

	traces, err := r.repo.PurgeTracesBatched(ctx, cutoff, r.purgeBatchSize, r.purgeBatchSleep)
	if err != nil {
		slog.Error("retention: purge traces failed", "error", err)
		purgeFailed = true
	}
	if metrics != nil && traces > 0 {
		metrics.RetentionRowsPurgedTotal.WithLabelValues("traces", driver).Add(float64(traces))
	}

	metricsPurged, err := r.repo.PurgeMetricBucketsBatched(ctx, cutoff, r.purgeBatchSize, r.purgeBatchSleep)
	if err != nil {
		slog.Error("retention: purge metrics failed", "error", err)
		purgeFailed = true
	}
	if metrics != nil && metricsPurged > 0 {
		metrics.RetentionRowsPurgedTotal.WithLabelValues("metric_buckets", driver).Add(float64(metricsPurged))
	}

	if metrics != nil {
		metrics.RetentionPurgeDurationSeconds.WithLabelValues(driver).Observe(time.Since(start).Seconds())
		if purgeFailed {
			metrics.RetentionConsecutiveFailures.WithLabelValues("purge").Inc()
		} else {
			metrics.RetentionConsecutiveFailures.WithLabelValues("purge").Set(0)
			metrics.RetentionLastSuccessTimestamp.WithLabelValues("purge").Set(float64(time.Now().Unix()))
		}
	}

	slog.Info("retention purge complete",
		"driver", driver,
		"cutoff", cutoff.Format(time.RFC3339),
		"logs_deleted", logs,
		"traces_deleted", traces,
		"metrics_deleted", metricsPurged,
		"duration", time.Since(start),
	)
}

// observeRowsBehind populates RetentionRowsBehindGauge so operators can see
// when ingest is outrunning purge. Best-effort — a failed COUNT is logged and
// skipped rather than failing the purge.
func (r *RetentionScheduler) observeRowsBehind(ctx context.Context, driver string, cutoff time.Time) {
	metrics := r.repo.metrics
	if metrics == nil || metrics.RetentionRowsBehindGauge == nil {
		return
	}
	probes := []struct {
		table    string
		model    any
		tsColumn string
	}{
		{"logs", &Log{}, "timestamp"},
		{"traces", &Trace{}, "timestamp"},
		{"metric_buckets", &MetricBucket{}, "time_bucket"},
	}
	for _, p := range probes {
		var n int64
		if err := r.repo.db.WithContext(ctx).Model(p.model).Where(p.tsColumn+" < ?", cutoff).Count(&n).Error; err != nil {
			continue // count failure is non-fatal; skip this label
		}
		metrics.RetentionRowsBehindGauge.WithLabelValues(p.table, driver).Set(float64(n))
	}
}

func (r *RetentionScheduler) runMaintenance(ctx context.Context) {
	if !r.running.CompareAndSwap(false, true) {
		r.skippedRuns.Add(1)
		slog.Warn("retention: previous run still in progress, skipping this tick", "phase", "maintenance")
		return
	}
	defer r.running.Store(false)

	driver := strings.ToLower(r.repo.driver)
	if driver == "" {
		driver = "sqlite"
	}
	metrics := r.repo.metrics

	// Fix 6: track whether any step failed so we can set the right gauge.
	maintFailed := false
	defer func() {
		if metrics == nil {
			return
		}
		if maintFailed {
			metrics.RetentionConsecutiveFailures.WithLabelValues("maintenance").Inc()
			return
		}
		metrics.RetentionConsecutiveFailures.WithLabelValues("maintenance").Set(0)
		metrics.RetentionLastSuccessTimestamp.WithLabelValues("maintenance").Set(float64(time.Now().Unix()))
	}()

	// VACUUM cannot run inside a transaction on Postgres or SQLite.
	// GORM's db.Exec wraps statements in an implicit tx, so we drop to the raw *sql.DB.
	sqlDB, err := r.repo.db.DB()
	if err != nil {
		slog.Error("retention: get raw sql.DB failed", "error", err)
		maintFailed = true
		return
	}

	observe := func(table string, d time.Duration) {
		if metrics != nil {
			metrics.RetentionVacuumDurationSeconds.WithLabelValues(driver, table).Observe(d.Seconds())
		}
	}

	switch driver {
	case "postgres", "postgresql":
		for _, t := range []string{"logs", "spans", "traces", "metric_buckets"} {
			start := time.Now()
			if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf("VACUUM ANALYZE %s", t)); err != nil {
				slog.Error("retention: VACUUM ANALYZE failed", "table", t, "error", err)
				maintFailed = true
			}
			observe(t, time.Since(start))
		}
	case "mysql":
		// OPTIMIZE TABLE can run through the gorm handle (no tx restriction).
		db := r.repo.db.WithContext(ctx)
		for _, t := range []string{"logs", "spans", "traces", "metric_buckets"} {
			start := time.Now()
			if err := db.Exec(fmt.Sprintf("OPTIMIZE TABLE %s", t)).Error; err != nil {
				slog.Error("retention: OPTIMIZE TABLE failed", "table", t, "error", err)
				maintFailed = true
			}
			observe(t, time.Since(start))
		}
	case "sqlite":
		start := time.Now()
		if _, err := sqlDB.ExecContext(ctx, "PRAGMA optimize"); err != nil {
			slog.Error("retention: PRAGMA optimize failed", "error", err)
			maintFailed = true
		}
		if _, err := sqlDB.ExecContext(ctx, "VACUUM"); err != nil {
			slog.Error("retention: VACUUM failed", "error", err)
			maintFailed = true
		}
		// SQLite VACUUM is whole-DB; record a single observation under "all".
		observe("all", time.Since(start))
	}
	slog.Info("retention maintenance complete", "driver", driver)
}
