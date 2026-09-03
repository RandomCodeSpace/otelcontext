package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

const (
	runtimeSchemaVersion = "otelcontext.runtime-proof/v1"
	activeMarkerName     = ".otelcontext-runtime-active.json"
	shutdownMarkerName   = ".otelcontext-shutdown-success.json"
)

var shutdownOwners = []string{
	"otlp_admission",
	"http",
	"pprof",
	"realtime",
	"ai",
	"ingest_pipeline",
	"aggregate_writer",
	"tsdb",
	"graphrag",
	"service_graph",
	"dlq",
	"disk_watchdog",
	"retention",
	"partitions",
	"tracer",
	"database_health",
	"boot_workers",
	"resource_registry",
	"aggregate_store",
	"main_database",
}

type activeRuntime struct {
	SchemaVersion     string    `json:"schema_version"`
	Status            string    `json:"status"`
	RuntimeID         string    `json:"runtime_id"`
	ConfigFingerprint string    `json:"config_fingerprint"`
	Candidate         Candidate `json:"candidate"`
	StartedAt         time.Time `json:"started_at"`
}

type shutdownReportWire struct {
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at"`
	Steps       []ShutdownStep `json:"steps"`
}

// CurrentCandidate hashes the running executable and reads the VCS revision
// stamped by the Go toolchain.
func CurrentCandidate(version string) (Candidate, error) {
	executable, err := os.Executable()
	if err != nil {
		return Candidate{}, fmt.Errorf("resolve current executable: %w", err)
	}
	digest, _, err := hashFile(executable)
	if err != nil {
		return Candidate{}, fmt.Errorf("hash current executable: %w", err)
	}
	commit := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && strings.TrimSpace(setting.Value) != "" {
				commit = setting.Value
				break
			}
		}
	}
	return Candidate{Version: version, Commit: commit, BinarySHA256: digest}, nil
}

// ConfigFingerprint binds settings which must not change across restore. Paths
// and database identities are intentionally separate so fresh targets are
// possible.
func ConfigFingerprint(cfg Config) (string, error) {
	tlsMode := "none"
	switch {
	case cfg.TLSCertFile != "" || cfg.TLSKeyFile != "":
		tlsMode = "operator-owned"
	case cfg.TLSAutoSelfSigned:
		tlsMode = "generated-self-signed"
	}
	return hashJSON(struct {
		Driver       string `json:"driver"`
		Partitioning string `json:"postgres_partitioning"`
		Mode         string `json:"aggregate_mode"`
		TLSMode      string `json:"tls_mode"`
	}{
		Driver:       normalizeDriver(cfg.DBDriver),
		Partitioning: strings.ToLower(strings.TrimSpace(cfg.DBPostgresPartitioning)),
		Mode:         strings.ToLower(strings.TrimSpace(cfg.AggregateMode)),
		TLSMode:      tlsMode,
	})
}

func runtimeFingerprint(cfg Config) (string, error) {
	base, err := ConfigFingerprint(cfg)
	if err != nil {
		return "", err
	}
	mainID, err := mainSourceIdentity(cfg)
	if err != nil {
		return "", err
	}
	aggregateID := "not-required"
	if strings.ToLower(cfg.AggregateMode) != "legacy" {
		path, err := resolved(cfg.AggregateDBPath)
		if err != nil {
			return "", err
		}
		aggregateID = identity(path)
	}
	dlqPath, err := resolved(cfg.DLQPath)
	if err != nil {
		return "", err
	}
	tlsID := "not-captured"
	if cfg.TLSAutoSelfSigned && cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" {
		path, err := resolved(cfg.TLSCacheDir)
		if err != nil {
			return "", err
		}
		tlsID = identity(path)
	}
	return hashJSON(struct {
		Config    string `json:"config_fingerprint"`
		Main      string `json:"main_identity"`
		Aggregate string `json:"aggregate_identity"`
		DLQ       string `json:"dlq_identity"`
		TLS       string `json:"tls_identity"`
	}{base, mainID, aggregateID, identity(dlqPath), tlsID})
}

