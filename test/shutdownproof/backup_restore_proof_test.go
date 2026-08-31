//go:build shutdownproof && !windows

package shutdownproof_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/backup"
	"github.com/coder/websocket"
)

type oneShotResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
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
	result := oneShotResult{Stdout: stdout.String(), Stderr: stderr.String(), Duration: time.Since(started)}
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

func migrateExact(t *testing.T, app *appProcess) {
	t.Helper()
	result := runOneShot(t, app, "migrate", "up")
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "result=ready") {
		t.Fatalf("migrate up exit=%d stdout=%s stderr=%s", result.ExitCode, result.Stdout, result.Stderr)
	}
	app.autoMigrate = "false"
}

type mcpSurface struct {
	Succeeded       bool `json:"succeeded"`
	MentionsFixture bool `json:"mentions_fixture"`
}

type surfaceFingerprint struct {
	REST      map[string]string     `json:"rest_sha256"`
	MCPTools  []string              `json:"mcp_tools"`
	MCPCalls  map[string]mcpSurface `json:"mcp_calls"`
	WebSocket string                `json:"websocket_sha256"`
}

func collectSurfaceFingerprint(t *testing.T, app *appProcess) surfaceFingerprint {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		fingerprint, err := trySurfaceFingerprint(app)
		if err == nil {
			return fingerprint
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("surface fingerprint did not stabilize: %v\n%s", lastErr, app.log.String())
	return surfaceFingerprint{}
}

func trySurfaceFingerprint(app *appProcess) (surfaceFingerprint, error) {
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", app.httpPort)
	fingerprint := surfaceFingerprint{
		REST:     make(map[string]string),
		MCPCalls: make(map[string]mcpSurface),
	}
	for _, path := range []string{"/api/traces?limit=50", "/api/logs?limit=50", "/api/metadata/services"} {
		body, err := getBody(baseURL + path)
		if err != nil {
			return surfaceFingerprint{}, err
		}
		if (strings.Contains(path, "traces") || strings.Contains(path, "logs")) && !strings.Contains(string(body), "shutdown-proof") && !strings.Contains(string(body), traceFixtureID) && !strings.Contains(string(body), logFixtureBody) {
			return surfaceFingerprint{}, fmt.Errorf("REST %s does not contain the fixture", path)
		}
		canonical, err := canonicalJSON(body)
		if err != nil {
			return surfaceFingerprint{}, fmt.Errorf("canonicalize REST %s: %w", path, err)
		}
		fingerprint.REST[path] = digestBytes(canonical)
	}
	tools, err := listMCPTools(baseURL + "/mcp")
	if err != nil {
		return surfaceFingerprint{}, err
	}
	fingerprint.MCPTools = tools
	if len(tools) != 7 {
		return surfaceFingerprint{}, fmt.Errorf("MCP tools=%v, want seven", tools)
	}
	calls := []struct {
		name string
		args map[string]any
	}{
		{"get_anomaly_timeline", nil},
		{"get_service_map", nil},
		{"get_service_health", map[string]any{"service_name": "shutdown-proof"}},
		{"root_cause_analysis", map[string]any{"service": "shutdown-proof"}},
		{"impact_analysis", map[string]any{"service": "shutdown-proof"}},
		{"trace_graph", map[string]any{"trace_id": traceFixtureID}},
		{"search_logs", map[string]any{"service": "shutdown-proof", "query": "payment failed"}},
	}
	for _, call := range calls {
		text, succeeded, err := callMCPTool(baseURL+"/mcp", call.name, call.args)
		if err != nil {
			return surfaceFingerprint{}, err
		}
		if !succeeded {
			return surfaceFingerprint{}, fmt.Errorf("MCP tool %s returned an error: %s", call.name, text)
		}
		fingerprint.MCPCalls[call.name] = mcpSurface{
			Succeeded:       true,
			MentionsFixture: strings.Contains(text, "shutdown-proof") || strings.Contains(text, traceFixtureID) || strings.Contains(text, logFixtureBody),
		}
	}
	webSocketBody, err := readWebSocketSnapshot(baseURL + "/ws/events")
	if err != nil {
		return surfaceFingerprint{}, err
	}
	if !strings.Contains(string(webSocketBody), "shutdown-proof") && !strings.Contains(string(webSocketBody), traceFixtureID) {
		return surfaceFingerprint{}, errors.New("WebSocket snapshot does not contain the fixture")
	}
	canonical, err := canonicalJSON(webSocketBody)
	if err != nil {
		return surfaceFingerprint{}, err
	}
	fingerprint.WebSocket = digestBytes(canonical)
	return fingerprint, nil
}

func getBody(url string) ([]byte, error) {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	response, err := client.Get(url) //nolint:gosec // exact loopback proof endpoint.
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s status=%d body=%s", url, response.StatusCode, body)
	}
	return body, nil
}

