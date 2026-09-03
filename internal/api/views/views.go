// Package views provides explicit JSON view models for the HTTP API.
//
// Handlers MUST NOT serialize GORM storage models directly — doing so leaks
// ORM bookkeeping (CreatedAt, UpdatedAt, DeletedAt) and tenant_id to the wire,
// and couples the UI contract to the schema. Each type here is the stable
// JSON shape consumed by the UI and by MCP clients.
//
// Rules:
//   - No GORM bookkeeping fields (DeletedAt, CreatedAt, UpdatedAt).
//   - No tenant_id — auth already scopes the request.
//   - Preserve JSON field names that consumers rely on.
package views

import (
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/latency"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

// --- Primary entity views ---

// Trace is the wire shape of a distributed trace summary.
type Trace struct {
	ID          uint      `json:"id"`
	TraceID     string    `json:"trace_id"`
	ServiceName string    `json:"service_name"`
	Operation   string    `json:"operation"`
	Status      string    `json:"status"`
	Duration    int64     `json:"duration"` // microseconds, preserved for legacy consumers
	DurationMs  float64   `json:"duration_ms"`
	SpanCount   int       `json:"span_count"`
	Timestamp   time.Time `json:"timestamp"`
	Spans       []Span    `json:"spans,omitempty"`
	Logs        []Log     `json:"logs,omitempty"`
}

// Span is the wire shape of a single operation inside a trace.
type Span struct {
	ID             uint      `json:"id"`
	TraceID        string    `json:"trace_id"`
	SpanID         string    `json:"span_id"`
	ParentSpanID   string    `json:"parent_span_id"`
	OperationName  string    `json:"operation_name"`
	StartTime      time.Time `json:"start_time"`
	EndTime        time.Time `json:"end_time"`
	Duration       int64     `json:"duration"`
	ServiceName    string    `json:"service_name"`
	Status         string    `json:"status"`
	AttributesJSON string    `json:"attributes_json"`
}

// Log is the wire shape of an ingested log record.
type Log struct {
	ID             uint      `json:"id"`
	TraceID        string    `json:"trace_id"`
	SpanID         string    `json:"span_id"`
	Severity       string    `json:"severity"`
	Body           string    `json:"body"`
	ServiceName    string    `json:"service_name"`
	AttributesJSON string    `json:"attributes_json"`
	AIInsight      string    `json:"ai_insight"`
	Timestamp      time.Time `json:"timestamp"`
}

// MetricBucket is the wire shape of a pre-aggregated metric window.
type MetricBucket struct {
	ID             uint      `json:"id"`
	Name           string    `json:"name"`
	ServiceName    string    `json:"service_name"`
	TimeBucket     time.Time `json:"time_bucket"`
	Min            float64   `json:"min"`
	Max            float64   `json:"max"`
	Sum            float64   `json:"sum"`
	Count          int64     `json:"count"`
	AttributesJSON string    `json:"attributes_json"`
}

// --- Compound response views ---

// TracesResponse is the paginated trace-list response.
type TracesResponse struct {
	Traces []Trace `json:"traces"`
	Total  int64   `json:"total"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
}

// ServiceError is the top-failing-service entry on the dashboard.
type ServiceError struct {
	ServiceName string  `json:"service_name"`
	ErrorCount  int64   `json:"error_count"`
	TotalCount  int64   `json:"total_count"`
	ErrorRate   float64 `json:"error_rate"`
}

// DashboardStats is the aggregated dashboard metric view.
//
// The coverage/accuracy/epoch/revision fields and the six basis fields are
// ADDITIVE and only populated in aggregate mode: `omitempty` keeps the legacy
// payload byte-for-byte what it has always been.
//
// total_traces / total_errors / error_rate are REQUEST-basis in both modes —
// legacy counts trace rows, aggregate counts root/SERVER spans (#194 blocker
// 5). requests/request_errors/request_error_rate restate that basis by name,
// and spans/span_errors/span_error_rate carry the span basis the old aggregate
// payload was mislabelling as traces. Both rates are percents, like error_rate.
type DashboardStats struct {
	TotalTraces        int64               `json:"total_traces"`
	TotalLogs          int64               `json:"total_logs"`
	TotalErrors        int64               `json:"total_errors"`
	AvgLatencyMs       float64             `json:"avg_latency_ms"`
	ErrorRate          float64             `json:"error_rate"`
	ActiveServices     int64               `json:"active_services"`
	P99LatencyMs       float64             `json:"p99_latency_ms"`
	LatencyProvenance  *latency.Provenance `json:"latency_provenance,omitempty"`
	TopFailingServices []ServiceError      `json:"top_failing_services"`

	Requests         int64   `json:"requests,omitempty"`
	RequestErrors    int64   `json:"request_errors,omitempty"`
	RequestErrorRate float64 `json:"request_error_rate,omitempty"`
	Spans            int64   `json:"spans,omitempty"`
	SpanErrors       int64   `json:"span_errors,omitempty"`
	SpanErrorRate    float64 `json:"span_error_rate,omitempty"`

	Coverage     string                      `json:"coverage,omitempty"`
	CoverageNote string                      `json:"coverage_note,omitempty"`
	Accuracy     *aggregate.AccuracyMetadata `json:"accuracy,omitempty"`
	Epoch        string                      `json:"epoch,omitempty"`
	Revision     uint64                      `json:"revision,omitempty"`
}

// ServiceMapNode is a node on the service topology view.
type ServiceMapNode struct {
	Name              string              `json:"name"`
	TotalTraces       int64               `json:"total_traces"`
	ErrorCount        int64               `json:"error_count"`
	AvgLatencyMs      float64             `json:"avg_latency_ms"`
	P99LatencyMs      float64             `json:"p99_latency_ms,omitempty"`
	LatencyProvenance *latency.Provenance `json:"latency_provenance,omitempty"`

	// Additive host projection (#288): kind is service|host, hosts is sorted
	// and capped at topology.MaxHostsPerNode, host_count is the full total.
	Kind      string   `json:"kind,omitempty"`
	HostCount int      `json:"host_count,omitempty"`
	Hosts     []string `json:"hosts,omitempty"`
}

// ServiceMapEdge is an edge on the service topology view.
type ServiceMapEdge struct {
	Source       string  `json:"source"`
	Target       string  `json:"target"`
	CallCount    int64   `json:"call_count"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	ErrorRate    float64 `json:"error_rate"`
}

// ServiceMapMetrics is the full topology view. Coverage is additive and only
// populated in aggregate mode.
type ServiceMapMetrics struct {
	Nodes []ServiceMapNode `json:"nodes"`
	Edges []ServiceMapEdge `json:"edges"`

	Source       string `json:"source,omitempty"`
	Coverage     string `json:"coverage,omitempty"`
	CoverageNote string `json:"coverage_note,omitempty"`
	Epoch        string `json:"epoch,omitempty"`
	Revision     uint64 `json:"revision,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`

	DroppedServices   uint64 `json:"dropped_services,omitempty"`
	DroppedOperations uint64 `json:"dropped_operations,omitempty"`
	DroppedEdges      uint64 `json:"dropped_edges,omitempty"`
	DroppedMetrics    uint64 `json:"dropped_metrics,omitempty"`
}

// --- GraphRAG views ---

// LogClusterNode is the wire shape of a log cluster (Drain template).
type LogClusterNode struct {
	ID             string           `json:"id"`
	Template       string           `json:"template"`
	TemplateID     uint64           `json:"template_id,omitempty"`
	TemplateTokens []string         `json:"template_tokens,omitempty"`
	SampleLog      string           `json:"sample_log,omitempty"`
	Count          int64            `json:"count"`
	FirstSeen      time.Time        `json:"first_seen"`
	LastSeen       time.Time        `json:"last_seen"`
	SeverityDist   map[string]int64 `json:"severity_distribution"`
}

// RootCauseInfo identifies the responsible service/operation behind an error chain.
type RootCauseInfo struct {
	Service      string `json:"service"`
	Operation    string `json:"operation"`
	ErrorMessage string `json:"error_message"`
	SpanID       string `json:"span_id"`
	TraceID      string `json:"trace_id"`
}

// AnomalyNode is an anomaly detected by the anomaly engine.
type AnomalyNode struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Severity  string    `json:"severity"`
	Service   string    `json:"service"`
	Evidence  string    `json:"evidence"`
	Timestamp time.Time `json:"timestamp"`
}

