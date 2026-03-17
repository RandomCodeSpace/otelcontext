package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"runtime"
	"sync"

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
)

type TraceServer struct {
	repo             *storage.Repository
	metrics          *telemetry.Metrics
	logCallback      func(storage.Log)
	minSeverity      int
	allowedServices  map[string]bool
	excludedServices map[string]bool
	sampler          *Sampler // nil = no sampling (keep all)
	coltracepb.UnimplementedTraceServiceServer
}

type LogsServer struct {
	repo             *storage.Repository
	metrics          *telemetry.Metrics
	logCallback      func(storage.Log)
	minSeverity      int
	allowedServices  map[string]bool
	excludedServices map[string]bool
	collogspb.UnimplementedLogsServiceServer
}

type MetricsServer struct {
	repo             *storage.Repository
	metrics          *telemetry.Metrics
	aggregator       *tsdb.Aggregator
	metricCallback   func(tsdb.RawMetric)
	allowedServices  map[string]bool
	excludedServices map[string]bool
	colmetricspb.UnimplementedMetricsServiceServer
}

func NewTraceServer(repo *storage.Repository, metrics *telemetry.Metrics, cfg *config.Config) *TraceServer {
	return &TraceServer{
		repo:             repo,
		metrics:          metrics,
		minSeverity:      parseSeverity(cfg.IngestMinSeverity),
		allowedServices:  parseServiceList(cfg.IngestAllowedServices),
		excludedServices: parseServiceList(cfg.IngestExcludedServices),
	}
}

// SetLogCallback sets the function to call when a new log is synthesized from a trace.
func (s *TraceServer) SetLogCallback(cb func(storage.Log)) {
	s.logCallback = cb
}

// SetSampler enables adaptive trace sampling. Pass nil to disable.
func (s *TraceServer) SetSampler(sm *Sampler) {
	s.sampler = sm
}

func NewLogsServer(repo *storage.Repository, metrics *telemetry.Metrics, cfg *config.Config) *LogsServer {
	return &LogsServer{
		repo:             repo,
		metrics:          metrics,
		minSeverity:      parseSeverity(cfg.IngestMinSeverity),
		allowedServices:  parseServiceList(cfg.IngestAllowedServices),
		excludedServices: parseServiceList(cfg.IngestExcludedServices),
	}
}

// SetLogCallback sets the function to call when a new log is received.
func (s *LogsServer) SetLogCallback(cb func(storage.Log)) {
	s.logCallback = cb
}

