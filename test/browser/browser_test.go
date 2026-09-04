//go:build browser && !windows

package browser_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/cdproto/fetch"
	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

//go:embed protected_features.json
var protectedFeaturesJSON []byte

const (
	readyTimeout     = 20 * time.Second
	reconnectTimeout = 30 * time.Second
	testTimeout      = 110 * time.Second
	maxLogBytes      = 256 << 10
)

type protectedInventory struct {
	SchemaVersion int `json:"schema_version"`
	Features      []struct {
		ID       string `json:"id"`
		Proof    string `json:"proof"`
		Phase    string `json:"phase"`
		Contract string `json:"contract"`
	} `json:"features"`
}

type tailBuffer struct {
	mu   sync.Mutex
	max  int
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.data = append(b.data, p...)
	if len(b.data) > b.max {
		b.data = append([]byte(nil), b.data[len(b.data)-b.max:]...)
	}
	return n, nil
}

func (b *tailBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...)
}

type appProcess struct {
	mu       sync.Mutex
	binary   string
	dir      string
	httpPort int
	grpcPort int
	pprof    int
	log      *tailBuffer
	cmd      *exec.Cmd
	done     chan error
}

func newAppProcess(t *testing.T, binary string) *appProcess {
	t.Helper()
	dir := t.TempDir()
	return &appProcess{
		binary:   binary,
		dir:      dir,
		httpPort: freePort(t),
		grpcPort: freePort(t),
		pprof:    freePort(t),
		log:      &tailBuffer{max: maxLogBytes},
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release port %d: %v", port, err)
	}
	return port
}

func (a *appProcess) baseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(a.httpPort)
}

func (a *appProcess) environment() []string {
	overrides := map[string]string{
		"AGGREGATE_MODE":                  "legacy",
		"API_KEY":                         "",
		"API_TENANT_KEYS_FILE":            "",
		"APP_ENV":                         "development",
		"AUTH_TRUST_EXTERNAL":             "false",
		"DB_DRIVER":                       "sqlite",
		"DB_DSN":                          filepath.Join(a.dir, "otelcontext.db"),
		"DATA_DISK_BUDGET_MB":             "1000000",
		"DATA_DISK_PATH":                  a.dir,
		"DLQ_PATH":                        filepath.Join(a.dir, "dlq"),
		"GRPC_PORT":                       strconv.Itoa(a.grpcPort),
		"HTTP_PORT":                       strconv.Itoa(a.httpPort),
		"INGEST_ASYNC_ENABLED":            "false",
		"LOG_LEVEL":                       "INFO",
		"MCP_ENABLED":                     "true",
		"MCP_PATH":                        "/mcp",
		"OTELCONTEXT_ALLOW_INSECURE_GRPC": "",
		"PPROF_ADDR":                      "127.0.0.1:" + strconv.Itoa(a.pprof),
		"SAMPLING_ALWAYS_ON_ERRORS":       "true",
		"SAMPLING_RATE":                   "1.0",
		"TLS_AUTO_SELFSIGNED":             "false",
		"TLS_CERT_FILE":                   "",
		"TLS_KEY_FILE":                    "",
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[key]; !replaced {
			env = append(env, item)
		}
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+overrides[key])
	}
	return env
}

func (a *appProcess) start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd != nil {
		return errors.New("application is already running")
	}
	cmd := exec.Command(a.binary)
	cmd.Dir = a.dir
	cmd.Env = a.environment()
	cmd.Stdout = a.log
	cmd.Stderr = a.log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start application: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	a.cmd = cmd
	a.done = done
	return nil
}

func (a *appProcess) stop() error {
	a.mu.Lock()
	cmd := a.cmd
	done := a.done
	a.cmd = nil
	a.done = nil
	a.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(8 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return errors.New("application did not stop within 8s and was killed")
	}
}

