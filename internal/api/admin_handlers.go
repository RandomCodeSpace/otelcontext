package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// handleGetStats handles GET /api/stats.
// The rendered JSON is cached for 10s per tenant with an ETag — same
// pattern as handleGetSystemGraph. The UI footer polls this endpoint and
// the COUNT(*) scans behind it are not free on a multi-GB SQLite file.
func (s *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	cacheKey := "db_stats:" + storage.TenantFromContext(r.Context())
	if cached, ok := s.cache.Get(cacheKey); ok {
		cached.(*cachedJSON).write(w, r, "HIT")
		return
	}

	stats, err := s.repo.GetStats(r.Context())
	if err != nil {
		slog.Error("Failed to get DB stats", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cj, err := newCachedJSON(stats)
	if err != nil {
		http.Error(w, "failed to encode DB stats", http.StatusInternalServerError)
		return
	}
	s.cache.Set(cacheKey, cj, hotPollCacheTTL)
	cj.write(w, r, "MISS")
}

// handlePurge handles DELETE /api/admin/purge
func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request) {
	// Default: purge data older than 7 days
	days := 7
	if d := r.URL.Query().Get("days"); d != "" {
		if v, err := strconv.Atoi(d); err == nil && v > 0 {
			days = v
		}
	}

	cutoff := time.Now().AddDate(0, 0, -days)

	logsDeleted, err := s.repo.PurgeLogs(cutoff)
	if err != nil {
		slog.Error("Failed to purge logs", "cutoff", cutoff, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tracesDeleted, err := s.repo.PurgeTraces(cutoff)
	if err != nil {
		slog.Error("Failed to purge traces", "cutoff", cutoff, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	slog.Info("Admin purge completed", "days", days, "logs_purged", logsDeleted, "traces_purged", tracesDeleted)

	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"logs_purged":   logsDeleted,
		"traces_purged": tracesDeleted,
		"cutoff":        cutoff,
	})
}

// handleVacuum handles POST /api/admin/vacuum
func (s *Server) handleVacuum(w http.ResponseWriter, _ *http.Request) {
	if err := s.repo.VacuumDB(); err != nil {
		slog.Error("Failed to vacuum database", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "vacuumed"})
}

// handleDropFTS handles POST /api/admin/drop_fts. Drops the SQLite FTS5
// virtual table + AFTER INSERT/DELETE/UPDATE triggers and runs VACUUM so the
// freed pages are returned to the OS. One-shot reclaim for existing deploys
// after LOG_FTS_ENABLED is set to false — typically reclaims 30-40% of DB disk.
//
// Refused (405) when LOG_FTS_ENABLED is currently truthy, because triggers
// fire on every log INSERT and dropping them mid-flight would silently break
// FTS5 sync until restart.
//
// VACUUM blocks writes for ~10-60 minutes on a multi-GB DB. Run this during
// a maintenance window.
func (s *Server) handleDropFTS(w http.ResponseWriter, r *http.Request) {
	if v, ok := os.LookupEnv("LOG_FTS_ENABLED"); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil && b {
			http.Error(w, "drop_fts refused: LOG_FTS_ENABLED is currently true; set it to false and restart before dropping", http.StatusMethodNotAllowed)
			return
		}
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "yes", "y", "on":
			http.Error(w, "drop_fts refused: LOG_FTS_ENABLED is currently true; set it to false and restart before dropping", http.StatusMethodNotAllowed)
			return
		}
	}

	started := time.Now()
	sizeBefore := s.repo.HotDBSizeBytes()

	if err := s.repo.DropLogsFTS(r.Context()); err != nil {
		slog.Error("drop_fts failed", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sizeAfter := s.repo.HotDBSizeBytes()
	elapsed := time.Since(started)
	reclaimed := sizeBefore - sizeAfter
	if reclaimed < 0 {
		reclaimed = 0
	}
	slog.Info("drop_fts completed", "elapsed_ms", elapsed.Milliseconds(), "reclaimed_bytes", reclaimed)

	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"reclaimed_bytes": reclaimed,
		"elapsed_ms":      elapsed.Milliseconds(),
	})
}
