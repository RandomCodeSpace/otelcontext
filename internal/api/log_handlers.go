package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/realtime"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// handleGetLogs handles GET /api/logs with advanced filtering
func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	offset := 0

	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil {
			limit = v
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil {
			offset = v
		}
	}

	filter := storage.LogFilter{
		ServiceName: r.URL.Query().Get("service_name"),
		Severity:    r.URL.Query().Get("severity"),
		Search:      r.URL.Query().Get("search"),
		Limit:       limit,
		Offset:      offset,
	}

	if startStr := r.URL.Query().Get("start"); startStr != "" {
		if t, err := time.Parse(time.RFC3339, startStr); err == nil {
			filter.StartTime = t
		}
	}
	if endStr := r.URL.Query().Get("end"); endStr != "" {
		if t, err := time.Parse(time.RFC3339, endStr); err == nil {
			filter.EndTime = t
		}
	}

	logs, total, err := s.repo.GetLogsV2(filter)
	if err != nil {
		slog.Error("Failed to get logs", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"data":  logs,
		"total": total,
	})
}

// handleGetLogContext handles GET /api/logs/context
func (s *Server) handleGetLogContext(w http.ResponseWriter, r *http.Request) {
	tsStr := r.URL.Query().Get("timestamp")
	if tsStr == "" {
		http.Error(w, "missing timestamp", http.StatusBadRequest)
		return
	}

	ts, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		slog.Warn("Invalid timestamp format for log context", "timestamp", tsStr)
		http.Error(w, "invalid timestamp format", http.StatusBadRequest)
		return
	}

	logs, err := s.repo.GetLogContext(ts)
	if err != nil {
		slog.Error("Failed to get log context", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(logs)
}

// handleGetLogInsight handles GET /api/logs/{id}/insight
func (s *Server) handleGetLogInsight(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	if idStr == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	l, err := s.repo.GetLog(uint(id))
	if err != nil {
		slog.Error("Log not found for insight", "id", id, "error", err)
		http.Error(w, "log not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"insight": string(l.AIInsight)})
}

// BroadcastLog sends a log entry to the buffered WebSocket hub.
func (s *Server) BroadcastLog(l storage.Log) {
	s.hub.Broadcast(realtime.LogEntry{
		ID:             l.ID,
		TraceID:        l.TraceID,
		SpanID:         l.SpanID,
		Severity:       l.Severity,
		Body:           string(l.Body),
		ServiceName:    l.ServiceName,
		AttributesJSON: string(l.AttributesJSON),
		AIInsight:      string(l.AIInsight),
		Timestamp:      l.Timestamp,
	})
}
