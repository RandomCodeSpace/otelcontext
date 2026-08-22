package gatecore

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Reading the server's startup-recovery summary out of its log.
//
// This exists because of a real gap: internal/aggregate publishes only
// recovery DURATION and two row counts to Prometheus
// (otelcontext_aggregate_recovery_duration_seconds, ..._recovery_rows{kind}).
// SkippedSeries — the corruption signal the contract gates on at zero — and
// SeededBaselines have no gauge at all. The only place they surface is the
// slog line emitted by aggregate.LogRecovery, so the gate parses the server's
// own stdout. The gap is recorded in the report; it is not papered over.

// RecoveryLogMarker is the message aggregate.LogRecovery emits.
const RecoveryLogMarker = "Aggregate store recovered"

// RecoveryLogStats is what the log line carries.
type RecoveryLogStats struct {
	Found            bool
	Line             string
	Path             string
	FinalizedWindows int
	ReplayedRows     int
	ReplayedSeries   int
	SeededBaselines  int
	SkippedSeries    int
	Duration         time.Duration
}

// ParseRecoveryLog scans a slog text-handler stream for the recovery summary
// and returns the LAST one, so a log that spans several server incarnations
// yields the most recent recovery.
func ParseRecoveryLog(body string) (RecoveryLogStats, error) {
	var out RecoveryLogStats
	for _, raw := range strings.Split(body, "\n") {
		if !strings.Contains(raw, RecoveryLogMarker) {
			continue
		}
		kv := parseLogfmt(raw)
		s := RecoveryLogStats{Found: true, Line: strings.TrimSpace(raw), Path: kv["path"]}
		s.FinalizedWindows = atoiOr(kv["finalized_windows"], -1)
		s.ReplayedRows = atoiOr(kv["replayed_rows"], -1)
		s.ReplayedSeries = atoiOr(kv["replayed_series_windows"], -1)
		s.SeededBaselines = atoiOr(kv["seeded_baselines"], -1)
		s.SkippedSeries = atoiOr(kv["unresolved_series"], -1)
		if d, err := time.ParseDuration(kv["duration"]); err == nil {
			s.Duration = d
		}
		out = s
	}
	if !out.Found {
		return out, fmt.Errorf("no %q line in the server log", RecoveryLogMarker)
	}
	if out.SkippedSeries < 0 {
		return out, fmt.Errorf("recovery log line carries no unresolved_series field: %q", out.Line)
	}
	return out, nil
}

func atoiOr(s string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return v
}

// parseLogfmt splits a slog text-handler line into key/value pairs, honouring
// double-quoted values.
func parseLogfmt(line string) map[string]string {
	out := make(map[string]string)
	i := 0
	for i < len(line) {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		start := i
		for i < len(line) && line[i] != '=' && line[i] != ' ' {
			i++
		}
		if i >= len(line) || line[i] != '=' {
			continue
		}
		key := line[start:i]
		i++ // past '='
		if i < len(line) && line[i] == '"' {
			i++
			var b strings.Builder
			for i < len(line) {
				if line[i] == '\\' && i+1 < len(line) {
					b.WriteByte(line[i+1])
					i += 2
					continue
				}
				if line[i] == '"' {
					i++
					break
				}
				b.WriteByte(line[i])
				i++
			}
			out[key] = b.String()
			continue
		}
		vs := i
		for i < len(line) && line[i] != ' ' {
			i++
		}
		out[key] = line[vs:i]
	}
	return out
}
