package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Create captures one stopped, quiesced set of durable owners and publishes
// the bundle only after manifest.json has been synced.
func Create(ctx context.Context, cfg Config, options CreateOptions) (CreateReport, error) {
	started := time.Now()
	if err := validateConfig(cfg); err != nil {
		return CreateReport{}, err
	}
	outputDir, err := absolute(options.OutputDirectory, "--out")
	if err != nil {
		return CreateReport{}, err
	}
	if err := validateCandidate(options.Candidate); err != nil {
		return CreateReport{}, err
	}
	if err := validateOutputLocation(cfg, outputDir); err != nil {
		return CreateReport{}, err
	}
	shutdown, err := loadShutdownProof(cfg, options.Candidate)
	if err != nil {
		return CreateReport{}, err
	}
	mainOwner, err := inspectMain(ctx, cfg)
	if err != nil {
		return CreateReport{}, err
	}
	var aggregateOwner *AggregateOwner
	if cfg.AggregateMode != "legacy" {
		owner, err := inspectAggregate(ctx, cfg.AggregateDBPath)
		if err != nil {
			return CreateReport{}, err
		}
		aggregateOwner = &owner
	}
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return CreateReport{}, fmt.Errorf("create backup output directory: %w", err)
	}
	id, err := newID()
	if err != nil {
		return CreateReport{}, err
	}
	clock := options.Now
	if clock == nil {
		clock = time.Now
	}
	stamp := clock().UTC().Format("20060102T150405Z")
	finalPath := filepath.Join(outputDir, "otelcontext-backup-"+stamp+"-"+id)
	stagingPath := finalPath + ".partial"
	if err := os.Mkdir(stagingPath, 0o750); err != nil {
		return CreateReport{}, fmt.Errorf("create unique backup staging directory: %w", err)
	}

	runner := defaultRunner(options.Runner)
	var artifacts []Artifact
	var commands []CommandRecord
	mainRelative := mainArtifactName(mainOwner.Adapter)
	mainTarget := filepath.Join(stagingPath, mainRelative)
	integrity, err := captureMainDatabase(ctx, cfg, mainTarget, runner, &commands)
	if err != nil {
		return CreateReport{}, fmt.Errorf("capture main database; incomplete bundle retained at %s: %w", stagingPath, err)
	}
	if err := syncRegular(mainTarget); err != nil {
		return CreateReport{}, fmt.Errorf("sync main database artifact; incomplete bundle retained at %s: %w", stagingPath, err)
	}
	mainOwner.ArtifactPath = filepath.ToSlash(mainRelative)
	mainOwner.Integrity = integrity
	mainArtifact, err := artifactFor(stagingPath, roleMain, mainRelative)
	if err != nil {
		return CreateReport{}, err
	}
	artifacts = append(artifacts, mainArtifact)
	if mainOwner.Adapter == "sqlite" {
		captured, err := inspectSQLiteMainArtifact(ctx, mainTarget)
		if err != nil {
			return CreateReport{}, fmt.Errorf("inspect captured main database: %w", err)
		}
		if err := compareMain(mainOwner, captured); err != nil {
			return CreateReport{}, err
		}
	}

	if aggregateOwner != nil {
		const aggregateRelative = "aggregate.sqlite"
		aggregateTarget := filepath.Join(stagingPath, aggregateRelative)
		if err := vacuumSQLite(ctx, cfg.AggregateDBPath, aggregateTarget); err != nil {
			return CreateReport{}, fmt.Errorf("capture aggregate database; incomplete bundle retained at %s: %w", stagingPath, err)
		}
		captured, err := inspectAggregate(ctx, aggregateTarget)
		if err != nil {
			return CreateReport{}, fmt.Errorf("inspect captured aggregate database: %w", err)
		}
		if err := compareAggregate(*aggregateOwner, captured); err != nil {
			return CreateReport{}, err
		}
		aggregateOwner.ArtifactPath = aggregateRelative
		aggregateOwner.Integrity = captured.Integrity
		artifact, err := artifactFor(stagingPath, roleAggregate, aggregateRelative)
		if err != nil {
			return CreateReport{}, err
		}
		artifacts = append(artifacts, artifact)
	}

	dlqOwner, dlqArtifacts, err := captureDLQ(cfg, stagingPath)
	if err != nil {
		return CreateReport{}, fmt.Errorf("capture DLQ; incomplete bundle retained at %s: %w", stagingPath, err)
	}
	artifacts = append(artifacts, dlqArtifacts...)
	tlsOwner, tlsArtifacts, err := captureTLS(cfg, stagingPath)
	if err != nil {
		return CreateReport{}, fmt.Errorf("capture generated TLS identity; incomplete bundle retained at %s: %w", stagingPath, err)
	}
	artifacts = append(artifacts, tlsArtifacts...)
	sortArtifacts(artifacts)

	configFingerprint, err := ConfigFingerprint(cfg)
	if err != nil {
		return CreateReport{}, err
	}
	lifecycle, err := lifecycleFingerprint(mainOwner, aggregateOwner, artifacts)
	if err != nil {
		return CreateReport{}, err
	}
	createdAt := clock().UTC()
	manifest := Manifest{
		SchemaVersion:        SchemaVersion,
		BackupID:             id,
		CreatedAt:            createdAt,
		Candidate:            options.Candidate,
		Mode:                 cfg.AggregateMode,
		ConfigFingerprint:    configFingerprint,
		Shutdown:             shutdown,
		Main:                 mainOwner,
		Aggregate:            aggregateOwner,
		DLQ:                  dlqOwner,
		TLS:                  tlsOwner,
		LifecycleFingerprint: lifecycle,
		Artifacts:            artifacts,
		Commands:             commands,
		Timings: Timings{
			QuiesceSeconds: shutdown.CompletedAt.Sub(shutdown.StartedAt).Seconds(),
			CaptureSeconds: time.Since(started).Seconds(),
		},
	}
	if err := validateManifestStructure(manifest); err != nil {
		return CreateReport{}, fmt.Errorf("refuse invalid generated manifest: %w", err)
	}
	manifestPath := filepath.Join(stagingPath, manifestName)
	if err := writeJSONExclusive(manifestPath, manifest, 0o600); err != nil {
		return CreateReport{}, fmt.Errorf("write manifest last: %w", err)
	}
	if err := syncDir(stagingPath); err != nil {
		return CreateReport{}, fmt.Errorf("sync completed bundle: %w", err)
	}
	if err := os.Rename(stagingPath, finalPath); err != nil {
		return CreateReport{}, fmt.Errorf("publish completed bundle atomically: %w", err)
	}
	if err := syncDir(outputDir); err != nil {
		return CreateReport{}, fmt.Errorf("sync backup output directory: %w", err)
	}
	manifestDigest, _, err := hashFile(filepath.Join(finalPath, manifestName))
	if err != nil {
		return CreateReport{}, err
	}
	return CreateReport{
		SchemaVersion:  SchemaVersion,
		Status:         "created",
		Bundle:         finalPath,
		BackupID:       id,
		ManifestSHA256: manifestDigest,
		CaptureSeconds: manifest.Timings.CaptureSeconds,
		QuiesceSeconds: manifest.Timings.QuiesceSeconds,
	}, nil
}

