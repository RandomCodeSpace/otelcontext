package ingest

import "testing"

func TestPipelineStatsSaturation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats PipelineStats
		want  float64
	}{
		{"unset", PipelineStats{}, 0},
		{"disabled", PipelineStats{Capacity: -1, MaxBytes: -1}, 0},
		{"count below threshold", PipelineStats{QueueDepth: 94, Capacity: 100}, .94},
		{"count at threshold", PipelineStats{QueueDepth: 95, Capacity: 100}, .95},
		{"bytes below threshold", PipelineStats{QueueBytes: 94, MaxBytes: 100}, .94},
		{"bytes at threshold", PipelineStats{QueueBytes: 95, MaxBytes: 100}, .95},
		{"bytes win", PipelineStats{QueueDepth: 2, Capacity: 100, QueueBytes: 98, MaxBytes: 100}, .98},
		{"count wins", PipelineStats{QueueDepth: 100, Capacity: 100, QueueBytes: 98, MaxBytes: 100}, 1},
		{"processing retains bytes", PipelineStats{Capacity: 100, QueueBytes: 100, MaxBytes: 100}, 1},
		{"not clamped", PipelineStats{QueueBytes: 110, MaxBytes: 100}, 1.1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stats.Saturation(); got != tc.want {
				t.Fatalf("Saturation = %v, want %v", got, tc.want)
			}
		})
	}
}
