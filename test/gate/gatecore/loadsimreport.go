package gatecore

import (
	"encoding/json"
	"fmt"
	"os"
)

// Parsing test/loadsim's JSON report.
//
// These mirror the shapes loadsim writes (otlpdirect.go). They are declared
// here as a read-only contract so the gate never has to be built with the
// loadtest tag to score a run.

// LoadsimLatency is one (phase, signal) latency summary.
type LoadsimLatency struct {
	Samples int64   `json:"samples"`
	MinMs   float64 `json:"min_ms"`
	P50Ms   float64 `json:"p50_ms"`
	P90Ms   float64 `json:"p90_ms"`
	P95Ms   float64 `json:"p95_ms"`
	P99Ms   float64 `json:"p99_ms"`
	P999Ms  float64 `json:"p999_ms"`
	MaxMs   float64 `json:"max_ms"`
	MeanMs  float64 `json:"mean_ms"`
}

// LoadsimPhase is one phase's accounting.
type LoadsimPhase struct {
	Phase        string                    `json:"phase"`
	DurationSec  float64                   `json:"duration_sec"`
	All          LoadsimLatency            `json:"ack_latency_all_signals"`
	BySignal     map[string]LoadsimLatency `json:"ack_latency_by_signal"`
	PointsSent   int64                     `json:"points_sent"`
	PointsAcked  int64                     `json:"points_acked"`
	PointsPerSec float64                   `json:"points_acked_per_sec"`
	RequestsOK   int64                     `json:"requests_ok"`
	RequestsErr  int64                     `json:"requests_err"`
	Exhausted    int64                     `json:"resource_exhausted"`
	Unavailable  int64                     `json:"unavailable"`
	OtherErrors  int64                     `json:"other_errors"`
}

// LoadsimReport is the whole document.
type LoadsimReport struct {
	StartedAt string         `json:"started_at"`
	EndedAt   string         `json:"ended_at"`
	Config    map[string]any `json:"config"`
	Phases    []LoadsimPhase `json:"phases"`
	FirstErr  string         `json:"first_error"`
}

// LoadLoadsimReport reads a loadsim report from disk.
func LoadLoadsimReport(path string) (LoadsimReport, error) {
	var rep LoadsimReport
	b, err := os.ReadFile(path) // #nosec G304 -- gate-generated report path
	if err != nil {
		return rep, err
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return rep, fmt.Errorf("parse loadsim report %s: %w", path, err)
	}
	if len(rep.Phases) == 0 {
		return rep, fmt.Errorf("loadsim report %s carries no phases", path)
	}
	return rep, nil
}

// PhaseNamed extracts one phase as the report's LoadPhase shape. A phase that
// recorded no ACK samples is treated as absent: it measured nothing.
func (rep LoadsimReport) PhaseNamed(source, phase string) LoadPhase {
	for _, p := range rep.Phases {
		if p.Phase != phase {
			continue
		}
		if p.All.Samples == 0 {
			return LoadPhase{Source: source, Phase: phase}
		}
		return LoadPhase{
			Present:      true,
			Source:       source,
			Phase:        phase,
			DurationSec:  p.DurationSec,
			Samples:      p.All.Samples,
			P50Ms:        p.All.P50Ms,
			P90Ms:        p.All.P90Ms,
			P99Ms:        p.All.P99Ms,
			P999Ms:       p.All.P999Ms,
			MaxMs:        p.All.MaxMs,
			PointsSent:   p.PointsSent,
			PointsAcked:  p.PointsAcked,
			PointsPerSec: p.PointsPerSec,
			RequestsOK:   p.RequestsOK,
			RequestsErr:  p.RequestsErr,
			Exhausted:    p.Exhausted,
			Unavailable:  p.Unavailable,
			OtherErrors:  p.OtherErrors,
			FirstErr:     rep.FirstErr,
		}
	}
	return LoadPhase{Source: source, Phase: phase}
}
