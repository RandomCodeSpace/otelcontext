//go:build readproof

package readproof

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/api"
)

const (
	binaryEnv   = "OTELCONTEXT_READPROOF_BINARY"
	proofDirEnv = "OTELCONTEXT_PROOF_DIR"

	warmupRequests = 10
	// endpointBudget bounds the timed phase of one endpoint so a slow surface
	// cannot run the job past its CI budget; a truncated phase fails its
	// assertion with the count it reached.
	endpointBudget = 60 * time.Second
	callTimeout    = 30 * time.Second
	rssInterval    = 5 * time.Second
	mcpPath        = "/mcp"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type appProcess struct {
	binary   string
	dir      string
	mode     string
	httpPort int
	grpcPort int
	env      map[string]string
	log      *lockedBuffer
	cmd      *exec.Cmd
	done     chan error
	started  time.Time
	client   *http.Client
}

func requireBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv(binaryEnv)
	if binary == "" {
		t.Fatalf("%s is required: the proof measures the exact binary", binaryEnv)
	}
	return binary
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stateDir creates the run's data directory under TMPDIR when set.
func stateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "readproof-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newAppProcess(t *testing.T, binary, dir, mode string) *appProcess {
	t.Helper()
	a := &appProcess{
		binary:   binary,
		dir:      dir,
		mode:     mode,
		httpPort: freePort(t),
		grpcPort: freePort(t),
		log:      &lockedBuffer{},
		client:   &http.Client{Timeout: callTimeout, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4}},
	}
	a.env = map[string]string{
		"AGGREGATE_ALLOW_REBUILD": "false",
		"AGGREGATE_DB_PATH":       filepath.Join(dir, "aggregate.db"),
		"AGGREGATE_MODE":          mode,
		"API_KEY":                 "",
		"API_TENANT_KEYS_FILE":    "",
		// The proof issues 200 back-to-back requests from one IP; the
		// per-IP limiter (100 rps, burst 100) would turn that into 429s.
		"API_RATE_LIMIT_RPS":  "0",
		"APP_ENV":             "development",
		"DATA_DISK_BUDGET_MB": "1000000",
		"DATA_DISK_PATH":      dir,
		"DB_AUTOMIGRATE":      "true",
		"DB_DRIVER":           "sqlite",
		"DB_DSN":              filepath.Join(dir, "otelcontext.db"),
		"DLQ_PATH":            filepath.Join(dir, "dlq"),
		"DLQ_REPLAY_INTERVAL": "1h",
		"GRPC_PORT":           strconv.Itoa(a.grpcPort),
		"HOT_RETENTION_DAYS":  "7",
		"HTTP_PORT":           strconv.Itoa(a.httpPort),
		"INGEST_MIN_SEVERITY": "INFO",
		"LOG_LEVEL":           "INFO",
		"MCP_PATH":            mcpPath,
		"PPROF_ADDR":          "",
		"SAMPLING_RATE":       "1.0",
		"STORE_MIN_SEVERITY":  "INFO",
		"TLS_AUTO_SELFSIGNED": "false",
		"TLS_CERT_FILE":       "",
		"TLS_KEY_FILE":        "",
	}
	return a
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func (a *appProcess) environment() []string {
	env := make([]string, 0, len(os.Environ())+len(a.env))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := a.env[key]; !replaced {
			env = append(env, item)
		}
	}
	keys := make([]string, 0, len(a.env))
	for key := range a.env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+a.env[key])
	}
	return env
}

func (a *appProcess) start() error {
	if a.cmd != nil {
		return errors.New("application already running")
	}
	cmd := exec.Command(a.binary)
	cmd.Dir = a.dir
	cmd.Env = a.environment()
	cmd.Stdout = a.log
	cmd.Stderr = a.log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	a.cmd, a.done, a.started = cmd, done, time.Now()
	return nil
}

