//go:build shutdownproof && !windows

package shutdownproof_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

	"github.com/RandomCodeSpace/otelcontext/internal/ingest"
	"github.com/RandomCodeSpace/otelcontext/internal/queue"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	_ "github.com/glebarez/go-sqlite"
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
)

const (
	traceFixtureID = "0102030405060708090a0b0c0d0e0f10"
	spanFixtureID  = "1112131415161718"
	logFixtureBody = "shutdown proof payment failed for order 4242"
	metricFixture  = "shutdown_proof_metric"
)

var requiredOwners = []string{
	"otlp_admission", "http", "pprof", "realtime", "ai",
	"ingest_pipeline", "aggregate_writer", "tsdb", "graphrag",
	"service_graph", "dlq", "disk_watchdog", "retention",
	"partitions", "tracer", "database_health", "boot_workers",
	"aggregate_store", "main_database",
}

type lockedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.data = append(b.data, p...)
	b.mu.Unlock()
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}

type appProcess struct {
	binary        string
	dir           string
	mode          string
	httpPort      int
	grpcPort      int
	log           *lockedBuffer
	cmd           *exec.Cmd
	done          chan error
	mainDBPath    string
	aggregatePath string
	dlqPath       string
}

func newAppProcess(t *testing.T, binary, mode string) *appProcess {
	t.Helper()
	dir := t.TempDir()
	return &appProcess{
		binary:        binary,
		dir:           dir,
		mode:          mode,
		httpPort:      freePort(t),
		grpcPort:      freePort(t),
		log:           &lockedBuffer{},
		mainDBPath:    filepath.Join(dir, "otelcontext.db"),
		aggregatePath: filepath.Join(dir, "aggregate.db"),
		dlqPath:       filepath.Join(dir, "dlq"),
	}
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
	overrides := map[string]string{
		"AGGREGATE_ALLOW_REBUILD":    "false",
		"AGGREGATE_DB_PATH":          a.aggregatePath,
		"AGGREGATE_MODE":             a.mode,
		"API_KEY":                    "",
		"API_TENANT_KEYS_FILE":       "",
		"APP_ENV":                    "development",
		"DATA_DISK_BUDGET_MB":        "1000000",
		"DATA_DISK_PATH":             a.dir,
		"DB_AUTOMIGRATE":             "true",
		"DB_DRIVER":                  "sqlite",
		"DB_DSN":                     a.mainDBPath,
		"DLQ_PATH":                   a.dlqPath,
		"DLQ_REPLAY_INTERVAL":        "1h",
		"GRAPHRAG_EVENT_QUEUE_SIZE":  "64",
		"GRAPHRAG_WORKER_COUNT":      "1",
		"GRPC_PORT":                  strconv.Itoa(a.grpcPort),
		"HTTP_PORT":                  strconv.Itoa(a.httpPort),
		"INGEST_ASYNC_ENABLED":       "true",
		"INGEST_PIPELINE_QUEUE_SIZE": "64",
		"INGEST_PIPELINE_WORKERS":    "1",
		"INGEST_MIN_SEVERITY":        "INFO",
		"LOG_LEVEL":                  "INFO",
		"PPROF_ADDR":                 "",
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
	a.cmd = cmd
	a.done = done
	return nil
}

func (a *appProcess) stop() (int, error) {
	cmd, done := a.cmd, a.done
	a.cmd, a.done = nil, nil
	if cmd == nil || cmd.Process == nil {
		return 0, nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return -1, err
	}
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
	case <-time.After(35 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return -1, errors.New("shutdown exceeded 35 seconds and was killed")
	}
}

func (a *appProcess) waitReady(ctx context.Context) error {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://127.0.0.1:"+strconv.Itoa(a.httpPort)+"/ready", nil)
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
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func resource(service string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
		Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}},
	}}}
}

