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
	"github.com/RandomCodeSpace/otelcontext/internal/authn"
	"github.com/RandomCodeSpace/otelcontext/internal/config"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
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
	// An authenticated tenant key binds absolutely. It outranks metadata, the
	// resource attribute, and the configured default; a disagreeing
	// `tenant.id` is counted so operators can see clients asserting a tenancy
	// they do not hold.
	if bound, ok := authn.BoundTenantFromContext(ctx); ok {
		if t := tenantFromResource(resourceAttrs); t != "" && t != bound {
			authn.RecordConflict("grpc", "resource_attribute")
		}
		return bound
	}
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
	aggregateEngine *aggregate.Engine
	// exemplar, when set (AGGREGATE_MODE=aggregate), is the ONLY raw-retention
	// gate: the adaptive Sampler is retired in that mode (#161). nil in
	// legacy/shadow, where the Sampler keeps governing the raw path unchanged.
	exemplar *ExemplarPolicy
	// resourceRegistry, when set, records every resource batch's
	// (tenant, service, host, workload) BEFORE the sampler so host identity
	// is independent of SAMPLING_RATE (#279). nil = disabled.
	resourceRegistry    *topology.Registry
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
	aggregateEngine *aggregate.Engine
	// exemplar — see TraceServer.exemplar. In aggregate mode INFO/DEBUG logs
	// are aggregate-only and ERROR/FATAL/WARN are budgeted per service/window.
	exemplar *ExemplarPolicy
	// resourceRegistry — see TraceServer.resourceRegistry.
	resourceRegistry    *topology.Registry
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
	aggregateEngine *aggregate.Engine
	// resourceRegistry — see TraceServer.resourceRegistry.
	resourceRegistry    *topology.Registry
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

// SetExemplarPolicy installs the bounded exemplar retention policy (#176).
// Wired only for AGGREGATE_MODE=aggregate, where it replaces the adaptive
// Sampler as the sole raw-retention gate. Passing nil (legacy / shadow) leaves
// the Sampler in charge and every byte of the export path unchanged.
func (s *TraceServer) SetExemplarPolicy(p *ExemplarPolicy) {
	s.exemplar = p
}

// SetExemplarPolicy — see TraceServer.SetExemplarPolicy.
func (s *LogsServer) SetExemplarPolicy(p *ExemplarPolicy) {
	s.exemplar = p
}

// SetResourceRegistry wires the bounded resource registry (#279). Every
// resource batch registers its host identity ahead of the sampler; passing
// nil disables registration.
func (s *TraceServer) SetResourceRegistry(r *topology.Registry) {
	s.resourceRegistry = r
}

// SetResourceRegistry — see TraceServer.SetResourceRegistry.
func (s *LogsServer) SetResourceRegistry(r *topology.Registry) {
	s.resourceRegistry = r
}

// SetResourceRegistry — see TraceServer.SetResourceRegistry.
func (s *MetricsServer) SetResourceRegistry(r *topology.Registry) {
	s.resourceRegistry = r
}

