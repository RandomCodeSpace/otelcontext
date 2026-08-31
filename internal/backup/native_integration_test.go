//go:build integration && !windows

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

type nativeFixture struct {
	adapter   string
	container testcontainers.Container
	id        string
	root      string
	sourceDSN string
	targetDSN string
	internal  map[string]string
}

type containerRunner struct {
	fixture *nativeFixture
}

func (runner containerRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	if err := runner.fixture.chmodShared(ctx); err != nil {
		return CommandResult{ExitCode: -1}, err
	}
	client := command.Name
	if runner.fixture.adapter == "mssql" && command.Name == "sqlcmd" {
		client = "/opt/mssql-tools18/bin/sqlcmd"
	}
	args := []string{"exec", "-i"}
	for _, value := range command.Env {
		args = append(args, "-e", value)
	}
	args = append(args, runner.fixture.id, client)
	clientArgs := append([]string(nil), command.Args...)
	switch runner.fixture.adapter {
	case "postgres":
		for index, argument := range clientArgs {
			if strings.HasPrefix(argument, "--dbname=") {
				external := strings.TrimPrefix(argument, "--dbname=")
				clientArgs[index] = "--dbname=" + runner.fixture.internal[external]
			}
		}
	case "mysql":
		for index, argument := range clientArgs {
			switch {
			case strings.HasPrefix(argument, "--host="):
				clientArgs[index] = "--host=127.0.0.1"
			case strings.HasPrefix(argument, "--port="):
				clientArgs[index] = "--port=3306"
			}
		}
	case "mssql":
		for index := 0; index+1 < len(clientArgs); index++ {
			if clientArgs[index] == "-S" {
				clientArgs[index+1] = "127.0.0.1,1433"
			}
		}
	}
	args = append(args, clientArgs...)
	redactions := append([]string(nil), command.Redactions...)
	for external, internal := range runner.fixture.internal {
		redactions = append(redactions, external, internal)
	}
	wrapped := command
	wrapped.Name = "docker"
	wrapped.Args = args
	wrapped.Env = nil
	wrapped.Redactions = redactions
	result, err := (execRunner{}).Run(ctx, wrapped)
	chmodErr := runner.fixture.chmodShared(ctx)
	if err != nil {
		return result, err
	}
	if chmodErr != nil {
		return result, chmodErr
	}
	return result, nil
}

func (fixture *nativeFixture) chmodShared(ctx context.Context) error {
	command := Command{
		Name:    "docker",
		Args:    []string{"exec", "-u", "0", fixture.id, "chmod", "-R", "a+rwX", fixture.root},
		Display: "docker exec <database> chmod shared backup fixture",
	}
	_, err := (execRunner{}).Run(ctx, command)
	return err
}

