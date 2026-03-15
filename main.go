package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/RandomCodeSpace/argus/internal/ai"
	"github.com/RandomCodeSpace/argus/internal/api"
	"github.com/RandomCodeSpace/argus/internal/archive"
	"github.com/RandomCodeSpace/argus/internal/config"
	"github.com/RandomCodeSpace/argus/internal/graph"
	"github.com/RandomCodeSpace/argus/internal/ingest"
	"github.com/RandomCodeSpace/argus/internal/mcp"
	"github.com/RandomCodeSpace/argus/internal/queue"
	"github.com/RandomCodeSpace/argus/internal/realtime"
	"github.com/RandomCodeSpace/argus/internal/storage"
	"github.com/RandomCodeSpace/argus/internal/telemetry"
	"github.com/RandomCodeSpace/argus/internal/tsdb"
	"github.com/RandomCodeSpace/argus/internal/vectordb"
	"github.com/RandomCodeSpace/argus/web"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	_ "google.golang.org/grpc/encoding/gzip" // Register gzip decompressor
	"google.golang.org/grpc/reflection"
)

func main() {
	// Force UTC timezone globally — prevents system timezone leaking into timestamps
	time.Local = time.UTC

	printBanner()

	// 0. Load Configuration
	cfg := config.Load()

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

	slog.Info("🚀 Starting Argus V5.4", "env", cfg.Env, "log_level", level)

	// 1. Initialize Internal Telemetry (first — everything registers metrics against this)
	metrics := telemetry.New()
	slog.Info("📊 Internal telemetry initialized")

	// 2. Initialize Storage
	repo, err := storage.NewRepository(metrics)
	if err != nil {
		log.Fatalf("Failed to initialize repository: %v", err)
	}
	slog.Info("💾 Storage initialized", "driver", cfg.DBDriver)

	// 3. Initialize DLQ (Dead Letter Queue)
	replayInterval, err := time.ParseDuration(cfg.DLQReplayInterval)
	if err != nil {
		replayInterval = 5 * time.Minute
	}

	dlq, err := queue.NewDLQ(cfg.DLQPath, replayInterval, func(data []byte) error {
		// Replay handler: try to deserialize and re-insert logs
		var logs []storage.Log
		if err := json.Unmarshal(data, &logs); err != nil {
			return fmt.Errorf("DLQ replay unmarshal failed: %w", err)
		}
		return repo.BatchCreateLogs(logs)
	})
	if err != nil {
		log.Fatalf("Failed to initialize DLQ: %v", err)
	}
	defer dlq.Stop()
	slog.Info("🔁 DLQ initialized", "path", cfg.DLQPath, "interval", replayInterval)

	// 4. Initialize Real-Time WebSocket Hub
	hub := realtime.NewHub(func(count int) {
		metrics.SetActiveConnections(count)
	})
	go hub.Run()
	defer hub.Stop()
	slog.Info("🔌 WebSocket hub started")

	// 4b. Initialize Event Notification Hub (for live mode — pushes data snapshots)
	eventHub := realtime.NewEventHub(
		repo,
		metrics.IncrementActiveConns,
		metrics.DecrementActiveConns,
	)
	ctxEvents, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	go eventHub.Start(ctxEvents, 5*time.Second, 500*time.Millisecond)
	slog.Info("⚡ Event notification hub started (5s snapshots, 500ms batches)")

	// 4c. Initialize TSDB Aggregator
	tsdbAgg := tsdb.NewAggregator(repo, 30*time.Second)
	ctxTSDB, cancelTSDB := context.WithCancel(context.Background())
	defer cancelTSDB()
	go tsdbAgg.Start(ctxTSDB)
	slog.Info("📈 TSDB Aggregator started (30s window)")

	// 4d. Initialize Archive Worker (hot/cold storage tiering)
	archiver := archive.New(repo, cfg)
	ctxArchive, cancelArchive := context.WithCancel(context.Background())
	defer cancelArchive()
	go archiver.Start(ctxArchive)
	slog.Info("🗄️  Archive worker started",
		"hot_retention_days", cfg.HotRetentionDays,
		"cold_path", cfg.ColdStoragePath,
	)

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
	defer cancelGraph()
	go svcGraph.Start(ctxGraph)
	slog.Info("🕸️  In-memory service graph started (5m window, 30s refresh)")

	// 4f. Initialize vector index for semantic log search
	vectorIdx := vectordb.New(cfg.VectorIndexMaxEntries)
	slog.Info("🔍 Vector index initialized", "max_entries", cfg.VectorIndexMaxEntries)

	// 5. Initialize AI Service
	aiService := ai.NewService(repo)
	defer aiService.Stop()

	// 6. Initialize API Server
	apiServer := api.NewServer(repo, hub, eventHub, metrics)
	apiServer.SetGraph(svcGraph)

	// 6b. Initialize MCP Server (HTTP Streamable, JSON-RPC 2.0 + SSE)
	mcpServer := mcp.New(repo, metrics, svcGraph, vectorIdx)
	slog.Info("🤖 MCP server initialized", "path", cfg.MCPPath, "enabled", cfg.MCPEnabled)

	// 7. Initialize OTLP Ingestion (gRPC)
	traceServer := ingest.NewTraceServer(repo, metrics, cfg)
	logsServer := ingest.NewLogsServer(repo, metrics, cfg)
	metricsServer := ingest.NewMetricsServer(repo, metrics, tsdbAgg, cfg)

	// Wire up live log streaming + AI + DLQ metrics
	logHandler := func(l storage.Log) {
		start := time.Now()
		eventHub.BroadcastLog(realtime.LogEntry{
			ID:             l.ID,
			TraceID:        l.TraceID,
			SpanID:         l.SpanID,
			Severity:       l.Severity,
			Body:           string(l.Body),
			ServiceName:    l.ServiceName,
			AttributesJSON: string(l.AttributesJSON),
			AIInsight:      string(l.AIInsight),
			Timestamp:      l.Timestamp,
		})
		aiService.EnqueueLog(l)
		vectorIdx.Add(l.ID, l.ServiceName, l.Severity, string(l.Body))
		eventHub.NotifyRefresh()
		if time.Since(start) > 100*time.Millisecond {
			slog.Warn("Slow broadcast/enqueue", "duration", time.Since(start))
		}
	}

	logsServer.SetLogCallback(logHandler)
	traceServer.SetLogCallback(logHandler)

	metricsServer.SetMetricCallback(func(m tsdb.RawMetric) {
		eventHub.BroadcastMetric(realtime.MetricEntry{
			Name:        m.Name,
			ServiceName: m.ServiceName,
			Value:       m.Value,
			Timestamp:   m.Timestamp,
			Attributes:  m.Attributes,
		})
	})

	// Update DLQ size metric periodically
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				metrics.SetDLQSize(dlq.Size())
			}
		}
	}()

	// Start gRPC Server
	lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		log.Fatalf("Failed to listen on :%s: %v", cfg.GRPCPort, err)
	}
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(metricsUnaryInterceptor(metrics)),
	)
	coltracepb.RegisterTraceServiceServer(grpcServer, traceServer)
	collogspb.RegisterLogsServiceServer(grpcServer, logsServer)
	colmetricspb.RegisterMetricsServiceServer(grpcServer, metricsServer)
	reflection.Register(grpcServer)

	go func() {
		slog.Info("📡 gRPC OTLP receiver started", "port", cfg.GRPCPort)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("Failed to serve gRPC: %v", err)
		}
	}()

	// Start runtime metrics sampling (every 15s)
	metrics.StartRuntimeMetrics()
	slog.Info("📊 Runtime metrics sampling started")

	// 8. Start HTTP Server
	mux := http.NewServeMux()
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

	// SPA Handler
	distFS, err := web.DistFS()
	if err != nil {
		log.Fatalf("Failed to load embedded frontend: %v", err)
	}
	fileServer := http.FileServer(http.FS(distFS))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "index.html"
		} else if path[0] == '/' {
			path = path[1:]
		}

		f, err := distFS.Open(path)
		if err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA catch-all → serve index.html
		f, err = distFS.Open("index.html")
		if err != nil {
			http.Error(w, "index.html not found", http.StatusInternalServerError)
			return
		}
		defer f.Close()

		stat, _ := f.Stat()
		http.ServeContent(w, r, "index.html", stat.ModTime(), f.(interface {
			Read(p []byte) (n int, err error)
			Seek(offset int64, whence int) (int64, error)
		}))
	})

	srv := &http.Server{
		Addr:    ":" + cfg.HTTPPort,
		Handler: api.MetricsMiddleware(metrics, mux),
	}

	go func() {
		slog.Info("🌐 HTTP server started", "port", cfg.HTTPPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// 9. Graceful Shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("Shutting down ARGUS V5.4...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 1. Stop high-ingestion paths
	grpcServer.GracefulStop()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("HTTP server forced shutdown", "error", err)
	}

	// 2. Stop processing engines (order: Hubs -> AI -> TSDB)
	aiService.Stop()
	tsdbAgg.Stop()
	eventHub.Stop() // Note: New Stop() method should be called if implemented, otherwise context handles it

	slog.Info("✅ ARGUS V5.4 shutdown complete")
}

// metricsUnaryInterceptor records argus_grpc_requests_total and argus_grpc_request_duration_seconds
// for every unary gRPC call.
func metricsUnaryInterceptor(m *telemetry.Metrics) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
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

func printBanner() {
	banner := `
     _    ____   ____ _   _ ____   __     _____ _____ ____  
    / \  |  _ \ / ___| | | / ___|  \ \   / / ___|___ /|  _ \ 
   / _ \ | |_) | |  _| | | \___ \   \ \ / /|___ \ |_ \| | | |
  / ___ \|  _ <| |_| | |_| |___) |   \ V /  ___) |__) | |_| |
 /_/   \_\_| \_\\____|\\___/|____/     \_/  |____/____/|____/ 

  ARGUS V5.4 (EMBEDDED TSDB) — High Performance Edition
  The Eye That Never Sleeps 👁️
`
	fmt.Println(banner)
}
