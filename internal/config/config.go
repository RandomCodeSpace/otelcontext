package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Env               string
	LogLevel          string
	HTTPPort          string
	GRPCPort          string
	DBDriver          string
	DBDSN             string
	DLQPath           string
	DLQReplayInterval string

	// PprofAddr serves net/http/pprof on a dedicated listener (never the
	// public mux). Loopback-only by default; empty disables profiling.
	PprofAddr string

	// Ingestion Filtering
	IngestMinSeverity      string
	IngestAllowedServices  string
	IngestExcludedServices string

	// Storage Filtering. Logs that pass IngestMinSeverity (so they reach the
	// receiver and feed in-memory consumers like GraphRAG / Drain) but fall
	// below StoreMinSeverity are skipped during the DB persist pass — only the
	// row-write is dropped, not the in-memory enrichment. Defaults to "WARN"
	// (all drivers): INFO/DEBUG still inform anomaly detection + clustering but
	// don't grow the DB. Empty falls back to IngestMinSeverity (no second-tier
	// gate); a value <= IngestMinSeverity is a no-op since the receiver already
	// drops below that.
	StoreMinSeverity string

	// DB Connection Pool
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime string // e.g. "1h", "30m"

	// Postgres-only opt-in: declarative range partitioning of the logs table by
	// day. When set to "daily", AutoMigrate provisions logs as a partitioned
	// table and the PartitionScheduler creates lookahead partitions and drops
	// expired ones (DROP PARTITION beats DELETE for retention by orders of
	// magnitude). Greenfield only — startup refuses if `logs` already exists
	// as a non-partitioned table. Empty / "none" = legacy unpartitioned schema.
	DBPostgresPartitioning string

	// Number of future daily partitions to maintain ahead of "today" when
	// DBPostgresPartitioning=daily. Defaults to 3. Tune up if your retention
	// policy is short and ingest spikes around a daily boundary.
	DBPartitionLookaheadDays int

	// Retention
	HotRetentionDays int

	// Retention tuning. Defaults (batch=50000, sleep=1ms) work for Postgres at
	// 100k logs/sec sustained. Lower on resource-constrained hosts; raise on
	// dedicated DB machines. 0/negative values use defaults.
	RetentionBatchSize    int
	RetentionBatchSleepMs int

	// RetentionFullVacuum restores the daily full VACUUM during SQLite
	// maintenance. Default false: the daily pass runs
	// PRAGMA incremental_vacuum(10000) instead, because a full VACUUM holds
	// an exclusive lock for 10-60 minutes on multi-GB files and starves
	// ingest into a 429 storm. On-demand full VACUUM remains available via
	// POST /api/admin/vacuum. Ignored on non-SQLite drivers.
	RetentionFullVacuum bool

	// TSDB
	TSDBRingBufferDuration string // e.g. "1h"

	// Smart Observability — Adaptive Sampling
	SamplingRate               float64
	SamplingAlwaysOnErrors     bool
	SamplingLatencyThresholdMs int

	// Smart Observability — Metric Cardinality
	MetricAttributeKeys  string // comma-separated allowlist
	MetricMaxCardinality int

	// Per-tenant cardinality cap. 0 = unlimited (only the global cap
	// applies, preserving legacy single-tenant behavior). Setting this
	// gives every tenant its own series budget so a noisy tenant cannot
	// starve siblings of fresh series in the in-memory TSDB. The global
	// cap (MetricMaxCardinality) remains a backstop and is checked
	// after the per-tenant cap.
	MetricMaxCardinalityPerTenant int

	// DLQ Safety
	DLQMaxFiles   int
	DLQMaxDiskMB  int
	DLQMaxRetries int
	// DLQMaxReplayPerTick caps how many DLQ files the replay worker attempts
	// in a single tick. Without it, an outage that filled the DLQ with 10k
	// files would replay all of them in the first post-restart tick,
	// hammering the (just-restarted) DB and exhausting connections.
	// 0 = unlimited (legacy default).
	DLQMaxReplayPerTick int

	// API Protection
	APIRateLimitRPS int

	// MCP Server
	MCPEnabled bool
	MCPPath    string
	// MCPMaxConcurrent caps the in-flight tools/call invocations server-wide.
	// Beyond this, callers receive a JSON-RPC server-overloaded error. <=0
	// disables the cap. Default 32 — sized for tight agent polling loops
	// without overrunning the GraphRAG in-memory store.
	MCPMaxConcurrent int
	// MCPCallTimeoutMs is the per-invocation deadline for tools/call. A tool
	// that exceeds it gets cancelled and the client receives an RPC timeout
	// error. <=0 disables the deadline. Default 30000 (30s).
	MCPCallTimeoutMs int
	// MCPCacheTTLMs is the lifetime of a memoized tool result for the cheap
	// in-memory GraphRAG tools (get_service_map, impact_analysis, etc.).
	// <=0 disables caching. Default 5000 (5s).
	MCPCacheTTLMs int

	// Compression
	CompressionLevel string // "default", "fast", "best"

	// LogFTSEnabled toggles SQLite FTS5 provisioning + querying. The FTS5
	// inverted index typically consumes 30-40% of SQLite DB disk for
	// log-heavy workloads, while the LIKE fallback (log_repo.go:105) keeps
	// search_logs functional without it. Default false; opt in with
	// LOG_FTS_ENABLED=true. Only meaningful on SQLite; Postgres uses pg_trgm
	// independently of this flag.
	LogFTSEnabled bool

	// GraphRAG worker count (background consumers of the ingestion event channel).
	// Defaults to 4 if unset or <=0. Increase under sustained high ingest.
	GraphRAGWorkerCount int

	// GraphRAG event channel buffer size. Defaults to 10000 if unset or <=0.
	GraphRAGEventQueueSize int

	// GraphRAGTraceTTL bounds how long spans/traces stay in the in-memory
	// TraceStore before the refresh tick prunes them. Duration string, e.g.
	// "1h". Defaults to "1h"; flipped to "30m" on SQLite (the in-memory span
	// window is the largest GraphRAG heap consumer at 120 services). Anomaly
	// and investigation paths look back <=5min, so a 30min window is safe.
	GraphRAGTraceTTL string

	// GraphRAGMaxSpansPerTenant hard-caps the in-memory TraceStore span map
	// per tenant. At the cap, NEW spans are skipped (counted via
	// otelcontext_graphrag_events_dropped_total{signal="span_capacity"});
	// updates to resident spans still apply. The graph is best-effort — the
	// DB remains the source of truth. 0 = default (500000); negative
	// disables the cap.
	GraphRAGMaxSpansPerTenant int

	// GraphRAGTenantIdleTTL evicts a tenant's entire in-memory store slice
	// after this much time without any ingest event or query. Duration
	// string, default "24h". The default tenant is never evicted, and an
	// active tenant is re-created within one refresh tick (60s) from recent
	// DB spans — eviction is self-healing.
	GraphRAGTenantIdleTTL string

	// Async ingest pipeline (Phase 1 robustness work). Decouples OTLP Export
	// from synchronous DB writes. When enabled, Export() returns as soon as
	// the parsed batch is enqueued; persistence runs on a worker pool.
	//
	// Backpressure is hybrid:
	//   <90% queue       — accept all
	//   90%-100% queue   — drop healthy batches (silent), errors/slow always pass
	//   100% queue       — return RESOURCE_EXHAUSTED so OTLP clients back off
	IngestAsyncEnabled      bool // default true; opt out via INGEST_ASYNC_ENABLED=false
	IngestPipelineQueueSize int  // default 50000 batches; per-deployment tunable
	// IngestPipelineMaxBytes caps the approximate bytes held by queued
	// batches. The item-count queue size alone cannot bound memory — one
	// batch may carry arbitrarily large span/log payloads. At the cap the
	// pipeline rejects with RESOURCE_EXHAUSTED / HTTP 429 even for priority
	// (error/slow) batches: a 429 is recoverable, an OOM kill is not.
	// Default 512MB; SQLite default 128MB (see applyDriverDefaults).
	IngestPipelineMaxBytes int
	IngestPipelineWorkers  int // default 8 worker goroutines
	// IngestPipelinePerTenantCap caps in-flight batches per tenant so a noisy
	// tenant cannot starve siblings of fresh queue slots when fullness is
	// below the soft-backpressure threshold. When unset it defaults to ~30% of
	// the resolved queue size (see Load) so multi-tenant deployments are
	// protected out of the box; an explicit INGEST_PIPELINE_PER_TENANT_CAP=0
	// disables the cap for single-tenant deployments. Operators can instead
	// pin it to roughly Capacity/N where N is the expected number of
	// concurrently-active tenants, with headroom for short bursts.
	IngestPipelinePerTenantCap int

	// TLS (HTTP + gRPC). When both paths are set, TLS is enabled on both servers.
	// Empty values (default) keep plaintext behavior.
	TLSCertFile string
	TLSKeyFile  string

	// TLSAutoSelfsigned enables zero-friction self-signed TLS bootstrap for dev /
	// internal deployments. Ignored when TLSCertFile/TLSKeyFile are set (explicit
	// cert-file mode wins). Generated material is cached under TLSCacheDir.
	TLSAutoSelfsigned bool
	TLSCacheDir       string

	// API key authentication. When empty, auth middleware is a pass-through.
	// Loaded from API_KEY env var — never logged.
	APIKey string

	// OTelExporterEndpoint enables self-instrumentation. When set, the platform
	// exports its own spans to the configured OTLP endpoint (e.g. "localhost:4317"
	// for self-ingest, or an external collector).
	OTelExporterEndpoint string

	// DefaultTenant is the tenant ID assigned to rows ingested without an explicit
	// X-Tenant-ID header (HTTP) / x-tenant-id gRPC metadata.
	DefaultTenant string

	// OTLPTrustResourceTenant enables resolving the tenant from the OTLP
	// `tenant.id` resource attribute when no transport-level tenant header
	// was provided. Disabled by default because resource attributes are
	// client-controlled — a compromised SDK could set tenant.id to forge
	// another tenant's data. Only turn this on in closed environments where
	// all OTLP producers are trusted.
	OTLPTrustResourceTenant bool

	// APITenantKeysFile, when non-empty, switches API auth from a single
	// shared API_KEY into per-tenant bearer tokens. The file contains one
	// `key=tenant` pair per line; the matched key's tenant OVERRIDES any
	// X-Tenant-ID header so callers cannot cross tenants. Empty = disabled
	// (legacy shared-key mode remains available for single-tenant dev).
	APITenantKeysFile string

	// DevMode disables origin checks for WebSocket and enables dev-friendly defaults.
	// Derived from APP_ENV == "development".
	DevMode bool

	// gRPC server tuning — protects against huge OTLP batches and connection abuse.
	GRPCMaxRecvMB            int
	GRPCMaxConcurrentStreams int

	// AllowSqliteProd lets operators explicitly acknowledge that SQLite is
	// being used outside dev/test. Without it, a production Env + SQLite
	// combination refuses to start.
	AllowSqliteProd bool

	// WSMaxClients caps simultaneous WebSocket connections to /ws*
	// endpoints. 0 = unlimited (default). When set, new connections past
	// the cap receive HTTP 503. Sized for the operator's expected dashboard
	// audience — small for ops dashboards, larger for read-heavy public UIs.
	WSMaxClients int
}