func NewMetricsServer(repo *storage.Repository, metrics *telemetry.Metrics, aggregator *tsdb.Aggregator, cfg *config.Config) *MetricsServer {
	return &MetricsServer{
		repo:             repo,
		metrics:          metrics,
		aggregator:       aggregator,
		allowedServices:  parseServiceList(cfg.IngestAllowedServices),
		excludedServices: parseServiceList(cfg.IngestExcludedServices),
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
						Timestamp:   time.Unix(0, int64(p.TimeUnixNano)),
						Attributes:  make(map[string]interface{}),
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

	var (
		spansMu         sync.Mutex
		spansToInsert   []storage.Span
		tracesToUpsert  []storage.Trace
		synthesizedLogs []storage.Log
	)

	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(runtime.GOMAXPROCS(0) * 4)

	for _, resourceSpans := range req.ResourceSpans {
		resourceSpans := resourceSpans // Capture
		g.Go(func() error {
			serviceName := getServiceName(resourceSpans.Resource.Attributes)

			if !shouldIngestService(serviceName, s.allowedServices, s.excludedServices) {
				slog.Debug("🚫 [TRACES] Dropped service", "service", serviceName)
				return nil
			}

			localSpans := make([]storage.Span, 0)
			localTraces := make([]storage.Trace, 0)
			localLogs := make([]storage.Log, 0)

			for _, scopeSpans := range resourceSpans.ScopeSpans {
				for _, span := range scopeSpans.Spans {
					startTime := time.Unix(0, int64(span.StartTimeUnixNano))
					endTime := time.Unix(0, int64(span.EndTimeUnixNano))
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
						TraceID:        fmt.Sprintf("%x", span.TraceId),
						SpanID:         fmt.Sprintf("%x", span.SpanId),
						ParentSpanID:   fmt.Sprintf("%x", span.ParentSpanId),
						OperationName:  span.Name,
						StartTime:      startTime,
						EndTime:        endTime,
						Duration:       duration,
						ServiceName:    serviceName,
						AttributesJSON: storage.CompressedText(attrs),
					}
					localSpans = append(localSpans, sModel)

					tModel := storage.Trace{
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
							TraceID:        fmt.Sprintf("%x", span.TraceId),
							SpanID:         fmt.Sprintf("%x", span.SpanId),
							Severity:       severity,
							Body:           storage.CompressedText(body),
							ServiceName:    serviceName,
							AttributesJSON: storage.CompressedText(eventAttrs),
							Timestamp:      time.Unix(0, int64(event.TimeUnixNano)),
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
								TraceID:        fmt.Sprintf("%x", span.TraceId),
								SpanID:         fmt.Sprintf("%x", span.SpanId),
								Severity:       "ERROR",
								Body:           storage.CompressedText(msg),
								ServiceName:    serviceName,
								AttributesJSON: "{}",
								Timestamp:      endTime,
							}
							localLogs = append(localLogs, l)
						}
					}
				}
			}

			// Fan-In: Collect local results into shared buffers
			spansMu.Lock()
			spansToInsert = append(spansToInsert, localSpans...)
			tracesToUpsert = append(tracesToUpsert, localTraces...)
			synthesizedLogs = append(synthesizedLogs, localLogs...)
			spansMu.Unlock()

			return nil
		})
	}

	g.Wait()

	// Persist - CRITICAL ORDER: Traces MUST be inserted before Spans due to FK
	if len(tracesToUpsert) > 0 {
		if err := s.repo.BatchCreateTraces(tracesToUpsert); err != nil {
			slog.Error("❌ Failed to insert traces", "error", err)
			// Continue anyway to allow spans to be inserted if traces exist from previous runs
		} else {
			// slog.Debug("✅ Successfully persisted trace records", "count", len(tracesToUpsert))
		}
	}

	if len(spansToInsert) > 0 {
		if s.metrics != nil {
			s.metrics.GRPCBatchSize.Observe(float64(len(spansToInsert)))
		}
		if err := s.repo.BatchCreateSpans(spansToInsert); err != nil {
			slog.Error("❌ Failed to insert spans", "error", err)
			return nil, err
		}
		if s.metrics != nil {
			s.metrics.RecordIngestion(len(spansToInsert))
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

	var (
		mu           sync.Mutex
		logsToInsert []storage.Log
	)

	g, _ := errgroup.WithContext(ctx)

	for _, resourceLogs := range req.ResourceLogs {
		resourceLogs := resourceLogs // Capture
		g.Go(func() error {
			serviceName := getServiceName(resourceLogs.Resource.Attributes)

			if !shouldIngestService(serviceName, s.allowedServices, s.excludedServices) {
				slog.Debug("🚫 [LOGS] Dropped service", "service", serviceName)
				return nil
			}

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

					timestamp := time.Unix(0, int64(l.TimeUnixNano))
					if timestamp.Unix() == 0 {
						timestamp = time.Now()
					}

					bodyStr := l.Body.GetStringValue()
					attrs, _ := json.Marshal(l.Attributes)

					logEntry := storage.Log{
						TraceID:        fmt.Sprintf("%x", l.TraceId),
						SpanID:         fmt.Sprintf("%x", l.SpanId),
						Severity:       severity,
						Body:           storage.CompressedText(bodyStr),
						ServiceName:    serviceName,
						AttributesJSON: storage.CompressedText(attrs),
						Timestamp:      timestamp,
					}
					localLogs = append(localLogs, logEntry)
				}
			}

			mu.Lock()
			logsToInsert = append(logsToInsert, localLogs...)
			mu.Unlock()

			return nil
		})
	}

	g.Wait()

	if len(logsToInsert) > 0 {
		if err := s.repo.BatchCreateLogs(logsToInsert); err != nil {
			slog.Error("❌ Failed to insert logs", "error", err)
			return nil, err
		}
		if s.metrics != nil {
			s.metrics.RecordIngestion(len(logsToInsert))
		}

		// Notify listener
		if s.logCallback != nil {
			for _, l := range logsToInsert {
				s.logCallback(l)
			}
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
		if strings.Contains(upper, "INFO") {
			lvl = 20
		} else if strings.Contains(upper, "WARN") {
			lvl = 30
		} else if strings.Contains(upper, "ERR") {
			lvl = 40
		} else {
			lvl = 20
		} // Default treat as info
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