// AffectedEntry is a service affected by an upstream failure.
type AffectedEntry struct {
	Service     string  `json:"service"`
	Depth       int     `json:"depth"`
	CallCount   int64   `json:"call_count"`
	ImpactScore float64 `json:"impact_score"`
}

// ImpactResult describes the blast radius of a service failure.
type ImpactResult struct {
	Service          string          `json:"service"`
	AffectedServices []AffectedEntry `json:"affected_services"`
	TotalDownstream  int             `json:"total_downstream"`
}

// Investigation is the wire shape of an automated investigation record.
// The raw-JSON fields (CausalChain, TraceIDs, etc.) are passed through
// verbatim — they are already JSON on the wire.
type Investigation struct {
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	Status           string    `json:"status"`
	Severity         string    `json:"severity"`
	TriggerService   string    `json:"trigger_service"`
	TriggerOperation string    `json:"trigger_operation"`
	ErrorMessage     string    `json:"error_message"`
	RootService      string    `json:"root_service"`
	RootOperation    string    `json:"root_operation"`
	CausalChain      any       `json:"causal_chain"`
	TraceIDs         any       `json:"trace_ids"`
	ErrorLogs        any       `json:"error_logs"`
	AnomalousMetrics any       `json:"anomalous_metrics"`
	AffectedServices any       `json:"affected_services"`
	SpanChain        any       `json:"span_chain"`
}

