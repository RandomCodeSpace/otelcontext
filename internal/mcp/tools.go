package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/aggregate"
	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/RandomCodeSpace/otelcontext/internal/topology"
)

const (
	errGraphRAGNotInit = "GraphRAG not initialized"
	errServiceRequired = "service is required"
	resourceURIPrefix  = "OtelContext://"
)

// toolDefs is the canonical list of triage-essential tools exposed by the
// OtelContext MCP server. History reads are bounded by retention, page size, and the shared call budget.
// schemaOpt mutates an InputSchema being built by mkTool. Use param(...) and
// required(...) to compose schemas without re-typing the InputSchema /
// Properties scaffolding on every tool definition.
type schemaOpt func(*InputSchema)

// param adds a single Property to the schema. Type is "string" or "number".
func param(name, typ, desc string) schemaOpt {
	return func(s *InputSchema) {
		s.Properties[name] = Property{Type: typ, Description: desc}
	}
}

// required marks one or more parameter names as required by JSON-schema.
func required(fields ...string) schemaOpt {
	return func(s *InputSchema) { s.Required = append(s.Required, fields...) }
}

// mkTool builds a Tool with a freshly-initialised InputSchema. Centralising
// the InputSchema/Properties scaffolding here keeps the toolDefs list one
// call per tool and avoids the repeated struct-literal boilerplate that
// SonarCloud (rightly) flagged as duplication.
func mkTool(name, desc string, opts ...schemaOpt) Tool {
	s := InputSchema{Type: "object", Properties: map[string]Property{}}
	for _, opt := range opts {
		opt(&s)
	}
	return Tool{Name: name, Description: desc, InputSchema: s}
}

