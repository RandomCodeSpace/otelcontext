package ingest

import (
	"testing"

	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

func TestPipelineExposesExplicitZeroDropSeries(t *testing.T) {
	metrics := telemetry.New()
	NewPipeline(nil, metrics, PipelineConfig{})

	children := make(chan prometheus.Metric, 16)
	metrics.IngestPipelineDroppedTotal.Collect(children)
	close(children)
	if got := len(children); got != 8 {
		t.Fatalf("pipeline drop child series = %d, want 8 (two signals x four bounded reasons)", got)
	}
}
