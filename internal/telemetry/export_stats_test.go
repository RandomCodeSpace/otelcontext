package telemetry

import (
	"testing"
	"time"
)

func TestExportStatsBoundedAndSignalCounts(t *testing.T) {
	m := &Metrics{}
	for range 2000 {
		m.RecordExportResult("traces", true, 10*time.Millisecond)
	}
	m.RecordExportResult("metrics", true, 20*time.Millisecond)
	m.RecordExportResult("logs", false, time.Second)
	s := m.ExportStats()
	if s.TraceExports != 2000 || s.MetricExports != 1 || s.LogExports != 0 || s.Rejected != 1 ||
		s.LatencyCount != 1024 || s.ACKP99MS != 10 {
		t.Fatalf("unexpected bounded stats: %+v", s)
	}
}
