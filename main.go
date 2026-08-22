package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/ai"
	"github.com/RandomCodeSpace/otelcontext/internal/api"
	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/graph"
	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/ingest"
	"github.com/RandomCodeSpace/otelcontext/internal/mcp"
	"github.com/RandomCodeSpace/otelcontext/internal/queue"
	"github.com/RandomCodeSpace/otelcontext/internal/realtime"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	tlsbootstrap "github.com/RandomCodeSpace/otelcontext/internal/tls"
	"github.com/RandomCodeSpace/otelcontext/internal/tsdb"
	"github.com/RandomCodeSpace/otelcontext/internal/ui"

	"runtime/debug"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.25.0"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	_ "google.golang.org/grpc/encoding/gzip" // Register gzip decompressor
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// Version is detected from build info at startup.
// Returns the real tag when installed via `go install`, "local" otherwise.
var Version = detectVersion()

// detectVersion reads runtime/debug.BuildInfo to return the module version
// that go install or go build stamped into the binary. Falls back to "local"
// for go run, raw go build, or any path that does not produce a stamped
// build (e.g. `(devel)` from module-aware development builds).
func detectVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "local"
}

// cleanupStack is an ordered LIFO list of cleanup closures registered during
// startup. fatal() walks it before os.Exit so DBs, DLQs, and tracer providers
// get a chance to flush even on a fatal error. Each fn should be non-blocking
// or have its own bounded timeout.
var (
	cleanupMu    sync.Mutex
	cleanupStack []func()
)

// RegisterCleanup pushes a cleanup closure onto the LIFO stack. Exported so
// future startup helpers outside main can enroll resources; the stack is
// walked by fatal() on failed boot.
func RegisterCleanup(fn func()) {
	cleanupMu.Lock()
	cleanupStack = append(cleanupStack, fn)
	cleanupMu.Unlock()
}

// runCleanups pops and invokes cleanup closures in LIFO order.
func runCleanups() {
	cleanupMu.Lock()
	fns := cleanupStack
	cleanupStack = nil
	cleanupMu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("cleanup panic", "panic", r)
				}
			}()
			fns[i]()
		}()
	}
}

// fatal replaces scattered log.Fatalf calls. It emits a structured error,
// runs any registered cleanups in LIFO order, and exits 1. Extra key/value
// pairs are passed straight through to slog.Error.
func fatal(msg string, err error, kv ...any) {
	args := append([]any{slog.Any("error", err)}, kv...)
	slog.Error(msg, args...)
	runCleanups()
	os.Exit(1)
}

