package ingest

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/topology"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func (s *TraceServer) SetInstanceGraph(g *topology.InstanceGraph)   { s.instanceGraph = g }
func (s *LogsServer) SetInstanceGraph(g *topology.InstanceGraph)    { s.instanceGraph = g }
func (s *MetricsServer) SetInstanceGraph(g *topology.InstanceGraph) { s.instanceGraph = g }

func registerInstanceResource(g *topology.InstanceGraph, tenant, service string, slots resourceSlots, signal topology.Signal, at time.Time) string {
	if g == nil || topology.IsHostEntity(service) {
		return ""
	}
	return g.Register(topology.InstanceIdentity{
		Tenant: tenant, Environment: slots.environment, Namespace: slots.serviceNamespace,
		Service: service, Instance: slots.serviceInstanceID, Host: slots.host, Workload: slots.workload,
	}, signal, at)
}

// Revisit the received request after acceptance, independently of raw sampling.
// No protobuf data or resource attribute maps are retained by the projection.
func (s *TraceServer) observeInstanceTraces(ctx context.Context, req *coltracepb.ExportTraceServiceRequest, arrival time.Time) {
	for _, resource := range req.GetResourceSpans() {
		attrs := resource.GetResource().GetAttributes()
		slots := scanResourceSlots(attrs)
		service := resolveServiceIdentity(slots, nil, "traces")
		if !shouldIngestService(service, s.allowedServices, s.excludedServices) {
			continue
		}
		tenant := resolveTenant(ctx, attrs, s.defaultTenant, s.trustResourceTenant)
		id := registerInstanceResource(s.instanceGraph, tenant, service, slots, topology.SignalTraces, arrival)
		if id == "" {
			continue
		}
		for _, scope := range resource.GetScopeSpans() {
			for _, span := range scope.GetSpans() {
				if span == nil || span.EndTimeUnixNano < span.StartTimeUnixNano {
					continue
				}
				start := time.Unix(0, int64(span.StartTimeUnixNano))                    // #nosec G115 -- validated against retained time in graph
				micros := int64((span.EndTimeUnixNano - span.StartTimeUnixNano) / 1000) // #nosec G115 -- uint64 nanoseconds / 1000 fits int64
				request := span.Kind == tracepb.Span_SPAN_KIND_SERVER || span.Kind == tracepb.Span_SPAN_KIND_CONSUMER || len(span.ParentSpanId) == 0
				s.instanceGraph.Observe(id, hex.EncodeToString(span.TraceId), hex.EncodeToString(span.SpanId), hex.EncodeToString(span.ParentSpanId),
					start, micros, request, span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR)
			}
		}
	}
}
