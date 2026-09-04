// Package readproof holds the untagged half of the read-latency proof
// (issue #289, decision #281): the result schema, exact ordered percentiles,
// threshold evaluation and the JSON writer. The tagged half (build tag
// readproof) starts the exact binary, prefills a shape and fills these
// structures in.
//
// The JSON is the source of truth. Every objective produces an assertion that
// carries the measured number and the objective; missing evidence is a failed
// assertion with its reason, never a blank.
package readproof

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SchemaVersion identifies the artifact layout.
const SchemaVersion = "otelcontext.read-latency.v1"

// FileName is the artifact written into OTELCONTEXT_PROOF_DIR.
const FileName = "read-latency-v1.json"

// Objectives are the decision's numbers for one shape: latency from #281,
// RSS steady p95 from #283.
type Objectives struct {
	Requests          int     `json:"requests"`
	WarmP99MS         float64 `json:"warm_p99_ms"`
	ColdMS            float64 `json:"cold_ms"`
	RSSSteadyP95Bytes int64   `json:"rss_steady_p95_bytes"`
}

// MiB is the unit the memory objectives are written in.
const MiB = 1 << 20

// Metric names the proof reads from the server's Prometheus exposition.
const (
	RSSMetric              = "otelcontext_process_resident_memory_bytes"
	HeapMetric             = "otelcontext_go_heap_inuse_bytes"
	RegistryEntriesMetric  = "otelcontext_resource_registry_entries"
	GraphRAGEntitiesMetric = "otelcontext_graphrag_store_entities"
	GraphRAGEdgesMetric    = "otelcontext_graphrag_store_edges"
	ReadCacheEntriesMetric = "otelcontext_read_cache_entries"
)

// Percentiles are exact ordered (nearest-rank) statistics in milliseconds.
type Percentiles struct {
	P50 float64 `json:"p50_ms"`
	P90 float64 `json:"p90_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
}

// Call is one recorded response.
type Call struct {
	MS       float64 `json:"ms"`
	Status   int     `json:"status"`
	Bytes    int     `json:"bytes"`
	Coverage string  `json:"coverage"`
	// BodyCoverage is the `coverage` field of an object-shaped body; the
	// dashboard and service-map views carry coverage there, not in the header.
	BodyCoverage string `json:"body_coverage,omitempty"`
	Error        string `json:"error,omitempty"`
}

// Measurement is one endpoint's evidence.
type Measurement struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // rest | mcp
	Path      string `json:"path,omitempty"`
	Query     string `json:"query,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// Cache is "client" when the request is what a real client sends and
	// "miss" when an ignored nonce argument defeats the MCP result cache.
	Cache    string `json:"cache,omitempty"`
	Asserted bool   `json:"asserted"`

	Cold          Call        `json:"cold"`
	Warmup        int         `json:"warmup"`
	Requests      int         `json:"requests"`
	BudgetSeconds float64     `json:"budget_seconds"`
	Seconds       float64     `json:"seconds"`
	Latency       Percentiles `json:"latency"`
	SamplesMS     []float64   `json:"samples_ms"`
	Status        int         `json:"status"`
	Coverage      string      `json:"coverage"`
	BodyCoverage  string      `json:"body_coverage,omitempty"`
	// RequestedStart and EffectiveStart are the aggregate range-clamp
	// headers (#217), present only when the server shortened the range.
	RequestedStart string `json:"requested_start,omitempty"`
	EffectiveStart string `json:"effective_start,omitempty"`
	ResponseBytes  int    `json:"response_bytes"`
	MaxBytes       int    `json:"response_bytes_max"`
	CacheHits      int    `json:"cache_hits"`
	Errors         int    `json:"errors"`
	Error          string `json:"error,omitempty"`
}

