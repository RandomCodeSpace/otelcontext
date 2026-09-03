//go:build dbcontract && !windows

package dbcontract_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/coder/websocket"
	collectlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collecttracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

//go:embed testdata/lifecycle-v1.json
var fixtureJSON []byte

const proofSchema = "otelcontext.database-proof.v1"

type fixture struct {
	SchemaVersion string          `json:"schema_version"`
	Services      []string        `json:"services"`
	Traces        []fixtureTrace  `json:"traces"`
	Logs          []fixtureLog    `json:"logs"`
	Metrics       []fixtureMetric `json:"metrics"`
	Stale         staleFixture    `json:"stale"`
}

type fixtureTrace struct {
	TraceID   string        `json:"trace_id"`
	Transport string        `json:"transport"`
	Spans     []fixtureSpan `json:"spans"`
}

type fixtureSpan struct {
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	Service      string `json:"service"`
	Operation    string `json:"operation"`
	Status       string `json:"status"`
}

type fixtureLog struct {
	Service   string `json:"service"`
	Body      string `json:"body"`
	Transport string `json:"transport"`
	TraceID   string `json:"trace_id"`
	SpanID    string `json:"span_id"`
}

type fixtureMetric struct {
	Service   string  `json:"service"`
	Name      string  `json:"name"`
	Transport string  `json:"transport"`
	Value     float64 `json:"value"`
}

type staleFixture struct {
	Service    string `json:"service"`
	TraceID    string `json:"trace_id"`
	SpanID     string `json:"span_id"`
	LogBody    string `json:"log_body"`
	MetricName string `json:"metric_name"`
}

type lockedBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.String()
}

type appProcess struct {
	binary      string
	driver      string
	dsn         string
	dir         string
	httpPort    int
	grpcPort    int
	autoMigrate bool
	log         *lockedBuffer
	cmd         *exec.Cmd
	done        chan error
}

func newAppProcess(t *testing.T, binary, driver, dsn string) *appProcess {
	t.Helper()
	return &appProcess{
		binary:      binary,
		driver:      driver,
		dsn:         dsn,
		dir:         t.TempDir(),
		httpPort:    freePort(t),
		grpcPort:    freePort(t),
		autoMigrate: false,
		log:         &lockedBuffer{},
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func (a *appProcess) environment() []string {
	overrides := map[string]string{
		"AGGREGATE_ALLOW_REBUILD":    "false",
		"AGGREGATE_DB_PATH":          filepath.Join(a.dir, "aggregate.db"),
		"AGGREGATE_MODE":             "legacy",
		"API_KEY":                    "",
		"API_TENANT_KEYS_FILE":       "",
		"APP_ENV":                    "development",
		"DATA_DISK_BUDGET_MB":        "1000000",
		"DATA_DISK_PATH":             a.dir,
		"DB_AUTOMIGRATE":             strconv.FormatBool(a.autoMigrate),
		"DB_DRIVER":                  a.driver,
		"DB_DSN":                     a.dsn,
		"DLQ_PATH":                   filepath.Join(a.dir, "dlq"),
		"DLQ_REPLAY_INTERVAL":        "1h",
		"EXEMPLAR_RETENTION_DAYS":    "1",
		"GRAPHRAG_EVENT_QUEUE_SIZE":  "128",
		"GRAPHRAG_WORKER_COUNT":      "1",
		"GRPC_PORT":                  strconv.Itoa(a.grpcPort),
		"HOT_RETENTION_DAYS":         "1",
		"HTTP_PORT":                  strconv.Itoa(a.httpPort),
		"INGEST_ASYNC_ENABLED":       "true",
		"INGEST_PIPELINE_QUEUE_SIZE": "256",
		"INGEST_PIPELINE_WORKERS":    "1",
		"INGEST_MIN_SEVERITY":        "INFO",
		"LOG_LEVEL":                  "INFO",
		"PPROF_ADDR":                 "",
		"RETENTION_BATCH_SIZE":       "100",
		"RETENTION_BATCH_SLEEP_MS":   "0",
		"SAMPLING_RATE":              "1.0",
		"STORE_MIN_SEVERITY":         "INFO",
		"TLS_AUTO_SELFSIGNED":        "false",
		"TLS_CERT_FILE":              "",
		"TLS_KEY_FILE":               "",
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
	if a.cmd != nil {
		return errors.New("application already running")
	}
	command := a.command()
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
		close(done)
	}()
	a.cmd = command
	a.done = done
	return nil
}

func (a *appProcess) command(arguments ...string) *exec.Cmd {
	command := exec.Command(a.binary, arguments...)
	command.Dir, command.Env = a.dir, a.environment()
	command.Stdout, command.Stderr = a.log, a.log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return command
}

func (a *appProcess) stop() (int, error) {
	command, done := a.cmd, a.done
	a.cmd, a.done = nil, nil
	if command == nil || command.Process == nil {
		return 0, nil
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM); err != nil {
		return -1, err
	}
	return waitForExit(command, done)
}

func waitForExit(command *exec.Cmd, done <-chan error) (int, error) {
	timer := time.NewTimer(35 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			return 0, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), err
		}
		return -1, err
	case <-timer.C:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		return -1, errors.New("shutdown exceeded 35 seconds")
	}
}

func (a *appProcess) waitReady(ctx context.Context) error {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL()+"/ready", nil)
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (a *appProcess) baseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(a.httpPort)
}

type oneShotResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Elapsed  time.Duration
}

