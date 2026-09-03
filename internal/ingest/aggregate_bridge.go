package ingest

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/telemetry"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// applyAggregate applies one Export's reduced deltas and maps a refusal onto
// the error the OTLP client should see.
//
// Under the durable-ACK contract (#160) an Export may only succeed once its
// deltas are inside a committed transaction, so a saturated group-commit
// writer answers RESOURCE_EXHAUSTED — the same signal, and the same HTTP 429
// mapping, the raw pipeline's ErrQueueFull already produces.
//
// A commit FAILURE is treated by mode. In aggregate mode the store is the
// authoritative dataset and a failed commit must not be acknowledged. In shadow
// mode the legacy path is still the source of truth and the aggregate numbers
// exist to be compared: failing the Export there would turn a shadow-side
// problem into raw telemetry loss, so it is logged and metered instead.
func applyAggregate(eng *aggregate.Engine, r *aggregate.Reducer) error {
	if eng == nil || r == nil {
		return nil
	}
	_, err := eng.ApplyReducerErr(r)
	if err == nil {
		return nil
	}
	if errors.Is(err, aggregate.ErrSaturated) {
		return grpcstatus.Errorf(codes.ResourceExhausted, "aggregate store at capacity")
	}
	if eng.Mode() == aggregate.ModeAggregate {
		// The one case where a disk problem MUST fail the Export (#201 Q5).
		// Raw exemplar shedding never does this — diagnostics are droppable.
		// The authoritative aggregate commit is not: a success response
		// asserts the deltas are in a committed transaction, and answering OK
		// for data that hit ENOSPC or SQLITE_FULL is data loss with better
		// branding. ResourceExhausted so the client backs off and retries
		// rather than hammering a full disk.
		if aggregate.IsDiskFull(err) {
			slog.Error("aggregate commit failed: device out of space — failing the Export so the client retries", "error", err)
			return grpcstatus.Errorf(codes.ResourceExhausted, "aggregate store commit failed: no space left on device")
		}
		return grpcstatus.Errorf(codes.Unavailable, "aggregate store commit failed")
	}
	slog.Error("aggregate shadow commit failed (legacy path remains the source of truth)", "error", err)
	return nil
}

// Ordering of the aggregate apply relative to the raw exemplar submit is
// MODE-CONDITIONAL, and deliberately so (#196 Q4). Exactly one of
// applyAggregatePre / applyAggregatePost applies a given reducer:
//
//	aggregate mode
//	    The durable aggregate commit IS the Export ACK (#160), so it must land
//	    BEFORE the raw exemplar submit. Once it has, a saturated exemplar queue
//	    degrades to DLQ-deferral or counted loss and can never make the Export
//	    retryable — a retry would double-count the authoritative aggregate,
//	    which is release blocker 1 of #194.
//
//	aggregate-shadow mode
//	    The mirror image: the legacy raw path is still the source of truth, so
//	    the shadow aggregate is applied only AFTER the raw path reaches a
//	    non-retry outcome. A hard ErrQueueFull returns RESOURCE_EXHAUSTED with
//	    no shadow contribution at all, so the client's retry contributes
//	    exactly once. An enqueue or an intentional soft-drop are both non-retry
//	    outcomes and both apply the shadow aggregate exactly once.
//
//	legacy mode
//	    No engine exists; both calls are nil-checks that return immediately.
//
// applyAggregatePre applies the reducer everywhere EXCEPT shadow mode.
func applyAggregatePre(eng *aggregate.Engine, r *aggregate.Reducer) error {
	if eng == nil || eng.Mode() == aggregate.ModeShadow {
		return nil
	}
	return applyAggregate(eng, r)
}

// applyAggregatePost applies the reducer ONLY in shadow mode, and only once
// the caller has established that the raw path will not ask the client to
// retry. See the ordering note on applyAggregatePre.
func applyAggregatePost(eng *aggregate.Engine, r *aggregate.Reducer) error {
	if eng == nil || eng.Mode() != aggregate.ModeShadow {
		return nil
	}
	return applyAggregate(eng, r)
}