func startNativeFixture(t *testing.T, adapter string) *nativeFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	request := testcontainers.ContainerRequest{
		ExposedPorts: []string{},
		Binds:        []string{root + ":" + root},
	}
	switch adapter {
	case "postgres":
		request.Image = "postgres:16-alpine"
		request.Env = map[string]string{"POSTGRES_USER": "otel", "POSTGRES_PASSWORD": "otel", "POSTGRES_DB": "postgres"}
		request.ExposedPorts = []string{"5432/tcp"}
		request.WaitingFor = wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(2 * time.Minute)
	case "mysql":
		request.Image = "mysql:8.4"
		request.Env = map[string]string{"MYSQL_ROOT_PASSWORD": "OtelContext-248"}
		request.ExposedPorts = []string{"3306/tcp"}
		request.WaitingFor = wait.ForLog("ready for connections").WithStartupTimeout(3 * time.Minute)
	case "mssql":
		request.Image = "mcr.microsoft.com/mssql/server:2022-latest"
		request.Env = map[string]string{
			"ACCEPT_EULA":           "Y",
			"MSSQL_SA_PASSWORD":     "OtelContext!248Proof",
			"MSSQL_PID":             "Developer",
			"MSSQL_MEMORY_LIMIT_MB": "2048",
		}
		request.ExposedPorts = []string{"1433/tcp"}
		request.WaitingFor = wait.ForLog("SQL Server is now ready for client connections").WithStartupTimeout(4 * time.Minute)
	default:
		t.Fatalf("unsupported adapter %q", adapter)
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: request, Started: true})
	if err != nil {
		t.Fatalf("start %s container: %v", adapter, err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	id := container.GetContainerID()
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	portID := request.ExposedPorts[0]
	mapped, err := container.MappedPort(ctx, portID)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &nativeFixture{adapter: adapter, container: container, id: id, root: root, internal: make(map[string]string)}
	switch adapter {
	case "postgres":
		admin := fmt.Sprintf("postgres://otel:otel@%s:%s/postgres?sslmode=disable", host, mapped.Port())
		createDatabases(t, adapter, admin, []string{"otel_source", "otel_target"})
		fixture.sourceDSN = fmt.Sprintf("postgres://otel:otel@%s:%s/otel_source?sslmode=disable", host, mapped.Port())
		fixture.targetDSN = fmt.Sprintf("postgres://otel:otel@%s:%s/otel_target?sslmode=disable", host, mapped.Port())
		fixture.internal[fixture.sourceDSN] = "postgres://otel:otel@127.0.0.1:5432/otel_source?sslmode=disable"
		fixture.internal[fixture.targetDSN] = "postgres://otel:otel@127.0.0.1:5432/otel_target?sslmode=disable"
	case "mysql":
		admin := fmt.Sprintf("root:OtelContext-248@tcp(%s:%s)/mysql?charset=utf8mb4&parseTime=True&loc=UTC", host, mapped.Port())
		createDatabases(t, adapter, admin, []string{"otel_source", "otel_target"})
		fixture.sourceDSN = fmt.Sprintf("root:OtelContext-248@tcp(%s:%s)/otel_source?charset=utf8mb4&parseTime=True&loc=UTC", host, mapped.Port())
		fixture.targetDSN = fmt.Sprintf("root:OtelContext-248@tcp(%s:%s)/otel_target?charset=utf8mb4&parseTime=True&loc=UTC", host, mapped.Port())
	case "mssql":
		adminURL := &url.URL{Scheme: "sqlserver", User: url.UserPassword("sa", "OtelContext!248Proof"), Host: host + ":" + mapped.Port()}
		adminQuery := adminURL.Query()
		adminQuery.Set("database", "master")
		adminQuery.Set("encrypt", "disable")
		adminQuery.Set("TrustServerCertificate", "true")
		adminURL.RawQuery = adminQuery.Encode()
		createDatabases(t, adapter, adminURL.String(), []string{"otel_source"})
		sourceURL := *adminURL
		sourceQuery := sourceURL.Query()
		sourceQuery.Set("database", "otel_source")
		sourceURL.RawQuery = sourceQuery.Encode()
		targetURL := *adminURL
		targetQuery := targetURL.Query()
		targetQuery.Set("database", "otel_target")
		targetURL.RawQuery = targetQuery.Encode()
		fixture.sourceDSN = sourceURL.String()
		fixture.targetDSN = targetURL.String()
	}
	return fixture
}

func createDatabases(t *testing.T, adapter, dsn string, names []string) {
	t.Helper()
	db, err := storage.NewDatabase(adapter, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeGORM(db)
	for _, name := range names {
		statement := "CREATE DATABASE " + name
		if adapter == "mysql" {
			statement += " CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
		}
		if err := db.Exec(statement).Error; err != nil { //nolint:gosec // fixed test database identifiers.
			t.Fatalf("create %s database %s: %v", adapter, name, err)
		}
	}
}

func prepareNativeSource(t *testing.T, fixture *nativeFixture) {
	t.Helper()
	db, err := storage.NewDatabase(fixture.adapter, fixture.sourceDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeGORM(db)
	if migrate.SupportsVersioned(fixture.adapter) {
		if _, err := migrate.Up(context.Background(), db, fixture.adapter); err != nil {
			t.Fatal(err)
		}
	} else if err := migrate.AutoMigrate(db, fixture.adapter, storage.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	trace := storage.Trace{
		TenantID: "default", TraceID: "native-backup-fixture", ServiceName: "checkout",
		Duration: 42, Status: storage.StatusCodeError, Timestamp: now,
	}
	if err := db.Create(&trace).Error; err != nil {
		t.Fatal(err)
	}
}

func nativeConfig(root, adapter, dsn, suffix string) Config {
	data := filepath.Join(root, suffix)
	return Config{
		DBDriver:        adapter,
		DBDSN:           dsn,
		AggregateMode:   "legacy",
		AggregateDBPath: filepath.Join(data, "aggregate.db"),
		DLQPath:         filepath.Join(data, "dlq"),
		DataDiskPath:    data,
		TLSCacheDir:     filepath.Join(data, "tls"),
	}
}

func writeNativeShutdownProof(t *testing.T, cfg Config, candidate Candidate) {
	t.Helper()
	if err := os.MkdirAll(cfg.DLQPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DLQPath, "batch_native.json"), []byte(`{"fixture":"native"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Second)
	handle, err := BeginRuntime(cfg, candidate, started)
	if err != nil {
		t.Fatal(err)
	}
	steps := make([]ShutdownStep, 0, len(shutdownOwners))
	for index, owner := range shutdownOwners {
		stepStart := started.Add(time.Duration(index) * time.Millisecond)
		steps = append(steps, ShutdownStep{Name: owner, StartedAt: stepStart, CompletedAt: stepStart.Add(time.Millisecond)})
	}
	report := shutdownReportWire{StartedAt: started, CompletedAt: started.Add(100 * time.Millisecond), Steps: steps}
	if _, err := CompleteRuntime(cfg, handle, report); err != nil {
		t.Fatal(err)
	}
}

type nativeProof struct {
	SchemaVersion     string        `json:"schema_version"`
	Adapter           string        `json:"adapter"`
	CandidateSHA      string        `json:"candidate_sha,omitempty"`
	EngineVersion     string        `json:"engine_version"`
	MigrationState    string        `json:"migration_state"`
	SourceLifecycle   string        `json:"source_lifecycle_fingerprint"`
	RestoredLifecycle string        `json:"restored_lifecycle_fingerprint"`
	Create            CreateReport  `json:"create"`
	Restore           RestoreReport `json:"restore"`
	ManifestSHA256    string        `json:"manifest_sha256"`
	Assertions        []struct {
		Name   string `json:"name"`
		Passed bool   `json:"passed"`
	} `json:"assertions"`
}

func TestNativeBackupRestoreLifecycle(t *testing.T) {
	adapter := os.Getenv("OTELCONTEXT_BACKUP_ADAPTER")
	if adapter != "postgres" && adapter != "mysql" && adapter != "mssql" {
		t.Fatalf("OTELCONTEXT_BACKUP_ADAPTER must be postgres, mysql, or mssql; got %q", adapter)
	}
	fixture := startNativeFixture(t, adapter)
	prepareNativeSource(t, fixture)
	candidate, err := CurrentCandidate("integration-test")
	if err != nil {
		t.Fatal(err)
	}
	sourceCfg := nativeConfig(fixture.root, adapter, fixture.sourceDSN, "source")
	writeNativeShutdownProof(t, sourceCfg, candidate)
	runner := containerRunner{fixture: fixture}
	create, err := Create(context.Background(), sourceCfg, CreateOptions{
		OutputDirectory: filepath.Join(fixture.root, "backups"),
		Candidate:       candidate,
		Runner:          runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestData, err := os.ReadFile(filepath.Join(create.Bundle, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	targetCfg := nativeConfig(fixture.root, adapter, fixture.targetDSN, "target")
	restore, err := Restore(context.Background(), targetCfg, RestoreOptions{
		BundleDirectory: create.Bundle,
		Candidate:       candidate,
		Runner:          runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := inspectMain(context.Background(), targetCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareMain(manifest.Main, restored); err != nil {
		t.Fatal(err)
	}
	targetDB, err := storage.NewDatabase(adapter, fixture.targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	var restoredFixtureRows int64
	if err := targetDB.Table("traces").Where("trace_id = ?", "native-backup-fixture").Count(&restoredFixtureRows).Error; err != nil {
		closeGORM(targetDB)
		t.Fatal(err)
	}
	closeGORM(targetDB)
	prefix := map[string]string{"postgres": "16.", "mysql": "8.4.", "mssql": "16."}[adapter]
	checks := []struct {
		Name   string `json:"name"`
		Passed bool   `json:"passed"`
	}{
		{"manifest_schema_v1", manifest.SchemaVersion == SchemaVersion},
		{"candidate_binary_bound", manifest.Candidate == candidate},
		{"adapter_bound", manifest.Main.Adapter == adapter},
		{"engine_version_frozen", strings.HasPrefix(manifest.Main.EngineVersion, prefix)},
		{"native_commands_recorded", len(manifest.Commands) >= 2},
		{"fresh_restore_completed", restore.Status == "restored"},
		{"migration_status_equal", manifest.Main.MigrationState == restored.MigrationState && manifest.Main.MigrationVersion == restored.MigrationVersion},
		{"lifecycle_fingerprint_equal", manifest.Main.LifecycleFingerprint == restored.LifecycleFingerprint},
		{"fixture_row_restored", restoredFixtureRows == 1},
		{"bundle_lifecycle_equal", manifest.LifecycleFingerprint == restore.LifecycleFingerprint},
	}
	for _, check := range checks {
		if !check.Passed {
			t.Fatalf("native backup assertion failed: %s", check.Name)
		}
	}
	digest := hashBytes(manifestData)
	proof := nativeProof{
		SchemaVersion:     "otelcontext.native-backup-proof/v1",
		Adapter:           adapter,
		CandidateSHA:      os.Getenv("GITHUB_SHA"),
		EngineVersion:     manifest.Main.EngineVersion,
		MigrationState:    manifest.Main.MigrationState,
		SourceLifecycle:   manifest.Main.LifecycleFingerprint,
		RestoredLifecycle: restored.LifecycleFingerprint,
		Create:            create,
		Restore:           restore,
		ManifestSHA256:    digest,
		Assertions:        checks,
	}
	proofDir := os.Getenv("OTELCONTEXT_NATIVE_BACKUP_PROOF_DIR")
	if proofDir != "" {
		if err := os.MkdirAll(proofDir, 0o750); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(proof, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proofDir, adapter+".json"), append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proofDir, adapter+"-manifest.json"), manifestData, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