var toolDefs = []Tool{
	mkTool("get_anomaly_timeline", "Returns recent anomalies with temporal causal links, optionally filtered by service. The triage entry point — answers \"what's wrong right now\". In-memory detections cover at most 24 hours and may be lost on restart. Persisted investigations supplement this list within aggregate retention; neither is a complete history of anomalies.",
		param("since", "string", "Start time RFC3339. Defaults to 1h ago."),
		param("service", "string", "Filter by service."),
	),
	mkTool("get_service_map", "Returns the service topology with health scores, error rates, call counts, and dependency edges. In aggregate mode, unfiltered counts describe accepted telemetry within aggregate retention. Identities may be collapsed into __other__; coverage metadata reports the actual source, range and limits.",
		param("depth", "number", "Max traversal depth (default 3)."),
		param("service", "string", "Focus on a specific service and its neighbors."),
		param("start", "string", "Optional RFC3339 range start; defaults to 1h before end when end is supplied."),
		param("end", "string", "Optional RFC3339 range end; defaults to now when start is supplied."),
		param("group_by", "string", "Set to \"host\" to wrap the map as {services, hosts}: `hosts` lists the tenant's hosts, each with its bounded, sorted service list. Omitted: the bare service array, unchanged."),
	),
	mkTool("get_service_health", "Returns detailed health metrics for a specific service: error rate, latency, request counts and dependency edges. Optional start/end read disk history within aggregate retention. Coverage reports the actual range and source. Returns an explicit \"not found in the current tenant window\" message rather than an empty guess.",
		required("service_name"),
		param("service_name", "string", "The service name to query."),
		param("start", "string", "Optional RFC3339 range start."),
		param("end", "string", "Optional RFC3339 range end."),
	),
	mkTool("root_cause_analysis", "Ranked probable root causes with evidence: error chains, anomalous metrics, correlated logs. Each ranked cause carries a `coverage` block: `source=retained_exemplar` means a complete retained error chain backs it, `partial_exemplar` means the upstream walk stopped at a span that was not retained (the named service is NOT proven to be the root), and `not_retained_or_not_found` means the ranking rests on aggregate anomaly evidence with no chain behind it. Errors are always eligible for retention but never all retained — treat a partial cause as a lead, not a conclusion.",
		required("service"),
		param("service", "string", "Service experiencing issues."),
		param("time_range", "string", "Lookback window. Defaults to '15m'."),
	),
	mkTool("impact_analysis", "BFS downstream from a service using the current in-memory topology. This does not scan aggregate retention; use get_service_map with start/end for historical dependencies.",
		required("service"),
		param("service", "string", "Service to analyze blast radius for."),
		param("depth", "number", "Max traversal depth (default 5)."),
	),
	mkTool("trace_graph", "Returns the retained span tree for a trace with service names, durations and errors, plus a `coverage` block stating what it covers. `coverage.source=retained_exemplar` with `complete=true` is a complete retained trace; `partial_exemplar` means spans reference parents that were not retained (per-trace span/byte bound or TTL) and `retained_spans`/`observed_spans` quantify the gap; `not_retained_or_not_found` means no exemplar exists for this trace — that is a definitive answer, not an error, and the trace should not be assumed to have been error-free. Only a fraction of healthy traces are retained by design; errors are always eligible but capped.",
		required("trace_id"),
		param("trace_id", "string", "The trace ID to visualize."),
	),
	mkTool("search_logs", "Searches log entries by severity, service, body text, trace ID, and time range. Returns id, timestamp, severity, service_name, body, trace_id. Searches the configured raw retention, defaulting to the most recent 24 hours. Coverage reports requested and available ranges. Raw exemplars are sampled in aggregate mode. Strongly recommend setting `service` and/or `severity` to scope the search; unscoped keyword queries scan large row counts when FTS5 is disabled. Use severity=ERROR to find errors, query= for full-text search, trace_id= to correlate with a trace. Use page= for pagination.",
		param("query", "string", "Full-text search in log body. SQLite uses stemmed token-prefix matching and BM25 ranking when FTS5 is enabled; use trace_id for exact trace lookup."),
		param("severity", "string", "Filter by severity level: ERROR, WARN, INFO, DEBUG."),
		param("service", "string", "Filter by service name (exact match)."),
		param("trace_id", "string", "Filter logs belonging to a specific trace ID."),
		param("start", "string", "Start time RFC3339. Defaults to 24h before end. Clamped to configured raw retention with explicit coverage."),
		param("end", "string", "End time RFC3339. Defaults to now. Cannot exceed now; future values are clamped."),
		param("limit", "number", "Max results per page (default 50, max 200)."),
		param("page", "number", "Page number for pagination (default 0); offset is capped at 10000."),
	),
	mkTool("promql_query", "Evaluate PromQL over ingested application and host metrics. Returns the unmodified Prometheus JSON envelope, including labels, timestamps, result type, warnings and errors. This is separate from the internal self-monitoring scrape.",
		required("query"), param("query", "string", `PromQL expression, maximum 16384 bytes. Native OTLP NoTranslation preserves dots: {__name__="system.cpu.utilization","host.name"="host-a"}. Standard PromQL operators and functions apply.`),
		param("time", "string", "Optional evaluation time, RFC3339 or Unix seconds. Defaults to now."),
		param("timeout", "string", "Optional query timeout, maximum 30s."),
		param("limit", "number", "Maximum returned series, 1 to 1000."),
	),
	mkTool("promql_query_range", "Evaluate PromQL over ingested metrics for a bounded range up to seven days. Preserves the Prometheus JSON envelope and warnings. At most 11000 evaluation points per series; backend limits also apply.",
		required("query", "start", "end", "step"), param("query", "string", `PromQL expression, maximum 16384 bytes. Native OTLP NoTranslation preserves dots: {__name__="system.cpu.utilization","host.name"="host-a"}. Standard PromQL operators and functions apply.`),
		param("start", "string", "RFC3339 or Unix seconds, inclusive."), param("end", "string", "RFC3339 or Unix seconds, inclusive."),
		param("step", "string", "Positive duration such as 5m, or seconds."),
		param("timeout", "string", "Optional timeout, maximum 30s."), param("limit", "number", "Maximum returned series, 1 to 1000."),
	),
	mkTool("get_log_summary", "Accepted log counts from aggregate buckets across retained history, including logs whose raw bodies were not sampled. Five-minute resolution; no raw text or severity breakdown. Coverage and actual bucket bounds accompany results.",
		param("service", "string", "Optional exact service name."), param("start", "string", "RFC3339 start; defaults to 24h before end."), param("end", "string", "RFC3339 end; defaults to now."),
	),
}