// Restore verifies the complete bundle and every fresh-target precondition
// before mutating any target.
func Restore(ctx context.Context, cfg Config, options RestoreOptions) (RestoreReport, error) {
	started := time.Now()
	if err := validateConfig(cfg); err != nil {
		return RestoreReport{}, err
	}
	bundle, err := absolute(options.BundleDirectory, "--bundle")
	if err != nil {
		return RestoreReport{}, err
	}
	if strings.HasSuffix(bundle, ".partial") {
		return RestoreReport{}, errors.New("restore refused: incomplete .partial bundle")
	}
	if err := validateCandidate(options.Candidate); err != nil {
		return RestoreReport{}, err
	}
	runner := defaultRunner(options.Runner)
	manifest, err := loadAndVerifyBundle(ctx, bundle, cfg, runner)
	if err != nil {
		return RestoreReport{}, err
	}
	if err := validateFreshTargets(ctx, bundle, cfg, manifest, runner); err != nil {
		return RestoreReport{}, err
	}

	var restoreCommands []CommandRecord
	mainSource := filepath.Join(bundle, filepath.FromSlash(manifest.Main.ArtifactPath))
	if err := restoreMainDatabase(ctx, manifest.Main.Adapter, mainSource, cfg.DBDSN, runner, &restoreCommands); err != nil {
		return RestoreReport{}, fmt.Errorf("restore main database: %w", err)
	}
	if manifest.Aggregate != nil {
		target, err := sqlitePath(cfg.AggregateDBPath)
		if err != nil {
			return RestoreReport{}, err
		}
		source := filepath.Join(bundle, filepath.FromSlash(manifest.Aggregate.ArtifactPath))
		if err := publishSQLiteCopy(source, target); err != nil {
			return RestoreReport{}, fmt.Errorf("restore aggregate database: %w", err)
		}
	}
	if err := restoreDLQ(bundle, cfg.DLQPath, manifest.Artifacts); err != nil {
		return RestoreReport{}, fmt.Errorf("restore DLQ: %w", err)
	}
	if manifest.TLS != nil {
		if err := restoreTLS(bundle, cfg.TLSCacheDir, *manifest.TLS); err != nil {
			return RestoreReport{}, fmt.Errorf("restore generated TLS identity: %w", err)
		}
	}

	actualMain, err := inspectMain(ctx, cfg)
	if err != nil {
		return RestoreReport{}, fmt.Errorf("inspect restored main database: %w", err)
	}
	if err := compareMain(manifest.Main, actualMain); err != nil {
		return RestoreReport{}, err
	}
	var actualAggregate *AggregateOwner
	if manifest.Aggregate != nil {
		owner, err := inspectAggregate(ctx, cfg.AggregateDBPath)
		if err != nil {
			return RestoreReport{}, fmt.Errorf("inspect restored aggregate database: %w", err)
		}
		if err := compareAggregate(*manifest.Aggregate, owner); err != nil {
			return RestoreReport{}, err
		}
		actualAggregate = &owner
	}
	actualArtifacts, err := restoredSidecarArtifacts(cfg, manifest)
	if err != nil {
		return RestoreReport{}, err
	}
	lifecycle, err := lifecycleFingerprint(actualMain, actualAggregate, actualArtifacts)
	if err != nil {
		return RestoreReport{}, err
	}
	if lifecycle != manifest.LifecycleFingerprint {
		return RestoreReport{}, fmt.Errorf("restored lifecycle fingerprint mismatch: got %s want %s", lifecycle, manifest.LifecycleFingerprint)
	}
	clock := options.Now
	if clock == nil {
		clock = time.Now
	}
	return RestoreReport{
		SchemaVersion:        SchemaVersion,
		Status:               "restored",
		Bundle:               bundle,
		BackupID:             manifest.BackupID,
		RestoreCandidate:     options.Candidate,
		BackupAgeSeconds:     clock().UTC().Sub(manifest.CreatedAt).Seconds(),
		RestoreSeconds:       time.Since(started).Seconds(),
		LifecycleFingerprint: lifecycle,
		Commands:             restoreCommands,
	}, nil
}

