package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"runtime"

	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/tsdb"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

// tenantHeader is the canonical HTTP / gRPC metadata key used to override the
// default tenant on OTLP ingest. Case-insensitive on the wire.
const tenantHeader = "x-tenant-id"

// tenantFromContext extracts a tenant ID from context or gRPC metadata.
// Precedence: storage.TenantFromContext (set by HTTP handler / read-side
// middleware) > gRPC metadata x-tenant-id > "".
//
// The storage package owns the single tenant context key — this package reads
// it via storage.TenantFromContext so write and read paths share identical
// plumbing and can never drift.
func tenantFromContext(ctx context.Context) string {
	if t := storage.TenantFromContext(ctx); t != "" && t != storage.DefaultTenantID {
		return t
	}
	// storage.TenantFromContext coerces missing values to DefaultTenantID, so
	// a genuine "no value set" is indistinguishable from "explicitly default".
	// Probe the raw key to tell the two apart before falling back to metadata.
	if hasStorageTenant(ctx) {
		return storage.TenantFromContext(ctx)
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(tenantHeader); len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	return ""
}

// hasStorageTenant reports whether the context already carries a tenant value
// stashed by storage.WithTenantContext. It uses a public probe exported from
// storage to avoid duplicating the context key type here.
func hasStorageTenant(ctx context.Context) bool {
	return storage.HasTenantContext(ctx)
}

// resolveTenant picks the first non-empty value from
// (context/metadata, configured default). The OTLP resource attribute
// "tenant.id" path is gated behind cfg.TrustResourceTenant — disabled by
// default so a compromised SDK cannot forge another tenant's data.
func resolveTenant(ctx context.Context, resourceAttrs []*commonpb.KeyValue, fallback string, trustResourceAttr bool) string {
	if t := tenantFromContext(ctx); t != "" {
		return t
	}
	if trustResourceAttr {
		if t := tenantFromResource(resourceAttrs); t != "" {
			return t
		}
	}
	if fallback != "" {
		return fallback
	}
	return storage.DefaultTenantID
}

// tenantFromResource looks for an OTLP resource attribute "tenant.id".
// Only consulted when cfg.TrustResourceTenant=true (off by default) —
// see resolveTenant.
func tenantFromResource(attrs []*commonpb.KeyValue) string {
	for _, kv := range attrs {
		if kv.Key == "tenant.id" {
			return kv.Value.GetStringValue()
		}
	}
	return ""
}

type TraceServer struct {
	repo                *storage.Repository
	metrics             *telemetry.Metrics
	logCallback         func(storage.Log)
	spanCallback        func(storage.Span) // called for each span after persistence
	minSeverity         int
	allowedServices     map[string]bool
	excludedServices    map[string]bool
	sampler             *Sampler  // nil = no sampling (keep all)
	pipeline            *Pipeline // nil = synchronous DB writes (legacy path)
	latencyThresholdMs  float64   // spans slower than this are flagged HasSlow for the pipeline
	defaultTenant       string
	trustResourceTenant bool
	coltracepb.UnimplementedTraceServiceServer
}

type LogsServer struct {
	repo                *storage.Repository
	metrics             *telemetry.Metrics
	logCallback         func(storage.Log)
	minSeverity         int
	allowedServices     map[string]bool
	excludedServices    map[string]bool
	pipeline            *Pipeline // nil = synchronous DB writes (legacy path)
	defaultTenant       string
	trustResourceTenant bool
	collogspb.UnimplementedLogsServiceServer
}

type MetricsServer struct {
	repo                *storage.Repository
	metrics             *telemetry.Metrics
	aggregator          *tsdb.Aggregator
	metricCallback      func(tsdb.RawMetric)
	allowedServices     map[string]bool
	excludedServices    map[string]bool
	defaultTenant       string
	trustResourceTenant bool
	colmetricspb.UnimplementedMetricsServiceServer
}

func NewTraceServer(repo *storage.Repository, metrics *telemetry.Metrics, cfg *config.Config) *TraceServer {
	return &TraceServer{
		repo:                repo,
		metrics:             metrics,
		minSeverity:         parseSeverity(cfg.IngestMinSeverity),
		allowedServices:     parseServiceList(cfg.IngestAllowedServices),
		excludedServices:    parseServiceList(cfg.IngestExcludedServices),
		latencyThresholdMs:  float64(cfg.SamplingLatencyThresholdMs),
		defaultTenant:       cfg.DefaultTenant,
		trustResourceTenant: cfg.OTLPTrustResourceTenant,
	}
}

// SetLogCallback sets the function to call when a new log is synthesized from a trace.
func (s *TraceServer) SetLogCallback(cb func(storage.Log)) {
	s.logCallback = cb
}

// SetSpanCallback sets the function to call when spans are persisted.
func (s *TraceServer) SetSpanCallback(cb func(storage.Span)) {
	s.spanCallback = cb
}

// SetSampler enables adaptive trace sampling. Pass nil to disable.
func (s *TraceServer) SetSampler(sm *Sampler) {
	s.sampler = sm
}

// SetPipeline enables the async ingest pipeline. When set, Export()
// returns to the caller as soon as the parsed batch is enqueued (or
// rejected), and persistence runs on the pipeline's worker pool. Pass
// nil to revert to the synchronous DB-write path.
func (s *TraceServer) SetPipeline(p *Pipeline) {
	s.pipeline = p
}

// SetPipeline enables the async ingest pipeline for log export. Same
// semantics as TraceServer.SetPipeline.
func (s *LogsServer) SetPipeline(p *Pipeline) {
	s.pipeline = p
}

func NewLogsServer(repo *storage.Repository, metrics *telemetry.Metrics, cfg *config.Config) *LogsServer {
	return &LogsServer{
		repo:                repo,
		metrics:             metrics,
		minSeverity:         parseSeverity(cfg.IngestMinSeverity),
		allowedServices:     parseServiceList(cfg.IngestAllowedServices),
		excludedServices:    parseServiceList(cfg.IngestExcludedServices),
		defaultTenant:       cfg.DefaultTenant,
		trustResourceTenant: cfg.OTLPTrustResourceTenant,
	}
}

// SetLogCallback sets the function to call when a new log is received.
func (s *LogsServer) SetLogCallback(cb func(storage.Log)) {
	s.logCallback = cb
}

func NewMetricsServer(repo *storage.Repository, metrics *telemetry.Metrics, aggregator *tsdb.Aggregator, cfg *config.Config) *MetricsServer {
	return &MetricsServer{
		repo:                repo,
		metrics:             metrics,
		aggregator:          aggregator,
		allowedServices:     parseServiceList(cfg.IngestAllowedServices),
		excludedServices:    parseServiceList(cfg.IngestExcludedServices),
		defaultTenant:       cfg.DefaultTenant,
		trustResourceTenant: cfg.OTLPTrustResourceTenant,
	}
}

// SetMetricCallback sets the function to call when a new metric point is received.
func (s *MetricsServer) SetMetricCallback(cb func(tsdb.RawMetric)) {
	s.metricCallback = cb
}

// Export handles incoming OTLP metrics data.
func (s *MetricsServer) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	for _, resourceMetrics := range req.ResourceMetrics {
		serviceName := getServiceName(resourceMetrics.Resource.Attributes)

		if !shouldIngestService(serviceName, s.allowedServices, s.excludedServices) {
			continue
		}

		tenantID := resolveTenant(ctx, resourceMetrics.Resource.Attributes, s.defaultTenant, s.trustResourceTenant)

		for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
			for _, m := range scopeMetrics.Metrics {
				var points []*metricspb.NumberDataPoint

				// Extract points based on metric type
				switch m.Data.(type) {
				case *metricspb.Metric_Gauge:
					points = m.GetGauge().DataPoints
				case *metricspb.Metric_Sum:
					points = m.GetSum().DataPoints
				}

				for _, p := range points {
					var val float64
					if p.Value != nil {
						switch v := p.Value.(type) {
						case *metricspb.NumberDataPoint_AsDouble:
							val = v.AsDouble
						case *metricspb.NumberDataPoint_AsInt:
							val = float64(v.AsInt)
						}
					}

					raw := tsdb.RawMetric{
						Name:        m.Name,
						ServiceName: serviceName,
						Value:       val,
						Timestamp:   time.Unix(0, int64(p.TimeUnixNano)), // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
						Attributes:  make(map[string]any),
						TenantID:    tenantID,
					}

					// Convert attributes to map for TSDB grouping
					for _, kv := range p.Attributes {
						raw.Attributes[kv.Key] = kv.Value.String()
					}

					// 1. Process via TSDB Aggregator (for storage)
					if s.aggregator != nil {
						s.aggregator.Ingest(raw)
					}

					// 2. Real-time bypass (for live charts)
					if s.metricCallback != nil {
						s.metricCallback(raw)
					}
				}
			}
		}
	}

	if s.metrics != nil {
		// Just a marker for Prometheus that metrics were received
		s.metrics.RecordIngestion(1)
	}

	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

