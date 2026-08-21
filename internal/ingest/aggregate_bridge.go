package ingest

import (
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

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
		Tenant:         tenantID,
		Service:        serviceName,
		SpanName:       span.Name,
		SpanKind:       int32(span.Kind),
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