func waitReady(ctx context.Context, baseURL string) error {
	client := &http.Client{
		Timeout:   time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ready", nil)
		if err != nil {
			return err
		}
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
			return fmt.Errorf("wait for /ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

type requestRecord struct {
	At       string `json:"at"`
	Phase    string `json:"phase"`
	Method   string `json:"method"`
	URL      string `json:"url"`
	PostData string `json:"post_data,omitempty"`
}

func stamp() string {
	return time.Now().Format("15:04:05.000")
}

type eventRecorder struct {
	mu                 sync.Mutex
	origin             *url.URL
	phase              string
	requests           []requestRecord
	console            []string
	exceptions         []string
	failedRequests     []string
	unexpectedFailures []string
	externalRequests   []string
	requestURLs        map[network.RequestID]string
	responseStatus     map[network.RequestID]int64
	phases             []string
}

func newEventRecorder(rawOrigin string) *eventRecorder {
	origin, _ := url.Parse(rawOrigin)
	return &eventRecorder{
		origin:         origin,
		phase:          "setup",
		requestURLs:    make(map[network.RequestID]string),
		responseStatus: make(map[network.RequestID]int64),
	}
}

// lenientPhase names the phases whose console errors and failed requests are
// expected: a forced graph failure, a server restart, and full page reloads.
func lenientPhase(phase string) bool {
	switch phase {
	case "forced-error", "websocket-recovery", "theme-persistence", "host-group-reload":
		return true
	}
	return false
}

func (r *eventRecorder) setPhase(phase string) {
	r.mu.Lock()
	r.phase = phase
	r.phases = append(r.phases, stamp()+" "+phase)
	r.mu.Unlock()
}

func (r *eventRecorder) snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{
		"console":             append([]string(nil), r.console...),
		"exceptions":          append([]string(nil), r.exceptions...),
		"failed_requests":     append([]string(nil), r.failedRequests...),
		"unexpected_failures": append([]string(nil), r.unexpectedFailures...),
		"external_requests":   append([]string(nil), r.externalRequests...),
		"phases":              append([]string(nil), r.phases...),
		"requests":            append([]requestRecord(nil), r.requests...),
	}
}

func (r *eventRecorder) hasMCPTool(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	needle := `"name":"` + name + `"`
	for _, request := range r.requests {
		if strings.HasSuffix(request.URL, "/mcp") && strings.Contains(request.PostData, needle) {
			return true
		}
	}
	return false
}

func (r *eventRecorder) listen(ctx context.Context) {
	chromedp.ListenTarget(ctx, func(event any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		phase := r.phase
		switch event := event.(type) {
		case *network.EventRequestWillBeSent:
			var postData strings.Builder
			for _, entry := range event.Request.PostDataEntries {
				decoded, err := base64.StdEncoding.DecodeString(entry.Bytes)
				if err != nil {
					postData.WriteString(entry.Bytes)
				} else {
					postData.Write(decoded)
				}
			}
			record := requestRecord{
				At:       stamp(),
				Phase:    phase,
				Method:   event.Request.Method,
				URL:      event.Request.URL,
				PostData: postData.String(),
			}
			if len(r.requests) < 500 {
				r.requests = append(r.requests, record)
			}
			r.requestURLs[event.RequestID] = event.Request.URL
			if requestURL, err := url.Parse(event.Request.URL); err == nil {
				switch requestURL.Scheme {
				case "http", "https", "ws", "wss":
					if r.origin != nil && !strings.EqualFold(requestURL.Host, r.origin.Host) {
						r.externalRequests = append(r.externalRequests, event.Request.URL)
					}
				}
			}
		case *network.EventResponseReceived:
			r.responseStatus[event.RequestID] = event.Response.Status
		case *network.EventLoadingFailed:
			message := phase + ": " + event.ErrorText + " " + r.requestURLs[event.RequestID] + " at " + stamp()
			// Chrome reports a conditional fetch answered 304 as an aborted
			// load once it serves the cached body; the page saw a success.
			revalidated := event.ErrorText == "net::ERR_ABORTED" && r.responseStatus[event.RequestID] == http.StatusNotModified
			if revalidated {
				message += " (304 revalidation)"
			}
			r.failedRequests = append(r.failedRequests, message)
			if !revalidated && !lenientPhase(phase) {
				r.unexpectedFailures = append(r.unexpectedFailures, message)
			}
		case *cdpruntime.EventExceptionThrown:
			r.exceptions = append(r.exceptions, fmt.Sprintf("%s: %s", phase, event.ExceptionDetails.Text))
		case *cdpruntime.EventConsoleAPICalled:
			message := fmt.Sprintf("%s: %s", phase, event.Type)
			r.console = append(r.console, message)
			if event.Type == cdpruntime.APITypeError && !lenientPhase(phase) {
				r.unexpectedFailures = append(r.unexpectedFailures, message)
			}
		case *cdplog.EventEntryAdded:
			message := fmt.Sprintf("%s: %s: %s", phase, event.Entry.Level, event.Entry.Text)
			r.console = append(r.console, message)
			if event.Entry.Level == cdplog.LevelError && !lenientPhase(phase) {
				r.unexpectedFailures = append(r.unexpectedFailures, message)
			}
		}
	})
}

type graphInterceptor struct {
	mu     sync.Mutex
	ctx    context.Context
	mode   string
	paused chan fetch.RequestID
}

func newGraphInterceptor(ctx context.Context) *graphInterceptor {
	return &graphInterceptor{
		ctx:    ctx,
		mode:   "pass",
		paused: make(chan fetch.RequestID, 4),
	}
}

func (i *graphInterceptor) setMode(mode string) {
	i.mu.Lock()
	i.mode = mode
	i.mu.Unlock()
}

func (i *graphInterceptor) listen() {
	chromedp.ListenTarget(i.ctx, func(event any) {
		paused, ok := event.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		i.mu.Lock()
		mode := i.mode
		i.mu.Unlock()
		switch mode {
		case "pause":
			select {
			case i.paused <- paused.RequestID:
			default:
			}
		case "fail":
			go func() {
				_ = chromedp.Run(i.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
					return fetch.FailRequest(paused.RequestID, network.ErrorReasonFailed).Do(ctx)
				}))
			}()
		default:
			go func() {
				_ = chromedp.Run(i.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
					return fetch.ContinueRequest(paused.RequestID).Do(ctx)
				}))
			}()
		}
	})
}

func (i *graphInterceptor) release(ctx context.Context) error {
	select {
	case requestID := <-i.paused:
		i.setMode("pass")
		return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			return fetch.ContinueRequest(requestID).Do(ctx)
		}))
	case <-ctx.Done():
		return fmt.Errorf("wait for paused graph request: %w", ctx.Err())
	}
}

type smokeRun struct {
	t             *testing.T
	ctx           context.Context
	artifacts     string
	binary        string
	binarySHA256  string
	chrome        string
	chromeVersion string
	app           *appProcess
	recorder      *eventRecorder
	completed     []string
	completedSet  map[string]bool
	inventory     protectedInventory
}

func newSmokeRun(t *testing.T, ctx context.Context, artifacts, binary, chrome string, app *appProcess, recorder *eventRecorder) *smokeRun {
	t.Helper()
	var inventory protectedInventory
	if err := json.Unmarshal(protectedFeaturesJSON, &inventory); err != nil {
		t.Fatalf("parse protected feature inventory: %v", err)
	}
	if inventory.SchemaVersion != 1 {
		t.Fatalf("protected feature inventory schema = %d, want 1", inventory.SchemaVersion)
	}
	return &smokeRun{
		t:             t,
		ctx:           ctx,
		artifacts:     artifacts,
		binary:        binary,
		binarySHA256:  fileSHA256(t, binary),
		chrome:        chrome,
		chromeVersion: commandOutput(t, chrome, "--version"),
		app:           app,
		recorder:      recorder,
		completedSet:  make(map[string]bool),
		inventory:     inventory,
	}
}

func (s *smokeRun) phase(name string) {
	s.recorder.setPhase(name)
}

func (s *smokeRun) complete(name string) {
	if s.completedSet[name] {
		return
	}
	s.completedSet[name] = true
	s.completed = append(s.completed, name)
	s.t.Logf("browser phase passed: %s", name)
}

func (s *smokeRun) validateInventory() {
	for _, feature := range s.inventory.Features {
		if feature.Proof == "browser" && !s.completedSet[feature.Phase] {
			s.t.Errorf("protected feature %q has no completed browser phase %q", feature.ID, feature.Phase)
		}
	}
}

func (s *smokeRun) screenshot(name string) {
	var image []byte
	if err := chromedp.Run(s.ctx, chromedp.FullScreenshot(&image, 90)); err != nil {
		s.t.Logf("capture %s screenshot: %v", name, err)
		return
	}
	if err := os.WriteFile(filepath.Join(s.artifacts, name+".png"), image, 0o600); err != nil {
		s.t.Logf("write %s screenshot: %v", name, err)
	}
}