// --- Conversion functions ---

// TraceFromModel converts a storage.Trace (with possibly-Preloaded children)
// into its wire-facing view.
func TraceFromModel(m storage.Trace) Trace {
	out := Trace{
		ID:          m.ID,
		TraceID:     m.TraceID,
		ServiceName: m.ServiceName,
		Operation:   m.Operation,
		Status:      m.Status,
		Duration:    m.Duration,
		DurationMs:  m.DurationMs,
		SpanCount:   m.SpanCount,
		Timestamp:   m.Timestamp,
	}
	if len(m.Spans) > 0 {
		out.Spans = SpansFromModels(m.Spans)
	}
	if len(m.Logs) > 0 {
		out.Logs = LogsFromModels(m.Logs)
	}
	return out
}

// TracesFromModels is the slice form of TraceFromModel.
func TracesFromModels(ms []storage.Trace) []Trace {
	out := make([]Trace, len(ms))
	for i, m := range ms {
		out[i] = TraceFromModel(m)
	}
	return out
}

// SpanFromModel converts a storage.Span into its view.
func SpanFromModel(m storage.Span) Span {
	return Span{
		ID:             m.ID,
		TraceID:        m.TraceID,
		SpanID:         m.SpanID,
		ParentSpanID:   m.ParentSpanID,
		OperationName:  m.OperationName,
		StartTime:      m.StartTime,
		EndTime:        m.EndTime,
		Duration:       m.Duration,
		ServiceName:    m.ServiceName,
		Status:         m.Status,
		AttributesJSON: string(m.AttributesJSON),
	}
}

// SpansFromModels is the slice form of SpanFromModel.
func SpansFromModels(ms []storage.Span) []Span {
	out := make([]Span, len(ms))
	for i, m := range ms {
		out[i] = SpanFromModel(m)
	}
	return out
}

// LogFromModel converts a storage.Log into its view.
func LogFromModel(m storage.Log) Log {
	return Log{
		ID:             m.ID,
		TraceID:        m.TraceID,
		SpanID:         m.SpanID,
		Severity:       m.Severity,
		Body:           m.Body,
		ServiceName:    m.ServiceName,
		AttributesJSON: string(m.AttributesJSON),
		AIInsight:      string(m.AIInsight),
		Timestamp:      m.Timestamp,
	}
}

// LogsFromModels is the slice form of LogFromModel.
func LogsFromModels(ms []storage.Log) []Log {
	out := make([]Log, len(ms))
	for i, m := range ms {
		out[i] = LogFromModel(m)
	}
	return out
}

