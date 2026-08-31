// Package backup owns OtelContext's offline, manifest-bound backup bundle.
package backup

import (
	"context"
	"io"
	"time"
)

const (
	// SchemaVersion is the only bundle schema this binary writes.
	SchemaVersion = "otelcontext.backup/v1"
	manifestName  = "manifest.json"

	roleMain      = "main-database"
	roleAggregate = "aggregate-database"
	roleDLQ       = "dlq"
	roleTLSCert   = "tls-certificate"
	roleTLSKey    = "tls-private-key"
)

// Config is the mode-critical and durable-owner subset of application config.
// It intentionally contains no API keys or other credentials beyond the main
// database DSN required by native database clients at execution time. The DSN
// is never serialized into a bundle or command log.
type Config struct {
	DBDriver               string
	DBDSN                  string
	DBPostgresPartitioning string
	AggregateMode          string
	AggregateDBPath        string
	DLQPath                string
	DataDiskPath           string
	TLSCertFile            string
	TLSKeyFile             string
	TLSAutoSelfSigned      bool
	TLSCacheDir            string
}

// Candidate identifies the exact executable which created or restored a
// bundle. Commit can be "unknown" for an unstamped local build; the binary
// digest remains authoritative.
type Candidate struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	BinarySHA256 string `json:"binary_sha256"`
}

// ShutdownStep is one completed owner in the successful quiescence proof.
type ShutdownStep struct {
	Name        string    `json:"name"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Error       string    `json:"error,omitempty"`
}

// ShutdownProof is written only after the service has stopped all durable
// owners and closed both databases.
type ShutdownProof struct {
	SchemaVersion     string         `json:"schema_version"`
	Status            string         `json:"status"`
	RuntimeID         string         `json:"runtime_id"`
	ConfigFingerprint string         `json:"config_fingerprint"`
	Candidate         Candidate      `json:"candidate"`
	StartedAt         time.Time      `json:"started_at"`
	CompletedAt       time.Time      `json:"completed_at"`
	Steps             []ShutdownStep `json:"steps"`
}

// Artifact binds one bundle-relative regular file to its role and digest.
type Artifact struct {
	Role   string `json:"role"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// AppliedMigration records the ordered ledger without timestamps, which are
// not compatibility inputs.
type AppliedMigration struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	Checksum string `json:"checksum"`
}

// MainOwner is the captured relational durability domain.
type MainOwner struct {
	Adapter              string             `json:"adapter"`
	EngineVersion        string             `json:"engine_version"`
	SourceIdentity       string             `json:"source_identity"`
	MigrationState       string             `json:"migration_state"`
	MigrationVersion     int                `json:"migration_version"`
	ExpectedMigration    int                `json:"expected_migration_version"`
	MigrationFingerprint string             `json:"migration_fingerprint"`
	AppliedMigrations    []AppliedMigration `json:"applied_migrations,omitempty"`
	LifecycleFingerprint string             `json:"lifecycle_fingerprint"`
	ArtifactPath         string             `json:"artifact_path"`
	Integrity            string             `json:"integrity"`
}

// AggregateOwner binds the separate aggregate SQLite durability domain.
type AggregateOwner struct {
	SourceIdentity       string `json:"source_identity"`
	StoreUUID            string `json:"store_uuid"`
	SchemaVersion        int    `json:"schema_version"`
	SeriesKeyVersion     int    `json:"series_key_version"`
	SketchCodecVersion   int    `json:"sketch_codec_version"`
	DictHighWatermark    uint64 `json:"dict_id_high_watermark"`
	SeriesHighWatermark  uint64 `json:"series_id_high_watermark"`
	LifecycleFingerprint string `json:"lifecycle_fingerprint"`
	ArtifactPath         string `json:"artifact_path"`
	Integrity            string `json:"integrity"`
}

// DLQOwner records the stopped dead-letter queue inventory.
type DLQOwner struct {
	SourceIdentity string `json:"source_identity"`
	Count          int    `json:"count"`
	Bytes          int64  `json:"bytes"`
}