func (s *smokeRun) writeDiagnostics() {
	var location, document string
	_ = chromedp.Run(s.ctx,
		chromedp.Location(&location),
		chromedp.OuterHTML("html", &document, chromedp.ByQuery),
	)
	_ = os.WriteFile(filepath.Join(s.artifacts, "dom.html"), []byte(document), 0o600)
	_ = os.WriteFile(filepath.Join(s.artifacts, "server.log"), s.app.log.Bytes(), 0o600)
	_ = os.WriteFile(filepath.Join(s.artifacts, "protected_features.json"), protectedFeaturesJSON, 0o600)
	events := s.recorder.snapshot()
	writeJSONFile(s.t, filepath.Join(s.artifacts, "browser-events.json"), events)
	metadata := map[string]any{
		"binary":               s.binary,
		"binary_sha256":        s.binarySHA256,
		"browser":              s.chrome,
		"browser_version":      s.chromeVersion,
		"completed_phases":     append([]string(nil), s.completed...),
		"last_completed_phase": lastString(s.completed),
		"url":                  location,
	}
	writeJSONFile(s.t, filepath.Join(s.artifacts, "metadata.json"), metadata)
	s.screenshot("final")
}

func lastString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Logf("marshal %s: %v", filepath.Base(path), err)
		return
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Logf("write %s: %v", filepath.Base(path), err)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read binary for digest: %v", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func commandOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output))
}

func requiredBinary(t *testing.T) string {
	t.Helper()
	binary := strings.TrimSpace(os.Getenv("OTELCONTEXT_BINARY"))
	if binary == "" {
		t.Fatal("OTELCONTEXT_BINARY is required and must point to the exact built application")
	}
	abs, err := filepath.Abs(binary)
	if err != nil {
		t.Fatalf("resolve OTELCONTEXT_BINARY: %v", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat OTELCONTEXT_BINARY: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatalf("OTELCONTEXT_BINARY %q is not an executable regular file", abs)
	}
	return abs
}

func requiredChrome(t *testing.T) string {
	t.Helper()
	if configured := strings.TrimSpace(os.Getenv("CHROME_BIN")); configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			t.Fatalf("CHROME_BIN %q is unavailable: %v", configured, err)
		}
		return path
	}
	candidates := []string{"chromium", "chromium-browser", "google-chrome-stable", "google-chrome"}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Fatalf("no Chrome-family browser found; searched %s", strings.Join(candidates, ", "))
	return ""
}

func artifactDirectory(t *testing.T) string {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv("BROWSER_SMOKE_ARTIFACTS"))
	if dir == "" {
		dir = filepath.Join(t.TempDir(), "browser-smoke")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolve browser artifact directory: %v", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("create browser artifact directory: %v", err)
	}
	t.Logf("browser smoke artifacts: %s", abs)
	return abs
}

func assertAssets(t *testing.T, baseURL string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	for _, path := range []string{"/", "/static/app.css", "/static/app.js", "/static/favicon.svg"} {
		resp, err := client.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if resp.StatusCode != http.StatusOK || len(body) == 0 {
			t.Fatalf("GET %s = %d with %d bytes, want 200 with a body", path, resp.StatusCode, len(body))
		}
	}
}

func waitJS(ctx context.Context, expression string, timeout time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var ready bool
		if err := chromedp.Run(deadline, chromedp.Evaluate(expression, &ready)); err == nil && ready {
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("condition did not become true: %s: %w", expression, deadline.Err())
		case <-ticker.C:
		}
	}
}

func requireJS(t *testing.T, ctx context.Context, expression string, timeout time.Duration) {
	t.Helper()
	if err := waitJS(ctx, expression, timeout); err != nil {
		t.Fatal(err)
	}
}

func evaluateString(t *testing.T, ctx context.Context, expression string) string {
	t.Helper()
	var value string
	if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &value)); err != nil {
		t.Fatalf("evaluate %s: %v", expression, err)
	}
	return value
}

func clickText(t *testing.T, ctx context.Context, label string) {
	t.Helper()
	xpath := fmt.Sprintf("//button[normalize-space()=%q]", label)
	if err := chromedp.Run(ctx, chromedp.Click(xpath, chromedp.BySearch)); err != nil {
		t.Fatalf("click button %q: %v", label, err)
	}
}

func tracePayload(service, spanID, parentSpanID string, start, end int64) map[string]any {
	span := map[string]any{
		"traceId":           "0123456789abcdef0123456789abcdef",
		"spanId":            spanID,
		"name":              service + " request",
		"kind":              2,
		"startTimeUnixNano": strconv.FormatInt(start, 10),
		"endTimeUnixNano":   strconv.FormatInt(end, 10),
		"status":            map[string]any{"code": 1},
	}
	if parentSpanID != "" {
		span["parentSpanId"] = parentSpanID
	}
	return map[string]any{
		"resource": map[string]any{
			"attributes": []map[string]any{{
				"key":   "service.name",
				"value": map[string]any{"stringValue": service},
			}},
		},
		"scopeSpans": []map[string]any{{"spans": []map[string]any{span}}},
	}
}

func postOTLP(t *testing.T, endpoint string, payload map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal OTLP payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create OTLP request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("export OTLP payload: %v", err)
	}
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("export OTLP payload to %s = %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if len(bytes.TrimSpace(responseBody)) > 0 {
		t.Logf("OTLP response from %s: %s", endpoint, strings.TrimSpace(string(responseBody)))
	}
}

func injectTopology(t *testing.T, baseURL string) {
	t.Helper()
	now := time.Now().UTC().UnixNano()
	resources := []map[string]any{
		tracePayload("gateway", "1111111111111111", "", now, now+int64(12*time.Millisecond)),
		tracePayload("checkout", "2222222222222222", "1111111111111111", now+int64(time.Millisecond), now+int64(10*time.Millisecond)),
	}
	for _, resource := range resources {
		postOTLP(t, baseURL+"/v1/traces", map[string]any{"resourceSpans": []map[string]any{resource}})
	}
}

// tracePayloadOnHost is tracePayload with a host.name resource attribute.
func tracePayloadOnHost(service, host, spanID, parentSpanID string, start, end int64) map[string]any {
	payload := tracePayload(service, spanID, parentSpanID, start, end)
	resource := payload["resource"].(map[string]any)
	resource["attributes"] = append(resource["attributes"].([]map[string]any), map[string]any{
		"key":   "host.name",
		"value": map[string]any{"stringValue": host},
	})
	return payload
}

