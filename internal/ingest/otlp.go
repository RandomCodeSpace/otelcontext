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

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
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
		if vals := md.Get(tenantHeader); len(vals) > 0 {
			// Reject empty, over-length, or control-char values via shared
			// sanitizer so HTTP and gRPC paths apply identical input-safety
			// rules. Empty return falls through to the configured default.
			if t := storage.SanitizeTenantID(vals[0]); t != "" {
				return t
			}
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
// see resolveTenant. The value is run through SanitizeTenantID so a
// compromised SDK cannot smuggle control characters or oversized strings
// even on the trusted-resource path.
func tenantFromResource(attrs []*commonpb.KeyValue) string {
	for _, kv := range attrs {
		if kv.Key == "tenant.id" {
			return storage.SanitizeTenantID(kv.Value.GetStringValue())
		}
	}
	return ""
}

type TraceServer struct {
	repo         *storage.Repository
	metrics      *telemetry.Metrics
	logCallback  func(storage.Log)
	spanCallback func(storage.Span) // called for each span after persistence
	// topologyObserver, when set, is invoked for EVERY received span BEFORE the
	// sampler's keep/drop decision so cross-service call topology survives
	// sampling (see graphrag.GraphRAG.ObserveSpanTopology). Args:
	// tenant, traceID, spanID, parentSpanID, service. nil = disabled.
	topologyObserver func(tenant, traceID, spanID, parentSpanID, service string)
	// aggregateEngine, when set (AGGREGATE_MODE != legacy), runs request-local
	// aggregate reduction BEFORE the sampler so aggregate counts describe
	// accepted telemetry rather than the sampling rate (#153 §8). nil leaves
	// the legacy path untouched.
	aggregateEngine     *aggregate.Engine
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
	repo        *storage.Repository
	metrics     *telemetry.Metrics
	logCallback func(storage.Log)
	// aggregateEngine — see TraceServer.aggregateEngine. Reduction runs before
	// the severity gate.
	aggregateEngine     *aggregate.Engine
	minSeverity         int
	allowedServices     map[string]bool
	excludedServices    map[string]bool
	pipeline            *Pipeline // nil = synchronous DB writes (legacy path)
	defaultTenant       string
	trustResourceTenant bool
	collogspb.UnimplementedLogsServiceServer
}

type MetricsServer struct {
	repo           *storage.Repository
	metrics        *telemetry.Metrics
	aggregator     *tsdb.Aggregator
	metricCallback func(tsdb.RawMetric)
	// aggregateEngine — see TraceServer.aggregateEngine.
	aggregateEngine     *aggregate.Engine
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

// SetTopologyObserver wires a pre-sample hook invoked for every received span
// before the sampler runs, so the in-memory service map keeps cross-service
// flow direction even when sampling drops the spans that would have formed the
// edge. Pass nil to disable. Wired in main.go to graphrag.ObserveSpanTopology.
func (s *TraceServer) SetTopologyObserver(cb func(tenant, traceID, spanID, parentSpanID, service string)) {
	s.topologyObserver = cb
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

// SetAggregateEngine enables aggregate accounting on this server. The reducer
// runs inside Export() ahead of the sampler and severity gates; passing nil
// (the AGGREGATE_MODE=legacy case) leaves the export path unchanged.
func (s *TraceServer) SetAggregateEngine(e *aggregate.Engine) {
	s.aggregateEngine = e
}

// SetAggregateEngine — see TraceServer.SetAggregateEngine.
func (s *LogsServer) SetAggregateEngine(e *aggregate.Engine) {
	s.aggregateEngine = e
}

// SetAggregateEngine — see TraceServer.SetAggregateEngine.
func (s *MetricsServer) SetAggregateEngine(e *aggregate.Engine) {
	s.aggregateEngine = e
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
	start := time.Now()
	defer func() { s.metrics.ObserveIngestDuration("metrics", time.Since(start)) }()

	// Aggregate accounting. One reducer, one arrival time for the whole
	// request (#160): lateness must not depend on a point's position in the
	// batch. nil engine = AGGREGATE_MODE=legacy = nothing below runs.
	var reducer *aggregate.Reducer
	if s.aggregateEngine != nil {
		reducer = s.aggregateEngine.NewReducer(start)
	}

	for _, resourceMetrics := range req.ResourceMetrics {
		serviceName := getServiceName(resourceMetrics.Resource.Attributes)

		if !shouldIngestService(serviceName, s.allowedServices, s.excludedServices) {
			continue
		}

		tenantID := resolveTenant(ctx, resourceMetrics.Resource.Attributes, s.defaultTenant, s.trustResourceTenant)

		var producerIdentity aggregate.ResourceIdentity
		if reducer != nil {
			producerIdentity = aggregateResourceIdentity(resourceMetrics.Resource.Attributes)
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
						Timestamp:   time.Unix(0, int64(p.TimeUnixNano)), // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
						Attributes:  make(map[string]any),
						TenantID:    tenantID,
					}

					// Convert attributes to map for TSDB grouping
					for _, kv := range p.Attributes {
						raw.Attributes[kv.Key] = kv.Value.String()
					}

					// 0. Aggregate accounting, ahead of every other consumer.
					if reducer != nil {
						temporality, monotonic := aggregateTemporality(m)
						reducer.ReduceMetricPoint(aggregate.MetricInput{
							Tenant:      tenantID,
							Service:     serviceName,
							Name:        m.Name,
							Value:       val,
							Timestamp:   raw.Timestamp,
							StartTime:   time.Unix(0, int64(p.StartTimeUnixNano)), // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
							Temporality: temporality,
							Monotonic:   monotonic,
							Resource:    producerIdentity,
						})
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

	// Apply the request's aggregate deltas. Under durable ACK this blocks
	// until the group commit lands, and a refusal is the client's answer.
	if err := applyAggregate(s.aggregateEngine, reducer); err != nil {
		return nil, err
	}

	if s.metrics != nil {
		// Just a marker for Prometheus that metrics were received
		s.metrics.RecordIngestion(1)
	}

	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

// Export handles incoming OTLP trace data.
func (s *TraceServer) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	start := time.Now()
	defer func() { s.metrics.ObserveIngestDuration("traces", time.Since(start)) }()
	slog.Debug("📥 [TRACES] Received Request", "resource_spans", len(req.ResourceSpans))

	type batchResult struct {
		spans   []storage.Span
		traces  []storage.Trace
		logs    []storage.Log
		hasErr  bool // any span in this slice had STATUS_CODE_ERROR
		hasSlow bool // any span exceeded latencyThresholdMs
		// reducer holds this resource batch's aggregate deltas. Reduction is
		// request-local and lock-free, so each goroutine owns its own reducer
		// and they are merged once below.
		reducer *aggregate.Reducer
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
			// traceIdx maps trace ID -> index in localTraces so this batch emits
			// exactly one Trace row per trace instead of one per span.
			traceIdx := make(map[string]int)
			var localHasErr, localHasSlow bool

			// Aggregate accounting. One arrival time for the whole Export
			// request (#160), captured above as `start`.
			var reducer *aggregate.Reducer
			if s.aggregateEngine != nil {
				reducer = s.aggregateEngine.NewReducer(start)
			}

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

					// Hex-encode the IDs exactly once per span. These are needed
					// by the topology observer, the span/trace rows and every
					// synthesized log below; formatting them per use turned the
					// hot path into an allocation mill.
					traceIDHex := fmt.Sprintf("%x", span.TraceId)
					spanIDHex := fmt.Sprintf("%x", span.SpanId)
					parentSpanIDHex := fmt.Sprintf("%x", span.ParentSpanId)

					// Observe cross-service call topology for EVERY span BEFORE
					// the sampler can drop it. The sampled path below still owns
					// edge aggregates; this only guarantees the edge exists so the
					// service map keeps flow direction at low sample rates. Cheap
					// and strictly bounded (per-tenant LRU + per-pair dedup).
					if s.topologyObserver != nil {
						s.topologyObserver(
							tenantID,
							traceIDHex,
							spanIDHex,
							parentSpanIDHex,
							serviceName,
						)
					}

					// Aggregate reduction runs BEFORE the sampler. Aggregate
					// counts must describe accepted telemetry, not the
					// sampling rate (#153 §8) — that invariant is the entire
					// reason the engine sits at this point in the path.
					if reducer != nil {
						reducer.ReduceSpan(aggregateSpanInput(tenantID, serviceName, span, startTime, endTime))
					}

					if s.sampler != nil {
						isError := statusStr == storage.StatusCodeError
						durationMs := float64(duration) / 1000.0
						if !s.sampler.ShouldSample(serviceName, isError, durationMs) {
							continue
						}
					}

					attrs, _ := json.Marshal(span.Attributes)

					// Create Span Model
					sModel := storage.Span{
						TenantID:       tenantID,
						TraceID:        traceIDHex,
						SpanID:         spanIDHex,
						ParentSpanID:   parentSpanIDHex,
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
					if statusStr == storage.StatusCodeError {
						localHasErr = true
					}
					if s.latencyThresholdMs > 0 && float64(duration)/1000.0 >= s.latencyThresholdMs {
						localHasSlow = true
					}

					// One Trace row per trace ID per resource-spans batch, not per
					// span. The first span of a trace seeds timestamp/duration/
					// service; later spans of the same trace can only UPGRADE the
					// status to ERROR, never downgrade it (the DB upsert applies
					// the same rule across batches).
					if i, seen := traceIdx[traceIDHex]; seen {
						if statusStr == storage.StatusCodeError {
							localTraces[i].Status = storage.StatusCodeError
						}
					} else {
						traceIdx[traceIDHex] = len(localTraces)
						localTraces = append(localTraces, storage.Trace{
							TenantID:    tenantID,
							TraceID:     traceIDHex,
							ServiceName: serviceName,
							Timestamp:   startTime,
							Duration:    duration,
							Status:      statusStr,
						})
					}

					// spanHasErrorLog records whether an ERROR log was already
					// synthesized for THIS span from its events. It replaces a
					// rescan of every previously synthesized log per span, which
					// made the loop O(n^2) in spans-per-resource.
					spanHasErrorLog := false

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
							TraceID:        traceIDHex,
							SpanID:         spanIDHex,
							Severity:       severity,
							Body:           body,
							ServiceName:    serviceName,
							AttributesJSON: storage.CompressedText(eventAttrs),
							Timestamp:      time.Unix(0, int64(event.TimeUnixNano)), // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
						}
						localLogs = append(localLogs, l)
						if severity == "ERROR" {
							spanHasErrorLog = true
						}
					}

					if !spanHasErrorLog && span.Status != nil && span.Status.Code == tracepb.Status_STATUS_CODE_ERROR {
						if shouldIngestSeverity("ERROR", s.minSeverity) {
							msg := span.Status.Message
							if msg == "" {
								msg = fmt.Sprintf("Span '%s' failed", span.Name)
							}

							l := storage.Log{
								TenantID:       tenantID,
								TraceID:        traceIDHex,
								SpanID:         spanIDHex,
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
				reducer: reducer,
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
	var merged *aggregate.Reducer
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
		if r.reducer == nil {
			continue
		}
		if merged == nil {
			merged = r.reducer
			continue
		}
		merged.MergeFrom(r.reducer)
	}

	// Apply the request's aggregate deltas before the persist decision: the
	// aggregate path has already accepted this telemetry, and a downstream
	// queue rejection must not retroactively unaccount it. Under durable ACK
	// this blocks until the group commit lands.
	if err := applyAggregate(s.aggregateEngine, merged); err != nil {
		return nil, err
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
	start := time.Now()
	defer func() { s.metrics.ObserveIngestDuration("logs", time.Since(start)) }()
	// slog.Debug("📥 [LOGS] Received Request", "resource_logs", len(req.ResourceLogs))

	logResults := make([][]storage.Log, len(req.ResourceLogs))
	// One reducer per resource batch (reduction is request-local and not
	// concurrency-safe); merged into one after the group finishes.
	reducers := make([]*aggregate.Reducer, len(req.ResourceLogs))

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

			var reducer *aggregate.Reducer
			if s.aggregateEngine != nil {
				reducer = s.aggregateEngine.NewReducer(start)
				reducers[idx] = reducer
			}

			for _, scopeLogs := range resourceLogs.ScopeLogs {
				for _, l := range scopeLogs.LogRecords {
					severity := l.SeverityText
					if severity == "" {
						severity = l.SeverityNumber.String()
					}

					timestamp := time.Unix(0, int64(l.TimeUnixNano)) // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
					if timestamp.Unix() == 0 {
						timestamp = time.Now()
					}

					// Aggregate reduction runs BEFORE the severity gate: a
					// DEBUG log that never reaches the DB is still accepted
					// telemetry and must be accounted (#153 §8).
					if reducer != nil {
						reducer.ReduceLog(aggregate.LogInput{
							Tenant:         tenantID,
							Service:        serviceName,
							Severity:       severity,
							SeverityNumber: int32(l.SeverityNumber),
							Body:           l.Body.GetStringValue(),
							Timestamp:      timestamp,
						})
					}

					if !shouldIngestSeverity(severity, s.minSeverity) {
						continue
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

	// Apply the request's aggregate deltas. This happens before the early
	// return below: a request whose every record was filtered out still
	// carries aggregate accounting.
	var mergedReducer *aggregate.Reducer
	for _, r := range reducers {
		if r == nil {
			continue
		}
		if mergedReducer == nil {
			mergedReducer = r
			continue
		}
		mergedReducer.MergeFrom(r)
	}
	if err := applyAggregate(s.aggregateEngine, mergedReducer); err != nil {
		return nil, err
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

// ParseSeverity is the exported wrapper for parseSeverity. Used by main.go
// to translate the STORE_MIN_SEVERITY env value into the integer rank the
// pipeline's second-tier filter expects.
func ParseSeverity(level string) int { return parseSeverity(level) }

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
