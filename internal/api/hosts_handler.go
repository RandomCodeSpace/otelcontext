package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

// hostProjection reads hosts through the mode-selected topology owner so
// every mode answers from the same registry. Without a provider there are no
// hosts.
func (s *Server) hostProjection(ctx context.Context) topology.HostProjection {
	if s.topology == nil {
		return topology.ProjectHosts(nil)
	}
	return s.topology.Hosts(ctx)
}

// handleGetHosts handles GET /api/hosts: the tenant's hosts as a bare array
// sorted by name, each with its bounded service list.
func (s *Server) handleGetHosts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(s.hostProjection(r.Context()).Hosts)
}

// handleGetHost handles GET /api/hosts/{host}; 404 when the tenant has no
// such host.
func (s *Server) handleGetHost(w http.ResponseWriter, r *http.Request) {
	host, ok := s.hostProjection(r.Context()).Host(r.PathValue("host"))
	if !ok {
		http.Error(w, "host not found", http.StatusNotFound)
		return
	}
	w.Header().Set(httpconst.HeaderContentType, httpconst.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(host)
}
