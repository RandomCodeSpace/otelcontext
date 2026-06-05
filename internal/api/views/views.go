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

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
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
type DashboardStats struct {
	TotalTraces        int64          `json:"total_traces"`
	TotalLogs          int64          `json:"total_logs"`
	TotalErrors        int64          `json:"total_errors"`
	AvgLatencyMs       float64        `json:"avg_latency_ms"`
	ErrorRate          float64        `json:"error_rate"`
	ActiveServices     int64          `json:"active_services"`
	P99LatencyMs       float64        `json:"p99_latency_ms"`
	TopFailingServices []ServiceError `json:"top_failing_services"`
}

// ServiceMapNode is a node on the service topology view.
type ServiceMapNode struct {
	Name         string  `json:"name"`
	TotalTraces  int64   `json:"total_traces"`
	ErrorCount   int64   `json:"error_count"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
}

// ServiceMapEdge is an edge on the service topology view.
type ServiceMapEdge struct {
	Source       string  `json:"source"`
	Target       string  `json:"target"`
	CallCount    int64   `json:"call_count"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	ErrorRate    float64 `json:"error_rate"`
}

// ServiceMapMetrics is the full topology view.
type ServiceMapMetrics struct {
	Nodes []ServiceMapNode `json:"nodes"`
	Edges []ServiceMapEdge `json:"edges"`
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
		P99LatencyMs: float64(s.P99Latency) / 1000.0,
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

// ServiceMapMetricsFromModel converts repo topology into the view form.
func ServiceMapMetricsFromModel(m *storage.ServiceMapMetrics) ServiceMapMetrics {
	if m == nil {
		return ServiceMapMetrics{Nodes: []ServiceMapNode{}, Edges: []ServiceMapEdge{}}
	}
	nodes := make([]ServiceMapNode, len(m.Nodes))
	for i, n := range m.Nodes {
		nodes[i] = ServiceMapNode{
			Name:         n.Name,
			TotalTraces:  n.TotalTraces,
			ErrorCount:   n.ErrorCount,
			AvgLatencyMs: n.AvgLatencyMs,
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