// MetricBucketFromModel converts a storage.MetricBucket into its view.
func MetricBucketFromModel(m storage.MetricBucket) MetricBucket {
	return MetricBucket{
		ID:             m.ID,
		Name:           m.Name,
		ServiceName:    m.ServiceName,
		TimeBucket:     m.TimeBucket,
		Min:            m.Min,
		Max:            m.Max,
		Sum:            m.Sum,
		Count:          m.Count,
		AttributesJSON: string(m.AttributesJSON),
	}
}

// MetricBucketsFromModels is the slice form of MetricBucketFromModel.
func MetricBucketsFromModels(ms []storage.MetricBucket) []MetricBucket {
	out := make([]MetricBucket, len(ms))
	for i, m := range ms {
		out[i] = MetricBucketFromModel(m)
	}
	return out
}

// TracesResponseFromModel wraps a repo TracesResponse into the view form.
func TracesResponseFromModel(r *storage.TracesResponse) TracesResponse {
	if r == nil {
		return TracesResponse{Traces: []Trace{}}
	}
	return TracesResponse{
		Traces: TracesFromModels(r.Traces),
		Total:  r.Total,
		Limit:  r.Limit,
		Offset: r.Offset,
	}
}

// DashboardStatsFromModel converts repo stats into the view form.
func DashboardStatsFromModel(s *storage.DashboardStats) DashboardStats {
	if s == nil {
		return DashboardStats{}
	}
	out := DashboardStats{
		TotalTraces:    s.TotalTraces,
		TotalLogs:      s.TotalLogs,
		TotalErrors:    s.TotalErrors,
		AvgLatencyMs:   s.AvgLatencyMs,
		ErrorRate:      s.ErrorRate,
		ActiveServices: s.ActiveServices,
		// storage.P99Latency is microseconds (storage tests assert µs); convert
		// to milliseconds here so the API matches AvgLatencyMs and the field name.
		P99LatencyMs:      float64(s.P99Latency) / 1000.0,
		LatencyProvenance: s.LatencyProvenance,
	}
	if len(s.TopFailingServices) > 0 {
		out.TopFailingServices = make([]ServiceError, len(s.TopFailingServices))
		for i, se := range s.TopFailingServices {
			out.TopFailingServices[i] = ServiceError{
				ServiceName: se.ServiceName,
				ErrorCount:  se.ErrorCount,
				TotalCount:  se.TotalCount,
				ErrorRate:   se.ErrorRate,
			}
		}
	}
	return out
}

// DashboardStatsFromAggregate converts an engine dashboard query into the same
// view the legacy path produces, plus the additive coverage and accuracy
// metadata. Field names, units and structure are deliberately identical: a
// client cannot tell which mode served it except by reading the new fields.
func DashboardStatsFromAggregate(r *aggregate.DashboardResult) DashboardStats {
	if r == nil {
		return DashboardStats{}
	}
	acc := r.Accuracy
	provenance := r.LatencyProvenance
	out := DashboardStats{
		// The headline trio is the REQUEST basis: that is what a dashboard
		// labelled "traces" and "error rate" has always meant to a reader.
		TotalTraces:      r.RequestCount,
		TotalErrors:      r.ErrorRequestCount,
		ErrorRate:        r.RequestErrorRate,
		Requests:         r.RequestCount,
		RequestErrors:    r.ErrorRequestCount,
		RequestErrorRate: r.RequestErrorRate,
		Spans:            r.SpanCount,
		SpanErrors:       r.SpanErrorCount,
		SpanErrorRate:    r.SpanErrorRate,
		TotalLogs:        r.TotalLogs,
		AvgLatencyMs:     r.AvgLatencyMs,
		ActiveServices:   r.ActiveServices,
		// The engine reports microseconds, matching storage.P99Latency; the
		// view is milliseconds in both modes.
		P99LatencyMs:      r.P99LatencyMicros / 1000.0,
		LatencyProvenance: &provenance,
		Coverage:          string(r.Coverage),
		CoverageNote:      r.Coverage.Note(),
		Accuracy:          &acc,
		Epoch:             r.Epoch,
		Revision:          r.Revision,
	}
	if len(r.TopFailing) > 0 {
		out.TopFailingServices = make([]ServiceError, len(r.TopFailing))
		for i, s := range r.TopFailing {
			out.TopFailingServices[i] = ServiceError{
				ServiceName: s.Service,
				ErrorCount:  s.ErrorCount,
				TotalCount:  s.Count,
				ErrorRate:   s.ErrorRate,
			}
		}
	}
	return out
}