// BeginRuntime invalidates any earlier clean-shutdown proof and writes the
// active marker before durable owners are opened.
func BeginRuntime(cfg Config, candidate Candidate, now time.Time) (RuntimeHandle, error) {
	dataDir, err := resolved(cfg.DataDiskPath)
	if err != nil {
		return RuntimeHandle{}, fmt.Errorf("resolve DATA_DISK_PATH: %w", err)
	}
	fingerprint, err := runtimeFingerprint(cfg)
	if err != nil {
		return RuntimeHandle{}, fmt.Errorf("runtime config fingerprint: %w", err)
	}
	id, err := newID()
	if err != nil {
		return RuntimeHandle{}, err
	}
	now = now.UTC()
	marker := activeRuntime{
		SchemaVersion:     runtimeSchemaVersion,
		Status:            "active",
		RuntimeID:         id,
		ConfigFingerprint: fingerprint,
		Candidate:         candidate,
		StartedAt:         now,
	}
	if err := writeJSONAtomic(filepath.Join(dataDir, activeMarkerName), marker, 0o600); err != nil {
		return RuntimeHandle{}, fmt.Errorf("write active runtime marker: %w", err)
	}
	if err := os.Remove(filepath.Join(dataDir, shutdownMarkerName)); err != nil && !os.IsNotExist(err) {
		return RuntimeHandle{}, fmt.Errorf("invalidate prior shutdown proof: %w", err)
	}
	if err := syncDir(dataDir); err != nil {
		return RuntimeHandle{}, fmt.Errorf("sync runtime marker directory: %w", err)
	}
	return RuntimeHandle{ID: id, ConfigFingerprint: fingerprint, Candidate: candidate, StartedAt: now}, nil
}

// CompleteRuntime converts the active marker into a successful shutdown proof.
// report must JSON-encode with started_at, completed_at, and steps fields.
func CompleteRuntime(cfg Config, handle RuntimeHandle, report any) (ShutdownProof, error) {
	dataDir, err := resolved(cfg.DataDiskPath)
	if err != nil {
		return ShutdownProof{}, err
	}
	activePath := filepath.Join(dataDir, activeMarkerName)
	activeData, err := os.ReadFile(activePath) // #nosec G304 -- fixed marker under resolved data directory.
	if err != nil {
		return ShutdownProof{}, fmt.Errorf("read active runtime marker: %w", err)
	}
	var active activeRuntime
	if err := json.Unmarshal(activeData, &active); err != nil {
		return ShutdownProof{}, fmt.Errorf("decode active runtime marker: %w", err)
	}
	if active.SchemaVersion != runtimeSchemaVersion || active.Status != "active" || active.RuntimeID != handle.ID || active.ConfigFingerprint != handle.ConfigFingerprint || active.Candidate != handle.Candidate {
		return ShutdownProof{}, errors.New("active runtime marker does not match this process")
	}
	wireData, err := json.Marshal(report)
	if err != nil {
		return ShutdownProof{}, fmt.Errorf("encode shutdown report: %w", err)
	}
	var wire shutdownReportWire
	if err := json.Unmarshal(wireData, &wire); err != nil {
		return ShutdownProof{}, fmt.Errorf("decode shutdown report: %w", err)
	}
	if err := validateShutdownSteps(wire.Steps); err != nil {
		return ShutdownProof{}, err
	}
	if wire.StartedAt.IsZero() || wire.CompletedAt.Before(wire.StartedAt) {
		return ShutdownProof{}, errors.New("shutdown report has invalid timestamps")
	}
	proof := ShutdownProof{
		SchemaVersion:     runtimeSchemaVersion,
		Status:            "success",
		RuntimeID:         handle.ID,
		ConfigFingerprint: handle.ConfigFingerprint,
		Candidate:         handle.Candidate,
		StartedAt:         wire.StartedAt.UTC(),
		CompletedAt:       wire.CompletedAt.UTC(),
		Steps:             wire.Steps,
	}
	if err := writeJSONAtomic(filepath.Join(dataDir, shutdownMarkerName), proof, 0o600); err != nil {
		return ShutdownProof{}, fmt.Errorf("write shutdown proof: %w", err)
	}
	if err := os.Remove(activePath); err != nil {
		return ShutdownProof{}, fmt.Errorf("remove active runtime marker: %w", err)
	}
	if err := syncDir(dataDir); err != nil {
		return ShutdownProof{}, fmt.Errorf("sync shutdown proof directory: %w", err)
	}
	return proof, nil
}