func (a *appProcess) stop() {
	cmd, done := a.cmd, a.done
	a.cmd, a.done = nil, nil
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(35 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func (a *appProcess) waitReady(ctx context.Context) error {
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if a.done != nil {
			select {
			case err := <-a.done:
				a.cmd, a.done = nil, nil
				return fmt.Errorf("server exited before ready: %v\n%s", err, tail(a.log.String(), 4000))
			default:
			}
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL()+"/ready", nil)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("readiness: %w\n%s", ctx.Err(), tail(a.log.String(), 4000))
		case <-ticker.C:
		}
	}
}

func (a *appProcess) baseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(a.httpPort)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// rssSampler reads VmRSS of the server every rssInterval plus on demand.
type rssSampler struct {
	app  *appProcess
	mu   sync.Mutex
	rss  RSS
	stop chan struct{}
	wg   sync.WaitGroup
}

func startRSSSampler(app *appProcess) *rssSampler {
	s := &rssSampler{app: app, stop: make(chan struct{})}
	s.sample()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(rssInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				s.sample()
			}
		}
	}()
	return s
}

func (s *rssSampler) sample() {
	cmd := s.app.cmd
	if cmd == nil || cmd.Process == nil {
		return
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/status")
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.rss.Error = err.Error()
		return
	}
	bytes, err := ParseVmRSS(string(raw))
	if err != nil {
		s.rss.Error = err.Error()
		return
	}
	s.rss.Samples = append(s.rss.Samples, RSSSample{Seconds: round3(time.Since(s.app.started).Seconds()), Bytes: bytes})
}

func (s *rssSampler) finish(steadyFrom float64) RSS {
	close(s.stop)
	s.wg.Wait()
	s.sample()
	s.mu.Lock()
	defer s.mu.Unlock()
	SummarizeRSS(&s.rss, steadyFrom)
	if s.rss.Samples == nil {
		s.rss.Samples = []RSSSample{}
		if s.rss.Error == "" {
			s.rss.Error = "no samples taken"
		}
	}
	return s.rss
}

func round3(v float64) float64 { return float64(int64(v*1000+0.5)) / 1000 }

// request is one call; the returned Call carries whatever the transport
// produced, so a failure is recorded rather than aborting the phase.
type request func(ctx context.Context, nonce int) timedCall

// endpoint pairs a measurement with the request that produces its evidence.
type endpoint struct {
	m  *Measurement
	fn request
}

func (a *appProcess) restRequest(path string) request {
	return func(ctx context.Context, _ int) timedCall {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL()+path, nil)
		if err != nil {
			return timedCall{Call: Call{Error: err.Error()}}
		}
		return a.do(req)
	}
}

// mcpRequest builds a JSON-RPC tools/call. When nonce is true an ignored
// `_nonce` argument is added per call; the result cache keys on every
// argument, so this measures the cache-miss path without changing the work.
func (a *appProcess) mcpRequest(tool string, args map[string]any, nonce bool) request {
	return func(ctx context.Context, n int) timedCall {
		callArgs := map[string]any{}
		for k, v := range args {
			callArgs[k] = v
		}
		if nonce {
			callArgs["_nonce"] = n
		}
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      n,
			"method":  "tools/call",
			"params":  map[string]any{"name": tool, "arguments": callArgs},
		})
		if err != nil {
			return timedCall{Call: Call{Error: err.Error()}}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL()+mcpPath, bytes.NewReader(payload))
		if err != nil {
			return timedCall{Call: Call{Error: err.Error()}}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		call := a.do(req)
		if call.Error == "" && call.Status == http.StatusOK && call.rpcError != "" {
			call.Error = call.rpcError
		}
		return call
	}
}

type timedCall struct {
	Call
	cacheHit       bool
	rpcError       string
	requestedStart string
	effectiveStart string
}

