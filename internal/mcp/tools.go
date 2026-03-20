package mcp

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/graphrag"
	"github.com/RandomCodeSpace/otelcontext/internal/storage"
)

// toolDefs is the canonical list of all tools exposed by the OtelContext MCP server.
var toolDefs = []Tool{
	{
		Name:        "get_system_graph",
		Description: "Returns the full service topology with health scores (0-1), error rates, latencies, and dependency edges. Use this to understand overall system health.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"time_range": {Type: "string", Description: "Lookback window, e.g. '1h', '30m'. Defaults to '1h'."},
			},
		},
	},
	{
		Name:        "get_service_health",
		Description: "Returns detailed health metrics for a specific service: error rate, latency percentiles, request rate, and active alerts.",
		InputSchema: InputSchema{
			Type:     "object",
			Required: []string{"service_name"},
			Properties: map[string]Property{
				"service_name": {Type: "string", Description: "The service name to query."},
			},
		},
	},
	{
		Name:        "search_logs",
		Description: "Searches log entries by severity, service, body text, trace ID, and time range. Returns id, timestamp, severity, service_name, body, trace_id. Default window is last 24h. Use severity=ERROR to find errors, query= for full-text search, trace_id= to correlate with a trace. Use page= for pagination.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"query":    {Type: "string", Description: "Full-text search in log body."},
				"severity": {Type: "string", Description: "Filter by severity level: ERROR, WARN, INFO, DEBUG."},
				"service":  {Type: "string", Description: "Filter by service name (exact match)."},
				"trace_id": {Type: "string", Description: "Filter logs belonging to a specific trace ID."},
				"start":    {Type: "string", Description: "Start time RFC3339. Defaults to 24h ago."},
				"end":      {Type: "string", Description: "End time RFC3339. Defaults to now."},
				"limit":    {Type: "number", Description: "Max results per page (default 50, max 200)."},
				"page":     {Type: "number", Description: "Page number for pagination (default 0)."},
			},
		},
	},
	{
		Name:        "tail_logs",
		Description: "Returns the N most recent log entries, optionally filtered by service and/or severity. No time range needed — fastest way to see what's happening right now.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"service":  {Type: "string", Description: "Filter by service name."},
				"severity": {Type: "string", Description: "Filter by severity: ERROR, WARN, INFO, DEBUG."},
				"limit":    {Type: "number", Description: "Number of recent entries to return (default 20, max 100)."},
			},
		},
	},
	{
		Name:        "get_trace",
		Description: "Returns full trace detail with all spans for a given trace ID.",
		InputSchema: InputSchema{
			Type:     "object",
			Required: []string{"trace_id"},
			Properties: map[string]Property{
				"trace_id": {Type: "string", Description: "The trace ID to retrieve."},
			},
		},
	},
	{
		Name:        "search_traces",
		Description: "Searches traces by service, status code, minimum duration, and time range.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"service":         {Type: "string", Description: "Filter by service name."},
				"status":          {Type: "string", Description: "Filter by status: OK, ERROR."},
				"min_duration_ms": {Type: "number", Description: "Minimum trace duration in ms."},
				"start":           {Type: "string", Description: "Start time RFC3339."},
				"end":             {Type: "string", Description: "End time RFC3339."},
				"limit":           {Type: "number", Description: "Max results (default 20, max 100)."},
			},
		},
	},
	{
		Name:        "get_metrics",
		Description: "Queries metric time series for a given metric name and optional service.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"name":    {Type: "string", Description: "Metric name to query."},
				"service": {Type: "string", Description: "Filter by service name."},
				"start":   {Type: "string", Description: "Start time RFC3339."},
				"end":     {Type: "string", Description: "End time RFC3339."},
			},
		},
	},
	{
		Name:        "get_dashboard_stats",
		Description: "Returns dashboard summary: total requests, error rate, avg latency, ingestion rate, and per-service breakdown.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"start": {Type: "string", Description: "Start time RFC3339. Defaults to 1h ago."},
				"end":   {Type: "string", Description: "End time RFC3339. Defaults to now."},
			},
		},
	},
	{
		Name:        "get_storage_status",
		Description: "Returns hot/cold storage sizes, DLQ size, last archival run, and database health.",
		InputSchema: InputSchema{Type: "object"},
	},
	{
		Name:        "find_similar_logs",
		Description: "Finds logs semantically similar to a query text using TF-IDF vector similarity. Useful for clustering errors and finding root causes.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"query": {Type: "string", Description: "Text query to find similar logs."},
				"limit": {Type: "number", Description: "Max results (default 10)."},
			},
		},
	},
	{
		Name:        "get_alerts",
		Description: "Returns active alerts and anomalies: services with high error rates, p99 latency spikes, and degraded health scores.",
		InputSchema: InputSchema{Type: "object"},
	},
	{
		Name:        "get_service_map",
		Description: "Returns the service topology with health scores, error rates, call counts, and dependency edges. Powered by the live GraphRAG.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"depth":   {Type: "number", Description: "Max traversal depth (default 3)."},
				"service": {Type: "string", Description: "Focus on a specific service and its neighbors."},
			},
		},
	},
	{
		Name:        "get_error_chains",
		Description: "Traces recent error spans upstream to identify root cause services. Returns span path, root cause service/operation, and correlated error logs.",
		InputSchema: InputSchema{
			Type:     "object",
			Required: []string{"service"},
			Properties: map[string]Property{
				"service":    {Type: "string", Description: "Service experiencing errors."},
				"time_range": {Type: "string", Description: "Lookback window, e.g. '5m', '1h'. Defaults to '15m'."},
				"limit":      {Type: "number", Description: "Max error chains to return (default 10)."},
			},
		},
	},
	{
		Name:        "trace_graph",
		Description: "Returns the full span tree for a trace with service names, durations, errors, and linked logs.",
		InputSchema: InputSchema{
			Type:     "object",
			Required: []string{"trace_id"},
			Properties: map[string]Property{
				"trace_id": {Type: "string", Description: "The trace ID to visualize."},
			},
		},
	},
	{
		Name:        "impact_analysis",
		Description: "BFS downstream from a service to find all affected services and impact scores.",
		InputSchema: InputSchema{
			Type:     "object",
			Required: []string{"service"},
			Properties: map[string]Property{
				"service": {Type: "string", Description: "Service to analyze blast radius for."},
				"depth":   {Type: "number", Description: "Max traversal depth (default 5)."},
			},
		},
	},
	{
		Name:        "root_cause_analysis",
		Description: "Ranked probable root causes with evidence: error chains, anomalous metrics, correlated logs.",
		InputSchema: InputSchema{
			Type:     "object",
			Required: []string{"service"},
			Properties: map[string]Property{
				"service":    {Type: "string", Description: "Service experiencing issues."},
				"time_range": {Type: "string", Description: "Lookback window. Defaults to '15m'."},
			},
		},
	},
	{
		Name:        "correlated_signals",
		Description: "All related signals for a service: error logs, metric anomalies, traces, and investigations.",
		InputSchema: InputSchema{
			Type:     "object",
			Required: []string{"service"},
			Properties: map[string]Property{
				"service":    {Type: "string", Description: "Service to gather signals for."},
				"time_range": {Type: "string", Description: "Lookback window. Defaults to '1h'."},
			},
		},
	},
	{
		Name:        "get_investigations",
		Description: "Lists persisted investigation records from automated error analysis.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"service":  {Type: "string", Description: "Filter by service."},
				"severity": {Type: "string", Description: "Filter: critical, warning, info."},
				"status":   {Type: "string", Description: "Filter: detected, triaged, resolved."},
				"limit":    {Type: "number", Description: "Max results (default 20)."},
			},
		},
	},
	{
		Name:        "get_investigation",
		Description: "Returns a full investigation record with causal chain, evidence, and affected services.",
		InputSchema: InputSchema{
			Type:     "object",
			Required: []string{"investigation_id"},
			Properties: map[string]Property{
				"investigation_id": {Type: "string", Description: "The investigation ID."},
			},
		},
	},
	{
		Name:        "get_graph_snapshot",
		Description: "Returns the historical service topology closest to the requested time.",
		InputSchema: InputSchema{
			Type:     "object",
			Required: []string{"time"},
			Properties: map[string]Property{
				"time": {Type: "string", Description: "RFC3339 timestamp to query the snapshot for."},
			},
		},
	},
	{
		Name:        "get_anomaly_timeline",
		Description: "Returns recent anomalies with temporal causal links, optionally filtered by service.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"since":   {Type: "string", Description: "Start time RFC3339. Defaults to 1h ago."},
				"service": {Type: "string", Description: "Filter by service."},
			},
		},
	},
	{
		Name:        "search_cold_archive",
		Description: "Searches archived data older than the hot retention window. Returns results with source: 'cold'.",
		InputSchema: InputSchema{
			Type:     "object",
			Required: []string{"type", "start", "end"},
			Properties: map[string]Property{
				"type":  {Type: "string", Description: "Data type: logs, traces, or metrics."},
				"start": {Type: "string", Description: "Start time RFC3339."},
				"end":   {Type: "string", Description: "End time RFC3339."},
				"query": {Type: "string", Description: "Optional text filter."},
			},
		},
	},
}