// RSSSample is one scrape of the server's own memory gauges, offset from
// server start: otelcontext_process_resident_memory_bytes and
// otelcontext_go_heap_inuse_bytes.
type RSSSample struct {
	Seconds   float64 `json:"t_s"`
	Bytes     int64   `json:"bytes"`
	HeapBytes int64   `json:"heap_inuse_bytes"`
}

// RSS is the server's resident-set series across the run, read from the
// server's /metrics every 5 s. Peak spans the whole run; the steady p95 is
// the exact ordered p95 over samples taken at or after SteadyFromSeconds —
// the start of the measurement phase, once prefill and readiness are done —
// and is what #283's objective is asserted against.
type RSS struct {
	Source            string      `json:"source"`
	Samples           []RSSSample `json:"samples"`
	PeakBytes         int64       `json:"peak_bytes"`
	HeapPeakBytes     int64       `json:"heap_inuse_peak_bytes"`
	SteadyFromSeconds float64     `json:"steady_from_s"`
	// SettleSeconds is how long the harness waited after seeding for the
	// runtime's first GC cycle; SteadyRule says how the window was chosen.
	SettleSeconds float64 `json:"settle_s"`
	SteadyRule    string  `json:"steady_rule"`
	// SteadyReached is false when the harness ran out of budget before the
	// RSS gauge plateaued under the read workload; SteadyReason says where
	// it stood. The assertion then fails: an under-sampled p95 is not
	// evidence.
	SteadyReached  bool   `json:"steady_reached"`
	SteadyReason   string `json:"steady_reason,omitempty"`
	SteadyP95Bytes int64  `json:"steady_p95_bytes"`
	SteadySamples  int    `json:"steady_samples"`
	Error          string `json:"error,omitempty"`
}

// MemoryAccounting says where the memory sits at the end of the measurement
// phase: one /metrics read of the counts the resource registry, the GraphRAG
// stores (with the #291 latency sketches) and the read caches publish, next
// to the RSS and heap gauges scraped at the same instant.
type MemoryAccounting struct {
	Seconds        float64 `json:"t_s"`
	RSSBytes       int64   `json:"rss_bytes"`
	HeapInuseBytes int64   `json:"heap_inuse_bytes"`
	// Registry counts are summed over tenants: kind=pair entries and
	// kind=host distinct hosts.
	RegistryPairEntries int64 `json:"registry_pair_entries"`
	RegistryHostEntries int64 `json:"registry_host_entries"`
	// GraphRAG counts are the census gauges by entity kind and edge store.
	GraphRAGEntities map[string]int64 `json:"graphrag_entities"`
	GraphRAGEdges    map[string]int64 `json:"graphrag_edges"`
	// LatencySketchBytes is latency_sketches × the fixed size of one
	// aggregate.Sketch value.
	LatencySketchBytes int64 `json:"graphrag_latency_sketch_bytes"`
	// ReadCacheEntries is otelcontext_read_cache_entries by cache name.
	ReadCacheEntries map[string]int64 `json:"read_cache_entries"`
	// MappingsBefore and MappingsAfter break the RSS down by mapping owner
	// from /proc/<pid>/smaps at the start and the end of the read phase, so
	// growth under reads is attributed rather than guessed.
	MappingsBefore Mappings `json:"mappings_before_reads"`
	MappingsAfter  Mappings `json:"mappings_after_reads"`
	Error          string   `json:"error,omitempty"`
}

