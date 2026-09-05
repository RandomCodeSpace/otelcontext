package main

import (
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
)

// TestLegacyMetricPathByMode pins the wiring decision behind #194 finding 10:
// aggregate mode constructs no TSDB aggregator or metric callback, while legacy
// and shadow keep both. Shadow especially — it runs both paths side by side,
// and dropping the legacy one would leave nothing to shadow.
func TestLegacyMetricPathByMode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{
		{aggregate.ModeLegacy, true},
		{aggregate.ModeShadow, true},
		{aggregate.ModeAggregate, false},
		{"", true}, // empty config value means legacy
	} {
		if got := legacyMetricPath(tc.mode); got != tc.want {
			t.Errorf("legacyMetricPath(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}