func Load(customPath string) (*Config, error) {
	envFile := ".env"
	if customPath != "" {
		envFile = customPath
	}

	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		if err := godotenv.Load(envFile); err != nil {
			log.Println("⚠️  Failed to load .env file, using system environment variables or defaults")
		} else {
			log.Println("✅ Loaded configuration from .env")
		}
	} else {
		log.Println("⚠️  No .env file found, using system environment variables or defaults")
	}

	env := getEnv("APP_ENV", "development")
	cfg := &Config{
		Env:               env,
		DevMode:           env == "development",
		LogLevel:          getEnv("LOG_LEVEL", "INFO"),
		HTTPPort:          getEnv("HTTP_PORT", "8080"),
		GRPCPort:          getEnv("GRPC_PORT", "4317"),
		DBDriver:          getEnv("DB_DRIVER", "sqlite"),
		DBDSN:             getEnv("DB_DSN", ""),
		DLQPath:           getEnv("DLQ_PATH", "./data/dlq"),
		DLQReplayInterval: getEnv("DLQ_REPLAY_INTERVAL", "5m"),
		PprofAddr:         getEnv("PPROF_ADDR", "127.0.0.1:6060"),

		IngestMinSeverity:      getEnv("INGEST_MIN_SEVERITY", "INFO"),
		StoreMinSeverity:       getEnv("STORE_MIN_SEVERITY", "WARN"),
		IngestAllowedServices:  getEnv("INGEST_ALLOWED_SERVICES", ""),
		IngestExcludedServices: getEnv("INGEST_EXCLUDED_SERVICES", ""),

		// DB Connection Pool
		DBMaxOpenConns:    getEnvInt("DB_MAX_OPEN_CONNS", 50),
		DBMaxIdleConns:    getEnvInt("DB_MAX_IDLE_CONNS", 10),
		DBConnMaxLifetime: getEnv("DB_CONN_MAX_LIFETIME", "1h"),

		// Postgres partitioning (opt-in). Default empty = legacy unpartitioned.
		DBPostgresPartitioning:   strings.ToLower(strings.TrimSpace(getEnv("DB_POSTGRES_PARTITIONING", ""))),
		DBPartitionLookaheadDays: getEnvInt("DB_PARTITION_LOOKAHEAD_DAYS", 3),

		// Retention
		HotRetentionDays:      getEnvInt("HOT_RETENTION_DAYS", 7),
		RetentionBatchSize:    getEnvInt("RETENTION_BATCH_SIZE", 50000),
		RetentionBatchSleepMs: getEnvInt("RETENTION_BATCH_SLEEP_MS", 1),
		RetentionFullVacuum:   getEnvBool("RETENTION_FULL_VACUUM", false),

		// TSDB
		TSDBRingBufferDuration: getEnv("TSDB_RING_BUFFER_DURATION", "1h"),

		// Adaptive Sampling
		SamplingRate:               getEnvFloat("SAMPLING_RATE", 1.0), // default: keep all
		SamplingAlwaysOnErrors:     getEnvBool("SAMPLING_ALWAYS_ON_ERRORS", true),
		SamplingLatencyThresholdMs: getEnvInt("SAMPLING_LATENCY_THRESHOLD_MS", 500),

		// Cardinality
		MetricAttributeKeys:           getEnv("METRIC_ATTRIBUTE_KEYS", ""),
		MetricMaxCardinality:          getEnvInt("METRIC_MAX_CARDINALITY", 10000),
		MetricMaxCardinalityPerTenant: getEnvInt("METRIC_MAX_CARDINALITY_PER_TENANT", 0),

		// DLQ
		DLQMaxFiles:         getEnvInt("DLQ_MAX_FILES", 1000),
		DLQMaxDiskMB:        getEnvInt("DLQ_MAX_DISK_MB", 500),
		DLQMaxRetries:       getEnvInt("DLQ_MAX_RETRIES", 10),
		DLQMaxReplayPerTick: getEnvInt("DLQ_MAX_REPLAY_PER_TICK", 100),

		// API
		APIRateLimitRPS: getEnvInt("API_RATE_LIMIT_RPS", 100),

		// MCP
		MCPEnabled:       getEnvBool("MCP_ENABLED", true),
		MCPPath:          getEnv("MCP_PATH", "/mcp"),
		MCPMaxConcurrent: getEnvInt("MCP_MAX_CONCURRENT", 32),
		MCPCallTimeoutMs: getEnvInt("MCP_CALL_TIMEOUT_MS", 30000),
		MCPCacheTTLMs:    getEnvInt("MCP_CACHE_TTL_MS", 5000),

		// Compression
		CompressionLevel: getEnv("COMPRESSION_LEVEL", "default"),

		// Log search FTS5 toggle (SQLite only). Default off — see field comment.
		LogFTSEnabled: parseTruthy(getEnv("LOG_FTS_ENABLED", "")),

		// GraphRAG
		GraphRAGWorkerCount:       getEnvInt("GRAPHRAG_WORKER_COUNT", 16),
		GraphRAGEventQueueSize:    getEnvInt("GRAPHRAG_EVENT_QUEUE_SIZE", 100000),
		GraphRAGTraceTTL:          getEnv("GRAPHRAG_TRACE_TTL", "1h"),
		GraphRAGMaxSpansPerTenant: getEnvInt("GRAPHRAG_MAX_SPANS_PER_TENANT", 500000),
		GraphRAGTenantIdleTTL:     getEnv("GRAPHRAG_TENANT_IDLE_TTL", "24h"),

		// Async ingest pipeline
		IngestAsyncEnabled:         getEnvBool("INGEST_ASYNC_ENABLED", true),
		IngestPipelineQueueSize:    getEnvInt("INGEST_PIPELINE_QUEUE_SIZE", 50000),
		IngestPipelineMaxBytes:     getEnvInt("INGEST_PIPELINE_MAX_BYTES", 512<<20),
		IngestPipelineWorkers:      getEnvInt("INGEST_PIPELINE_WORKERS", 8),
		IngestPipelinePerTenantCap: getEnvInt("INGEST_PIPELINE_PER_TENANT_CAP", 0),

		// TLS
		TLSCertFile:       getEnv("TLS_CERT_FILE", ""),
		TLSKeyFile:        getEnv("TLS_KEY_FILE", ""),
		TLSAutoSelfsigned: parseTruthy(getEnv("TLS_AUTO_SELFSIGNED", "")),
		TLSCacheDir:       getEnv("TLS_CACHE_DIR", "./data/tls"),

		// Auth
		APIKey: getEnv("API_KEY", ""),

		// OTel self-instrumentation
		OTelExporterEndpoint: getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),

		// WebSocket admission cap
		WSMaxClients: getEnvInt("WS_MAX_CLIENTS", 0),

		// Multi-tenancy
		DefaultTenant:           getEnv("DEFAULT_TENANT", "default"),
		OTLPTrustResourceTenant: parseTruthy(getEnv("OTLP_TRUST_RESOURCE_TENANT", "")),
		APITenantKeysFile:       getEnv("API_TENANT_KEYS_FILE", ""),

		// gRPC server tuning
		GRPCMaxRecvMB:            getEnvInt("GRPC_MAX_RECV_MB", 16),
		GRPCMaxConcurrentStreams: getEnvInt("GRPC_MAX_CONCURRENT_STREAMS", 1000),

		// Production safety guard for SQLite
		AllowSqliteProd: parseTruthy(getEnv("OTELCONTEXT_ALLOW_SQLITE_PROD", "")),
	}
	applyDriverDefaults(cfg)

	// Derive a sane per-tenant ingest cap when the operator did not set one.
	// Run AFTER applyDriverDefaults so it tracks the (possibly SQLite-adjusted)
	// queue size: ~30% of the queue lets a single tenant burst but stops one
	// noisy tenant from monopolising every slot at 100–200 services. An explicit
	// INGEST_PIPELINE_PER_TENANT_CAP=0 is respected as "disabled".
	if _, set := os.LookupEnv("INGEST_PIPELINE_PER_TENANT_CAP"); !set && cfg.IngestPipelinePerTenantCap == 0 {
		cfg.IngestPipelinePerTenantCap = cfg.IngestPipelineQueueSize * 30 / 100
	}

	return cfg, nil
}

