package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/RandomCodeSpace/otelcontext/internal/api/views"
)

// handleGetTraces handles GET /api/traces
func (s *Server) handleGetTraces(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePaging(r, 20)

	start, end, err := parseTimeRange(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("Invalid time range: %v", err), http.StatusBadRequest)
		return
	}

	serviceNames := r.URL.Query()["service_name"]
	status := r.URL.Query().Get("status")
	search := r.URL.Query().Get("search")
	sortBy := r.URL.Query().Get("sort_by")
	orderBy := r.URL.Query().Get("order_by")

	response, err := s.repo.GetTracesFiltered(r.Context(), start, end, serviceNames, status, search, limit, offset, sortBy, orderBy)
	if err != nil {
		slog.Error("Failed to get filtered traces", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(views.TracesResponseFromModel(response))
}

// handleGetTraceByID handles GET /api/traces/{id}
func (s *Server) handleGetTraceByID(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("id")
	if traceID == "" {
		http.Error(w, "missing trace id", http.StatusBadRequest)
		return
	}

	trace, err := s.repo.GetTrace(r.Context(), traceID)
	if err != nil {
		slog.Error("Trace not found", "trace_id", traceID, "error", err) // #nosec G706 -- slog uses structured k/v fields
		http.Error(w, "trace not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(views.TraceFromModel(*trace))
}