func main() {
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("OtelContext version %s\n", Version)
		os.Exit(0)
	}

	// Force UTC timezone globally — prevents system timezone leaking into timestamps
	time.Local = time.UTC

	printBanner()

	// Top-level application context used by boot-time background goroutines
	// (e.g. vector-index hydrator) so they can be cancelled before the DB closes.
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	// WaitGroup for boot-time goroutines whose completion must be awaited
	// during shutdown (vector index hydrator, DB health poller).
	var bootWG sync.WaitGroup

	// 0. Load Configuration
	cfg, err := config.Load("")
	if err != nil {
		fatal("failed to load configuration", err)
	}
	if err := cfg.Validate(); err != nil {
		fatal("invalid configuration", err)
	}
	// Auto-exclude own service when self-instrumentation points to a loopback
	// address (otherwise every span emitted re-enters Export and amplifies).
	cfg.GuardSelfInstrumentation()
	if err := cfg.ValidateDBForEnv(); err != nil {
		fatal("DB/Env validation", err)
	}
	// Authenticated tenant identity (#194 blockers 7 + 8). Loaded once, at
	// startup: swapping the key file is an explicit restart, never a live
	// reload, and the process keeps only SHA-256 digests of the keys.
	tenantKeys, err := authn.LoadKeyStore(cfg.APITenantKeysFile)
	if err != nil {
		fatal("load tenant keys file", err, "path", cfg.APITenantKeysFile)
	}
	authenticator := authn.NewAuthenticator(cfg.APIKey, tenantKeys, cfg.AuthTrustExternal)
	authnMetrics := telemetry.NewAuthnMetrics()
	authn.ConflictHook = func(surface, reason string) {
		authnMetrics.TenantConflictsTotal.WithLabelValues(surface, reason).Inc()
	}
	if strings.EqualFold(cfg.DBDriver, "sqlite") {
		slog.Warn("SQLite driver in use. Auto-tuned defaults survive ~50-120 services " +
			"on a 4 GB host with 7-day retention. Switch to Postgres beyond that band, " +
			"or for sustained >50 writes/sec. See README 'Production sizing'.")
	}

	// Initialize structured logger
	var level slog.Level
	switch strings.ToUpper(cfg.LogLevel) {
	case "DEBUG":
		level = slog.LevelDebug
	case "WARN":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)

	slog.Info("🚀 Starting OtelContext", "version", Version, "env", cfg.Env, "log_level", level)

	// Pace the GC against a soft memory ceiling so RSS stays bounded under
	// sustained ingest (honors an explicit GOMEMLIMIT; otherwise 75% of the
	// detected cgroup/host budget). See applyMemoryLimit in memlimit.go.
	applyMemoryLimit(75)

	// Profiling is an observability aid — a busy port must not abort startup.
	pprofSrv, _, err := startPprofServer(cfg.PprofAddr, logger)
	if err != nil {
		slog.Warn("pprof server disabled", "error", err, "addr", cfg.PprofAddr)
	}

	// 1. Initialize Internal Telemetry (first — everything registers metrics against this)
	metrics := telemetry.New()
	slog.Info("📊 Internal telemetry initialized")

	// 1b. Initialize OTel self-instrumentation (optional)
	var shutdownTracer func(context.Context) error
	if cfg.OTelExporterEndpoint != "" {
		tp, err := initTracerProvider(cfg.OTelExporterEndpoint)
		if err != nil {
			slog.Error("Failed to initialize OTel tracer provider", "error", err, "endpoint", cfg.OTelExporterEndpoint)
		} else {
			otel.SetTracerProvider(tp)
			shutdownTracer = tp.Shutdown
			slog.Info("🔭 OTel self-instrumentation enabled", "endpoint", cfg.OTelExporterEndpoint)
		}
	}

	// 2. Initialize Storage
	repo, err := storage.NewRepository(metrics)
	if err != nil {
		fatal("Failed to initialize repository", err)
	}
	slog.Info("💾 Storage initialized", "driver", cfg.DBDriver)

	// 2a. Retention scheduler: hourly batched purge + daily maintenance
	// (VACUUM ANALYZE / OPTIMIZE / PRAGMA optimize + incremental_vacuum).
	ctxRetention, cancelRetention := context.WithCancel(context.Background())
	retention := storage.NewRetentionScheduler(
		repo,
		cfg.HotRetentionDays,
		cfg.RetentionBatchSize,
		time.Duration(cfg.RetentionBatchSleepMs)*time.Millisecond,
	)
	retention.SetFullVacuum(cfg.RetentionFullVacuum)
	// Start() is deferred until after the aggregate store is wired in below —
	// SetAggregateRetention must be called before the loop reads the fields.

	// 2b. Partition scheduler: only when DB_POSTGRES_PARTITIONING=daily.
	// Maintains lookahead daily partitions and drops expired ones — DROP
	// PARTITION is orders of magnitude faster than DELETE for retention.
	var partitionScheduler *storage.PartitionScheduler
	var cancelPartitions context.CancelFunc = func() {}
	if cfg.DBPostgresPartitioning == storage.PartitioningModeDaily {
		ctxPart, cancelPart := context.WithCancel(context.Background())
		partitionScheduler = storage.NewPartitionScheduler(repo, cfg.HotRetentionDays, cfg.DBPartitionLookaheadDays)
		if metrics != nil {
			partitionScheduler.SetMetrics(
				func(n int) {
					if metrics.PartitionsDropped != nil {
						metrics.PartitionsDropped.Add(float64(n))
					}
				},
				func(n int) {
					if metrics.PartitionsActive != nil {
						metrics.PartitionsActive.Set(float64(n))
					}
				},
			)
		}
		partitionScheduler.Start(ctxPart)
		cancelPartitions = cancelPart
		slog.Info("📦 Partition scheduler started", "lookahead_days", cfg.DBPartitionLookaheadDays, "retention_days", cfg.HotRetentionDays)
	}

	// 3. Initialize DLQ (Dead Letter Queue)
	replayInterval, err := time.ParseDuration(cfg.DLQReplayInterval)
	if err != nil {
		replayInterval = 5 * time.Minute
	}

	dlq, err := queue.NewDLQWithLimits(cfg.DLQPath, replayInterval, func(data []byte) error {
		// Replay handler: typed envelope supports logs, spans, traces, and metrics
		var envelope struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			// Legacy format: try to deserialize as []storage.Log
			var logs []storage.Log
			if json.Unmarshal(data, &logs) != nil {
				return fmt.Errorf("DLQ replay unmarshal failed: %w", err)
			}
			return repo.BatchCreateLogs(logs)
		}
		switch envelope.Type {
		case "logs":
			var logs []storage.Log
			if err := json.Unmarshal(envelope.Data, &logs); err != nil {
				return fmt.Errorf("DLQ replay logs unmarshal failed: %w", err)
			}
			return repo.BatchCreateLogs(logs)
		case "spans":
			var spans []storage.Span
			if err := json.Unmarshal(envelope.Data, &spans); err != nil {
				return fmt.Errorf("DLQ replay spans unmarshal failed: %w", err)
			}
			return repo.BatchCreateSpans(spans)
		case "traces":
			var traces []storage.Trace
			if err := json.Unmarshal(envelope.Data, &traces); err != nil {
				return fmt.Errorf("DLQ replay traces unmarshal failed: %w", err)
			}
			return repo.BatchCreateTraces(traces)
		case "metrics":
			var metrics []storage.MetricBucket
			if err := json.Unmarshal(envelope.Data, &metrics); err != nil {
				return fmt.Errorf("DLQ replay metrics unmarshal failed: %w", err)
			}
			return repo.BatchCreateMetrics(metrics)
		case ingest.DLQBatchType:
			// Whole async-pipeline batch (#194 finding 11). Replayed through
			// BatchCreateAll so the Trace->Span->Log FK ordering and the
			// single-transaction atomicity of the primary path are preserved.
			var payload ingest.DLQBatchPayload
			if err := json.Unmarshal(envelope.Data, &payload); err != nil {
				return fmt.Errorf("DLQ replay batch unmarshal failed: %w", err)
			}
			return repo.BatchCreateAll(payload.Traces, payload.Spans, payload.Logs)
		default:
			return fmt.Errorf("DLQ replay: unknown type %q", envelope.Type)
		}
	}, cfg.DLQMaxFiles, int64(cfg.DLQMaxDiskMB), cfg.DLQMaxRetries)
	if err != nil {
		fatal("Failed to initialize DLQ", err)
	}
	dlq.SetMetrics(
		func() { metrics.DLQEnqueuedTotal.Inc() },
		func() { metrics.DLQReplaySuccess.Inc() },
		func() { metrics.DLQReplayFailure.Inc() },
		func(b int64) { metrics.DLQDiskBytes.Set(float64(b)) },
	)
	dlq.SetTelemetryMetrics(metrics)
	dlq.SetMaxReplayPerTick(cfg.DLQMaxReplayPerTick)
	slog.Info("🔁 DLQ initialized", "path", cfg.DLQPath, "interval", replayInterval,
		"max_replay_per_tick", cfg.DLQMaxReplayPerTick)

	// 4. Initialize Real-Time WebSocket Hub
	hub := realtime.NewHub(func(count int) {
		metrics.SetActiveConnections(count)
	})
	hub.SetDevMode(cfg.DevMode)
	hub.SetMaxClients(cfg.WSMaxClients)
	hub.SetOriginPolicy(cfg.EnforceWSOrigin(), api.WSAllowedOriginHosts(cfg.WSAllowedOrigins))
	hub.SetWSMetrics(
		func(msgType string) { metrics.WSMessagesSent.WithLabelValues(msgType).Inc() },
		func() { metrics.WSSlowClientsRemoved.Inc() },
	)
	go hub.Run()
	slog.Info("🔌 WebSocket hub started")

	// 4b. Initialize Event Notification Hub (for live mode — pushes data snapshots)
	eventHub := realtime.NewEventHub(
		repo,
		metrics.IncrementActiveConns,
		metrics.DecrementActiveConns,
	)
	eventHub.SetMaxClients(cfg.WSMaxClients)
	eventHub.SetOriginPolicy(cfg.EnforceWSOrigin(), api.WSAllowedOriginHosts(cfg.WSAllowedOrigins))
	// The hub is STARTED further down, after the aggregate engine exists: in
	// AGGREGATE_MODE=aggregate its publication loop is revision-driven and
	// needs the publisher wired before the first tick.
	ctxEvents, cancelEvents := context.WithCancel(context.Background())

	// 4c. Initialize TSDB Aggregator + Ring Buffer.
	//
	// Retired outright in AGGREGATE_MODE=aggregate (#194 finding 10). There the
	// aggregate engine owns every metric read, so the ring buffer, the 30s
	// flush into metric_buckets, the per-point RawMetric allocation and the
	// metric callback are all pure waste on the hot path. Legacy keeps the path
	// because it IS the metric store; shadow keeps it because shadow's whole
	// job is running both paths side by side.
	legacyTSDB := legacyMetricPath(cfg.AggregateMode)
	var (
		tsdbAgg *tsdb.Aggregator
		ringBuf *tsdb.RingBuffer
	)
	ctxTSDB, cancelTSDB := context.WithCancel(context.Background())
	if legacyTSDB {
		tsdbAgg = tsdb.NewAggregator(repo, 30*time.Second)
		if cfg.MetricMaxCardinality > 0 || cfg.MetricMaxCardinalityPerTenant > 0 {
			tsdbAgg.SetCardinalityLimit(cfg.MetricMaxCardinality, cfg.MetricMaxCardinalityPerTenant, func(tenantID string) {
				// Maintain the legacy unlabeled counter for back-compat dashboards
				// AND emit the labeled by-tenant counter for fairness diagnostics.
				metrics.TSDBCardinalityOverflow.Inc()
				if metrics.TSDBCardinalityOverflowByTenant != nil {
					metrics.TSDBCardinalityOverflowByTenant.WithLabelValues(tenantID).Inc()
				}
			})
			slog.Info("📈 TSDB cardinality limits configured",
				"global_max", cfg.MetricMaxCardinality,
				"per_tenant_max", cfg.MetricMaxCardinalityPerTenant,
			)
		}
		tsdbAgg.SetMetrics(
			func() { metrics.TSDBIngestTotal.Inc() },
			func() { metrics.TSDBBatchesDropped.Inc() },
		)
		ringBuf = tsdb.NewRingBuffer(120, 30*time.Second, cfg.MetricMaxCardinality, metrics.TSDBRingSeriesRejected.Inc)
		tsdbAgg.SetRingBuffer(ringBuf)
		slog.Info("📈 TSDB ring buffer attached (120 slots × 30s = 1h retention)")

		go tsdbAgg.Start(ctxTSDB)
		slog.Info("📈 TSDB Aggregator started (30s window)")
	} else {
		// Nothing can move these counters once the aggregator is gone, and a
		// TSDB-specific cardinality gauge frozen at 0 reads as "no overflow"
		// rather than "no TSDB". The aggregate engine publishes its own caps
		// (#200/#201); these are unregistered so no scrape can confuse them.
		metrics.DisableTSDBCollectors()
		slog.Info("📈 Legacy TSDB aggregator and ring buffer disabled (AGGREGATE_MODE=aggregate)",
			"note", "metric reads are served by the aggregate engine; TSDB metrics unregistered",
		)
	}

	// 4e. Initialize In-Memory Service Graph (rebuilds from spans every 30s)
	svcGraph := graph.New(func(since time.Time) ([]graph.SpanRow, error) {
		rows, err := repo.GetSpansForGraph(since)
		if err != nil {
			return nil, err
		}
		out := make([]graph.SpanRow, len(rows))
		for i, r := range rows {
			out[i] = graph.SpanRow{
				SpanID:        r.SpanID,
				ParentSpanID:  r.ParentSpanID,
				ServiceName:   r.ServiceName,
				OperationName: r.OperationName,
				DurationMs:    r.DurationMs,
				IsError:       r.IsError,
				Timestamp:     r.Timestamp,
			}
		}
		return out, nil
	}, 5*time.Minute, 30*time.Second)
	ctxGraph, cancelGraph := context.WithCancel(context.Background())
	// The legacy graph is a raw-span scanner. In aggregate mode the topology
	// comes from the aggregate engine's snapshot and this scan is retired
	// outright (#174) — the object stays wired so the nil-tolerant handlers
	// keep their shape, but nothing refreshes it.
	if cfg.AggregateMode != aggregate.ModeAggregate {
		go svcGraph.Start(ctxGraph)
		slog.Info("🕸️  In-memory service graph started (5m window, 30s refresh)")
	} else {
		slog.Info("🕸️  Legacy in-memory service graph disabled (AGGREGATE_MODE=aggregate)")
	}

	// 4g. Initialize GraphRAG (replaces simple graph for advanced queries)
	graphrag.SetPanicMetrics(metrics)
	graphRAGCfg := graphrag.DefaultConfig()
	graphRAGCfg.WorkerCount = cfg.GraphRAGWorkerCount
	graphRAGCfg.ChannelSize = cfg.GraphRAGEventQueueSize
	graphRAGCfg.MaxSpansPerTenant = cfg.GraphRAGMaxSpansPerTenant
	// Aggregate mode retires the raw-span rebuild and the GraphRAG-owned
	// Drain miner; shadow mode retires only the miner (#163).
	graphRAGCfg.Mode = cfg.AggregateMode
	// Duration knobs follow the DLQ_REPLAY_INTERVAL pattern: unparsable
	// values fall back to the package default rather than aborting startup.
	if ttl, err := time.ParseDuration(cfg.GraphRAGTraceTTL); err == nil && ttl > 0 {
		graphRAGCfg.TraceTTL = ttl
	}
	if idle, err := time.ParseDuration(cfg.GraphRAGTenantIdleTTL); err == nil && idle > 0 {
		graphRAGCfg.TenantIdleTTL = idle
	}
	graphRAG := graphrag.New(repo, tsdbAgg, ringBuf, graphRAGCfg)
	graphRAG.SetMetrics(metrics)
	ctxGraphRAG, cancelGraphRAG := context.WithCancel(context.Background())
	go graphRAG.Start(ctxGraphRAG)
	slog.Info("GraphRAG started (layered graph with anomaly detection)",
		"workers", cfg.GraphRAGWorkerCount,
		"event_queue_size", cfg.GraphRAGEventQueueSize,
		"trace_ttl", graphRAGCfg.TraceTTL,
		"max_spans_per_tenant", graphRAGCfg.MaxSpansPerTenant,
		"tenant_idle_ttl", graphRAGCfg.TenantIdleTTL,
		"mode", graphRAGCfg.Mode,
	)

	// Auto-migrate GraphRAG models (Investigation, DrainTemplateRow)
	if err := graphrag.AutoMigrateGraphRAG(repo.DB()); err != nil {
		slog.Error("Failed to migrate GraphRAG models", "error", err)
	}

	// 5. Initialize AI Service.
	// Workers inherit aiCtx so an in-flight LLM call (30s timeout) is
	// cancelled the moment shutdown begins — without this, aiService.Stop()
	// blocks for up to 30s per in-flight worker waiting on the upstream
	// HTTP call to finish.
	aiCtx, aiCancel := context.WithCancel(appCtx)
	aiService := ai.NewService(repo)
	aiService.SetParentContext(aiCtx)

	// 6. Initialize API Server
	apiServer := api.NewServer(repo, hub, eventHub, metrics)
	apiServer.SetGraph(svcGraph)
	apiServer.SetGraphRAG(graphRAG)

	// 6b. Initialize MCP Server (HTTP Streamable, JSON-RPC 2.0 + SSE)
	mcpServer := mcp.New(cfg.DefaultTenant, repo, metrics, svcGraph)
	mcpServer.SetGraphRAG(graphRAG)
	mcpServer.SetCallLimit(cfg.MCPMaxConcurrent)
	mcpServer.SetCallTimeout(time.Duration(cfg.MCPCallTimeoutMs) * time.Millisecond)
	mcpServer.SetCacheTTL(time.Duration(cfg.MCPCacheTTLMs) * time.Millisecond)
	slog.Info("🤖 MCP server initialized",
		"path", cfg.MCPPath,
		"enabled", cfg.MCPEnabled,
		"default_tenant", cfg.DefaultTenant,
		"max_concurrent", cfg.MCPMaxConcurrent,
		"call_timeout_ms", cfg.MCPCallTimeoutMs,
		"cache_ttl_ms", cfg.MCPCacheTTLMs,
	)

	// 7. Initialize OTLP Ingestion (gRPC)
	traceServer := ingest.NewTraceServer(repo, metrics, cfg)
	logsServer := ingest.NewLogsServer(repo, metrics, cfg)
	// tsdbAgg is nil in aggregate mode: MetricsServer then skips the RawMetric
	// build entirely (see exportNumberPoints).
	metricsServer := ingest.NewMetricsServer(repo, metrics, tsdbAgg, cfg)

	// Aggregate engine + durable store (AGGREGATE_MODE != legacy). The reducer
	// runs inside Export() ahead of the sampler; the group-commit writer makes
	// the reduced deltas durable before the Export is acknowledged (#160), and
	// startup recovery replays the mutable delta log before readiness flips.
	// Shadow mode persists too — shadow IS the durability rehearsal.
	// AGGREGATE_MODE=legacy constructs nothing and leaves every ingest path
	// byte-for-byte unchanged.
	var (
		aggStore     *aggregate.SQLiteStore
		aggWriter    *aggregate.Writer
		aggRecovery  *aggregate.RecoveryGate
		aggEngine    *aggregate.Engine
		aggStoreMetr = aggregate.NewPrometheusStoreMetrics(metrics)
	)
	if cfg.AggregateMode != aggregate.ModeLegacy {
		store, err := aggregate.OpenSQLiteStore(aggregate.StoreConfig{
			Path:         cfg.AggregateDBPath,
			AllowRebuild: cfg.AggregateAllowRebuild,
			Synchronous:  cfg.AggregateSynchronous,
			Metrics:      aggStoreMetr,
		})
		if err != nil {
			fatal("❌ Aggregate store rejected", err, "path", cfg.AggregateDBPath)
		}
		aggStore = store

		// Dictionary IDs become DB-owned: the in-memory registrar's IDs are
		// provisional and would strand every persisted SeriesKey on restart.
		//
		// The per-kind caps bound dictionary DISK growth. A name that can never
		// appear in an admitted series is pure disk cost, so each name namespace
		// is capped at the series budget that could reference it; past the cap
		// the value resolves to __other__, the designed degradation. Service,
		// tenant and dimension namespaces stay uncapped — they are bounded by
		// the deployment and by AGGREGATE_METRIC_DIMS respectively.
		//
		// #200 Q3 closed the remaining holes: the service, dim-key, dim-value
		// and dim-tuple namespaces are capped per tenant AND instance-wide,
		// every value carries an encoded-length bound, and the tenant
		// namespace is capped and REFUSES rather than degrading.
		aggBounds := aggregate.Bounds{
			MaxValueBytes:  cfg.AggregateMaxValueBytes,
			MaxTenantBytes: cfg.AggregateMaxTenantBytes,
			MaxTenants:     cfg.AggregateMaxTenants,
			PerTenantKind: map[aggregate.Kind]int{
				aggregate.KindOperation:   cfg.AggregateMaxSeriesTraces + cfg.AggregateMaxSeriesEdges,
				aggregate.KindMetricName:  cfg.AggregateMaxSeriesMetrics,
				aggregate.KindLogTemplate: cfg.AggregateMaxSeriesLogs,
				aggregate.KindService:     cfg.AggregateMaxServicesPerTenant,
				aggregate.KindDimKey:      cfg.AggregateMaxDimKeysPerTenant,
				aggregate.KindDimValue:    cfg.AggregateMaxDimValuesPerTenant,
				aggregate.KindDimTuple:    cfg.AggregateMaxDimTuplesPerTenant,
			},
			InstanceKind: map[aggregate.Kind]int{
				aggregate.KindService:  cfg.AggregateMaxServices,
				aggregate.KindDimKey:   cfg.AggregateMaxDimKeys,
				aggregate.KindDimValue: cfg.AggregateMaxDimValues,
				aggregate.KindDimTuple: cfg.AggregateMaxDimTuples,
			},
		}
		registrar, err := aggregate.NewDurableRegistrarWithBounds(aggStore, aggBounds)
		if err != nil {
			fatal("❌ Aggregate dictionary could not be loaded", err, "path", cfg.AggregateDBPath)
		}

		aggEngine, err = aggregate.NewEngine(aggregate.EngineConfig{
			Mode:      cfg.AggregateMode,
			Registrar: registrar,
			Bounds:    aggBounds,
			Limiter: aggregate.LimiterConfig{
				MaxSeries:                 cfg.AggregateMaxSeries,
				MaxSeriesMetrics:          cfg.AggregateMaxSeriesMetrics,
				MaxSeriesTraces:           cfg.AggregateMaxSeriesTraces,
				MaxSeriesEdges:            cfg.AggregateMaxSeriesEdges,
				MaxSeriesLogs:             cfg.AggregateMaxSeriesLogs,
				MaxSeriesSystem:           cfg.AggregateMaxSeriesSystem,
				MaxOperationsPerService:   cfg.AggregateMaxOperationsPerService,
				MaxLogTemplatesPerService: cfg.AggregateMaxLogTemplatesPerService,
				MaxTraceSeriesPerService:  cfg.AggregateMaxTraceSeriesPerService,
				MaxMetricSeriesPerService: cfg.AggregateMaxMetricSeriesPerService,
				SeriesPerTenantFraction:   cfg.AggregateSeriesPerTenantFraction,
			},
			MaxProducerBaselinesPerSeries: cfg.AggregateMaxProducerBaselinesPerSeries,
			MaxBaselines:                  cfg.ResolvedAggregateMaxBaselines(),
			Metrics:                       aggregate.NewPrometheusRecorder(metrics),
			MetricDims:                    aggregate.DimsConfig(cfg.AggregateMetricDims),
			// Keep the topology projection's caps in step with the series
			// budget: an operation the limiter would collapse into __other__
			// must not survive as a named topology entry.
			Topology: aggregate.TopologyConfig{
				MaxOperationsPerService: cfg.AggregateMaxOperationsPerService,
				MaxEdges:                cfg.AggregateMaxSeriesEdges,
			},
		})
		if err != nil {
			fatal("❌ Aggregate engine configuration rejected", err, "mode", cfg.AggregateMode)
		}

		aggWriter, err = aggregate.NewWriter(aggregate.WriterConfig{
			Store:            aggStore,
			Engine:           aggEngine,
			Registrar:        registrar,
			CoalesceWindow:   time.Duration(cfg.AggregateCommitCoalesceMs) * time.Millisecond,
			MaxBatchDeltas:   cfg.AggregateCommitMaxDeltas,
			MaxBatchBytes:    int64(cfg.AggregateCommitMaxBytes),
			MaxPendingBytes:  int64(cfg.AggregateCommitMaxPendingBytes),
			MaxWaiters:       cfg.AggregateCommitMaxWaiters,
			MaxPendingDeltas: cfg.AggregateCommitMaxPendingDeltas,
			FinalizeInterval: time.Duration(cfg.AggregateFinalizeIntervalSec) * time.Second,
			Metrics:          aggStoreMetr,
		})
		if err != nil {
			fatal("❌ Aggregate group-commit writer rejected", err)
		}

		// Log-template miner state is reloaded BEFORE anything can mine a line
		// (#200 Q4). A line mined against an empty miner would mint a second
		// identity for a pattern that already has one, and both would be live
		// against the same seven-day buckets.
		if restored, err := aggregate.RestoreMiner(aggStore, aggEngine.Miner()); err != nil {
			fatal("❌ Aggregate log-template state could not be reloaded", err, "path", cfg.AggregateDBPath)
		} else if restored > 0 {
			slog.Info("🔤 Aggregate log templates reloaded", "templates", restored)
		}

		// Recovery runs BEFORE the writer starts accepting Exports and before
		// readiness flips: acknowledged-but-unfinalized deltas go back into the
		// shards, windows whose lateness expired during downtime finalize, and
		// durable baselines are seeded.
		aggRecovery = aggregate.NewRecoveryGate()
		recoveryStats, err := aggregate.Recover(aggStore, aggEngine, aggWriter, time.Now())
		if err != nil {
			fatal("❌ Aggregate store recovery failed", err, "path", cfg.AggregateDBPath)
		}
		aggregate.LogRecovery(recoveryStats, cfg.AggregateDBPath)
		aggStoreMetr.RecordRecovery(recoveryStats.Duration, recoveryStats.ReplayedRows, recoveryStats.FinalizedWindows)

		aggEngine.SetApplier(aggWriter)
		aggWriter.Start()
		aggRecovery.Complete()

		traceServer.SetAggregateEngine(aggEngine)
		logsServer.SetAggregateEngine(aggEngine)
		metricsServer.SetAggregateEngine(aggEngine)

		// Shadow and aggregate modes: log templates are mined once, on the
		// ingest path, and handed to GraphRAG as facts. GraphRAG runs no
		// miner of its own in either mode, so the two never disagree about a
		// template's identity (#163).
		aggEngine.SetTemplateFactSink(graphRAG.OnTemplateFact)
		// Aggregate mode: GraphRAG replaces its topology from the engine's
		// per-revision snapshot instead of rescanning the spans table (#174).
		if cfg.AggregateMode == aggregate.ModeAggregate {
			graphRAG.SetAggregateSource(aggEngine)
			slog.Info("🧭 GraphRAG consuming aggregate topology snapshots (raw-span rebuild retired)",
				"epoch", aggEngine.TopologyEpoch(),
			)
		}
		slog.Info("🧮 Aggregate engine enabled",
			"mode", cfg.AggregateMode,
			"max_series", cfg.AggregateMaxSeries,
			"max_baselines", cfg.ResolvedAggregateMaxBaselines(),
			"store_path", cfg.AggregateDBPath,
			"store_uuid", aggStore.UUID(),
			"synchronous", cfg.AggregateSynchronous,
			"commit_coalesce_ms", cfg.AggregateCommitCoalesceMs,
			"note", "durable ACK: Export returns only after the group commit",
		)

		// Exemplar-tier retention (#201 Q2). Only in aggregate mode: there the
		// raw rows are exemplars attached to a seven-day aggregate dataset and
		// a two-day purge is budget enforcement. In legacy and shadow the raw
		// rows ARE the dataset and the same purge would be data loss.
		retention.SetExemplarRetention(cfg.ExemplarRetentionDays)

		// Retention owns the hourly aggregate purge and the conservative daily
		// ANALYZE. No VACUUM on this file, by decision (#162).
		retention.SetAggregateRetention(
			func(cutoff time.Time) (int64, error) {
				stats, err := aggStore.PurgeBefore(aggregate.WindowStart(cutoff))
				return stats.Buckets + stats.Deltas + stats.Baselines, err
			},
			aggStore.Analyze,
		)

		// Identity GC rides the same daily maintenance tick as ANALYZE
		// (#200 Q1). Retention deletes the buckets; this is what deletes the
		// names those buckets were the last reference to. The full mark scan
		// runs lock-free — only the revalidate/fence/delete tail serializes
		// with the group commit.
		if cfg.AggregateGCEnabled {
			writer := aggWriter
			retention.SetAggregateGC(func() error {
				stats, err := writer.CollectIdentities()
				aggregate.LogGC(stats, err)
				return err
			})
		}
	}

	// Aggregate READ path (#175). Only AGGREGATE_MODE=aggregate switches the
	// dashboard, the WebSocket publisher and the MCP coverage metadata over;
	// shadow mode writes aggregates but keeps serving the legacy reads.
	if cfg.AggregateMode == aggregate.ModeAggregate && aggEngine != nil {
		apiServer.SetAggregateEngine(aggEngine)
		mcpServer.SetAggregateMode(true)
		// Per-event log/metric broadcasts are replaced by the coalesced,
		// revision-driven snapshot.
		hub.SetAggregateMode(true)
		eventHub.SetAggregatePublisher(realtime.NewEnginePublisher(realtime.EnginePublisherConfig{
			Engine: aggEngine,
			Tenant: cfg.DefaultTenant,
			Edges: func(ctx context.Context) []storage.ServiceMapEdge {
				return graphRAGServiceEdges(ctx, graphRAG)
			},
		}), realtime.DefaultPublishFloor)
		slog.Info("📊 Aggregate read path enabled",
			"epoch", aggEngine.Epoch(),
			"ws_publish_floor", realtime.DefaultPublishFloor,
			"note", "dashboard, topology and WebSocket snapshots are served from the aggregate engine",
		)
	}

	go eventHub.Start(ctxEvents, 5*time.Second, 500*time.Millisecond)
	slog.Info("⚡ Event notification hub started (5s snapshots, 500ms batches)")

	retention.Start(ctxRetention)
	slog.Info("🧹 Retention scheduler started",
		"retention_days", cfg.HotRetentionDays,
		"aggregate_store", cfg.AggregateMode != aggregate.ModeLegacy,
	)

	// diskWatchdogPolicy is the exemplar policy the disk watchdog sheds
	// through. nil outside aggregate mode, where there is no exemplar policy
	// to shed and the raw rows are the dataset.
	var diskWatchdogPolicy *ingest.ExemplarPolicy

	// Bounded exemplar retention (#176). In AGGREGATE_MODE=aggregate this is
	// the ONLY raw-retention gate — the adaptive sampler below is skipped
	// entirely, so SAMPLING_RATE has no effect on what gets persisted (#161).
	// SAMPLING_LATENCY_THRESHOLD_MS survives as the shared "slow" predicate.
	if cfg.AggregateMode == aggregate.ModeAggregate {
		exemplarPolicy := ingest.NewExemplarPolicy(ingest.ExemplarConfig{
			TracesPerServiceWindow:    cfg.ExemplarTracesPerServiceWindow,
			TracesGlobalWindow:        cfg.ExemplarTracesGlobalWindow,
			BytesPerServiceWindow:     int64(cfg.ExemplarBytesPerServiceWindow),
			BytesGlobalWindow:         int64(cfg.ExemplarBytesGlobalWindow),
			HealthyRate:               cfg.ExemplarHealthyRate,
			StratumTopK:               cfg.ExemplarStratumTopK,
			LatencyThresholdMs:        float64(cfg.SamplingLatencyThresholdMs),
			LogsErrorPerServiceWindow: cfg.ExemplarLogsErrorPerServiceWindow,
			LogsWarnEnabled:           cfg.ExemplarLogsWarnEnabled,
			LogsWarnPerServiceWindow:  cfg.ExemplarLogsWarnPerServiceWindow,
			MaxSpansPerTrace:          cfg.ExemplarMaxSpansPerTrace,
			MaxBytesPerTrace:          int64(cfg.ExemplarMaxBytesPerTrace),
			SynthLogsPerSpan:          cfg.ExemplarSynthLogsPerSpan,
			SynthLogsPerTrace:         cfg.ExemplarSynthLogsPerTrace,
			Metrics:                   metrics,
		})
		traceServer.SetExemplarPolicy(exemplarPolicy)
		logsServer.SetExemplarPolicy(exemplarPolicy)
		diskWatchdogPolicy = exemplarPolicy
		slog.Info("🎚️  Bounded exemplar retention enabled (adaptive sampler retired)",
			"traces_per_service_window", cfg.ExemplarTracesPerServiceWindow,
			"traces_global_window", cfg.ExemplarTracesGlobalWindow,
			"bytes_per_service_window", cfg.ExemplarBytesPerServiceWindow,
			"bytes_global_window", cfg.ExemplarBytesGlobalWindow,
			"healthy_rate", cfg.ExemplarHealthyRate,
			"latency_threshold_ms", cfg.SamplingLatencyThresholdMs,
			"retention_days", cfg.ExemplarRetentionDays,
			"synth_logs_per_span", cfg.ExemplarSynthLogsPerSpan,
			"synth_logs_per_trace", cfg.ExemplarSynthLogsPerTrace,
		)
	} else if cfg.SamplingRate > 0 && cfg.SamplingRate < 1.0 {
		// Wire adaptive sampler (only when rate < 1.0 to avoid unnecessary overhead)
		sampler := ingest.NewSampler(cfg.SamplingRate, cfg.SamplingAlwaysOnErrors, float64(cfg.SamplingLatencyThresholdMs))
		traceServer.SetSampler(sampler)
		slog.Info("🎯 Adaptive trace sampling enabled",
			"rate", cfg.SamplingRate,
			"always_errors", cfg.SamplingAlwaysOnErrors,
			"latency_threshold_ms", cfg.SamplingLatencyThresholdMs,
		)
	}

	// Disk watchdog (#201 Q5). statfs on the data volume drives staged
	// shedding with hysteresis; per-component file sizes attribute the 8 GiB
	// budget table (#201 Q1) so the seven-day gate (#202) validates it against
	// measurements rather than intent.
	diskWatchdog := storage.NewDiskWatchdog(storage.DiskWatchdogConfig{
		Path:        cfg.DataDiskPath,
		BudgetBytes: int64(cfg.DataDiskBudgetMB) * 1024 * 1024,
		Metrics:     metrics,
		OnRawOff: func() {
			// At >=95% waiting up to an hour for the next retention tick is
			// not a plan. Purge the expired exemplar tier now, then checkpoint
			// the WAL so the freed pages are actually handed back.
			retention.PurgeExemplarsNow(ctxRetention)
			if err := repo.CheckpointWAL(ctxRetention); err != nil {
				slog.Error("disk watchdog: WAL checkpoint failed", "error", err)
			}
		},
	})
	diskWatchdog.AddComponent("main_db", func() int64 { return repo.HotDBSizeBytes() })
	diskWatchdog.AddComponent("wal", func() int64 {
		return sidecarBytes(sqliteMainDBPath(cfg)) + sidecarBytes(cfg.AggregateDBPath)
	})
	if cfg.AggregateMode != aggregate.ModeLegacy {
		diskWatchdog.AddComponent("aggregate_db", func() int64 { return fileBytes(cfg.AggregateDBPath) })
	}
	if dlq != nil {
		diskWatchdog.AddComponent("dlq", func() int64 { return dlq.DiskBytes() })
	}
	if diskWatchdogPolicy != nil {
		diskWatchdog.AddObserver(diskWatchdogPolicy.SetShedding)
	}
	apiServer.SetDiskPressureProbe(func() (string, bool) {
		return diskWatchdog.State().String(), diskWatchdog.Healthy()
	})
	ctxDisk, cancelDisk := context.WithCancel(context.Background())
	diskWatchdog.Start(ctxDisk)
	slog.Info("💽 Disk watchdog started",
		"path", cfg.DataDiskPath,
		"budget_mb", cfg.DataDiskBudgetMB,
		"errors_only_at", "90%",
		"raw_off_at", "95%",
	)

	// Wire async ingest pipeline. Decouples OTLP Export() from synchronous
	// DB writes — caller returns as soon as the parsed batch is enqueued.
	// When disabled (INGEST_ASYNC_ENABLED=false), trace/logs servers fall
	// back to the inline-write path bit-for-bit.
	var ingestPipeline *ingest.Pipeline
	if cfg.IngestAsyncEnabled {
		ingestPipeline = ingest.NewPipeline(repo, metrics, ingest.PipelineConfig{
			Capacity: cfg.IngestPipelineQueueSize,
			Workers:  cfg.IngestPipelineWorkers,
			MaxBytes: int64(cfg.IngestPipelineMaxBytes),
		})
		ingestPipeline.SetPerTenantCap(cfg.IngestPipelinePerTenantCap)
		// Persist-failure sink. Without this a BatchCreateAll rollback drops
		// the batch on the floor; with it the whole batch lands on disk and
		// the replay worker re-runs it (#194 finding 11).
		ingestPipeline.SetDLQ(dlq)

		// Second-tier severity gate. Empty STORE_MIN_SEVERITY means "use the
		// same threshold as INGEST_MIN_SEVERITY" — i.e. behavior is identical
		// to the legacy single-threshold path. Only enable the gate when the
		// store threshold is strictly higher than the ingest threshold; equal
		// or lower is wasted work since the receiver has already dropped the
		// affected logs.
		ingestRank := ingest.ParseSeverity(cfg.IngestMinSeverity)
		storeRank := ingestRank
		if cfg.StoreMinSeverity != "" {
			storeRank = ingest.ParseSeverity(cfg.StoreMinSeverity)
		}
		if storeRank > ingestRank {
			ingestPipeline.SetStoreMinSeverity(storeRank)
			slog.Info("🪛 Store-severity gate enabled",
				"ingest_min", cfg.IngestMinSeverity,
				"store_min", cfg.StoreMinSeverity,
				"note", "logs below store_min reach in-memory consumers but are not persisted",
			)
		} else if cfg.StoreMinSeverity != "" && storeRank < ingestRank {
			slog.Warn("STORE_MIN_SEVERITY is lower than INGEST_MIN_SEVERITY — has no effect; receiver already filters",
				"ingest_min", cfg.IngestMinSeverity,
				"store_min", cfg.StoreMinSeverity,
			)
		}

		ingestPipeline.Start(context.Background())
		traceServer.SetPipeline(ingestPipeline)
		logsServer.SetPipeline(ingestPipeline)
		slog.Info("🌊 Async ingest pipeline enabled",
			"queue_size", cfg.IngestPipelineQueueSize,
			"workers", cfg.IngestPipelineWorkers,
			"per_tenant_cap", cfg.IngestPipelinePerTenantCap,
		)
	} else {
		slog.Warn("🐌 Async ingest pipeline disabled (INGEST_ASYNC_ENABLED=false) — Export() blocks on DB writes")
	}

	// Wire /ready saturation probes. Both probes are nil-tolerant on the
	// api server side; we additionally guard against unconfigured caps
	// (DLQ unbounded, async pipeline disabled) by returning 0 — i.e.
	// "skipped" semantics — rather than dividing by zero.
	if dlq != nil && cfg.DLQMaxDiskMB > 0 {
		maxBytes := float64(cfg.DLQMaxDiskMB) * 1024 * 1024
		apiServer.SetDLQSaturationProbe(func() float64 {
			return float64(dlq.DiskBytes()) / maxBytes
		})
	}
	if ingestPipeline != nil {
		apiServer.SetPipelineSaturationProbe(func() float64 {
			st := ingestPipeline.Stats()
			if st.Capacity == 0 {
				return 0
			}
			return float64(st.QueueDepth) / float64(st.Capacity)
		})
	}
	if aggRecovery != nil {
		// /ready stays 503 until the aggregate delta log has been replayed.
		apiServer.SetAggregateRecoveryProbe(aggRecovery.Done)
	}

	// Aggregate RUNTIME readiness probes (#194 finding 18). Recovery is a
	// one-time gate; these cover the ways a recovered process later stops
	// being able to serve: the store goes unreachable, group commits fail in
	// a row, admission saturates, the finalizer wedges, the delta log stops
	// draining, or the aggregate tier outgrows its share of the disk budget.
	// Every signal is read from counters the writer already maintains, so a
	// readiness request never queues behind the single SQLite writer.
	apiServer.SetReadinessThresholds(api.ReadinessThresholds{
		MaxCommitFailureStreak:   uint64(cfg.ReadyMaxCommitFailureStreak),
		MaxFinalizeFailureStreak: uint64(cfg.ReadyMaxFinalizeFailureStreak),
		MaxAdmissionRatio:        cfg.ReadyMaxAdmissionRatio,
		MaxDeltaLogAgeSeconds:    float64(cfg.ReadyMaxDeltaLogAgeS),
		MaxAggregateDiskRatio:    cfg.ReadyMaxAggregateDiskRatio,
	})
	if aggStore != nil {
		apiServer.SetAggregateDBProbe(aggStore.PingContext)
	}
	if aggWriter != nil {
		aggDiskBudget := int64(cfg.ReadyAggregateDiskBudgetMB) * 1024 * 1024
		aggDBPath := cfg.AggregateDBPath
		writer := aggWriter
		apiServer.SetAggregateRuntimeProbe(func() api.AggregateRuntime {
			st := writer.Stats()
			return api.AggregateRuntime{
				CommitFailureStreak:   st.CommitFailureStreak,
				FinalizeFailureStreak: st.FinalizeFailureStreak,
				AdmissionRatio:        st.AdmissionRatio(),
				DeltaLogAgeSeconds:    st.DeltaLogAge(time.Now()),
				// Same figure the disk watchdog attributes to the
				// aggregate_db component, measured against the tier's
				// share of the budget instead of the whole volume.
				DiskUsedBytes:   fileBytes(aggDBPath),
				DiskBudgetBytes: aggDiskBudget,
			}
		})
	}

	// Wire up live log streaming + AI + DLQ metrics
	logHandler := func(l storage.Log) {
		start := time.Now()
		eventHub.BroadcastLog(realtime.LogEntry{
			Tenant:         l.TenantID,
			ID:             l.ID,
			TraceID:        l.TraceID,
			SpanID:         l.SpanID,
			Severity:       l.Severity,
			Body:           l.Body,
			ServiceName:    l.ServiceName,
			AttributesJSON: string(l.AttributesJSON),
			AIInsight:      string(l.AIInsight),
			Timestamp:      l.Timestamp,
		})
		aiService.EnqueueLog(l)
		eventHub.NotifyRefresh()
		if time.Since(start) > 100*time.Millisecond {
			slog.Warn("Slow broadcast/enqueue", "duration", time.Since(start))
		}
	}

	logsServer.SetLogCallback(func(l storage.Log) {
		logHandler(l)
		graphRAG.OnLogIngested(l)
	})
	traceServer.SetLogCallback(func(l storage.Log) {
		logHandler(l)
		graphRAG.OnLogIngested(l)
	})

	// Wire span callbacks for GraphRAG
	traceServer.SetSpanCallback(func(span storage.Span) {
		graphRAG.OnSpanIngested(span)
	})

	// Observe cross-service call topology pre-sample so the service map keeps
	// flow direction even when sampling drops the spans forming each edge.
	// In aggregate mode edges come from the engine's service-edge series and
	// this observer would fight the snapshot, so it is not wired.
	if cfg.AggregateMode != aggregate.ModeAggregate {
		traceServer.SetTopologyObserver(func(tenant, traceID, spanID, parentSpanID, service string) {
			graphRAG.ObserveSpanTopology(tenant, traceID, spanID, parentSpanID, service)
		})
	}

	// Per-point metric fan-out. Not wired in aggregate mode: the event hub
	// publishes from the engine snapshot instead (BroadcastMetric is already a
	// no-op behind the aggregate publisher) and GraphRAG replaces its metric
	// nodes per topology revision, so processMetric would discard every event
	// after paying for the channel hop and the RawMetric allocation.
	if legacyTSDB {
		metricsServer.SetMetricCallback(func(m tsdb.RawMetric) {
			eventHub.BroadcastMetric(realtime.MetricEntry{
				Tenant:      m.TenantID,
				Name:        m.Name,
				ServiceName: m.ServiceName,
				Value:       m.Value,
				Timestamp:   m.Timestamp,
				Attributes:  m.Attributes,
			})
			graphRAG.OnMetricIngested(m)
		})
	}

	// Update DLQ size metric periodically. Tied to appCtx so the goroutine
	// exits before dlq.Stop() — otherwise it keeps polling Size()/DiskBytes()
	// on a stopped DLQ and races with the file-handle close in repo.Close().
	bootWG.Add(1)
	go func() {
		defer bootWG.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-appCtx.Done():
				return
			case <-ticker.C:
				metrics.SetDLQSize(dlq.Size())
				metrics.DLQDiskBytes.Set(float64(dlq.DiskBytes()))
			}
		}
	}()

	// Resolve TLS material once: explicit cert-file > self-signed > plaintext.
	// Both gRPC and HTTP reuse the same resolved paths below.
	const (
		tlsModeCertFile   = "cert-file"
		tlsModeSelfSigned = "self-signed"
	)
	var (
		tlsCertPath string
		tlsKeyPath  string
		tlsMode     string // tlsModeCertFile, tlsModeSelfSigned, or "" (plaintext)
	)
	switch {
	case cfg.TLSCertFileMode():
		tlsCertPath = cfg.TLSCertFile
		tlsKeyPath = cfg.TLSKeyFile
		tlsMode = tlsModeCertFile
	case cfg.TLSSelfsignedMode():
		cp, kp, err := tlsbootstrap.EnsureSelfSignedCert(cfg.TLSCacheDir)
		if err != nil {
			fatal("Failed to bootstrap self-signed TLS cert", err)
		}
		tlsCertPath = cp
		tlsKeyPath = kp
		tlsMode = tlsModeSelfSigned
	}

	// Start gRPC Server
	lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		fatal("Failed to listen on gRPC port", err, "port", cfg.GRPCPort)
	}
	recvBytes := cfg.GRPCMaxRecvMB
	if recvBytes <= 0 {
		recvBytes = 16
	}
	streams := cfg.GRPCMaxConcurrentStreams
	if streams <= 0 {
		streams = 1000
	}

	grpcOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(recvBytes * 1024 * 1024),
		grpc.MaxConcurrentStreams(uint32(streams)),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:                  60 * time.Second, // ping idle clients
			Timeout:               10 * time.Second, // drop if no pong
			MaxConnectionIdle:     10 * time.Minute, // garbage-collect dead NAT entries
			MaxConnectionAge:      2 * time.Hour,    // force periodic reconnects
			MaxConnectionAgeGrace: 30 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		// Recovery FIRST so a panic inside the metrics interceptor is still caught.
		grpc.ChainUnaryInterceptor(
			recoveryUnaryInterceptor(metrics),
			metricsUnaryInterceptor(metrics),
		),
	}
	// OTLP gRPC authentication. Installed only when a credential source is
	// configured, so an unauthenticated development deployment is untouched.
	// Auth runs AFTER the metrics interceptor so rejected calls still show up
	// in the request counters.
	if authUnary, authStream := ingest.NewGRPCAuthInterceptors(ingest.GRPCAuthOptions{
		Auth:                      authenticator,
		ExternalTenantMetadataKey: cfg.AuthExternalTenantHeader,
		OnAuthFailure: func(reason string) {
			authnMetrics.GRPCAuthFailuresTotal.WithLabelValues(reason).Inc()
		},
	}); authUnary != nil {
		grpcOpts = append(grpcOpts,
			grpc.ChainUnaryInterceptor(authUnary),
			grpc.ChainStreamInterceptor(authStream),
		)
		slog.Info("🔑 gRPC OTLP authentication enabled",
			"tenant_keys", authenticator.TenantKeyCount(),
			"operator_key", cfg.APIKey != "",
			"trust_external", cfg.AuthTrustExternal)
	} else {
		slog.Warn("🔓 gRPC OTLP unauthenticated — tenant identity is client-controlled; set API_KEY or API_TENANT_KEYS_FILE")
	}
	slog.Info("📡 gRPC server tuned",
		"max_recv_mb", recvBytes,
		"max_concurrent_streams", streams,
	)
	switch tlsMode {
	case tlsModeCertFile:
		creds, err := credentials.NewServerTLSFromFile(tlsCertPath, tlsKeyPath)
		if err != nil {
			fatal("Failed to load gRPC TLS credentials", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
		slog.Info("🔒 gRPC TLS enabled", "mode", tlsModeCertFile)
	case tlsModeSelfSigned:
		creds, err := credentials.NewServerTLSFromFile(tlsCertPath, tlsKeyPath)
		if err != nil {
			fatal("Failed to load gRPC TLS credentials (self-signed)", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
		slog.Info("🔒 gRPC TLS enabled", "mode", tlsModeSelfSigned, "cache_dir", cfg.TLSCacheDir)
	default:
		slog.Info("🔓 gRPC plaintext — not for production; set TLS_CERT_FILE/TLS_KEY_FILE or TLS_AUTO_SELFSIGNED=true")
	}
	grpcServer := grpc.NewServer(grpcOpts...)
	coltracepb.RegisterTraceServiceServer(grpcServer, traceServer)
	collogspb.RegisterLogsServiceServer(grpcServer, logsServer)
	colmetricspb.RegisterMetricsServiceServer(grpcServer, metricsServer)
	// Reflection enumerates every service and message type to an
	// unauthenticated peer, so production defaults to off. GRPC_REFLECTION=true
	// re-enables it explicitly.
	if cfg.GRPCReflectionEnabled() {
		reflection.Register(grpcServer)
	} else {
		slog.Info("🔒 gRPC reflection disabled (APP_ENV=production) — set GRPC_REFLECTION=true to re-enable")
	}

	go func() {
		slog.Info("📡 gRPC OTLP receiver started", "port", cfg.GRPCPort)
		if err := grpcServer.Serve(lis); err != nil {
			fatal("Failed to serve gRPC", err)
		}
	}()

	// Start runtime metrics sampling (every 15s)
	metrics.StartRuntimeMetrics()
	slog.Info("📊 Runtime metrics sampling started")

	// 7b. Register HTTP OTLP endpoints (before catch-all UI handler)
	otlpHTTP := ingest.NewHTTPHandler(traceServer, logsServer, metricsServer)
	if metrics != nil && metrics.HTTPOTLPThrottledTotal != nil {
		otlpHTTP.SetThrottleCallback(func(signal string) {
			metrics.HTTPOTLPThrottledTotal.WithLabelValues(signal).Inc()
		})
	}

	// 8. Start HTTP Server
	mux := http.NewServeMux()
	otlpHTTP.RegisterRoutes(mux)
	apiServer.RegisterRoutes(mux)

	// MCP Server routes (conditionally enabled via MCP_ENABLED)
	if cfg.MCPEnabled {
		mcpPath := cfg.MCPPath
		if mcpPath == "" {
			mcpPath = "/mcp"
		}
		mux.Handle(mcpPath, http.StripPrefix(mcpPath, mcpServer.Handler()))
		mux.Handle(mcpPath+"/", http.StripPrefix(mcpPath, mcpServer.Handler()))
		slog.Info("🤖 MCP endpoint registered", "path", mcpPath)
	}

	// Embedded UI Server
	uiServer := ui.NewServer(repo, metrics, svcGraph)
	uiServer.SetMCPConfig(cfg.MCPEnabled, cfg.MCPPath)
	if err := uiServer.RegisterRoutes(mux); err != nil {
		fatal("Failed to register UI routes", err)
	}

	var httpHandler http.Handler = mux

	// Gzip GET /api/* responses (innermost wrapper — only handler output is
	// compressed; error responses written by outer middleware like auth and
	// rate limiting stay identity-encoded, and /ws*, /v1/*, /metrics*, and
	// the MCP/SSE path pass through untouched).
	httpHandler = api.GzipMiddleware(cfg.MCPPath)(httpHandler)

	// Resolve tenant on /api/* read-side requests (passes through OTLP /v1,
	// MCP, UI assets, and health probes untouched).
	httpHandler = api.TenantMiddleware(cfg)(httpHandler)

	// Wire auth-failure metric hook before installing any auth middleware.
	api.AuthFailureHook = func(reason string) {
		metrics.APIAuthFailuresTotal.WithLabelValues(reason).Inc()
	}

	// Authentication (HTTP + WebSocket). One authenticator serves every
	// surface: the operator key authenticates, a tenant key authenticates AND
	// binds the tenant, and a proxy-injected identity binds it too when the
	// operator has accepted the AUTH_TRUST_EXTERNAL deployment contract.
	httpHandler = api.WebSocketGate(api.WSGateOptions{
		Auth:                 authenticator,
		DefaultTenant:        cfg.DefaultTenant,
		AllowedOrigins:       cfg.WSAllowedOrigins,
		EnforceOrigin:        cfg.EnforceWSOrigin(),
		ExternalTenantHeader: cfg.AuthExternalTenantHeader,
	}, httpHandler)
	httpHandler = api.AuthGate(api.AuthGateOptions{
		Auth:                 authenticator,
		MCPPath:              cfg.MCPPath,
		ExternalTenantHeader: cfg.AuthExternalTenantHeader,
	}, httpHandler)
	switch {
	case authenticator.HasTenantKeys():
		slog.Info("🔑 Per-tenant bearer authentication enabled",
			"keys", authenticator.TenantKeyCount(),
			"tenants", len(authenticator.Tenants()),
			"operator_key", cfg.APIKey != "")
	case cfg.APIKey != "":
		slog.Info("🔑 API key authentication enabled (shared key)")
	case cfg.AuthTrustExternal:
		slog.Warn("🔑 Authentication delegated to the front proxy (AUTH_TRUST_EXTERNAL=true) — the deployment contract in CLAUDE.md is mandatory",
			"identity_header", cfg.AuthExternalTenantHeader)
	default:
		slog.Warn("API authentication disabled — set API_KEY or API_TENANT_KEYS_FILE for production")
	}
	if cfg.EnforceWSOrigin() {
		slog.Info("🔒 WebSocket origin policy enforced",
			"allowed_origins", cfg.WSAllowedOrigins, "same_host_only", len(cfg.WSAllowedOrigins) == 0)
	}

	httpHandler = api.MetricsMiddleware(metrics, httpHandler)
	if cfg.APIRateLimitRPS > 0 {
		rl := api.NewRateLimiter(float64(cfg.APIRateLimitRPS))
		// OTLP ingestion paths (/v1/*) are exempt from the per-IP rate limiter.
		//
		// Why: OTLP collectors batch aggressively and a healthy agent routinely
		// exceeds the API_RATE_LIMIT_RPS default (100 RPS/IP). Throttling the
		// ingestion path drops legitimate telemetry — the exact data this
		// platform exists to capture — so /v1/* bypasses the limiter.
		//
		// DoS trade-off (acknowledged): the APIKeyGate runs *downstream* of the
		// limiter in the middleware chain, which means an unauthenticated
		// attacker can push /v1/* requests past the (bypassed) limiter all the
		// way to the auth check before getting a 401. This is acceptable
		// because APIKeyGate is header-only: it inspects the Authorization
		// header and returns 401 without parsing the request body, so the
		// per-request CPU cost is bounded and small (no protobuf decode, no
		// JSON parse, no DB touch). Layer-4/7 protections (firewall, LB,
		// WAF, mTLS) remain the primary defense against volumetric abuse.
		//
		// TODO: if this trade-off becomes a concern (e.g. abuse observed in
		// prod, or CPU pressure from 401 storms), add a separate
		// higher-ceiling OTLP-specific limiter scoped to /v1/* — tuned for
		// collector-class RPS — rather than lowering the general API limit.
		httpHandler = rl.MiddlewareExcept(func(path string) bool {
			return strings.HasPrefix(path, "/v1/")
		})(httpHandler)
		slog.Info("🛡️  API rate limiter enabled",
			"rps_per_ip", cfg.APIRateLimitRPS,
			"exempt_prefixes", []string{"/v1/"},
		)
	}

	// DB health fast-fail gate: returns 503 for DB-dependent paths when the
	// pool is unreachable. Probes, metrics, and UI assets bypass.
	var dbHealth *api.DBHealth
	if sqlDB, dbErr := repo.DB().DB(); dbErr == nil && sqlDB != nil {
		dbHealth = api.NewDBHealth(sqlDB, cfg.DBDriver, metrics)
		dbHealth.Start(appCtx)
		httpHandler = api.DBHealthMiddleware(dbHealth)(httpHandler)
		slog.Info("🩺 DB health middleware enabled", "driver", cfg.DBDriver)
	} else {
		slog.Warn("DB health middleware disabled (cannot get *sql.DB)", "error", dbErr)
	}

	// GraphRAG event-buffer depth poller (Fix 6).
	bootWG.Add(1)
	go func() {
		defer bootWG.Done()
		tick := time.NewTicker(1 * time.Second)
		defer tick.Stop()
		// Store census is len()-under-RLock per tenant — cheap, but 15s is
		// plenty for trend attribution of RSS growth.
		census := time.NewTicker(15 * time.Second)
		defer census.Stop()
		for {
			select {
			case <-appCtx.Done():
				return
			case <-tick.C:
				metrics.GraphRAGEventBufferDepth.Set(float64(graphRAG.EventBufferDepth()))
			case <-census.C:
				c := graphRAG.StoreCounts()
				ent := metrics.GraphRAGStoreEntities
				ent.WithLabelValues("tenants").Set(float64(c.Tenants))
				ent.WithLabelValues("services").Set(float64(c.Services))
				ent.WithLabelValues("operations").Set(float64(c.Operations))
				ent.WithLabelValues("traces").Set(float64(c.Traces))
				ent.WithLabelValues("spans").Set(float64(c.Spans))
				ent.WithLabelValues("log_clusters").Set(float64(c.LogClusters))
				ent.WithLabelValues("metrics").Set(float64(c.Metrics))
				ent.WithLabelValues("anomalies").Set(float64(c.Anomalies))
				edg := metrics.GraphRAGStoreEdges
				edg.WithLabelValues("service").Set(float64(c.ServiceEdges))
				edg.WithLabelValues("trace").Set(float64(c.TraceEdges))
				edg.WithLabelValues("signal").Set(float64(c.SignalEdges))
				edg.WithLabelValues("anomaly").Set(float64(c.AnomalyEdges))
				if ringBuf != nil {
					metrics.TSDBRingSeriesActive.Set(float64(ringBuf.MetricCount()))
				}
				metrics.DrainTemplatesActive.Set(float64(graphRAG.DrainTemplateCount()))
			}
		}
	}()

	// DB pool stats sampler (Task 7 — visibility for DB_MAX_OPEN_CONNS sizing).
	// sql.DB.Stats() is cheap (atomic loads on the pool struct), so 5s is fine.
	bootWG.Add(1)
	go func() {
		defer bootWG.Done()
		sqlDB, err := repo.DB().DB()
		if err != nil || sqlDB == nil {
			slog.Warn("DB pool sampler disabled (cannot get *sql.DB)", "error", err)
			return
		}
		// Initial sample so the gauge has a value immediately after startup.
		metrics.SampleDBPoolStats(sqlDB)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-appCtx.Done():
				return
			case <-tick.C:
				metrics.SampleDBPoolStats(sqlDB)
			}
		}
	}()

	// Panic recovery: OUTERMOST middleware below OTel tracing — ensures any
	// panic in downstream middleware or handlers is logged + metered and the
	// process survives.
	httpHandler = api.RecoverMiddleware(metrics, httpHandler)

	// OTel HTTP instrumentation (outermost — captures every request).
	if shutdownTracer != nil {
		httpHandler = otelhttp.NewHandler(httpHandler, "otelcontext.http")
	}

	srv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           httpHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if tlsMode != "" {
			slog.Info("🔒 HTTPS server started", "port", cfg.HTTPPort, "mode", tlsMode)
			if err := srv.ListenAndServeTLS(tlsCertPath, tlsKeyPath); err != nil && err != http.ErrServerClosed {
				fatal("HTTPS server failed", err)
			}
		} else {
			slog.Info("🌐 HTTP server started (plaintext — not for production)", "port", cfg.HTTPPort)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fatal("HTTP server failed", err)
			}
		}
	}()

	// 9. Graceful Shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("Shutting down OtelContext V5.4...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Ordered shutdown: ingestion → HTTP → hubs/events → processing → DLQ → DB
	// 1. Stop ingestion paths first (no new data)
	grpcServer.GracefulStop()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("HTTP server forced shutdown", "error", err)
	}
	if pprofSrv != nil {
		_ = pprofSrv.Close()
	}

	// 2. Stop real-time hubs and event processing
	hub.Stop()
	cancelEvents()
	// Cancel in-flight LLM calls BEFORE Stop so workers don't burn the
	// 30s LLM deadline waiting on a half-dead upstream during shutdown.
	aiCancel()
	aiService.Stop()

	// 3. Stop processing engines (TSDB flush, graph, GraphRAG)
	if tsdbAgg != nil {
		tsdbAgg.Stop()
	}
	cancelTSDB()
	cancelGraph()
	graphRAG.Stop()
	cancelGraphRAG()

	// 3a. Drain async ingest pipeline. gRPC GracefulStop above guarantees
	// no new Submits land; this blocks until workers finish in-flight
	// batches so a graceful shutdown doesn't lose buffered ingest.
	if ingestPipeline != nil {
		ingestPipeline.Stop()
	}

	// 3b. Drain the aggregate group-commit writer. gRPC GracefulStop above
	// guarantees no new Exports, so this commits whatever was still queued
	// rather than dropping deltas an Export is still waiting on.
	if aggWriter != nil {
		aggWriter.Stop()
	}

	// 4. Stop DLQ (may still be replaying)
	dlq.Stop()

	// 4a. Stop retention + partition schedulers before closing DB (both issue queries).
	cancelDisk()
	diskWatchdog.Stop()
	cancelRetention()
	retention.Stop()
	cancelPartitions()
	if partitionScheduler != nil {
		partitionScheduler.Stop()
	}

	// 4b. Shutdown the OTel tracer provider (flushes pending spans).
	if shutdownTracer != nil {
		if err := shutdownTracer(ctx); err != nil {
			slog.Error("Failed to shutdown tracer provider", "error", err)
		}
	}

	// 4b2. Stop DB health poller before cancelling appCtx so final state is
	// written to the gauge before the pool closes.
	if dbHealth != nil {
		dbHealth.Stop()
	}

	// 4c. Cancel boot-time goroutines (hydrator, DB health poller) and wait
	// with a bounded timeout before closing the DB — otherwise a mid-query
	// hydrator would race with the pool closing underneath it.
	appCancel()
	waitDone := make(chan struct{})
	go func() { bootWG.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		slog.Warn("hydrator did not finish before shutdown; cancelling")
	}

	// 5. Close the databases last (everything above may still write). The
	// aggregate store closes before the main DB: its writer has already
	// drained, and retention — which touches both — has already stopped.
	if aggStore != nil {
		if err := aggStore.Close(); err != nil {
			slog.Error("Failed to close aggregate store", "error", err)
		}
	}
	if err := repo.Close(); err != nil {
		slog.Error("Failed to close database", "error", err)
	}

	slog.Info("✅ OtelContext V5.4 shutdown complete")
}