// aggregateACK reports whether the durable aggregate commit is this Export's
// authoritative acknowledgement — true only in AGGREGATE_MODE=aggregate. It
// is the switch that lets submitExemplars absorb a hard queue rejection.
func aggregateACK(eng *aggregate.Engine) bool {
	return eng != nil && eng.Mode() == aggregate.ModeAggregate
}

// exemplarOutcome accumulates one Export's raw-exemplar submission results so
// the OTLP response can carry an honest partial_success warning.
//
// Counts are of SELECTED RAW EXEMPLARS — records the client actually sent and
// this Export chose to retain. Synthesized logs are never counted: the client
// never sent them, so OTLP must not describe them (#196).
type exemplarOutcome struct {
	deferred int // handed to the DLQ after the primary queue refused them
	lost     int // the DLQ could not hold them either; permanently gone
}

// warn reports whether the OTLP response must carry a partial_success warning.
func (o exemplarOutcome) warn() bool { return o.deferred > 0 || o.lost > 0 }

// message renders the partial_success error_message for this outcome. Empty
// when nothing was deferred or lost.
func (o exemplarOutcome) message() string {
	switch {
	case o.deferred > 0 && o.lost > 0:
		return fmt.Sprintf("aggregate data accepted; %d selected raw exemplars were deferred to DLQ; %d could not be retained", o.deferred, o.lost)
	case o.deferred > 0:
		return fmt.Sprintf("aggregate data accepted; %d selected raw exemplars were deferred to DLQ", o.deferred)
	case o.lost > 0:
		return fmt.Sprintf("aggregate data accepted; %d selected raw exemplars could not be retained", o.lost)
	default:
		return ""
	}
}

// submitExemplars runs one Export's per-tenant Submit loop.
//
// When ackedByAggregate is false (shadow, legacy, and the synchronous
// fallback) the behaviour is exactly what it always was: the first hard
// rejection is returned so the caller answers RESOURCE_EXHAUSTED / 429 and the
// client retries the whole Export.
//
// When ackedByAggregate is true the aggregate commit has already acknowledged
// this Export, so ErrQueueFull is absorbed rather than returned: the batch is
// offered to the DLQ exactly once, the outcome is counted, and the loop
// continues with the remaining tenant groups. Non-ErrQueueFull errors are
// still returned — those are not backpressure.
//
// dlqDisabled closes the DLQ fallback. Set when the disk watchdog is in
// raw-off (#201 Q5): at >=95% of the enforcement ceiling, deferring an
// exemplar to the DLQ is still writing to the disk that is about to fill.
//
// exemplars(b) counts the selected raw exemplars carried by one batch.
//
// Byte accounting: each batch carries the reservation for the rows in it
// (#201 Q4). Accepted by the primary queue or the DLQ => Commit, and the
// charge is monotonic for that window from then on. Refused by both, or
// abandoned because this Export is going to fail => Release.
func submitExemplars(
	p *Pipeline,
	metrics *telemetry.Metrics,
	signal SignalType,
	batches []*Batch,
	ackedByAggregate bool,
	dlqDisabled bool,
	exemplars func(*Batch) int,
) (exemplarOutcome, error) {
	var out exemplarOutcome
	for i, b := range batches {
		_, err := p.Submit(b)
		if err == nil {
			b.Reservation.Commit()
			if ackedByAggregate {
				observeExemplarSubmit(metrics, signal, "queued", "none")
			}
			continue
		}
		if !ackedByAggregate || !errors.Is(err, ErrQueueFull) {
			// The Export fails, so nothing in this batch or any batch after it
			// reached a destination. Their bytes were never written.
			releaseFrom(batches, i)
			return out, err
		}

		// Aggregate mode, primary queue saturated: one bounded attempt at the
		// DLQ. Accepted => deferred; refused, errored, or closed by the disk
		// watchdog => permanent counted loss. Neither changes the Export
		// result.
		n := exemplars(b)
		if dlqDisabled {
			b.Reservation.Release()
			out.lost += n
			observeExemplarSubmit(metrics, signal, "lost", "disk_shed")
			observeExemplarLost(metrics, signal, "disk_shed")
			slog.Warn("exemplar batch dropped: raw pipeline saturated and DLQ closed by disk pressure",
				"signal", signalLabel(signal),
				"tenant", b.Tenant,
				"exemplars", n,
			)
			continue
		}
		switch dlqErr := p.OfferToDLQ(b); {
		case dlqErr == nil:
			b.Reservation.Commit()
			out.deferred += n
			observeExemplarSubmit(metrics, signal, "dlq", "queue_full")
		case errors.Is(dlqErr, ErrDLQFull):
			b.Reservation.Release()
			out.lost += n
			observeExemplarSubmit(metrics, signal, "lost", "dlq_full")
			observeExemplarLost(metrics, signal, "dlq_full")
		default:
			b.Reservation.Release()
			out.lost += n
			observeExemplarSubmit(metrics, signal, "lost", "dlq_error")
			observeExemplarLost(metrics, signal, "dlq_error")
			slog.Warn("exemplar batch lost: raw pipeline saturated and DLQ unavailable",
				"signal", signalLabel(signal),
				"tenant", b.Tenant,
				"exemplars", n,
				"error", dlqErr,
			)
		}
	}
	return out, nil
}