func validateConfig(cfg Config) error {
	driver := normalizeDriver(cfg.DBDriver)
	switch driver {
	case "sqlite", "postgres", "mysql", "mssql":
	default:
		return fmt.Errorf("unsupported DB_DRIVER %q", cfg.DBDriver)
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.AggregateMode))
	switch mode {
	case "legacy", "aggregate-shadow", "aggregate":
	default:
		return fmt.Errorf("invalid AGGREGATE_MODE %q", cfg.AggregateMode)
	}
	if cfg.AggregateMode != mode {
		return fmt.Errorf("AGGREGATE_MODE must use canonical value %q", mode)
	}
	if mode != "legacy" && strings.TrimSpace(cfg.AggregateDBPath) == "" {
		return errors.New("AGGREGATE_DB_PATH is required outside legacy mode")
	}
	if strings.TrimSpace(cfg.DLQPath) == "" || strings.TrimSpace(cfg.DataDiskPath) == "" {
		return errors.New("DLQ_PATH and DATA_DISK_PATH must not be empty")
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return errors.New("TLS_CERT_FILE and TLS_KEY_FILE must both be set or both empty")
	}
	return nil
}

func validateCandidate(candidate Candidate) error {
	if strings.TrimSpace(candidate.Version) == "" || strings.TrimSpace(candidate.Commit) == "" {
		return errors.New("candidate version and commit are required")
	}
	if len(candidate.BinarySHA256) != 64 {
		return errors.New("candidate binary_sha256 must be 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(candidate.BinarySHA256); err != nil {
		return errors.New("candidate binary_sha256 must be hexadecimal")
	}
	return nil
}

func validateOutputLocation(cfg Config, output string) error {
	dlq, err := resolved(cfg.DLQPath)
	if err != nil {
		return err
	}
	if pathWithin(output, dlq) {
		return errors.New("backup output must not be inside DLQ_PATH")
	}
	if cfg.TLSAutoSelfSigned && cfg.TLSCertFile == "" {
		tlsDir, err := resolved(cfg.TLSCacheDir)
		if err != nil {
			return err
		}
		if pathWithin(output, tlsDir) {
			return errors.New("backup output must not be inside TLS_CACHE_DIR")
		}
	}
	return nil
}

func captureDLQ(cfg Config, staging string) (DLQOwner, []Artifact, error) {
	source, err := resolved(cfg.DLQPath)
	if err != nil {
		return DLQOwner{}, nil, err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return DLQOwner{}, nil, err
	}
	if !info.IsDir() {
		return DLQOwner{}, nil, fmt.Errorf("DLQ_PATH is not a directory: %s", source)
	}
	target := filepath.Join(staging, "dlq")
	if err := os.Mkdir(target, 0o750); err != nil {
		return DLQOwner{}, nil, err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return DLQOwner{}, nil, err
	}
	var artifacts []Artifact
	var bytes int64
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		sourcePath := filepath.Join(source, entry.Name())
		if _, err := requireRegular(sourcePath); err != nil {
			return DLQOwner{}, nil, fmt.Errorf("DLQ entry %s: %w", entry.Name(), err)
		}
		relative := filepath.ToSlash(filepath.Join("dlq", entry.Name()))
		if err := copyRegular(sourcePath, filepath.Join(staging, filepath.FromSlash(relative)), 0o600); err != nil {
			return DLQOwner{}, nil, err
		}
		artifact, err := artifactFor(staging, roleDLQ, relative)
		if err != nil {
			return DLQOwner{}, nil, err
		}
		bytes += artifact.Size
		artifacts = append(artifacts, artifact)
	}
	return DLQOwner{SourceIdentity: identity(source), Count: len(artifacts), Bytes: bytes}, artifacts, nil
}