// applyDriverDefaults flips defaults on a freshly-Load()'d Config when the
// driver is SQLite AND the operator did not explicitly set the env var.
// Postgres/MSSQL/MySQL defaults are unchanged.
//
// The platform's stock defaults are tuned for Postgres at 100k events/sec
// with a parallel writer pool. On SQLite those same defaults overrun the
// single-writer lock and inflate heap until the process OOMs — see
// docs/superpowers/specs/2026-05-24-mcp-7tool-sqlite-survival-design.md.
// This override gives the SQLite path a survivable starting point at
// 120 services while preserving the existing Postgres path bit-for-bit.
//
// "Explicit operator override" is detected via os.LookupEnv (presence)
// rather than value comparison so that, e.g., DB_MAX_OPEN_CONNS=50 set by
// hand is still honoured even though it equals the Postgres default.
// sqliteOverrides is the table of (env-var, apply) pairs that
// applyDriverDefaults walks when DB_DRIVER=sqlite. Add a row here to
// introduce a new SQLite-only default; the apply closure is the only place
// that names the Config field, so the surrounding lookup/skip logic stays
// in one spot.
var sqliteOverrides = []struct {
	envKey string
	apply  func(*Config)
}{
	{"DB_MAX_OPEN_CONNS", func(c *Config) { c.DBMaxOpenConns = 1 }},
	{"DB_MAX_IDLE_CONNS", func(c *Config) { c.DBMaxIdleConns = 1 }},
	{"INGEST_PIPELINE_WORKERS", func(c *Config) { c.IngestPipelineWorkers = 2 }},
	{"INGEST_PIPELINE_QUEUE_SIZE", func(c *Config) { c.IngestPipelineQueueSize = 10000 }},
	// The SQLite single writer drains slowly, so the ingest queue is the
	// first structure to bloat — bound it to 128MB instead of 512MB.
	{"INGEST_PIPELINE_MAX_BYTES", func(c *Config) { c.IngestPipelineMaxBytes = 128 << 20 }},
	{"METRIC_MAX_CARDINALITY", func(c *Config) { c.MetricMaxCardinality = 3000 }},
	{"SAMPLING_RATE", func(c *Config) { c.SamplingRate = 0.05 }},
	{"GRPC_MAX_CONCURRENT_STREAMS", func(c *Config) { c.GRPCMaxConcurrentStreams = 240 }},
	{"LOG_FTS_ENABLED", func(c *Config) { c.LogFTSEnabled = true }},
	// Each queued event embeds a storage.Span/Log by value (~0.5–2 KB); the
	// 100k Postgres default is ~100 MB+ of standing buffer. On SQLite the
	// single writer starves the workers anyway — drop sooner (metered via
	// otelcontext_graphrag_events_dropped_total) instead of buffering RAM.
	{"GRAPHRAG_EVENT_QUEUE_SIZE", func(c *Config) { c.GraphRAGEventQueueSize = 10000 }},
	// The TraceStore span window dominates GraphRAG heap at 120 services
	// (~1.5 GB potential at 1h). Anomaly/investigation lookbacks are <=5min,
	// so halving the window costs nothing they rely on; MCP trace tools fall
	// through to the DB for older traces.
	{"GRAPHRAG_TRACE_TTL", func(c *Config) { c.GraphRAGTraceTTL = "30m" }},
}