// hostMetricsPayload is a hostmetrics-shaped resource: host.name, no
// service.name, one gauge point per metric.
func hostMetricsPayload(host string, now int64, values map[string]float64) map[string]any {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	metrics := make([]map[string]any, 0, len(names))
	for _, name := range names {
		metrics = append(metrics, map[string]any{
			"name": name,
			"gauge": map[string]any{"dataPoints": []map[string]any{{
				"timeUnixNano": strconv.FormatInt(now, 10),
				"asDouble":     values[name],
			}}},
		})
	}
	return map[string]any{"resourceMetrics": []map[string]any{{
		"resource": map[string]any{
			"attributes": []map[string]any{{
				"key":   "host.name",
				"value": map[string]any{"stringValue": host},
			}},
		},
		"scopeMetrics": []map[string]any{{"metrics": metrics}},
	}}}
}

// injectHostFixture seeds the host-grouping fixture: gateway on node-a,
// checkout on node-a and node-b, and a hostmetrics-only node-c reporting CPU
// and memory utilization but no filesystem metric.
func injectHostFixture(t *testing.T, baseURL string) {
	t.Helper()
	now := time.Now().UTC().UnixNano()
	resources := []map[string]any{
		tracePayloadOnHost("gateway", "node-a", "3333333333333333", "", now, now+int64(12*time.Millisecond)),
		tracePayloadOnHost("checkout", "node-a", "4444444444444444", "3333333333333333", now+int64(time.Millisecond), now+int64(9*time.Millisecond)),
		tracePayloadOnHost("checkout", "node-b", "5555555555555555", "3333333333333333", now+int64(2*time.Millisecond), now+int64(10*time.Millisecond)),
	}
	for _, resource := range resources {
		postOTLP(t, baseURL+"/v1/traces", map[string]any{"resourceSpans": []map[string]any{resource}})
	}
	postOTLP(t, baseURL+"/v1/metrics", hostMetricsPayload("node-c", now, map[string]float64{
		"system.cpu.utilization":    0.42,
		"system.memory.utilization": 0.61,
	}))
}

func getJSON(ctx context.Context, rawURL string, target any) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	_ = resp.Body.Close()
	if readErr != nil {
		return string(body), readErr
	}
	return string(body), json.Unmarshal(body, target)
}