// registerResource records one resource batch in the registry. now is the
// Export's arrival time; a nil registry is a no-op.
func registerResource(reg *topology.Registry, tenant, service string, attrs []*commonpb.KeyValue, signal topology.Signal, now time.Time) {
	if reg == nil {
		return
	}
	slots := scanResourceSlots(attrs)
	reg.Register(tenant, service, slots.host, slots.workload, slots.workloadKind, signal, now)
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
	// rejected accumulates the points aggregate accounting refused, for the
	// OTLP partial-success response (#199 Q5).
	var rejected metricRejections

	for _, resourceMetrics := range req.ResourceMetrics {
		serviceName := getServiceName(resourceMetrics.Resource.Attributes)

		if !shouldIngestService(serviceName, s.allowedServices, s.excludedServices) {
			continue
		}

		tenantID := resolveTenant(ctx, resourceMetrics.Resource.Attributes, s.defaultTenant, s.trustResourceTenant)
		registerResource(s.resourceRegistry, tenantID, serviceName, resourceMetrics.Resource.Attributes, topology.SignalMetrics, start)

		var producerIdentity aggregate.ResourceIdentity
		if reducer != nil {
			producerIdentity = aggregateResourceIdentity(resourceMetrics.Resource.Attributes)
		}

		for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
			for _, m := range scopeMetrics.Metrics {
				switch data := m.Data.(type) {
				case *metricspb.Metric_Gauge:
					s.exportNumberPoints(reducer, m, data.Gauge.GetDataPoints(), serviceName, tenantID, producerIdentity)
				case *metricspb.Metric_Sum:
					s.exportNumberPoints(reducer, m, data.Sum.GetDataPoints(), serviceName, tenantID, producerIdentity)
				case *metricspb.Metric_Histogram:
					// Distribution points have no legacy consumer: the TSDB
					// ring buffer holds scalars only. They exist for aggregate
					// accounting, so nothing runs in legacy mode and nothing
					// is reported rejected there either — the aggregate
					// contract is what promises to account for them (#199).
					if reducer == nil {
						continue
					}
					temporality := otlpTemporality(data.Histogram.GetAggregationTemporality())
					for _, p := range data.Histogram.GetDataPoints() {
						if p == nil || p.Flags&otlpDataPointNoRecordedValue != 0 {
							continue
						}
						res := reducer.ReduceHistogramPoint(aggregateHistogramInput(
							tenantID, serviceName, m.Name, producerIdentity, temporality, p))
						rejected.record(s.metrics, pointTypeHistogram, res)
					}
				case *metricspb.Metric_ExponentialHistogram:
					if reducer == nil {
						continue
					}
					temporality := otlpTemporality(data.ExponentialHistogram.GetAggregationTemporality())
					for _, p := range data.ExponentialHistogram.GetDataPoints() {
						if p == nil || p.Flags&otlpDataPointNoRecordedValue != 0 {
							continue
						}
						res := reducer.ReduceExponentialHistogramPoint(aggregateExpHistogramInput(
							tenantID, serviceName, m.Name, producerIdentity, temporality, p))
						rejected.record(s.metrics, pointTypeExpHistogram, res)
					}
				case *metricspb.Metric_Summary:
					// Summary is wholly unsupported (#199 Q5). It carries
					// producer-chosen quantiles that cannot be merged across
					// series or windows, so there is no honest way to fold it
					// into a sketch. Every point is counted and reported.
					if reducer == nil {
						continue
					}
					n := uint64(len(data.Summary.GetDataPoints()))
					rejected.add(pointTypeSummary, aggregate.ReasonUnsupportedType, n)
					s.metrics.RecordMetricUnsupported(pointTypeSummary, aggregate.ReasonUnsupportedType, len(data.Summary.GetDataPoints()))
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

	resp := &colmetricspb.ExportMetricsServiceResponse{}
	if rejected.total > 0 {
		// Export SUCCEEDS: every accepted point is committed. The rejected
		// count is exact and the client must not retry it — a retry replays
		// the same unsupported points to the same refusal. A zero here is
		// reserved for warning-only responses where everything was accepted,
		// so it is never written on this branch (#199 Q5).
		resp.PartialSuccess = &colmetricspb.ExportMetricsPartialSuccess{
			RejectedDataPoints: int64(rejected.total), // #nosec G115 -- bounded by the request's point count
			ErrorMessage:       rejected.message(),
		}
	}
	return resp, nil
}

// exportNumberPoints handles the Gauge and Sum data points of one metric: the
// aggregate reduction, then — only when a legacy consumer exists — the TSDB
// ring buffer and the real-time bypass. It is a method rather than a closure so
// the histogram branches above stay flat and the loop body is not duplicated
// per instrument type.
func (s *MetricsServer) exportNumberPoints(
	reducer *aggregate.Reducer,
	m *metricspb.Metric,
	points []*metricspb.NumberDataPoint,
	serviceName, tenantID string,
	producerIdentity aggregate.ResourceIdentity,
) {
	// In AGGREGATE_MODE=aggregate main.go constructs neither the TSDB
	// aggregator nor the metric callback, and this collapses to zero: no
	// RawMetric, no per-point attribute map, no channel hop (#194 finding 10).
	// The attribute map alone is an allocation per data point, and at 120
	// services it is the single largest allocator left on the metric path.
	legacy := s.aggregator != nil || s.metricCallback != nil

	for _, p := range points {
		if p == nil {
			continue
		}
		var val float64
		switch v := p.Value.(type) {
		case *metricspb.NumberDataPoint_AsDouble:
			val = v.AsDouble
		case *metricspb.NumberDataPoint_AsInt:
			val = float64(v.AsInt)
		}

		ts := time.Unix(0, int64(p.TimeUnixNano)) // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262

		// 0. Aggregate accounting, ahead of every other consumer.
		if reducer != nil {
			temporality, monotonic := aggregateTemporality(m)
			reducer.ReduceMetricPoint(aggregate.MetricInput{
				Tenant:      tenantID,
				Service:     serviceName,
				Name:        m.Name,
				Value:       val,
				Timestamp:   ts,
				StartTime:   time.Unix(0, int64(p.StartTimeUnixNano)), // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
				Temporality: temporality,
				Monotonic:   monotonic,
				Resource:    producerIdentity,
				Attributes:  p.Attributes,
			})
		}

		if !legacy {
			continue
		}

		raw := tsdb.RawMetric{
			Name:        m.Name,
			ServiceName: serviceName,
			Value:       val,
			Timestamp:   ts,
			Attributes:  make(map[string]any, len(p.Attributes)),
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

// Export handles incoming OTLP trace data.
func (s *TraceServer) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	start := time.Now()
	defer func() { s.metrics.ObserveIngestDuration("traces", time.Since(start)) }()
	slog.Debug("📥 [TRACES] Received Request", "resource_spans", len(req.ResourceSpans))

	type batchResult struct {
		spans   []storage.Span
		traces  []storage.Trace
		logs    []storage.Log
		tenant  string // tenant this resource resolved to (#194 finding 12)
		hasErr  bool   // any span in this slice had STATUS_CODE_ERROR
		hasSlow bool   // any span exceeded latencyThresholdMs
		// reducer holds this resource batch's aggregate deltas. Reduction is
		// request-local and lock-free, so each goroutine owns its own reducer
		// and they are merged once below.
		reducer *aggregate.Reducer
		// exemplarRes holds the exemplar bytes this resource batch RESERVED
		// (#201 Q4). Reservations are per-goroutine and merged by tenant
		// below, then committed when a destination accepts the batch or
		// released when nothing does.
		exemplarRes *ExemplarReservation
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
			// Pre-sample, like the topology observer: host identity must not
			// depend on SAMPLING_RATE (#279).
			registerResource(s.resourceRegistry, tenantID, serviceName, resourceSpans.Resource.Attributes, topology.SignalTraces, start)

			localSpans := make([]storage.Span, 0)
			localTraces := make([]storage.Trace, 0)
			localLogs := make([]storage.Log, 0)
			// traceIdx maps trace ID -> index in localTraces so this batch emits
			// exactly one Trace row per trace instead of one per span.
			traceIdx := make(map[string]int)
			var localHasErr, localHasSlow bool
			// One reservation per resource batch; nil when no exemplar policy
			// is wired (legacy/shadow), which every method tolerates.
			exemplarRes := s.exemplar.NewReservation()

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
						spanIn := aggregateSpanInput(tenantID, serviceName, span, startTime, endTime)
						reducer.ReduceSpan(spanIn)
						// Recover this call's caller from its parent span and
						// emit the caller->callee edge series. #183 shipped
						// SignalServiceEdge but emitted nothing into it; the
						// caller join was explicitly deferred to #174.
						if caller, ok := s.aggregateEngine.EdgeResolver().Observe(
							tenantID, spanIDHex, parentSpanIDHex, serviceName,
						); ok {
							reducer.ReduceEdge(aggregateEdgeInput(caller, spanIn))
						}
					}

					// Raw-retention gate. Exactly one policy governs this per
					// mode (#161): in aggregate mode the exemplar policy is it
					// and the Sampler is retired; in legacy/shadow the Sampler
					// is unchanged and s.exemplar is nil.
					isError := statusStr == storage.StatusCodeError
					durationMs := float64(duration) / 1000.0
					if s.exemplar != nil {
						if !s.exemplar.AdmitSpan(ExemplarSpan{
							Tenant:     tenantID,
							Service:    serviceName,
							TraceID:    traceIDHex,
							Operation:  span.Name,
							Status:     statusStr,
							DurationMs: durationMs,
							Timestamp:  startTime,
						}) {
							continue
						}
					} else if s.sampler != nil {
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
					// Byte metering happens here, on the row that is actually
					// handed to persistence, not on the OTLP wire message —
					// the byte budget is a budget on what gets written. The
					// bytes are RESERVED, not charged: they become a charge
					// when a destination accepts the batch and evaporate if
					// none does (#201 Q4). A refusal drops this span and marks
					// the trace truncated; the trace row below still lands so
					// the gap is recorded in the data rather than inferred.
					keepSpan := true
					if s.exemplar != nil {
						keepSpan = s.exemplar.ReserveSpan(exemplarRes, tenantID, serviceName, traceIDHex, startTime, spanRowBytes(&sModel))
					}
					if keepSpan {
						localSpans = append(localSpans, sModel)
					}

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

					// Synthesized logs ride along with their span. A span the
					// byte budget refused takes its synthesized logs with it —
					// persisting a log whose span was dropped would be exactly
					// the dangling evidence #163 forbids.
					if !keepSpan {
						continue
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
						// Cheap gate before the attribute marshal: an INFO span
						// event is aggregate-only in aggregate mode and must
						// not cost a JSON encode to find that out.
						if s.exemplar != nil && !s.exemplar.SynthesizedLogEligible(severity) {
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

						// Aggregate mode: INFO/DEBUG never persist raw (#161),
						// and every synthesized log that does is METERED
						// against its trace's per-trace budget and the shared
						// window budgets (#201 Q3). Riding a selected trace is
						// not the same as being free.
						if s.exemplar != nil && !s.exemplar.ReserveSynthesizedLog(
							exemplarRes, tenantID, serviceName, traceIDHex, spanIDHex, severity,
							time.Unix(0, int64(event.TimeUnixNano)), // #nosec G115 -- OTLP time in nanos: uint64 source fits int64 until year 2262
							len(body)+len(eventAttrs),
						) {
							continue
						}

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

							// Metered like the event-derived logs above: the
							// status fallback is one more row on the same
							// budget. "{}" is the attributes payload.
							if s.exemplar != nil && !s.exemplar.ReserveSynthesizedLog(
								exemplarRes, tenantID, serviceName, traceIDHex, spanIDHex, "ERROR",
								endTime, len(msg)+len("{}"),
							) {
								continue
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

			// Stamp the complete-retained-trace contract onto the trace rows
			// (#163). Only traces the exemplar policy actually cut short carry
			// a claim; everything else leaves the columns NULL.
			if s.exemplar != nil {
				for i := range localTraces {
					st, ok := s.exemplar.TraceStats(tenantID, serviceName, localTraces[i].TraceID, localTraces[i].Timestamp)
					if !ok || !st.Truncated {
						continue
					}
					truncated, retained, observed := true, st.Retained, st.Observed
					localTraces[i].Truncated = &truncated
					localTraces[i].RetainedSpanCount = &retained
					localTraces[i].ObservedSpanCount = &observed
				}
			}

			// Store results in pre-allocated slot (no mutex needed)
			results[idx] = batchResult{
				spans:       localSpans,
				traces:      localTraces,
				logs:        localLogs,
				tenant:      tenantID,
				hasErr:      localHasErr,
				hasSlow:     localHasSlow,
				reducer:     reducer,
				exemplarRes: exemplarRes,
			}

			return nil
		})
	}

	_ = g.Wait()

	// Merge results after all goroutines complete (no lock contention)
	var spansToInsert []storage.Span
	var tracesToUpsert []storage.Trace
	var synthesizedLogs []storage.Log
	var merged *aggregate.Reducer
	// Rows are additionally grouped by the tenant their resource resolved to
	// so the pipeline's per-tenant admission cap charges each tenant its own
	// slot (#194 finding 12). With TRUST_RESOURCE_TENANT off — the default —
	// every resource in one Export resolves to the same transport tenant and
	// this collapses to exactly one group, i.e. the previous single-batch
	// behaviour with Tenant now populated.
	groups := newTenantGroups()
	// Reservations merge along the same tenant boundary the batches do, so one
	// Submit outcome settles exactly the bytes that Submit carried.
	reservations := make(map[string]*ExemplarReservation, len(results))
	for _, r := range results {
		spansToInsert = append(spansToInsert, r.spans...)
		tracesToUpsert = append(tracesToUpsert, r.traces...)
		synthesizedLogs = append(synthesizedLogs, r.logs...)
		groups.add(r.tenant, r.traces, r.spans, r.logs, r.hasErr, r.hasSlow)
		mergeReservation(reservations, r.tenant, r.exemplarRes)
		if r.reducer == nil {
			continue
		}
		if merged == nil {
			merged = r.reducer
			continue
		}
		merged.MergeFrom(r.reducer)
	}

	// Apply the request's aggregate deltas. In aggregate mode this runs
	// BEFORE the persist decision — the aggregate path has already accepted
	// this telemetry and a downstream queue rejection must not retroactively
	// unaccount it — and blocks until the group commit lands. In shadow mode
	// it is a no-op here and runs after the submit loop instead; see the
	// mode-conditional ordering note on applyAggregatePre.
	if err := applyAggregatePre(s.aggregateEngine, merged); err != nil {
		// Nothing was submitted, so nothing was written: the reserved bytes
		// belong back in the window budget (#201 Q4). The client will retry
		// and reserve them again.
		releaseReservations(reservations)
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
		// One Submit per tenant. On the multi-tenant path (trusted resource
		// tenancy only) an ErrQueueFull on the Nth group leaves groups 1..N-1
		// enqueued while the client retries the whole Export; traces and spans
		// are idempotent on their composite unique indexes, synthesized logs
		// are not and can duplicate. Charging every tenant to one slot would
		// mean the cap protects nobody, which is the worse trade.
		all := groups.all()
		batches := make([]*Batch, 0, len(all))
		for _, g := range all {
			batches = append(batches, &Batch{
				Type:         SignalTraces,
				Tenant:       g.tenant,
				Traces:       g.traces,
				Spans:        g.spans,
				Logs:         g.logs,
				HasError:     g.hasErr,
				HasSlow:      g.hasSlow,
				SpanCallback: s.spanCallback,
				LogCallback:  s.logCallback,
				Reservation:  reservations[g.tenant],
			})
		}
		// Selected raw exemplars for the trace signal are the retained SPANS.
		// Trace rows are per-trace bookkeeping and synthesized logs were never
		// sent by the client, so neither is reportable to OTLP (#196).
		out, err := submitExemplars(s.pipeline, s.metrics, SignalTraces, batches,
			aggregateACK(s.aggregateEngine), s.exemplar.DLQDisabled(),
			func(b *Batch) int { return len(b.Spans) })
		if err != nil {
			if errors.Is(err, ErrQueueFull) {
				return nil, grpcstatus.Errorf(codes.ResourceExhausted, "ingest pipeline at capacity")
			}
			return nil, err
		}
		// Shadow mode: the raw path has now reached a non-retry outcome, so
		// the shadow aggregate may be applied — exactly once per successful
		// Export attempt.
		if err := applyAggregatePost(s.aggregateEngine, merged); err != nil {
			return nil, err
		}
		resp := &coltracepb.ExportTraceServiceResponse{}
		if out.warn() {
			// Zero rejected records: the authoritative aggregate accepted
			// every span the client sent. OTLP permits a zero-rejected
			// partial_success as a warning, and clients must not retry it.
			resp.PartialSuccess = &coltracepb.ExportTracePartialSuccess{
				RejectedSpans: 0,
				ErrorMessage:  out.message(),
			}
		}
		return resp, nil
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
			releaseReservations(reservations)
			return nil, err
		}
		// Notify GraphRAG of persisted spans
		if s.spanCallback != nil {
			for _, span := range spansToInsert {
				s.spanCallback(span)
			}
		}
	}

	// Synchronous fallback has no queue: the rows are either in the DB by now
	// or they errored out above (which returns before this point for spans).
	// Either way the submission boundary has passed, so the reservation
	// settles here.
	commitReservations(reservations)

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

	// Shadow mode, synchronous fallback: the raw writes above are the source
	// of truth and every retryable failure has already returned, so this is
	// the same non-retry point the async path applies the shadow aggregate at.
	if err := applyAggregatePost(s.aggregateEngine, merged); err != nil {
		return nil, err
	}

	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// Export handles incoming OTLP log data.
func (s *LogsServer) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	start := time.Now()
	defer func() { s.metrics.ObserveIngestDuration("logs", time.Since(start)) }()
	// slog.Debug("📥 [LOGS] Received Request", "resource_logs", len(req.ResourceLogs))

	logResults := make([][]storage.Log, len(req.ResourceLogs))
	// Per-resource tenant, parallel to logResults, so the merge below can
	// charge each tenant its own pipeline admission slot (#194 finding 12).
	logTenants := make([]string, len(req.ResourceLogs))
	// One reducer per resource batch (reduction is request-local and not
	// concurrency-safe); merged into one after the group finishes.
	reducers := make([]*aggregate.Reducer, len(req.ResourceLogs))
	// Per-resource exemplar reservations, merged by tenant below (#201 Q4).
	logReservations := make([]*ExemplarReservation, len(req.ResourceLogs))

	g, _ := errgroup.WithContext(ctx)

	for idx, resourceLogs := range req.ResourceLogs {
		g.Go(func() error {
			serviceName := getServiceName(resourceLogs.Resource.Attributes)

			if !shouldIngestService(serviceName, s.allowedServices, s.excludedServices) {
				slog.Debug("🚫 [LOGS] Dropped service", "service", serviceName)
				return nil
			}

			tenantID := resolveTenant(ctx, resourceLogs.Resource.Attributes, s.defaultTenant, s.trustResourceTenant)
			registerResource(s.resourceRegistry, tenantID, serviceName, resourceLogs.Resource.Attributes, topology.SignalLogs, start)

			localLogs := make([]storage.Log, 0)
			exemplarRes := s.exemplar.NewReservation()

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

					// Aggregate mode: raw log retention is bounded per
					// service/window and by severity — ERROR/FATAL budgeted,
					// WARN opt-in, INFO/DEBUG aggregate-only (#161). The
					// reduction above already accounted every one of them.
					if s.exemplar != nil {
						if !s.exemplar.ReserveLog(exemplarRes, tenantID, serviceName, severity, timestamp, len(bodyStr)+len(attrs)) {
							continue
						}
					}

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
			logTenants[idx] = tenantID
			logReservations[idx] = exemplarRes

			return nil
		})
	}

	_ = g.Wait()

	// Merge results after all goroutines complete (no lock contention)
	var logsToInsert []storage.Log
	// See the TraceServer.Export merge for the grouping rationale; with
	// TRUST_RESOURCE_TENANT off this yields exactly one group.
	groups := newTenantGroups()
	reservations := make(map[string]*ExemplarReservation, len(logResults))
	for idx, lr := range logResults {
		logsToInsert = append(logsToInsert, lr...)
		groups.add(logTenants[idx], nil, nil, lr, hasPriorityLog(lr), false)
		mergeReservation(reservations, logTenants[idx], logReservations[idx])
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
	// Mode-conditional ordering, same contract as TraceServer.Export: pre in
	// aggregate/legacy, post (after the submit loop) in shadow.
	if err := applyAggregatePre(s.aggregateEngine, mergedReducer); err != nil {
		releaseReservations(reservations)
		return nil, err
	}

	if len(logsToInsert) == 0 {
		// No rows means no reservations, but settle explicitly rather than
		// relying on that: a released empty reservation costs nothing.
		releaseReservations(reservations)
		// Nothing raw to submit, so the raw path's outcome is trivially
		// non-retry and the shadow aggregate applies immediately.
		if err := applyAggregatePost(s.aggregateEngine, mergedReducer); err != nil {
			return nil, err
		}
		return &collogspb.ExportLogsServiceResponse{}, nil
	}

	// Intake metric fires before the persist decision (see TraceServer.Export
	// rationale). Net persisted = ingestion_total - ingest_pipeline_dropped_total.
	if s.metrics != nil {
		s.metrics.RecordIngestion(len(logsToInsert))
	}

	// Async path: hand off to the pipeline, one Submit per tenant. Priority
	// (ERROR/FATAL bypasses soft backpressure) is now evaluated per tenant
	// group rather than across the whole Export — see hasPriorityLog.
	if s.pipeline != nil {
		all := groups.all()
		batches := make([]*Batch, 0, len(all))
		for _, g := range all {
			batches = append(batches, &Batch{
				Type:        SignalLogs,
				Tenant:      g.tenant,
				Logs:        g.logs,
				HasError:    g.hasErr,
				LogCallback: s.logCallback,
				Reservation: reservations[g.tenant],
			})
		}
		// Every log row here came off the wire — the log signal synthesizes
		// nothing — so the whole batch is selected raw exemplars.
		out, err := submitExemplars(s.pipeline, s.metrics, SignalLogs, batches,
			aggregateACK(s.aggregateEngine), s.exemplar.DLQDisabled(),
			func(b *Batch) int { return len(b.Logs) })
		if err != nil {
			if errors.Is(err, ErrQueueFull) {
				return nil, grpcstatus.Errorf(codes.ResourceExhausted, "ingest pipeline at capacity")
			}
			return nil, err
		}
		if err := applyAggregatePost(s.aggregateEngine, mergedReducer); err != nil {
			return nil, err
		}
		resp := &collogspb.ExportLogsServiceResponse{}
		if out.warn() {
			resp.PartialSuccess = &collogspb.ExportLogsPartialSuccess{
				RejectedLogRecords: 0,
				ErrorMessage:       out.message(),
			}
		}
		return resp, nil
	}

	// Synchronous fallback (preserves original behavior when async is disabled).
	if err := s.repo.BatchCreateLogs(logsToInsert); err != nil {
		slog.Error("❌ Failed to insert logs", "error", err)
		releaseReservations(reservations)
		return nil, err
	}
	commitReservations(reservations)
	if s.logCallback != nil {
		for _, l := range logsToInsert {
			s.logCallback(l)
		}
	}

	// Shadow mode, synchronous fallback — see TraceServer.Export.
	if err := applyAggregatePost(s.aggregateEngine, mergedReducer); err != nil {
		return nil, err
	}

	return &collogspb.ExportLogsServiceResponse{}, nil
}

// mergeReservation folds one resource batch's exemplar reservation into the
// per-tenant reservation the matching Batch will carry.
func mergeReservation(dst map[string]*ExemplarReservation, tenant string, res *ExemplarReservation) {
	if res == nil || res.Len() == 0 {
		return
	}
	if cur, ok := dst[tenant]; ok {
		cur.Merge(res)
		return
	}
	dst[tenant] = res
}

// commitReservations settles every tenant's reservation as written.
func commitReservations(m map[string]*ExemplarReservation) {
	for _, r := range m {
		r.Commit()
	}
}

// releaseReservations hands every tenant's reserved bytes back. Only correct
// when nothing was submitted anywhere.
func releaseReservations(m map[string]*ExemplarReservation) {
	for _, r := range m {
		r.Release()
	}
}

// tenantGroup accumulates one Export's rows for a single tenant so the async
// pipeline can charge that tenant its own per-tenant admission slot.
type tenantGroup struct {
	tenant  string
	traces  []storage.Trace
	spans   []storage.Span
	logs    []storage.Log
	hasErr  bool
	hasSlow bool
}

// tenantGroups preserves first-seen tenant order so a single-tenant Export
// (the default, since TRUST_RESOURCE_TENANT is off) produces exactly one
// group and the submit loop below behaves identically to the pre-split path.
type tenantGroups struct {
	order []string
	byID  map[string]*tenantGroup
}

func newTenantGroups() *tenantGroups {
	return &tenantGroups{byID: make(map[string]*tenantGroup)}
}

// add folds one resource's rows into its tenant's group. Empty contributions
// are ignored so resources filtered out by the service allow/deny list never
// materialize an empty batch.
func (g *tenantGroups) add(tenant string, traces []storage.Trace, spans []storage.Span, logs []storage.Log, hasErr, hasSlow bool) {
	if len(traces) == 0 && len(spans) == 0 && len(logs) == 0 {
		return
	}
	grp, ok := g.byID[tenant]
	if !ok {
		// First contribution for this tenant adopts the caller's slices
		// rather than copying them. They are per-resource locals nothing
		// reads after the merge, so the single-tenant case — the default —
		// costs no allocation beyond the pre-split path.
		g.byID[tenant] = &tenantGroup{
			tenant:  tenant,
			traces:  traces,
			spans:   spans,
			logs:    logs,
			hasErr:  hasErr,
			hasSlow: hasSlow,
		}
		g.order = append(g.order, tenant)
		return
	}
	grp.traces = append(grp.traces, traces...)
	grp.spans = append(grp.spans, spans...)
	grp.logs = append(grp.logs, logs...)
	grp.hasErr = grp.hasErr || hasErr
	grp.hasSlow = grp.hasSlow || hasSlow
}

// all returns the groups in first-seen tenant order.
func (g *tenantGroups) all() []*tenantGroup {
	out := make([]*tenantGroup, 0, len(g.order))
	for _, t := range g.order {
		out = append(out, g.byID[t])
	}
	return out
}

// hasPriorityLog reports whether any record is ERROR/FATAL, which exempts the
// batch from soft backpressure.
func hasPriorityLog(logs []storage.Log) bool {
	for _, l := range logs {
		if l.Severity == "ERROR" || l.Severity == "FATAL" {
			return true
		}
	}
	return false
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
