package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"
)

// readySaturationThreshold is the fullness fraction at which a saturation
// probe (DLQ disk, ingest pipeline) flips /ready to 503. Set high enough
// that brief spikes don't cause restart loops, low enough that orchestrators
// stop sending traffic before the system fails outright.
const readySaturationThreshold = 0.95

// aggregateDBPingTimeout bounds the aggregate store reachability probe. Short
// on purpose: a readiness request that waits on a database is a readiness
// request an orchestrator will time out on anyway, and the answer it needs
// ("this process cannot reach its aggregates right now") is the same either
// way.
const aggregateDBPingTimeout = 2 * time.Second

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
	if s.shuttingDown.Load() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ready":  false,
			"checks": map[string]string{"shutdown": "in_progress"},
		})
		return
	}

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

	// Aggregate store recovery. Until the delta log has been replayed the
	// shards hold only part of the acknowledged aggregates, and numbers served
	// from them would be wrong in a way no dashboard can detect (#173).
	switch {
	case s.aggregateRecovered == nil:
		checks["aggregate_store"] = "skipped"
	case !s.aggregateRecovered():
		checks["aggregate_store"] = "recovering"
		ready = false
	default:
		checks["aggregate_store"] = "ok"
	}

	// Disk pressure. At >=95% of the enforcement ceiling the raw exemplar
	// path is off entirely; readiness says so rather than letting an
	// orchestrator keep aiming ingest at a nearly full volume.
	if s.diskPressure == nil {
		checks["disk"] = "skipped"
	} else {
		state, ok := s.diskPressure()
		checks["disk"] = state
		if !ok {
			ready = false
		}
	}

	// Aggregate RUNTIME probes (#194 finding 18). Recovery completing is a
	// one-time gate; these are the ways a process that finished recovery
	// stops being able to serve afterwards. Degraded-not-dead: each one can
	// flip /ready to 503, none of them touches /live and none of them stops
	// the process.
	if !s.aggregateRuntimeChecks(r.Context(), checks) {
		ready = false
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

// aggregateRuntimeChecks fills the aggregate runtime entries of the readiness
// breakdown and reports whether all of them passed.
//
// Check names are part of the operator contract (#202 asserts them): keep
// them stable and keep them documented in CLAUDE.md.
func (s *Server) aggregateRuntimeChecks(ctx context.Context, checks map[string]string) bool {
	ok := true

	// Reachability first: every other number below is read from memory, so
	// this is the only probe that can tell an operator the store itself is
	// gone. The read pool, not the writer — see SQLiteStore.PingContext.
	switch {
	case s.aggregateDBPing == nil:
		checks["aggregate_db"] = "skipped"
	default:
		pingCtx, cancel := context.WithTimeout(ctx, aggregateDBPingTimeout)
		err := s.aggregateDBPing(pingCtx)
		cancel()
		if err != nil {
			checks["aggregate_db"] = "ping failed: " + err.Error()
			ok = false
		} else {
			checks["aggregate_db"] = "ok"
		}
	}

	var rt AggregateRuntime
	if s.aggregateRuntime != nil {
		rt = s.aggregateRuntime()
	}
	th := s.thresholds()
	for _, p := range []struct {
		name, label  string
		value, limit float64
	}{
		{"aggregate_commit", "failure streak", float64(rt.CommitFailureStreak), float64(th.MaxCommitFailureStreak)},
		{"aggregate_finalizer", "failure streak", float64(rt.FinalizeFailureStreak), float64(th.MaxFinalizeFailureStreak)},
		{"aggregate_admission", "saturation", rt.AdmissionRatio, th.MaxAdmissionRatio},
		{"aggregate_delta_log", "age seconds", rt.DeltaLogAgeSeconds, th.MaxDeltaLogAgeSeconds},
		{"aggregate_disk", "usage", rt.DiskRatio(), th.MaxAggregateDiskRatio},
	} {
		if s.aggregateRuntime == nil {
			checks[p.name] = "skipped"
			continue
		}
		if !runtimeProbe(checks, p.name, p.label, p.value, p.limit) {
			ok = false
		}
	}
	return ok
}

// runtimeProbe records one numeric readiness probe under name and reports
// whether it passed. A non-positive limit disables the probe. The measured
// number is in the payload either way: "ok" without a figure is not something
// an operator can alarm on or graph.
func runtimeProbe(checks map[string]string, name, label string, value, limit float64) bool {
	switch {
	case limit <= 0:
		checks[name] = "skipped"
		return true
	case value >= limit:
		checks[name] = fmt.Sprintf("%s %s >= %s", label, formatMeasure(value), formatMeasure(limit))
		return false
	default:
		checks[name] = fmt.Sprintf("ok (%s %s)", label, formatMeasure(value))
		return true
	}
}

// formatMeasure renders a probe figure: whole numbers for counts and ages,
// two decimals for ratios.
func formatMeasure(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e9 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}