// waitHosts polls /api/hosts until the fixture's three hosts are projected
// with checkout and gateway both on node-a.
func waitHosts(ctx context.Context, baseURL string) error {
	type host struct {
		Name     string   `json:"name"`
		Services []string `json:"services"`
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		var hosts []host
		body, err := getJSON(ctx, baseURL+"/api/hosts", &hosts)
		last = body
		if err == nil && len(hosts) == 3 && hosts[0].Name == "node-a" && strings.Join(hosts[0].Services, ",") == "checkout,gateway" &&
			hosts[1].Name == "node-b" && hosts[2].Name == "node-c" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for host projection: %w; last response: %s", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

// waitHostMetric polls the metrics API until the TSDB has flushed node-c's
// CPU gauge into a bucket the host panel can read.
func waitHostMetric(ctx context.Context, baseURL string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		end := time.Now().UTC()
		query := url.Values{
			"name":         {"system.cpu.utilization"},
			"service_name": {"host/node-c"},
			"start":        {end.Add(-time.Hour).Format(time.RFC3339)},
			"end":          {end.Format(time.RFC3339)},
		}
		var buckets []map[string]any
		body, err := getJSON(ctx, baseURL+"/api/metrics?"+query.Encode(), &buckets)
		last = body
		if err == nil && len(buckets) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for host/node-c metric bucket: %w; last response: %s", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

func waitTopology(ctx context.Context, baseURL string) error {
	type graph struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
		Edges []struct {
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"edges"`
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastResponse string
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/system/graph", nil)
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
			lastResponse = string(body)
			var value graph
			decodeErr := json.Unmarshal(body, &value)
			if readErr == nil && decodeErr == nil && len(value.Nodes) == 2 {
				for _, edge := range value.Edges {
					if edge.Source == "gateway" && edge.Target == "checkout" {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for gateway -> checkout topology: %w; last response: %s", ctx.Err(), lastResponse)
		case <-ticker.C:
		}
	}
}

func assertNoUnexpectedBrowserEvents(t *testing.T, recorder *eventRecorder) {
	t.Helper()
	events := recorder.snapshot()
	for _, key := range []string{"exceptions", "unexpected_failures", "external_requests"} {
		values, _ := events[key].([]string)
		if len(values) > 0 {
			t.Errorf("browser %s: %v", key, values)
		}
	}
}

func TestProtectedBrowserWorkflow(t *testing.T) {
	if browserCase := strings.TrimSpace(os.Getenv("OTELCONTEXT_BROWSER_CASE")); browserCase != "" && browserCase != "protected" {
		t.Skip("OTELCONTEXT_BROWSER_CASE selects " + browserCase)
	}
	binary := requiredBinary(t)
	chrome := requiredChrome(t)
	artifacts := artifactDirectory(t)

	app := newAppProcess(t, binary)
	if err := app.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.stop(); err != nil {
			t.Logf("cleanup application: %v", err)
		}
	})
	readyCtx, readyCancel := context.WithTimeout(context.Background(), readyTimeout)
	if err := waitReady(readyCtx, app.baseURL()); err != nil {
		readyCancel()
		t.Fatalf("application did not become ready: %v\n%s", err, app.log.Bytes())
	}
	readyCancel()
	assertAssets(t, app.baseURL())

	allocatorOptions := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	allocatorOptions = append(allocatorOptions,
		chromedp.ExecPath(chrome),
		chromedp.UserDataDir(filepath.Join(t.TempDir(), "chrome-profile")),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	if os.Geteuid() == 0 {
		allocatorOptions = append(allocatorOptions, chromedp.NoSandbox)
	}
	allocatorCtx, cancelAllocator := chromedp.NewExecAllocator(context.Background(), allocatorOptions...)
	defer cancelAllocator()
	browserCtx, cancelBrowser := chromedp.NewContext(allocatorCtx)
	defer cancelBrowser()
	ctx, cancelTest := context.WithTimeout(browserCtx, testTimeout)
	defer cancelTest()

	recorder := newEventRecorder(app.baseURL())
	recorder.listen(ctx)
	interceptor := newGraphInterceptor(ctx)
	interceptor.listen()
	smoke := newSmokeRun(t, ctx, artifacts, binary, chrome, app, recorder)
	defer smoke.writeDiagnostics()

	smoke.phase("page-and-assets")
	interceptor.setMode("pause")
	if err := chromedp.Run(ctx,
		cdpruntime.Enable(),
		network.Enable(),
		cdplog.Enable(),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{
			URLPattern:   "*/api/system/graph*",
			RequestStage: fetch.RequestStageRequest,
		}}),
		chromedp.Navigate(app.baseURL()+"/"),
	); err != nil {
		t.Fatalf("open embedded UI: %v", err)
	}
	requireJS(t, ctx, `document.readyState === "complete" && !!document.querySelector("#service-search")`, 10*time.Second)
	smoke.complete("page-and-assets")

	smoke.phase("loading-empty-error-retry")
	requireJS(t, ctx, `!document.querySelector("#loading-state").hidden`, 5*time.Second)
	releaseCtx, releaseCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := interceptor.release(releaseCtx); err != nil {
		releaseCancel()
		t.Fatal(err)
	}
	releaseCancel()
	requireJS(t, ctx, `!document.querySelector("#empty-state").hidden`, 10*time.Second)

	smoke.phase("forced-error")
	interceptor.setMode("fail")
	if err := chromedp.Run(ctx, chromedp.Click("#refresh-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("force graph refresh: %v", err)
	}
	requireJS(t, ctx, `!document.querySelector("#error-state").hidden && !document.querySelector("#retry-button").hidden`, 10*time.Second)
	interceptor.setMode("pass")
	if err := chromedp.Run(ctx, chromedp.Click("#retry-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("retry graph request: %v", err)
	}
	requireJS(t, ctx, `!document.querySelector("#empty-state").hidden`, 10*time.Second)
	smoke.phase("loading-empty-error-retry")
	smoke.complete("loading-empty-error-retry")

	smoke.phase("connect-guidance")
	requireJS(t, ctx, `
		(() => {
			const text = document.querySelector("#empty-state").textContent;
			return text.includes("MCP URL") && text.includes("OTLP gRPC") && text.includes("OTLP HTTP");
		})()
	`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#connect-toggle", chromedp.ByQuery)); err != nil {
		t.Fatalf("open Connect menu: %v", err)
	}
	requireJS(t, ctx, `document.querySelector("#connect-menu").open && document.querySelectorAll("[data-copy-endpoint]").length >= 6`, 5*time.Second)
	smoke.complete("connect-guidance")

	smoke.phase("live-pulse-and-topology")
	injectTopology(t, app.baseURL())
	topologyCtx, topologyCancel := context.WithTimeout(context.Background(), readyTimeout)
	if err := waitTopology(topologyCtx, app.baseURL()); err != nil {
		topologyCancel()
		t.Fatal(err)
	}
	topologyCancel()
	requireJS(t, ctx, `!document.querySelector("#refresh-button").disabled`, 10*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#refresh-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("refresh injected topology: %v", err)
	}
	requireJS(t, ctx, `
		(() => {
			const edge = document.querySelector('#graph-edges [data-source="gateway"][data-target="checkout"]');
			return document.querySelector("#pulse-services").textContent.trim() === "2" &&
				document.querySelectorAll("#graph-nodes .service-node").length === 2 && !!edge;
		})()
	`, 15*time.Second)
	requireJS(t, ctx, `document.querySelectorAll("#service-minimap circle").length === 2`, 5*time.Second)
	constUptime := evaluateString(t, ctx, `document.querySelector("#pulse-uptime").textContent.trim()`)
	requireJS(t, ctx, fmt.Sprintf(`document.querySelector("#pulse-uptime").textContent.trim() !== %q`, constUptime), 4*time.Second)
	smoke.screenshot("desktop-dark-initial")
	smoke.complete("live-pulse-and-topology")

	smoke.phase("search-selection-and-url")
	if err := chromedp.Run(ctx,
		chromedp.Focus("#service-search", chromedp.ByQuery),
		chromedp.SendKeys("#service-search", "checkout", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("search for checkout: %v", err)
	}
	requireJS(t, ctx, `document.querySelector("#search-count").textContent.trim() === "1 found"`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click(`#service-list [data-service="checkout"]`, chromedp.ByQuery)); err != nil {
		t.Fatalf("select checkout from service list: %v", err)
	}
	requireJS(t, ctx, `
		!document.querySelector("#inspector").inert &&
		document.querySelector("#inspector-title").textContent.trim() === "checkout" &&
		new URL(location.href).searchParams.get("service") === "checkout"
	`, 5*time.Second)
	smoke.complete("search-selection-and-url")

	smoke.phase("overview-and-dependencies")
	requireJS(t, ctx, `document.querySelectorAll("#inspector-body .stat-card").length === 4`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#tab-dependencies", chromedp.ByQuery)); err != nil {
		t.Fatalf("open Dependencies: %v", err)
	}
	requireJS(t, ctx, `
		document.querySelector("#inspector-body").textContent.includes("Upstream callers") &&
		document.querySelector("#inspector-body").textContent.includes("gateway")
	`, 5*time.Second)
	smoke.complete("overview-and-dependencies")

	smoke.phase("why-and-impact")
	if err := chromedp.Run(ctx, chromedp.Click("#tab-why", chromedp.ByQuery)); err != nil {
		t.Fatalf("open Why: %v", err)
	}
	clickText(t, ctx, "Run root-cause analysis")
	requireJS(t, ctx, `
		!document.querySelector("#inspector-body .spinner") &&
		(document.querySelector("#inspector-body").textContent.includes("No probable causes") || !!document.querySelector("#inspector-body .cause-list"))
	`, 20*time.Second)
	if !recorder.hasMCPTool("root_cause_analysis") {
		t.Fatal("Why did not issue a tools/call request for root_cause_analysis")
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		(() => {
			const search = document.querySelector("#service-search");
			search.value = "";
			search.dispatchEvent(new Event("input", { bubbles: true }));
		})()
	`, nil)); err != nil {
		t.Fatalf("clear service search: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Click("#close-inspector-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("close checkout inspector: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`#service-list [data-service="gateway"]`, chromedp.ByQuery)); err != nil {
		t.Fatalf("select gateway: %v", err)
	}
	requireJS(t, ctx, `document.querySelector("#inspector-title").textContent.trim() === "gateway"`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#tab-impact", chromedp.ByQuery)); err != nil {
		t.Fatalf("open Impact: %v", err)
	}
	clickText(t, ctx, "Map blast radius")
	requireJS(t, ctx, `
		!document.querySelector("#inspector-body .spinner") &&
		document.querySelector("#inspector-body").textContent.includes("checkout") &&
		document.querySelector("#inspector-body").textContent.includes("Show on map")
	`, 20*time.Second)
	if !recorder.hasMCPTool("impact_analysis") {
		t.Fatal("Impact did not issue a tools/call request for impact_analysis")
	}
	clickText(t, ctx, "Show on map")
	requireJS(t, ctx, `
		new URL(location.href).searchParams.get("impact") === "gateway" &&
		!document.querySelector("#impact-banner").hidden &&
		document.querySelector("#inspector").inert
	`, 5*time.Second)
	smoke.complete("why-and-impact")

	smoke.phase("command-palette-and-keyboard")
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		document.dispatchEvent(new KeyboardEvent("keydown", { key: "k", ctrlKey: true, bubbles: true }))
	`, nil)); err != nil {
		t.Fatalf("open command palette with Ctrl+K: %v", err)
	}
	requireJS(t, ctx, `
		document.querySelector("#command-dialog").open &&
		!!document.querySelector('[data-command-action="root-cause"]') &&
		!!document.querySelector('[data-command-action="impact"]') &&
		!!document.querySelector('[data-command-service="checkout"]') &&
		!!document.querySelector('[data-command="toggle-theme"]') &&
		!!document.querySelector('[data-command="copy-mcp"]')
	`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click(`[data-command-service="checkout"]`, chromedp.ByQuery)); err != nil {
		t.Fatalf("open checkout from command palette: %v", err)
	}
	requireJS(t, ctx, `document.querySelector("#inspector-title").textContent.trim() === "checkout"`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#close-inspector-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("close command-selected inspector: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		document.dispatchEvent(new KeyboardEvent("keydown", { key: "?", bubbles: true }))
	`, nil)); err != nil {
		t.Fatalf("open shortcut sheet: %v", err)
	}
	requireJS(t, ctx, `document.querySelector("#shortcut-dialog").open && document.querySelector("#shortcut-dialog").textContent.includes("Keyboard shortcuts")`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#close-shortcut-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("close shortcut sheet: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		document.dispatchEvent(new KeyboardEvent("keydown", { key: "/", bubbles: true }))
	`, nil)); err != nil {
		t.Fatalf("focus search shortcut: %v", err)
	}
	requireJS(t, ctx, `document.activeElement === document.querySelector("#service-search")`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		(() => {
			const node = document.querySelector('#graph-nodes [data-service="gateway"]');
			node.focus();
			node.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
		})()
	`, nil)); err != nil {
		t.Fatalf("open map node from keyboard: %v", err)
	}
	requireJS(t, ctx, `document.querySelector("#inspector-title").textContent.trim() === "gateway"`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		(() => {
			const tab = document.querySelector("#tab-overview");
			tab.focus();
			tab.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true }));
		})()
	`, nil)); err != nil {
		t.Fatalf("move inspector tab with keyboard: %v", err)
	}
	requireJS(t, ctx, `document.querySelector("#tab-why").getAttribute("aria-selected") === "true"`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#close-inspector-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("close keyboard inspector: %v", err)
	}
	smoke.complete("command-palette-and-keyboard")

	smoke.phase("websocket-recovery")
	requireJS(t, ctx, `document.querySelector("#connection-label").textContent.trim() === "Live"`, 15*time.Second)
	if err := app.stop(); err != nil {
		t.Fatal(err)
	}
	requireJS(t, ctx, `/Offline|Reconnecting/.test(document.querySelector("#connection-label").textContent)`, reconnectTimeout)
	if err := app.start(); err != nil {
		t.Fatalf("restart application: %v", err)
	}
	restartReadyCtx, restartReadyCancel := context.WithTimeout(context.Background(), readyTimeout)
	if err := waitReady(restartReadyCtx, app.baseURL()); err != nil {
		restartReadyCancel()
		t.Fatal(err)
	}
	restartReadyCancel()
	requireJS(t, ctx, `document.querySelector("#connection-label").textContent.trim() === "Live"`, reconnectTimeout)
	requireJS(t, ctx, `!document.querySelector("#refresh-button").disabled`, 10*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#refresh-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("refresh after reconnect: %v", err)
	}
	requireJS(t, ctx, `
		document.querySelector("#pulse-services").textContent.trim() === "2" &&
		!!document.querySelector('#graph-edges [data-source="gateway"][data-target="checkout"]')
	`, 15*time.Second)
	smoke.complete("websocket-recovery")

	smoke.phase("theme-persistence")
	currentTheme := evaluateString(t, ctx, `document.documentElement.dataset.theme`)
	if currentTheme != "dark" {
		if err := chromedp.Run(ctx, chromedp.Click("#theme-button", chromedp.ByQuery)); err != nil {
			t.Fatalf("switch to dark theme: %v", err)
		}
	}
	requireJS(t, ctx, `document.documentElement.dataset.theme === "dark"`, 5*time.Second)
	smoke.screenshot("theme-dark")
	if err := chromedp.Run(ctx, chromedp.Click("#theme-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("switch to light theme: %v", err)
	}
	requireJS(t, ctx, `document.documentElement.dataset.theme === "light" && localStorage.getItem("oc-theme") === "light"`, 5*time.Second)
	smoke.screenshot("theme-light")
	if err := chromedp.Run(ctx, chromedp.Reload()); err != nil {
		t.Fatalf("reload persisted theme: %v", err)
	}
	requireJS(t, ctx, `document.documentElement.dataset.theme === "light" && document.querySelector("#pulse-services").textContent.trim() === "2"`, 15*time.Second)
	smoke.complete("theme-persistence")

	smoke.phase("mobile-map-list-inspector")
	if err := chromedp.Run(ctx, chromedp.EmulateViewport(390, 844)); err != nil {
		t.Fatalf("set mobile viewport: %v", err)
	}
	requireJS(t, ctx, `matchMedia("(max-width: 767px)").matches`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#list-view-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("open mobile list: %v", err)
	}
	requireJS(t, ctx, `!document.querySelector("#mobile-list").hidden`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click(`#mobile-list [data-service="checkout"]`, chromedp.ByQuery)); err != nil {
		t.Fatalf("open mobile inspector: %v", err)
	}
	requireJS(t, ctx, `
		!document.querySelector("#inspector").inert &&
		document.documentElement.scrollWidth <= document.documentElement.clientWidth &&
		document.body.scrollWidth <= document.body.clientWidth
	`, 5*time.Second)
	smoke.screenshot("mobile-inspector")
	if err := chromedp.Run(ctx,
		chromedp.Click("#close-inspector-button", chromedp.ByQuery),
		chromedp.Click("#map-view-button", chromedp.ByQuery),
	); err != nil {
		t.Fatalf("return to mobile map: %v", err)
	}
	requireJS(t, ctx, `
		!document.querySelector("#canvas-wrap").hidden &&
		document.documentElement.scrollWidth <= document.documentElement.clientWidth &&
		document.body.scrollWidth <= document.body.clientWidth
	`, 5*time.Second)
	smoke.screenshot("mobile-map")
	smoke.complete("mobile-map-list-inspector")

	smoke.phase("host-group")
	if err := chromedp.Run(ctx, chromedp.EmulateViewport(1280, 900)); err != nil {
		t.Fatalf("restore desktop viewport: %v", err)
	}
	requireJS(t, ctx, `!matchMedia("(max-width: 767px)").matches`, 5*time.Second)
	injectHostFixture(t, app.baseURL())
	hostsCtx, hostsCancel := context.WithTimeout(context.Background(), readyTimeout)
	if err := waitHosts(hostsCtx, app.baseURL()); err != nil {
		hostsCancel()
		t.Fatal(err)
	}
	hostsCancel()
	requireJS(t, ctx, `!document.querySelector("#refresh-button").disabled`, 10*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#refresh-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("refresh host fixture: %v", err)
	}
	requireJS(t, ctx, `document.querySelector("#host-group-button").getAttribute("aria-disabled") === "false"`, 10*time.Second)
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector("#host-group-button").focus()`, nil)); err != nil {
		t.Fatalf("focus host toggle: %v", err)
	}
	requireJS(t, ctx, `document.activeElement === document.querySelector("#host-group-button")`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Click("#host-group-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("toggle host grouping: %v", err)
	}
	requireJS(t, ctx, `
		(() => {
			const headings = Array.from(document.querySelectorAll("#service-list .host-heading"));
			const checkout = document.querySelectorAll('#service-list [data-service="checkout"]');
			let heading = checkout.length === 1 ? checkout[0].previousElementSibling : null;
			while (heading && !heading.classList.contains("host-heading")) heading = heading.previousElementSibling;
			return document.querySelector("#host-group-button").getAttribute("aria-pressed") === "true" &&
				new URL(location.href).searchParams.get("group") === "host" &&
				headings.map((item) => item.dataset.host + ":" + item.querySelector(".host-count").textContent).join("|") ===
					"node-a:2 services|node-b:1 service|node-c:0 services" &&
				checkout.length === 1 && heading && heading.dataset.host === "node-a" &&
				checkout[0].querySelector(".service-meta").textContent.includes("2 hosts") &&
				document.querySelectorAll("#graph-clusters .cluster-heading[data-host]").length === 3 &&
				document.querySelectorAll("#graph-nodes .service-node").length === 2 &&
				document.querySelector("#service-count").textContent.trim() === "2" &&
				document.querySelector("#pulse-services").textContent.trim() === "2";
		})()
	`, 10*time.Second)
	smoke.screenshot("host-group-desktop")
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		(() => {
			const heading = document.querySelector('#graph-clusters .cluster-heading[data-host="node-c"]');
			heading.focus();
			heading.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
		})()
	`, nil)); err != nil {
		t.Fatalf("open host panel from map heading: %v", err)
	}
	requireJS(t, ctx, `
		!document.querySelector("#inspector").inert &&
		document.querySelector("#inspector-title").textContent.trim() === "node-c" &&
		document.querySelector("#inspector-tabs").hidden &&
		new URL(location.href).searchParams.get("host") === "node-c" &&
		document.querySelectorAll('#inspector-body [data-metric]').length === 3 &&
		document.querySelector("#inspector-body").textContent.includes("Services · 0")
	`, 5*time.Second)
	metricCtx, metricCancel := context.WithTimeout(context.Background(), 45*time.Second)
	if err := waitHostMetric(metricCtx, app.baseURL()); err != nil {
		metricCancel()
		t.Fatal(err)
	}
	metricCancel()
	if err := chromedp.Run(ctx, chromedp.Click("#refresh-button", chromedp.ByQuery)); err != nil {
		t.Fatalf("refresh host metrics: %v", err)
	}
	requireJS(t, ctx, `
		(() => {
			const value = (name) => document.querySelector('#inspector-body [data-metric="' + name + '"] strong').textContent.trim();
			return value("system.cpu.utilization") === "42%" &&
				value("system.memory.utilization") === "61%" &&
				value("system.filesystem.utilization") === "not reported" &&
				document.querySelectorAll('#inspector-body [data-metric="system.cpu.utilization"] .spark').length === 1;
		})()
	`, 15*time.Second)
	smoke.screenshot("host-panel-desktop")
	if err := chromedp.Run(ctx,
		chromedp.Click("#close-inspector-button", chromedp.ByQuery),
		chromedp.Click(`#service-list [data-service="checkout"]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("open checkout from grouped list: %v", err)
	}
	requireJS(t, ctx, `
		document.querySelector("#inspector-title").textContent.trim() === "checkout" &&
		!document.querySelector("#inspector-tabs").hidden &&
		document.querySelectorAll("#inspector-body .host-chip").length === 2
	`, 5*time.Second)
	// Focus and activate the chip in one evaluation. A live snapshot can
	// re-render the inspector body between two round-trips; the replaced
	// chip then no longer holds focus and a click on document.activeElement
	// lands on <body>.
	requireJS(t, ctx, `
		(() => {
			const chip = document.querySelector('#inspector-body .host-chip[data-host="node-b"]');
			if (!chip) return false;
			chip.focus();
			if (document.activeElement !== chip) return false;
			chip.click();
			return true;
		})()
	`, 5*time.Second)
	requireJS(t, ctx, `
		document.querySelector("#inspector-title").textContent.trim() === "node-b" &&
		!!document.querySelector('#inspector-body .dependency-row[data-service="checkout"]')
	`, 5*time.Second)
	// A fresh load on a phone-sized viewport: the reload cancels in-flight
	// polling requests, which is expected and scoped to this sub-phase.
	smoke.phase("host-group-reload")
	if err := chromedp.Run(ctx, chromedp.EmulateViewport(390, 844)); err != nil {
		t.Fatalf("set mobile viewport for host grouping: %v", err)
	}
	if err := chromedp.Run(ctx, chromedp.Navigate(app.baseURL()+"/?group=host")); err != nil {
		t.Fatalf("reload host grouping on mobile: %v", err)
	}
	requireJS(t, ctx, `document.readyState === "complete" && !!document.querySelector("#host-group-button")`, 10*time.Second)
	smoke.phase("host-group")
	requireJS(t, ctx, `
		matchMedia("(max-width: 767px)").matches &&
		document.querySelector("#host-group-button").getAttribute("aria-pressed") === "true" &&
		!document.querySelector("#mobile-list").hidden &&
		document.querySelectorAll("#mobile-list .host-heading").length === 3 &&
		document.documentElement.scrollWidth <= document.documentElement.clientWidth &&
		document.body.scrollWidth <= document.body.clientWidth
	`, 15*time.Second)
	smoke.screenshot("host-group-mobile")
	if err := chromedp.Run(ctx, chromedp.Click(`#mobile-list .host-heading[data-host="node-a"]`, chromedp.ByQuery)); err != nil {
		t.Fatalf("open mobile host panel: %v", err)
	}
	requireJS(t, ctx, `
		!document.querySelector("#inspector").inert &&
		document.querySelector("#inspector-title").textContent.trim() === "node-a" &&
		document.querySelectorAll("#inspector-body .dependency-row").length === 2 &&
		document.documentElement.scrollWidth <= document.documentElement.clientWidth &&
		document.body.scrollWidth <= document.body.clientWidth
	`, 10*time.Second)
	smoke.screenshot("host-panel-mobile")
	smoke.complete("host-group")

	smoke.validateInventory()
	assertNoUnexpectedBrowserEvents(t, recorder)
}