// releaseFrom hands back the reservations of batches[i:] — the batch that just
// failed and every one that will never be attempted.
func releaseFrom(batches []*Batch, i int) {
	for _, b := range batches[i:] {
		b.Reservation.Release()
	}
}

func observeExemplarSubmit(m *telemetry.Metrics, signal SignalType, outcome, reason string) {
	if m == nil || m.ExemplarSubmitTotal == nil {
		return
	}
	m.ExemplarSubmitTotal.WithLabelValues(signalLabel(signal), outcome, reason).Inc()
}

func observeExemplarLost(m *telemetry.Metrics, signal SignalType, reason string) {
	if m == nil || m.ExemplarSubmitLostTotal == nil {
		return
	}
	m.ExemplarSubmitLostTotal.WithLabelValues(signalLabel(signal), reason).Inc()
}

// OTLP -> aggregate translation.
//
// Kept out of otlp.go so the legacy Export paths stay readable and so the
// aggregate wiring is one nil check plus one call at each site. When
// AGGREGATE_MODE=legacy no Engine is constructed, every reducer reference is
// nil, and none of this runs.

// aggregateSpanInput builds the reducer input for one span. It reads only the
// attributes that can affect series identity; span/trace IDs, URLs and messages
// are on the permanent banned list (#153, #159) and are never carried across.
func aggregateSpanInput(tenantID, serviceName string, span *tracepb.Span, start, end time.Time) aggregate.SpanInput {
	in := aggregate.SpanInput{
		Tenant:   tenantID,
		Service:  serviceName,
		SpanName: span.Name,
		SpanKind: int32(span.Kind),
		// Only the EMPTINESS of the parent span ID crosses into the aggregate
		// path; the ID itself is on the permanent banned list (#153, #159).
		Root:           len(span.ParentSpanId) == 0,
		Timestamp:      start,
		DurationMicros: float64(end.Sub(start).Microseconds()),
	}
	if span.Status != nil {
		in.StatusCode = int32(span.Status.Code)
	}
	for _, kv := range span.Attributes {
		if kv == nil || kv.Value == nil {
			continue
		}
		switch kv.Key {
		case "http.route":
			in.HTTPRoute = kv.Value.GetStringValue()
		case "url.path":
			in.URLPath = kv.Value.GetStringValue()
		case "http.target":
			if in.URLPath == "" {
				in.URLPath = kv.Value.GetStringValue()
			}
		case "http.request.method":
			in.Method = kv.Value.GetStringValue()
		case "http.method":
			if in.Method == "" {
				in.Method = kv.Value.GetStringValue()
			}
		case "http.response.status_code":
			in.HTTPStatusCode = int(kv.Value.GetIntValue())
		case "http.status_code":
			if in.HTTPStatusCode == 0 {
				in.HTTPStatusCode = int(kv.Value.GetIntValue())
			}
		}
	}
	return in
}

