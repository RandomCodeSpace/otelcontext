package telemetry

import (
	"os"
	"runtime"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func gatherGauges(t *testing.T, collectors ...prometheus.Collector) map[string][]*dto.Metric {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors...)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string][]*dto.Metric{}
	for _, f := range families {
		out[f.GetName()] = f.GetMetric()
	}
	return out
}

func TestProcessMemoryCollector(t *testing.T) {
	const rssName, heapName = "otelcontext_process_resident_memory_bytes", "otelcontext_go_heap_inuse_bytes"

	t.Run("PlatformReader", func(t *testing.T) {
		got := gatherGauges(t, newProcessMemoryCollector(processRSSReader))
		heap := got[heapName]
		if len(heap) != 1 || heap[0].GetGauge().GetValue() <= 0 {
			t.Fatalf("%s = %v, want one sample > 0", heapName, heap)
		}
		rss, present := got[rssName]
		if runtime.GOOS != "linux" {
			if present {
				t.Fatalf("%s must be omitted on %s", rssName, runtime.GOOS)
			}
			return
		}
		if !present || len(rss) != 1 {
			t.Fatalf("%s absent on linux: %v", rssName, got)
		}
		bytes := int64(rss[0].GetGauge().GetValue())
		if bytes <= 0 || bytes%int64(os.Getpagesize()) != 0 {
			t.Fatalf("%s = %d, want a positive multiple of the page size", rssName, bytes)
		}
		if bytes < int64(heap[0].GetGauge().GetValue()) {
			t.Fatalf("rss %d < heap in use %.0f", bytes, heap[0].GetGauge().GetValue())
		}
	})

	// The non-Linux build wires a nil reader; the collector must then omit
	// the RSS series rather than publish a zero. Exercised here on every
	// platform so the omission is proven without a second OS.
	t.Run("NilReaderOmitsRSS", func(t *testing.T) {
		got := gatherGauges(t, newProcessMemoryCollector(nil))
		if _, present := got[rssName]; present {
			t.Fatalf("%s must be omitted with a nil reader", rssName)
		}
		if len(got[heapName]) != 1 {
			t.Fatalf("%s absent with a nil rss reader", heapName)
		}
	})
}

func TestReadCacheCollector(t *testing.T) {
	c := newReadCacheCollector()
	m := &Metrics{readCaches: c}
	m.RegisterReadCache("mcp_result", func() int { return 7 })
	m.RegisterReadCache("api_ttl", func() int { return 1 })
	m.RegisterReadCache("api_ttl", func() int { return 3 }) // re-registration replaces
	m.RegisterReadCache("ignored", nil)
	var nilMetrics *Metrics
	nilMetrics.RegisterReadCache("x", func() int { return 1 }) // must not panic

	got := gatherGauges(t, c)["otelcontext_read_cache_entries"]
	if len(got) != 2 {
		t.Fatalf("want 2 series, got %v", got)
	}
	want := map[string]float64{"api_ttl": 3, "mcp_result": 7}
	for _, metric := range got {
		name := metric.GetLabel()[0].GetValue()
		if metric.GetLabel()[0].GetName() != "cache" || metric.GetGauge().GetValue() != want[name] {
			t.Errorf("series %v", metric)
		}
	}
}
