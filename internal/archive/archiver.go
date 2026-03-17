// Package archive implements hot/cold storage tiering for OtelContext.
// Data older than HOT_RETENTION_DAYS is moved from the relational DB (hot)
// to zstd-compressed JSONL files on local disk (cold).
// Cold files are organized as: {cold_path}/{year}/{month}/{day}/{type}.jsonl.zst
// Each day directory also contains a manifest.json with record counts and checksums.
package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/compress"
	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

// Manifest describes what was archived in a single day directory.
type Manifest struct {
	Date         string    `json:"date"`
	ArchivedAt   time.Time `json:"archived_at"`
	TraceCount   int       `json:"trace_count"`
	LogCount     int       `json:"log_count"`
	MetricCount  int       `json:"metric_count"`
	TraceBytes   int64     `json:"trace_bytes"`
	LogBytes     int64     `json:"log_bytes"`
	MetricBytes  int64     `json:"metric_bytes"`
	TraceHash    string    `json:"trace_sha256"`
	LogHash      string    `json:"log_sha256"`
	MetricHash   string    `json:"metric_sha256"`
}

// Archiver moves data older than the hot retention window to compressed cold files.
type Archiver struct {
	repo         *storage.Repository
	cfg          *config.Config
	metrics      *telemetry.Metrics
	recordsMoved atomic.Int64
	lastRun      atomic.Value // stores time.Time
}

// New creates a new Archiver.
func New(repo *storage.Repository, cfg *config.Config) *Archiver {
	a := &Archiver{repo: repo, cfg: cfg}
	a.lastRun.Store(time.Time{})
	return a
}

// SetMetrics wires Prometheus metrics into the archiver.
func (a *Archiver) SetMetrics(m *telemetry.Metrics) { a.metrics = m }

// RecordsMoved returns the total number of records moved to cold storage.
func (a *Archiver) RecordsMoved() int64 { return a.recordsMoved.Load() }

// LastRun returns the time of the last successful archival.
func (a *Archiver) LastRun() time.Time {
	if t, ok := a.lastRun.Load().(time.Time); ok {
		return t
	}
	return time.Time{}
}

// Start runs the archival loop. It fires once at the configured hour each day.
// Blocks until ctx is cancelled.
func (a *Archiver) Start(ctx context.Context) {
	slog.Info("🗄️  Archive worker started",
		"hot_retention_days", a.cfg.HotRetentionDays,
		"cold_path", a.cfg.ColdStoragePath,
		"schedule_hour", a.cfg.ArchiveScheduleHour,
	)

	for {
		next := nextScheduledRun(a.cfg.ArchiveScheduleHour)
		slog.Debug("Archive: next run scheduled", "at", next)

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
			if err := a.RunOnce(ctx); err != nil {
				slog.Error("Archive run failed", "error", err)
			}
		}
	}
}

// RunOnce performs a single archival pass — useful for testing or manual triggers.
func (a *Archiver) RunOnce(ctx context.Context) error {
	cutoff := time.Now().UTC().Truncate(24*time.Hour).
		AddDate(0, 0, -a.cfg.HotRetentionDays)

	slog.Info("🗄️  Starting archival pass", "cutoff", cutoff.Format("2006-01-02"))

	// Archive day by day from the oldest record up to cutoff.
	// We work one full UTC day at a time so cold files are day-granular.
	dates, err := a.repo.GetArchivedDateRange(cutoff)
	if err != nil {
		return fmt.Errorf("archive: failed to get date range: %w", err)
	}

	for _, date := range dates {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := a.archiveDay(ctx, date); err != nil {
			slog.Error("Archive: failed to archive day", "date", date.Format("2006-01-02"), "error", err)
			// continue to next day rather than aborting
		}
	}

	// Enforce cold storage size limit (FIFO eviction of oldest days)
	if err := a.enforceSizeLimit(); err != nil {
		slog.Warn("Archive: size limit enforcement failed", "error", err)
	}

	if err := Maintain(a.repo, a.cfg); err != nil {
		slog.Warn("Archive: DB maintenance failed", "error", err)
	}

	if a.metrics != nil {
		a.metrics.HotDBSizeBytes.Set(float64(a.repo.HotDBSizeBytes()))
		a.metrics.ColdStorageBytes.Set(float64(coldStorageBytes(a.cfg.ColdStoragePath)))
	}

	a.lastRun.Store(time.Now())
	slog.Info("✅ Archival pass complete")
	return nil
}

