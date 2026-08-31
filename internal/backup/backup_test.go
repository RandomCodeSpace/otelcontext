package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	schemamigrate "github.com/RandomCodeSpace/otelcontext/internal/migrate"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

var testCandidate = Candidate{
	Version:      "v-test",
	Commit:       strings.Repeat("b", 40),
	BinarySHA256: strings.Repeat("a", 64),
}

type fixture struct {
	cfg      Config
	root     string
	shutdown ShutdownProof
}

func newFixture(t *testing.T, mode string, generatedTLS bool) fixture {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DBDriver:          "sqlite",
		DBDSN:             filepath.Join(data, "main.db"),
		AggregateMode:     mode,
		AggregateDBPath:   filepath.Join(data, "aggregate.db"),
		DLQPath:           filepath.Join(data, "dlq"),
		DataDiskPath:      data,
		TLSAutoSelfSigned: generatedTLS,
		TLSCacheDir:       filepath.Join(data, "tls"),
	}
	if err := os.MkdirAll(cfg.DLQPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DLQPath, "batch_fixture.json"), []byte(`{"fixture":"durable"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := storage.NewDatabase("sqlite", cfg.DBDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schemamigrate.Up(context.Background(), db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO traces
(tenant_id,trace_id,service_name,duration,status,timestamp,created_at,updated_at,truncated,retained_span_count,observed_span_count)
VALUES ('default','backup-fixture','checkout',42,'STATUS_CODE_ERROR',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,0,1,1)`).Error; err != nil {
		t.Fatal(err)
	}
	closeGORM(db)
	if mode != "legacy" {
		store, err := aggregate.OpenSQLiteStore(aggregate.StoreConfig{Path: cfg.AggregateDBPath})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if generatedTLS {
		if err := os.MkdirAll(cfg.TLSCacheDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.TLSCacheDir, "cert.pem"), []byte("fixture certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.TLSCacheDir, "key.pem"), []byte("fixture private key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now().UTC().Add(-2 * time.Second)
	handle, err := BeginRuntime(cfg, testCandidate, started)
	if err != nil {
		t.Fatal(err)
	}
	report := shutdownReportWire{
		StartedAt:   started.Add(time.Second),
		CompletedAt: started.Add(1500 * time.Millisecond),
		Steps:       successfulSteps(started.Add(time.Second)),
	}
	proof, err := CompleteRuntime(cfg, handle, report)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{cfg: cfg, root: root, shutdown: proof}
}

func successfulSteps(start time.Time) []ShutdownStep {
	steps := make([]ShutdownStep, 0, len(shutdownOwners))
	for index, owner := range shutdownOwners {
		stepStart := start.Add(time.Duration(index) * time.Millisecond)
		steps = append(steps, ShutdownStep{Name: owner, StartedAt: stepStart, CompletedAt: stepStart.Add(time.Millisecond)})
	}
	return steps
}

func createFixtureBundle(t *testing.T, fixture fixture) (CreateReport, Manifest) {
	t.Helper()
	output := filepath.Join(fixture.root, "backups")
	report, err := Create(context.Background(), fixture.cfg, CreateOptions{
		OutputDirectory: output,
		Candidate:       testCandidate,
		Now:             func() time.Time { return fixture.shutdown.CompletedAt.Add(time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := readManifest(t, report.Bundle)
	return report, manifest
}

func readManifest(t *testing.T, bundle string) Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(bundle, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeManifest(t *testing.T, bundle string, manifest Manifest) {
	t.Helper()
	path := filepath.Join(bundle, manifestName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONExclusive(path, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
}

func freshRestoreConfig(root, mode string, generatedTLS bool) Config {
	data := filepath.Join(root, "restored")
	return Config{
		DBDriver:          "sqlite",
		DBDSN:             filepath.Join(data, "main.db"),
		AggregateMode:     mode,
		AggregateDBPath:   filepath.Join(data, "aggregate.db"),
		DLQPath:           filepath.Join(data, "dlq"),
		DataDiskPath:      data,
		TLSAutoSelfSigned: generatedTLS,
		TLSCacheDir:       filepath.Join(data, "tls"),
	}
}

func TestCreateAndRestoreEveryMode(t *testing.T) {
	for _, mode := range []string{"legacy", "aggregate-shadow", "aggregate"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newFixture(t, mode, true)
			report, manifest := createFixtureBundle(t, fixture)
			if strings.HasSuffix(report.Bundle, ".partial") {
				t.Fatalf("published bundle kept partial suffix: %s", report.Bundle)
			}
			if manifest.SchemaVersion != SchemaVersion || manifest.Mode != mode || manifest.DLQ.Count != 1 || manifest.TLS == nil {
				t.Fatalf("manifest inventory = %#v", manifest)
			}
			if (mode != "legacy") != (manifest.Aggregate != nil) {
				t.Fatalf("aggregate inventory mismatch for %s", mode)
			}
			target := freshRestoreConfig(fixture.root, mode, true)
			restored, err := Restore(context.Background(), target, RestoreOptions{
				BundleDirectory: report.Bundle,
				Candidate:       testCandidate,
				Now:             func() time.Time { return manifest.CreatedAt.Add(10 * time.Second) },
			})
			if err != nil {
				t.Fatal(err)
			}
			if restored.Status != "restored" || restored.LifecycleFingerprint != manifest.LifecycleFingerprint || restored.BackupAgeSeconds != 10 {
				t.Fatalf("restore report = %#v", restored)
			}
			if _, err := os.Stat(filepath.Join(target.DLQPath, "batch_fixture.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(target.TLSCacheDir, "cert.pem")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreateRefusesActiveRuntimeAndHalfTLSPair(t *testing.T) {
	fixture := newFixture(t, "legacy", false)
	handle, err := BeginRuntime(fixture.cfg, testCandidate, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Create(context.Background(), fixture.cfg, CreateOptions{OutputDirectory: filepath.Join(fixture.root, "active-backups"), Candidate: testCandidate})
	if err == nil || !strings.Contains(err.Error(), "runtime is active") {
		t.Fatalf("active-runtime error = %v", err)
	}
	if err := os.Remove(filepath.Join(fixture.cfg.DataDiskPath, activeMarkerName)); err != nil {
		t.Fatal(err)
	}
	_ = handle

	fixture = newFixture(t, "legacy", true)
	if err := os.Remove(filepath.Join(fixture.cfg.TLSCacheDir, "key.pem")); err != nil {
		t.Fatal(err)
	}
	_, err = Create(context.Background(), fixture.cfg, CreateOptions{OutputDirectory: filepath.Join(fixture.root, "half-tls-backups"), Candidate: testCandidate})
	if err == nil || !strings.Contains(err.Error(), "both cert.pem and key.pem") {
		t.Fatalf("half-TLS error = %v", err)
	}
}

func TestRestoreRejectsBundleDamageBeforeTargetMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, *Manifest)
		want   string
	}{
		{
			name: "missing artifact",
			mutate: func(t *testing.T, bundle string, manifest *Manifest) {
				t.Helper()
				if err := os.Remove(filepath.Join(bundle, manifest.Main.ArtifactPath)); err != nil {
					t.Fatal(err)
				}
			},
			want: "missing artifacts",
		},
		{
			name: "hash mismatch",
			mutate: func(t *testing.T, bundle string, manifest *Manifest) {
				t.Helper()
				path := filepath.Join(bundle, manifest.Main.ArtifactPath)
				file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte("tamper")); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
			want: "hash or size mismatch",
		},
		{
			name: "tampered DLQ",
			mutate: func(t *testing.T, bundle string, _ *Manifest) {
				t.Helper()
				path := filepath.Join(bundle, "dlq", "batch_fixture.json")
				file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte("tamper")); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
			want: "hash or size mismatch",
		},
		{
			name: "unsupported migration",
			mutate: func(t *testing.T, bundle string, manifest *Manifest) {
				t.Helper()
				manifest.Main.MigrationVersion = schemamigrate.CurrentVersion + 1
				writeManifest(t, bundle, *manifest)
			},
			want: "unsupported main migration",
		},
		{
			name: "half TLS pair",
			mutate: func(t *testing.T, bundle string, manifest *Manifest) {
				t.Helper()
				for index, artifact := range manifest.Artifacts {
					if artifact.Role == roleTLSKey {
						manifest.Artifacts = append(manifest.Artifacts[:index], manifest.Artifacts[index+1:]...)
						break
					}
				}
				writeManifest(t, bundle, *manifest)
			},
			want: "complete generated TLS pair",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t, "legacy", true)
			report, manifest := createFixtureBundle(t, fixture)
			test.mutate(t, report.Bundle, &manifest)
			target := freshRestoreConfig(fixture.root, "legacy", true)
			_, err := Restore(context.Background(), target, RestoreOptions{BundleDirectory: report.Bundle, Candidate: testCandidate})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("restore error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(target.DBDSN); !os.IsNotExist(statErr) {
				t.Fatalf("main target mutated before rejection: %v", statErr)
			}
		})
	}
}

func TestRestoreRefusesPartialWrongModeInPlaceAndNonFreshSidecar(t *testing.T) {
	fixture := newFixture(t, "aggregate-shadow", false)
	report, _ := createFixtureBundle(t, fixture)
	t.Run("partial", func(t *testing.T) {
		partial := report.Bundle + ".partial"
		if err := os.Rename(report.Bundle, partial); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Rename(partial, report.Bundle) }()
		_, err := Restore(context.Background(), freshRestoreConfig(filepath.Join(fixture.root, "partial"), "aggregate-shadow", false), RestoreOptions{BundleDirectory: partial, Candidate: testCandidate})
		if err == nil || !strings.Contains(err.Error(), "incomplete .partial") {
			t.Fatalf("partial error = %v", err)
		}
	})
	t.Run("wrong mode", func(t *testing.T) {
		_, err := Restore(context.Background(), freshRestoreConfig(filepath.Join(fixture.root, "mode"), "legacy", false), RestoreOptions{BundleDirectory: report.Bundle, Candidate: testCandidate})
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("wrong-mode error = %v", err)
		}
	})
	t.Run("in place", func(t *testing.T) {
		_, err := Restore(context.Background(), fixture.cfg, RestoreOptions{BundleDirectory: report.Bundle, Candidate: testCandidate})
		if err == nil || !strings.Contains(err.Error(), "source identity") {
			t.Fatalf("in-place error = %v", err)
		}
	})
	t.Run("non-fresh DLQ", func(t *testing.T) {
		target := freshRestoreConfig(filepath.Join(fixture.root, "nonfresh"), "aggregate-shadow", false)
		if err := os.MkdirAll(target.DLQPath, 0o750); err != nil {
			t.Fatal(err)
		}
		_, err := Restore(context.Background(), target, RestoreOptions{BundleDirectory: report.Bundle, Candidate: testCandidate})
		if err == nil || !strings.Contains(err.Error(), "not fresh") {
			t.Fatalf("non-fresh error = %v", err)
		}
		if _, statErr := os.Stat(target.DBDSN); !os.IsNotExist(statErr) {
			t.Fatalf("main target mutated before sidecar validation: %v", statErr)
		}
	})
}

func TestRestoreRejectsAggregateIdentityMismatchAfterHashValidation(t *testing.T) {
	fixture := newFixture(t, "aggregate", false)
	report, manifest := createFixtureBundle(t, fixture)
	path := filepath.Join(report.Bundle, manifest.Aggregate.ArtifactPath)
	db, err := storage.NewDatabase("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE aggregate_meta SET value='different-store' WHERE key='store_uuid'`).Error; err != nil {
		t.Fatal(err)
	}
	closeGORM(db)
	for index := range manifest.Artifacts {
		if manifest.Artifacts[index].Role == roleAggregate {
			digest, size, err := hashFile(path)
			if err != nil {
				t.Fatal(err)
			}
			manifest.Artifacts[index].SHA256 = digest
			manifest.Artifacts[index].Size = size
		}
	}
	writeManifest(t, report.Bundle, manifest)
	_, err = Restore(context.Background(), freshRestoreConfig(fixture.root, "aggregate", false), RestoreOptions{BundleDirectory: report.Bundle, Candidate: testCandidate})
	if err == nil || !strings.Contains(err.Error(), "aggregate fingerprint mismatch") {
		t.Fatalf("aggregate identity error = %v", err)
	}
}

func TestRestoreRejectsAggregateVersionMismatchBeforeTargetMutation(t *testing.T) {
	fixture := newFixture(t, "aggregate", false)
	report, manifest := createFixtureBundle(t, fixture)
	path := filepath.Join(report.Bundle, manifest.Aggregate.ArtifactPath)
	db, err := storage.NewDatabase("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE aggregate_meta SET value=? WHERE key='schema_version'`, aggregate.StoreSchemaVersion+1).Error; err != nil {
		t.Fatal(err)
	}
	closeGORM(db)
	for index := range manifest.Artifacts {
		if manifest.Artifacts[index].Role == roleAggregate {
			digest, size, err := hashFile(path)
			if err != nil {
				t.Fatal(err)
			}
			manifest.Artifacts[index].SHA256 = digest
			manifest.Artifacts[index].Size = size
		}
	}
	writeManifest(t, report.Bundle, manifest)
	target := freshRestoreConfig(fixture.root, "aggregate", false)
	_, err = Restore(context.Background(), target, RestoreOptions{BundleDirectory: report.Bundle, Candidate: testCandidate})
	if err == nil || !strings.Contains(err.Error(), "aggregate store state") {
		t.Fatalf("aggregate version error = %v", err)
	}
	if _, statErr := os.Stat(target.DBDSN); !os.IsNotExist(statErr) {
		t.Fatalf("main target mutated before aggregate rejection: %v", statErr)
	}
}

type commandRunnerFunc func(context.Context, Command) (CommandResult, error)

func (runner commandRunnerFunc) Run(ctx context.Context, command Command) (CommandResult, error) {
	return runner(ctx, command)
}

func TestNativeClientFailuresAreClear(t *testing.T) {
	t.Run("missing client", func(t *testing.T) {
		_, err := (execRunner{}).Run(context.Background(), Command{
			Name:    "otelcontext-native-client-does-not-exist",
			Display: "missing native client",
		})
		if err == nil || !strings.Contains(err.Error(), `required client "otelcontext-native-client-does-not-exist" is missing from PATH`) {
			t.Fatalf("missing client error = %v", err)
		}
	})

	t.Run("wrong client version", func(t *testing.T) {
		runner := commandRunnerFunc(func(context.Context, Command) (CommandResult, error) {
			return CommandResult{Output: "pg_dump (PostgreSQL) 15.13", ExitCode: 0}, nil
		})
		var records []CommandRecord
		err := requireVersion(context.Background(), runner, "pg_dump", []string{"--version"}, "pg_dump --version", " 16.", &records)
		if err == nil || !strings.Contains(err.Error(), "wrong version") || !strings.Contains(err.Error(), `require output containing " 16."`) {
			t.Fatalf("wrong version error = %v", err)
		}
		if len(records) != 1 || records[0].ExitCode != 0 {
			t.Fatalf("version command records = %#v", records)
		}
	})
}
