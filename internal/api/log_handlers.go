package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/api/views"
	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
	"github.com/RandomCodeSpace/otelcontext/internal/realtime"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// handleGetLogs handles GET /api/logs with advanced filtering
func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePaging(r, pagingDefaultLimit)

	filter := storage.LogFilter{
		ServiceName: r.URL.Query().Get("service_name"),
		Severity:    r.URL.Query().Get("severity"),
		Search:      r.URL.Query().Get("search"),
		TraceID:     r.URL.Query().Get("trace_id"),
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

	// When the caller is doing a body keyword search, enforce the same 24h
	// cap as the MCP search_logs tool so a direct HTTP caller cannot bypass
	// via the alternate transport. Pure filtered listings (no search term)
	// keep the full retention range.
	if filter.Search != "" {
		cs, ce, err := storage.ClampSearchWindowTo24h(filter.StartTime, filter.EndTime, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		filter.StartTime, filter.EndTime = cs, ce
	}

	logs, total, err := s.repo.GetLogsV2(r.Context(), filter)
	if err != nil {
		slog.Error("Failed to get logs", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":  views.LogsFromModels(logs),
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
		slog.Warn("Invalid timestamp format for log context", "timestamp", tsStr) // #nosec G706 -- slog uses structured k/v fields, not format interpolation
		http.Error(w, "invalid timestamp format", http.StatusBadRequest)
		return
	}

	logs, err := s.repo.GetLogContext(r.Context(), ts)
	if err != nil {
		slog.Error("Failed to get log context", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(views.LogsFromModels(logs))
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

	l, err := s.repo.GetLog(r.Context(), uint(id)) // #nosec G115 -- id is parsed from URL; upstream validates positivity
	if err != nil {
		slog.Error("Log not found for insight", "id", id, "error", err)
		http.Error(w, "log not found", http.StatusNotFound)
		return
	}

	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]string{"insight": string(l.AIInsight)})
}

// BroadcastLog sends a log entry to the buffered WebSocket hub.
func (s *Server) BroadcastLog(l storage.Log) {
	s.hub.Broadcast(realtime.LogEntry{
		ID:             l.ID,
		TraceID:        l.TraceID,
		SpanID:         l.SpanID,
		Severity:       l.Severity,
		Body:           l.Body,
		ServiceName:    l.ServiceName,
		AttributesJSON: string(l.AttributesJSON),
		AIInsight:      string(l.AIInsight),
		Timestamp:      l.Timestamp,
	})
}
