package queue

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

// DeadLetterQueue provides disk-based resilience for failed database writes.
// When a batch insert fails, the data is serialized to JSON and written to disk.
// A background replay worker periodically attempts to re-insert failed batches
// with exponential backoff, bounded by configurable file count and disk limits.
type DeadLetterQueue struct {
	dir      string
	interval time.Duration
	replayFn func(data []byte) error
	stopCh   chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex

	// Bounds
	maxFiles   int   // 0 = unlimited
	maxDiskMB  int64 // 0 = unlimited
	maxRetries int   // 0 = unlimited

	// maxReplayPerTick caps the number of files replayed per tick. Without
	// this, an outage that filled the DLQ with 10k files would replay all
	// of them in the first post-restart tick, hammering the (just-restarted)
	// DB. 0 = unlimited (legacy default), set via SetMaxReplayPerTick or
	// the DLQ_MAX_REPLAY_PER_TICK env var.
	maxReplayPerTick int

	// Per-file retry tracking (in-memory; resets on restart)
	retries map[string]int

	// Metric callbacks (optional, set via SetMetrics)
	onEnqueue   func()
	onSuccess   func()
	onFailure   func()
	onDiskBytes func(int64)

	// Eviction observability (Task 8)
	evicted      atomic.Int64
	evictedBytes atomic.Int64
	metricsTel   *telemetry.Metrics // nil-safe; enables otelcontext_dlq_evicted_* counters
}

// NewDLQ creates a new Dead Letter Queue.
// maxFiles/maxDiskMB/maxRetries = 0 means unlimited.
func NewDLQ(dir string, interval time.Duration, replayFn func(data []byte) error) (*DeadLetterQueue, error) {
	return NewDLQWithLimits(dir, interval, replayFn, 0, 0, 0)
}

// NewDLQWithLimits creates a DLQ with explicit bounds.
func NewDLQWithLimits(dir string, interval time.Duration, replayFn func(data []byte) error,
	maxFiles int, maxDiskMB int64, maxRetries int) (*DeadLetterQueue, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create DLQ directory %s: %w", dir, err)
	}

	dlq := &DeadLetterQueue{
		dir:        dir,
		interval:   interval,
		replayFn:   replayFn,
		stopCh:     make(chan struct{}),
		maxFiles:   maxFiles,
		maxDiskMB:  maxDiskMB,
		maxRetries: maxRetries,
		retries:    make(map[string]int),
	}

	dlq.wg.Add(1)
	go dlq.replayWorker()

	slog.Info("🔁 DLQ replay worker started", "dir", dir, "interval", interval,
		"max_files", maxFiles, "max_disk_mb", maxDiskMB, "max_retries", maxRetries)
	return dlq, nil
}

// SetMetrics wires Prometheus metric callbacks into the DLQ.
func (d *DeadLetterQueue) SetMetrics(onEnqueue, onSuccess, onFailure func(), onDiskBytes func(int64)) {
	d.mu.Lock()
	d.onEnqueue = onEnqueue
	d.onSuccess = onSuccess
	d.onFailure = onFailure
	d.onDiskBytes = onDiskBytes
	d.mu.Unlock()
}

// SetTelemetryMetrics wires the Prometheus registry so eviction counts surface
// in telemetry. Safe to call with a nil *telemetry.Metrics (disables the hook).
func (d *DeadLetterQueue) SetTelemetryMetrics(m *telemetry.Metrics) {
	d.metricsTel = m
}

// SetMaxReplayPerTick caps how many files the replay worker will attempt in
// one tick. n <= 0 disables the cap (unlimited). Safe to call after
// construction; the next tick observes the new value.
func (d *DeadLetterQueue) SetMaxReplayPerTick(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n < 0 {
		n = 0
	}
	d.maxReplayPerTick = n
}

// EvictedCount reports the cumulative number of DLQ files dropped due to
// MaxFiles/MaxDiskMB caps. Exposed for tests; see otelcontext_dlq_evicted_total.
func (d *DeadLetterQueue) EvictedCount() int64 { return d.evicted.Load() }