// ServiceMapMetricsFromAggregate converts an engine topology query into the
// topology view.
//
// Nodes AND edges come from the one result: they were read from one tenant,
// one range and one ownership snapshot, and the coverage the engine reported
// describes both. Nothing is supplemented from a second store here — that
// supplementation is #194 finding 15.
func ServiceMapMetricsFromAggregate(r *aggregate.TopologyResult) ServiceMapMetrics {
	if r == nil {
		return ServiceMapMetrics{Nodes: []ServiceMapNode{}, Edges: []ServiceMapEdge{}}
	}
	nodes := make([]ServiceMapNode, len(r.Nodes))
	for i, n := range r.Nodes {
		nodes[i] = ServiceMapNode{
			Name:              n.Service,
			TotalTraces:       n.Count,
			ErrorCount:        n.ErrorCount,
			AvgLatencyMs:      n.AvgLatencyMs,
			P99LatencyMs:      n.P99LatencyMicros / 1000.0,
			LatencyProvenance: &n.LatencyProvenance,
		}
	}
	edges := make([]ServiceMapEdge, len(r.Edges))
	for i, e := range r.Edges {
		edges[i] = ServiceMapEdge{
			Source:       e.Source,
			Target:       e.Target,
			CallCount:    e.CallCount,
			AvgLatencyMs: e.AvgLatencyMs,
			ErrorRate:    e.ErrorRate,
		}
	}
	return ServiceMapMetrics{
		Nodes:        nodes,
		Edges:        edges,
		Coverage:     string(r.Coverage),
		CoverageNote: r.Coverage.Note(),
		Epoch:        r.Epoch,
		Revision:     r.Revision,
	}
}

// ServiceMapMetricsFromTopology converts the mode-selected provider snapshot
// without exposing its owning package on the HTTP wire.
func ServiceMapMetricsFromTopology(snapshot topology.Snapshot) ServiceMapMetrics {
	nodes := make([]ServiceMapNode, len(snapshot.Nodes))
	for i, node := range snapshot.Nodes {
		nodes[i] = ServiceMapNode{
			Name:              node.Name,
			TotalTraces:       node.TotalTraces,
			ErrorCount:        node.ErrorCount,
			AvgLatencyMs:      node.AvgLatencyMs,
			P99LatencyMs:      node.P99LatencyMs,
			LatencyProvenance: node.LatencyProvenance,
			Kind:              node.Kind,
			HostCount:         node.HostCount,
			Hosts:             node.Hosts,
		}
	}
	edges := make([]ServiceMapEdge, len(snapshot.Edges))
	for i, edge := range snapshot.Edges {
		edges[i] = ServiceMapEdge{
			Source:       edge.Source,
			Target:       edge.Target,
			CallCount:    edge.CallCount,
			AvgLatencyMs: edge.AvgLatencyMs,
			ErrorRate:    edge.ErrorRate,
		}
	}
	out := ServiceMapMetrics{Nodes: nodes, Edges: edges}
	if snapshot.Meta.Source == topology.SourceAggregate {
		out.Source = string(snapshot.Meta.Source)
		out.Coverage = snapshot.Meta.Coverage
		out.CoverageNote = snapshot.Meta.CoverageNote
		out.Epoch = snapshot.Meta.Epoch
		out.Revision = snapshot.Meta.Revision
		out.Truncated = snapshot.Meta.Truncated
		out.DroppedServices = snapshot.Meta.DroppedServices
		out.DroppedOperations = snapshot.Meta.DroppedOperations
		out.DroppedEdges = snapshot.Meta.DroppedEdges
		out.DroppedMetrics = snapshot.Meta.DroppedMetrics
	}
	return out
}