func captureTLS(cfg Config, staging string) (*TLSOwner, []Artifact, error) {
	if cfg.TLSCertFile != "" || !cfg.TLSAutoSelfSigned {
		return nil, nil, nil
	}
	source, err := resolved(cfg.TLSCacheDir)
	if err != nil {
		return nil, nil, err
	}
	certSource := filepath.Join(source, "cert.pem")
	keySource := filepath.Join(source, "key.pem")
	_, certErr := requireRegular(certSource)
	_, keyErr := requireRegular(keySource)
	if certErr != nil || keyErr != nil {
		return nil, nil, fmt.Errorf("generated TLS cache must contain both cert.pem and key.pem: cert=%v key=%v", certErr, keyErr)
	}
	if err := os.Mkdir(filepath.Join(staging, "tls"), 0o700); err != nil {
		return nil, nil, err
	}
	certRelative := "tls/cert.pem"
	keyRelative := "tls/key.pem"
	if err := copyRegular(certSource, filepath.Join(staging, filepath.FromSlash(certRelative)), 0o600); err != nil {
		return nil, nil, err
	}
	if err := copyRegular(keySource, filepath.Join(staging, filepath.FromSlash(keyRelative)), 0o600); err != nil {
		return nil, nil, err
	}
	certArtifact, err := artifactFor(staging, roleTLSCert, certRelative)
	if err != nil {
		return nil, nil, err
	}
	keyArtifact, err := artifactFor(staging, roleTLSKey, keyRelative)
	if err != nil {
		return nil, nil, err
	}
	return &TLSOwner{SourceIdentity: identity(source), Certificate: certRelative, PrivateKey: keyRelative}, []Artifact{certArtifact, keyArtifact}, nil
}

