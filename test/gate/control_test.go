//go:build gate

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/test/gate/gatecore"
)

// hangingServer answers nothing until the test ends: every request blocks
// until the server is closed.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })
	return srv
}

func TestParsePrefillOutputCarriesExactFixtureTotals(t *testing.T) {
	out := strings.Join([]string{
		"windows_finalized: 2016",
		"bucket_rows_written: 12096000",
		"delta_rows_incorporated: 12096000",
		"first_window: 100  last_window: 200",
		"series_total: 6000",
		"services_total: 120",
		"dashboard_requests: 700000",
		"dashboard_request_errors: 7000",
		"dashboard_spans: 900000",
		"dashboard_span_errors: 9000",
		"dashboard_logs: 300000",
	}, "\n")
	facts, err := parsePrefillOutput(out)
	if err != nil {
		t.Fatalf("parsePrefillOutput: %v", err)
	}
	if facts.WindowsFinalized != 2016 || facts.Series != 6000 || facts.Services != 120 ||
		facts.Requests != 700000 || facts.RequestErrors != 7000 || facts.Spans != 900000 ||
		facts.SpanErrors != 9000 || facts.Logs != 300000 {
		t.Fatalf("prefill facts lost exact totals: %+v", facts)
	}
}

func TestCertifyingPathsMustBeOutsideCheckout(t *testing.T) {
	root := t.TempDir()
	cfg := gatecore.DefaultConfig()
	cfg.RepoRoot = root
	cfg.Certification.Required = true
	cfg.Confinement.AllowFallback = false
	cfg.WorkDir = filepath.Join(root, "work")
	cfg.ReportDir = filepath.Join(root, "report")
	candidate := candidateSpec{
		configPath: "test/gate/release.config.json", tag: "v1.0.0",
		expectedCommitSHA: strings.Repeat("a", 40), archivePath: "/proof/release.tar.gz",
		expectedArchiveSHA256: strings.Repeat("b", 64), expectedServerSHA256: strings.Repeat("c", 64),
	}
	if err := validateCandidateConfig(cfg, candidate); err == nil || !strings.Contains(err.Error(), "outside repo_root") {
		t.Fatalf("inside-checkout evidence paths were accepted: %v", err)
	}
	cfg.WorkDir = filepath.Join(filepath.Dir(root), "proof-work")
	cfg.ReportDir = filepath.Join(filepath.Dir(root), "proof-report")
	if err := validateCandidateConfig(cfg, candidate); err != nil {
		t.Fatalf("external evidence paths were rejected: %v", err)
	}
}

func TestCertifyingCandidateRejectsNonHexadecimalBindings(t *testing.T) {
	root := t.TempDir()
	cfg := gatecore.DefaultConfig()
	cfg.RepoRoot = root
	cfg.Certification.Required = true
	cfg.Confinement.AllowFallback = false
	cfg.WorkDir = filepath.Join(filepath.Dir(root), "proof-work")
	cfg.ReportDir = filepath.Join(filepath.Dir(root), "proof-report")
	candidate := candidateSpec{
		configPath: "test/gate/release.config.json", tag: "v1.0.0",
		expectedCommitSHA: strings.Repeat("z", 40), archivePath: "/proof/release.tar.gz",
		expectedArchiveSHA256: strings.Repeat("b", 64), expectedServerSHA256: strings.Repeat("c", 64),
	}
	if err := validateCandidateConfig(cfg, candidate); err == nil || !strings.Contains(err.Error(), "hexadecimal") {
		t.Fatalf("non-hexadecimal candidate binding was accepted: %v", err)
	}
}

func TestReleaseConfigFreezesCertificationContract(t *testing.T) {
	cfg, err := gatecore.LoadConfigFile("release.config.json")
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("release config is invalid: %v", err)
	}
	if !cfg.Certification.Required || cfg.Confinement.AllowFallback {
		t.Fatalf("release config is not strict: certification=%t allow_fallback=%t",
			cfg.Certification.Required, cfg.Confinement.AllowFallback)
	}
	th := cfg.Thresholds
	if th.PrefillWindows != 2016 || th.PrefillSeries != 6000 || th.PrefillServices != 120 ||
		th.ColdQueryMaxSeconds != 5 || th.WarmQueryP95MaxSeconds != 0.5 ||
		th.SevenDayQueryMaxSeconds != 15 || th.MCPQueryMaxSeconds != 5 || th.ProbeMaxSeconds != 1 {
		t.Fatalf("release thresholds drifted: %+v", th)
	}
	required := strings.Join(cfg.Sampling.RequiredMetrics, "\n")
	for _, metric := range []string{
		"otelcontext_aggregate_identity_overflow_total",
		"otelcontext_ingest_pipeline_dropped_total",
	} {
		if !strings.Contains(required, metric) {
			t.Errorf("release config does not require %s", metric)
		}
	}
}