func sendFixtures(ctx context.Context, grpcPort int) error {
	conn, err := grpc.NewClient("127.0.0.1:"+strconv.Itoa(grpcPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	now := uint64(time.Now().UnixNano())
	traceID, _ := hex.DecodeString(traceFixtureID)
	spanID, _ := hex.DecodeString(spanFixtureID)

	if _, err := collecttracepb.NewTraceServiceClient(conn).Export(ctx, &collecttracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: resource("shutdown-proof"),
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
				TraceId: traceID, SpanId: spanID, Name: "last-accepted-trace",
				StartTimeUnixNano: now, EndTimeUnixNano: now + uint64(time.Millisecond),
				Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
			}}}},
		}},
	}); err != nil {
		return fmt.Errorf("export trace: %w", err)
	}

	if _, err := collectlogspb.NewLogsServiceClient(conn).Export(ctx, &collectlogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: resource("shutdown-proof"),
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
				TimeUnixNano: now, ObservedTimeUnixNano: now,
				SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR,
				SeverityText:   "ERROR",
				Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: logFixtureBody}},
				TraceId:        traceID, SpanId: spanID,
			}}}},
		}},
	}); err != nil {
		return fmt.Errorf("export log: %w", err)
	}

	if _, err := collectmetricspb.NewMetricsServiceClient(conn).Export(ctx, &collectmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: resource("shutdown-proof"),
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
				Name: metricFixture,
				Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{{
					TimeUnixNano: now,
					Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 42},
				}}}},
			}}}},
		}},
	}); err != nil {
		return fmt.Errorf("export metric: %w", err)
	}
	return nil
}

type fingerprint struct {
	Traces             int64 `json:"traces"`
	Spans              int64 `json:"spans"`
	Logs               int64 `json:"logs"`
	MetricBuckets      int64 `json:"metric_buckets"`
	DrainTemplates     int64 `json:"drain_templates"`
	AggregateDeltas    int64 `json:"aggregate_deltas"`
	AggregateTemplates int64 `json:"aggregate_templates"`
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return count
}

func readFingerprint(t *testing.T, app *appProcess) fingerprint {
	t.Helper()
	mainDB, err := sql.Open("sqlite", app.mainDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mainDB.Close() }()
	fp := fingerprint{
		Traces: countRows(t, mainDB, "SELECT COUNT(*) FROM traces WHERE trace_id = ?", traceFixtureID),
		Spans:  countRows(t, mainDB, "SELECT COUNT(*) FROM spans WHERE span_id = ?", spanFixtureID),
		Logs:   countRows(t, mainDB, "SELECT COUNT(*) FROM logs WHERE body = ?", logFixtureBody),
	}
	if app.mode != "aggregate" {
		fp.MetricBuckets = countRows(t, mainDB, "SELECT COUNT(*) FROM metric_buckets WHERE name = ?", metricFixture)
	}
	if app.mode == "legacy" {
		fp.DrainTemplates = countRows(t, mainDB, "SELECT COUNT(*) FROM drain_templates")
		return fp
	}
	aggregateDB, err := sql.Open("sqlite", app.aggregatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = aggregateDB.Close() }()
	fp.AggregateDeltas = countRows(t, aggregateDB, "SELECT COUNT(*) FROM aggregate_delta_log")
	fp.AggregateTemplates = countRows(t, aggregateDB, "SELECT COUNT(*) FROM aggregate_log_template")
	return fp
}

type runtimeStep struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

type runtimeReport struct {
	StartedAt   time.Time     `json:"started_at"`
	CompletedAt time.Time     `json:"completed_at"`
	Steps       []runtimeStep `json:"steps"`
}

func latestShutdownReport(t *testing.T, output string) runtimeReport {
	t.Helper()
	var latest runtimeReport
	found := false
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "msg=shutdown_complete") {
			continue
		}
		idx := strings.Index(line, " proof=")
		if idx < 0 {
			t.Fatalf("shutdown_complete missing proof: %s", line)
		}
		raw, err := strconv.Unquote(strings.TrimSpace(line[idx+len(" proof="):]))
		if err != nil {
			t.Fatalf("unquote shutdown proof: %v", err)
		}
		if err := json.Unmarshal([]byte(raw), &latest); err != nil {
			t.Fatalf("decode shutdown proof: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatal("no shutdown_complete record")
	}
	return latest
}