// TLSOwner records the generated self-signed identity only. Explicit
// operator-owned TLS files are never copied into a bundle.
type TLSOwner struct {
	SourceIdentity string `json:"source_identity"`
	Certificate    string `json:"certificate"`
	PrivateKey     string `json:"private_key"`
}

// CommandRecord is a secret-free execution record for a native adapter.
type CommandRecord struct {
	Step       string `json:"step"`
	Command    string `json:"command"`
	DurationMS int64  `json:"duration_ms"`
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output,omitempty"`
}

// Timings are measured durations, not RPO or RTO promises.
type Timings struct {
	QuiesceSeconds float64 `json:"quiesce_seconds"`
	CaptureSeconds float64 `json:"capture_seconds"`
}

// Manifest is written last, after every captured artifact is closed, synced,
// inspected, and hashed.
type Manifest struct {
	SchemaVersion        string          `json:"schema_version"`
	BackupID             string          `json:"backup_id"`
	CreatedAt            time.Time       `json:"created_at"`
	Candidate            Candidate       `json:"candidate"`
	Mode                 string          `json:"mode"`
	ConfigFingerprint    string          `json:"config_fingerprint"`
	Shutdown             ShutdownProof   `json:"shutdown"`
	Main                 MainOwner       `json:"main"`
	Aggregate            *AggregateOwner `json:"aggregate,omitempty"`
	DLQ                  DLQOwner        `json:"dlq"`
	TLS                  *TLSOwner       `json:"tls,omitempty"`
	LifecycleFingerprint string          `json:"lifecycle_fingerprint"`
	Artifacts            []Artifact      `json:"artifacts"`
	Commands             []CommandRecord `json:"commands,omitempty"`
	Timings              Timings         `json:"timings"`
}

// CreateOptions supplies the explicit destination and test seams.
type CreateOptions struct {
	OutputDirectory string
	Candidate       Candidate
	Runner          CommandRunner
	Now             func() time.Time
}

// RestoreOptions supplies the immutable bundle and native command runner.
type RestoreOptions struct {
	BundleDirectory string
	Candidate       Candidate
	Runner          CommandRunner
	Now             func() time.Time
}

// CreateReport is printed by the one-shot create command.
type CreateReport struct {
	SchemaVersion  string  `json:"schema_version"`
	Status         string  `json:"status"`
	Bundle         string  `json:"bundle"`
	BackupID       string  `json:"backup_id"`
	ManifestSHA256 string  `json:"manifest_sha256"`
	CaptureSeconds float64 `json:"capture_seconds"`
	QuiesceSeconds float64 `json:"quiesce_seconds"`
}

// RestoreReport is printed after a fresh target has been populated and
// inspected. ReadySeconds is filled by the process-level restore drill, not
// guessed by the storage command.
type RestoreReport struct {
	SchemaVersion        string          `json:"schema_version"`
	Status               string          `json:"status"`
	Bundle               string          `json:"bundle"`
	BackupID             string          `json:"backup_id"`
	RestoreCandidate     Candidate       `json:"restore_candidate"`
	BackupAgeSeconds     float64         `json:"backup_age_at_restore_seconds"`
	RestoreSeconds       float64         `json:"restore_seconds"`
	ReadySeconds         *float64        `json:"ready_seconds,omitempty"`
	LifecycleFingerprint string          `json:"lifecycle_fingerprint"`
	Commands             []CommandRecord `json:"commands,omitempty"`
}

// Command describes one native client invocation. Display must contain no
// secret; Redactions are removed from captured errors before they leave this
// package.
type Command struct {
	Name       string
	Args       []string
	Env        []string
	StdinPath  string
	StdoutPath string
	Display    string
	Redactions []string
}

// CommandResult is the bounded observable result of a native command.
type CommandResult struct {
	Output   string
	ExitCode int
	Duration time.Duration
}

// CommandRunner executes pinned native database clients.
type CommandRunner interface {
	Run(context.Context, Command) (CommandResult, error)
}

// RuntimeHandle binds the active marker to the process that wrote it.
type RuntimeHandle struct {
	ID                string
	ConfigFingerprint string
	Candidate         Candidate
	StartedAt         time.Time
}

// outputWriter is the subset used by command tests and the CLI adapter.
type outputWriter interface {
	io.Writer
}