// Mappings is the resident set broken down by mapping owner, in bytes, from
// one /proc/<pid>/smaps read. The Go runtime names its mappings (Linux
// prctl PR_SET_VMA), which is what separates the heap from everything else.
type Mappings struct {
	Seconds    float64 `json:"t_s"`
	TotalBytes int64   `json:"total_bytes"`
	// GoHeapBytes is "[anon: Go: heap]": the runtime heap arenas.
	GoHeapBytes int64 `json:"go_heap_bytes"`
	// GoRuntimeBytes is every other named Go mapping (metadata, GC bits,
	// spans, scavenger structures) plus the main thread stack and vDSO.
	GoRuntimeBytes int64 `json:"go_runtime_bytes"`
	// OtherAnonBytes is unnamed anonymous memory: with the pure-Go SQLite
	// driver this is modernc's libc allocator — the page cache, the sorter
	// and temp tables — plus anything else outside the Go heap.
	OtherAnonBytes int64 `json:"other_anon_bytes"`
	// FileBytes is file-backed and shmem-backed residency: an mmapped
	// database, shared libraries, tmpfs files.
	FileBytes int64 `json:"file_bytes"`
	// BinaryBytes is the executable's own text and data.
	BinaryBytes int64  `json:"binary_bytes"`
	Error       string `json:"error,omitempty"`
}

// ParseSmaps classifies every mapping in a /proc/<pid>/smaps payload and
// sums its Rss; binary is the executable path.
func ParseSmaps(text, binary string) Mappings {
	var m Mappings
	add := func(*Mappings, int64) {}
	for _, line := range strings.Split(text, "\n") {
		if len(line) > 0 && isHexByte(line[0]) && strings.Contains(line, "-") && strings.Count(line, " ") >= 4 {
			fields := strings.Fields(line)
			name := ""
			if len(fields) >= 6 {
				name = strings.Join(fields[5:], " ")
			}
			switch {
			case name == "[anon: Go: heap]":
				add = func(m *Mappings, b int64) { m.GoHeapBytes += b }
			case strings.HasPrefix(name, "[anon: Go:"), strings.HasPrefix(name, "[stack"), name == "[vdso]", name == "[vvar]", name == "[vsyscall]":
				add = func(m *Mappings, b int64) { m.GoRuntimeBytes += b }
			case name == "" || name == "[heap]" || strings.HasPrefix(name, "[anon"):
				add = func(m *Mappings, b int64) { m.OtherAnonBytes += b }
			case name == binary:
				add = func(m *Mappings, b int64) { m.BinaryBytes += b }
			default:
				add = func(m *Mappings, b int64) { m.FileBytes += b }
			}
			continue
		}
		if strings.HasPrefix(line, "Rss:") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[2] == "kB" {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					add(&m, kb*1024)
					m.TotalBytes += kb * 1024
				}
			}
		}
	}
	if m.TotalBytes == 0 {
		m.Error = "no Rss lines parsed"
	}
	return m
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// MetricSample is one line of Prometheus text exposition.
type MetricSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// ParseMetrics reads Prometheus text exposition into samples. Comment and
// blank lines are skipped; a malformed line is an error naming it.
func ParseMetrics(text string) ([]MetricSample, error) {
	var out []MetricSample
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, err := parseMetricLine(line)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func parseMetricLine(line string) (MetricSample, error) {
	s := MetricSample{Labels: map[string]string{}}
	var body string
	if i := strings.IndexByte(line, '{'); i >= 0 {
		end := strings.LastIndexByte(line, '}')
		if end < i {
			return s, fmt.Errorf("metric line %q: unterminated labels", line)
		}
		s.Name = line[:i]
		for _, pair := range splitLabels(line[i+1 : end]) {
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				return s, fmt.Errorf("metric line %q: label %q", line, pair)
			}
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return s, fmt.Errorf("metric line %q: label %q: %w", line, pair, err)
			}
			s.Labels[key] = unquoted
		}
		body = strings.TrimSpace(line[end+1:])
	} else {
		name, rest, ok := strings.Cut(line, " ")
		if !ok {
			return s, fmt.Errorf("metric line %q: no value", line)
		}
		s.Name, body = name, strings.TrimSpace(rest)
	}
	// A trailing timestamp is legal; the value is the first field.
	value := strings.Fields(body)
	if len(value) == 0 {
		return s, fmt.Errorf("metric line %q: no value", line)
	}
	v, err := strconv.ParseFloat(value[0], 64)
	if err != nil {
		return s, fmt.Errorf("metric line %q: value: %w", line, err)
	}
	s.Value = v
	return s, nil
}