// ServiceMapMetricsFromModel converts repo topology into the view form.
func ServiceMapMetricsFromModel(m *storage.ServiceMapMetrics) ServiceMapMetrics {
	if m == nil {
		return ServiceMapMetrics{Nodes: []ServiceMapNode{}, Edges: []ServiceMapEdge{}}
	}
	nodes := make([]ServiceMapNode, len(m.Nodes))
	for i, n := range m.Nodes {
		nodes[i] = ServiceMapNode{
			Name:              n.Name,
			TotalTraces:       n.TotalTraces,
			ErrorCount:        n.ErrorCount,
			AvgLatencyMs:      n.AvgLatencyMs,
			P99LatencyMs:      n.P99LatencyMs,
			LatencyProvenance: n.LatencyProvenance,
		}
	}
	edges := make([]ServiceMapEdge, len(m.Edges))
	for i, e := range m.Edges {
		edges[i] = ServiceMapEdge{
			Source:       e.Source,
			Target:       e.Target,
			CallCount:    e.CallCount,
			AvgLatencyMs: e.AvgLatencyMs,
			ErrorRate:    e.ErrorRate,
		}
	}
	return ServiceMapMetrics{Nodes: nodes, Edges: edges}
}

// LogClusterNodeFromModel converts a GraphRAG log cluster into its view.
func LogClusterNodeFromModel(n graphrag.LogClusterNode) LogClusterNode {
	return LogClusterNode{
		ID:             n.ID,
		Template:       n.Template,
		TemplateID:     n.TemplateID,
		TemplateTokens: n.TemplateTokens,
		SampleLog:      n.SampleLog,
		Count:          n.Count,
		FirstSeen:      n.FirstSeen,
		LastSeen:       n.LastSeen,
		SeverityDist:   n.SeverityDist,
	}
}

// RootCauseInfoFromModel converts a GraphRAG root-cause node into its view.
func RootCauseInfoFromModel(r *graphrag.RootCauseInfo) *RootCauseInfo {
	if r == nil {
		return nil
	}
	return &RootCauseInfo{
		Service:      r.Service,
		Operation:    r.Operation,
		ErrorMessage: r.ErrorMessage,
		SpanID:       r.SpanID,
		TraceID:      r.TraceID,
	}
}

// AnomalyNodeFromModel converts a GraphRAG anomaly node into its view.
func AnomalyNodeFromModel(a graphrag.AnomalyNode) AnomalyNode {
	return AnomalyNode{
		ID:        a.ID,
		Type:      string(a.Type),
		Severity:  string(a.Severity),
		Service:   a.Service,
		Evidence:  a.Evidence,
		Timestamp: a.Timestamp,
	}
}

// ImpactResultFromModel converts a GraphRAG impact result into its view.
func ImpactResultFromModel(r *graphrag.ImpactResult) *ImpactResult {
	if r == nil {
		return nil
	}
	affected := make([]AffectedEntry, len(r.AffectedServices))
	for i, a := range r.AffectedServices {
		affected[i] = AffectedEntry{
			Service:     a.Service,
			Depth:       a.Depth,
			CallCount:   a.CallCount,
			ImpactScore: a.ImpactScore,
		}
	}
	return &ImpactResult{
		Service:          r.Service,
		AffectedServices: affected,
		TotalDownstream:  r.TotalDownstream,
	}
}

// InvestigationFromModel converts a persisted GraphRAG Investigation into its
// view. The RawMessage fields are unwrapped to `any` so JSON output is the
// decoded structure, not a base64 blob.
func InvestigationFromModel(m graphrag.Investigation) Investigation {
	return Investigation{
		ID:               m.ID,
		CreatedAt:        m.CreatedAt,
		Status:           m.Status,
		Severity:         m.Severity,
		TriggerService:   m.TriggerService,
		TriggerOperation: m.TriggerOperation,
		ErrorMessage:     m.ErrorMessage,
		RootService:      m.RootService,
		RootOperation:    m.RootOperation,
		CausalChain:      rawToAny(m.CausalChain),
		TraceIDs:         rawToAny(m.TraceIDs),
		ErrorLogs:        rawToAny(m.ErrorLogs),
		AnomalousMetrics: rawToAny(m.AnomalousMetrics),
		AffectedServices: rawToAny(m.AffectedServices),
		SpanChain:        rawToAny(m.SpanChain),
	}
}

// InvestigationsFromModels is the slice form of InvestigationFromModel.
func InvestigationsFromModels(ms []graphrag.Investigation) []Investigation {
	out := make([]Investigation, len(ms))
	for i, m := range ms {
		out[i] = InvestigationFromModel(m)
	}
	return out
}