// toolHandler routes a tool call to its implementation and returns the result.
func (s *Server) toolHandler(name string, args map[string]any) ToolCallResult {
	switch name {
	case "get_system_graph":
		return s.toolGetSystemGraph(args)
	case "get_service_health":
		return s.toolGetServiceHealth(args)
	case "search_logs":
		return s.toolSearchLogs(args)
	case "tail_logs":
		return s.toolTailLogs(args)
	case "get_trace":
		return s.toolGetTrace(args)
	case "search_traces":
		return s.toolSearchTraces(args)
	case "get_metrics":
		return s.toolGetMetrics(args)
	case "get_dashboard_stats":
		return s.toolGetDashboardStats(args)
	case "get_storage_status":
		return s.toolGetStorageStatus()
	case "find_similar_logs":
		return s.toolFindSimilarLogs(args)
	case "get_alerts":
		return s.toolGetAlerts()
	case "get_service_map":
		return s.toolGetServiceMap(args)
	case "get_error_chains":
		return s.toolGetErrorChains(args)
	case "trace_graph":
		return s.toolTraceGraph(args)
	case "impact_analysis":
		return s.toolImpactAnalysis(args)
	case "root_cause_analysis":
		return s.toolRootCauseAnalysis(args)
	case "correlated_signals":
		return s.toolCorrelatedSignals(args)
	case "get_investigations":
		return s.toolGetInvestigations(args)
	case "get_investigation":
		return s.toolGetInvestigationByID(args)
	case "get_graph_snapshot":
		return s.toolGetGraphSnapshot(args)
	case "get_anomaly_timeline":
		return s.toolGetAnomalyTimeline(args)
	case "search_cold_archive":
		return s.toolSearchColdArchive(args)
	default:
		return errorResult(fmt.Sprintf("unknown tool: %s", name))
	}
}

