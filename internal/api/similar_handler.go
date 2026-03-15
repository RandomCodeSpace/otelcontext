package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// handleGetSimilarLogs handles GET /api/logs/similar?q=<text>&limit=10
// Returns logs semantically similar to the query string using TF-IDF cosine similarity.
func (s *Server) handleGetSimilarLogs(w http.ResponseWriter, r *http.Request) {
	if s.vectorIdx == nil {
		http.Error(w, "vector index not initialized", http.StatusServiceUnavailable)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, "q parameter is required", http.StatusBadRequest)
		return
	}

	limit := 10
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if n, err := strconv.Atoi(lStr); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 50 {
		limit = 50
	}

	results := s.vectorIdx.Search(query, limit)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"query":   query,
		"count":   len(results),
		"results": results,
	})
}