// aggregateEdgeInput derives the service-edge reducer input from a resolved
// caller and the callee's own span input. Everything except the caller comes
// from the callee: an edge measures the callee's work as observed by this call.
func aggregateEdgeInput(caller string, in aggregate.SpanInput) aggregate.EdgeInput {
	return aggregate.EdgeInput{
		Tenant:         in.Tenant,
		Caller:         caller,
		Callee:         in.Service,
		HTTPRoute:      in.HTTPRoute,
		URLPath:        in.URLPath,
		SpanName:       in.SpanName,
		Method:         in.Method,
		HTTPStatusCode: in.HTTPStatusCode,
		SpanKind:       in.SpanKind,
		StatusCode:     in.StatusCode,
		Root:           in.Root,
		Timestamp:      in.Timestamp,
		DurationMicros: in.DurationMicros,
	}
}

// aggregateResourceIdentity extracts the stable resource identity used to
// derive a metric producer's ProducerID (#166). Only the first-present value
// per slot is taken: attributes that vary per export would fragment baselines.
func aggregateResourceIdentity(attrs []*commonpb.KeyValue) aggregate.ResourceIdentity {
	return scanResourceSlots(attrs).identity()
}

// resourceSlots is the stable identity slice of one OTLP resource, read once
// per resource batch and shared by the aggregate producer identity and the
// resource registry (#279). host is host.id else host.name; workload is
// k8s.pod.uid else container.id else process.pid; workloadKind names the
// slot that filled workload (pod|container|process) or "".
type resourceSlots struct {
	serviceInstanceID string
	serviceNamespace  string
	serviceName       string
	host              string
	workload          string
	workloadKind      string
}

func (s resourceSlots) identity() aggregate.ResourceIdentity {
	return aggregate.ResourceIdentity{
		ServiceInstanceID: s.serviceInstanceID,
		ServiceNamespace:  s.serviceNamespace,
		ServiceName:       s.serviceName,
		Host:              s.host,
		Workload:          s.workload,
	}
}

// pidString renders process.pid as its decimal value whether the producer sent
// it as an OTLP int or string; the registry persists it, so the protobuf text
// form (int_value:1234) must never reach a row.
func pidString(v *commonpb.AnyValue) string {
	if s := v.GetStringValue(); s != "" {
		return s
	}
	if _, ok := v.GetValue().(*commonpb.AnyValue_IntValue); ok {
		return strconv.FormatInt(v.GetIntValue(), 10)
	}
	return ""
}

func scanResourceSlots(attrs []*commonpb.KeyValue) resourceSlots {
	var id resourceSlots
	for _, kv := range attrs {
		if kv == nil || kv.Value == nil {
			continue
		}
		switch kv.Key {
		case "service.instance.id":
			id.serviceInstanceID = kv.Value.GetStringValue()
		case "service.namespace":
			id.serviceNamespace = kv.Value.GetStringValue()
		case "service.name":
			id.serviceName = kv.Value.GetStringValue()
		case "host.id":
			id.host = kv.Value.GetStringValue()
		case "host.name":
			if id.host == "" {
				id.host = kv.Value.GetStringValue()
			}
		case "k8s.pod.uid":
			id.workload = kv.Value.GetStringValue()
			id.workloadKind = "pod"
		case "container.id":
			if id.workload == "" {
				id.workload = kv.Value.GetStringValue()
				id.workloadKind = "container"
			}
		case "process.pid":
			if id.workload == "" {
				id.workload = pidString(kv.Value)
				id.workloadKind = "process"
			}
		}
	}
	return id
}