func applyDriverDefaults(cfg *Config) {
	if !strings.EqualFold(cfg.DBDriver, "sqlite") {
		return
	}
	for _, ov := range sqliteOverrides {
		if _, ok := os.LookupEnv(ov.envKey); !ok {
			ov.apply(cfg)
		}
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, exists := os.LookupEnv(key); exists {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if v, exists := os.LookupEnv(key); exists {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

// parseTruthy accepts common truthy spellings, case-insensitive, trimmed.
// Used for env vars whose canonical value is `true` but where operators
// often type `1`, `yes`, or `on`.
func parseTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func getEnvBool(key string, fallback bool) bool {
	if v, exists := os.LookupEnv(key); exists {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

// Validate checks that all configuration values are within valid ranges.
// Call this once after Load() during startup to catch misconfiguration early.
func (c *Config) Validate() error {
	// Port validation
	httpPort, err := strconv.Atoi(c.HTTPPort)
	if err != nil || httpPort < 1 || httpPort > 65535 {
		return fmt.Errorf("invalid HTTP_PORT %q: must be 1-65535", c.HTTPPort)
	}
	grpcPort, err := strconv.Atoi(c.GRPCPort)
	if err != nil || grpcPort < 1 || grpcPort > 65535 {
		return fmt.Errorf("invalid GRPC_PORT %q: must be 1-65535", c.GRPCPort)
	}

	// DB driver
	validDrivers := map[string]bool{
		"sqlite": true, "postgres": true, "postgresql": true,
		"mysql": true, "mssql": true, "sqlserver": true,
	}
	if !validDrivers[strings.ToLower(c.DBDriver)] {
		return fmt.Errorf("invalid DB_DRIVER %q: must be one of sqlite, postgres, mysql, mssql", c.DBDriver)
	}

	// Partitioning is Postgres-only. Reject mismatched configs at startup so
	// the operator finds out immediately rather than silently running in
	// unpartitioned mode.
	switch c.DBPostgresPartitioning {
	case "", "none", "daily":
		// ok
	default:
		return fmt.Errorf("invalid DB_POSTGRES_PARTITIONING %q: must be one of \"\", \"none\", \"daily\"", c.DBPostgresPartitioning)
	}
	if c.DBPostgresPartitioning == "daily" {
		drv := strings.ToLower(c.DBDriver)
		if drv != "postgres" && drv != "postgresql" {
			return fmt.Errorf("DB_POSTGRES_PARTITIONING=daily requires DB_DRIVER=postgres, got %q", c.DBDriver)
		}
	}
	// 0 == "use default at the storage layer" so direct struct construction
	// (tests, embedded callers) doesn't have to set it.
	if c.DBPartitionLookaheadDays < 0 || c.DBPartitionLookaheadDays > 365 {
		return fmt.Errorf("DB_PARTITION_LOOKAHEAD_DAYS must be between 0 and 365, got %d", c.DBPartitionLookaheadDays)
	}

	// MCP robustness knobs. 0 is the documented sentinel for "disable" on
	// each axis; negative values are nonsensical (clamping to 0 silently
	// would mask typos like MCP_MAX_CONCURRENT=-1). Reject explicitly.
	if c.MCPMaxConcurrent < 0 {
		return fmt.Errorf("MCP_MAX_CONCURRENT must be >= 0 (0 disables the cap), got %d", c.MCPMaxConcurrent)
	}
	if c.MCPCallTimeoutMs < 0 {
		return fmt.Errorf("MCP_CALL_TIMEOUT_MS must be >= 0 (0 disables the deadline), got %d", c.MCPCallTimeoutMs)
	}
	if c.MCPCacheTTLMs < 0 {
		return fmt.Errorf("MCP_CACHE_TTL_MS must be >= 0 (0 disables the cache), got %d", c.MCPCacheTTLMs)
	}

	// Numeric ranges.
	// Upper bound on HOT_RETENTION_DAYS guards against int64 nanosecond overflow in
	// time.Duration(days) * 24 * time.Hour (overflow above ~106751 days flips the
	// cutoff into the future and deletes everything). 36500 (100y) is generous.
	if c.HotRetentionDays < 1 || c.HotRetentionDays > 36500 {
		return fmt.Errorf("HOT_RETENTION_DAYS must be between 1 and 36500, got %d", c.HotRetentionDays)
	}
	if c.RetentionBatchSize < 1 || c.RetentionBatchSize > 10_000_000 {
		return fmt.Errorf("RETENTION_BATCH_SIZE must be between 1 and 10000000, got %d", c.RetentionBatchSize)
	}
	if c.RetentionBatchSleepMs < 0 || c.RetentionBatchSleepMs > 60_000 {
		return fmt.Errorf("RETENTION_BATCH_SLEEP_MS must be between 0 and 60000, got %d", c.RetentionBatchSleepMs)
	}
	if c.MetricMaxCardinality < 0 {
		return fmt.Errorf("METRIC_MAX_CARDINALITY must be >= 0, got %d", c.MetricMaxCardinality)
	}
	if c.MetricMaxCardinalityPerTenant < 0 {
		return fmt.Errorf("METRIC_MAX_CARDINALITY_PER_TENANT must be >= 0, got %d", c.MetricMaxCardinalityPerTenant)
	}
	if c.SamplingRate < 0 || c.SamplingRate > 1.0 {
		return fmt.Errorf("SAMPLING_RATE must be between 0 and 1, got %f", c.SamplingRate)
	}
	if c.APIRateLimitRPS < 0 {
		return fmt.Errorf("API_RATE_LIMIT_RPS must be >= 0, got %d", c.APIRateLimitRPS)
	}
	// gRPC receive cap: must be positive, and capped to prevent per-message OOM
	// from a bad env value (the limit pre-allocates a buffer of this size on
	// the first large message). 256 MiB is far beyond any legitimate OTLP batch
	// and still small enough that a 200-connection flood cannot exhaust a host
	// with typical RAM.
	if c.GRPCMaxRecvMB < 1 || c.GRPCMaxRecvMB > 256 {
		return fmt.Errorf("GRPC_MAX_RECV_MB must be between 1 and 256, got %d", c.GRPCMaxRecvMB)
	}
	if c.GRPCMaxConcurrentStreams < 1 || c.GRPCMaxConcurrentStreams > 1_000_000 {
		return fmt.Errorf("GRPC_MAX_CONCURRENT_STREAMS must be between 1 and 1000000, got %d", c.GRPCMaxConcurrentStreams)
	}
	// GraphRAG event queue: the channel buffer is allocated up front and each
	// queued event embeds a Span/Log by value (~0.5-2 KB), so an unbounded env
	// value is a real OOM lever. 1M buffered events is already ~1-2 GB.
	if c.GraphRAGEventQueueSize < 1 || c.GraphRAGEventQueueSize > 1_000_000 {
		return fmt.Errorf("GRAPHRAG_EVENT_QUEUE_SIZE must be between 1 and 1000000, got %d", c.GraphRAGEventQueueSize)
	}
	if c.DBMaxOpenConns < 1 {
		return fmt.Errorf("DB_MAX_OPEN_CONNS must be >= 1, got %d", c.DBMaxOpenConns)
	}
	if c.DBMaxIdleConns < 0 {
		return fmt.Errorf("DB_MAX_IDLE_CONNS must be >= 0, got %d", c.DBMaxIdleConns)
	}

	// Compression level
	switch strings.ToLower(c.CompressionLevel) {
	case "default", "fast", "best":
	default:
		return fmt.Errorf("invalid COMPRESSION_LEVEL %q: must be one of default, fast, best", c.CompressionLevel)
	}

	// Per-tenant API keys: warn loudly when the operator configured a non-
	// default tenant but left API_TENANT_KEYS_FILE empty — the shared API_KEY
	// + self-asserted X-Tenant-ID header model lets any key holder read any
	// tenant's data, which is almost never what a multi-tenant install wants.
	if c.APITenantKeysFile == "" && c.DefaultTenant != "" && c.DefaultTenant != "default" {
		log.Printf("⚠️  API_TENANT_KEYS_FILE is empty but DEFAULT_TENANT=%q — shared API_KEY permits any holder to read any tenant's data. Set API_TENANT_KEYS_FILE to enforce per-tenant auth.", c.DefaultTenant)
	}

	// TLS: both paths must be set together, and both files must exist & be readable.
	certSet := c.TLSCertFile != ""
	keySet := c.TLSKeyFile != ""
	if certSet != keySet {
		return fmt.Errorf("TLS_CERT_FILE and TLS_KEY_FILE must both be set or both empty")
	}
	if certSet {
		if err := checkReadable(c.TLSCertFile); err != nil {
			return fmt.Errorf("TLS_CERT_FILE %q: %w", c.TLSCertFile, err)
		}
		if err := checkReadable(c.TLSKeyFile); err != nil {
			return fmt.Errorf("TLS_KEY_FILE %q: %w", c.TLSKeyFile, err)
		}
		// Precedence notice: explicit cert files override auto-selfsigned.
		if c.TLSAutoSelfsigned {
			log.Println("ℹ️  TLS_AUTO_SELFSIGNED ignored — explicit TLS_CERT_FILE/TLS_KEY_FILE take precedence")
		}
	}

	return nil
}

// TLSEnabled reports whether HTTPS + gRPC-TLS should be served using any
// mode (explicit files or auto self-signed).
func (c *Config) TLSEnabled() bool {
	return c.TLSCertFileMode() || c.TLSSelfsignedMode()
}

// TLSCertFileMode reports whether explicit cert-file TLS is configured.
// This path has precedence over self-signed.
func (c *Config) TLSCertFileMode() bool {
	return c.TLSCertFile != "" && c.TLSKeyFile != ""
}

// TLSSelfsignedMode reports whether the self-signed bootstrap path should
// be used. False when explicit cert files are set (cert-file wins).
func (c *Config) TLSSelfsignedMode() bool {
	if c.TLSCertFileMode() {
		return false
	}
	return c.TLSAutoSelfsigned
}

// checkReadable verifies the file exists and can be opened for reading.
func checkReadable(path string) error {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied TLS material path
	if err != nil {
		return err
	}
	return f.Close()
}

// ValidateDBForEnv refuses the combination of SQLite driver + production
// environment unless AllowSqliteProd is explicitly set. SQLite's single-writer
// lock caps sustained throughput to ~5 services; using it in production will
// silently throttle ingestion.
//
// Call once during startup after Load + Validate.
func (c *Config) ValidateDBForEnv() error {
	if !strings.EqualFold(c.DBDriver, "sqlite") {
		return nil
	}
	if strings.EqualFold(c.Env, "production") && !c.AllowSqliteProd {
		return fmt.Errorf("SQLite is unsuitable for APP_ENV=production " +
			"(single-writer lock caps throughput at ~5 services). " +
			"Use DB_DRIVER=postgres, or set OTELCONTEXT_ALLOW_SQLITE_PROD=true to acknowledge")
	}
	return nil
}