func lifecycleFingerprint(main MainOwner, aggregate *AggregateOwner, artifacts []Artifact) (string, error) {
	type sidecar struct {
		Role   string `json:"role"`
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	sidecars := make([]sidecar, 0)
	for _, artifact := range artifacts {
		if artifact.Role == roleDLQ || artifact.Role == roleTLSCert || artifact.Role == roleTLSKey {
			sidecars = append(sidecars, sidecar{artifact.Role, artifact.Path, artifact.Size, artifact.SHA256})
		}
	}
	aggregateFingerprint := "not-required"
	if aggregate != nil {
		aggregateFingerprint = aggregate.LifecycleFingerprint
	}
	return hashJSON(struct {
		Main      string    `json:"main"`
		Aggregate string    `json:"aggregate"`
		Sidecars  []sidecar `json:"sidecars"`
	}{main.LifecycleFingerprint, aggregateFingerprint, sidecars})
}

func syncRegular(path string) error {
	file, err := os.Open(path) // #nosec G304 -- newly captured staging artifact.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return file.Sync()
}

func loadAndVerifyBundle(ctx context.Context, bundle string, cfg Config, runner CommandRunner) (Manifest, error) {
	info, err := os.Lstat(bundle)
	if err != nil {
		return Manifest{}, err
	}
	if !info.IsDir() {
		return Manifest{}, errors.New("--bundle must name a directory")
	}
	manifestPath := filepath.Join(bundle, manifestName)
	manifestInfo, err := requireRegular(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("read bundle manifest: %w", err)
	}
	if manifestInfo.Size() > 4<<20 {
		return Manifest{}, errors.New("bundle manifest exceeds 4 MiB")
	}
	data, err := os.ReadFile(manifestPath) // #nosec G304 -- resolved bundle manifest.
	if err != nil {
		return Manifest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode bundle manifest: %w", err)
	}
	if err := validateManifestStructure(manifest); err != nil {
		return Manifest{}, err
	}
	wantConfig, err := ConfigFingerprint(cfg)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.ConfigFingerprint != wantConfig || manifest.Mode != cfg.AggregateMode || manifest.Main.Adapter != normalizeDriver(cfg.DBDriver) {
		return Manifest{}, errors.New("restore refused: bundle mode, adapter, or mode-critical configuration does not match the target configuration")
	}
	if err := verifyArtifactSet(bundle, manifest.Artifacts); err != nil {
		return Manifest{}, err
	}
	if err := validateMigrationCompatibility(manifest.Main); err != nil {
		return Manifest{}, err
	}
	mainPath := filepath.Join(bundle, filepath.FromSlash(manifest.Main.ArtifactPath))
	var verifyRecords []CommandRecord
	if err := verifyMainArtifact(ctx, manifest.Main.Adapter, mainPath, cfg.DBDSN, runner, &verifyRecords); err != nil {
		return Manifest{}, err
	}
	if manifest.Main.Adapter == "sqlite" {
		actual, err := inspectSQLiteMainArtifact(ctx, mainPath)
		if err != nil {
			return Manifest{}, err
		}
		if err := compareMain(manifest.Main, actual); err != nil {
			return Manifest{}, err
		}
	}
	if manifest.Aggregate != nil {
		path := filepath.Join(bundle, filepath.FromSlash(manifest.Aggregate.ArtifactPath))
		actual, err := inspectAggregate(ctx, path)
		if err != nil {
			return Manifest{}, err
		}
		if err := compareAggregate(*manifest.Aggregate, actual); err != nil {
			return Manifest{}, err
		}
	}
	computed, err := lifecycleFingerprint(manifest.Main, manifest.Aggregate, manifest.Artifacts)
	if err != nil {
		return Manifest{}, err
	}
	if computed != manifest.LifecycleFingerprint {
		return Manifest{}, errors.New("bundle lifecycle fingerprint does not match its manifest")
	}
	return manifest, nil
}

func validateManifestStructure(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported backup schema %q", manifest.SchemaVersion)
	}
	if len(manifest.BackupID) != 32 {
		return errors.New("manifest backup_id is invalid")
	}
	if _, err := hex.DecodeString(manifest.BackupID); err != nil {
		return errors.New("manifest backup_id is invalid")
	}
	if manifest.CreatedAt.IsZero() || manifest.CreatedAt.Before(manifest.Shutdown.CompletedAt) {
		return errors.New("manifest timestamps do not follow the shutdown boundary")
	}
	if err := validateCandidate(manifest.Candidate); err != nil {
		return err
	}
	if manifest.Shutdown.SchemaVersion != runtimeSchemaVersion || manifest.Shutdown.Status != "success" || manifest.Shutdown.Candidate != manifest.Candidate {
		return errors.New("manifest shutdown proof is invalid")
	}
	if err := validateShutdownSteps(manifest.Shutdown.Steps); err != nil {
		return err
	}
	if len(manifest.ConfigFingerprint) != 64 || len(manifest.LifecycleFingerprint) != 64 {
		return errors.New("manifest fingerprints are invalid")
	}
	if err := validateMigrationCompatibility(manifest.Main); err != nil {
		return err
	}
	if err := validateEngineProfile(manifest.Main.Adapter, manifest.Main.EngineVersion); err != nil {
		return err
	}
	aggregateRequired := manifest.Mode == "aggregate" || manifest.Mode == "aggregate-shadow"
	if manifest.Mode != "legacy" && !aggregateRequired {
		return fmt.Errorf("invalid manifest mode %q", manifest.Mode)
	}
	if aggregateRequired != (manifest.Aggregate != nil) {
		return errors.New("manifest aggregate inventory does not match its mode")
	}
	if manifest.Main.SourceIdentity == "" || manifest.Main.ArtifactPath == "" || manifest.DLQ.SourceIdentity == "" || manifest.DLQ.Count < 0 || manifest.DLQ.Bytes < 0 {
		return errors.New("manifest durable-owner metadata is incomplete")
	}
	roleCounts := make(map[string]int)
	paths := make(map[string]struct{})
	var dlqBytes int64
	for _, artifact := range manifest.Artifacts {
		if !safeRelative(filepath.FromSlash(artifact.Path)) || artifact.Size < 0 || len(artifact.SHA256) != 64 {
			return fmt.Errorf("invalid artifact metadata for %q", artifact.Path)
		}
		if _, err := hex.DecodeString(artifact.SHA256); err != nil {
			return fmt.Errorf("invalid artifact digest for %q", artifact.Path)
		}
		if _, duplicate := paths[artifact.Path]; duplicate {
			return fmt.Errorf("duplicate artifact path %q", artifact.Path)
		}
		paths[artifact.Path] = struct{}{}
		roleCounts[artifact.Role]++
		if artifact.Role == roleDLQ {
			dlqBytes += artifact.Size
		}
	}
	if roleCounts[roleMain] != 1 || roleCounts[roleAggregate] != boolCount(aggregateRequired) || roleCounts[roleDLQ] != manifest.DLQ.Count || dlqBytes != manifest.DLQ.Bytes {
		return errors.New("manifest artifact roles do not match durable-owner inventory")
	}
	if _, ok := paths[manifest.Main.ArtifactPath]; !ok {
		return errors.New("manifest main artifact path is absent")
	}
	if manifest.Aggregate != nil {
		if manifest.Aggregate.SourceIdentity == "" || manifest.Aggregate.StoreUUID == "" {
			return errors.New("manifest aggregate metadata is incomplete")
		}
		if _, ok := paths[manifest.Aggregate.ArtifactPath]; !ok {
			return errors.New("manifest aggregate artifact path is absent")
		}
	}
	if manifest.TLS == nil {
		if roleCounts[roleTLSCert] != 0 || roleCounts[roleTLSKey] != 0 {
			return errors.New("manifest has unowned TLS artifacts")
		}
	} else if roleCounts[roleTLSCert] != 1 || roleCounts[roleTLSKey] != 1 || manifest.TLS.SourceIdentity == "" {
		return errors.New("manifest must contain a complete generated TLS pair")
	}
	return nil
}

