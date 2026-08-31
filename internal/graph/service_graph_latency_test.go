package graph

import (
	"strconv"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/latency"
)

func TestServiceLatencyProvenanceBoundaries(t *testing.T) {
	for _, count := range []int{1, 99, 100, 1000, 1001} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			rows := make([]SpanRow, count)
			for i := range rows {
				duration := 10.0
				if count >= 1000 && i >= 989 {
					duration = 1000
				}
				rows[i] = SpanRow{SpanID: strconv.Itoa(i), ServiceName: "api", DurationMs: duration, Timestamp: time.Now().UTC()}
			}
			g := New(func(time.Time) ([]SpanRow, error) { return rows, nil }, 5*time.Minute, time.Minute)
			g.rebuild()
			node := g.Snapshot().Nodes["api"]
			if node == nil || node.LatencyProvenance == nil || node.LatencyProvenance.P99 == nil {
				t.Fatalf("node provenance missing: %+v", node)
			}
			p99 := node.LatencyProvenance.P99
			wantStatus := latency.StatusMeasured
			wantMethod := latency.MethodNearestRank
			wantSamples := count
			if count > serviceLatencySampleLimit {
				wantStatus = latency.StatusBounded
				wantMethod = latency.MethodRetainedPrefix
				wantSamples = serviceLatencySampleLimit
			}
			if p99.Status != wantStatus || p99.Method != wantMethod || p99.SampleCount != uint64(wantSamples) || p99.LowSample != (wantSamples < 100) {
				t.Fatalf("count=%d provenance=%+v", count, p99)
			}
			if count >= 1000 && node.P99LatencyMs != 1000 {
				t.Fatalf("count=%d p99=%v, want 1000", count, node.P99LatencyMs)
			}
			if count == 1001 && (p99.PopulationCount != 1001 || p99.SampleLimit != 1000) {
				t.Fatalf("bounded provenance=%+v", p99)
			}
		})
	}
}
