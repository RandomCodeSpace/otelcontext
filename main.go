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

	"github.com/RandomCodeSpace/central-ops/pkg/version"

	"github.com/RandomCodeSpace/otelcontext/internal/ai"
	"github.com/RandomCodeSpace/otelcontext/internal/api"
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
	"github.com/RandomCodeSpace/otelcontext/internal/vectordb"

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
var Version = version.Detect()

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
	if err := cfg.ValidateDBForEnv(); err != nil {
		fatal("DB/Env validation", err)
	}
	if strings.EqualFold(cfg.DBDriver, "sqlite") {
		slog.Warn("SQLite driver in use — suitable for dev/small deployments only. " +
			"Expected cap: ~5 services, ~1k events/sec sustained.")
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

	// 2a. Retention scheduler: hourly batched purge + daily VACUUM/ANALYZE.
	ctxRetention, cancelRetention := context.WithCancel(context.Background())
	retention := storage.NewRetentionScheduler(
		repo,
		cfg.HotRetentionDays,
		cfg.RetentionBatchSize,
		time.Duration(cfg.RetentionBatchSleepMs)*time.Millisecond,
	)
	retention.Start(ctxRetention)
	slog.Info("🧹 Retention scheduler started", "retention_days", cfg.HotRetentionDays)

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
			if err2 := json.Unmarshal(data, &logs); err2 != nil {
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
	slog.Info("🔁 DLQ initialized", "path", cfg.DLQPath, "interval", replayInterval)

	// 4. Initialize Real-Time WebSocket Hub
	hub := realtime.NewHub(func(count int) {
		metrics.SetActiveConnections(count)
	})
	hub.SetDevMode(cfg.DevMode)
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
	ctxEvents, cancelEvents := context.WithCancel(context.Background())
	go eventHub.Start(ctxEvents, 5*time.Second, 500*time.Millisecond)
	slog.Info("⚡ Event notification hub started (5s snapshots, 500ms batches)")

	// 4c. Initialize TSDB Aggregator + Ring Buffer
	tsdbAgg := tsdb.NewAggregator(repo, 30*time.Second)
	if cfg.MetricMaxCardinality > 0 {
		tsdbAgg.SetCardinalityLimit(cfg.MetricMaxCardinality, func() {
			metrics.TSDBCardinalityOverflow.Inc()
		})
		slog.Info("📈 TSDB cardinality limit set", "max", cfg.MetricMaxCardinality)
	}
	tsdbAgg.SetMetrics(
		func() { metrics.TSDBIngestTotal.Inc() },
		func() { metrics.TSDBBatchesDropped.Inc() },
	)
	ringBuf := tsdb.NewRingBuffer(120, 30*time.Second)
	tsdbAgg.SetRingBuffer(ringBuf)
	slog.Info("📈 TSDB ring buffer attached (120 slots × 30s = 1h retention)")

	ctxTSDB, cancelTSDB := context.WithCancel(context.Background())
	go tsdbAgg.Start(ctxTSDB)
	slog.Info("📈 TSDB Aggregator started (30s window)")

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
	go svcGraph.Start(ctxGraph)
	slog.Info("🕸️  In-memory service graph started (5m window, 30s refresh)")

	// 4f. Initialize vector index for semantic log search
	vectorIdx := vectordb.New(cfg.VectorIndexMaxEntries)
	slog.Info("🔍 Vector index initialized", "max_entries", cfg.VectorIndexMaxEntries)

	// Hydrate vector index from recent ERROR/WARN logs on startup (non-blocking).
	// Uses appCtx so SIGTERM during boot cancels the query before repo.Close().
	bootWG.Add(1)
	go func() {
		defer bootWG.Done()
		recentLogs, _, err := repo.GetLogsV2(appCtx, storage.LogFilter{
			Severity:  "ERROR",
			StartTime: time.Now().Add(-24 * time.Hour),
			EndTime:   time.Now(),
			Limit:     5000,
		})
		if err == nil {
			for _, l := range recentLogs {
				vectorIdx.Add(l.ID, l.TenantID, l.ServiceName, l.Severity, l.Body)
			}
			slog.Info("🔍 Vector index hydrated from recent ERROR logs", "count", len(recentLogs))
		}
	}()

	// 4g. Initialize GraphRAG (replaces simple graph for advanced queries)
	graphrag.SetPanicMetrics(metrics)
	graphRAGCfg := graphrag.DefaultConfig()
	graphRAGCfg.WorkerCount = cfg.GraphRAGWorkerCount
	graphRAGCfg.ChannelSize = cfg.GraphRAGEventQueueSize
	graphRAG := graphrag.New(repo, vectorIdx, tsdbAgg, ringBuf, graphRAGCfg)
	graphRAG.SetMetrics(metrics)
	ctxGraphRAG, cancelGraphRAG := context.WithCancel(context.Background())
	go graphRAG.Start(ctxGraphRAG)
	slog.Info("GraphRAG started (layered graph with anomaly detection)",
		"workers", cfg.GraphRAGWorkerCount,
		"event_queue_size", cfg.GraphRAGEventQueueSize,
	)

	// Auto-migrate GraphRAG models (Investigation, GraphSnapshot)
	if err := graphrag.AutoMigrateGraphRAG(repo.DB()); err != nil {
		slog.Error("Failed to migrate GraphRAG models", "error", err)
	}

	// 5. Initialize AI Service
	aiService := ai.NewService(repo)

	// 6. Initialize API Server
	apiServer := api.NewServer(repo, hub, eventHub, metrics)
	apiServer.SetGraph(svcGraph)
	apiServer.SetGraphRAG(graphRAG)
	apiServer.SetVectorIndex(vectorIdx)

	// 6b. Initialize MCP Server (HTTP Streamable, JSON-RPC 2.0 + SSE)
	mcpServer := mcp.New(repo, metrics, svcGraph, vectorIdx)
	mcpServer.SetGraphRAG(graphRAG)
	slog.Info("🤖 MCP server initialized", "path", cfg.MCPPath, "enabled", cfg.MCPEnabled)

	// 7. Initialize OTLP Ingestion (gRPC)
	traceServer := ingest.NewTraceServer(repo, metrics, cfg)
	logsServer := ingest.NewLogsServer(repo, metrics, cfg)
	metricsServer := ingest.NewMetricsServer(repo, metrics, tsdbAgg, cfg)

	// Wire adaptive sampler (only when rate < 1.0 to avoid unnecessary overhead)
	if cfg.SamplingRate > 0 && cfg.SamplingRate < 1.0 {
		sampler := ingest.NewSampler(cfg.SamplingRate, cfg.SamplingAlwaysOnErrors, float64(cfg.SamplingLatencyThresholdMs))
		traceServer.SetSampler(sampler)
		slog.Info("🎯 Adaptive trace sampling enabled",
			"rate", cfg.SamplingRate,
			"always_errors", cfg.SamplingAlwaysOnErrors,
			"latency_threshold_ms", cfg.SamplingLatencyThresholdMs,
		)
	}

	// Wire up live log streaming + AI + DLQ metrics
	logHandler := func(l storage.Log) {
		start := time.Now()
		eventHub.BroadcastLog(realtime.LogEntry{
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
		vectorIdx.Add(l.ID, l.TenantID, l.ServiceName, l.Severity, l.Body)
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

	metricsServer.SetMetricCallback(func(m tsdb.RawMetric) {
		eventHub.BroadcastMetric(realtime.MetricEntry{
			Name:        m.Name,
			ServiceName: m.ServiceName,
			Value:       m.Value,
			Timestamp:   m.Timestamp,
			Attributes:  m.Attributes,
		})
		graphRAG.OnMetricIngested(m)
	})

	// Update DLQ size metric periodically
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			metrics.SetDLQSize(dlq.Size())
			metrics.DLQDiskBytes.Set(float64(dlq.DiskBytes()))
		}
	}()

	// Resolve TLS material once: explicit cert-file > self-signed > plaintext.
	// Both gRPC and HTTP reuse the same resolved paths below.
	var (
		tlsCertPath string
		tlsKeyPath  string
		tlsMode     string // "cert-file", "self-signed", or "" (plaintext)
	)
	switch {
	case cfg.TLSCertFileMode():
		tlsCertPath = cfg.TLSCertFile
		tlsKeyPath = cfg.TLSKeyFile
		tlsMode = "cert-file"
	case cfg.TLSSelfsignedMode():
		cp, kp, err := tlsbootstrap.EnsureSelfSignedCert(cfg.TLSCacheDir)
		if err != nil {
			fatal("Failed to bootstrap self-signed TLS cert", err)
		}
		tlsCertPath = cp
		tlsKeyPath = kp
		tlsMode = "self-signed"
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
	slog.Info("📡 gRPC server tuned",
		"max_recv_mb", recvBytes,
		"max_concurrent_streams", streams,
	)
	switch tlsMode {
	case "cert-file":
		creds, err := credentials.NewServerTLSFromFile(tlsCertPath, tlsKeyPath)
		if err != nil {
			fatal("Failed to load gRPC TLS credentials", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
		slog.Info("🔒 gRPC TLS enabled", "mode", "cert-file")
	case "self-signed":
		creds, err := credentials.NewServerTLSFromFile(tlsCertPath, tlsKeyPath)
		if err != nil {
			fatal("Failed to load gRPC TLS credentials (self-signed)", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
		slog.Info("🔒 gRPC TLS enabled", "mode", "self-signed", "cache_dir", cfg.TLSCacheDir)
	default:
		slog.Info("🔓 gRPC plaintext — not for production; set TLS_CERT_FILE/TLS_KEY_FILE or TLS_AUTO_SELFSIGNED=true")
	}
	grpcServer := grpc.NewServer(grpcOpts...)
	coltracepb.RegisterTraceServiceServer(grpcServer, traceServer)
	collogspb.RegisterLogsServiceServer(grpcServer, logsServer)
	colmetricspb.RegisterMetricsServiceServer(grpcServer, metricsServer)
	reflection.Register(grpcServer)

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
	uiServer := ui.NewServer(repo, metrics, svcGraph, vectorIdx)
	uiServer.SetMCPConfig(cfg.MCPEnabled, cfg.MCPPath)
	if err := uiServer.RegisterRoutes(mux); err != nil {
		fatal("Failed to register UI routes", err)
	}

	var httpHandler http.Handler = mux

	// Resolve tenant on /api/* read-side requests (passes through OTLP /v1,
	// MCP, UI assets, and health probes untouched).
	httpHandler = api.TenantMiddleware(cfg)(httpHandler)

	// Wire auth-failure metric hook before installing any auth middleware.
	api.AuthFailureHook = func(reason string) {
		metrics.APIAuthFailuresTotal.WithLabelValues(reason).Inc()
	}

	// Authentication. Per-tenant keys (if configured) take precedence over the
	// shared API key — they enforce tenant boundaries at the auth layer rather
	// than trusting a client-supplied X-Tenant-ID header.
	switch {
	case cfg.APITenantKeysFile != "":
		entries, err := api.LoadTenantKeys(cfg.APITenantKeysFile)
		if err != nil {
			fatal("load tenant keys file", err, "path", cfg.APITenantKeysFile)
		}
		tka := api.NewTenantKeyAuth(entries)
		httpHandler = tka.Middleware(cfg.MCPPath, httpHandler)
		slog.Info("🔑 Per-tenant API key authentication enabled", "tenants", len(entries))
	case cfg.APIKey != "":
		httpHandler = api.APIKeyGate(cfg.APIKey, cfg.MCPPath, httpHandler)
		slog.Info("🔑 API key authentication enabled (shared key)")
	default:
		slog.Warn("API authentication disabled — set API_KEY or API_TENANT_KEYS_FILE for production")
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
		for {
			select {
			case <-appCtx.Done():
				return
			case <-tick.C:
				metrics.GraphRAGEventBufferDepth.Set(float64(graphRAG.EventBufferDepth()))
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

	// 2. Stop real-time hubs and event processing
	hub.Stop()
	cancelEvents()
	aiService.Stop()

	// 3. Stop processing engines (TSDB flush, graph, GraphRAG)
	tsdbAgg.Stop()
	cancelTSDB()
	cancelGraph()
	graphRAG.Stop()
	cancelGraphRAG()

	// 4. Stop DLQ (may still be replaying)
	dlq.Stop()

	// 4a. Stop retention scheduler before closing DB (it issues queries).
	cancelRetention()
	retention.Stop()

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

	// 5. Close database last (everything above may still write)
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
			semconv.ServiceName("otelcontext"),
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