// EvictedBytesCount reports the byte volume dropped alongside EvictedCount.
func (d *DeadLetterQueue) EvictedBytesCount() int64 { return d.evictedBytes.Load() }

// DiskBytes returns the current total bytes of files in the DLQ directory.
func (d *DeadLetterQueue) DiskBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	entries, _ := os.ReadDir(d.dir)
	var total int64
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	return total
}

// Enqueue serializes the given batch to JSON and writes it to disk.
// Enforces file count and disk size limits (FIFO eviction when exceeded).
//
// Uses os.CreateTemp under the hood so concurrent enqueues never collide on
// a filename, even when the OS clock's resolution is coarser than goroutine
// scheduling (Windows, virtualised hosts) or thousands of failures hit the
// same nanosecond. A nanosecond-prefixed pattern is still passed to CreateTemp
// so the files sort chronologically for FIFO eviction.
func (d *DeadLetterQueue) Enqueue(batch any) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	data, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("DLQ: failed to marshal batch: %w", err)
	}

	// Enforce limits before writing a new file.
	d.enforceLimits(int64(len(data)))

	// batch_<nanos>_*.json — CreateTemp replaces `*` with a unique suffix so
	// two goroutines in the same nanosecond still get distinct files.
	pattern := fmt.Sprintf("batch_%d_*.json", time.Now().UnixNano())
	f, err := os.CreateTemp(d.dir, pattern)
	if err != nil {
		return fmt.Errorf("DLQ: failed to create file: %w", err)
	}
	path := f.Name()
	filename := filepath.Base(path)

	// Tighten perms: CreateTemp defaults to 0o600 on Unix already, but set
	// explicitly for clarity and for platforms with different defaults.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("DLQ: failed to chmod %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("DLQ: failed to write %s: %w", path, err)
	}
	// fsync before close so a host crash between Write and Close cannot leave
	// a torn file on disk that permanently consumes a retry slot. Without
	// this, the partial JSON would unmarshal-fail every replay until
	// DLQ_MAX_RETRIES evicts it — wasting the slot and emitting a steady
	// stream of replay-error logs.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("DLQ: failed to fsync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("DLQ: failed to close %s: %w", path, err)
	}

	slog.Warn("📦 Batch written to DLQ", "file", filename, "bytes", len(data))
	if d.onEnqueue != nil {
		d.onEnqueue()
	}
	return nil
}

// enforceLimits removes oldest files to stay within maxFiles and maxDiskMB.
// Must be called with d.mu held.
func (d *DeadLetterQueue) enforceLimits(incomingBytes int64) {
	if d.maxFiles == 0 && d.maxDiskMB == 0 {
		return
	}

	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}

	// Collect JSON files sorted by name (timestamp-prefixed → chronological).
	type fileInfo struct {
		name string
		size int64
	}
	var files []fileInfo
	var totalBytes int64
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, fileInfo{e.Name(), info.Size()})
			totalBytes += info.Size()
		}
	}

	maxBytes := d.maxDiskMB * 1024 * 1024
	var evictedThisCall int
	var evictedBytesThisCall int64
	i := 0
	for i < len(files) {
		overFiles := d.maxFiles > 0 && len(files)-i >= d.maxFiles
		overDisk := maxBytes > 0 && totalBytes+incomingBytes > maxBytes

		if !overFiles && !overDisk {
			break
		}

		// Evict oldest file.
		path := filepath.Join(d.dir, files[i].name)
		totalBytes -= files[i].size
		_ = os.Remove(path)
		delete(d.retries, files[i].name)
		slog.Warn("🗑️  DLQ FIFO eviction", "file", files[i].name)
		d.evicted.Add(1)
		d.evictedBytes.Add(files[i].size)
		evictedThisCall++
		evictedBytesThisCall += files[i].size
		if d.metricsTel != nil {
			if d.metricsTel.DLQEvictedTotal != nil {
				d.metricsTel.DLQEvictedTotal.Inc()
			}
			if d.metricsTel.DLQEvictedBytesTotal != nil {
				d.metricsTel.DLQEvictedBytesTotal.Add(float64(files[i].size))
			}
		}
		i++
	}

	if evictedThisCall > 0 {
		slog.Warn("dlq: evicted oldest files to stay under cap",
			"files", evictedThisCall,
			"bytes", evictedBytesThisCall,
			"max_files", d.maxFiles,
			"max_disk_mb", d.maxDiskMB,
		)
	}
}

