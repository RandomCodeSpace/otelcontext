package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// readySaturationThreshold is the fullness fraction at which a saturation
// probe (DLQ disk, ingest pipeline) flips /ready to 503. Set high enough
// that brief spikes don't cause restart loops, low enough that orchestrators
// stop sending traffic before the system fails outright.
const readySaturationThreshold = 0.95

// handleLive is a Kubernetes-style liveness probe.
// Returns 200 OK as long as the process is up. Does not check dependencies.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "alive"})
}

// handleReady is a Kubernetes-style readiness probe.
// Returns 200 only if the service can serve traffic: DB ping succeeds and
// the GraphRAG coordinator is running. Returns 503 with a per-check breakdown
// on failure.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{
		"database": "ok",
		"graphrag": "ok",
	}
	ready := true

	// DB ping with a short timeout so the probe cannot hang.
	if s.repo == nil {
		checks["database"] = "repository not initialized"
		ready = false
	} else {
		sqlDB, err := s.repo.DB().DB()
		if err != nil {
			checks["database"] = "failed to obtain sql.DB: " + err.Error()
			ready = false
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := sqlDB.PingContext(ctx); err != nil {
				checks["database"] = "ping failed: " + err.Error()
				ready = false
			}
		}
	}

	// GraphRAG running check. The coordinator is optional in some configurations
	// (e.g. pure tests), so treat a nil instance as "skipped" rather than fatal.
	switch {
	case s.graphRAG == nil:
		checks["graphrag"] = "skipped"
	case !s.graphRAG.IsRunning():
		checks["graphrag"] = "not running"
		ready = false
	}

	// Saturation probes — flip to 503 when downstream buffers are full so
	// orchestrators (k8s, load balancers) stop routing fresh traffic before
	// the pipeline starts hard-rejecting (gRPC RESOURCE_EXHAUSTED / HTTP 429)
	// or DLQ starts FIFO-evicting unflushed batches.
	if s.dlqSaturation != nil {
		if sat := s.dlqSaturation(); sat >= readySaturationThreshold {
			checks["dlq_disk"] = fmt.Sprintf("saturated %.0f%%", sat*100)
			ready = false
		} else {
			checks["dlq_disk"] = "ok"
		}
	} else {
		checks["dlq_disk"] = "skipped"
	}
	if s.pipelineSaturation != nil {
		if sat := s.pipelineSaturation(); sat >= readySaturationThreshold {
			checks["pipeline"] = fmt.Sprintf("saturated %.0f%%", sat*100)
			ready = false
		} else {
			checks["pipeline"] = "ok"
		}
	} else {
		checks["pipeline"] = "skipped"
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ready":  ready,
		"checks": checks,
	})
}
