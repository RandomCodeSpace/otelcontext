package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/httpconst"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

const (
	errGraphRAGNotInit = "GraphRAG not initialized"
	errServiceRequired = "service is required"
	resourceURIPrefix  = "OtelContext://"
)

// toolDefs is the canonical list of triage-essential tools exposed by the
// OtelContext MCP server. The surface was reduced from 21 to 7 in
// 2026-05-24 so the platform survives 120 services on SQLite — see
// docs/superpowers/specs/2026-05-24-mcp-7tool-sqlite-survival-design.md.
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
	mkTool("get_anomaly_timeline", "Returns recent anomalies with temporal causal links, optionally filtered by service. The triage entry point — answers \"what's wrong right now\".",
		param("since", "string", "Start time RFC3339. Defaults to 1h ago."),
		param("service", "string", "Filter by service."),
	),
	mkTool("get_service_map", "Returns the service topology with health scores, error rates, call counts, and dependency edges. Powered by the live GraphRAG.",
		param("depth", "number", "Max traversal depth (default 3)."),
		param("service", "string", "Focus on a specific service and its neighbors."),
	),
	mkTool("get_service_health", "Returns detailed health metrics for a specific service: error rate, latency percentiles, request rate, and active alerts.",
		required("service_name"),
		param("service_name", "string", "The service name to query."),
	),
	mkTool("root_cause_analysis", "Ranked probable root causes with evidence: error chains, anomalous metrics, correlated logs.",
		required("service"),
		param("service", "string", "Service experiencing issues."),
		param("time_range", "string", "Lookback window. Defaults to '15m'."),
	),
	mkTool("impact_analysis", "BFS downstream from a service to find all affected services and impact scores.",
		required("service"),
		param("service", "string", "Service to analyze blast radius for."),
		param("depth", "number", "Max traversal depth (default 5)."),
	),
	mkTool("trace_graph", "Returns the full span tree for a trace with service names, durations, errors, and linked logs.",
		required("trace_id"),
		param("trace_id", "string", "The trace ID to visualize."),
	),
	mkTool("search_logs", "Searches log entries by severity, service, body text, trace ID, and time range. Returns id, timestamp, severity, service_name, body, trace_id. **Limited to the last 24 hours** — windows entirely outside the 24h cap are rejected. Strongly recommend setting `service` and/or `severity` to scope the search; unscoped keyword queries scan large row counts when FTS5 is disabled. Use severity=ERROR to find errors, query= for full-text search, trace_id= to correlate with a trace. Use page= for pagination.",
		param("query", "string", "Full-text search in log body."),
		param("severity", "string", "Filter by severity level: ERROR, WARN, INFO, DEBUG."),
		param("service", "string", "Filter by service name (exact match)."),
		param("trace_id", "string", "Filter logs belonging to a specific trace ID."),
		param("start", "string", "Start time RFC3339. Defaults to 24h ago. Cannot be earlier than now-24h; older values are clamped."),
		param("end", "string", "End time RFC3339. Defaults to now. Cannot exceed now; future values are clamped."),
		param("limit", "number", "Max results per page (default 50, max 200)."),
		param("page", "number", "Page number for pagination (default 0)."),
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
	}
	if fn, ok := dispatch[name]; ok {
		return fn(ctx, args)
	}
	return errorResult(fmt.Sprintf("unknown tool: %s", name))
}

// --- Tool implementations ---