func verifyArtifactSet(bundle string, artifacts []Artifact) error {
	expected := map[string]Artifact{manifestName: {Path: manifestName}}
	for _, artifact := range artifacts {
		expected[filepath.Clean(filepath.FromSlash(artifact.Path))] = artifact
	}
	seen := make(map[string]struct{})
	err := filepath.WalkDir(bundle, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == bundle {
			return nil
		}
		relative, err := filepath.Rel(bundle, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundle contains symlink %q", relative)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("bundle contains non-regular file %q", relative)
		}
		artifact, ok := expected[relative]
		if !ok {
			return fmt.Errorf("bundle contains unmanifested file %q", relative)
		}
		seen[relative] = struct{}{}
		if relative == manifestName {
			return nil
		}
		digest, size, err := hashFile(path)
		if err != nil {
			return err
		}
		if digest != artifact.SHA256 || size != artifact.Size {
			return fmt.Errorf("artifact %q hash or size mismatch", artifact.Path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(expected) {
		var missing []string
		for path := range expected {
			if _, ok := seen[path]; !ok {
				missing = append(missing, path)
			}
		}
		sort.Strings(missing)
		return fmt.Errorf("bundle is missing artifacts: %s", strings.Join(missing, ", "))
	}
	return nil
}

func validateFreshTargets(ctx context.Context, bundle string, cfg Config, manifest Manifest, runner CommandRunner) error {
	mainIdentity, err := mainSourceIdentity(cfg)
	if err != nil {
		return err
	}
	if mainIdentity == manifest.Main.SourceIdentity {
		return errors.New("restore refused: main database target is the source identity")
	}
	if normalizeDriver(cfg.DBDriver) == "sqlite" {
		target, err := sqlitePath(cfg.DBDSN)
		if err != nil {
			return err
		}
		if pathWithin(target, bundle) {
			return errors.New("restore refused: main database target is inside the bundle")
		}
		if err := ensureSQLiteFresh(target); err != nil {
			return err
		}
	} else if err := validateFreshNativeTarget(ctx, normalizeDriver(cfg.DBDriver), cfg.DBDSN, runner); err != nil {
		return err
	}
	if manifest.Aggregate != nil {
		target, err := sqlitePath(cfg.AggregateDBPath)
		if err != nil {
			return err
		}
		if identity(target) == manifest.Aggregate.SourceIdentity {
			return errors.New("restore refused: aggregate target is the source identity")
		}
		if pathWithin(target, bundle) {
			return errors.New("restore refused: aggregate target is inside the bundle")
		}
		if err := ensureSQLiteFresh(target); err != nil {
			return err
		}
	}
	dlq, err := resolved(cfg.DLQPath)
	if err != nil {
		return err
	}
	if identity(dlq) == manifest.DLQ.SourceIdentity {
		return errors.New("restore refused: DLQ target is the source identity")
	}
	if pathWithin(dlq, bundle) {
		return errors.New("restore refused: DLQ target is inside the bundle")
	}
	if _, err := os.Lstat(dlq); err == nil {
		return fmt.Errorf("restore target is not fresh: %s already exists", dlq)
	} else if !os.IsNotExist(err) {
		return err
	}
	if manifest.TLS != nil {
		tlsDir, err := resolved(cfg.TLSCacheDir)
		if err != nil {
			return err
		}
		if identity(tlsDir) == manifest.TLS.SourceIdentity {
			return errors.New("restore refused: TLS cache target is the source identity")
		}
		if pathWithin(tlsDir, bundle) {
			return errors.New("restore refused: TLS target is inside the bundle")
		}
		if _, err := os.Lstat(tlsDir); err == nil {
			return fmt.Errorf("restore target is not fresh: %s already exists", tlsDir)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func restoreDLQ(bundle, target string, artifacts []Artifact) error {
	target, err := resolved(target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	partial := target + ".restore.partial"
	if err := os.Mkdir(partial, 0o750); err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if artifact.Role != roleDLQ {
			continue
		}
		name := filepath.Base(filepath.FromSlash(artifact.Path))
		if name != filepath.FromSlash(strings.TrimPrefix(artifact.Path, "dlq/")) {
			return fmt.Errorf("invalid DLQ artifact path %q", artifact.Path)
		}
		if err := copyRegular(filepath.Join(bundle, filepath.FromSlash(artifact.Path)), filepath.Join(partial, name), 0o600); err != nil {
			return err
		}
	}
	if err := syncDir(partial); err != nil {
		return err
	}
	if err := os.Rename(partial, target); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return err
	}
	return nil
}

func restoreTLS(bundle, target string, owner TLSOwner) error {
	target, err := resolved(target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	partial := target + ".restore.partial"
	if err := os.Mkdir(partial, 0o700); err != nil {
		return err
	}
	if err := copyRegular(filepath.Join(bundle, filepath.FromSlash(owner.Certificate)), filepath.Join(partial, "cert.pem"), 0o600); err != nil {
		return err
	}
	if err := copyRegular(filepath.Join(bundle, filepath.FromSlash(owner.PrivateKey)), filepath.Join(partial, "key.pem"), 0o600); err != nil {
		return err
	}
	if err := syncDir(partial); err != nil {
		return err
	}
	if err := os.Rename(partial, target); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return err
	}
	return nil
}

func restoredSidecarArtifacts(cfg Config, manifest Manifest) ([]Artifact, error) {
	var artifacts []Artifact
	for _, expected := range manifest.Artifacts {
		var path string
		switch expected.Role {
		case roleDLQ:
			path = filepath.Join(cfg.DLQPath, filepath.Base(filepath.FromSlash(expected.Path)))
		case roleTLSCert:
			path = filepath.Join(cfg.TLSCacheDir, "cert.pem")
		case roleTLSKey:
			path = filepath.Join(cfg.TLSCacheDir, "key.pem")
		default:
			continue
		}
		digest, size, err := hashFile(path)
		if err != nil {
			return nil, err
		}
		if digest != expected.SHA256 || size != expected.Size {
			return nil, fmt.Errorf("restored sidecar %q does not match manifest", expected.Path)
		}
		artifacts = append(artifacts, expected)
	}
	return artifacts, nil
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}