// Export handles incoming OTLP trace data.
func (s *TraceServer) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	slog.Debug("📥 [TRACES] Received Request", "resource_spans", len(req.ResourceSpans))

	type batchResult struct {
		spans   []storage.Span
		traces  []storage.Trace
		logs    []storage.Log
		hasErr  bool // any span in this slice had STATUS_CODE_ERROR
		hasSlow bool // any span exceeded latencyThresholdMs
	}

	results := make([]batchResult, len(req.ResourceSpans))

	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(runtime.GOMAXPROCS(0) * 4)

	for idx, resourceSpans := range req.ResourceSpans {
		g.Go(func() error {
			serviceName := getServiceName(resourceSpans.Resource.Attributes)

			if !shouldIngestService(serviceName, s.allowedServices, s.excludedServices) {
				slog.Debug("🚫 [TRACES] Dropped service", "service", serviceName)
				return nil
			}

			tenantID := resolveTenant(ctx, resourceSpans.Resource.Attributes, s.defaultTenant, s.trustResourceTenant)

			localSpans := make([]storage.Span, 0)
			localTraces := make([]storage.Trace, 0)
			localLogs := make([]storage.Log, 0)
			var localHasErr, localHasSlow bool

			for _, scopeSpans := range resourceSpans.ScopeSpans {
				for _, span := range scopeSpans.Spans {
					startTime := time.Unix(0, int64(span.StartTimeUnixNano)) // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
					endTime := time.Unix(0, int64(span.EndTimeUnixNano))     // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
					duration := endTime.Sub(startTime).Microseconds()

					// Adaptive sampling: evaluate before any allocations.
					statusStr := "STATUS_CODE_UNSET"
					if span.Status != nil {
						statusStr = span.Status.Code.String()
					}
					if s.sampler != nil {
						isError := statusStr == "STATUS_CODE_ERROR"
						durationMs := float64(duration) / 1000.0
						if !s.sampler.ShouldSample(serviceName, isError, durationMs) {
							continue
						}
					}

					attrs, _ := json.Marshal(span.Attributes)

					// Create Span Model
					sModel := storage.Span{
						TenantID:       tenantID,
						TraceID:        fmt.Sprintf("%x", span.TraceId),
						SpanID:         fmt.Sprintf("%x", span.SpanId),
						ParentSpanID:   fmt.Sprintf("%x", span.ParentSpanId),
						OperationName:  span.Name,
						StartTime:      startTime,
						EndTime:        endTime,
						Duration:       duration,
						ServiceName:    serviceName,
						Status:         statusStr,
						AttributesJSON: storage.CompressedText(attrs),
					}
					localSpans = append(localSpans, sModel)

					// Flag the batch for the async pipeline's priority lane.
					// Errors and slow spans bypass soft-backpressure drops so
					// diagnostic data is never silently lost at >=90% queue.
					if statusStr == "STATUS_CODE_ERROR" {
						localHasErr = true
					}
					if s.latencyThresholdMs > 0 && float64(duration)/1000.0 >= s.latencyThresholdMs {
						localHasSlow = true
					}

					tModel := storage.Trace{
						TenantID:    tenantID,
						TraceID:     fmt.Sprintf("%x", span.TraceId),
						ServiceName: serviceName,
						Timestamp:   startTime,
						Duration:    duration,
						Status:      statusStr,
					}
					localTraces = append(localTraces, tModel)

					// Synthesize Logs from Span Events (exceptions) and Status
					for _, event := range span.Events {
						severity := "INFO"
						if event.Name == "exception" {
							severity = "ERROR"
						}

						if !shouldIngestSeverity(severity, s.minSeverity) {
							continue
						}

						body := event.Name
						for _, attr := range event.Attributes {
							if attr.Key == "exception.message" || attr.Key == "message" {
								body = attr.Value.GetStringValue()
								break
							}
						}

						eventAttrs, _ := json.Marshal(event.Attributes)

						l := storage.Log{
							TenantID:       tenantID,
							TraceID:        fmt.Sprintf("%x", span.TraceId),
							SpanID:         fmt.Sprintf("%x", span.SpanId),
							Severity:       severity,
							Body:           body,
							ServiceName:    serviceName,
							AttributesJSON: storage.CompressedText(eventAttrs),
							Timestamp:      time.Unix(0, int64(event.TimeUnixNano)), // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
						}
						localLogs = append(localLogs, l)
					}

					hasErrorLog := false
					for _, sl := range localLogs {
						if sl.Severity == "ERROR" && sl.SpanID == fmt.Sprintf("%x", span.SpanId) {
							hasErrorLog = true
							break
						}
					}

					if !hasErrorLog && span.Status != nil && span.Status.Code == tracepb.Status_STATUS_CODE_ERROR {
						if shouldIngestSeverity("ERROR", s.minSeverity) {
							msg := span.Status.Message
							if msg == "" {
								msg = fmt.Sprintf("Span '%s' failed", span.Name)
							}

							l := storage.Log{
								TenantID:       tenantID,
								TraceID:        fmt.Sprintf("%x", span.TraceId),
								SpanID:         fmt.Sprintf("%x", span.SpanId),
								Severity:       "ERROR",
								Body:           msg,
								ServiceName:    serviceName,
								AttributesJSON: "{}",
								Timestamp:      endTime,
							}
							localLogs = append(localLogs, l)
						}
					}
				}
			}

			// Store results in pre-allocated slot (no mutex needed)
			results[idx] = batchResult{
				spans:   localSpans,
				traces:  localTraces,
				logs:    localLogs,
				hasErr:  localHasErr,
				hasSlow: localHasSlow,
			}

			return nil
		})
	}

	_ = g.Wait()

	// Merge results after all goroutines complete (no lock contention)
	var spansToInsert []storage.Span
	var tracesToUpsert []storage.Trace
	var synthesizedLogs []storage.Log
	var batchHasErr, batchHasSlow bool
	for _, r := range results {
		spansToInsert = append(spansToInsert, r.spans...)
		tracesToUpsert = append(tracesToUpsert, r.traces...)
		synthesizedLogs = append(synthesizedLogs, r.logs...)
		if r.hasErr {
			batchHasErr = true
		}
		if r.hasSlow {
			batchHasSlow = true
		}
	}

	// Intake metrics fire before the persist decision so operators see
	// what was received regardless of async drops/rejections. Net
	// persisted = ingestion_total - ingest_pipeline_dropped_total.
	if s.metrics != nil && len(spansToInsert) > 0 {
		s.metrics.GRPCBatchSize.Observe(float64(len(spansToInsert)))
		s.metrics.RecordIngestion(len(spansToInsert))
	}

	// Async path: hand off to the pipeline. ErrQueueFull is the only
	// signal we need to surface to the OTLP client — translates to
	// gRPC RESOURCE_EXHAUSTED so the client backs off rather than
	// retrying tighter. Soft backpressure drops are silent.
	if s.pipeline != nil {
		batch := &Batch{
			Type:         SignalTraces,
			Traces:       tracesToUpsert,
			Spans:        spansToInsert,
			Logs:         synthesizedLogs,
			HasError:     batchHasErr,
			HasSlow:      batchHasSlow,
			SpanCallback: s.spanCallback,
			LogCallback:  s.logCallback,
		}
		if err := s.pipeline.Submit(batch); err != nil {
			if errors.Is(err, ErrQueueFull) {
				return nil, grpcstatus.Errorf(codes.ResourceExhausted, "ingest pipeline at capacity")
			}
			return nil, err
		}
		return &coltracepb.ExportTraceServiceResponse{}, nil
	}

	// Synchronous fallback (s.pipeline == nil). Preserves the original
	// behavior bit-for-bit — no async-related side effects when the
	// operator opts out via INGEST_ASYNC_ENABLED=false.

	// Persist - CRITICAL ORDER: Traces MUST be inserted before Spans due to FK
	if len(tracesToUpsert) > 0 {
		if err := s.repo.BatchCreateTraces(tracesToUpsert); err != nil {
			slog.Error("❌ Failed to insert traces", "error", err)
			// Continue anyway to allow spans to be inserted if traces exist from previous runs
		}
	}

	if len(spansToInsert) > 0 {
		if err := s.repo.BatchCreateSpans(spansToInsert); err != nil {
			slog.Error("❌ Failed to insert spans", "error", err)
			return nil, err
		}
		// Notify GraphRAG of persisted spans
		if s.spanCallback != nil {
			for _, span := range spansToInsert {
				s.spanCallback(span)
			}
		}
	}

	if len(synthesizedLogs) > 0 {
		if err := s.repo.BatchCreateLogs(synthesizedLogs); err != nil {
			slog.Error("❌ Failed to insert synthesized logs", "error", err)
			// Continue, don't fail the whole trace request
		}

		if s.logCallback != nil {
			for _, l := range synthesizedLogs {
				s.logCallback(l)
			}
		}
	}

	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// Export handles incoming OTLP log data.