// aggregateTemporality maps the OTLP aggregation temporality of a metric onto
// the engine's enum, along with whether the instrument is monotonic. A gauge
// reports (unspecified, false), which is exactly the gauge-like model.
func aggregateTemporality(m *metricspb.Metric) (aggregate.Temporality, bool) {
	if sum := m.GetSum(); sum != nil {
		switch sum.AggregationTemporality {
		case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA:
			return aggregate.TemporalityDelta, sum.IsMonotonic
		case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE:
			return aggregate.TemporalityCumulative, sum.IsMonotonic
		default:
			// Unspecified temporality on a Sum is a broken producer. It is
			// passed through as unspecified so the reducer picks the model
			// from monotonicity alone: gauge-like when non-monotonic,
			// baseline conversion when monotonic.
			return aggregate.TemporalityUnspecified, sum.IsMonotonic
		}
	}
	return aggregate.TemporalityUnspecified, false
}

// --- OTLP metric completeness (#199) -----------------------------------------

// otlpDataPointNoRecordedValue is DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK. A
// point carrying it asserts that no value was recorded in the interval; it is
// neither accepted nor rejected, because there is nothing to account for.
const otlpDataPointNoRecordedValue uint32 = 1

// OTLP point-type labels for the unsupported/rejection counters.
const (
	pointTypeHistogram    = "histogram"
	pointTypeExpHistogram = "exponential_histogram"
	pointTypeSummary      = "summary"
)

// otlpTemporality maps an OTLP AggregationTemporality onto the engine's enum.
// Unlike aggregateTemporality it is not Sum-specific: histogram points carry
// their temporality on the enclosing Histogram/ExponentialHistogram message.
func otlpTemporality(t metricspb.AggregationTemporality) aggregate.Temporality {
	switch t {
	case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA:
		return aggregate.TemporalityDelta
	case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE:
		return aggregate.TemporalityCumulative
	default:
		return aggregate.TemporalityUnspecified
	}
}

// histogramCommonFields builds the identity and scalar payload shared by both
// histogram point shapes. Optional sum/min/max arrive as proto pointers and
// their presence is carried explicitly: a missing min is not a min of zero,
// and treating it as one would claim the population is non-negative.
func histogramCommonFields(
	tenant, service, name string,
	res aggregate.ResourceIdentity,
	temporality aggregate.Temporality,
	attrs []*commonpb.KeyValue,
	timeNanos, startNanos uint64,
	count uint64,
	sum, minV, maxV *float64,
) aggregate.HistogramCommon {
	c := aggregate.HistogramCommon{
		Tenant:      tenant,
		Service:     service,
		Name:        name,
		Resource:    res,
		Timestamp:   time.Unix(0, int64(timeNanos)),  // #nosec G115 -- OTLP nanos fit int64 until year 2262
		StartTime:   time.Unix(0, int64(startNanos)), // #nosec G115 -- OTLP nanos fit int64 until year 2262
		Temporality: temporality,
		Attributes:  attrs,
		Count:       count,
	}
	if sum != nil {
		c.Sum, c.HasSum = *sum, true
	}
	if minV != nil {
		c.Min, c.HasMin = *minV, true
	}
	if maxV != nil {
		c.Max, c.HasMax = *maxV, true
	}
	return c
}

// aggregateHistogramInput converts one OTLP HistogramDataPoint.
func aggregateHistogramInput(
	tenant, service, name string,
	res aggregate.ResourceIdentity,
	temporality aggregate.Temporality,
	p *metricspb.HistogramDataPoint,
) aggregate.HistogramInput {
	return aggregate.HistogramInput{
		HistogramCommon: histogramCommonFields(tenant, service, name, res, temporality,
			p.Attributes, p.TimeUnixNano, p.StartTimeUnixNano, p.Count, p.Sum, p.Min, p.Max),
		Bounds:       p.ExplicitBounds,
		BucketCounts: p.BucketCounts,
	}
}