func TestLatencyLabels(t *testing.T) {
	if strings.TrimSpace(os.Getenv("OTELCONTEXT_BROWSER_CASE")) != "latency" {
		t.Skip("set OTELCONTEXT_BROWSER_CASE=latency")
	}
	binary := requiredBinary(t)
	chrome := requiredChrome(t)
	app := newAppProcess(t, binary)
	if err := app.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.stop() })
	readyCtx, readyCancel := context.WithTimeout(context.Background(), readyTimeout)
	if err := waitReady(readyCtx, app.baseURL()); err != nil {
		readyCancel()
		t.Fatalf("application did not become ready: %v\n%s", err, app.log.Bytes())
	}
	readyCancel()

	allocatorOptions := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	allocatorOptions = append(allocatorOptions, chromedp.ExecPath(chrome), chromedp.Flag("disable-dev-shm-usage", true))
	if os.Geteuid() == 0 {
		allocatorOptions = append(allocatorOptions, chromedp.NoSandbox)
	}
	allocatorCtx, cancelAllocator := chromedp.NewExecAllocator(context.Background(), allocatorOptions...)
	defer cancelAllocator()
	browserCtx, cancelBrowser := chromedp.NewContext(allocatorCtx)
	defer cancelBrowser()
	ctx, cancel := context.WithTimeout(browserCtx, 60*time.Second)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate(app.baseURL()+"/")); err != nil {
		t.Fatal(err)
	}
	requireJS(t, ctx, `document.readyState === "complete" && !!document.querySelector('script[type="module"][src="/static/app.js"]')`, 10*time.Second)

	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		(async () => {
			const ui = await import("/static/app.js");
			return JSON.stringify({
				cases: [
					ui.formatP99(1000, {p99:{status:"measured",method:"ordered_rank",sample_count:1000}}),
					ui.formatP99(980, {p99:{status:"approximate",method:"ddsketch",sample_count:1000,relative_error_bound:0.0217}}),
					ui.formatP99(52, {p99:{status:"estimated",method:"average_multiplier",sample_count:1000,estimate_factor:2.5}}),
					ui.formatP99(1000, {p99:{status:"bounded",method:"retained_prefix",sample_count:1000,population_count:1001,sample_limit:1000}}),
					ui.formatP99(0, {p99:{status:"unavailable",reason:"no_observations"}}),
					ui.formatP99(42, null),
					ui.formatP99(10, {p99:{status:"measured",method:"ordered_rank",sample_count:99,low_sample:true}})
				],
				estimatedPulse: ui.formatPulseLatency(
					{avg_latency_ms:21},
					{p99_latency_ms:52,latency_provenance:{p99:{status:"estimated",method:"average_multiplier",sample_count:1000,estimate_factor:2.5}}}
				),
				averagePulse: ui.formatPulseLatency({avg_latency_ms:21}, {})
			});
		})()
	`, &raw, func(params *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
		return params.WithAwaitPromise(true)
	})); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Cases []struct {
			Label       string `json:"label"`
			Value       string `json:"value"`
			Explanation string `json:"explanation"`
		} `json:"cases"`
		EstimatedPulse struct {
			Label string `json:"label"`
			Value string `json:"value"`
		} `json:"estimatedPulse"`
		AveragePulse struct {
			Label string `json:"label"`
			Value string `json:"value"`
		} `json:"averagePulse"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	wantLabels := []string{"P99", "Approx. p99", "Estimated tail", "Sample p99", "P99 unavailable", "Reported p99", "P99"}
	for i, label := range wantLabels {
		if got.Cases[i].Label != label {
			t.Fatalf("case %d label=%q, want %q", i, got.Cases[i].Label, label)
		}
	}
	if got.Cases[2].Value != "~52ms" || got.Cases[4].Value != "—" || !strings.Contains(got.Cases[1].Explanation, "±2.2%") || !strings.Contains(got.Cases[6].Explanation, "low sample") {
		t.Fatalf("formatter cases = %+v", got.Cases)
	}
	if got.EstimatedPulse.Label != "Estimated tail" || got.EstimatedPulse.Value != "~52ms" || got.AveragePulse.Label != "Average" || got.AveragePulse.Value != "21ms" {
		t.Fatalf("pulse cases = estimated %+v average %+v", got.EstimatedPulse, got.AveragePulse)
	}
}