func (a *appProcess) do(req *http.Request) timedCall {
	started := time.Now()
	resp, err := a.client.Do(req)
	if err != nil {
		return timedCall{Call: Call{MS: msSince(started), Error: err.Error()}}
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	call := timedCall{Call: Call{
		MS:       msSince(started),
		Status:   resp.StatusCode,
		Bytes:    len(body),
		Coverage: resp.Header.Get(aggregate.CoverageHeader),
	},
		cacheHit:       resp.Header.Get("X-Cache") == "HIT",
		requestedStart: resp.Header.Get(api.RequestedStartHeader),
		effectiveStart: resp.Header.Get(api.EffectiveStartHeader),
	}
	if readErr != nil {
		call.Error = readErr.Error()
		return call
	}
	if resp.StatusCode != http.StatusOK {
		call.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, tail(strings.TrimSpace(string(body)), 200))
		return call
	}
	if req.Method == http.MethodGet && bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		var view struct {
			Coverage string `json:"coverage"`
		}
		if err := json.Unmarshal(body, &view); err == nil {
			call.BodyCoverage = view.Coverage
		}
	}
	if req.Method == http.MethodPost {
		var envelope struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			call.rpcError = "decode JSON-RPC envelope: " + err.Error()
		} else if envelope.Error != nil {
			call.rpcError = fmt.Sprintf("JSON-RPC error %d: %s", envelope.Error.Code, envelope.Error.Message)
		} else if envelope.Result.IsError {
			call.rpcError = "tool reported isError"
		}
	}
	return call
}

func msSince(started time.Time) float64 {
	return round3(float64(time.Since(started).Microseconds()) / 1000)
}

// measure runs the per-endpoint protocol: one cold call recorded on its own,
// warmupRequests untimed calls, then up to objectives.Requests timed calls
// inside endpointBudget.
func measure(t *testing.T, m *Measurement, fn request, o Objectives) {
	t.Helper()
	m.Warmup = warmupRequests
	m.BudgetSeconds = endpointBudget.Seconds()
	m.SamplesMS = []float64{}

	call := func(n int) timedCall {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		return fn(ctx, n)
	}

	nonce := 1
	cold := call(nonce)
	m.Cold = cold.Call
	if cold.Error != "" {
		t.Logf("%s: cold call failed: %s", m.Name, cold.Error)
	}

	for i := 0; i < warmupRequests; i++ {
		nonce++
		call(nonce)
	}

	started := time.Now()
	deadline := started.Add(endpointBudget)
	for m.Requests < o.Requests && time.Now().Before(deadline) {
		nonce++
		c := call(nonce)
		m.Requests++
		m.SamplesMS = append(m.SamplesMS, c.MS)
		m.Status = c.Status
		m.Coverage = c.Coverage
		m.BodyCoverage = c.BodyCoverage
		m.RequestedStart = c.requestedStart
		m.EffectiveStart = c.effectiveStart
		m.ResponseBytes = c.Bytes
		if c.Bytes > m.MaxBytes {
			m.MaxBytes = c.Bytes
		}
		if c.cacheHit {
			m.CacheHits++
		}
		if c.Error != "" {
			m.Errors++
			if m.Error == "" {
				m.Error = c.Error
			}
		}
	}
	m.Seconds = round3(time.Since(started).Seconds())
	m.Latency = Summarize(m.SamplesMS)
	if m.Requests < o.Requests && m.Error == "" {
		m.Error = fmt.Sprintf("budget of %.0f s exhausted after %d requests", endpointBudget.Seconds(), m.Requests)
	}
	t.Logf("%s: cold %.1f ms (HTTP %d, %d bytes, coverage header %q body %q); warm n=%d p50 %.1f p90 %.1f p99 %.1f max %.1f ms; cache hits %d; errors %d; clamp %q->%q",
		m.Name, m.Cold.MS, m.Cold.Status, m.Cold.Bytes, m.Cold.Coverage, m.Cold.BodyCoverage, m.Requests,
		m.Latency.P50, m.Latency.P90, m.Latency.P99, m.Latency.Max, m.CacheHits, m.Errors, m.RequestedStart, m.EffectiveStart)
}

// markUnmeasured stamps every endpoint that has no evidence yet with the
// reason, so a setup failure still yields a complete assertion table.
func markUnmeasured(p *Proof, reason string) {
	for _, m := range p.Measurements {
		if m.Requests == 0 && m.Cold.Status == 0 && m.Cold.Error == "" {
			m.Error = "not measured: " + reason
			m.Cold.Error = m.Error
		}
	}
}

