package ingest

import (
	"errors"
	"fmt"
	"log/slog"
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
// exemplars(b) counts the selected raw exemplars carried by one batch.
func submitExemplars(
	p *Pipeline,
	metrics *telemetry.Metrics,
	signal SignalType,
	batches []*Batch,
	ackedByAggregate bool,
	exemplars func(*Batch) int,
) (exemplarOutcome, error) {
	var out exemplarOutcome
	for _, b := range batches {
		_, err := p.Submit(b)
		if err == nil {
			if ackedByAggregate {
				observeExemplarSubmit(metrics, signal, "queued", "none")
			}
			continue
		}
		if !ackedByAggregate || !errors.Is(err, ErrQueueFull) {
			return out, err
		}

		// Aggregate mode, primary queue saturated: one bounded attempt at the
		// DLQ. Accepted => deferred; refused or errored => permanent counted
		// loss. Neither changes the Export result.
		n := exemplars(b)
		switch dlqErr := p.OfferToDLQ(b); {
		case dlqErr == nil:
			out.deferred += n
			observeExemplarSubmit(metrics, signal, "dlq", "queue_full")
		case errors.Is(dlqErr, ErrDLQFull):
			out.lost += n
			observeExemplarSubmit(metrics, signal, "lost", "dlq_full")
			observeExemplarLost(metrics, signal, "dlq_full")
		default:
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
	var id aggregate.ResourceIdentity
	for _, kv := range attrs {
		if kv == nil || kv.Value == nil {
			continue
		}
		switch kv.Key {
		case "service.instance.id":
			id.ServiceInstanceID = kv.Value.GetStringValue()
		case "service.namespace":
			id.ServiceNamespace = kv.Value.GetStringValue()
		case "service.name":
			id.ServiceName = kv.Value.GetStringValue()
		case "host.id":
			id.Host = kv.Value.GetStringValue()
		case "host.name":
			if id.Host == "" {
				id.Host = kv.Value.GetStringValue()
			}
		case "k8s.pod.uid":
			id.Workload = kv.Value.GetStringValue()
		case "container.id":
			if id.Workload == "" {
				id.Workload = kv.Value.GetStringValue()
			}
		case "process.pid":
			if id.Workload == "" {
				id.Workload = kv.Value.String()
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
