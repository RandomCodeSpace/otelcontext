package aggregate

import (
	"math"
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/latency"
)

func TestSketchLatencyProvenanceStates(t *testing.T) {
	if got := PercentileFromSketch(nil); got.Status != latency.StatusUnavailable || got.Reason != latency.ReasonNoObservations {
		t.Fatalf("nil sketch provenance = %+v", got)
	}

	sketch, err := NewSketchAtScale(2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 99; i++ {
		sketch.Observe(10)
	}
	got := PercentileFromSketch(sketch)
	if got.Status != latency.StatusApproximate || got.Method != latency.MethodDDSketch || got.SampleCount != 99 || !got.LowSample || got.SketchScale != 2 || math.Abs(got.RelativeErrorBound-sketch.RelativeError()) > 1e-12 {
		t.Fatalf("ordinary sketch provenance = %+v", got)
	}

	sketch.Observe(10)
	sketch.collapsed = true
	sketch.saturations = 3
	got = PercentileFromSketch(sketch)
	if !got.Degraded || !got.Collapsed || got.Saturations != 3 || got.Reason != latency.ReasonSketchCollapsedSaturated || got.LowSample {
		t.Fatalf("degraded sketch provenance = %+v", got)
	}
}