func runOneShot(t *testing.T, app *appProcess, args ...string) oneShotResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, app.binary, args...)
	command.Dir = app.dir
	command.Env = app.environment()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	started := time.Now()
	err := command.Run()
	result := oneShotResult{Stdout: stdout.String(), Stderr: stderr.String(), Elapsed: time.Since(started)}
	if err == nil {
		return result
	}
	if ctx.Err() != nil {
		t.Fatalf("%s timed out: %v\n%s", strings.Join(args, " "), ctx.Err(), stderr.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("%s failed: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	result.ExitCode = exitErr.ExitCode()
	return result
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	var value fixture
	if err := json.Unmarshal(fixtureJSON, &value); err != nil {
		t.Fatal(err)
	}
	if value.SchemaVersion != "otelcontext.db-lifecycle-fixture.v1" || len(value.Services) != 5 || len(value.Traces) != 2 || len(value.Logs) != 6 || len(value.Metrics) != 6 {
		t.Fatalf("fixture contract changed: %#v", value)
	}
	spanCount, errorsCount := 0, 0
	transports := map[string]map[string]bool{"trace": {}, "log": {}, "metric": {}}
	for _, trace := range value.Traces {
		transports["trace"][trace.Transport] = true
		spanCount += len(trace.Spans)
		for _, span := range trace.Spans {
			if span.Status == "error" {
				errorsCount++
			}
		}
	}
	for _, record := range value.Logs {
		transports["log"][record.Transport] = true
	}
	for _, metric := range value.Metrics {
		transports["metric"][metric.Transport] = true
	}
	if spanCount != 6 || errorsCount != 2 {
		t.Fatalf("fixture spans=%d errors=%d, want 6 and 2", spanCount, errorsCount)
	}
	for signal, seen := range transports {
		if !seen["grpc"] || !seen["http"] {
			t.Fatalf("fixture %s does not exercise both transports: %v", signal, seen)
		}
	}
	return value
}

func resource(service string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
		Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}},
	}}}
}

func decodeHex(value string) []byte {
	decoded, _ := hex.DecodeString(value)
	return decoded
}

func traceRequest(fixture fixture, transport string, anchor time.Time) *collecttracepb.ExportTraceServiceRequest {
	request := &collecttracepb.ExportTraceServiceRequest{}
	for traceIndex, trace := range fixture.Traces {
		if trace.Transport != transport {
			continue
		}
		for spanIndex, span := range trace.Spans {
			started := anchor.Add(time.Duration(traceIndex*10+spanIndex) * time.Millisecond)
			status := tracepb.Status_STATUS_CODE_UNSET
			if span.Status == "error" {
				status = tracepb.Status_STATUS_CODE_ERROR
			}
			request.ResourceSpans = append(request.ResourceSpans, &tracepb.ResourceSpans{
				Resource: resource(span.Service),
				ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
					TraceId: decodeHex(trace.TraceID), SpanId: decodeHex(span.SpanID), ParentSpanId: decodeHex(span.ParentSpanID),
					Name: span.Operation, StartTimeUnixNano: uint64(started.UnixNano()), EndTimeUnixNano: uint64(started.Add(10 * time.Millisecond).UnixNano()),
					Status: &tracepb.Status{Code: status},
				}}}},
			})
		}
	}
	return request
}

func logsRequest(fixture fixture, transport string, anchor time.Time) *collectlogspb.ExportLogsServiceRequest {
	request := &collectlogspb.ExportLogsServiceRequest{}
	for index, record := range fixture.Logs {
		if record.Transport != transport {
			continue
		}
		when := uint64(anchor.Add(time.Duration(index) * time.Millisecond).UnixNano())
		request.ResourceLogs = append(request.ResourceLogs, &logspb.ResourceLogs{
			Resource: resource(record.Service),
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
				TimeUnixNano: when, ObservedTimeUnixNano: when,
				SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, SeverityText: "ERROR",
				Body:    &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: record.Body}},
				TraceId: decodeHex(record.TraceID), SpanId: decodeHex(record.SpanID),
			}}}},
		})
	}
	return request
}

func metricsRequest(fixture fixture, transport string, anchor time.Time) *collectmetricspb.ExportMetricsServiceRequest {
	request := &collectmetricspb.ExportMetricsServiceRequest{}
	for index, metric := range fixture.Metrics {
		if metric.Transport != transport {
			continue
		}
		request.ResourceMetrics = append(request.ResourceMetrics, &metricspb.ResourceMetrics{
			Resource: resource(metric.Service),
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
				Name: metric.Name,
				Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
					TimeUnixNano: uint64(anchor.Add(time.Duration(index) * time.Millisecond).UnixNano()),
					Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: metric.Value},
				}}}},
			}}}},
		})
	}
	return request
}

func staleRequests(value staleFixture, anchor time.Time) (*collecttracepb.ExportTraceServiceRequest, *collectlogspb.ExportLogsServiceRequest, *collectmetricspb.ExportMetricsServiceRequest) {
	when := uint64(anchor.UnixNano())
	traceID, spanID := decodeHex(value.TraceID), decodeHex(value.SpanID)
	traces := &collecttracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: resource(value.Service), ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId: traceID, SpanId: spanID, Name: "stale-operation", StartTimeUnixNano: when, EndTimeUnixNano: when + uint64(time.Millisecond),
		}}}},
	}}}
	logs := &collectlogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: resource(value.Service), ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: when, ObservedTimeUnixNano: when, SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
			SeverityText: "INFO", Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value.LogBody}},
			TraceId: traceID, SpanId: spanID,
		}}}},
	}}}
	metrics := &collectmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: resource(value.Service), ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: value.MetricName, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
				TimeUnixNano: when, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: 1},
			}}}},
		}}}},
	}}}
	return traces, logs, metrics
}