// aggregateExpHistogramInput converts one OTLP ExponentialHistogramDataPoint.
func aggregateExpHistogramInput(
	tenant, service, name string,
	res aggregate.ResourceIdentity,
	temporality aggregate.Temporality,
	p *metricspb.ExponentialHistogramDataPoint,
) aggregate.ExponentialHistogramInput {
	return aggregate.ExponentialHistogramInput{
		HistogramCommon: histogramCommonFields(tenant, service, name, res, temporality,
			p.Attributes, p.TimeUnixNano, p.StartTimeUnixNano, p.Count, p.Sum, p.Min, p.Max),
		Scale:     p.Scale,
		ZeroCount: p.ZeroCount,
		Positive:  expBuckets(p.Positive),
		Negative:  expBuckets(p.Negative),
	}
}

// expBuckets converts one side of an exponential histogram.
func expBuckets(b *metricspb.ExponentialHistogramDataPoint_Buckets) aggregate.ExpBuckets {
	if b == nil {
		return aggregate.ExpBuckets{}
	}
	return aggregate.ExpBuckets{Offset: b.Offset, Counts: b.BucketCounts}
}

// metricRejectKey identifies one (point type, reason) pair in the partial
// success accounting.
type metricRejectKey struct{ kind, reason string }

// metricRejections accumulates the metric data points one Export refused, so
// the response can report an exact rejected_data_points count with a bounded
// message naming the types and reasons (#199 Q5).
//
// Late and future points are deliberately NOT counted here. They are excluded
// from aggregates and reported on the lateness counters, but the client sent
// well-formed telemetry and a retry would not change the outcome; calling that
// a rejection would train operators to ignore the field that matters.
type metricRejections struct {
	total  uint64
	counts map[metricRejectKey]uint64
}

// add records n rejected points of one type and reason.
func (m *metricRejections) add(kind, reason string, n uint64) {
	if n == 0 {
		return
	}
	if m.counts == nil {
		m.counts = make(map[metricRejectKey]uint64, 4)
	}
	m.counts[metricRejectKey{kind: kind, reason: reason}] += n
	m.total += n
}

// maxRejectionReasons bounds how many distinct (type, reason) pairs the
// response message names. The COUNT is always exact; only the prose is capped,
// because an error message is not a log sink.
const maxRejectionReasons = 6

// message renders the bounded human-readable summary.
func (m *metricRejections) message() string {
	if m.total == 0 {
		return ""
	}
	keys := make([]metricRejectKey, 0, len(m.counts))
	for k := range m.counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m.counts[keys[i]] != m.counts[keys[j]] {
			return m.counts[keys[i]] > m.counts[keys[j]]
		}
		if keys[i].kind != keys[j].kind {
			return keys[i].kind < keys[j].kind
		}
		return keys[i].reason < keys[j].reason
	})
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d metric data points were not aggregated and must not be retried:", m.total)
	for i, k := range keys {
		if i == maxRejectionReasons {
			fmt.Fprintf(&sb, " (+%d more reasons)", len(keys)-i)
			break
		}
		fmt.Fprintf(&sb, " %s/%s=%d", k.kind, k.reason, m.counts[k])
	}
	return sb.String()
}

// record folds one reducer verdict into the rejection accounting and the
// telemetry counters.
func (m *metricRejections) record(metrics *telemetry.Metrics, kind string, res aggregate.MetricPointResult) {
	if res.Rejected() {
		m.add(kind, res.Reason, 1)
		metrics.RecordMetricUnsupported(kind, res.Reason, 1)
		return
	}
	if res.SketchDropped {
		metrics.RecordMetricSketchDropped(res.DropReason.String())
	}
}
