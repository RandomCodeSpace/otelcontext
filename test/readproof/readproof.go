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
	"errors"
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

// Objectives are the decision's numbers for one shape (#281).
type Objectives struct {
	Requests  int     `json:"requests"`
	WarmP99MS float64 `json:"warm_p99_ms"`
	ColdMS    float64 `json:"cold_ms"`
}

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

// RSSSample is one VmRSS reading, offset from server start.
type RSSSample struct {
	Seconds float64 `json:"t_s"`
	Bytes   int64   `json:"bytes"`
}

// RSS is the server's resident-set series across the run. Recorded, not
// asserted (#292 owns the objective).
type RSS struct {
	Samples        []RSSSample `json:"samples"`
	PeakBytes      int64       `json:"peak_bytes"`
	SteadyP95Bytes int64       `json:"steady_p95_bytes"`
	SteadySamples  int         `json:"steady_samples"`
	Error          string      `json:"error,omitempty"`
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
// measured: a missing or failed measurement fails both with the reason.
func Evaluate(p *Proof) []Assertion {
	o := p.Objectives
	var out []Assertion
	for _, m := range p.Measurements {
		if !m.Asserted {
			continue
		}
		out = append(out, coldAssertion(m, o), warmAssertion(m, o))
	}
	return out
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

// SummarizeRSS fills peak and the p95 over samples taken at or after
// steadyFrom seconds.
func SummarizeRSS(r *RSS, steadyFrom float64) {
	var steady []float64
	r.PeakBytes = 0
	for _, s := range r.Samples {
		if s.Bytes > r.PeakBytes {
			r.PeakBytes = s.Bytes
		}
		if s.Seconds >= steadyFrom {
			steady = append(steady, float64(s.Bytes))
		}
	}
	r.SteadySamples = len(steady)
	r.SteadyP95Bytes = int64(NearestRank(steady, 0.95))
}

// ParseVmRSS extracts the resident-set size in bytes from a
// /proc/<pid>/status payload.
func ParseVmRSS(status string) (int64, error) {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "VmRSS:"))
		if len(fields) < 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("unexpected VmRSS line %q", line)
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("VmRSS %q: %w", fields[0], err)
		}
		return kb * 1024, nil
	}
	return 0, errors.New("VmRSS line absent")
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
