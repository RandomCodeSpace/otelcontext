package config

import (
	"fmt"
	"log"
	"os"
	"sort"
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
	// shared API_KEY into per-tenant bearer tokens. JSON or YAML, chosen by
	// extension, mapping bearer key to tenant ID (several keys may map to one
	// tenant). Loaded once at startup; the process holds only SHA-256 digests.
	// A matched key's tenant BINDS the request — client-supplied X-Tenant-ID,
	// gRPC metadata, and OTLP `tenant.id` resource attributes are ignored and
	// counted. Empty = disabled (shared-key mode remains for single-tenant dev).
	APITenantKeysFile string

	// AuthTrustExternal turns on proxy-injected identity: the value of
	// AuthExternalTenantHeader is trusted as an authenticated tenant. It is
	// an authentication BYPASS unless the front proxy authenticates callers,
	// strips inbound copies of that header, and the application ports are
	// unreachable except through it — see CLAUDE.md's Authentication section.
	AuthTrustExternal bool

	// AuthExternalTenantHeader is the dedicated identity header (and gRPC
	// metadata key, lower-cased) honoured only when AuthTrustExternal is set.
	// Deliberately distinct from X-Tenant-ID, which stays client-controlled.
	AuthExternalTenantHeader string

	// WSAllowedOrigins is the WebSocket origin allowlist. Entries may be full
	// origins ("https://app.example.com") or bare hosts. Empty means
	// same-host only. Enforced when authentication is enabled or in
	// production; ignored in an unauthenticated development deployment.
	WSAllowedOrigins []string

	// GRPCReflection controls gRPC server reflection. Defaults to true outside
	// production and false in production — reflection enumerates every service
	// and message type to an unauthenticated peer.
	GRPCReflection bool

	// AllowInsecureGRPC waives the production requirement for TLS and
	// authentication on the OTLP gRPC listener. Explicit acknowledgement that
	// telemetry crosses the network unprotected and unauthenticated.
	AllowInsecureGRPC bool

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
	// Aggregate Engine Configuration (Phase 1, accounting only)
	//
	// AggregateMode controls how the aggregation engine participates:
	// - "legacy": aggregate engine inactive, use existing TSDB/GraphRAG path only
	// - "aggregate-shadow": aggregate accounting runs; counts identical to legacy;
	//   the read path uses legacy TSDB; allows A/B testing before switchover
	// - "aggregate": aggregate engine is the only active path; legacy TSDB
	//   aggregation is retired
	AggregateMode string

	// AggregateMaxSeries is the global budget for materialized active series
	// across all signals. Active = present in ≥1 mutable window.
	AggregateMaxSeries int

	// Per-signal sub-caps (materialized active series). These must sum to ≤
	// AggregateMaxSeries. Sum validation runs at startup.
	AggregateMaxSeriesMetrics int // Default 2400
	AggregateMaxSeriesTraces  int // Default 2400
	AggregateMaxSeriesEdges   int // Default 500
	AggregateMaxSeriesLogs    int // Default 500
	AggregateMaxSeriesSystem  int // Default 200

	// Per-service caps. Enforcement order: tenant → service → signal sub-cap →
	// global. These are isolation ceilings, not reservations; sum of per-service
	// budgets may exceed instance-wide cap.
	AggregateMaxOperationsPerService   int     // Default 20
	AggregateMaxTraceSeriesPerService  int     // Default 50
	AggregateMaxLogTemplatesPerService int     // Default 10
	AggregateMaxMetricSeriesPerService int     // Default 50
	AggregateSeriesPerTenantFraction   float64 // Default 0 (disabled); range [0, 1]

	// Baseline configuration for counter temporality tracking.
	// AggregateMaxProducerBaselinesPerSeries caps the number of baseline
	// entries per series (one per producer). Default 8.
	AggregateMaxProducerBaselinesPerSeries int

	// AggregateMaxBaselines is the global budget for baseline entries.
	// 0 (default) = derive as AggregateMaxSeriesMetrics ×
	// AggregateMaxProducerBaselinesPerSeries. Nonzero overrides.
	// Use ResolvedAggregateMaxBaselines() to get the final value.
	AggregateMaxBaselines int

	// Durable aggregate store (Phase 2, #173). Active whenever
	// AggregateMode != "legacy" — shadow mode persists too, because shadow
	// IS the durability rehearsal.

	// AggregateDBPath is the aggregate database file (ADR 0003: its own
	// file, its own WAL, its own PRAGMA stanza — never the main DB).
	AggregateDBPath string

	// AggregateAllowRebuild permits DESTROYING and recreating the
	// aggregate-owned tables when the on-disk schema is partial or
	// version-mismatched. Off by default: v1 has no automatic migrations and
	// a refused startup is better than silent data loss.
	AggregateAllowRebuild bool

	// AggregateSynchronous is the aggregate DB's SQLite synchronous mode,
	// "NORMAL" or "FULL". NORMAL survives process/container death (the ACK
	// contract of #160); FULL additionally survives host power loss at the
	// cost of one fsync per group commit.
	AggregateSynchronous string

	// Group-commit cadence (#160): the first waiter opens a coalescing
	// window of AggregateCommitCoalesceMs, and the commit fires early once
	// the batch reaches the delta-count or byte target.
	//
	// The default is 25 ms, not #160's provisional 5 ms. Measured on the
	// wave-5 2-vCPU acceptance run at 10k pts/s: 5 ms held the writer at a
	// 37.8% duty cycle for a 286 ms ACK p99, while 25 ms measured 109 ms p99
	// and still absorbed the 2x burst. Wider batches amortise the fixed cost
	// of a WAL commit over more deltas, which is the opposite of what the
	// latency arithmetic suggests until you notice the writer is the queue.
	AggregateCommitCoalesceMs int
	AggregateCommitMaxDeltas  int
	AggregateCommitMaxBytes   int

	// Triple admission bound (#160). A breach returns gRPC
	// RESOURCE_EXHAUSTED / HTTP 429, never a silent drop and never an
	// automatic downgrade to bounded-loss ACK.
	AggregateCommitMaxPendingBytes  int
	AggregateCommitMaxWaiters       int
	AggregateCommitMaxPendingDeltas int

	// AggregateFinalizeIntervalSec is how often the writer looks for windows
	// whose lateness horizon has expired.
	AggregateFinalizeIntervalSec int

	// Identity lifecycle (#200). The aggregate dictionary and series table
	// were append-only: every name a deployment ever emitted stayed on disk
	// long after retention purged the last bucket naming it.

	// AggregateGCEnabled runs the daily mark-and-sweep over the aggregate
	// dictionary, series and log-template tables. On by default. Turning it
	// off is a disk-growth decision, not a safety one — a pass that fails
	// leaves memory untouched.
	AggregateGCEnabled bool

	// AggregateMaxValueBytes caps the ENCODED length of a dictionary value
	// for every non-tenant kind: service names, metric names, operations,
	// dimension keys, dimension values and dimension tuples. An over-length
	// value routes to __other__ and is NEVER truncated — a truncated value is
	// a different identity wearing the same name.
	AggregateMaxValueBytes int

	// AggregateMaxTenantBytes is the stricter cap on a tenant name, and
	// AggregateMaxTenants is the instance-wide tenant-identity cap. A tenant
	// that breaches either is REJECTED: the point is refused and counted, and
	// the tenant is never collapsed into a shared identity, because a shared
	// overflow tenant is precisely the cross-tenant merge the cap prevents.
	AggregateMaxTenantBytes int
	AggregateMaxTenants     int

	// Per-tenant and instance-wide dictionary count caps for the namespaces
	// that were uncapped before #200. The per-tenant cap bounds one tenant;
	// the instance cap is the backstop for many tenants each staying just
	// under their own. Overflow routes to __other__, per existing semantics.
	AggregateMaxServicesPerTenant  int
	AggregateMaxServices           int
	AggregateMaxDimKeysPerTenant   int
	AggregateMaxDimKeys            int
	AggregateMaxDimValuesPerTenant int
	AggregateMaxDimValues          int
	AggregateMaxDimTuplesPerTenant int
	AggregateMaxDimTuples          int

	// AggregateMetricDims is the parsed AGGREGATE_METRIC_DIMS config:
	// map of metric name -> sorted list of OTLP attribute keys.
	// Empty map when AGGREGATE_METRIC_DIMS is unset/empty.
	// Populated during Load() by parsing and validating the env var.
	AggregateMetricDims map[string][]string

	// Bounded exemplar retention (#176; budgets frozen in #161).
	//
	// These apply ONLY when AggregateMode == "aggregate", where the adaptive
	// Sampler is retired and the exemplar policy is the sole raw-retention
	// gate. In legacy and aggregate-shadow the Sampler is untouched and none of
	// these values are read.
	//
	// Counts AND bytes bind; first breach wins. SamplingLatencyThresholdMs
	// stays the shared definition of "slow" in every mode.
	ExemplarTracesPerServiceWindow    int     // Default 25, unified budget with priority fill
	ExemplarTracesGlobalWindow        int     // Default 1500
	ExemplarBytesPerServiceWindow     int     // Default 524288 (512 KiB)
	ExemplarBytesGlobalWindow         int     // Default 8388608 (8 MiB)
	ExemplarHealthyRate               float64 // Default 0.005 (0.5% eligibility target)
	ExemplarStratumTopK               int     // Default 5, per (operation × status class)
	ExemplarLogsErrorPerServiceWindow int     // Default 50
	ExemplarLogsWarnEnabled           bool    // Default false — WARN is opt-in
	ExemplarLogsWarnPerServiceWindow  int     // Default 20
	ExemplarMaxSpansPerTrace          int     // Default 500
	ExemplarMaxBytesPerTrace          int     // Default 262144 (256 KiB)

	// Exemplar-tier retention and synthesized-log metering (#201 Q2/Q3).
	//
	// ExemplarRetentionDays is SHORTER than HotRetentionDays on purpose: in
	// aggregate mode the raw rows are exemplars attached to a seven-day
	// aggregate dataset, and 576 five-minute windows (two days) at the 3 MiB
	// global window budget is 1.69 GiB of charged payload — 3.38 GiB at the
	// provisional 2x DB/index/FTS amplification, inside the 4.5 GiB main tier
	// with ~1.12 GiB of margin. Seven days of the same rate does not fit.
	ExemplarRetentionDays    int // Default 2, validated 1..HotRetentionDays
	ExemplarSynthLogsPerSpan int // Default 8
	// ExemplarSynthLogsPerTrace bounds the synthesized logs one retained trace
	// may carry across all its spans.
	ExemplarSynthLogsPerTrace int // Default 64

	// Data-volume budget and disk watchdog (#201 Q1/Q5).
	//
	// DataDiskBudgetMB is the configured ceiling for everything this process
	// writes: main relational tier, aggregate.db, DLQ, WAL/temp, and the
	// mandatory unused headroom. Enforcement uses the LOWER of this and the
	// usable volume capacity — a 4 GiB PVC does not become 8 GiB because the
	// config says so.
	DataDiskBudgetMB int    // Default 8192 (8 GiB)
	DataDiskPath     string // Default ./data — any path on the data volume

	// Aggregate runtime readiness thresholds (#194 finding 18).
	//
	// Startup recovery is not the only way an aggregate deployment stops
	// being able to serve: the store can become unreachable, group commits
	// can fail in a row, admission can saturate, the finalizer can wedge and
	// the delta log can grow without bound. Each threshold below turns one of
	// those into a /ready 503. Degraded-not-dead: none of them touches
	// /live, and none of them stops the process.
	//
	// Every threshold takes 0 as "disable this probe" so an operator who
	// disagrees with a default can switch it off without patching the binary.
	ReadyMaxCommitFailureStreak   int     // READY_MAX_COMMIT_FAILURE_STREAK, default 3
	ReadyMaxFinalizeFailureStreak int     // READY_MAX_FINALIZE_FAILURE_STREAK, default 3
	ReadyMaxDeltaLogAgeS          int     // READY_MAX_DELTA_LOG_AGE_S, default 1800
	ReadyMaxAdmissionRatio        float64 // READY_MAX_ADMISSION_RATIO, default 0.9
	ReadyAggregateDiskBudgetMB    int     // READY_AGGREGATE_DISK_BUDGET_MB, default 1536
	ReadyMaxAggregateDiskRatio    float64 // READY_MAX_AGGREGATE_DISK_RATIO, default 0.9
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

		// Authenticated tenant identity (HTTP / WebSocket / gRPC)
		AuthTrustExternal:        parseTruthy(getEnv("AUTH_TRUST_EXTERNAL", "")),
		AuthExternalTenantHeader: getEnv("AUTH_EXTERNAL_TENANT_HEADER", "X-OtelContext-Tenant"),
		WSAllowedOrigins:         splitCSV(getEnv("WS_ALLOWED_ORIGINS", "")),
		GRPCReflection:           getEnvBool("GRPC_REFLECTION", env != "production"),
		AllowInsecureGRPC:        parseTruthy(getEnv("OTELCONTEXT_ALLOW_INSECURE_GRPC", "")),

		// gRPC server tuning
		GRPCMaxRecvMB:            getEnvInt("GRPC_MAX_RECV_MB", 16),
		GRPCMaxConcurrentStreams: getEnvInt("GRPC_MAX_CONCURRENT_STREAMS", 1000),

		// Production safety guard for SQLite
		AllowSqliteProd: parseTruthy(getEnv("OTELCONTEXT_ALLOW_SQLITE_PROD", "")),

		// Aggregate Engine Configuration
		AggregateMode:                          strings.ToLower(strings.TrimSpace(getEnv("AGGREGATE_MODE", "legacy"))),
		AggregateMaxSeries:                     getEnvInt("AGGREGATE_MAX_SERIES", 6000),
		AggregateMaxSeriesMetrics:              getEnvInt("AGGREGATE_MAX_SERIES_METRICS", 2400),
		AggregateMaxSeriesTraces:               getEnvInt("AGGREGATE_MAX_SERIES_TRACES", 2400),
		AggregateMaxSeriesEdges:                getEnvInt("AGGREGATE_MAX_SERIES_EDGES", 500),
		AggregateMaxSeriesLogs:                 getEnvInt("AGGREGATE_MAX_SERIES_LOGS", 500),
		AggregateMaxSeriesSystem:               getEnvInt("AGGREGATE_MAX_SERIES_SYSTEM", 200),
		AggregateMaxOperationsPerService:       getEnvInt("AGGREGATE_MAX_OPERATIONS_PER_SERVICE", 20),
		AggregateMaxTraceSeriesPerService:      getEnvInt("AGGREGATE_MAX_TRACE_SERIES_PER_SERVICE", 50),
		AggregateMaxLogTemplatesPerService:     getEnvInt("AGGREGATE_MAX_LOG_TEMPLATES_PER_SERVICE", 10),
		AggregateMaxMetricSeriesPerService:     getEnvInt("AGGREGATE_MAX_METRIC_SERIES_PER_SERVICE", 50),
		AggregateSeriesPerTenantFraction:       getEnvFloat("AGGREGATE_SERIES_PER_TENANT_FRACTION", 0),
		AggregateMaxProducerBaselinesPerSeries: getEnvInt("AGGREGATE_MAX_PRODUCER_BASELINES_PER_SERIES", 8),
		AggregateMaxBaselines:                  getEnvInt("AGGREGATE_MAX_BASELINES", 0),

		// Durable aggregate store (#173)
		AggregateDBPath:                 strings.TrimSpace(getEnv("AGGREGATE_DB_PATH", "./data/aggregate.db")),
		AggregateAllowRebuild:           parseTruthy(getEnv("AGGREGATE_ALLOW_REBUILD", "")),
		AggregateSynchronous:            strings.ToUpper(strings.TrimSpace(getEnv("AGGREGATE_SYNCHRONOUS", "NORMAL"))),
		AggregateCommitCoalesceMs:       getEnvInt("AGGREGATE_COMMIT_COALESCE_MS", 25),
		AggregateCommitMaxDeltas:        getEnvInt("AGGREGATE_COMMIT_MAX_DELTAS", 5000),
		AggregateCommitMaxBytes:         getEnvInt("AGGREGATE_COMMIT_MAX_BYTES", 8*1024*1024),
		AggregateCommitMaxPendingBytes:  getEnvInt("AGGREGATE_COMMIT_MAX_PENDING_BYTES", 64*1024*1024),
		AggregateCommitMaxWaiters:       getEnvInt("AGGREGATE_COMMIT_MAX_WAITERS", 512),
		AggregateCommitMaxPendingDeltas: getEnvInt("AGGREGATE_COMMIT_MAX_PENDING_DELTAS", 200000),
		AggregateFinalizeIntervalSec:    getEnvInt("AGGREGATE_FINALIZE_INTERVAL_SEC", 30),

		// Identity lifecycle (#200)
		AggregateGCEnabled:             getEnvBool("AGGREGATE_GC_ENABLED", true),
		AggregateMaxValueBytes:         getEnvInt("AGGREGATE_MAX_VALUE_BYTES", 512),
		AggregateMaxTenantBytes:        getEnvInt("AGGREGATE_MAX_TENANT_BYTES", 128),
		AggregateMaxTenants:            getEnvInt("AGGREGATE_MAX_TENANTS", 256),
		AggregateMaxServicesPerTenant:  getEnvInt("AGGREGATE_MAX_SERVICES_PER_TENANT", 500),
		AggregateMaxServices:           getEnvInt("AGGREGATE_MAX_SERVICES", 5000),
		AggregateMaxDimKeysPerTenant:   getEnvInt("AGGREGATE_MAX_DIM_KEYS_PER_TENANT", 200),
		AggregateMaxDimKeys:            getEnvInt("AGGREGATE_MAX_DIM_KEYS", 2000),
		AggregateMaxDimValuesPerTenant: getEnvInt("AGGREGATE_MAX_DIM_VALUES_PER_TENANT", 5000),
		AggregateMaxDimValues:          getEnvInt("AGGREGATE_MAX_DIM_VALUES", 50000),
		AggregateMaxDimTuplesPerTenant: getEnvInt("AGGREGATE_MAX_DIM_TUPLES_PER_TENANT", 5000),
		AggregateMaxDimTuples:          getEnvInt("AGGREGATE_MAX_DIM_TUPLES", 50000),
		// Bounded exemplar retention (aggregate mode only)
		ExemplarTracesPerServiceWindow: getEnvInt("EXEMPLAR_TRACES_PER_SERVICE_WINDOW", 25),
		ExemplarTracesGlobalWindow:     getEnvInt("EXEMPLAR_TRACES_GLOBAL_WINDOW", 1500),
		ExemplarBytesPerServiceWindow:  getEnvInt("EXEMPLAR_BYTES_PER_SERVICE_WINDOW", 512*1024),
		// 3 MiB, not 4 (#201 Q2). 4 MiB/window consumes the entire 4.5 GiB
		// main tier under the optimistic 2x amplification assumption and
		// leaves no operational margin; it stays configurable, it is not the
		// default until the seven-day gate (#202) proves it fits.
		ExemplarBytesGlobalWindow:         getEnvInt("EXEMPLAR_BYTES_GLOBAL_WINDOW", 3*1024*1024),
		ExemplarHealthyRate:               getEnvFloat("EXEMPLAR_HEALTHY_RATE", 0.005),
		ExemplarStratumTopK:               getEnvInt("EXEMPLAR_STRATUM_TOP_K", 5),
		ExemplarLogsErrorPerServiceWindow: getEnvInt("EXEMPLAR_LOGS_ERROR_PER_SERVICE_WINDOW", 50),
		ExemplarLogsWarnEnabled:           getEnvBool("EXEMPLAR_LOGS_WARN_ENABLED", false),
		ExemplarLogsWarnPerServiceWindow:  getEnvInt("EXEMPLAR_LOGS_WARN_PER_SERVICE_WINDOW", 20),
		ExemplarMaxSpansPerTrace:          getEnvInt("EXEMPLAR_MAX_SPANS_PER_TRACE", 500),
		ExemplarMaxBytesPerTrace:          getEnvInt("EXEMPLAR_MAX_BYTES_PER_TRACE", 256*1024),
		ExemplarRetentionDays:             getEnvInt("EXEMPLAR_RETENTION_DAYS", 2),
		ExemplarSynthLogsPerSpan:          getEnvInt("EXEMPLAR_SYNTH_LOGS_PER_SPAN", 8),
		ExemplarSynthLogsPerTrace:         getEnvInt("EXEMPLAR_SYNTH_LOGS_PER_TRACE", 64),
		// 8 GiB data budget (#201 Q1).
		DataDiskBudgetMB: getEnvInt("DATA_DISK_BUDGET_MB", 8192),
		DataDiskPath:     getEnv("DATA_DISK_PATH", "./data"),
		// Aggregate runtime readiness probes (#194 finding 18).
		//
		// Three consecutive failures, not one: a single failed group commit or
		// finalize pass is a retry, and flipping an orchestrator's readiness on
		// a retry is how a healthy process gets pulled out of rotation by a
		// transient lock.
		ReadyMaxCommitFailureStreak:   getEnvInt("READY_MAX_COMMIT_FAILURE_STREAK", 3),
		ReadyMaxFinalizeFailureStreak: getEnvInt("READY_MAX_FINALIZE_FAILURE_STREAK", 3),
		// 1800s = 2x (WindowSize 5m + AllowedLateness 10m). A window is
		// finalizable 900s after it opens, so a healthy oldest delta-log entry
		// tops out just past 900s plus one finalize tick; double that is
		// margin, not a target.
		ReadyMaxDeltaLogAgeS: getEnvInt("READY_MAX_DELTA_LOG_AGE_S", 1800),
		// 0.9, below the 0.95 the DLQ and pipeline probes use: the writer's
		// admission bound is what turns an Export into RESOURCE_EXHAUSTED, so
		// readiness should say "stop sending" before clients are being refused,
		// not while they are.
		ReadyMaxAdmissionRatio: getEnvFloat("READY_MAX_ADMISSION_RATIO", 0.9),
		// 1.5 GiB is aggregate.db's share of the 8 GiB data budget (#201 Q1).
		// The disk watchdog enforces the VOLUME; this enforces the tier, so a
		// runaway aggregate file is visible before it eats another tier's
		// allocation and takes the whole volume past 95% with it.
		ReadyAggregateDiskBudgetMB: getEnvInt("READY_AGGREGATE_DISK_BUDGET_MB", 1536),
		ReadyMaxAggregateDiskRatio: getEnvFloat("READY_MAX_AGGREGATE_DISK_RATIO", 0.9),
	}

	// Parse AGGREGATE_METRIC_DIMS config
	metricDims, err := ParseAggregateMetricDims(getEnv("AGGREGATE_METRIC_DIMS", ""))
	if err != nil {
		return nil, fmt.Errorf("parsing AGGREGATE_METRIC_DIMS: %w", err)
	}
	cfg.AggregateMetricDims = metricDims
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