// endpoints lists the surfaces both shapes share; the legacy shape appends
// the GraphRAG-backed system graph. rcaService is the root_cause_analysis
// argument, a service the prefill is known to contain. The three REST
// surfaces are measured twice: at the UI's default range and, as
// `<name>_full_range`, with explicit start/end spanning the whole seeded
// horizon — the SUM-over-every-window path #219 optimised.
func endpoints(app *appProcess, rcaService string, fullStart, fullEnd time.Time) []endpoint {
	type ep = endpoint
	fullRange := url.Values{
		"start": {fullStart.UTC().Format(time.RFC3339)},
		"end":   {fullEnd.UTC().Format(time.RFC3339)},
	}.Encode()
	rest := func(name, path string) []ep {
		return []ep{
			{&Measurement{Name: name, Kind: "rest", Path: path, Asserted: true}, app.restRequest(path)},
			{&Measurement{Name: name + "_full_range", Kind: "rest", Path: path, Query: fullRange, Asserted: true}, app.restRequest(path + "?" + fullRange)},
		}
	}
	tools := []struct {
		name string
		args map[string]any
	}{
		{"get_service_map", map[string]any{}},
		{"get_anomaly_timeline", map[string]any{}},
		{"root_cause_analysis", map[string]any{"service": rcaService}},
	}
	var out []ep
	out = append(out, rest("rest_dashboard", "/api/metrics/dashboard")...)
	out = append(out, rest("rest_traffic", "/api/metrics/traffic")...)
	out = append(out, rest("rest_service_map", "/api/metrics/service-map")...)
	for _, tool := range tools {
		args, _ := json.Marshal(tool.args)
		out = append(out,
			ep{&Measurement{Name: "mcp_" + tool.name, Kind: "mcp", Path: mcpPath, Tool: tool.name, Arguments: string(args), Cache: "client", Asserted: true}, app.mcpRequest(tool.name, tool.args, false)},
			ep{&Measurement{Name: "mcp_" + tool.name + "_miss", Kind: "mcp", Path: mcpPath, Tool: tool.name, Arguments: string(args), Cache: "miss", Asserted: false}, app.mcpRequest(tool.name, tool.args, true)},
		)
	}
	return out
}

func newProof(t *testing.T, shape, binary string, o Objectives) *Proof {
	t.Helper()
	return &Proof{
		SchemaVersion: SchemaVersion,
		Shape:         shape,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		GoVersion:     runtime.Version(),
		BinarySHA256:  sha256File(t, binary),
		Objectives:    o,
		Measurements:  []*Measurement{},
		Notes: []string{
			"warm percentiles are exact ordered (nearest-rank) over the timed requests; the cold call is the first request ever issued to the endpoint",
			"rest_*: the default range (no query parameters), exactly as the embedded UI polls; rest_*_full_range: explicit start/end spanning the whole seeded horizon; server-side dashboard (10s) and service-map (30s) caches are in play as they are for any client",
			"coverage is the OtelContext-Data-Coverage header; body_coverage is the `coverage` field of object-shaped bodies (dashboard, service-map); requested_start/effective_start appear only when the aggregate range clamp shortened the request",
			"mcp_*: arguments a real client sends, so the 5s MCP result cache serves most warm calls; mcp_*_miss: an ignored `_nonce` argument defeats the cache key and is recorded, not asserted",
			"rss is recorded for #292 and not asserted; steady_p95 covers samples taken from the start of the measurement phase",
		},
	}
}

func writeProof(t *testing.T, p *Proof, started time.Time) {
	t.Helper()
	p.Duration = round3(time.Since(started).Seconds())
	dir := os.Getenv(proofDirEnv)
	if dir == "" {
		dir = t.TempDir()
	}
	path, err := p.Write(dir)
	if err != nil {
		t.Errorf("write proof: %v", err)
		return
	}
	t.Logf("read-latency proof: %s", path)
	for _, a := range p.Assertions {
		if !a.Passed {
			t.Errorf("%s: %s", a.Name, a.Detail)
		}
	}
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