func listMCPTools(endpoint string) ([]string, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if err != nil {
		return nil, err
	}
	response, err := http.Post(endpoint, "application/json", bytes.NewReader(body)) //nolint:gosec // exact loopback proof endpoint.
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var envelope struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("tools/list error: %v", envelope.Error)
	}
	names := make([]string, 0, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names, nil
}

func callMCPTool(endpoint, name string, arguments map[string]any) (string, bool, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": arguments},
	})
	if err != nil {
		return "", false, err
	}
	response, err := http.Post(endpoint, "application/json", bytes.NewReader(body)) //nolint:gosec // exact loopback proof endpoint.
	if err != nil {
		return "", false, err
	}
	defer response.Body.Close()
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text     string `json:"text"`
				Resource *struct {
					Text string `json:"text"`
				} `json:"resource,omitempty"`
			} `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return "", false, err
	}
	if envelope.Error != nil {
		return fmt.Sprint(envelope.Error), false, nil
	}
	var text strings.Builder
	for _, content := range envelope.Result.Content {
		text.WriteString(content.Text)
		if content.Resource != nil {
			text.WriteString(content.Resource.Text)
		}
	}
	return text.String(), !envelope.Result.IsError && text.Len() > 0, nil
}

func readWebSocketSnapshot(httpURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpURL, "http"), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	defer connection.Close(websocket.StatusNormalClosure, "backup proof complete")
	_, body, err := connection.Read(ctx)
	return body, err
}

func canonicalJSON(data []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	removeVolatile(value)
	return json.Marshal(value)
}

func removeVolatile(value any) {
	switch typed := value.(type) {
	case map[string]any:
		delete(typed, "epoch")
		delete(typed, "revision")
		delete(typed, "reset")
		for _, child := range typed {
			removeVolatile(child)
		}
	case []any:
		for _, child := range typed {
			removeVolatile(child)
		}
	}
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type backupProofArtifact struct {
	SchemaVersion     string               `json:"schema_version"`
	Mode              string               `json:"mode"`
	CandidateSHA      string               `json:"candidate_sha,omitempty"`
	BinarySHA256      string               `json:"binary_sha256"`
	ManifestSHA256    string               `json:"manifest_sha256"`
	BackupID          string               `json:"backup_id"`
	Create            backup.CreateReport  `json:"create"`
	Restore           backup.RestoreReport `json:"restore"`
	SourceFingerprint fingerprint          `json:"source_fingerprint"`
	TargetFingerprint fingerprint          `json:"target_fingerprint"`
	SourceSurfaces    surfaceFingerprint   `json:"source_surfaces"`
	TargetSurfaces    surfaceFingerprint   `json:"target_surfaces"`
	DLQBefore         map[string]string    `json:"dlq_before"`
	DLQAfter          map[string]string    `json:"dlq_after"`
	Assertions        []assertion          `json:"assertions"`
}

func TestBackupRestoreModeProof(t *testing.T) {
	binary := os.Getenv("OTELCONTEXT_SHUTDOWN_BINARY")
	mode := os.Getenv("OTELCONTEXT_BACKUP_PROOF_MODE")
	if binary == "" {
		t.Fatal("OTELCONTEXT_SHUTDOWN_BINARY is required")
	}
	if mode != "legacy" && mode != "aggregate-shadow" && mode != "aggregate" {
		t.Fatalf("unsupported proof mode %q", mode)
	}
	source := newAppProcess(t, binary, mode)
	t.Cleanup(func() {
		if source.cmd != nil {
			_, _ = source.stop()
		}
	})
	migrateExact(t, source)
	dlqBefore := seedDLQ(t, source.dlqPath)
	if err := source.start(); err != nil {
		t.Fatal(err)
	}
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 20*time.Second)
	if err := source.waitReady(readyCtx); err != nil {
		cancelReady()
		t.Fatalf("source readiness: %v\n%s", err, source.log.String())
	}
	cancelReady()
	exportCtx, cancelExport := context.WithTimeout(context.Background(), 10*time.Second)
	if err := sendFixtures(exportCtx, source.grpcPort); err != nil {
		cancelExport()
		t.Fatal(err)
	}
	cancelExport()
	if exit, err := source.stop(); err != nil || exit != 0 {
		t.Fatalf("source first shutdown exit=%d err=%v\n%s", exit, err, source.log.String())
	}
	if err := source.start(); err != nil {
		t.Fatal(err)
	}
	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 20*time.Second)
	if err := source.waitReady(recoveryCtx); err != nil {
		cancelRecovery()
		t.Fatalf("source recovery readiness: %v\n%s", err, source.log.String())
	}
	cancelRecovery()
	sourceSurfaces := collectSurfaceFingerprint(t, source)
	if exit, err := source.stop(); err != nil || exit != 0 {
		t.Fatalf("source proof shutdown exit=%d err=%v\n%s", exit, err, source.log.String())
	}
	sourceFingerprint := readFingerprint(t, source)

	backupParent := t.TempDir()
	createCommand := runOneShot(t, source, "backup", "create", "--out", backupParent)
	if createCommand.ExitCode != 0 {
		t.Fatalf("backup create exit=%d stdout=%s stderr=%s", createCommand.ExitCode, createCommand.Stdout, createCommand.Stderr)
	}
	var createReport backup.CreateReport
	if err := json.Unmarshal([]byte(createCommand.Stdout), &createReport); err != nil {
		t.Fatalf("decode create report: %v (%s)", err, createCommand.Stdout)
	}
	manifestPath := filepath.Join(createReport.Bundle, "manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest backup.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}

	target := newAppProcess(t, binary, mode)
	t.Cleanup(func() {
		if target.cmd != nil {
			_, _ = target.stop()
		}
	})
	target.autoMigrate = "false"
	restoreCommand := runOneShot(t, target, "backup", "restore", "--bundle", createReport.Bundle)
	if restoreCommand.ExitCode != 0 {
		t.Fatalf("backup restore exit=%d stdout=%s stderr=%s", restoreCommand.ExitCode, restoreCommand.Stdout, restoreCommand.Stderr)
	}
	var restoreReport backup.RestoreReport
	if err := json.Unmarshal([]byte(restoreCommand.Stdout), &restoreReport); err != nil {
		t.Fatalf("decode restore report: %v (%s)", err, restoreCommand.Stdout)
	}
	if restoreReport.ReadySeconds == nil || *restoreReport.ReadySeconds <= 0 {
		t.Fatalf("restore did not report measured readiness: %#v", restoreReport)
	}
	if err := target.start(); err != nil {
		t.Fatal(err)
	}
	targetReadyCtx, cancelTargetReady := context.WithTimeout(context.Background(), 20*time.Second)
	if err := target.waitReady(targetReadyCtx); err != nil {
		cancelTargetReady()
		t.Fatalf("target readiness: %v\n%s", err, target.log.String())
	}
	cancelTargetReady()
	targetSurfaces := collectSurfaceFingerprint(t, target)
	if exit, err := target.stop(); err != nil || exit != 0 {
		t.Fatalf("target shutdown exit=%d err=%v\n%s", exit, err, target.log.String())
	}
	targetFingerprint := readFingerprint(t, target)
	dlqAfter := fileInventory(t, target.dlqPath)

	mainEqual := reflect.DeepEqual(sourceFingerprint, targetFingerprint)
	dlqEqual := reflect.DeepEqual(dlqBefore, dlqAfter)
	surfacesEqual := reflect.DeepEqual(sourceSurfaces, targetSurfaces)
	artifact := backupProofArtifact{
		SchemaVersion:     "otelcontext.backup-proof.v1",
		Mode:              mode,
		CandidateSHA:      os.Getenv("GITHUB_SHA"),
		BinarySHA256:      sha256File(t, binary),
		ManifestSHA256:    digestBytes(manifestData),
		BackupID:          manifest.BackupID,
		Create:            createReport,
		Restore:           restoreReport,
		SourceFingerprint: sourceFingerprint,
		TargetFingerprint: targetFingerprint,
		SourceSurfaces:    sourceSurfaces,
		TargetSurfaces:    targetSurfaces,
		DLQBefore:         dlqBefore,
		DLQAfter:          dlqAfter,
		Assertions: []assertion{
			{Name: "manifest_schema_v1", Passed: manifest.SchemaVersion == backup.SchemaVersion},
			{Name: "candidate_binary_bound", Passed: manifest.Candidate.BinarySHA256 == sha256File(t, binary)},
			{Name: "mode_inventory_matches", Passed: manifest.Mode == mode && (mode == "legacy") == (manifest.Aggregate == nil)},
			{Name: "clean_shutdown_bound", Passed: manifest.Shutdown.Status == "success" && len(manifest.Shutdown.Steps) == len(requiredOwners)},
			{Name: "create_published", Passed: createReport.Status == "created" && !strings.HasSuffix(createReport.Bundle, ".partial")},
			{Name: "fresh_restore_completed", Passed: restoreReport.Status == "restored"},
			{Name: "readiness_measured", Passed: restoreReport.ReadySeconds != nil && *restoreReport.ReadySeconds > 0},
			{Name: "main_and_mode_owned_state_equal", Passed: mainEqual},
			{Name: "dlq_inventory_equal", Passed: dlqEqual},
			{Name: "rest_surfaces_equal", Passed: reflect.DeepEqual(sourceSurfaces.REST, targetSurfaces.REST)},
			{Name: "seven_mcp_tools_callable", Passed: len(targetSurfaces.MCPTools) == 7 && len(targetSurfaces.MCPCalls) == 7},
			{Name: "websocket_surface_equal", Passed: sourceSurfaces.WebSocket == targetSurfaces.WebSocket},
			{Name: "all_surfaces_equal", Passed: surfacesEqual},
			{Name: "lifecycle_fingerprint_equal", Passed: createReport.BackupID == restoreReport.BackupID && restoreReport.LifecycleFingerprint == manifest.LifecycleFingerprint},
		},
	}
	for _, check := range artifact.Assertions {
		if !check.Passed {
			t.Fatalf("backup proof assertion failed: %s\nsource=%#v\ntarget=%#v", check.Name, sourceSurfaces, targetSurfaces)
		}
	}
	proofDir := os.Getenv("OTELCONTEXT_BACKUP_PROOF_DIR")
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
		if err := os.WriteFile(filepath.Join(proofDir, mode+"-manifest.json"), manifestData, 0o600); err != nil {
			t.Fatal(err)
		}
		logs := "create stderr:\n" + createCommand.Stderr + "\nrestore stderr:\n" + restoreCommand.Stderr + "\nsource:\n" + source.log.String() + "\ntarget:\n" + target.log.String()
		if err := os.WriteFile(filepath.Join(proofDir, mode+".log"), []byte(logs), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
