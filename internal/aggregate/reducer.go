package aggregate

import (
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// Request-local reduction.
//
// One Reducer per Export call. It collapses every span, log record and metric
// point in the request into a small map of (series, window) deltas BEFORE any
// global aggregate state is touched: the shards are never mutated here, so an
// Export's cost is bounded by its own batch and no ingest goroutine blocks
// another one behind a shard lock (#160).
//
// "No global mutation" means no shard mutation. Reduction does intern
// dictionary IDs and mine log templates, both of which are shared, mutex-guarded
// and idempotent — a SeriesKey cannot be built without them, and #163 puts the
// miner on this exact path on purpose.
//
// The reducer runs ahead of the sampler and the severity gates. Aggregate counts
// describe accepted telemetry, not the sampling rate (#153 §8); that is the
// whole point of the engine, and it is the invariant the shadow-mode tests
// assert.
//
// A Reducer is NOT safe for concurrent use. Export paths that fan out over
// resource batches build one Reducer per goroutine and merge them with
// MergeFrom before applying — merging a handful of deltas is cheap precisely
// because reduction already happened.

// signalCount sizes the per-signal stat arrays.
const signalCount = int(signalMax) + 1

// ReducerStats is one Export request's reduction accounting.
type ReducerStats struct {
	// InputPoints counts points offered to the reducer, per signal, including
	// the ones excluded as late or future.
	InputPoints [signalCount]uint64
	// LatePoints counts points older than the lateness horizon. They are
	// excluded from aggregates and counted here — never dropped silently
	// (#160). The raw/exemplar path still sees them.
	LatePoints [signalCount]uint64
	// FuturePoints counts points timestamped beyond the tolerated skew.
	FuturePoints [signalCount]uint64
	// Accepted counts points that contributed to a delta. This is the
	// shadow-comparison numerator: it must be identical for the same input
	// stream at any sampling rate.
	Accepted [signalCount]uint64
	// StaleCumulative counts cumulative points ignored as stale or duplicate
	// against their baseline (#166 case 1).
	StaleCumulative uint64
	// DimsRejected counts metric points whose configured dimension tuple was
	// refused from series identity because an attribute value had no scalar
	// rendering (#199 Q4). The point is still aggregated, under DimsID 0.
	DimsRejected uint64
	// TenantsRejected counts points DROPPED because their tenant identity was
	// refused — over-length, empty, or past the instance-wide tenant cap
	// (#200 Q3). Every other namespace degrades into __other__; the tenant
	// namespace refuses instead, because a shared overflow tenant is exactly
	// the cross-tenant merge the cap exists to prevent.
	TenantsRejected uint64
	// ErrorsByService counts errors per service — the cheap invariant #165
	// asks for on the aggregate side, and nothing more expensive.
	ErrorsByService map[string]uint64
}

// merge folds other into s.
func (s *ReducerStats) merge(other *ReducerStats) {
	for i := 0; i < signalCount; i++ {
		s.InputPoints[i] += other.InputPoints[i]
		s.LatePoints[i] += other.LatePoints[i]
		s.FuturePoints[i] += other.FuturePoints[i]
		s.Accepted[i] += other.Accepted[i]
	}
	s.StaleCumulative += other.StaleCumulative
	s.DimsRejected += other.DimsRejected
	s.TenantsRejected += other.TenantsRejected
	for svc, n := range other.ErrorsByService {
		if s.ErrorsByService == nil {
			s.ErrorsByService = make(map[string]uint64, len(other.ErrorsByService))
		}
		s.ErrorsByService[svc] += n
	}
}

// SpanInput is one span, already parsed out of OTLP. Only the fields that can
// affect series identity or aggregates are carried: IDs, URLs and messages are
// on the permanent banned list (#153, #159) and never reach this struct.
type SpanInput struct {
	Tenant  string
	Service string
	// SpanName is the raw span name, used for operation naming when no route
	// attribute is present.
	SpanName string
	// HTTPRoute is http.route, URLPath is url.path or http.target. Route
	// normalization precedence is fixed in #159.
	HTTPRoute string
	URLPath   string
	// Method is the raw HTTP method string; it collapses onto the bounded
	// Method enum.
	Method string
	// HTTPStatusCode is http.response.status_code, 0 when absent.
	HTTPStatusCode int
	// SpanKind and StatusCode are the OTLP numeric values.
	SpanKind   int32
	StatusCode int32
	// Root reports that the span has no parent span, i.e. it starts a trace.
	// The parent span ID itself is NOT carried: it is on the permanent banned
	// list (#153, #159) and only its emptiness affects an aggregate.
	Root bool
	// Timestamp is the span start time; it selects the window.
	Timestamp      time.Time
	DurationMicros float64
}

// LogInput is one log record.
type LogInput struct {
	Tenant  string
	Service string
	// Severity is the severity text; SeverityNumber is the OTLP number and
	// wins when non-zero.
	Severity       string
	SeverityNumber int32
	// Body is mined into a template. It never enters series identity itself.
	Body      string
	Timestamp time.Time
}

// MetricInput is one metric data point.
type MetricInput struct {
	Tenant  string
	Service string
	Name    string
	Value   float64
	// Timestamp selects the window; StartTime is the cumulative start time
	// used for reset detection.
	Timestamp time.Time
	StartTime time.Time
	// Temporality and Monotonic select the aggregation model (#166).
	Temporality Temporality
	Monotonic   bool
	// Resource carries the stable identity used to derive the ProducerID.
	Resource ResourceIdentity
	// Attributes are the RAW OTLP point attributes. The configured dimension
	// tuple is extracted from them here, against a request-local scratch, so
	// the hot path never allocates a per-point map (#199 Q4).
	Attributes []*commonpb.KeyValue
}

// Reducer collapses one Export request into deltas.
type Reducer struct {
	eng     *Engine
	arrival time.Time
	deltas  DeltaMap
	stats   ReducerStats
	// ids carries the STRING identity of each delta for the engine's topology
	// projection (#174). It is one small struct per delta, not per point, and
	// it exists so the projection never has to reverse a dictionary ID — after
	// a restart the durable dictionary is warm but the intern cache is empty,
	// so a reverse lookup would silently render an unnamed topology.
	ids map[SeriesWindowKey]topoIdentity
	// dims is the request-local dimension-extraction scratch (#199 Q4). It is
	// allocated on the first configured metric point and reused for every
	// point in the request.
	dims *dimScratch
}

// NewReducer returns a reducer for one Export request. arrival is the single
// timestamp captured for that request and used to evaluate lateness and future
// skew for every point in it.
func (e *Engine) NewReducer(arrival time.Time) *Reducer {
	return &Reducer{eng: e, arrival: arrival}
}

// Arrival returns the reducer's arrival time.
func (r *Reducer) Arrival() time.Time { return r.arrival }

// Stats returns the reduction accounting.
func (r *Reducer) Stats() ReducerStats { return r.stats }

// Deltas returns the reduced deltas. The map is handed to the engine as-is; the
// reducer must not be used afterwards.
func (r *Reducer) Deltas() DeltaMap { return r.deltas }

// Len returns the number of deltas produced so far.
func (r *Reducer) Len() int { return len(r.deltas) }

// MergeFrom folds another reducer's deltas and stats into r. Used by Export
// paths that reduce resource batches in parallel.
func (r *Reducer) MergeFrom(other *Reducer) {
	if other == nil {
		return
	}
	for swk, d := range other.deltas {
		if cur, ok := r.deltas[swk]; ok {
			cur.Merge(d)
			continue
		}
		if r.deltas == nil {
			r.deltas = make(DeltaMap, len(other.deltas))
		}
		r.deltas[swk] = d
	}
	for swk, id := range other.ids {
		if r.ids == nil {
			r.ids = make(map[SeriesWindowKey]topoIdentity, len(other.ids))
		}
		r.ids[swk] = id
	}
	r.stats.merge(&other.stats)
}

// deltaCountBySignal counts emitted deltas per signal — the denominator of the
// reduction ratio.
func (r *Reducer) deltaCountBySignal() map[Signal]uint64 {
	out := make(map[Signal]uint64, signalCount)
	for swk := range r.deltas {
		out[swk.Key.Signal]++
	}
	return out
}

// delta returns the delta for (key, window), creating it on first use.
func (r *Reducer) delta(key SeriesKey, window int64) *AggregateDelta {
	swk := SeriesWindowKey{Key: key, WindowStart: window}
	if d, ok := r.deltas[swk]; ok {
		return d
	}
	if r.deltas == nil {
		r.deltas = make(DeltaMap, 8)
	}
	d := &AggregateDelta{}
	r.deltas[swk] = d
	return d
}

// identify records the string identity of one (series, window) for the
// topology projection. Repeated calls for the same key are idempotent.
func (r *Reducer) identify(key SeriesKey, window int64, id topoIdentity) {
	swk := SeriesWindowKey{Key: key, WindowStart: window}
	if r.ids == nil {
		r.ids = make(map[SeriesWindowKey]topoIdentity, 8)
	}
	r.ids[swk] = id
}

// topologyIDs returns the reducer's identity map. The engine folds it into the
// topology projection after a successful apply.
func (r *Reducer) topologyIDs() map[SeriesWindowKey]topoIdentity { return r.ids }

// admitPoint applies the window/lateness rules and updates the per-signal
// counters. It reports ok=false when the point is excluded.
func (r *Reducer) admitPoint(signal Signal, ts time.Time) (int64, bool) {
	r.stats.InputPoints[signal]++
	window, disp := Classify(r.arrival, ts)
	switch disp {
	case PointLate:
		r.stats.LatePoints[signal]++
		return 0, false
	case PointFuture:
		r.stats.FuturePoints[signal]++
		return 0, false
	default:
		return window, true
	}
}

// countError records a per-service error for the shadow comparison.
func (r *Reducer) countError(service string) {
	if r.stats.ErrorsByService == nil {
		r.stats.ErrorsByService = make(map[string]uint64, 4)
	}
	r.stats.ErrorsByService[service]++
}

// rejectTenant records one point dropped for an unusable tenant identity. The
// point was already counted as input by admitPoint; it is NOT counted as
// accepted, so the shadow-comparison numerator stays honest.
func (r *Reducer) rejectTenant(signal Signal) {
	r.stats.TenantsRejected++
	r.eng.metrics.RecordTenantRejected(signal)
}

// IsRequestSpan reports whether a span is a request entry point and therefore
// contributes to AggregateDelta.RequestCount.
//
// The contract frozen in #197 Q2 is "root OR server span", and it is
// deliberately an OR: a root span with no parent starts a request whatever its
// kind, and a SERVER span is the server side of one even when the caller
// propagated a parent. Either qualifies, and a span that is both is still one
// request — the caller increments once per span, never once per condition.
func IsRequestSpan(root bool, spanKind int32) bool {
	return root || VariantFromSpanKind(spanKind) == SpanKindServer
}

// ReduceSpan folds one span into its trace-operation series.
func (r *Reducer) ReduceSpan(in SpanInput) {
	window, ok := r.admitPoint(SignalTraceOp, in.Timestamp)
	if !ok {
		return
	}
	tenantID, tenantOK := r.eng.cache.InternTenant(in.Tenant)
	if !tenantOK {
		r.rejectTenant(SignalTraceOp)
		return
	}
	operation := NormalizeOperation(in.HTTPRoute, in.URLPath, in.SpanName)
	key := SeriesKey{
		TenantID:    tenantID,
		ServiceID:   r.eng.cache.Intern(tenantID, KindService, in.Service),
		NameID:      r.eng.cache.Intern(tenantID, KindOperation, operation),
		Signal:      SignalTraceOp,
		StatusClass: TraceStatusFromCode(in.StatusCode),
		HTTPClass:   HTTPClassFromStatus(in.HTTPStatusCode),
		Method:      ParseMethod(in.Method),
		Variant:     VariantFromSpanKind(in.SpanKind),
	}
	isError := key.StatusClass == StatusError
	r.delta(key, window).ObserveSpan(in.DurationMicros, isError, IsRequestSpan(in.Root, in.SpanKind))
	r.identify(key, window, topoIdentity{Kind: topoTrace, Tenant: in.Tenant, A: in.Service, B: operation})
	r.stats.Accepted[SignalTraceOp]++
	if isError {
		r.countError(in.Service)
	}
}

// EdgeInput is one resolved caller/callee call, derived from a child span whose
// parent span belongs to a different service. Everything except Caller comes
// from the child span, matching what the edge measures: the callee's work as
// observed by this call.
type EdgeInput struct {
	Tenant string
	// Caller is the service that owns the parent span.
	Caller string
	// Callee is the service that owns this span.
	Callee string
	// HTTPRoute, URLPath and SpanName resolve the callee's operation for
	// route normalization; only the callee service name enters edge identity.
	HTTPRoute string
	URLPath   string
	SpanName  string

	Method         string
	HTTPStatusCode int
	SpanKind       int32
	StatusCode     int32
	// Root mirrors SpanInput.Root for the callee's span.
	Root bool

	Timestamp      time.Time
	DurationMicros float64
}

// ReduceEdge folds one resolved cross-service call into its service-edge series.
//
// #183 shipped SignalServiceEdge but emitted nothing into it: a single span
// does not know its caller. #174 supplies the caller through the engine's
// EdgeResolver and this is where it lands. Edge identity is
// (caller service, callee service) plus the callee's status/HTTP/kind
// dimensions — never a span ID, never an operation of the caller.
func (r *Reducer) ReduceEdge(in EdgeInput) {
	if in.Caller == "" || in.Callee == "" || in.Caller == in.Callee {
		return
	}
	window, ok := r.admitPoint(SignalServiceEdge, in.Timestamp)
	if !ok {
		return
	}
	tenantID, tenantOK := r.eng.cache.InternTenant(in.Tenant)
	if !tenantOK {
		r.rejectTenant(SignalServiceEdge)
		return
	}
	key := SeriesKey{
		TenantID:  tenantID,
		ServiceID: r.eng.cache.Intern(tenantID, KindService, in.Caller),
		// NameKind(SignalServiceEdge) is KindOperation: the callee service
		// name is the edge's "name" within the caller's namespace, so
		// MAX_OPERATIONS_PER_SERVICE bounds a caller's fan-out.
		NameID:      r.eng.cache.Intern(tenantID, KindOperation, in.Callee),
		Signal:      SignalServiceEdge,
		StatusClass: TraceStatusFromCode(in.StatusCode),
		HTTPClass:   HTTPClassFromStatus(in.HTTPStatusCode),
		Method:      ParseMethod(in.Method),
		Variant:     VariantFromSpanKind(in.SpanKind),
	}
	isError := key.StatusClass == StatusError
	r.delta(key, window).ObserveSpan(in.DurationMicros, isError, IsRequestSpan(in.Root, in.SpanKind))
	r.identify(key, window, topoIdentity{Kind: topoEdge, Tenant: in.Tenant, A: in.Caller, B: in.Callee})
	r.stats.Accepted[SignalServiceEdge]++
}

// ReduceLog folds one log record into its log series. The template is mined
// synchronously by the ingest-owned miner (#163); the template ID is the
// series' NameID, resolved through the log_template dictionary namespace.
func (r *Reducer) ReduceLog(in LogInput) {
	window, ok := r.admitPoint(SignalLog, in.Timestamp)
	if !ok {
		return
	}
	tenantID, tenantOK := r.eng.cache.InternTenant(in.Tenant)
	if !tenantOK {
		r.rejectTenant(SignalLog)
		return
	}
	templateID, _ := r.eng.miner.MineAt(in.Tenant, in.Service, in.Severity, in.Body, r.arrival)
	tier := SeverityTier(in.Severity, in.SeverityNumber)
	key := SeriesKey{
		TenantID:    tenantID,
		ServiceID:   r.eng.cache.Intern(tenantID, KindService, in.Service),
		NameID:      templateID,
		Signal:      SignalLog,
		StatusClass: tier,
	}
	isError := tier >= SeverityTierError
	r.delta(key, window).ObserveLog(in.Timestamp, isError)
	r.stats.Accepted[SignalLog]++
	if isError {
		r.countError(in.Service)
	}
}

// dimsIDFor resolves the DimsID of one metric point from its attributes.
//
// Missing any configured key yields 0, the "no configured dims" sentinel and
// the existing all-or-nothing contract. The scan allocates no per-point map:
// the configured tuple is bounded, so a request-local scratch holds it.
func (r *Reducer) dimsIDFor(tenantID uint32, metricName string, attrs []*commonpb.KeyValue) uint32 {
	keys := r.eng.dims.Get(metricName)
	if len(keys) == 0 {
		return 0
	}
	if r.dims == nil {
		r.dims = &dimScratch{}
	}
	values, rejected, ok := r.dims.resolve(keys, attrs)
	if rejected {
		r.stats.DimsRejected++
	}
	if !ok {
		return 0
	}
	return InternDimValues(r.eng.cache, tenantID, keys, values)
}

// metricSeriesKey builds the metric SeriesKey for one point, including its
// configured dimension tuple. It is shared by every metric point shape so the
// three of them cannot drift apart on identity (#199 Q4).
func (r *Reducer) metricSeriesKey(tenantID uint32, service, name string, attrs []*commonpb.KeyValue) SeriesKey {
	return SeriesKey{
		TenantID:  tenantID,
		ServiceID: r.eng.cache.Intern(tenantID, KindService, service),
		NameID:    r.eng.cache.Intern(tenantID, KindMetricName, name),
		DimsID:    r.dimsIDFor(tenantID, name, attrs),
		Signal:    SignalMetric,
	}
}

// MetricPointOutcome is the reducer's verdict on one metric data point.
type MetricPointOutcome uint8

// MetricPointOutcome values.
const (
	// MetricPointAccepted means the point contributed to a delta.
	MetricPointAccepted MetricPointOutcome = iota
	// MetricPointExcluded means the point fell outside the mutable-window horizon.
	// It is counted as late or future, NOT as an OTLP rejection: the client
	// sent well-formed telemetry and retrying would not help.
	MetricPointExcluded
	// MetricPointRejectedTemporality means a histogram point arrived with a
	// temporality the GA engine does not support (#199 Q3).
	MetricPointRejectedTemporality
	// MetricPointRejectedMalformed means the point violates the OTLP data model.
	MetricPointRejectedMalformed
)

// MetricPointResult reports what the reducer did with one metric data point,
// so the OTLP Export path can build an honest partial-success response.
type MetricPointResult struct {
	Outcome MetricPointOutcome
	// Reason is the metric label for a rejection, "" when accepted.
	Reason string
	// Err carries the validation failure behind PointRejectedMalformed.
	Err error
	// SketchDropped reports that the point's scalars were accepted but its
	// percentiles suppressed, and DropReason says why. This is NOT a
	// rejection: the point still contributes count, sum, min and max.
	SketchDropped bool
	DropReason    SketchDropReason
}

// Rejected reports whether the point must be counted in
// ExportMetricsPartialSuccess.rejected_data_points.
func (r MetricPointResult) Rejected() bool {
	return r.Outcome == MetricPointRejectedTemporality || r.Outcome == MetricPointRejectedMalformed
}

// Rejection reasons reported to OTLP clients.
const (
	// ReasonCumulativeTemporality — GA aggregates delta-temporality
	// histograms only; see CLAUDE.md for the collector-side conversion.
	ReasonCumulativeTemporality = "cumulative_temporality"
	// ReasonUnspecifiedTemporality — a histogram with no temporality set is
	// not interpretable either way.
	ReasonUnspecifiedTemporality = "unspecified_temporality"
	// ReasonMalformedPoint — the point violates the OTLP data model.
	ReasonMalformedPoint = "malformed_point"
	// ReasonUnsupportedType — the point type has no aggregate model at all
	// (Summary).
	ReasonUnsupportedType = "unsupported_type"
)

// checkHistogramTemporality enforces the #199 Q3 contract: GA aggregates
// delta-temporality histogram points only. A cumulative histogram is refused
// COMPLETELY -- it does not contribute a count, and it is reported to the
// client as a rejected data point so nobody builds a dashboard on a number
// that silently is not there.
func checkHistogramTemporality(t Temporality) (MetricPointResult, bool) {
	switch t {
	case TemporalityDelta:
		return MetricPointResult{}, true
	case TemporalityCumulative:
		return MetricPointResult{Outcome: MetricPointRejectedTemporality, Reason: ReasonCumulativeTemporality}, false
	default:
		return MetricPointResult{Outcome: MetricPointRejectedTemporality, Reason: ReasonUnspecifiedTemporality}, false
	}
}

// reduceFold folds an already-validated histogram fold into its series.
func (r *Reducer) reduceFold(c HistogramCommon, fold HistogramFold) MetricPointResult {
	window, ok := r.admitPoint(SignalMetric, c.Timestamp)
	if !ok {
		return MetricPointResult{Outcome: MetricPointExcluded}
	}
	tenantID, tenantOK := r.eng.cache.InternTenant(c.Tenant)
	if !tenantOK {
		r.rejectTenant(SignalMetric)
		return MetricPointResult{Outcome: MetricPointExcluded}
	}
	key := r.metricSeriesKey(tenantID, c.Service, c.Name, c.Attributes)
	r.delta(key, window).ObserveHistogram(fold)
	r.identify(key, window, topoIdentity{Kind: topoMetric, Tenant: c.Tenant, A: c.Service, B: c.Name})
	r.stats.Accepted[SignalMetric]++
	return MetricPointResult{
		Outcome:       MetricPointAccepted,
		SketchDropped: fold.PercentilesUnavailable,
		DropReason:    fold.DropReason,
	}
}

// ReduceHistogramPoint folds one OTLP explicit-bounds Histogram data point
// (#199 Q2).
func (r *Reducer) ReduceHistogramPoint(in HistogramInput) MetricPointResult {
	if res, ok := checkHistogramTemporality(in.Temporality); !ok {
		return res
	}
	fold, err := FoldHistogram(in)
	if err != nil {
		return MetricPointResult{Outcome: MetricPointRejectedMalformed, Reason: ReasonMalformedPoint, Err: err}
	}
	return r.reduceFold(in.HistogramCommon, fold)
}

// ReduceExponentialHistogramPoint folds one OTLP ExponentialHistogram data
// point (#199 Q1).
func (r *Reducer) ReduceExponentialHistogramPoint(in ExponentialHistogramInput) MetricPointResult {
	if res, ok := checkHistogramTemporality(in.Temporality); !ok {
		return res
	}
	fold, err := FoldExponentialHistogram(in)
	if err != nil {
		return MetricPointResult{Outcome: MetricPointRejectedMalformed, Reason: ReasonMalformedPoint, Err: err}
	}
	return r.reduceFold(in.HistogramCommon, fold)
}

// ReduceMetricPoint folds one metric data point into its metric series,
// applying the #166 aggregation model for its temporality and monotonicity.
func (r *Reducer) ReduceMetricPoint(in MetricInput) {
	window, ok := r.admitPoint(SignalMetric, in.Timestamp)
	if !ok {
		return
	}
	tenantID, tenantOK := r.eng.cache.InternTenant(in.Tenant)
	if !tenantOK {
		r.rejectTenant(SignalMetric)
		return
	}
	key := r.metricSeriesKey(tenantID, in.Service, in.Name, in.Attributes)

	switch {
	// Gauges and cumulative non-monotonic sums (UpDownCounter): gauge-like,
	// never reset-detected. Negative movement is legitimate here.
	case IsGaugeLike(in.Temporality, in.Monotonic):
		r.delta(key, window).ObserveGauge(in.Value, in.Timestamp)

	// Delta temporality merges directly; negative deltas are legal for
	// non-monotonic instruments.
	case in.Temporality == TemporalityDelta:
		r.delta(key, window).ObserveCounter(in.Value, false)

	// Cumulative monotonic: convert against the producer's baseline.
	default:
		producer := ResolveProducerID(in.Resource)
		out := r.eng.baselines.ObserveCumulative(key, producer, in.StartTime, in.Timestamp, in.Value)
		if out.Ignored {
			// Stale or duplicate: it must not move the baseline, synthesize a
			// delta, or count as a reset.
			r.stats.StaleCumulative++
			return
		}
		d := r.delta(key, window)
		d.ObserveCounter(out.Delta, out.Reset)
	}
	r.identify(key, window, topoIdentity{Kind: topoMetric, Tenant: in.Tenant, A: in.Service, B: in.Name})
	r.stats.Accepted[SignalMetric]++
}

// SeverityTier maps an OTLP severity onto the log StatusClass tier. The numeric
// severity wins when present; otherwise the text is classified with the same
// substring rules the legacy ingest gate uses, so a shadow-mode comparison is
// not thrown off by a disagreement about what "WARNING" means.
func SeverityTier(text string, number int32) StatusClass {
	if number > 0 {
		return SeverityTierFromNumber(number)
	}
	upper := strings.ToUpper(text)
	switch {
	case strings.Contains(upper, "FATAL"), strings.Contains(upper, "CRIT"):
		return SeverityTierFatal
	case strings.Contains(upper, "ERR"):
		return SeverityTierError
	case strings.Contains(upper, "WARN"):
		return SeverityTierWarn
	case strings.Contains(upper, "DEBUG"):
		return SeverityTierDebug
	case strings.Contains(upper, "TRACE"):
		return SeverityTierTrace
	case strings.Contains(upper, "INFO"):
		return SeverityTierInfo
	case upper == "":
		return SeverityTierUnspecified
	default:
		return SeverityTierInfo
	}
}