// --- Tool implementations ---

func (s *Server) toolGetSystemGraph(_ map[string]any) ToolCallResult {
	if s.svcGraph == nil {
		return errorResult("service graph not yet initialized")
	}
	snap := s.svcGraph.Snapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal system graph: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolGetServiceHealth(args map[string]any) ToolCallResult {
	svcName, _ := args["service_name"].(string)
	if svcName == "" {
		return errorResult("service_name is required")
	}
	if s.svcGraph == nil {
		return errorResult("service graph not yet initialized")
	}
	snap := s.svcGraph.Snapshot()
	node, ok := snap.Nodes[svcName]
	if !ok {
		return textResult(fmt.Sprintf("service %q not found in the current graph window", svcName))
	}
	data, err := json.MarshalIndent(node, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal service health: %v", err))
	}
	return textResult(string(data))
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
			Body:        string(l.Body),
			TraceID:     l.TraceID,
			SpanID:      l.SpanID,
		}
	}
	return out
}

func (s *Server) toolSearchLogs(args map[string]any) ToolCallResult {
	end := time.Now()
	start := end.Add(-24 * time.Hour) // wider default window for AI agents
	parseTime(args, "start", &start)
	parseTime(args, "end", &end)

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

	logs, total, err := s.repo.GetLogsV2(filter)
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
	return resourceResult("OtelContext://logs/search", "application/json", string(data))
}

func (s *Server) toolTailLogs(args map[string]any) ToolCallResult {
	limit := argInt(args, "limit", 20)
	if limit > 100 {
		limit = 100
	}

	filter := storage.LogFilter{
		EndTime: time.Now(),
		Limit:   limit,
	}
	if v, ok := args["service"].(string); ok && v != "" {
		filter.ServiceName = v
	}
	if v, ok := args["severity"].(string); ok && v != "" {
		filter.Severity = v
	}

	logs, _, err := s.repo.GetLogsV2(filter)
	if err != nil {
		return errorResult(fmt.Sprintf("tail_logs failed: %v", err))
	}
	data, err := json.MarshalIndent(toLogSummaries(logs), "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal tail results: %v", err))
	}
	return resourceResult("OtelContext://logs/tail", "application/json", string(data))
}

func (s *Server) toolGetTrace(args map[string]any) ToolCallResult {
	traceID, _ := args["trace_id"].(string)
	if traceID == "" {
		return errorResult("trace_id is required")
	}
	trace, err := s.repo.GetTrace(traceID)
	if err != nil {
		return errorResult(fmt.Sprintf("get_trace failed: %v", err))
	}
	data, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal trace: %v", err))
	}
	return resourceResult("OtelContext://traces/"+traceID, "application/json", string(data))
}

