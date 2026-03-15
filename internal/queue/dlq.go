package queue

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DeadLetterQueue provides disk-based resilience for failed database writes.
// When a batch insert fails, the data is serialized to JSON and written to disk.
// A background replay worker periodically attempts to re-insert failed batches
// with exponential backoff, bounded by configurable file count and disk limits.
type DeadLetterQueue struct {
	dir        string
	interval   time.Duration
	replayFn   func(data []byte) error
	stopCh     chan struct{}
	wg         sync.WaitGroup
	mu         sync.Mutex

	// Bounds
	maxFiles   int   // 0 = unlimited
	maxDiskMB  int64 // 0 = unlimited
	maxRetries int   // 0 = unlimited

	// Per-file retry tracking (in-memory; resets on restart)
	retries map[string]int
}

// NewDLQ creates a new Dead Letter Queue.
// maxFiles/maxDiskMB/maxRetries = 0 means unlimited.
func NewDLQ(dir string, interval time.Duration, replayFn func(data []byte) error) (*DeadLetterQueue, error) {
	return NewDLQWithLimits(dir, interval, replayFn, 0, 0, 0)
}

// NewDLQWithLimits creates a DLQ with explicit bounds.
func NewDLQWithLimits(dir string, interval time.Duration, replayFn func(data []byte) error,
	maxFiles int, maxDiskMB int64, maxRetries int) (*DeadLetterQueue, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

// Enqueue serializes the given batch to JSON and writes it to disk.
// Enforces file count and disk size limits (FIFO eviction when exceeded).
func (d *DeadLetterQueue) Enqueue(batch interface{}) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	data, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("DLQ: failed to marshal batch: %w", err)
	}

	// Enforce limits before writing a new file.
	d.enforceLimits(int64(len(data)))

	filename := fmt.Sprintf("batch_%d.json", time.Now().UnixNano())
	path := filepath.Join(d.dir, filename)

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("DLQ: failed to write file %s: %w", path, err)
	}

	slog.Warn("📦 Batch written to DLQ", "file", filename, "bytes", len(data))
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
		os.Remove(path)
		delete(d.retries, files[i].name)
		slog.Warn("🗑️  DLQ FIFO eviction", "file", files[i].name)
		i++
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

	replayed := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		name := entry.Name()

		// Check max retries — permanently drop if exceeded.
		d.mu.Lock()
		retries := d.retries[name]
		if d.maxRetries > 0 && retries >= d.maxRetries {
			path := filepath.Join(d.dir, name)
			os.Remove(path)
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
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("DLQ: failed to read file", "file", name, "error", err)
			continue
		}

		if err := d.replayFn(data); err != nil {
			d.mu.Lock()
			d.retries[name]++
			newRetries := d.retries[name]
			d.mu.Unlock()
			slog.Warn("DLQ: replay failed, backing off", "file", name, "retries", newRetries, "error", err)
			// Touch the file to reset the backoff timer.
			now := time.Now()
			os.Chtimes(path, now, now)
			continue
		}

		// Success — remove the file and clear retry counter.
		d.mu.Lock()
		if err := os.Remove(path); err != nil {
			slog.Error("DLQ: failed to remove replayed file", "file", name, "error", err)
		} else {
			delete(d.retries, name)
			replayed++
			slog.Info("✅ DLQ file replayed and removed", "file", name)
		}
		d.mu.Unlock()
	}

	if replayed > 0 {
		slog.Info("🔁 DLQ replay cycle complete", "replayed", replayed)
	}
}