// mcpCtx returns a tenant-scoped context for repository calls. If the caller's
// ctx already carries a tenant (set by the MCP transport from X-Tenant-ID), it
// is reused as-is; otherwise the server's default tenant is applied.
//
// Handlers invoked from the HTTP transport should always pass r.Context() in
// (via toolHandler), which keeps tenant scoping end-to-end and makes cross-
// tenant reads impossible through MCP.
func mcpCtx(ctx context.Context) context.Context {
	if ctx == nil {
		return storage.WithTenantContext(context.Background(), storage.DefaultTenantID)
	}
	if storage.HasTenantContext(ctx) {
		return ctx
	}
	return storage.WithTenantContext(ctx, storage.DefaultTenantID)
}

// toolHandler routes a tool call to its implementation and returns the result.
// ctx carries the tenant resolved from the MCP transport layer.
func (s *Server) toolHandler(ctx context.Context, name string, args map[string]any) (res ToolCallResult) {
	// Fix 6: emit OtelContext_mcp_tool_invocations_total{tool,status}. IsError
	// on the returned result drives the status label.
	defer func() {
		if s == nil || s.metrics == nil || s.metrics.MCPToolInvocationsTotal == nil {
			return
		}
		status := "ok"
		if res.IsError {
			status = "error"
		}
		s.metrics.MCPToolInvocationsTotal.WithLabelValues(name, status).Inc()
	}()
	// Map dispatch: the name -> handler binding is the single source of truth
	// for which tools the surface exposes. Adding a new tool means one entry
	// in this map plus a definition in toolDefs, nothing else.
	dispatch := map[string]func(context.Context, map[string]any) ToolCallResult{
		"get_anomaly_timeline": s.toolGetAnomalyTimeline,
		"get_service_map":      s.toolGetServiceMap,
		"get_service_health":   s.toolGetServiceHealth,
		"root_cause_analysis":  s.toolRootCauseAnalysis,
		"impact_analysis":      s.toolImpactAnalysis,
		"trace_graph":          s.toolTraceGraph,
		"search_logs":          s.toolSearchLogs,
		"promql_query":         s.toolPromQLQuery,
		"promql_query_range":   s.toolPromQLRange,
		"get_log_summary":      s.toolLogSummary,
	}
	if fn, ok := dispatch[name]; ok {
		result := fn(ctx, args)
		if name == "get_service_map" || name == "get_service_health" {
			return result
		}
		return s.withCoverage(result, toolCoverage[name])
	}
	return errorResult(fmt.Sprintf("unknown tool: %s", name))
}

// toolCoverage records what each tool's numbers are derived from once the
// aggregate read path is live (#164). Raw-backed tools read tables that hold
// only retained exemplars; the GraphRAG-backed tools are built from those same
// exemplars plus complete log-template mining, which is "sampled", not "full".
//
// In legacy and shadow modes this map is not consulted at all.
var toolCoverage = map[string]aggregate.Coverage{
	"get_anomaly_timeline": aggregate.CoverageSampled,
	"get_service_map":      aggregate.CoverageSampled,
	"get_service_health":   aggregate.CoverageSampled,
	"root_cause_analysis":  aggregate.CoverageSampled,
	"impact_analysis":      aggregate.CoverageSampled,
	"trace_graph":          aggregate.CoverageExemplar,
	"search_logs":          aggregate.CoverageExemplar,
	"promql_query":         "", // Backend envelope owns metric coverage and warnings.
	"promql_query_range":   "",
	"get_log_summary":      "",
}

// coverageMeta is the body metadata appended to every successful tool result
// in aggregate mode.
type coverageMeta struct {
	Coverage  string `json:"coverage"`
	Note      string `json:"coverage_note,omitempty"`
	Source    string `json:"source,omitempty"`
	Epoch     string `json:"epoch,omitempty"`
	Revision  uint64 `json:"revision,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`

	DroppedServices   uint64 `json:"dropped_services,omitempty"`
	DroppedOperations uint64 `json:"dropped_operations,omitempty"`
	DroppedEdges      uint64 `json:"dropped_edges,omitempty"`
	DroppedMetrics    uint64 `json:"dropped_metrics,omitempty"`
}

// withCoverage appends coverage metadata as an ADDITIONAL content item.
//
// Most tool bodies are bare JSON arrays; wrapping one in an envelope to carry a
// "coverage" field would silently break every client parsing it. An extra
// content item is additive — a client that ignores it sees exactly the body it
// saw before, and a client that reads it learns that a missing exemplar is not
// evidence of absence.
func (s *Server) withCoverage(res ToolCallResult, c aggregate.Coverage) ToolCallResult {
	if s == nil || !s.aggregateMode || res.IsError || c == "" {
		return res
	}
	data, err := json.Marshal(coverageMeta{Coverage: string(c), Note: c.Note()})
	if err != nil {
		return res
	}
	res.Content = append(res.Content, ContentItem{Type: "text", Text: string(data)})
	return res
}