// toolGetServiceHealth returns the ServiceMap entry for svcName scoped to
// the tenant on ctx.
func (s *Server) toolGetServiceHealth(ctx context.Context, args map[string]any) ToolCallResult {
	svcName, _ := args["service_name"].(string)
	if svcName == "" {
		return errorResult("service_name is required")
	}
	if s.graphRAG == nil {
		return errorResult(errGraphRAGNotInit)
	}
	for _, entry := range s.graphRAG.ServiceMap(mcpCtx(ctx), 0) {
		if entry.Service != nil && entry.Service.Name == svcName {
			data, err := json.MarshalIndent(entry, "", "  ")
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

func (s *Server) toolSearchLogs(ctx context.Context, args map[string]any) ToolCallResult {
	var start, end time.Time
	parseTime(args, "start", &start)
	parseTime(args, "end", &end)

	// Enforce the 24h cap centrally so an MCP caller cannot bypass via the
	// alternate HTTP transport. clampTo24h handles defaults (zero values) and
	// returns a clean error when the window is entirely older than the cap.
	clampedStart, clampedEnd, capErr := storage.ClampSearchWindowTo24h(start, end, time.Now())
	if capErr != nil {
		return errorResult(capErr.Error())
	}
	start, end = clampedStart, clampedEnd

	limit := argInt(args, "limit", 50)
	if limit > 200 {
		limit = 200
	}
	page := argInt(args, "page", 0)

	filter := storage.LogFilter{
		StartTime: start,
		EndTime:   end,
		Limit:     limit,
		Offset:    page * limit,
	}
	if v, ok := args["severity"].(string); ok && v != "" {
		filter.Severity = v
	}
	if v, ok := args["service"].(string); ok && v != "" {
		filter.ServiceName = v
	}
	if v, ok := args["query"].(string); ok && v != "" {
		filter.Search = v
	}
	if v, ok := args["trace_id"].(string); ok && v != "" {
		filter.TraceID = v
	}

	logs, total, err := s.repo.GetLogsV2(mcpCtx(ctx), filter)
	if err != nil {
		return errorResult(fmt.Sprintf("search_logs failed: %v", err))
	}

	result := map[string]any{
		"total":   total,
		"page":    page,
		"limit":   limit,
		"count":   len(logs),
		"entries": toLogSummaries(logs),
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal search results: %v", err))
	}
	return resourceResult(resourceURIPrefix+"logs/search", httpconst.ContentTypeJSON, string(data))
}

// --- GraphRAG Tool implementations ---

func (s *Server) toolGetServiceMap(ctx context.Context, args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult(errGraphRAGNotInit)
	}
	depth := argInt(args, "depth", 3)
	result := s.graphRAG.ServiceMap(mcpCtx(ctx), depth)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal service map: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolTraceGraph(ctx context.Context, args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult(errGraphRAGNotInit)
	}
	traceID, _ := args["trace_id"].(string)
	if traceID == "" {
		return errorResult("trace_id is required")
	}
	spans := s.graphRAG.DependencyChain(mcpCtx(ctx), traceID)
	if len(spans) == 0 {
		// Fallback to DB
		trace, err := s.repo.GetTrace(mcpCtx(ctx), traceID)
		if err != nil {
			return errorResult(fmt.Sprintf("trace not found: %v", err))
		}
		data, err := json.MarshalIndent(trace, "", "  ")
		if err != nil {
			return errorResult(fmt.Sprintf("failed to marshal trace: %v", err))
		}
		return resourceResult(resourceURIPrefix+"traces/"+traceID, httpconst.ContentTypeJSON, string(data))
	}
	data, err := json.MarshalIndent(spans, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal trace graph: %v", err))
	}
	return textResult(string(data))
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
	data, err := json.MarshalIndent(result, "", "  ")
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
	since := time.Now().Add(-15 * time.Minute)
	parseTimeRange(args, "time_range", &since)

	causes := s.graphRAG.RootCauseAnalysis(mcpCtx(ctx), svcName, since)
	data, err := json.MarshalIndent(causes, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal root cause analysis: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolGetAnomalyTimeline(ctx context.Context, args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult(errGraphRAGNotInit)
	}
	since := time.Now().Add(-1 * time.Hour)
	parseTime(args, "since", &since)
	service, _ := args["service"].(string)

	var anomalies []*graphrag.AnomalyNode
	if service != "" {
		anomalies = s.graphRAG.AnomaliesForService(mcpCtx(ctx), service, since)
	} else {
		anomalies = s.graphRAG.AnomalyTimeline(mcpCtx(ctx), since)
	}
	data, err := json.MarshalIndent(anomalies, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal anomaly timeline: %v", err))
	}
	return textResult(string(data))
}

// parseTimeRange converts a duration-like string (e.g. "15m", "1h") to a since time.
func parseTimeRange(args map[string]any, key string, since *time.Time) {
	if v, ok := args[key].(string); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*since = time.Now().Add(-d)
		}
	}
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

func parseTime(args map[string]any, key string, dest *time.Time) {
	if v, ok := args[key].(string); ok && v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			*dest = t
		}
	}
}

func argInt(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok && v > 0 {
		return int(v)
	}
	return def
}
