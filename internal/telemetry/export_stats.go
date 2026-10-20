package telemetry

import (
	"math"
	"sort"
	"sync"
	"time"
)

// UI counters describe Export responses, not persisted rows or accepted
// telemetry points. The latency ring is explicitly a bounded observation set.
type exportStats struct {
	mu       sync.Mutex
	accepted [3]uint64
	rejected [3]uint64
	duration [1024]float64
	next     int
	count    uint64
}

type ExportStats struct {
	TraceExports  uint64  `json:"trace_exports"`
	MetricExports uint64  `json:"metric_exports"`
	LogExports    uint64  `json:"log_exports"`
	Rejected      uint64  `json:"rejected_exports"`
	ACKP99MS      float64 `json:"ack_p99_ms"`
	LatencyCount  int     `json:"latency_samples"`
	LatencyLimit  int     `json:"latency_sample_limit"`
	LatencyNote   string  `json:"latency_note"`
}

func (m *Metrics) RecordExportResult(signal string, accepted bool, duration time.Duration) {
	if m == nil {
		return
	}
	var index int
	switch signal {
	case "traces":
		index = 0
	case "metrics":
		index = 1
	case "logs":
		index = 2
	default:
		return
	}
	s := &m.exportStats
	s.mu.Lock()
	defer s.mu.Unlock()
	if !accepted {
		s.rejected[index]++
		return
	}
	s.accepted[index]++
	s.duration[s.next] = float64(duration) / float64(time.Millisecond)
	s.next = (s.next + 1) % len(s.duration)
	s.count++
}

func (m *Metrics) ExportStats() ExportStats {
	out := ExportStats{LatencyLimit: 1024, LatencyNote: "Server processing time for the most recent 1024 successful Export calls, since this process started; excludes network transit. Not a time-window SLO."}
	if m == nil {
		return out
	}
	s := &m.exportStats
	s.mu.Lock()
	out.TraceExports, out.MetricExports, out.LogExports = s.accepted[0], s.accepted[1], s.accepted[2]
	out.Rejected = s.rejected[0] + s.rejected[1] + s.rejected[2]
	out.LatencyCount = int(min(s.count, uint64(len(s.duration))))
	values := append([]float64(nil), s.duration[:out.LatencyCount]...)
	s.mu.Unlock()
	if len(values) > 0 {
		sort.Float64s(values)
		out.ACKP99MS = values[int(math.Ceil(float64(len(values))*.99))-1]
	}
	return out
}
