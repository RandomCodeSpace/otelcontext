// Auto-mirrored from Go backend types. Keep in sync with:
// - internal/storage/models.go              (Trace, Span, Log, MetricBucket)
// - internal/storage/trace_repo.go          (TracesResponse, ServiceMap*)
// - internal/storage/metrics_repo.go        (DashboardStats, ServiceError, TrafficPoint, LatencyPoint)
// - internal/graphrag/schema.go             (ServiceNode, OperationNode, LogClusterNode, etc.)
//
// Conventions:
// - Field names match Go `json:"..."` tags exactly (snake_case).
// - Go int/int64/uint → number (safe up to 2^53).
// - Go time.Time → string (RFC3339).
// - Omitempty Go fields → optional TS fields.
// - When adding a Go field, add the matching TS field here.

// ============================================================================
// internal/storage/models.go
// ============================================================================

/** storage.Trace — a complete distributed trace (GORM model). */
export interface Trace {
  id: number
  trace_id: string
  service_name: string
  /** microseconds */
  duration: number
  duration_ms: number
  span_count: number
  operation: string
  status: string
  /** RFC3339 */
  timestamp: string
  spans?: Span[]
  logs?: LogEntry[]
}

/** storage.Span — a single operation within a trace. */
export interface Span {
  id: number
  trace_id: string
  span_id: string
  parent_span_id: string
  operation_name: string
  /** RFC3339 */
  start_time: string
  /** RFC3339 */
  end_time: string
  /** microseconds */
  duration: number
  service_name: string
  /** OTLP status code, e.g. STATUS_CODE_ERROR */
  status: string
  /** compressed JSON string */
  attributes_json: string
}

/** storage.Log — a log entry (mirrors Go `Log`; UI keeps legacy `LogEntry` alias). */
export interface LogEntry {
  id: number
  trace_id: string
  span_id: string
  severity: string
  body: string
  service_name: string
  attributes_json: string
  ai_insight?: string
  /** RFC3339 */
  timestamp: string
}
/** Alias matching the Go type name. */
export type Log = LogEntry

/** storage.MetricBucket — aggregated metric over a time window. */
export interface MetricBucket {
  id: number
  name: string
  service_name: string
  /** RFC3339 */
  time_bucket: string
  min: number
  max: number
  sum: number
  count: number
  attributes_json: string
}

// ============================================================================
// internal/storage/trace_repo.go
// ============================================================================

/** storage.TracesResponse — paginated traces listing. */
export interface TracesResponse {
  traces: Trace[]
  total: number
  limit: number
  offset: number
}

/** storage.ServiceMapNode — a service node on the service map. */
export interface ServiceMapNode {
  name: string
  total_traces: number
  error_count: number
  avg_latency_ms: number
}

/** storage.ServiceMapEdge — a connection between two services. */
export interface ServiceMapEdge {
  source: string
  target: string
  call_count: number
  avg_latency_ms: number
  error_rate: number
}

/** storage.ServiceMapMetrics — full service topology with metrics. */
export interface ServiceMapMetrics {
  nodes: ServiceMapNode[]
  edges: ServiceMapEdge[]
}

// ============================================================================
// internal/storage/metrics_repo.go
// ============================================================================

/** storage.TrafficPoint — point on the traffic chart. */
export interface TrafficPoint {
  /** RFC3339 */
  timestamp: string
  count: number
  error_count: number
}

/** storage.LatencyPoint — point on the latency heatmap. */
export interface LatencyPoint {
  /** RFC3339 */
  timestamp: string
  /** microseconds */
  duration: number
}

/** storage.ServiceError — error counts per service. */
export interface ServiceError {
  service_name: string
  error_count: number
  total_count: number
  error_rate: number
}

/** storage.DashboardStats — aggregated dashboard metrics. */
export interface DashboardStats {
  total_traces: number
  total_logs: number
  total_errors: number
  avg_latency_ms: number
  error_rate: number
  active_services: number
  p99_latency_ms: number
  top_failing_services: ServiceError[]
}

// ============================================================================
// internal/storage/log_repo.go (request shape; not JSON-tagged server-side,
// but mirrored here for hook-level filters)
// ============================================================================

/** storage.LogFilter — log search request parameters (UI-side). */
export interface LogFilter {
  service_name?: string
  severity?: string
  search?: string
  trace_id?: string
  /** RFC3339 */
  start_time?: string
  /** RFC3339 */
  end_time?: string
  limit?: number
  offset?: number
}

/** GET /api/logs envelope (internal/api/log_handlers.go: {data, total}). */
export interface LogsResponse {
  data: LogEntry[]
  total: number
}

// ============================================================================
// internal/graphrag/schema.go
// ============================================================================

export type NodeType =
  | 'service'
  | 'operation'
  | 'trace'
  | 'span'
  | 'log_cluster'
  | 'metric'
  | 'anomaly'

export type EdgeType =
  | 'CALLS'
  | 'EXPOSES'
  | 'CONTAINS'
  | 'CHILD_OF'
  | 'EMITTED_BY'
  | 'LOGGED_DURING'
  | 'MEASURED_BY'
  | 'PRECEDED_BY'
  | 'TRIGGERED_BY'

