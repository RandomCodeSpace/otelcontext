package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
)

// RuntimeFiles contains operator-supplied paths, never returned to clients.
type RuntimeFiles struct {
	MainDB, AggregateDB string
	Mode                string
	RetentionDays       int
}

type StorageFootprint struct {
	MainBytes      *int64 `json:"main_bytes"`
	AggregateBytes *int64 `json:"aggregate_bytes"`
	SidecarBytes   *int64 `json:"sidecar_bytes"`
	TotalBytes     *int64 `json:"total_bytes"`
	FreeBytes      *int64 `json:"free_bytes"`
	Complete       bool   `json:"complete"`
	Note           string `json:"note"`
}

type RuntimeStats struct {
	Mode          string                `json:"mode"`
	RetentionDays int                   `json:"retention_days"`
	Storage       StorageFootprint      `json:"storage"`
	Exports       telemetry.ExportStats `json:"exports"`
	HeapBytes     uint64                `json:"heap_bytes"`
	QueueRatio    *float64              `json:"queue_ratio"`
	ObservedAt    time.Time             `json:"observed_at"`
}

func (s *Server) SetRuntimeFiles(files RuntimeFiles) { s.runtimeFiles = files }

func databaseFootprint(path string) (main, sidecars int64, resolved string, ok bool) {
	if path == "" || path == ":memory:" {
		return 0, 0, "", false
	}
	// WAL/SHM live beside the real database, not a symlink supplied by config.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return 0, 0, "", false
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.Mode().IsRegular() {
		return 0, 0, "", false
	}
	main = st.Size()
	for _, suffix := range []string{"-wal", "-shm"} {
		st, err = os.Stat(resolved + suffix)
		if os.IsNotExist(err) {
			continue // transient SQLite sidecars are optional
		}
		if err != nil || !st.Mode().IsRegular() {
			return main, 0, resolved, false
		}
		sidecars += st.Size()
	}
	return main, sidecars, resolved, true
}

func runtimeFootprint(files RuntimeFiles) StorageFootprint {
	out := StorageFootprint{Note: "Point-in-time logical file sizes, including indexes and reusable space; not live payload bytes. SQLite sidecars may change during sampling. Non-local database storage is unavailable."}
	main, wal, resolved, ok := databaseFootprint(files.MainDB)
	if ok {
		out.MainBytes = &main
		if usage, err := storage.FilesystemUsage(resolved); err == nil {
			out.FreeBytes = &usage.AvailBytes
		}
	}
	aggregate, aggWAL := int64(0), int64(0)
	aggOK := files.AggregateDB == ""
	if !aggOK {
		aggregate, aggWAL, _, aggOK = databaseFootprint(files.AggregateDB)
	}
	if aggOK {
		out.AggregateBytes = &aggregate
	}
	if ok && aggOK {
		combinedWAL, total := wal+aggWAL, main+aggregate+wal+aggWAL
		out.SidecarBytes, out.TotalBytes, out.Complete = &combinedWAL, &total, true
	}
	return out
}

// This endpoint uses file metadata and in-process counters only. It never
// counts raw trace/log tables and does not enqueue a database query.
func (s *Server) handleRuntimeStats(w http.ResponseWriter, r *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	out := RuntimeStats{Mode: s.runtimeFiles.Mode, RetentionDays: s.runtimeFiles.RetentionDays,
		Storage: runtimeFootprint(s.runtimeFiles), Exports: s.metrics.ExportStats(), HeapBytes: mem.HeapAlloc, ObservedAt: time.Now()}
	if s.pipelineSaturation != nil {
		ratio := s.pipelineSaturation()
		out.QueueRatio = &ratio
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
