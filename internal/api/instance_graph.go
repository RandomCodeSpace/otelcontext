package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

func (s *Server) SetInstanceGraph(g *topology.InstanceGraph) { s.instanceGraph = g }

func graphRange(r *http.Request) (time.Time, time.Time, error) {
	end := time.Now()
	start := end.Add(-15 * time.Minute)
	var err error
	if raw := r.URL.Query().Get("end"); raw != "" {
		end, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return start, end, errors.New("end must be RFC3339")
		}
		start = end.Add(-15 * time.Minute)
	}
	if raw := r.URL.Query().Get("start"); raw != "" {
		start, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return start, end, errors.New("start must be RFC3339")
		}
	}
	if !end.After(start) || end.Sub(start) > 24*time.Hour || end.After(time.Now().Add(2*time.Minute)) {
		return start, end, errors.New("range must be positive, no more than 24 hours, and not in the future")
	}
	return start, end, nil
}

func (s *Server) handleTopologyCatalog(w http.ResponseWriter, r *http.Request) {
	if s.instanceGraph == nil {
		http.Error(w, "instance topology is unavailable on this storage adapter", http.StatusServiceUnavailable)
		return
	}
	start, end, err := graphRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.instanceGraph.Catalog(storage.TenantFromContext(r.Context()), start, end))
}

func (s *Server) handleInstanceGraph(w http.ResponseWriter, r *http.Request) {
	if s.instanceGraph == nil {
		http.Error(w, "instance topology is unavailable on this storage adapter", http.StatusServiceUnavailable)
		return
	}
	start, end, err := graphRange(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v := r.URL.Query()
	q := topology.InstanceQuery{Start: start, End: end, Mode: "services", Hops: 1, Limit: 40, ServiceID: v.Get("service_id")}
	if v.Has("environment") {
		env := v.Get("environment")
		q.Environment = &env
		if len(env) > topology.RegistryMaxValueBytes {
			http.Error(w, "environment is too long", http.StatusBadRequest)
			return
		}
	}
	if v.Has("mode") {
		q.Mode = v.Get("mode")
	}
	for key, target := range map[string]*int{"hops": &q.Hops, "limit": &q.Limit} {
		if v.Has(key) {
			parsed, err := strconv.Atoi(v.Get(key))
			if err != nil {
				http.Error(w, "invalid "+key, http.StatusBadRequest)
				return
			}
			*target = parsed
		}
	}
	if (q.Mode != "services" && q.Mode != "instances") || q.Hops < 1 || q.Hops > 2 || q.Limit < 1 || q.Limit > 100 || len(q.ServiceID) > 64 {
		http.Error(w, "mode, hops, limit or service_id is invalid", http.StatusBadRequest)
		return
	}
	select {
	case s.topologyReads <- struct{}{}:
		defer func() { <-s.topologyReads }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "topology query slots are busy", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.instanceGraph.Snapshot(ctx, storage.TenantFromContext(r.Context()), q)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, "topology query could not complete", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(result)
}
