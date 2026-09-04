package readproof

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNearestRankIsExactOrdered(t *testing.T) {
	samples := []float64{5, 1, 4, 2, 3}
	cases := []struct {
		q    float64
		want float64
	}{{0.50, 3}, {0.90, 5}, {0.99, 5}, {1, 5}, {0.20, 1}, {0.21, 2}}
	for _, c := range cases {
		if got := NearestRank(samples, c.q); got != c.want {
			t.Errorf("q=%v: got %v want %v", c.q, got, c.want)
		}
	}
	if NearestRank(nil, 0.5) != 0 {
		t.Error("empty input must yield 0")
	}
	if samples[0] != 5 {
		t.Error("input must not be sorted in place")
	}
	// 200 samples: p99 is rank 198, never an interpolation.
	two := make([]float64, 200)
	for i := range two {
		two[i] = float64(i + 1)
	}
	p := Summarize(two)
	if p.P50 != 100 || p.P90 != 180 || p.P99 != 198 || p.Max != 200 {
		t.Errorf("percentiles over 1..200 = %+v", p)
	}
}

func TestEvaluateCarriesNumbersAndReasons(t *testing.T) {
	o := Objectives{Requests: 200, WarmP99MS: 300, ColdMS: 1000}
	ok := &Measurement{Name: "ok", Asserted: true, Requests: 200,
		Cold: Call{MS: 120, Status: 200, Bytes: 10}, Latency: Percentiles{P99: 40}}
	slow := &Measurement{Name: "slow", Asserted: true, Requests: 200,
		Cold: Call{MS: 1500, Status: 200}, Latency: Percentiles{P99: 450, P50: 100, P90: 300, Max: 900}}
	short := &Measurement{Name: "short", Asserted: true, Requests: 37, BudgetSeconds: 60,
		Cold: Call{MS: 10, Status: 200}, Latency: Percentiles{P99: 20}}
	missing := &Measurement{Name: "missing", Asserted: true, Error: "not measured: prefill failed",
		Cold: Call{Error: "not measured: prefill failed"}}
	failing := &Measurement{Name: "failing", Asserted: true, Requests: 200, Errors: 3, Error: "HTTP 429",
		Cold: Call{MS: 10, Status: 404}, Latency: Percentiles{P99: 20}}
	unasserted := &Measurement{Name: "miss", Asserted: false}

	p := &Proof{Objectives: o, Measurements: []*Measurement{ok, slow, short, missing, failing, unasserted}}
	got := Evaluate(p)
	if len(got) != 10 {
		t.Fatalf("expected 2 assertions per asserted endpoint, got %d", len(got))
	}
	byName := map[string]Assertion{}
	for _, a := range got {
		byName[a.Name] = a
		if a.Unit != "ms" || a.Detail == "" {
			t.Errorf("%s: unit=%q detail=%q", a.Name, a.Unit, a.Detail)
		}
	}
	check := func(name string, passed bool, measured, objective float64, detail string) {
		t.Helper()
		a, present := byName[name]
		if !present {
			t.Fatalf("%s absent", name)
		}
		if a.Passed != passed || a.Measured != measured || a.Objective != objective || !strings.Contains(a.Detail, detail) {
			t.Errorf("%s = %+v", name, a)
		}
	}
	check("ok.cold_ms", true, 120, 1000, "<= 1000 ms")
	check("ok.warm_p99_ms", true, 40, 300, "over 200 requests")
	check("slow.cold_ms", false, 1500, 1000, "1500.0 ms > 1000 ms")
	check("slow.warm_p99_ms", false, 450, 300, "450.0 ms > 300 ms")
	check("short.warm_p99_ms", false, 20, 300, "only 37 of 200")
	check("missing.cold_ms", false, 0, 1000, "prefill failed")
	check("missing.warm_p99_ms", false, 0, 300, "not measured")
	check("failing.cold_ms", false, 10, 1000, "HTTP 404")
	check("failing.warm_p99_ms", false, 20, 300, "3 of 200 requests failed")
	if len(Failed(got)) != 7 {
		t.Errorf("failed = %d", len(Failed(got)))
	}
}