func exportFixtures(ctx context.Context, app *appProcess, value fixture, anchor time.Time) error {
	connection, err := grpc.NewClient("127.0.0.1:"+strconv.Itoa(app.grpcPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	if _, err := collecttracepb.NewTraceServiceClient(connection).Export(ctx, traceRequest(value, "grpc", anchor)); err != nil {
		return fmt.Errorf("gRPC traces: %w", err)
	}
	if _, err := collectlogspb.NewLogsServiceClient(connection).Export(ctx, logsRequest(value, "grpc", anchor)); err != nil {
		return fmt.Errorf("gRPC logs: %w", err)
	}
	if _, err := collectmetricspb.NewMetricsServiceClient(connection).Export(ctx, metricsRequest(value, "grpc", anchor)); err != nil {
		return fmt.Errorf("gRPC metrics: %w", err)
	}
	if err := postProto(ctx, app.baseURL()+"/v1/traces", traceRequest(value, "http", anchor)); err != nil {
		return err
	}
	if err := postProto(ctx, app.baseURL()+"/v1/logs", logsRequest(value, "http", anchor)); err != nil {
		return err
	}
	return postProto(ctx, app.baseURL()+"/v1/metrics", metricsRequest(value, "http", anchor))
}

func exportStale(ctx context.Context, app *appProcess, value staleFixture, anchor time.Time) error {
	connection, err := grpc.NewClient("127.0.0.1:"+strconv.Itoa(app.grpcPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	traces, logs, metrics := staleRequests(value, anchor)
	if _, err := collecttracepb.NewTraceServiceClient(connection).Export(ctx, traces); err != nil {
		return err
	}
	if _, err := collectlogspb.NewLogsServiceClient(connection).Export(ctx, logs); err != nil {
		return err
	}
	_, err = collectmetricspb.NewMetricsServiceClient(connection).Export(ctx, metrics)
	return err
}

func postProto(ctx context.Context, endpoint string, message proto.Message) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-protobuf")
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s status=%d body=%s", endpoint, response.StatusCode, payload)
	}
	return nil
}

type rowFingerprint struct {
	Traces        int64  `json:"traces"`
	Spans         int64  `json:"spans"`
	Logs          int64  `json:"logs"`
	MetricBuckets int64  `json:"metric_buckets"`
	Digest        string `json:"digest"`
}

func fingerprintRows(t *testing.T, driver, dsn string, value fixture, stale bool) rowFingerprint {
	t.Helper()
	db, err := storage.NewDatabase(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, sqlErr := db.DB(); sqlErr == nil {
		defer func() { _ = sqlDB.Close() }()
	}
	traceIDs, spanIDs, logBodies, metricNames := make([]string, 0), make([]string, 0), make([]string, 0), make([]string, 0)
	if stale {
		traceIDs = append(traceIDs, value.Stale.TraceID)
		spanIDs = append(spanIDs, value.Stale.SpanID)
		logBodies = append(logBodies, value.Stale.LogBody)
		metricNames = append(metricNames, value.Stale.MetricName)
	} else {
		for _, trace := range value.Traces {
			traceIDs = append(traceIDs, trace.TraceID)
			for _, span := range trace.Spans {
				spanIDs = append(spanIDs, span.SpanID)
			}
		}
		for _, record := range value.Logs {
			logBodies = append(logBodies, record.Body)
		}
		for _, metric := range value.Metrics {
			metricNames = append(metricNames, metric.Name)
		}
	}
	result := rowFingerprint{}
	for table, query := range map[string]struct {
		column string
		values []string
	}{
		"traces": {"trace_id", traceIDs}, "spans": {"span_id", spanIDs}, "logs": {"body", logBodies}, "metric_buckets": {"name", metricNames},
	} {
		column := query.column
		if driver == "mssql" && table == "logs" {
			// logs.body is a text column on SQL Server, which cannot be
			// compared with IN; compare its nvarchar cast instead.
			column = "CAST(body AS NVARCHAR(MAX))"
		}
		var count int64
		if err := db.Table(table).Where(column+" IN ?", query.values).Count(&count).Error; err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		switch table {
		case "traces":
			result.Traces = count
		case "spans":
			result.Spans = count
		case "logs":
			result.Logs = count
		case "metric_buckets":
			result.MetricBuckets = count
		}
	}
	digestInput := fmt.Sprintf("%d|%d|%d|%d", result.Traces, result.Spans, result.Logs, result.MetricBuckets)
	result.Digest = sha256Hex([]byte(digestInput))
	return result
}

func waitForRows(t *testing.T, driver, dsn string, value fixture, wantStale bool) rowFingerprint {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		live := fingerprintRows(t, driver, dsn, value, false)
		stale := fingerprintRows(t, driver, dsn, value, true)
		liveReady := live.Traces == 2 && live.Spans == 6 && live.Logs == 6 && live.MetricBuckets == 6
		staleReady := stale.Traces == 1 && stale.Spans == 1 && stale.Logs == 1 && stale.MetricBuckets == 1
		if liveReady && staleReady == wantStale {
			return live
		}
		if time.Now().After(deadline) {
			t.Fatalf("row contract did not stabilize: live=%#v stale=%#v want_stale=%t", live, stale, wantStale)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func waitForRawRows(t *testing.T, driver, dsn string, value fixture) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		live := fingerprintRows(t, driver, dsn, value, false)
		stale := fingerprintRows(t, driver, dsn, value, true)
		if live.Traces == 2 && live.Spans == 6 && live.Logs == 6 && stale.Traces == 1 && stale.Spans == 1 && stale.Logs == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("raw ingest did not stabilize: live=%#v stale=%#v", live, stale)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type mcpCallProof struct {
	Succeeded bool `json:"succeeded"`
	HasBody   bool `json:"has_body"`
}

type surfaceProof struct {
	RESTPaths  []string                `json:"rest_paths"`
	MCPTools   []string                `json:"mcp_tools"`
	MCPCalls   map[string]mcpCallProof `json:"mcp_calls"`
	WebSockets []string                `json:"websockets"`
}

func collectSurfaces(t *testing.T, app *appProcess, value fixture) surfaceProof {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		result, err := trySurfaces(app, value)
		if err == nil {
			return result
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("query surfaces did not stabilize: %v\n%s", lastErr, app.log.String())
	return surfaceProof{}
}

func trySurfaces(app *appProcess, value fixture) (surfaceProof, error) {
	paths := []string{
		"/ready", "/api/metadata/services", "/api/traces?limit=50", "/api/traces/" + value.Traces[0].TraceID,
		"/api/logs?limit=50", "/api/metrics?name=" + url.QueryEscape(value.Metrics[0].Name),
		"/api/metrics/dashboard", "/api/metrics/service-map", "/api/system/graph",
	}
	bodies := make(map[string][]byte, len(paths))
	for _, path := range paths {
		body, err := getBody(app.baseURL() + path)
		if err != nil {
			return surfaceProof{}, err
		}
		bodies[path] = body
	}
	for _, service := range value.Services {
		if !bytes.Contains(bodies["/api/metadata/services"], []byte(service)) || !bytes.Contains(bodies["/api/system/graph"], []byte(service)) {
			return surfaceProof{}, fmt.Errorf("service %q missing from metadata or system graph", service)
		}
	}
	for _, trace := range value.Traces {
		if !bytes.Contains(bodies["/api/traces?limit=50"], []byte(trace.TraceID)) {
			return surfaceProof{}, fmt.Errorf("trace %q missing", trace.TraceID)
		}
	}
	for _, record := range value.Logs {
		if !bytes.Contains(bodies["/api/logs?limit=50"], []byte(record.Body)) {
			return surfaceProof{}, fmt.Errorf("log %q missing", record.Body)
		}
	}
	tools, err := listMCPTools(app.baseURL() + "/mcp")
	if err != nil {
		return surfaceProof{}, err
	}
	wantTools := []string{"get_anomaly_timeline", "get_service_health", "get_service_map", "impact_analysis", "root_cause_analysis", "search_logs", "trace_graph"}
	if !reflect.DeepEqual(tools, wantTools) {
		return surfaceProof{}, fmt.Errorf("MCP tools=%v want=%v", tools, wantTools)
	}
	calls := []struct {
		name string
		args map[string]any
	}{
		{"get_anomaly_timeline", nil},
		{"get_service_map", nil},
		{"get_service_health", map[string]any{"service_name": "payments"}},
		{"root_cause_analysis", map[string]any{"service": "payments"}},
		{"impact_analysis", map[string]any{"service": "gateway"}},
		{"trace_graph", map[string]any{"trace_id": value.Traces[0].TraceID}},
		{"search_logs", map[string]any{"service": "payments", "query": "dbcontract"}},
	}
	callProof := make(map[string]mcpCallProof, len(calls))
	for _, call := range calls {
		text, succeeded, err := callMCPTool(app.baseURL()+"/mcp", call.name, call.args)
		if err != nil {
			return surfaceProof{}, err
		}
		if !succeeded {
			return surfaceProof{}, fmt.Errorf("MCP %s failed: %s", call.name, text)
		}
		callProof[call.name] = mcpCallProof{Succeeded: true, HasBody: text != ""}
	}
	webSockets := []string{"/ws", "/ws/events"}
	if err := probeWebSocket(app.baseURL() + "/ws"); err != nil {
		return surfaceProof{}, fmt.Errorf("websocket /ws handshake: %w", err)
	}
	eventBody, err := readWebSocket(app.baseURL() + "/ws/events")
	if err != nil {
		return surfaceProof{}, fmt.Errorf("websocket /ws/events: %w", err)
	}
	if !containsAny(eventBody, value.Services) {
		return surfaceProof{}, fmt.Errorf("websocket /ws/events omitted fixture services: %s", eventBody)
	}
	return surfaceProof{RESTPaths: paths, MCPTools: tools, MCPCalls: callProof, WebSockets: webSockets}, nil
}

func getBody(endpoint string) ([]byte, error) {
	client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil}}
	response, err := client.Get(endpoint) //nolint:gosec // exact loopback proof endpoint.
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s status=%d body=%s", endpoint, response.StatusCode, body)
	}
	return body, nil
}

func listMCPTools(endpoint string) ([]string, error) {
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := postJSONRPC(endpoint, "tools/list", nil, &result); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names, nil
}

func callMCPTool(endpoint, name string, arguments map[string]any) (string, bool, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text     string `json:"text"`
			Resource *struct {
				Text string `json:"text"`
			} `json:"resource,omitempty"`
		} `json:"content"`
	}
	params := map[string]any{"name": name, "arguments": arguments}
	if err := postJSONRPC(endpoint, "tools/call", params, &result); err != nil {
		return "", false, err
	}
	var text strings.Builder
	for _, content := range result.Content {
		text.WriteString(content.Text)
		if content.Resource != nil {
			text.WriteString(content.Resource.Text)
		}
	}
	return text.String(), !result.IsError && text.Len() > 0, nil
}

func postJSONRPC(endpoint, method string, params any, result any) error {
	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		payload["params"] = params
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(request) //nolint:gosec // exact loopback proof endpoint.
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s status=%d", method, response.StatusCode)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return fmt.Errorf("%s error: %s", method, envelope.Error)
	}
	if len(envelope.Result) == 0 {
		return fmt.Errorf("%s returned no result", method)
	}
	return json.Unmarshal(envelope.Result, result)
}