// splitCSV splits a comma-separated env var into trimmed, non-empty entries.
// Returns nil (not an empty slice) for an unset or blank value so callers can
// test the "unset" case with len().
func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

	// Authenticated tenant identity.
	if c.AuthTrustExternal {
		if err := validateHeaderToken(c.AuthExternalTenantHeader); err != nil {
			return fmt.Errorf("AUTH_EXTERNAL_TENANT_HEADER: %w", err)
		}
		if strings.EqualFold(c.AuthExternalTenantHeader, "X-Tenant-ID") {
			return fmt.Errorf("AUTH_EXTERNAL_TENANT_HEADER must not be X-Tenant-ID — that header stays client-controlled; use a dedicated header your proxy strips and re-injects")
		}
	}

	// Production fail-closed: the OTLP gRPC listener must be both protected in
	// transport and authenticated. Two waivers, each named in the refusal so
	// the operator knows exactly which acknowledgement they are making.
	if c.IsProduction() && !c.AuthTrustExternal && !c.AllowInsecureGRPC {
		if !c.TLSEnabled() {
			return fmt.Errorf("APP_ENV=production requires transport protection on the OTLP gRPC listener: set TLS_CERT_FILE/TLS_KEY_FILE, or TLS_AUTO_SELFSIGNED=true, or waive with AUTH_TRUST_EXTERNAL=true (proxy-terminated TLS) or OTELCONTEXT_ALLOW_INSECURE_GRPC=true")
		}
		if !c.AuthEnabled() {
			return fmt.Errorf("APP_ENV=production requires authentication on the OTLP gRPC listener: set API_KEY or API_TENANT_KEYS_FILE, or waive with AUTH_TRUST_EXTERNAL=true (proxy-authenticated identity) or OTELCONTEXT_ALLOW_INSECURE_GRPC=true")
		}
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

	// Aggregate Engine Configuration Validation
	// No per-driver defaults for aggregate config — all drivers use the same defaults.
	validAggregateModes := map[string]bool{"legacy": true, "aggregate-shadow": true, "aggregate": true}
	if !validAggregateModes[c.AggregateMode] {
		return fmt.Errorf("invalid AGGREGATE_MODE %q: must be one of legacy, aggregate-shadow, aggregate", c.AggregateMode)
	}

	// Validate global and per-signal caps
	if c.AggregateMaxSeries < 1 {
		return fmt.Errorf("AGGREGATE_MAX_SERIES must be >= 1, got %d", c.AggregateMaxSeries)
	}
	if c.AggregateMaxSeriesMetrics < 1 {
		return fmt.Errorf("AGGREGATE_MAX_SERIES_METRICS must be >= 1, got %d", c.AggregateMaxSeriesMetrics)
	}
	if c.AggregateMaxSeriesTraces < 1 {
		return fmt.Errorf("AGGREGATE_MAX_SERIES_TRACES must be >= 1, got %d", c.AggregateMaxSeriesTraces)
	}
	if c.AggregateMaxSeriesEdges < 1 {
		return fmt.Errorf("AGGREGATE_MAX_SERIES_EDGES must be >= 1, got %d", c.AggregateMaxSeriesEdges)
	}
	if c.AggregateMaxSeriesLogs < 1 {
		return fmt.Errorf("AGGREGATE_MAX_SERIES_LOGS must be >= 1, got %d", c.AggregateMaxSeriesLogs)
	}
	if c.AggregateMaxSeriesSystem < 1 {
		return fmt.Errorf("AGGREGATE_MAX_SERIES_SYSTEM must be >= 1, got %d", c.AggregateMaxSeriesSystem)
	}

	// Validate per-service caps
	if c.AggregateMaxOperationsPerService < 1 {
		return fmt.Errorf("AGGREGATE_MAX_OPERATIONS_PER_SERVICE must be >= 1, got %d", c.AggregateMaxOperationsPerService)
	}
	if c.AggregateMaxTraceSeriesPerService < 1 {
		return fmt.Errorf("AGGREGATE_MAX_TRACE_SERIES_PER_SERVICE must be >= 1, got %d", c.AggregateMaxTraceSeriesPerService)
	}
	if c.AggregateMaxLogTemplatesPerService < 1 {
		return fmt.Errorf("AGGREGATE_MAX_LOG_TEMPLATES_PER_SERVICE must be >= 1, got %d", c.AggregateMaxLogTemplatesPerService)
	}
	if c.AggregateMaxMetricSeriesPerService < 1 {
		return fmt.Errorf("AGGREGATE_MAX_METRIC_SERIES_PER_SERVICE must be >= 1, got %d", c.AggregateMaxMetricSeriesPerService)
	}

	// Validate tenant fraction
	if c.AggregateSeriesPerTenantFraction < 0 || c.AggregateSeriesPerTenantFraction > 1.0 {
		return fmt.Errorf("AGGREGATE_SERIES_PER_TENANT_FRACTION must be between 0 and 1, got %f", c.AggregateSeriesPerTenantFraction)
	}

	// Validate baseline caps
	if c.AggregateMaxProducerBaselinesPerSeries < 1 {
		return fmt.Errorf("AGGREGATE_MAX_PRODUCER_BASELINES_PER_SERIES must be >= 1, got %d", c.AggregateMaxProducerBaselinesPerSeries)
	}
	if c.AggregateMaxBaselines > 0 && c.AggregateMaxBaselines < c.AggregateMaxProducerBaselinesPerSeries {
		return fmt.Errorf("AGGREGATE_MAX_BASELINES when nonzero must be >= AGGREGATE_MAX_PRODUCER_BASELINES_PER_SERIES (%d), got %d", c.AggregateMaxProducerBaselinesPerSeries, c.AggregateMaxBaselines)
	}

	// Durable aggregate store validation. Only enforced when the engine runs:
	// AGGREGATE_MODE=legacy constructs no store and reads none of these.
	if c.AggregateMode != "legacy" {
		if c.AggregateDBPath == "" {
			return fmt.Errorf("AGGREGATE_DB_PATH must not be empty when AGGREGATE_MODE=%s", c.AggregateMode)
		}
		if c.AggregateSynchronous != "NORMAL" && c.AggregateSynchronous != "FULL" {
			return fmt.Errorf("invalid AGGREGATE_SYNCHRONOUS %q: must be NORMAL or FULL", c.AggregateSynchronous)
		}
		commitBounds := []struct {
			name  string
			value int
		}{
			{"AGGREGATE_COMMIT_COALESCE_MS", c.AggregateCommitCoalesceMs},
			{"AGGREGATE_COMMIT_MAX_DELTAS", c.AggregateCommitMaxDeltas},
			{"AGGREGATE_COMMIT_MAX_BYTES", c.AggregateCommitMaxBytes},
			{"AGGREGATE_COMMIT_MAX_PENDING_BYTES", c.AggregateCommitMaxPendingBytes},
			{"AGGREGATE_COMMIT_MAX_WAITERS", c.AggregateCommitMaxWaiters},
			{"AGGREGATE_COMMIT_MAX_PENDING_DELTAS", c.AggregateCommitMaxPendingDeltas},
			{"AGGREGATE_FINALIZE_INTERVAL_SEC", c.AggregateFinalizeIntervalSec},
		}
		for _, b := range commitBounds {
			if b.value < 1 {
				return fmt.Errorf("%s must be >= 1, got %d", b.name, b.value)
			}
		}
		// A pending bound below the per-commit target would refuse the very
		// batch the writer is trying to build.
		if c.AggregateCommitMaxPendingDeltas < c.AggregateCommitMaxDeltas {
			return fmt.Errorf("AGGREGATE_COMMIT_MAX_PENDING_DELTAS (%d) must be >= AGGREGATE_COMMIT_MAX_DELTAS (%d)",
				c.AggregateCommitMaxPendingDeltas, c.AggregateCommitMaxDeltas)
		}
		if c.AggregateCommitMaxPendingBytes < c.AggregateCommitMaxBytes {
			return fmt.Errorf("AGGREGATE_COMMIT_MAX_PENDING_BYTES (%d) must be >= AGGREGATE_COMMIT_MAX_BYTES (%d)",
				c.AggregateCommitMaxPendingBytes, c.AggregateCommitMaxBytes)
		}
	}

	// Bounded exemplar retention validation (#176). Validated in every mode so
	// a typo is caught at startup rather than the day the operator flips
	// AGGREGATE_MODE=aggregate during an incident.
	if c.ExemplarTracesPerServiceWindow < 1 {
		return fmt.Errorf("EXEMPLAR_TRACES_PER_SERVICE_WINDOW must be >= 1, got %d", c.ExemplarTracesPerServiceWindow)
	}
	if c.ExemplarTracesGlobalWindow < c.ExemplarTracesPerServiceWindow {
		return fmt.Errorf("EXEMPLAR_TRACES_GLOBAL_WINDOW (%d) must be >= EXEMPLAR_TRACES_PER_SERVICE_WINDOW (%d): a global cap below the per-service cap makes the per-service budget unreachable", c.ExemplarTracesGlobalWindow, c.ExemplarTracesPerServiceWindow)
	}
	if c.ExemplarBytesPerServiceWindow < 1024 {
		return fmt.Errorf("EXEMPLAR_BYTES_PER_SERVICE_WINDOW must be >= 1024, got %d", c.ExemplarBytesPerServiceWindow)
	}
	if c.ExemplarBytesGlobalWindow < c.ExemplarBytesPerServiceWindow {
		return fmt.Errorf("EXEMPLAR_BYTES_GLOBAL_WINDOW (%d) must be >= EXEMPLAR_BYTES_PER_SERVICE_WINDOW (%d)", c.ExemplarBytesGlobalWindow, c.ExemplarBytesPerServiceWindow)
	}
	if c.ExemplarHealthyRate < 0 || c.ExemplarHealthyRate > 1.0 {
		return fmt.Errorf("EXEMPLAR_HEALTHY_RATE must be between 0 and 1, got %f", c.ExemplarHealthyRate)
	}
	if c.ExemplarStratumTopK < 1 {
		return fmt.Errorf("EXEMPLAR_STRATUM_TOP_K must be >= 1, got %d", c.ExemplarStratumTopK)
	}
	if c.ExemplarLogsErrorPerServiceWindow < 1 {
		return fmt.Errorf("EXEMPLAR_LOGS_ERROR_PER_SERVICE_WINDOW must be >= 1, got %d", c.ExemplarLogsErrorPerServiceWindow)
	}
	if c.ExemplarLogsWarnPerServiceWindow < 1 {
		return fmt.Errorf("EXEMPLAR_LOGS_WARN_PER_SERVICE_WINDOW must be >= 1, got %d", c.ExemplarLogsWarnPerServiceWindow)
	}
	if c.ExemplarMaxSpansPerTrace < 1 {
		return fmt.Errorf("EXEMPLAR_MAX_SPANS_PER_TRACE must be >= 1, got %d", c.ExemplarMaxSpansPerTrace)
	}
	if c.ExemplarMaxBytesPerTrace < 1024 {
		return fmt.Errorf("EXEMPLAR_MAX_BYTES_PER_TRACE must be >= 1024, got %d", c.ExemplarMaxBytesPerTrace)
	}
	if c.ExemplarRetentionDays < 1 || c.ExemplarRetentionDays > c.HotRetentionDays {
		return fmt.Errorf("EXEMPLAR_RETENTION_DAYS must be between 1 and HOT_RETENTION_DAYS (%d), got %d: the exemplar tier is a shorter-lived subset of hot retention, never a longer-lived one", c.HotRetentionDays, c.ExemplarRetentionDays)
	}
	if c.ExemplarSynthLogsPerSpan < 1 {
		return fmt.Errorf("EXEMPLAR_SYNTH_LOGS_PER_SPAN must be >= 1, got %d", c.ExemplarSynthLogsPerSpan)
	}
	if c.ExemplarSynthLogsPerTrace < c.ExemplarSynthLogsPerSpan {
		return fmt.Errorf("EXEMPLAR_SYNTH_LOGS_PER_TRACE (%d) must be >= EXEMPLAR_SYNTH_LOGS_PER_SPAN (%d): a per-trace cap below the per-span cap makes the per-span cap unreachable", c.ExemplarSynthLogsPerTrace, c.ExemplarSynthLogsPerSpan)
	}
	if c.DataDiskBudgetMB < 64 {
		return fmt.Errorf("DATA_DISK_BUDGET_MB must be >= 64, got %d", c.DataDiskBudgetMB)
	}
	if strings.TrimSpace(c.DataDiskPath) == "" {
		return fmt.Errorf("DATA_DISK_PATH must not be empty")
	}
	if c.ReadyMaxCommitFailureStreak < 0 {
		return fmt.Errorf("READY_MAX_COMMIT_FAILURE_STREAK must be >= 0 (0 disables the probe), got %d", c.ReadyMaxCommitFailureStreak)
	}
	if c.ReadyMaxFinalizeFailureStreak < 0 {
		return fmt.Errorf("READY_MAX_FINALIZE_FAILURE_STREAK must be >= 0 (0 disables the probe), got %d", c.ReadyMaxFinalizeFailureStreak)
	}
	if c.ReadyMaxDeltaLogAgeS < 0 {
		return fmt.Errorf("READY_MAX_DELTA_LOG_AGE_S must be >= 0 (0 disables the probe), got %d", c.ReadyMaxDeltaLogAgeS)
	}
	if c.ReadyMaxAdmissionRatio < 0 || c.ReadyMaxAdmissionRatio > 1 {
		return fmt.Errorf("READY_MAX_ADMISSION_RATIO must be between 0 and 1 (0 disables the probe), got %v", c.ReadyMaxAdmissionRatio)
	}
	if c.ReadyAggregateDiskBudgetMB < 0 {
		return fmt.Errorf("READY_AGGREGATE_DISK_BUDGET_MB must be >= 0 (0 disables the probe), got %d", c.ReadyAggregateDiskBudgetMB)
	}
	if c.ReadyMaxAggregateDiskRatio < 0 || c.ReadyMaxAggregateDiskRatio > 1 {
		return fmt.Errorf("READY_MAX_AGGREGATE_DISK_RATIO must be between 0 and 1 (0 disables the probe), got %v", c.ReadyMaxAggregateDiskRatio)
	}

	// Sum-of-caps validation: sub-caps must fit under global cap
	sumSubCaps := c.AggregateMaxSeriesMetrics + c.AggregateMaxSeriesTraces + c.AggregateMaxSeriesEdges + c.AggregateMaxSeriesLogs + c.AggregateMaxSeriesSystem
	if sumSubCaps > c.AggregateMaxSeries {
		return fmt.Errorf("sum of AGGREGATE_MAX_SERIES_* caps (%d = %d+%d+%d+%d+%d) must be <= AGGREGATE_MAX_SERIES (%d)", sumSubCaps, c.AggregateMaxSeriesMetrics, c.AggregateMaxSeriesTraces, c.AggregateMaxSeriesEdges, c.AggregateMaxSeriesLogs, c.AggregateMaxSeriesSystem, c.AggregateMaxSeries)
	}

	return nil
}