func TestSummarizeRSSAndAssertion(t *testing.T) {
	r := RSS{Samples: []RSSSample{{0, 10, 4}, {5, 50, 30}, {10, 30, 20}, {15, 40, 25}, {20, 35, 22}}}
	SummarizeRSS(&r, 10)
	if r.PeakBytes != 50 || r.HeapPeakBytes != 30 || r.SteadySamples != 3 || r.SteadyP95Bytes != 40 || r.SteadyFromSeconds != 10 {
		t.Errorf("SummarizeRSS = %+v", r)
	}

	o := Objectives{Requests: 200, WarmP99MS: 300, ColdMS: 1000, RSSSteadyP95Bytes: 512 * MiB}
	pass := &Proof{Objectives: o, RSS: RSS{SteadyReached: true, SteadySamples: 8, SteadyP95Bytes: 400 * MiB, PeakBytes: 700 * MiB, HeapPeakBytes: 300 * MiB}}
	fail := &Proof{Objectives: o, RSS: RSS{SteadyReached: true, SteadySamples: 8, SteadyP95Bytes: 925 * MiB, PeakBytes: 925 * MiB}}
	climbing := &Proof{Objectives: o, RSS: RSS{SteadySamples: 8, SteadyP95Bytes: 480 * MiB, PeakBytes: 480 * MiB, SteadyReason: "rss still moving, 461.0 -> 574.0 MiB over the last 15 s"}}
	none := &Proof{Objectives: o, RSS: RSS{Error: "connection refused"}}
	unasserted := &Proof{Objectives: Objectives{Requests: 200, WarmP99MS: 300, ColdMS: 1000}, RSS: RSS{SteadySamples: 1, SteadyP95Bytes: 5 * MiB}}
	check := func(p *Proof, passed bool, detail string) {
		t.Helper()
		got := Evaluate(p)
		if len(got) != 1 {
			t.Fatalf("assertions = %+v", got)
		}
		a := got[0]
		if a.Name != "rss.steady_p95_bytes" || a.Unit != "bytes" || a.Passed != passed ||
			a.Measured != float64(p.RSS.SteadyP95Bytes) || a.Objective != float64(512*MiB) || !strings.Contains(a.Detail, detail) {
			t.Errorf("assertion = %+v", a)
		}
	}
	check(pass, true, "400.0 MiB <= 512 MiB objective over 8 samples (peak 700.0 MiB, heap in use peak 300.0 MiB)")
	check(fail, false, "925.0 MiB > 512 MiB objective")
	check(climbing, false, "no steady state reached under the read workload: rss still moving, 461.0 -> 574.0 MiB")
	check(none, false, "no steady-state RSS samples: connection refused")
	if got := Evaluate(unasserted); len(got) != 0 {
		t.Errorf("no RSS objective must add no assertion, got %+v", got)
	}
}

func TestParseSmaps(t *testing.T) {
	smaps := `c000000000-c004000000 rw-p 00000000 00:00 0                          [anon: Go: heap]
Size:              65536 kB
Rss:               40960 kB
7f0000000000-7f0000100000 rw-p 00000000 00:00 0                          [anon: Go: gc bits]
Rss:                 512 kB
7f0001000000-7f0011000000 rw-p 00000000 00:00 0
Rss:              131072 kB
7f0020000000-7f0030000000 r--s 00000000 00:2a 12345                      /dev/shm/x/otelcontext.db
Rss:               20480 kB
55d000000000-55d003000000 r-xp 00000000 08:01 999                        /dev/shm/rp/otelcontext
Rss:               27648 kB
7ffd00000000-7ffd00021000 rw-p 00000000 00:00 0                          [stack]
Rss:                 132 kB
7ffd00100000-7ffd00102000 r-xp 00000000 00:00 0                          [vdso]
Rss:                   8 kB
`
	m := ParseSmaps(smaps, "/dev/shm/rp/otelcontext")
	want := Mappings{TotalBytes: (40960 + 512 + 131072 + 20480 + 27648 + 132 + 8) * 1024,
		GoHeapBytes: 40960 * 1024, GoRuntimeBytes: (512 + 132 + 8) * 1024, OtherAnonBytes: 131072 * 1024,
		FileBytes: 20480 * 1024, BinaryBytes: 27648 * 1024}
	if m != want {
		t.Errorf("ParseSmaps = %+v, want %+v", m, want)
	}
	if empty := ParseSmaps("", "x"); empty.Error == "" {
		t.Error("empty smaps must carry an error")
	}
}