func readWebSocket(httpURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpURL, "http"), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	defer connection.Close(websocket.StatusNormalClosure, "database proof complete")
	_, body, err := connection.Read(ctx)
	return body, err
}

func probeWebSocket(httpURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpURL, "http"), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return err
	}
	return connection.Close(websocket.StatusNormalClosure, "database proof handshake complete")
}

func containsAny(body []byte, values []string) bool {
	for _, value := range values {
		if bytes.Contains(body, []byte(value)) {
			return true
		}
	}
	return false
}

func assertPostConnectEvent(t *testing.T, app *appProcess, value fixture, export func() error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(app.baseURL()+"/ws/events", "http"), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "fixture observed")
	_, _, _ = connection.Read(ctx) // The immediate snapshot is not the post-connect event.
	if err := export(); err != nil {
		t.Fatal(err)
	}
	for {
		_, body, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("post-connect WebSocket event: %v", err)
		}
		if containsAny(body, value.Services) {
			return
		}
	}
}

func engineVersion(t *testing.T, driver, dsn string) string {
	t.Helper()
	db, err := storage.NewDatabase(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, sqlErr := db.DB(); sqlErr == nil {
		defer func() { _ = sqlDB.Close() }()
	}
	query := map[string]string{
		"sqlite": "SELECT sqlite_version()", "postgres": "SHOW server_version", "mysql": "SELECT VERSION()",
		"mssql": "SELECT CONVERT(varchar(128), SERVERPROPERTY('ProductVersion'))",
	}[driver]
	var version string
	if query == "" {
		t.Fatalf("unsupported driver %q", driver)
	}
	if err := db.Raw(query).Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	prefix := map[string]string{"sqlite": "3.53.", "postgres": "16.", "mysql": "8.4.", "mssql": "16.0.4265.3"}[driver]
	if !strings.HasPrefix(version, prefix) {
		t.Fatalf("%s engine version=%q, want prefix %q", driver, version, prefix)
	}
	return version
}

type migrationProof struct {
	Before string `json:"before"`
	Up     string `json:"up"`
	After  string `json:"after"`
	State  string `json:"state"`
}

func installSchema(t *testing.T, app *appProcess, proofDir string) migrationProof {
	t.Helper()
	if app.driver == "sqlite" || app.driver == "postgres" {
		before := runOneShot(t, app, "migrate", "status")
		if before.ExitCode != 10 || !strings.Contains(before.Stdout, "state=empty") {
			t.Fatalf("initial migration status exit=%d stdout=%s stderr=%s", before.ExitCode, before.Stdout, before.Stderr)
		}
		up := runOneShot(t, app, "migrate", "up")
		if up.ExitCode != 0 || !strings.Contains(up.Stdout, "result=ready") {
			t.Fatalf("migrate up exit=%d stdout=%s stderr=%s", up.ExitCode, up.Stdout, up.Stderr)
		}
		after := runOneShot(t, app, "migrate", "status")
		if after.ExitCode != 0 || !strings.Contains(after.Stdout, "state=exact") {
			t.Fatalf("final migration status exit=%d stdout=%s stderr=%s", after.ExitCode, after.Stdout, after.Stderr)
		}
		writeProofFile(t, proofDir, "migration-before.txt", before.Stdout+before.Stderr)
		writeProofFile(t, proofDir, "migration-after.txt", after.Stdout+after.Stderr)
		return migrationProof{Before: strings.TrimSpace(before.Stdout), Up: strings.TrimSpace(up.Stdout), After: strings.TrimSpace(after.Stdout), State: "exact"}
	}
	app.autoMigrate = true
	if err := app.start(); err != nil {
		t.Fatal(err)
	}
	readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := app.waitReady(readyCtx); err != nil {
		cancel()
		t.Fatalf("preview schema install: %v\n%s", err, app.log.String())
	}
	cancel()
	if exit, err := app.stop(); err != nil || exit != 0 {
		t.Fatalf("preview schema install shutdown exit=%d err=%v\n%s", exit, err, app.log.String())
	}
	app.autoMigrate = false
	status := runOneShot(t, app, "migrate", "status")
	if status.ExitCode != 16 || !strings.Contains(status.Stdout, "state=unverified") {
		t.Fatalf("preview migration status exit=%d stdout=%s stderr=%s", status.ExitCode, status.Stdout, status.Stderr)
	}
	writeProofFile(t, proofDir, "migration-before.txt", "preview AutoMigrate install\n")
	writeProofFile(t, proofDir, "migration-after.txt", status.Stdout+status.Stderr)
	return migrationProof{Before: "preview AutoMigrate install", After: strings.TrimSpace(status.Stdout), State: "unverified-preview"}
}

type baselineRelease struct {
	Release           string `json:"release"`
	BinaryPath        string `json:"binary_path"`
	ArchiveSHA256     string `json:"archive_sha256"`
	BinarySHA256      string `json:"binary_sha256"`
	SignatureVerified bool   `json:"signature_verified"`
}

type baselineManifest struct {
	SchemaVersion string            `json:"schema_version"`
	Releases      []baselineRelease `json:"releases"`
}

type baselineProof struct {
	Release       string `json:"release"`
	ArchiveSHA256 string `json:"archive_sha256"`
	BinarySHA256  string `json:"binary_sha256"`
	DataPreserved bool   `json:"data_preserved"`
	State         string `json:"state"`
}

func proveSignedBaselines(t *testing.T, candidate *appProcess, value fixture) []baselineProof {
	t.Helper()
	manifestPath := os.Getenv("OTELCONTEXT_TEST_BASELINE_MANIFEST")
	if manifestPath == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" && (candidate.driver == "sqlite" || candidate.driver == "postgres") {
			t.Fatal("OTELCONTEXT_TEST_BASELINE_MANIFEST is required in hosted production-profile proof")
		}
		return nil
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest baselineManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != "otelcontext.signed-baselines.v1" || len(manifest.Releases) != 2 {
		t.Fatalf("invalid signed baseline manifest: %#v", manifest)
	}
	proofs := make([]baselineProof, 0, 2)
	for _, release := range manifest.Releases {
		if !release.SignatureVerified || sha256File(t, release.BinaryPath) != release.BinarySHA256 {
			t.Fatalf("signed baseline %s is not bound to its binary", release.Release)
		}
		dsn, cleanup := baselineDSN(t, candidate.driver, candidate.dsn, release.Release)
		defer cleanup()
		old := newAppProcess(t, release.BinaryPath, candidate.driver, dsn)
		old.autoMigrate = true
		if err := old.start(); err != nil {
			t.Fatal(err)
		}
		readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := old.waitReady(readyCtx); err != nil {
			cancel()
			t.Fatalf("%s baseline readiness: %v\n%s", release.Release, err, old.log.String())
		}
		cancel()
		ctx, cancelExport := context.WithTimeout(context.Background(), 10*time.Second)
		baselineFixture := value
		baselineFixture.Traces = value.Traces[:1]
		baselineFixture.Logs = value.Logs[:1]
		baselineFixture.Metrics = value.Metrics[:1]
		baselineFixture.Traces[0].Transport = "grpc"
		baselineFixture.Logs[0].Transport = "grpc"
		baselineFixture.Metrics[0].Transport = "grpc"
		if err := exportFixtures(ctx, old, baselineFixture, time.Now().UTC()); err != nil {
			cancelExport()
			t.Fatalf("seed %s baseline: %v", release.Release, err)
		}
		cancelExport()
		if exit, err := old.stop(); err != nil || exit != 0 {
			t.Fatalf("%s baseline shutdown exit=%d err=%v\n%s", release.Release, exit, err, old.log.String())
		}
		upgrade := newAppProcess(t, candidate.binary, candidate.driver, dsn)
		status := runOneShot(t, upgrade, "migrate", "status")
		if status.ExitCode != 11 || !strings.Contains(status.Stdout, "state=unmanaged") {
			t.Fatalf("%s candidate pre-baseline status exit=%d stdout=%s stderr=%s", release.Release, status.ExitCode, status.Stdout, status.Stderr)
		}
		baseline := runOneShot(t, upgrade, "migrate", "baseline", "--from", release.Release)
		if baseline.ExitCode != 0 {
			t.Fatalf("%s baseline exit=%d stdout=%s stderr=%s", release.Release, baseline.ExitCode, baseline.Stdout, baseline.Stderr)
		}
		up := runOneShot(t, upgrade, "migrate", "up")
		if up.ExitCode != 0 || !strings.Contains(up.Stdout, "state=exact") {
			t.Fatalf("%s upgrade exit=%d stdout=%s stderr=%s", release.Release, up.ExitCode, up.Stdout, up.Stderr)
		}
		if err := upgrade.start(); err != nil {
			t.Fatal(err)
		}
		upgradeReady, cancelUpgrade := context.WithTimeout(context.Background(), 30*time.Second)
		if err := upgrade.waitReady(upgradeReady); err != nil {
			cancelUpgrade()
			t.Fatalf("%s upgraded readiness: %v\n%s", release.Release, err, upgrade.log.String())
		}
		cancelUpgrade()
		body, err := getBody(upgrade.baseURL() + "/api/traces/" + value.Traces[0].TraceID)
		preserved := err == nil && bytes.Contains(body, []byte(value.Traces[0].TraceID))
		if exit, stopErr := upgrade.stop(); stopErr != nil || exit != 0 {
			t.Fatalf("%s upgraded shutdown exit=%d err=%v", release.Release, exit, stopErr)
		}
		if !preserved {
			t.Fatalf("%s baseline fixture was not preserved: err=%v body=%s", release.Release, err, body)
		}
		proofs = append(proofs, baselineProof{Release: release.Release, ArchiveSHA256: release.ArchiveSHA256, BinarySHA256: release.BinarySHA256, DataPreserved: true, State: "exact"})
	}
	return proofs
}

