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

func TestSummarizeRSSAndParseVmRSS(t *testing.T) {
	bytes, err := ParseVmRSS("Name:\totelcontext\nVmPeak:\t 123 kB\nVmRSS:\t   2048 kB\nThreads:\t9\n")
	if err != nil || bytes != 2048*1024 {
		t.Fatalf("ParseVmRSS = %d, %v", bytes, err)
	}
	if _, err := ParseVmRSS("Name:\tx\n"); err == nil {
		t.Error("absent VmRSS must error")
	}
	if _, err := ParseVmRSS("VmRSS:\t 12 MB\n"); err == nil {
		t.Error("unexpected unit must error")
	}
	r := RSS{Samples: []RSSSample{{0, 10}, {5, 50}, {10, 30}, {15, 40}, {20, 35}}}
	SummarizeRSS(&r, 10)
	if r.PeakBytes != 50 || r.SteadySamples != 3 || r.SteadyP95Bytes != 40 {
		t.Errorf("SummarizeRSS = %+v", r)
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