func (s *LogsServer) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	// slog.Debug("📥 [LOGS] Received Request", "resource_logs", len(req.ResourceLogs))

	logResults := make([][]storage.Log, len(req.ResourceLogs))

	g, _ := errgroup.WithContext(ctx)

	for idx, resourceLogs := range req.ResourceLogs {
		g.Go(func() error {
			serviceName := getServiceName(resourceLogs.Resource.Attributes)

			if !shouldIngestService(serviceName, s.allowedServices, s.excludedServices) {
				slog.Debug("🚫 [LOGS] Dropped service", "service", serviceName)
				return nil
			}

			tenantID := resolveTenant(ctx, resourceLogs.Resource.Attributes, s.defaultTenant, s.trustResourceTenant)

			localLogs := make([]storage.Log, 0)

			for _, scopeLogs := range resourceLogs.ScopeLogs {
				for _, l := range scopeLogs.LogRecords {
					severity := l.SeverityText
					if severity == "" {
						severity = l.SeverityNumber.String()
					}

					if !shouldIngestSeverity(severity, s.minSeverity) {
						continue
					}

					timestamp := time.Unix(0, int64(l.TimeUnixNano)) // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
					if timestamp.Unix() == 0 {
						timestamp = time.Now()
					}

					bodyStr := l.Body.GetStringValue()
					attrs, _ := json.Marshal(l.Attributes)

					logEntry := storage.Log{
						TenantID:       tenantID,
						TraceID:        fmt.Sprintf("%x", l.TraceId),
						SpanID:         fmt.Sprintf("%x", l.SpanId),
						Severity:       severity,
						Body:           bodyStr,
						ServiceName:    serviceName,
						AttributesJSON: storage.CompressedText(attrs),
						Timestamp:      timestamp,
					}
					localLogs = append(localLogs, logEntry)
				}
			}

			logResults[idx] = localLogs

			return nil
		})
	}

	_ = g.Wait()

	// Merge results after all goroutines complete (no lock contention)
	var logsToInsert []storage.Log
	for _, lr := range logResults {
		logsToInsert = append(logsToInsert, lr...)
	}

	if len(logsToInsert) == 0 {
		return &collogspb.ExportLogsServiceResponse{}, nil
	}

	// Intake metric fires before the persist decision (see TraceServer.Export
	// rationale). Net persisted = ingestion_total - ingest_pipeline_dropped_total.
	if s.metrics != nil {
		s.metrics.RecordIngestion(len(logsToInsert))
	}

	// Detect priority logs — ERROR/FATAL must bypass soft backpressure.
	var hasErr bool
	for _, l := range logsToInsert {
		if l.Severity == "ERROR" || l.Severity == "FATAL" {
			hasErr = true
			break
		}
	}

	// Async path: hand off to the pipeline.
	if s.pipeline != nil {
		batch := &Batch{
			Type:        SignalLogs,
			Logs:        logsToInsert,
			HasError:    hasErr,
			LogCallback: s.logCallback,
		}
		if err := s.pipeline.Submit(batch); err != nil {
			if errors.Is(err, ErrQueueFull) {
				return nil, grpcstatus.Errorf(codes.ResourceExhausted, "ingest pipeline at capacity")
			}
			return nil, err
		}
		return &collogspb.ExportLogsServiceResponse{}, nil
	}

	// Synchronous fallback (preserves original behavior when async is disabled).
	if err := s.repo.BatchCreateLogs(logsToInsert); err != nil {
		slog.Error("❌ Failed to insert logs", "error", err)
		return nil, err
	}
	if s.logCallback != nil {
		for _, l := range logsToInsert {
			s.logCallback(l)
		}
	}

	return &collogspb.ExportLogsServiceResponse{}, nil
}