func baselineDSN(t *testing.T, driver, sourceDSN, release string) (string, func()) {
	t.Helper()
	if driver == "sqlite" {
		return filepath.Join(t.TempDir(), strings.NewReplacer(".", "_", "-", "_").Replace(release)+".db"), func() {}
	}
	if driver != "postgres" {
		t.Fatalf("signed baselines are not promoted for %s", driver)
	}
	parsed, err := url.Parse(sourceDSN)
	if err != nil || parsed.Scheme == "" {
		t.Fatalf("PostgreSQL dbcontract DSN must be a URL: %q (%v)", sourceDSN, err)
	}
	name := "dbcontract_" + strings.NewReplacer(".", "_", "-", "_").Replace(strings.TrimPrefix(release, "v")) + "_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	adminURL := *parsed
	adminURL.Path = "/postgres"
	admin, err := storage.NewDatabase("postgres", adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("CREATE DATABASE \"" + name + "\"").Error; err != nil { //nolint:gosec // generated safe identifier.
		t.Fatal(err)
	}
	if sqlDB, sqlErr := admin.DB(); sqlErr == nil {
		_ = sqlDB.Close()
	}
	target := *parsed
	target.Path = "/" + name
	cleanup := func() {
		db, openErr := storage.NewDatabase("postgres", adminURL.String())
		if openErr != nil {
			return
		}
		_ = db.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = ?", name).Error
		_ = db.Exec("DROP DATABASE IF EXISTS \"" + name + "\"").Error //nolint:gosec // generated safe identifier.
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	}
	return target.String(), cleanup
}

type restoreProof struct {
	Kind                string `json:"kind"`
	SourceFingerprint   string `json:"source_fingerprint"`
	RestoredFingerprint string `json:"restored_fingerprint"`
	EvidenceSHA256      string `json:"evidence_sha256"`
}

func proveRestore(t *testing.T, source *appProcess, value fixture, sourceRows rowFingerprint, sourceSurfaces surfaceProof, proofDir string) restoreProof {
	t.Helper()
	if source.driver != "sqlite" && source.driver != "postgres" {
		path := os.Getenv("OTELCONTEXT_TEST_NATIVE_RESTORE_PROOF")
		if path == "" {
			t.Fatal("OTELCONTEXT_TEST_NATIVE_RESTORE_PROOF is required for preview adapters")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var native struct {
			Adapter    string `json:"adapter"`
			Source     string `json:"source_lifecycle_fingerprint"`
			Restored   string `json:"restored_lifecycle_fingerprint"`
			Assertions []struct {
				Passed bool `json:"passed"`
			} `json:"assertions"`
		}
		if err := json.Unmarshal(data, &native); err != nil {
			t.Fatal(err)
		}
		if native.Adapter != source.driver || native.Source == "" || native.Source != native.Restored || len(native.Assertions) < 8 {
			t.Fatalf("native restore proof mismatch: %#v", native)
		}
		for _, assertion := range native.Assertions {
			if !assertion.Passed {
				t.Fatal("native restore proof contains a failed assertion")
			}
		}
		return restoreProof{Kind: "native-fresh-target", SourceFingerprint: native.Source, RestoredFingerprint: native.Restored, EvidenceSHA256: sha256Hex(data)}
	}
	backupParent := t.TempDir()
	create := runOneShot(t, source, "backup", "create", "--out", backupParent)
	if create.ExitCode != 0 {
		t.Fatalf("backup create exit=%d stdout=%s stderr=%s", create.ExitCode, create.Stdout, create.Stderr)
	}
	var createReport struct {
		Bundle string `json:"bundle"`
	}
	if err := json.Unmarshal([]byte(create.Stdout), &createReport); err != nil || createReport.Bundle == "" {
		t.Fatalf("decode backup create: %v (%s)", err, create.Stdout)
	}
	targetDSN := os.Getenv("OTELCONTEXT_TEST_RESTORE_DSN")
	if source.driver == "sqlite" {
		targetDSN = filepath.Join(t.TempDir(), "restored.db")
	}
	if targetDSN == "" {
		t.Fatal("OTELCONTEXT_TEST_RESTORE_DSN is required for PostgreSQL")
	}
	target := newAppProcess(t, source.binary, source.driver, targetDSN)
	restore := runOneShot(t, target, "backup", "restore", "--bundle", createReport.Bundle)
	if restore.ExitCode != 0 {
		t.Fatalf("backup restore exit=%d stdout=%s stderr=%s", restore.ExitCode, restore.Stdout, restore.Stderr)
	}
	if err := target.start(); err != nil {
		t.Fatal(err)
	}
	readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := target.waitReady(readyCtx); err != nil {
		cancel()
		t.Fatalf("restored readiness: %v\n%s", err, target.log.String())
	}
	cancel()
	restoredRows := waitForRows(t, source.driver, targetDSN, value, false)
	restoredSurfaces := collectSurfaces(t, target, value)
	if exit, err := target.stop(); err != nil || exit != 0 {
		t.Fatalf("restored shutdown exit=%d err=%v\n%s", exit, err, target.log.String())
	}
	if !reflect.DeepEqual(sourceRows, restoredRows) || !reflect.DeepEqual(sourceSurfaces.MCPTools, restoredSurfaces.MCPTools) || len(restoredSurfaces.MCPCalls) != 7 {
		t.Fatalf("restored contract mismatch: source=%#v target=%#v", sourceRows, restoredRows)
	}
	manifestData, err := os.ReadFile(filepath.Join(createReport.Bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, proofDir, "post-restore-fingerprint.json", restoredRows)
	return restoreProof{Kind: "candidate-backup-cli-fresh-target", SourceFingerprint: sourceRows.Digest, RestoredFingerprint: restoredRows.Digest, EvidenceSHA256: sha256Hex(manifestData)}
}

type assertion struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

type proofArtifact struct {
	SchemaVersion  string          `json:"schema_version"`
	CandidateSHA   string          `json:"candidate_sha,omitempty"`
	BinarySHA256   string          `json:"binary_sha256"`
	Driver         string          `json:"driver"`
	Tier           string          `json:"tier"`
	EngineImage    string          `json:"engine_image"`
	EngineVersion  string          `json:"engine_version"`
	FixtureSHA256  string          `json:"fixture_sha256"`
	Migration      migrationProof  `json:"migration"`
	Baselines      []baselineProof `json:"baselines"`
	PreRestart     rowFingerprint  `json:"pre_restart_fingerprint"`
	PostRestart    rowFingerprint  `json:"post_restart_fingerprint"`
	Surfaces       surfaceProof    `json:"surfaces"`
	Restore        restoreProof    `json:"restore"`
	ElapsedSeconds float64         `json:"elapsed_seconds"`
	Assertions     []assertion     `json:"assertions"`
}

func TestDatabaseLifecycle(t *testing.T) {
	started := time.Now()
	binary := os.Getenv("OTELCONTEXT_TEST_BINARY")
	driver := strings.ToLower(strings.TrimSpace(os.Getenv("OTELCONTEXT_TEST_DRIVER")))
	dsn := os.Getenv("OTELCONTEXT_TEST_DSN")
	proofDir := os.Getenv("OTELCONTEXT_PROOF_DIR")
	requireDB := os.Getenv("OTELCONTEXT_TEST_REQUIRE_DB") == "1"
	if binary == "" || driver == "" || dsn == "" || proofDir == "" {
		t.Fatal("OTELCONTEXT_TEST_BINARY, OTELCONTEXT_TEST_DRIVER, OTELCONTEXT_TEST_DSN, and OTELCONTEXT_PROOF_DIR are required")
	}
	if !requireDB {
		t.Fatal("OTELCONTEXT_TEST_REQUIRE_DB=1 is required; lifecycle proof never auto-skips")
	}
	if driver != "sqlite" && driver != "postgres" && driver != "mysql" && driver != "mssql" {
		t.Fatalf("unsupported driver %q", driver)
	}
	if err := os.MkdirAll(proofDir, 0o750); err != nil {
		t.Fatal(err)
	}
	value := loadFixture(t)
	app := newAppProcess(t, binary, driver, dsn)
	t.Cleanup(func() {
		if app.cmd != nil {
			_, _ = app.stop()
		}
	})
	version := engineVersion(t, driver, dsn)
	migration := installSchema(t, app, proofDir)
	baselines := proveSignedBaselines(t, app, value)
	if err := app.start(); err != nil {
		t.Fatal(err)
	}
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 30*time.Second)
	if err := app.waitReady(readyCtx); err != nil {
		cancelReady()
		t.Fatalf("initial readiness: %v\n%s", err, app.log.String())
	}
	cancelReady()
	anchor := time.Now().UTC().Truncate(time.Millisecond)
	assertPostConnectEvent(t, app, value, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return exportFixtures(ctx, app, value, anchor)
	})
	ctxStale, cancelStale := context.WithTimeout(context.Background(), 10*time.Second)
	if err := exportStale(ctxStale, app, value.Stale, anchor.Add(-25*time.Hour)); err != nil {
		cancelStale()
		t.Fatal(err)
	}
	cancelStale()
	waitForRawRows(t, driver, dsn, value)
	surfaces := collectSurfaces(t, app, value)
	if exit, err := app.stop(); err != nil || exit != 0 {
		t.Fatalf("first shutdown exit=%d err=%v\n%s", exit, err, app.log.String())
	}
	preRestart := waitForRows(t, driver, dsn, value, true)
	if err := app.start(); err != nil {
		t.Fatal(err)
	}
	restartCtx, cancelRestart := context.WithTimeout(context.Background(), 30*time.Second)
	if err := app.waitReady(restartCtx); err != nil {
		cancelRestart()
		t.Fatalf("restart readiness: %v\n%s", err, app.log.String())
	}
	cancelRestart()
	postRestart := waitForRows(t, driver, dsn, value, false)
	restartSurfaces := collectSurfaces(t, app, value)
	if exit, err := app.stop(); err != nil || exit != 0 {
		t.Fatalf("restart shutdown exit=%d err=%v\n%s", exit, err, app.log.String())
	}
	if !reflect.DeepEqual(preRestart, postRestart) {
		t.Fatalf("restart fingerprint changed: before=%#v after=%#v", preRestart, postRestart)
	}
	if !reflect.DeepEqual(surfaces.MCPTools, restartSurfaces.MCPTools) || len(restartSurfaces.MCPCalls) != 7 {
		t.Fatalf("restart surface contract changed: %#v", restartSurfaces)
	}
	restore := proveRestore(t, app, value, postRestart, restartSurfaces, proofDir)
	tier := map[string]string{"postgres": "primary", "sqlite": "bounded-opt-in", "mysql": "preview", "mssql": "experimental"}[driver]
	assertions := []assertion{
		{Name: "database_required_no_skip", Passed: requireDB},
		{Name: "engine_version_pinned", Passed: version != ""},
		{Name: "migration_contract_recorded", Passed: migration.State == "exact" || migration.State == "unverified-preview"},
		{Name: "signed_baselines_proved_or_preview", Passed: len(baselines) == 2 || driver == "mysql" || driver == "mssql" || os.Getenv("GITHUB_ACTIONS") != "true"},
		{Name: "both_otlp_transports_all_signals", Passed: true},
		{Name: "five_services_two_traces_six_spans", Passed: postRestart.Traces == 2 && postRestart.Spans == 6},
		{Name: "six_logs_six_metric_series", Passed: postRestart.Logs == 6 && postRestart.MetricBuckets == 6},
		{Name: "representative_rest_complete", Passed: len(restartSurfaces.RESTPaths) >= 9},
		{Name: "both_websockets_complete", Passed: len(restartSurfaces.WebSockets) == 2},
		{Name: "seven_mcp_tools_listed_and_called", Passed: len(restartSurfaces.MCPTools) == 7 && len(restartSurfaces.MCPCalls) == 7},
		{Name: "stale_rows_purged_live_rows_retained", Passed: reflect.DeepEqual(preRestart, postRestart)},
		{Name: "graceful_restart_preserved_fingerprint", Passed: preRestart.Digest == postRestart.Digest},
		{Name: "fresh_target_restore_preserved_fingerprint", Passed: restore.SourceFingerprint == restore.RestoredFingerprint},
	}
	for _, check := range assertions {
		if !check.Passed {
			t.Fatalf("database proof assertion failed: %s", check.Name)
		}
	}
	artifact := proofArtifact{
		SchemaVersion: proofSchema, CandidateSHA: firstNonEmpty(os.Getenv("OTELCONTEXT_TEST_CANDIDATE_SHA"), os.Getenv("GITHUB_SHA")),
		BinarySHA256: sha256File(t, binary), Driver: driver, Tier: tier, EngineImage: os.Getenv("OTELCONTEXT_TEST_ENGINE_IMAGE"),
		EngineVersion: version, FixtureSHA256: sha256Hex(fixtureJSON), Migration: migration, Baselines: baselines,
		PreRestart: preRestart, PostRestart: postRestart, Surfaces: restartSurfaces, Restore: restore,
		ElapsedSeconds: time.Since(started).Seconds(), Assertions: assertions,
	}
	writeJSONFile(t, proofDir, "database-proof-v1.json", artifact)
	writeJSONFile(t, proofDir, "pre-backup-fingerprint.json", preRestart)
	writeProofFile(t, proofDir, "fixture-manifest.sha256", sha256Hex(fixtureJSON)+"  lifecycle-v1.json\n")
	logOutput := strings.ReplaceAll(app.log.String(), dsn, "[redacted-dsn]")
	writeProofFile(t, proofDir, "server.log", logOutput)
}

func writeProofFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSONFile(t *testing.T, dir, name string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeProofFile(t, dir, name, string(append(data, '\n')))
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
