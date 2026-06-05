// Response types and result-parsing helpers for the MCP Trial console.
//
// The transport (lib/mcpClient) returns the raw JSON-RPC `result` as an
// McpToolResult. Parsing that payload is the console's job, and the backend
// uses TWO content encodings (see internal/mcp/tools.go):
//
//   • GraphRAG tools (get_anomaly_timeline, get_service_map, get_service_health,
//     root_cause_analysis, impact_analysis, in-memory trace_graph) wrap their
//     JSON in content[0].text.
//   • search_logs and the trace_graph DB fallback wrap JSON in
//     content[0].resource.text (type: "resource").
//   • Tool-level failures set isError:true with the message in content[0].text
//     prefixed by "Error: " (unknown tool / not-init / response-too-large).
//
// extractToolText handles all three so callers never branch on content type.

import type { McpToolResult } from '../../lib/mcpClient';

// --- Backend GraphRAG result shapes (mirror internal/graphrag/schema.go) ---

/** Shared root-cause block returned by error-identifying tools. */
export interface RootCauseInfo {
  service: string;
  operation: string;
  error_message: string;
  span_id: string;
  trace_id: string;
}

export interface AnomalyNode {
  id: string;
  type: 'error_spike' | 'latency_spike' | 'metric_zscore' | string;
  severity: string;
  service: string;
  evidence: string;
  timestamp: string;
}

export interface ServiceNode {
  id: string;
  name: string;
  first_seen: string;
  last_seen: string;
  health_score: number;
  call_count: number;
  error_count: number;
  error_rate: number;
  avg_latency_ms: number;
}

export interface OperationNode {
  id?: string;
  name?: string;
  service?: string;
}

export interface GraphEdge {
  type: string;
  from_id: string;
  to_id: string;
  weight?: number;
  call_count?: number;
  error_rate?: number;
  avg_latency_ms?: number;
  updated_at?: string;
}

export interface ServiceMapEntry {
  service: ServiceNode | null;
  operations?: OperationNode[];
  calls_to?: GraphEdge[];
  called_by?: GraphEdge[];
}

export interface AffectedEntry {
  service: string;
  depth: number;
  call_count: number;
  impact_score: number;
}

export interface ImpactResult {
  service: string;
  affected_services: AffectedEntry[] | null;
  total_downstream: number;
}

export interface RankedCause {
  service: string;
  operation: string;
  score: number;
  evidence: string[] | null;
  anomalies?: AnomalyNode[] | null;
}

export interface SpanNode {
  id: string;
  trace_id: string;
  parent_span_id: string;
  service: string;
  operation: string;
  duration_ms: number;
  status_code: string;
  is_error: boolean;
  timestamp: string;
}

export interface LogSummary {
  id: number;
  timestamp: string;
  severity: string;
  service_name: string;
  body: string;
  trace_id?: string;
  span_id?: string;
}

export interface SearchLogsResult {
  total: number;
  page: number;
  limit: number;
  count: number;
  entries: LogSummary[] | null;
}

// --- Parsed-result envelope used by the UI ---

export interface ParsedResult {
  /** Whether the tool reported a tool-level failure (isError === true). */
  isError: boolean;
  /** The decoded JSON payload, or undefined when the text was not JSON. */
  payload: unknown;
  /** Raw text pulled out of content (pretty-printed JSON or an error string). */
  text: string;
  /** First root_cause block found anywhere in the payload, if any. */
  rootCause: RootCauseInfo | null;
}

/**
 * Pulls the textual payload out of a tool result regardless of which content
 * encoding the backend used (text vs resource). Returns '' when empty.
 */
export function extractToolText(result: McpToolResult): string {
  const first = result.content?.[0];
  if (!first) return '';
  if (typeof first.text === 'string' && first.text.length > 0) return first.text;
  const res = (first as { resource?: { text?: string } }).resource;
  if (res && typeof res.text === 'string') return res.text;
  return '';
}

/** Depth-limited search for a `root_cause` object inside an arbitrary payload. */
export function findRootCause(value: unknown, depth = 0): RootCauseInfo | null {
  if (depth > 6 || value === null || typeof value !== 'object') return null;
  if (Array.isArray(value)) {
    for (const item of value) {
      const found = findRootCause(item, depth + 1);
      if (found) return found;
    }
    return null;
  }
  const obj = value as Record<string, unknown>;
  const rc = obj.root_cause;
  if (rc && typeof rc === 'object' && 'service' in (rc as object)) {
    return rc as RootCauseInfo;
  }
  for (const key of Object.keys(obj)) {
    const found = findRootCause(obj[key], depth + 1);
    if (found) return found;
  }
  return null;
}

/** Decodes a raw tool result into the UI envelope. */
export function parseToolResult(result: McpToolResult): ParsedResult {
  const text = extractToolText(result);
  let payload: unknown;
  try {
    payload = text ? JSON.parse(text) : undefined;
  } catch {
    payload = undefined;
  }
  return {
    isError: result.isError === true,
    payload,
    text,
    rootCause: payload !== undefined ? findRootCause(payload) : null,
  };
}