// IsProduction reports whether APP_ENV names the production environment.
func (c *Config) IsProduction() bool { return strings.EqualFold(c.Env, "production") }

// AuthEnabled reports whether any credential source is configured: the shared
// operator key, a per-tenant key file, or a trusted front proxy.
func (c *Config) AuthEnabled() bool {
	return c.APIKey != "" || c.APITenantKeysFile != "" || c.AuthTrustExternal
}

// EnforceWSOrigin reports whether the WebSocket origin policy applies. It does
// as soon as authentication is configured, and always in production — an
// unauthenticated development deployment keeps today's permissive behaviour.
func (c *Config) EnforceWSOrigin() bool { return c.AuthEnabled() || c.IsProduction() }

// GRPCReflectionEnabled reports whether to register gRPC server reflection.
// Production defaults to off; GRPC_REFLECTION=true re-enables it explicitly.
func (c *Config) GRPCReflectionEnabled() bool { return c.GRPCReflection }

// validateHeaderToken checks that a header name is a legal HTTP field name.
// Anything else would be silently unreachable at runtime.
func validateHeaderToken(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("must not be empty")
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("must not have leading or trailing whitespace")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_.", r):
		default:
			return fmt.Errorf("invalid character %q in header name", r)
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

// ResolvedAggregateMaxBaselines returns the effective global baseline entry cap.
// When AggregateMaxBaselines is 0 (default), derives it as AggregateMaxSeriesMetrics ×
// AggregateMaxProducerBaselinesPerSeries. Nonzero AggregateMaxBaselines overrides.
func (c *Config) ResolvedAggregateMaxBaselines() int {
	if c.AggregateMaxBaselines > 0 {
		return c.AggregateMaxBaselines
	}
	return c.AggregateMaxSeriesMetrics * c.AggregateMaxProducerBaselinesPerSeries
}

// ParseAggregateMetricDims parses the AGGREGATE_METRIC_DIMS config string.
// Format: "metric_name:key1,key2;metric_name2:key3,key4"
// Returns a map of metric name -> sorted list of dimension keys.
// Fails on malformed input (fail-closed): empty metric name, empty key list,
// duplicate metric, duplicate key within a metric, stray separators.
// Empty/unset var is valid (returns empty map).
func ParseAggregateMetricDims(s string) (map[string][]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return make(map[string][]string), nil
	}

	result := make(map[string][]string)

	// Split by semicolon to get metric:keys pairs
	metricPairs := strings.Split(s, ";")
	for _, pair := range metricPairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			return nil, fmt.Errorf("empty metric pair (stray semicolon)")
		}

		// Split by colon to get metric name and keys
		parts := strings.Split(pair, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("%q must have exactly one colon (format: metric_name:key1,key2)", pair)
		}

		metricName := strings.TrimSpace(parts[0])
		keysStr := strings.TrimSpace(parts[1])

		if metricName == "" {
			return nil, fmt.Errorf("empty metric name in %q", pair)
		}
		if keysStr == "" {
			return nil, fmt.Errorf("empty key list for metric %q", metricName)
		}

		if _, ok := result[metricName]; ok {
			return nil, fmt.Errorf("duplicate metric name %q", metricName)
		}

		// Split keys by comma
		rawKeys := strings.Split(keysStr, ",")
		seenKeys := make(map[string]bool)
		var keys []string

		for _, rawKey := range rawKeys {
			key := strings.TrimSpace(rawKey)
			if key == "" {
				return nil, fmt.Errorf("empty key in metric %q", metricName)
			}
			if seenKeys[key] {
				return nil, fmt.Errorf("duplicate key %q in metric %q", key, metricName)
			}
			seenKeys[key] = true
			keys = append(keys, key)
		}

		// Sort keys for canonical ordering
		sort.Strings(keys)
		result[metricName] = keys
	}

	return result, nil
}