// recoveryUnaryInterceptor catches panics inside any unary gRPC handler,
// logs the stack, increments the panics-recovered metric, and maps the panic
// to codes.Internal so the connection stays alive.
func recoveryUnaryInterceptor(m *telemetry.Metrics) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("grpc panic recovered",
					"method", info.FullMethod,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				if m != nil && m.PanicsRecoveredTotal != nil {
					m.PanicsRecoveredTotal.WithLabelValues("grpc").Inc()
				}
				err = status.Errorf(codes.Internal, "internal")
			}
		}()
		return handler(ctx, req)
	}
}

// metricsUnaryInterceptor records OtelContext_grpc_requests_total and OtelContext_grpc_request_duration_seconds
// for every unary gRPC call.
func metricsUnaryInterceptor(m *telemetry.Metrics) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		duration := time.Since(start).Seconds()

		status := "ok"
		if err != nil {
			status = "error"
		}
		m.GRPCRequestsTotal.WithLabelValues(info.FullMethod, status).Inc()
		m.GRPCRequestDuration.WithLabelValues(info.FullMethod).Observe(duration)
		return resp, err
	}
}

// initTracerProvider builds an OTel tracer provider that exports spans via OTLP
// gRPC to the configured endpoint. The endpoint can be "host:port" (insecure is
// used since the endpoint is typically the platform's own gRPC port or a local
// collector — TLS to an external collector can be added later).
func initTracerProvider(endpoint string) (*sdktrace.TracerProvider, error) {
	ctx := context.Background()

	client := otlptracegrpc.NewClient(
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	exporter, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("otlptrace.New: %w", err)
	}

	res, err := sdkresource.New(ctx,
		sdkresource.WithAttributes(
			semconv.ServiceName(config.SelfServiceName),
			semconv.ServiceVersion(Version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("sdkresource.New: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	return tp, nil
}

func printBanner() {
	banner := `
  ___ _____ _____ _     
 / _ \_   _| ____| |    
| | | || | |  _| | |    
| |_| || | | |___| |___ 
 \___/ |_| |_____|_____|

  version: %s
`
	fmt.Printf(banner, Version)
}

// graphRAGServiceEdges projects the GraphRAG service store's CALLS edges into
// the storage topology shape. Caller/callee identity is not part of an
// aggregate SeriesKey, so the aggregate read path sources edges here — which is
// also why any response carrying them is marked "sampled" rather than "full".
func graphRAGServiceEdges(ctx context.Context, g *graphrag.GraphRAG) []storage.ServiceMapEdge {
	if g == nil {
		return nil
	}
	all := g.AllServiceEdges(ctx)
	edges := make([]storage.ServiceMapEdge, 0, len(all))
	for _, e := range all {
		if e.Type != "CALLS" {
			continue
		}
		edges = append(edges, storage.ServiceMapEdge{
			Source:       e.FromID,
			Target:       e.ToID,
			CallCount:    e.CallCount,
			AvgLatencyMs: e.AvgMs,
			ErrorRate:    e.ErrorRate,
		})
	}
	return edges
}

// fileBytes returns the size of one file, or 0 when it cannot be stat'ed.
// Attribution only: a missing file is a component that is not using disk.
func fileBytes(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// sidecarBytes returns the combined size of a SQLite file's -wal and -shm
// sidecars. They are charged to the WAL/temp tier of the budget table, not to
// the database tier: a checkpoint moves bytes between the two and the gauges
// should show that rather than double-count it.
func sidecarBytes(path string) int64 {
	if path == "" {
		return 0
	}
	return fileBytes(path+"-wal") + fileBytes(path+"-shm")
}

// sqliteMainDBPath resolves the main relational DB file for SQLite. Mirrors the
// default in storage.NewDatabase; returns "" for every server-backed driver,
// where there is no local file to stat.
func sqliteMainDBPath(cfg *config.Config) string {
	if strings.ToLower(cfg.DBDriver) != "sqlite" && cfg.DBDriver != "" {
		return ""
	}
	if cfg.DBDSN == "" {
		return "OtelContext.db"
	}
	return cfg.DBDSN
}

// legacyMetricPath reports whether the legacy metric path — the TSDB
// aggregator, its ring buffer, the 30s flush into metric_buckets and the
// per-point metric callback — is constructed for a given AGGREGATE_MODE.
//
// Only AGGREGATE_MODE=aggregate retires it (#194 finding 10). Legacy has no
// other metric store, and shadow's entire purpose is running both paths at
// once and comparing them, so both keep it. It is a function rather than an
// inline comparison so the wiring decision is assertable without booting the
// process.
func legacyMetricPath(mode string) bool { return mode != aggregate.ModeAggregate }