// coldStorageBytes walks the cold storage directory and sums file sizes.
func coldStorageBytes(coldPath string) int64 {
	var total int64
	filepath.WalkDir(coldPath, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// archiveDay moves all data for a single UTC day to cold storage.
func (a *Archiver) archiveDay(ctx context.Context, date time.Time) error {
	dayStart := date.Truncate(24 * time.Hour)
	dayEnd := dayStart.Add(24 * time.Hour)
	dir := coldDir(a.cfg.ColdStoragePath, dayStart)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create cold dir %s: %w", dir, err)
	}

	manifest := Manifest{
		Date:       dayStart.Format("2006-01-02"),
		ArchivedAt: time.Now().UTC(),
	}

	// Archive traces
	tBytes, tCount, tHash, err := a.archiveTraces(ctx, dir, dayStart, dayEnd)
	if err != nil {
		return err
	}
	manifest.TraceCount = tCount
	manifest.TraceBytes = tBytes
	manifest.TraceHash = tHash

	// Archive logs
	lBytes, lCount, lHash, err := a.archiveLogs(ctx, dir, dayStart, dayEnd)
	if err != nil {
		return err
	}
	manifest.LogCount = lCount
	manifest.LogBytes = lBytes
	manifest.LogHash = lHash

	// Archive metrics
	mBytes, mCount, mHash, err := a.archiveMetrics(ctx, dir, dayStart, dayEnd)
	if err != nil {
		return err
	}
	manifest.MetricCount = mCount
	manifest.MetricBytes = mBytes
	manifest.MetricHash = mHash

	// Write manifest
	if err := writeJSON(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	total := int64(tCount + lCount + mCount)
	a.recordsMoved.Add(total)

	if a.metrics != nil {
		a.metrics.ArchiveRecordsMoved.WithLabelValues("traces").Add(float64(tCount))
		a.metrics.ArchiveRecordsMoved.WithLabelValues("logs").Add(float64(lCount))
		a.metrics.ArchiveRecordsMoved.WithLabelValues("metrics").Add(float64(mCount))
	}

	slog.Info("📦 Day archived",
		"date", manifest.Date,
		"traces", tCount,
		"logs", lCount,
		"metrics", mCount,
	)
	return nil
}

func (a *Archiver) archiveTraces(ctx context.Context, dir string, start, end time.Time) (int64, int, string, error) {
	batchSize := a.cfg.ArchiveBatchSize
	offset := 0
	var allData []byte
	count := 0

	for {
		select {
		case <-ctx.Done():
			return 0, 0, "", ctx.Err()
		default:
		}

		batch, err := a.repo.GetTracesForArchive(start, end, batchSize, offset)
		if err != nil {
			return 0, 0, "", fmt.Errorf("archive traces query: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		for _, t := range batch {
			line, _ := json.Marshal(t)
			allData = append(allData, line...)
			allData = append(allData, '\n')
		}
		count += len(batch)
		offset += len(batch)

		// Delete after successfully encoding
		ids := make([]uint, len(batch))
		for i, t := range batch {
			ids[i] = t.ID
		}
		if err := a.repo.DeleteTracesByIDs(ids); err != nil {
			return 0, 0, "", fmt.Errorf("archive: delete traces failed: %w", err)
		}

		if len(batch) < batchSize {
			break
		}
	}

	if count == 0 {
		return 0, 0, "", nil
	}

	compressed := compress.Compress(allData)
	path := filepath.Join(dir, "traces.jsonl.zst")
	if err := os.WriteFile(path, compressed, 0o644); err != nil {
		return 0, 0, "", fmt.Errorf("failed to write traces archive: %w", err)
	}

	h := sha256.Sum256(compressed)
	return int64(len(compressed)), count, hex.EncodeToString(h[:]), nil
}

func (a *Archiver) archiveLogs(ctx context.Context, dir string, start, end time.Time) (int64, int, string, error) {
	batchSize := a.cfg.ArchiveBatchSize
	offset := 0
	var allData []byte
	count := 0

	for {
		select {
		case <-ctx.Done():
			return 0, 0, "", ctx.Err()
		default:
		}

		batch, err := a.repo.GetLogsForArchive(start, end, batchSize, offset)
		if err != nil {
			return 0, 0, "", fmt.Errorf("archive logs query: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		for _, l := range batch {
			line, _ := json.Marshal(l)
			allData = append(allData, line...)
			allData = append(allData, '\n')
		}
		count += len(batch)
		offset += len(batch)

		ids := make([]uint, len(batch))
		for i, l := range batch {
			ids[i] = l.ID
		}
		if err := a.repo.DeleteLogsByIDs(ids); err != nil {
			return 0, 0, "", fmt.Errorf("archive: delete logs failed: %w", err)
		}

		if len(batch) < batchSize {
			break
		}
	}

	if count == 0 {
		return 0, 0, "", nil
	}

	compressed := compress.Compress(allData)
	path := filepath.Join(dir, "logs.jsonl.zst")
	if err := os.WriteFile(path, compressed, 0o644); err != nil {
		return 0, 0, "", fmt.Errorf("failed to write logs archive: %w", err)
	}

	h := sha256.Sum256(compressed)
	return int64(len(compressed)), count, hex.EncodeToString(h[:]), nil
}

func (a *Archiver) archiveMetrics(ctx context.Context, dir string, start, end time.Time) (int64, int, string, error) {
	batchSize := a.cfg.ArchiveBatchSize
	offset := 0
	var allData []byte
	count := 0

	for {
		select {
		case <-ctx.Done():
			return 0, 0, "", ctx.Err()
		default:
		}

		batch, err := a.repo.GetMetricsForArchive(start, end, batchSize, offset)
		if err != nil {
			return 0, 0, "", fmt.Errorf("archive metrics query: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		for _, m := range batch {
			line, _ := json.Marshal(m)
			allData = append(allData, line...)
			allData = append(allData, '\n')
		}
		count += len(batch)
		offset += len(batch)

		ids := make([]uint, len(batch))
		for i, m := range batch {
			ids[i] = m.ID
		}
		if err := a.repo.DeleteMetricsByIDs(ids); err != nil {
			return 0, 0, "", fmt.Errorf("archive: delete metrics failed: %w", err)
		}

		if len(batch) < batchSize {
			break
		}
	}

	if count == 0 {
		return 0, 0, "", nil
	}

	compressed := compress.Compress(allData)
	path := filepath.Join(dir, "metrics.jsonl.zst")
	if err := os.WriteFile(path, compressed, 0o644); err != nil {
		return 0, 0, "", fmt.Errorf("failed to write metrics archive: %w", err)
	}

	h := sha256.Sum256(compressed)
	return int64(len(compressed)), count, hex.EncodeToString(h[:]), nil
}

// enforceSizeLimit removes the oldest day directories when cold storage exceeds ColdStorageMaxGB.
func (a *Archiver) enforceSizeLimit() error {
	maxBytes := int64(a.cfg.ColdStorageMaxGB) * 1024 * 1024 * 1024

	entries, err := os.ReadDir(a.cfg.ColdStoragePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	// Collect all day-level directories sorted oldest first
	type dayDir struct {
		path string
		size int64
	}
	var days []dayDir
	var totalSize int64

	for _, year := range entries {
		if !year.IsDir() {
			continue
		}
		yearPath := filepath.Join(a.cfg.ColdStoragePath, year.Name())
		months, _ := os.ReadDir(yearPath)
		for _, month := range months {
			if !month.IsDir() {
				continue
			}
			monthPath := filepath.Join(yearPath, month.Name())
			daysEntries, _ := os.ReadDir(monthPath)
			for _, day := range daysEntries {
				if !day.IsDir() {
					continue
				}
				dayPath := filepath.Join(monthPath, day.Name())
				size := dirSize(dayPath)
				days = append(days, dayDir{path: dayPath, size: size})
				totalSize += size
			}
		}
	}

	for totalSize > maxBytes && len(days) > 0 {
		oldest := days[0]
		days = days[1:]
		slog.Warn("🗑️  Cold storage limit reached, evicting oldest day", "path", oldest.path, "size_mb", oldest.size/1024/1024)
		if err := os.RemoveAll(oldest.path); err != nil {
			return err
		}
		totalSize -= oldest.size
	}
	return nil
}

// coldDir returns the path for a given day's cold storage directory.
func coldDir(base string, t time.Time) string {
	return filepath.Join(base,
		fmt.Sprintf("%04d", t.Year()),
		fmt.Sprintf("%02d", t.Month()),
		fmt.Sprintf("%02d", t.Day()),
	)
}

// nextScheduledRun returns the next time the archival should run (today or tomorrow at scheduleHour UTC).
func nextScheduledRun(scheduleHour int) time.Time {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), scheduleHour, 0, 0, 0, time.UTC)
	if now.After(next) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func dirSize(path string) int64 {
	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}

