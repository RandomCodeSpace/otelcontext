package aggregate

import (
	"strings"
	"time"
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

// ReduceSpan folds one span into its trace-operation series.
func (r *Reducer) ReduceSpan(in SpanInput) {
	window, ok := r.admitPoint(SignalTraceOp, in.Timestamp)
	if !ok {
		return
	}
	tenantID := r.eng.cache.InternTenant(in.Tenant)
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
	r.delta(key, window).ObserveSpan(in.DurationMicros, isError)
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
	tenantID := r.eng.cache.InternTenant(in.Tenant)
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
	r.delta(key, window).ObserveSpan(in.DurationMicros, isError)
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
	tenantID := r.eng.cache.InternTenant(in.Tenant)
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

// ReduceMetricPoint folds one metric data point into its metric series,
// applying the #166 aggregation model for its temporality and monotonicity.
func (r *Reducer) ReduceMetricPoint(in MetricInput) {
	window, ok := r.admitPoint(SignalMetric, in.Timestamp)
	if !ok {
		return
	}
	tenantID := r.eng.cache.InternTenant(in.Tenant)
	key := SeriesKey{
		TenantID:  tenantID,
		ServiceID: r.eng.cache.Intern(tenantID, KindService, in.Service),
		NameID:    r.eng.cache.Intern(tenantID, KindMetricName, in.Name),
		Signal:    SignalMetric,
	}

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
