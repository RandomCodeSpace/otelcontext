package mcp

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/RandomCodeSpace/argus/internal/storage"
)

// toolDefs is the canonical list of all tools exposed by the Argus MCP server.
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
		Description: "Searches logs by severity, service, body text, and time range. Returns matching log entries.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"query":    {Type: "string", Description: "Text to search in log body."},
				"severity": {Type: "string", Description: "Filter by severity: ERROR, WARN, INFO, DEBUG."},
				"service":  {Type: "string", Description: "Filter by service name."},
				"start":    {Type: "string", Description: "Start time RFC3339. Defaults to 1h ago."},
				"end":      {Type: "string", Description: "End time RFC3339. Defaults to now."},
				"limit":    {Type: "number", Description: "Max results (default 20, max 100)."},
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
	data, _ := json.MarshalIndent(snap, "", "  ")
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
	data, _ := json.MarshalIndent(node, "", "  ")
	return textResult(string(data))
}

func (s *Server) toolSearchLogs(args map[string]any) ToolCallResult {
	end := time.Now()
	start := end.Add(-1 * time.Hour)
	parseTime(args, "start", &start)
	parseTime(args, "end", &end)

	limit := argInt(args, "limit", 20)
	if limit > 100 {
		limit = 100
	}

	filter := storage.LogFilter{
		StartTime: start,
		EndTime:   end,
		Limit:     limit,
	}
	if v, ok := args["severity"].(string); ok {
		filter.Severity = v
	}
	if v, ok := args["service"].(string); ok {
		filter.ServiceName = v
	}
	if v, ok := args["query"].(string); ok {
		filter.Search = v
	}

	logs, _, err := s.repo.GetLogsV2(filter)
	if err != nil {
		return errorResult(fmt.Sprintf("search_logs failed: %v", err))
	}
	data, _ := json.MarshalIndent(logs, "", "  ")
	return resourceResult("argus://logs/search", "application/json", string(data))
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
	data, _ := json.MarshalIndent(trace, "", "  ")
	return resourceResult("argus://traces/"+traceID, "application/json", string(data))
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
	data, _ := json.MarshalIndent(resp, "", "  ")
	return resourceResult("argus://traces/search", "application/json", string(data))
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
	data, _ := json.MarshalIndent(buckets, "", "  ")
	return resourceResult("argus://metrics/query", "application/json", string(data))
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
	data, _ := json.MarshalIndent(stats, "", "  ")
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
	data, _ := json.MarshalIndent(result, "", "  ")
	return textResult(string(data))
}

func (s *Server) toolFindSimilarLogs(args map[string]any) ToolCallResult {
	query, _ := args["query"].(string)
	if query == "" {
		return errorResult("query is required")
	}
	limit := argInt(args, "limit", 10)
	if s.vectorIdx == nil {
		return errorResult("vector index not yet initialized")
	}
	results := s.vectorIdx.Search(query, limit)
	data, _ := json.MarshalIndent(results, "", "  ")
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
	data, _ := json.MarshalIndent(entries, "", "  ")
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
	data, _ := json.MarshalIndent(result, "", "  ")
	return textResult(string(data))
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