// splitLabels splits `a="x",b="y,z"` on commas outside quotes.
func splitLabels(raw string) []string {
	var out []string
	start, quoted := 0, false
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			if quoted {
				i++
			}
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				out = append(out, strings.TrimSpace(raw[start:i]))
				start = i + 1
			}
		}
	}
	if rest := strings.TrimSpace(raw[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// Gauge returns the single unlabelled sample of name.
func Gauge(samples []MetricSample, name string) (float64, error) {
	for _, s := range samples {
		if s.Name == name && len(s.Labels) == 0 {
			return s.Value, nil
		}
	}
	return 0, fmt.Errorf("%s absent from /metrics", name)
}

// GaugeByLabel maps one label's values to the sample values for name.
func GaugeByLabel(samples []MetricSample, name, label string) map[string]int64 {
	out := map[string]int64{}
	for _, s := range samples {
		if s.Name == name {
			out[s.Labels[label]] = int64(s.Value)
		}
	}
	return out
}

// GaugeSum adds every sample of name whose labels include want.
func GaugeSum(samples []MetricSample, name string, want map[string]string) int64 {
	var total int64
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		match := true
		for k, v := range want {
			if s.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			total += int64(s.Value)
		}
	}
	return total
}

// Account fills the accounting from one exposition read; sketchBytes is the
// size of one aggregate.Sketch value.
func Account(samples []MetricSample, sketchBytes int64) MemoryAccounting {
	a := MemoryAccounting{
		GraphRAGEntities: GaugeByLabel(samples, GraphRAGEntitiesMetric, "entity"),
		GraphRAGEdges:    GaugeByLabel(samples, GraphRAGEdgesMetric, "store"),
		ReadCacheEntries: GaugeByLabel(samples, ReadCacheEntriesMetric, "cache"),
	}
	a.RegistryPairEntries = GaugeSum(samples, RegistryEntriesMetric, map[string]string{"kind": "pair"})
	a.RegistryHostEntries = GaugeSum(samples, RegistryEntriesMetric, map[string]string{"kind": "host"})
	a.LatencySketchBytes = a.GraphRAGEntities["latency_sketches"] * sketchBytes
	var errs []string
	if v, err := Gauge(samples, RSSMetric); err == nil {
		a.RSSBytes = int64(v)
	} else {
		errs = append(errs, err.Error())
	}
	if v, err := Gauge(samples, HeapMetric); err == nil {
		a.HeapInuseBytes = int64(v)
	} else {
		errs = append(errs, err.Error())
	}
	for _, name := range []string{GraphRAGEntitiesMetric, GraphRAGEdgesMetric, ReadCacheEntriesMetric} {
		if len(GaugeByLabel(samples, name, "")) == 0 {
			errs = append(errs, name+" absent from /metrics")
		}
	}
	a.Error = strings.Join(errs, "; ")
	return a
}

// Prefill describes the seeded history. Fields are shape-specific.
type Prefill struct {
	Seconds  float64 `json:"seconds"`
	Services int     `json:"services"`
	Error    string  `json:"error,omitempty"`

	// Aggregate shape.
	Windows          int   `json:"windows,omitempty"`
	RequestedWindows int   `json:"requested_windows,omitempty"`
	Series           int   `json:"series,omitempty"`
	AggregateDBBytes int64 `json:"aggregate_db_bytes,omitempty"`

	// Legacy shape.
	Days       int   `json:"days,omitempty"`
	Traces     int   `json:"traces,omitempty"`
	Spans      int   `json:"spans,omitempty"`
	Logs       int   `json:"logs,omitempty"`
	MainDBByte int64 `json:"main_db_bytes,omitempty"`
}

// Assertion is one named check carrying its number and its objective.
type Assertion struct {
	Name      string  `json:"name"`
	Passed    bool    `json:"passed"`
	Measured  float64 `json:"measured"`
	Objective float64 `json:"objective"`
	Unit      string  `json:"unit"`
	Detail    string  `json:"detail"`
}

// Proof is the artifact.
type Proof struct {
	SchemaVersion string            `json:"schema_version"`
	Shape         string            `json:"shape"`
	GeneratedAt   string            `json:"generated_at"`
	GoVersion     string            `json:"go_version"`
	BinarySHA256  string            `json:"binary_sha256"`
	ServerEnv     map[string]string `json:"server_env"`
	Prefill       Prefill           `json:"prefill"`
	ReadySeconds  float64           `json:"ready_seconds"`
	Objectives    Objectives        `json:"objectives"`
	Measurements  []*Measurement    `json:"measurements"`
	RSS           RSS               `json:"rss"`
	Memory        MemoryAccounting  `json:"memory_accounting"`
	Assertions    []Assertion       `json:"assertions"`
	Duration      float64           `json:"duration_seconds"`
	Notes         []string          `json:"notes,omitempty"`
}

// NearestRank returns the exact ordered q-quantile of samples (0 < q <= 1):
// the value at rank ceil(q*n), never interpolated. Zero when empty.
func NearestRank(samples []float64, q float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(q * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Summarize computes the four exact percentiles of a sample set.
func Summarize(samples []float64) Percentiles {
	return Percentiles{
		P50: NearestRank(samples, 0.50),
		P90: NearestRank(samples, 0.90),
		P99: NearestRank(samples, 0.99),
		Max: NearestRank(samples, 1),
	}
}

// Evaluate scores every asserted measurement against the objectives. Each
// asserted endpoint yields exactly two assertions, whether or not it was
// measured: a missing or failed measurement fails both with the reason. An
// RSS objective (#283) adds one more, `rss.steady_p95_bytes`.
func Evaluate(p *Proof) []Assertion {
	o := p.Objectives
	var out []Assertion
	for _, m := range p.Measurements {
		if !m.Asserted {
			continue
		}
		out = append(out, coldAssertion(m, o), warmAssertion(m, o))
	}
	if o.RSSSteadyP95Bytes > 0 {
		out = append(out, rssAssertion(&p.RSS, o))
	}
	return out
}

func rssAssertion(r *RSS, o Objectives) Assertion {
	a := Assertion{Name: "rss.steady_p95_bytes", Measured: float64(r.SteadyP95Bytes), Objective: float64(o.RSSSteadyP95Bytes), Unit: "bytes"}
	mib := func(b int64) float64 { return float64(b) / MiB }
	switch {
	case r.SteadySamples == 0 && r.Error != "":
		a.Detail = "no steady-state RSS samples: " + r.Error
	case r.SteadySamples == 0:
		a.Detail = "no steady-state RSS samples: the measurement phase never started"
	case !r.SteadyReached:
		a.Detail = fmt.Sprintf("no steady state reached under the read workload: %s (p95 of the %d samples taken anyway %.1f MiB, peak %.1f MiB)",
			r.SteadyReason, r.SteadySamples, mib(r.SteadyP95Bytes), mib(r.PeakBytes))
	case r.SteadyP95Bytes > o.RSSSteadyP95Bytes:
		a.Detail = fmt.Sprintf("rss steady p95 %.1f MiB > %.0f MiB objective over %d samples (peak %.1f MiB, heap in use peak %.1f MiB)",
			mib(r.SteadyP95Bytes), mib(o.RSSSteadyP95Bytes), r.SteadySamples, mib(r.PeakBytes), mib(r.HeapPeakBytes))
	default:
		a.Passed = true
		a.Detail = fmt.Sprintf("rss steady p95 %.1f MiB <= %.0f MiB objective over %d samples (peak %.1f MiB, heap in use peak %.1f MiB)",
			mib(r.SteadyP95Bytes), mib(o.RSSSteadyP95Bytes), r.SteadySamples, mib(r.PeakBytes), mib(r.HeapPeakBytes))
	}
	if r.Error != "" && r.SteadySamples > 0 {
		a.Detail += "; sampler error: " + r.Error
	}
	return a
}

func coldAssertion(m *Measurement, o Objectives) Assertion {
	a := Assertion{Name: m.Name + ".cold_ms", Measured: m.Cold.MS, Objective: o.ColdMS, Unit: "ms"}
	switch {
	case m.Cold.Error != "":
		a.Detail = "cold call failed: " + m.Cold.Error
	case m.Cold.Status != 200:
		a.Detail = fmt.Sprintf("cold call HTTP %d", m.Cold.Status)
	case m.Cold.MS > o.ColdMS:
		a.Detail = fmt.Sprintf("cold %.1f ms > %.0f ms objective", m.Cold.MS, o.ColdMS)
	default:
		a.Passed = true
		a.Detail = fmt.Sprintf("cold %.1f ms <= %.0f ms objective (%d bytes)", m.Cold.MS, o.ColdMS, m.Cold.Bytes)
	}
	return a
}

func warmAssertion(m *Measurement, o Objectives) Assertion {
	a := Assertion{Name: m.Name + ".warm_p99_ms", Measured: m.Latency.P99, Objective: o.WarmP99MS, Unit: "ms"}
	switch {
	case m.Error != "" && m.Requests == 0:
		a.Detail = "not measured: " + m.Error
	case m.Errors > 0:
		a.Detail = fmt.Sprintf("%d of %d requests failed: %s", m.Errors, m.Requests, m.Error)
	case m.Requests < o.Requests:
		a.Detail = fmt.Sprintf("only %d of %d requests completed within the %.0f s budget (p99 so far %.1f ms)", m.Requests, o.Requests, m.BudgetSeconds, m.Latency.P99)
	case m.Latency.P99 > o.WarmP99MS:
		a.Detail = fmt.Sprintf("warm p99 %.1f ms > %.0f ms objective over %d requests (p50 %.1f, p90 %.1f, max %.1f)", m.Latency.P99, o.WarmP99MS, m.Requests, m.Latency.P50, m.Latency.P90, m.Latency.Max)
	default:
		a.Passed = true
		a.Detail = fmt.Sprintf("warm p99 %.1f ms <= %.0f ms objective over %d requests (p50 %.1f, p90 %.1f, max %.1f)", m.Latency.P99, o.WarmP99MS, m.Requests, m.Latency.P50, m.Latency.P90, m.Latency.Max)
	}
	return a
}

// Failed lists the assertions that did not pass.
func Failed(assertions []Assertion) []Assertion {
	var out []Assertion
	for _, a := range assertions {
		if !a.Passed {
			out = append(out, a)
		}
	}
	return out
}

// SummarizeRSS fills the peaks and the exact ordered p95 over samples taken
// at or after steadyFrom seconds.
func SummarizeRSS(r *RSS, steadyFrom float64) {
	var steady []float64
	r.PeakBytes, r.HeapPeakBytes = 0, 0
	r.SteadyFromSeconds = steadyFrom
	for _, s := range r.Samples {
		if s.Bytes > r.PeakBytes {
			r.PeakBytes = s.Bytes
		}
		if s.HeapBytes > r.HeapPeakBytes {
			r.HeapPeakBytes = s.HeapBytes
		}
		if s.Seconds >= steadyFrom {
			steady = append(steady, float64(s.Bytes))
		}
	}
	r.SteadySamples = len(steady)
	r.SteadyP95Bytes = int64(NearestRank(steady, 0.95))
}

// Write evaluates the proof and writes it to dir/FileName.
func (p *Proof) Write(dir string) (string, error) {
	if p.SchemaVersion == "" {
		p.SchemaVersion = SchemaVersion
	}
	p.Assertions = Evaluate(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