func (s *Server) toolSearchTraces(args map[string]any) ToolCallResult {
	end := time.Now()
	start := end.Add(-1 * time.Hour)
	parseTime(args, "start", &start)
	parseTime(args, "end", &end)

	limit := argInt(args, "limit", 20)
	if limit > 100 {
		limit = 100
	}

	svcName, _ := args["service"].(string)
	status, _ := args["status"].(string)
	search := ""

	var services []string
	if svcName != "" {
		services = []string{svcName}
	}

	resp, err := s.repo.GetTracesFiltered(start, end, services, status, search, limit, 0, "timestamp", "desc")
	if err != nil {
		return errorResult(fmt.Sprintf("search_traces failed: %v", err))
	}
	data, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal trace search results: %v", err))
	}
	return resourceResult("OtelContext://traces/search", "application/json", string(data))
}

func (s *Server) toolGetMetrics(args map[string]any) ToolCallResult {
	end := time.Now()
	start := end.Add(-1 * time.Hour)
	parseTime(args, "start", &start)
	parseTime(args, "end", &end)

	metricName, _ := args["name"].(string)
	svcName, _ := args["service"].(string)

	buckets, err := s.repo.GetMetricBuckets(start, end, svcName, metricName)
	if err != nil {
		return errorResult(fmt.Sprintf("get_metrics failed: %v", err))
	}
	data, err := json.MarshalIndent(buckets, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal metrics: %v", err))
	}
	return resourceResult("OtelContext://metrics/query", "application/json", string(data))
}