export type AnomalySeverity = 'critical' | 'warning' | 'info'
export type AnomalyTypeName =
  | 'error_spike'
  | 'latency_spike'
  | 'metric_zscore'

/** graphrag.ServiceNode — aggregated service health. */
export interface GraphServiceNode {
  id: string
  name: string
  /** RFC3339 */
  first_seen: string
  /** RFC3339 */
  last_seen: string
  /** 0.0–1.0 */
  health_score: number
  call_count: number
  error_count: number
  error_rate: number
  avg_latency_ms: number
}

/** graphrag.OperationNode — endpoint/RPC within a service. */
export interface OperationNode {
  id: string
  service: string
  operation: string
  /** RFC3339 */
  first_seen: string
  /** RFC3339 */
  last_seen: string
  health_score: number
  call_count: number
  error_count: number
  error_rate: number
}

/** graphrag.TraceNode — a trace node in the in-memory graph. */
export interface TraceNode {
  id: string
  trace_id: string
  root_service: string
  /** RFC3339 */
  timestamp: string
  duration_ms?: number
  status?: string
}

/** graphrag.SpanNode — a span node in the in-memory graph. */
export interface SpanNode {
  id: string
  span_id: string
  trace_id: string
  service: string
  operation: string
  /** RFC3339 */
  timestamp: string
  duration_ms?: number
  status?: string
  error_message?: string
  parent_span_id?: string
}

/** graphrag.LogClusterNode — a clustered log template. */
export interface LogClusterNode {
  id: string
  template_id: string
  service: string
  severity: string
  template: string
  sample_count: number
  /** RFC3339 */
  last_seen: string
}

/** graphrag.MetricNode — a metric node in the in-memory graph. */
export interface MetricNode {
  id: string
  name: string
  service: string
  /** RFC3339 */
  last_seen: string
}

/** graphrag.AnomalyNode — a detected anomaly. */
export interface AnomalyNode {
  id: string
  type: AnomalyTypeName
  severity: AnomalySeverity
  service: string
  evidence: string
  /** RFC3339 */
  timestamp: string
}

/** graphrag.Edge — directed relationship between two graph nodes. */
export interface GraphEdge {
  type: EdgeType
  from_id: string
  to_id: string
  weight?: number
  call_count?: number
}

/** graphrag.RootCauseInfo — responsible service/operation. */
export interface RootCauseInfo {
  service: string
  operation: string
  error_message: string
  span_id: string
  trace_id: string
}

/** graphrag.AffectedEntry — service affected by upstream failure. */
export interface AffectedEntry {
  service: string
  depth: number
  call_count: number
  impact_score: number
}

/** graphrag.ImpactResult — blast radius of a service failure. */
export interface ImpactResult {
  service: string
  affected_services: AffectedEntry[]
  total_downstream: number
}

/** graphrag.RankedCause — probable root cause with evidence. */
export interface RankedCause {
  service: string
  operation: string
  score: number
  evidence: string[]
  error_chain?: SpanNode[]
  anomalies?: AnomalyNode[]
}

// ============================================================================
// UI-specific envelopes for the `/api/system/graph` endpoint.
// These mirror the graph_handler response shape — not the raw graphrag schema.
// ============================================================================

export interface SystemNodeMetrics {
  request_rate_rps: number
  error_rate: number
  avg_latency_ms: number
  p99_latency_ms: number
  span_count_1h: number
}

export interface SystemNode {
  id: string
  type: string
  health_score: number
  status: string
  metrics: SystemNodeMetrics
  alerts: string[]
}

export interface SystemEdge {
  source: string
  target: string
  call_count: number
  avg_latency_ms: number
  error_rate: number
  status: string
}

export interface SystemSummary {
  total_services: number
  healthy: number
  degraded: number
  critical: number
  overall_health_score: number
  total_error_rate: number
  avg_latency_ms: number
  uptime_seconds: number
}

export interface SystemGraphResponse {
  /** RFC3339 */
  timestamp: string
  system: SystemSummary
  nodes: SystemNode[]
  edges: SystemEdge[]
}

/** Storage-level stats returned by `/api/stats`. Shape is loose by design. */
export interface RepoStats {
  logCount?: number
  traceCount?: number
  serviceCount?: number
  errorCount?: number
  db_size_mb?: number
  [key: string]: unknown
}

// ============================================================================
// MCP — JSON-RPC 2.0 envelopes
// ============================================================================

export interface MCPTool {
  name: string
  description: string
  inputSchema?: {
    properties?: Record<string, { type?: string; description?: string }>
    required?: string[]
  }
}

export interface JsonRpcError {
  code: number
  message: string
  data?: unknown
}

/** Generic JSON-RPC 2.0 response envelope (MCP transport). */
export interface JsonRpcResponse<T = unknown> {
  jsonrpc: '2.0'
  id: number | string | null
  result?: T
  error?: JsonRpcError
}

/** MCP `tools/list` result. */
export interface McpToolsListResult {
  tools: MCPTool[]
}

/** MCP `tools/call` result: array of content blocks. */
export interface McpToolCallResult {
  content: Array<{ type: string; text?: string; [key: string]: unknown }>
  isError?: boolean
}

/** Union of common MCP result shapes we consume in the UI. */
export type McpResult = McpToolsListResult | McpToolCallResult | Record<string, unknown>