func assertReport(t *testing.T, report runtimeReport) {
	t.Helper()
	if report.StartedAt.IsZero() || report.CompletedAt.IsZero() {
		t.Fatalf("shutdown report lacks timestamps: %#v", report)
	}
	if len(report.Steps) != len(requiredOwners) {
		t.Fatalf("shutdown owners = %d, want %d: %#v", len(report.Steps), len(requiredOwners), report.Steps)
	}
	for i, owner := range requiredOwners {
		if report.Steps[i].Name != owner || report.Steps[i].Error != "" {
			t.Fatalf("shutdown step %d = %#v, want %q without error", i, report.Steps[i], owner)
		}
	}
}

func fileInventory(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		out[entry.Name()] = hex.EncodeToString(sum[:])
	}
	return out
}

func seedDLQ(t *testing.T, dir string) map[string]string {
	t.Helper()
	q, err := queue.NewDLQ(dir, time.Hour, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ingest.DLQBatchEnvelope{
		Type: ingest.DLQBatchType,
		Data: ingest.DLQBatchPayload{
			Tenant: storage.DefaultTenantID,
			Signal: "logs",
			Logs:   []storage.Log{{ServiceName: "shutdown-proof", Body: "pending shutdown DLQ envelope"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := q.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	return fileInventory(t, dir)
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type assertion struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

type proofArtifact struct {
	SchemaVersion   string            `json:"schema_version"`
	Mode            string            `json:"mode"`
	CandidateSHA    string            `json:"candidate_sha,omitempty"`
	BinarySHA256    string            `json:"binary_sha256"`
	FixtureIDs      map[string]string `json:"fixture_ids"`
	FirstExitCode   int               `json:"first_exit_code"`
	RestartExitCode int               `json:"restart_exit_code"`
	FirstShutdown   runtimeReport     `json:"first_shutdown"`
	RestartShutdown runtimeReport     `json:"restart_shutdown"`
	PreRestart      fingerprint       `json:"pre_restart_fingerprint"`
	PostRestart     fingerprint       `json:"post_restart_fingerprint"`
	DLQBefore       map[string]string `json:"dlq_before"`
	DLQAfter        map[string]string `json:"dlq_after"`
	Assertions      []assertion       `json:"assertions"`
}

func TestShutdownModeProof(t *testing.T) {
	binary := os.Getenv("OTELCONTEXT_SHUTDOWN_BINARY")
	mode := os.Getenv("OTELCONTEXT_SHUTDOWN_PROOF_MODE")
	if binary == "" {
		t.Fatal("OTELCONTEXT_SHUTDOWN_BINARY is required")
	}
	if mode != "legacy" && mode != "aggregate-shadow" && mode != "aggregate" {
		t.Fatalf("unsupported proof mode %q", mode)
	}
	app := newAppProcess(t, binary, mode)
	t.Cleanup(func() {
		if app.cmd != nil {
			_, _ = app.stop()
		}
	})
	dlqBefore := seedDLQ(t, app.dlqPath)
	if len(dlqBefore) != 1 {
		t.Fatalf("DLQ seed files = %d, want 1", len(dlqBefore))
	}

	if err := app.start(); err != nil {
		t.Fatal(err)
	}
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 20*time.Second)
	if err := app.waitReady(readyCtx); err != nil {
		cancelReady()
		t.Fatalf("first readiness: %v\n%s", err, app.log.String())
	}
	cancelReady()
	exportCtx, cancelExport := context.WithTimeout(context.Background(), 10*time.Second)
	if err := sendFixtures(exportCtx, app.grpcPort); err != nil {
		cancelExport()
		t.Fatal(err)
	}
	cancelExport()
	firstExit, err := app.stop()
	if err != nil || firstExit != 0 {
		t.Fatalf("first shutdown exit=%d err=%v\n%s", firstExit, err, app.log.String())
	}
	firstReport := latestShutdownReport(t, app.log.String())
	assertReport(t, firstReport)
	preRestart := readFingerprint(t, app)
	if preRestart.Traces != 1 || preRestart.Spans != 1 || preRestart.Logs != 1 {
		t.Fatalf("raw fixture fingerprint = %#v", preRestart)
	}
	if mode == "legacy" || mode == "aggregate-shadow" {
		if preRestart.MetricBuckets != 1 {
			t.Fatalf("legacy metric did not survive shutdown: %#v", preRestart)
		}
	}
	if mode == "legacy" {
		if preRestart.DrainTemplates == 0 {
			t.Fatalf("GraphRAG template did not survive shutdown: %#v", preRestart)
		}
	} else if preRestart.AggregateDeltas == 0 || preRestart.AggregateTemplates == 0 {
		t.Fatalf("aggregate window/template did not survive shutdown: %#v", preRestart)
	}

	if err := app.start(); err != nil {
		t.Fatal(err)
	}
	restartReadyCtx, cancelRestartReady := context.WithTimeout(context.Background(), 20*time.Second)
	if err := app.waitReady(restartReadyCtx); err != nil {
		cancelRestartReady()
		t.Fatalf("restart readiness: %v\n%s", err, app.log.String())
	}
	cancelRestartReady()
	restartExit, err := app.stop()
	if err != nil || restartExit != 0 {
		t.Fatalf("restart shutdown exit=%d err=%v\n%s", restartExit, err, app.log.String())
	}
	restartReport := latestShutdownReport(t, app.log.String())
	assertReport(t, restartReport)
	postRestart := readFingerprint(t, app)
	if !reflect.DeepEqual(preRestart, postRestart) {
		t.Fatalf("restart fingerprint changed: pre=%#v post=%#v", preRestart, postRestart)
	}
	dlqAfter := fileInventory(t, app.dlqPath)
	if !reflect.DeepEqual(dlqBefore, dlqAfter) {
		t.Fatalf("DLQ inventory changed: before=%v after=%v", dlqBefore, dlqAfter)
	}
	output := app.log.String()
	if got := strings.Count(output, "msg=shutdown_complete"); got != 2 {
		t.Fatalf("shutdown_complete count = %d, want 2", got)
	}
	if strings.Contains(output, "msg=shutdown_failed") {
		t.Fatal("shutdown_failed record present")
	}

	artifact := proofArtifact{
		SchemaVersion: "otelcontext.shutdown-proof.v1",
		Mode:          mode,
		CandidateSHA:  os.Getenv("GITHUB_SHA"),
		BinarySHA256:  sha256File(t, binary),
		FixtureIDs: map[string]string{
			"trace_id": traceFixtureID, "span_id": spanFixtureID,
			"log_body": logFixtureBody, "metric_name": metricFixture,
		},
		FirstExitCode:   firstExit,
		RestartExitCode: restartExit,
		FirstShutdown:   firstReport,
		RestartShutdown: restartReport,
		PreRestart:      preRestart,
		PostRestart:     postRestart,
		DLQBefore:       dlqBefore,
		DLQAfter:        dlqAfter,
		Assertions: []assertion{
			{Name: "first_exit_zero", Passed: firstExit == 0},
			{Name: "restart_exit_zero", Passed: restartExit == 0},
			{Name: "all_shutdown_owners_completed", Passed: true},
			{Name: "last_trace_persisted", Passed: preRestart.Traces == 1},
			{Name: "last_span_persisted", Passed: preRestart.Spans == 1},
			{Name: "last_log_persisted", Passed: preRestart.Logs == 1},
			{Name: "mode_owned_metric_persisted", Passed: preRestart.MetricBuckets == 1 || preRestart.AggregateDeltas > 0},
			{Name: "mode_owned_template_persisted", Passed: preRestart.DrainTemplates > 0 || preRestart.AggregateTemplates > 0},
			{Name: "restart_fingerprint_equal", Passed: reflect.DeepEqual(preRestart, postRestart)},
			{Name: "dlq_inventory_equal", Passed: reflect.DeepEqual(dlqBefore, dlqAfter)},
			{Name: "no_failure_record", Passed: !strings.Contains(output, "msg=shutdown_failed")},
		},
	}
	proofDir := os.Getenv("OTELCONTEXT_SHUTDOWN_PROOF_DIR")
	if proofDir != "" {
		if err := os.MkdirAll(proofDir, 0o750); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(artifact, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proofDir, mode+".json"), append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proofDir, mode+".log"), []byte(output), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