func (s *Server) toolGetDashboardStats(args map[string]any) ToolCallResult {
	end := time.Now()
	start := end.Add(-1 * time.Hour)
	parseTime(args, "start", &start)
	parseTime(args, "end", &end)

	stats, err := s.repo.GetDashboardStats(start, end, nil)
	if err != nil {
		return errorResult(fmt.Sprintf("get_dashboard_stats failed: %v", err))
	}
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal dashboard stats: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolGetStorageStatus() ToolCallResult {
	health := s.metrics.GetHealthStats()
	result := map[string]any{
		"hot_db_size_mb":    float64(s.repo.HotDBSizeBytes()) / 1024 / 1024,
		"dlq_size_files":    health.DLQSize,
		"active_conns":      health.ActiveConns,
		"goroutines":        health.Goroutines,
		"heap_alloc_mb":     health.HeapAllocMB,
		"uptime_seconds":    health.UptimeSeconds,
		"ingestion_total":   health.IngestionRate,
		"db_latency_p99_ms": health.DBLatencyP99Ms,
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal storage status: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolFindSimilarLogs(args map[string]any) ToolCallResult {
	query, _ := args["query"].(string)
	if query == "" {
		return errorResult("query is required")
	}
	limit := argInt(args, "limit", 20)
	if limit > 100 {
		limit = 100
	}
	if s.vectorIdx == nil {
		return errorResult("vector index not yet initialized")
	}
	results := s.vectorIdx.Search(query, limit)
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal similar logs: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolGetAlerts() ToolCallResult {
	if s.svcGraph == nil {
		return errorResult("service graph not yet initialized")
	}
	snap := s.svcGraph.Snapshot()
	type alertEntry struct {
		Service string   `json:"service"`
		Status  string   `json:"status"`
		Score   float64  `json:"health_score"`
		Alerts  []string `json:"alerts"`
	}
	var entries []alertEntry
	for _, n := range snap.Nodes {
		if len(n.Alerts) > 0 || n.Status != "healthy" {
			entries = append(entries, alertEntry{
				Service: n.Name,
				Status:  n.Status,
				Score:   n.HealthScore,
				Alerts:  n.Alerts,
			})
		}
	}
	if len(entries) == 0 {
		return textResult("No active alerts. All services are healthy.")
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal alerts: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolSearchColdArchive(args map[string]any) ToolCallResult {
	dataType, _ := args["type"].(string)
	startStr, _ := args["start"].(string)
	endStr, _ := args["end"].(string)
	if dataType == "" || startStr == "" || endStr == "" {
		return errorResult("type, start, and end are required")
	}
	result := map[string]any{
		"source":  "cold",
		"type":    dataType,
		"start":   startStr,
		"end":     endStr,
		"message": "Cold archive data is available. Query the /api/archive/search endpoint for full streaming results.",
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal cold archive result: %v", err))
	}
	return textResult(string(data))
}

// --- GraphRAG Tool implementations ---

func (s *Server) toolGetServiceMap(args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult("GraphRAG not initialized")
	}
	depth := argInt(args, "depth", 3)
	result := s.graphRAG.ServiceMap(depth)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal service map: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolGetErrorChains(args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult("GraphRAG not initialized")
	}
	svcName, _ := args["service"].(string)
	if svcName == "" {
		return errorResult("service is required")
	}
	since := time.Now().Add(-15 * time.Minute)
	parseTimeRange(args, "time_range", &since)
	limit := argInt(args, "limit", 10)

	chains := s.graphRAG.ErrorChain(svcName, since, limit)
	data, err := json.MarshalIndent(chains, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal error chains: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolTraceGraph(args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult("GraphRAG not initialized")
	}
	traceID, _ := args["trace_id"].(string)
	if traceID == "" {
		return errorResult("trace_id is required")
	}
	spans := s.graphRAG.DependencyChain(traceID)
	if len(spans) == 0 {
		// Fallback to DB
		trace, err := s.repo.GetTrace(traceID)
		if err != nil {
			return errorResult(fmt.Sprintf("trace not found: %v", err))
		}
		data, err := json.MarshalIndent(trace, "", "  ")
		if err != nil {
			return errorResult(fmt.Sprintf("failed to marshal trace: %v", err))
		}
		return resourceResult("OtelContext://traces/"+traceID, "application/json", string(data))
	}
	data, err := json.MarshalIndent(spans, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal trace graph: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolImpactAnalysis(args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult("GraphRAG not initialized")
	}
	svcName, _ := args["service"].(string)
	if svcName == "" {
		return errorResult("service is required")
	}
	depth := argInt(args, "depth", 5)
	result := s.graphRAG.ImpactAnalysis(svcName, depth)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal impact analysis: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolRootCauseAnalysis(args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult("GraphRAG not initialized")
	}
	svcName, _ := args["service"].(string)
	if svcName == "" {
		return errorResult("service is required")
	}
	since := time.Now().Add(-15 * time.Minute)
	parseTimeRange(args, "time_range", &since)

	causes := s.graphRAG.RootCauseAnalysis(svcName, since)
	data, err := json.MarshalIndent(causes, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal root cause analysis: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolCorrelatedSignals(args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult("GraphRAG not initialized")
	}
	svcName, _ := args["service"].(string)
	if svcName == "" {
		return errorResult("service is required")
	}
	since := time.Now().Add(-1 * time.Hour)
	parseTimeRange(args, "time_range", &since)

	result := s.graphRAG.CorrelatedSignals(svcName, since)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal correlated signals: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolGetInvestigations(args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult("GraphRAG not initialized")
	}
	service, _ := args["service"].(string)
	severity, _ := args["severity"].(string)
	status, _ := args["status"].(string)
	limit := argInt(args, "limit", 20)

	investigations, err := s.graphRAG.GetInvestigations(service, severity, status, limit)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to query investigations: %v", err))
	}
	data, err := json.MarshalIndent(investigations, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal investigations: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolGetInvestigationByID(args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult("GraphRAG not initialized")
	}
	id, _ := args["investigation_id"].(string)
	if id == "" {
		return errorResult("investigation_id is required")
	}
	inv, err := s.graphRAG.GetInvestigation(id)
	if err != nil {
		return errorResult(fmt.Sprintf("investigation not found: %v", err))
	}
	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal investigation: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolGetGraphSnapshot(args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult("GraphRAG not initialized")
	}
	var at time.Time
	parseTime(args, "time", &at)
	if at.IsZero() {
		at = time.Now()
	}
	snap, err := s.graphRAG.GetGraphSnapshot(at)
	if err != nil {
		return errorResult(fmt.Sprintf("no snapshot found: %v", err))
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("failed to marshal snapshot: %v", err))
	}
	return textResult(string(data))
}

func (s *Server) toolGetAnomalyTimeline(args map[string]any) ToolCallResult {
	if s.graphRAG == nil {
		return errorResult("GraphRAG not initialized")
	}
	since := time.Now().Add(-1 * time.Hour)
	parseTime(args, "since", &since)
	service, _ := args["service"].(string)

	var anomalies []*graphrag.AnomalyNode
	if service != "" {
		anomalies = s.graphRAG.AnomalyStore.AnomaliesForService(service, since)
	} else {
		anomalies = s.graphRAG.AnomalyTimeline(since)
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

func textResult(text string) ToolCallResult {
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