func loadShutdownProof(cfg Config, candidate Candidate) (ShutdownProof, error) {
	dataDir, err := resolved(cfg.DataDiskPath)
	if err != nil {
		return ShutdownProof{}, err
	}
	if _, err := os.Stat(filepath.Join(dataDir, activeMarkerName)); err == nil {
		return ShutdownProof{}, errors.New("backup refused: an OtelContext runtime is active or did not complete a clean shutdown")
	} else if !os.IsNotExist(err) {
		return ShutdownProof{}, fmt.Errorf("inspect active runtime marker: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(dataDir, shutdownMarkerName)) // #nosec G304 -- fixed marker under resolved data directory.
	if err != nil {
		if os.IsNotExist(err) {
			return ShutdownProof{}, errors.New("backup refused: no successful shutdown proof exists; start this binary and stop it cleanly first")
		}
		return ShutdownProof{}, err
	}
	var proof ShutdownProof
	if err := json.Unmarshal(data, &proof); err != nil {
		return ShutdownProof{}, fmt.Errorf("decode shutdown proof: %w", err)
	}
	wantFingerprint, err := runtimeFingerprint(cfg)
	if err != nil {
		return ShutdownProof{}, err
	}
	if proof.SchemaVersion != runtimeSchemaVersion || proof.Status != "success" || proof.ConfigFingerprint != wantFingerprint {
		return ShutdownProof{}, errors.New("backup refused: shutdown proof does not match the configured durable owners")
	}
	if proof.Candidate != candidate {
		return ShutdownProof{}, errors.New("backup refused: shutdown proof was produced by a different candidate binary")
	}
	if err := validateShutdownSteps(proof.Steps); err != nil {
		return ShutdownProof{}, fmt.Errorf("backup refused: %w", err)
	}
	if proof.StartedAt.IsZero() || proof.CompletedAt.Before(proof.StartedAt) || proof.CompletedAt.After(time.Now().UTC().Add(time.Minute)) {
		return ShutdownProof{}, errors.New("backup refused: shutdown proof timestamps are invalid")
	}
	return proof, nil
}

func validateShutdownSteps(steps []ShutdownStep) error {
	if len(steps) != len(shutdownOwners) {
		return fmt.Errorf("shutdown proof completed %d owners, want %d", len(steps), len(shutdownOwners))
	}
	for index, want := range shutdownOwners {
		step := steps[index]
		if step.Name != want {
			return fmt.Errorf("shutdown owner %d is %q, want %q", index, step.Name, want)
		}
		if step.Error != "" {
			return fmt.Errorf("shutdown owner %s failed: %s", step.Name, step.Error)
		}
		if step.StartedAt.IsZero() || step.CompletedAt.Before(step.StartedAt) {
			return fmt.Errorf("shutdown owner %s has invalid timestamps", step.Name)
		}
	}
	return nil
}

func normalizeDriver(driver string) string {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "", "sqlite":
		return "sqlite"
	case "postgresql", "postgres":
		return "postgres"
	case "sqlserver", "mssql":
		return "mssql"
	case "mysql":
		return "mysql"
	default:
		return strings.ToLower(strings.TrimSpace(driver))
	}
}