func TestParseMetricsAndAccount(t *testing.T) {
	exposition := `# HELP otelcontext_process_resident_memory_bytes Resident set size.
# TYPE otelcontext_process_resident_memory_bytes gauge
otelcontext_process_resident_memory_bytes 9.25519872e+08
otelcontext_go_heap_inuse_bytes 3.1e+08
otelcontext_resource_registry_entries{kind="host",tenant="default"} 5
otelcontext_resource_registry_entries{kind="pair",tenant="default"} 7
otelcontext_resource_registry_entries{kind="pair",tenant="acme, inc"} 2
otelcontext_graphrag_store_entities{entity="latency_sketches"} 5
otelcontext_graphrag_store_entities{entity="spans"} 184000
otelcontext_graphrag_store_edges{store="trace"} 368000
otelcontext_read_cache_entries{cache="api_ttl"} 3
otelcontext_read_cache_entries{cache="mcp_result"} 4 1700000000000
go_goroutines 42
`
	samples, err := ParseMetrics(exposition)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 11 {
		t.Fatalf("samples = %d", len(samples))
	}
	if v, err := Gauge(samples, RSSMetric); err != nil || v != 925519872 {
		t.Errorf("rss = %v, %v", v, err)
	}
	if _, err := Gauge(samples, "absent"); err == nil {
		t.Error("absent gauge must error")
	}
	if _, err := ParseMetrics("broken{x=\"1\" 2\n"); err == nil {
		t.Error("unterminated labels must error")
	}
	if _, err := ParseMetrics("novalue\n"); err == nil {
		t.Error("missing value must error")
	}

	a := Account(samples, 2112)
	if a.Error != "" || a.RSSBytes != 925519872 || a.HeapInuseBytes != 310000000 ||
		a.RegistryPairEntries != 9 || a.RegistryHostEntries != 5 ||
		a.GraphRAGEntities["spans"] != 184000 || a.GraphRAGEdges["trace"] != 368000 ||
		a.LatencySketchBytes != 5*2112 || a.ReadCacheEntries["api_ttl"] != 3 || a.ReadCacheEntries["mcp_result"] != 4 {
		t.Errorf("Account = %+v", a)
	}
	empty := Account(nil, 2112)
	for _, want := range []string{RSSMetric, HeapMetric, GraphRAGEntitiesMetric, GraphRAGEdgesMetric, ReadCacheEntriesMetric} {
		if !strings.Contains(empty.Error, want+" absent") {
			t.Errorf("empty accounting must name %s: %q", want, empty.Error)
		}
	}
}

func TestWriteRendersArtifact(t *testing.T) {
	dir := t.TempDir()
	p := &Proof{Shape: "legacy", Objectives: Objectives{Requests: 200, WarmP99MS: 300, ColdMS: 1000},
		Measurements: []*Measurement{{Name: "rest_dashboard", Asserted: true, Error: "not measured: server never became ready", Cold: Call{Error: "not measured: server never became ready"}}}}
	path, err := p.Write(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != FileName {
		t.Errorf("path = %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		SchemaVersion string `json:"schema_version"`
		Shape         string `json:"shape"`
		Assertions    []struct {
			Name      string   `json:"name"`
			Passed    bool     `json:"passed"`
			Measured  *float64 `json:"measured"`
			Objective *float64 `json:"objective"`
			Detail    string   `json:"detail"`
		} `json:"assertions"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != SchemaVersion || decoded.Shape != "legacy" || len(decoded.Assertions) != 2 {
		t.Fatalf("decoded = %+v", decoded)
	}
	for _, a := range decoded.Assertions {
		if a.Passed || a.Measured == nil || a.Objective == nil || !strings.Contains(a.Detail, "never became ready") {
			t.Errorf("assertion %+v must fail with the reason and carry both numbers", a)
		}
	}
}