// Helper to extract service.name from attributes
func getServiceName(attrs []*commonpb.KeyValue) string {
	for _, kv := range attrs {
		if kv.Key == "service.name" {
			return kv.Value.GetStringValue()
		}
	}
	return "unknown-service"
}

// Filtering Helpers
func parseSeverity(level string) int {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return 10
	case "INFO":
		return 20
	case "WARN", "WARNING":
		return 30
	case "ERROR":
		return 40
	case "FATAL":
		return 50
	default:
		return 20 // Default INFO
	}
}

func parseServiceList(list string) map[string]bool {
	m := make(map[string]bool)
	if list == "" {
		return m
	}
	parts := strings.Split(list, ",")
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			m[trimmed] = true
		}
	}
	return m
}

func shouldIngestSeverity(level string, minLevel int) bool {
	// Map OTLP/Text severity to int
	// If it's a number string "1", "9", etc., convert.
	// OTLP: TRACE=1, DEBUG=5, INFO=9, WARN=13, ERROR=17, FATAL=21
	// Simple mapping for text:

	lvl := 0
	upper := strings.ToUpper(level)

	switch {
	case strings.Contains(upper, "DEBUG"):
		lvl = 10
	case strings.Contains(upper, "INFO"):
		lvl = 20
	case strings.Contains(upper, "WARN"):
		lvl = 30
	case strings.Contains(upper, "ERR"):
		lvl = 40
	case strings.Contains(upper, "FATAL"):
		lvl = 50
	default:
		// Fallback for strict numeric strings or unknown
		// If "SEVERITY_NUMBER_INFO" etc.
		switch {
		case strings.Contains(upper, "WARN"):
			lvl = 30
		case strings.Contains(upper, "ERR"):
			lvl = 40
		default:
			lvl = 20 // Default treat as info (includes "INFO" and unknown)
		}
	}

	return lvl >= minLevel
}

func shouldIngestService(service string, allowed map[string]bool, excluded map[string]bool) bool {
	if len(excluded) > 0 {
		if excluded[service] {
			return false
		}
	}

	if len(allowed) > 0 {
		if !allowed[service] {
			return false
		}
	}

	return true
}