// Size returns the number of files currently in the DLQ directory.
func (d *DeadLetterQueue) Size() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	entries, err := os.ReadDir(d.dir)
	if err != nil {
		slog.Error("DLQ: failed to read directory", "error", err)
		return 0
	}

	count := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			count++
		}
	}
	return count
}

// Stop gracefully shuts down the replay worker.
func (d *DeadLetterQueue) Stop() {
	close(d.stopCh)
	d.wg.Wait()
	slog.Info("🛑 DLQ replay worker stopped")
}

// replayWorker periodically scans the DLQ directory and attempts to re-insert failed batches.
func (d *DeadLetterQueue) replayWorker() {
	defer d.wg.Done()

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.processFiles()
		}
	}
}

// processFiles reads all JSON files in the DLQ directory and attempts to replay them
// with exponential backoff based on per-file retry count.
func (d *DeadLetterQueue) processFiles() {
	d.mu.Lock()
	entries, err := os.ReadDir(d.dir)
	d.mu.Unlock()

	if err != nil {
		slog.Error("DLQ: failed to read directory for replay", "error", err)
		return
	}

	d.mu.Lock()
	replayCap := d.maxReplayPerTick
	d.mu.Unlock()

	replayed := 0
	attempts := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		// Cap actual replayFn calls per tick so a 10k-file backlog after an
		// outage doesn't hammer the just-restarted DB. Backoff-skipped files
		// don't count — they cost nothing.
		if replayCap > 0 && attempts >= replayCap {
			slog.Debug("DLQ: max replay-per-tick cap reached", "cap", replayCap)
			break
		}

		name := entry.Name()

		// Check max retries — permanently drop if exceeded.
		d.mu.Lock()
		retries := d.retries[name]
		if d.maxRetries > 0 && retries >= d.maxRetries {
			path := filepath.Join(d.dir, name)
			_ = os.Remove(path)
			delete(d.retries, name)
			d.mu.Unlock()
			slog.Error("DLQ: max retries exceeded, dropping file", "file", name, "retries", retries)
			continue
		}
		d.mu.Unlock()

		// Exponential backoff: wait 2^retries × base interval before retrying.
		if retries > 0 {
			backoff := time.Duration(math.Pow(2, float64(retries-1))) * d.interval
			const maxBackoff = 30 * time.Minute
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			// Skip this file until enough time has elapsed.
			if info, err := entry.Info(); err == nil {
				if time.Since(info.ModTime()) < backoff {
					continue
				}
			}
		}

		path := filepath.Join(d.dir, name)
		data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed from d.dir (operator-controlled) + files we previously wrote

		if err != nil {
			slog.Error("DLQ: failed to read file", "file", name, "error", err)
			continue
		}

		attempts++
		if err := d.replayFn(data); err != nil {
			d.mu.Lock()
			d.retries[name]++
			newRetries := d.retries[name]
			cb := d.onFailure
			d.mu.Unlock()
			slog.Warn("DLQ: replay failed, backing off", "file", name, "retries", newRetries, "error", err)
			if cb != nil {
				cb()
			}
			// Touch the file to reset the backoff timer.
			now := time.Now()
			_ = os.Chtimes(path, now, now)
			continue
		}

		// Success — remove the file and clear retry counter.
		d.mu.Lock()
		var successCb func()
		if err := os.Remove(path); err != nil {
			slog.Error("DLQ: failed to remove replayed file", "file", name, "error", err)
		} else {
			delete(d.retries, name)
			replayed++
			successCb = d.onSuccess
			slog.Info("✅ DLQ file replayed and removed", "file", name)
		}
		d.mu.Unlock()
		if successCb != nil {
			successCb()
		}
	}

	if replayed > 0 {
		slog.Info("🔁 DLQ replay cycle complete", "replayed", replayed)
	}
}
