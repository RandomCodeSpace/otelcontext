package telemetry

import (
	"runtime/metrics"
	"sort"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	heapObjectsMetric = "/memory/classes/heap/objects:bytes"
	heapUnusedMetric  = "/memory/classes/heap/unused:bytes"
)

// processMemoryCollector publishes the process resident set and the Go heap
// in use (#283, #292). Both are sampled on scrape: no goroutine, no cached
// value, the scrape is the sample.
//
// otelcontext_go_heap_inuse_bytes is read through runtime/metrics as
// /memory/classes/heap/objects:bytes + /memory/classes/heap/unused:bytes,
// which is exactly MemStats.HeapInuse (bytes in in-use spans) without the
// stop-the-world that runtime.ReadMemStats costs on every scrape.
//
// otelcontext_process_resident_memory_bytes comes from rss, which the Linux
// build wires to /proc/self/statm (resident pages × page size). A nil reader,
// every other platform, omits the series entirely: a zero would read as an
// empty process rather than an absent witness.
type processMemoryCollector struct {
	rss      func() (int64, error)
	rssDesc  *prometheus.Desc
	heapDesc *prometheus.Desc
}

func newProcessMemoryCollector(rss func() (int64, error)) *processMemoryCollector {
	return &processMemoryCollector{
		rss: rss,
		rssDesc: prometheus.NewDesc("otelcontext_process_resident_memory_bytes",
			"Resident set size of the process from /proc/self/statm, sampled on scrape. Linux only.", nil, nil),
		heapDesc: prometheus.NewDesc("otelcontext_go_heap_inuse_bytes",
			"Go heap bytes in in-use spans (runtime/metrics heap objects + unused, equal to MemStats.HeapInuse), sampled on scrape.", nil, nil),
	}
}

func (c *processMemoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.heapDesc
	if c.rss != nil {
		ch <- c.rssDesc
	}
}

func (c *processMemoryCollector) Collect(ch chan<- prometheus.Metric) {
	samples := []metrics.Sample{{Name: heapObjectsMetric}, {Name: heapUnusedMetric}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 && samples[1].Value.Kind() == metrics.KindUint64 {
		ch <- prometheus.MustNewConstMetric(c.heapDesc, prometheus.GaugeValue,
			float64(samples[0].Value.Uint64()+samples[1].Value.Uint64()))
	}
	if c.rss == nil {
		return
	}
	if bytes, err := c.rss(); err == nil {
		ch <- prometheus.MustNewConstMetric(c.rssDesc, prometheus.GaugeValue, float64(bytes))
	}
}

// readCacheCollector publishes otelcontext_read_cache_entries{cache}: the live
// entry count of every registered read cache, sampled on scrape. The caches
// (API TTL cache, MCP result cache) register a size function once at
// construction; re-registering a name replaces the function, so a test that
// builds several servers never trips a duplicate-collector panic.
type readCacheCollector struct {
	mu    sync.Mutex
	sizes map[string]func() int
	desc  *prometheus.Desc
}

func newReadCacheCollector() *readCacheCollector {
	return &readCacheCollector{
		sizes: map[string]func() int{},
		desc: prometheus.NewDesc("otelcontext_read_cache_entries",
			"Live entries in a server-side read cache (api_ttl: dashboard, service-map and ETag entries; mcp_result: cached tools/call results), sampled on scrape.",
			[]string{"cache"}, nil),
	}
}

func (c *readCacheCollector) register(name string, size func() int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sizes[name] = size
}

func (c *readCacheCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *readCacheCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	names := make([]string, 0, len(c.sizes))
	fns := make([]func() int, 0, len(c.sizes))
	for name := range c.sizes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fns = append(fns, c.sizes[name])
	}
	c.mu.Unlock()
	for i, name := range names {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(fns[i]()), name)
	}
}

// RegisterReadCache exposes a read cache's live entry count as
// otelcontext_read_cache_entries{cache=name}. Safe on a nil receiver.
func (m *Metrics) RegisterReadCache(name string, size func() int) {
	if m == nil || m.readCaches == nil || size == nil {
		return
	}
	m.readCaches.register(name, size)
}