// --- Tool implementations ---

// toolGetServiceHealth returns the ServiceMap entry for svcName scoped to
// the tenant on ctx.
func (s *Server) toolGetServiceHealth(ctx context.Context, args map[string]any) ToolCallResult {
	if s.useTopologySnapshot(args) {
		return s.topologyTool(ctx, args, true)
	}
	svcName, _ := args["service_name"].(string)
	if svcName == "" {
		return errorResult("service_name is required")
	}
	if s.graphRAG == nil {
		return errorResult(errGraphRAGNotInit)
	}
	for _, entry := range s.graphRAG.ServiceMap(mcpCtx(ctx), 0) {
		if entry.Service != nil && entry.Service.Name == svcName {
			_, hosts := s.hosts(ctx).ServiceHosts(svcName)
			data, err := json.Marshal(serviceHealthResult{ServiceMapEntry: entry, Hosts: hosts})
			if err != nil {
				return errorResult(fmt.Sprintf("failed to marshal service health: %v", err))
			}
			return textResult(string(data))
		}
	}
	return textResult(fmt.Sprintf("service %q not found in the current tenant window", svcName))
}

// logSummary is a lean projection of storage.Log for AI consumption.
// It strips compressed/binary fields and only returns what an AI agent needs.
type logSummary struct {
	ID          uint      `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	Severity    string    `json:"severity"`
	ServiceName string    `json:"service_name"`
	Body        string    `json:"body"`
	TraceID     string    `json:"trace_id,omitempty"`
	SpanID      string    `json:"span_id,omitempty"`
}

func toLogSummaries(logs []storage.Log) []logSummary {
	out := make([]logSummary, len(logs))
	for i, l := range logs {
		out[i] = logSummary{
			ID:          l.ID,
			Timestamp:   l.Timestamp,
			Severity:    l.Severity,
			ServiceName: l.ServiceName,
			Body:        l.Body,
			TraceID:     l.TraceID,
			SpanID:      l.SpanID,
		}
	}
	return out
}

// --- GraphRAG Tool implementations ---

func (s *Server) toolGetServiceMap(ctx context.Context, args map[string]any) ToolCallResult {
	if s.useTopologySnapshot(args) {
		return s.topologyTool(ctx, args, false)
	}
	if s.graphRAG == nil {
		return errorResult(errGraphRAGNotInit)
	}
	depth := argInt(args, "depth", 3)
	// When a focus service is supplied, return only the subgraph reachable within
	// `depth` hops (bounds compute + payload at 100–200 services). With no focus,
	// return the full topology so existing clients are unaffected.
	var result []graphrag.ServiceMapEntry
	if svc, _ := args["service"].(string); svc != "" {
		result = s.graphRAG.ServiceMapAround(mcpCtx(ctx), svc, depth)
	} else {
		result = s.graphRAG.ServiceMap(mcpCtx(ctx), depth)
	}
	// group_by=host (#288) wraps the unchanged array with the tenant's hosts.
	var payload any = result
	switch groupBy, _ := args["group_by"].(string); groupBy {
	case "":
	case "host":
		payload = serviceMapByHost{Services: result, Hosts: s.hosts(ctx).Hosts}
	default:
		return errorResult(fmt.Sprintf("unsupported group_by %q (only \"host\")", groupBy))
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal service map: %v", err))
	}
	return textResult(string(data))
}

// serviceMapByHost is the get_service_map payload under group_by=host.
type serviceMapByHost struct {
	Services []graphrag.ServiceMapEntry `json:"services"`
	Hosts    []topology.Host            `json:"hosts"`
}

// serviceHealthResult is the get_service_health payload: the entry as
// before, plus the hosts the service was observed on.
type serviceHealthResult struct {
	graphrag.ServiceMapEntry
	Hosts []string `json:"hosts"`
}

// hosts reads the tenant's host projection through the topology owner.
func (s *Server) hosts(ctx context.Context) topology.HostProjection {
	if s.topology == nil {
		return topology.ProjectHosts(nil)
	}
	return s.topology.Hosts(mcpCtx(ctx))
}

func (s *Server) toolImpactAnalysis(ctx context.Context, args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult(errGraphRAGNotInit)
	}
	svcName, _ := args["service"].(string)
	if svcName == "" {
		return errorResult(errServiceRequired)
	}
	depth := argInt(args, "depth", 5)
	result := s.graphRAG.ImpactAnalysis(mcpCtx(ctx), svcName, depth)
	data, err := json.Marshal(result)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal impact analysis: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolRootCauseAnalysis(ctx context.Context, args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult(errGraphRAGNotInit)
	}
	svcName, _ := args["service"].(string)
	if svcName == "" {
		return errorResult(errServiceRequired)
	}
	now := time.Now().UTC()
	lookback := 15 * time.Minute
	if raw, ok := args["time_range"]; ok {
		v, ok := raw.(string)
		if !ok {
			return errorResult("time_range must be a duration")
		}
		var err error
		lookback, err = time.ParseDuration(v)
		if err != nil || lookback <= 0 || lookback > s.historyRetention {
			return errorResult("time_range must be positive and within configured history retention")
		}
	}
	since := now.Add(-lookback)

	causes := s.graphRAG.RootCauseAnalysis(mcpCtx(ctx), svcName, since)
	data, err := json.Marshal(causes)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal root cause analysis: %v", err))
	}
	return s.investigationHistory(ctx, textResult(string(data)), svcName, since, now)
}

func (s *Server) toolGetAnomalyTimeline(ctx context.Context, args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult(errGraphRAGNotInit)
	}
	now := time.Now().UTC()
	since, err := strictTime(args, "since")
	if err != nil {
		return errorResult(err.Error())
	}
	if since.IsZero() {
		since = now.Add(-time.Hour)
	}
	if !since.Before(now) || now.Sub(since) > s.historyRetention {
		return errorResult("since must be within configured history retention and before now")
	}
	service, _ := args["service"].(string)

	var anomalies []*graphrag.AnomalyNode
	if service != "" {
		anomalies = s.graphRAG.AnomaliesForService(mcpCtx(ctx), service, since)
	} else {
		anomalies = s.graphRAG.AnomalyTimeline(mcpCtx(ctx), since)
	}
	data, err := json.Marshal(anomalies)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal anomaly timeline: %v", err))
	}
	return s.investigationHistory(ctx, textResult(string(data)), service, since, now)
}

// --- Helpers ---

// MaxToolResponseBytes caps the rendered length of any tool response. Without
// this, large in-memory GraphRAG dumps can produce 100MB+ JSON on adversarial
// input, OOM the process, and stall every concurrent MCP call until
// MCP_CALL_TIMEOUT_MS fires.
//
// The cap is intentionally set well above any legitimate row-capped tool
// response (search_logs at 200 rows is typically <1 MB) so it triggers only
// on pathological cases. Operators hitting it should narrow their query
// time range or use pagination.
const MaxToolResponseBytes = 4 * 1024 * 1024

// textResult wraps a successful tool response. Inputs over MaxToolResponseBytes
// are converted to a structured error so callers see a clear failure mode
// instead of a hung connection.
func textResult(text string) ToolCallResult {
	if len(text) > MaxToolResponseBytes {
		return errorResult(fmt.Sprintf(
			"response too large: %d bytes exceeds %d-byte cap; narrow time range or use pagination",
			len(text), MaxToolResponseBytes,
		))
	}
	return ToolCallResult{
		Content: []ContentItem{{Type: "text", Text: text}},
	}
}

func resourceResult(uri, mimeType, text string) ToolCallResult {
	if len(text) > MaxToolResponseBytes {
		return errorResult(fmt.Sprintf(
			"response too large: %d bytes exceeds %d-byte cap; narrow time range or use pagination",
			len(text), MaxToolResponseBytes,
		))
	}
	return ToolCallResult{
		Content: []ContentItem{
			{Type: "resource", Resource: &Resource{URI: uri, MimeType: mimeType, Text: text}},
		},
	}
}

func errorResult(msg string) ToolCallResult {
	return ToolCallResult{
		IsError: true,
		Content: []ContentItem{{Type: "text", Text: "Error: " + msg}},
	}
}

func argInt(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok && v > 0 {
		return int(v)
	}
	return def
}