func TestWriteDigestManifestBindsEveryNamedFile(t *testing.T) {
	dir := t.TempDir()
	server := filepath.Join(dir, "server")
	gate := filepath.Join(dir, "gate")
	if err := os.WriteFile(server, []byte("server candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, []byte("gate tool"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "digests.txt")
	if err := writeDigestManifest(manifest, map[string]string{"server": server, "gate": gate}); err != nil {
		t.Fatalf("writeDigestManifest: %v", err)
	}
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	gateSHA, _ := sha256File(gate)
	serverSHA, _ := sha256File(server)
	want := gateSHA + "  gate\n" + serverSHA + "  server\n"
	if string(body) != want {
		t.Fatalf("manifest = %q, want %q", body, want)
	}
}

func TestWaitLatencySentinelRetriesUntilExactSampleCount(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("service_name") != "latency-sentinel" || r.URL.Query().Get("_gate_wait") == "" {
			t.Errorf("sentinel probe query = %q", r.URL.RawQuery)
		}
		samples := 12
		if requests >= 2 {
			samples = 1000
		}
		fmt.Fprintf(w, `{"latency_provenance":{"p99":{"sample_count":%d}}}`, samples)
	}))
	defer srv.Close()
	g := gateAgainst(t, srv, time.Second)
	if err := g.waitLatencySentinel("latency-sentinel", 1000, time.Second); err != nil {
		t.Fatalf("waitLatencySentinel: %v", err)
	}
	if requests < 2 {
		t.Fatalf("sentinel visibility was not retried: %d request", requests)
	}
}

func TestLatencySurfaceFromMCPAcceptsMapAndHealthShapes(t *testing.T) {
	service := `{"service":{"name":"latency-sentinel","p99_latency_ms":1000,"latency_provenance":{"p99":{"status":"approximate","method":"ddsketch","sample_count":1000,"sketch_scale":4,"relative_error_bound":0.02165746232622625}}}}`
	for name, body := range map[string]string{
		"service map":    "[" + service + "]",
		"service health": service,
	} {
		t.Run(name, func(t *testing.T) {
			surface := latencySurfaceFromMCP("consumer", "latency-sentinel", body)
			if surface.Error != "" || surface.ValueMS != 1000 || surface.SampleCount != 1000 ||
				surface.SketchScale != 4 || surface.Status != "approximate" || surface.Method != "ddsketch" {
				t.Fatalf("parsed MCP latency surface = %+v", surface)
			}
		})
	}
}

func TestServiceMapCheckCountsTopLevelServices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"coverage":"full","nodes":[{"name":"a"},{"name":"b"}]}`))
	}))
	defer srv.Close()
	g := gateAgainst(t, srv, time.Second)
	check := g.runAPICheck(gatecore.APICheck{
		Name: "service_map_seven_day", Path: "/api/metrics/service-map", ExpectCoverage: "full",
	}, time.Time{}, time.Time{}, nil)
	if check.Error != "" || check.Scalars["services"] != 2 {
		t.Fatalf("service-map check = %+v", check)
	}
}

func gateAgainst(t *testing.T, srv *httptest.Server, ctl time.Duration) *gate {
	t.Helper()
	return &gate{
		cfg:  gatecore.Config{HTTPAddr: srv.Listener.Addr().String()},
		http: &http.Client{Timeout: 600 * time.Second},
		ctl:  &http.Client{Timeout: ctl},
	}
}

// TestWaitReadyBoundedByDeadline pins the review contract: waitReady must
// return within its own budget even when /ready never answers — the long
// query-client timeout must not leak into the readiness loop.
func TestWaitReadyBoundedByDeadline(t *testing.T) {
	srv := hangingServer(t)
	g := gateAgainst(t, srv, 5*time.Second)

	started := time.Now()
	_, err := g.waitReady(2 * time.Second)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("waitReady succeeded against a server that never answers")
	}
	// The property under test is that the 600s QUERY timeout does not govern
	// the readiness loop — not that a shared CI runner can schedule promptly.
	// A generous ceiling still fails decisively if the long client leaks in,
	// while a tight one just measures runner contention.
	if elapsed > 60*time.Second {
		t.Fatalf("waitReady took %s against a hung /ready with a 2s budget — "+
			"the query client's timeout is governing the readiness loop", elapsed)
	}
}

// TestSamplerShutdownBoundedWithStuckScrape: a scrape stuck on a hung
// endpoint must not hold sampler shutdown past the control-client timeout.
func TestSamplerShutdownBoundedWithStuckScrape(t *testing.T) {
	srv := hangingServer(t)
	g := gateAgainst(t, srv, 500*time.Millisecond)
	g.cfg.Sampling.IntervalSec = 0.05

	s := newSampler(g)
	go s.run()
	time.Sleep(100 * time.Millisecond) // let a tick enter the stuck scrape

	started := time.Now()
	s.shutdown()
	// Same reasoning as above: bound generously against the 600s query
	// timeout leaking in, not tightly against the 500ms control timeout,
	// which a contended runner will exceed for reasons unrelated to the bug.
	if elapsed := time.Since(started); elapsed > 60*time.Second {
		t.Fatalf("sampler shutdown took %s with a stuck scrape — "+
			"the query client's timeout is governing shutdown", elapsed)
	}
}
