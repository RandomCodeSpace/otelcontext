// Parsers for MCP tool results. The backend tools JSON-encode their payload
// inside result.content[0].text (json.MarshalIndent), with two quirks every
// parser here must tolerate:
//   - an empty Go slice marshals to the literal "null"
//   - error payloads can be plain "Error: ..." text
// search_logs-style tools return a resource content block instead of text.

import type { McpToolResult } from './mcpClient'
import type { AnomalyNode, ImpactResult, RankedCause } from '@/types/api'

/** First text payload of a tool result — text block or resource text. */
export function extractToolText(result: McpToolResult | null): string {
  if (!result?.content) return ''
  for (const item of result.content) {
    if (item.type === 'text' && typeof item.text === 'string') return item.text
    const resource = (item as { resource?: { text?: string } }).resource
    if (typeof resource?.text === 'string') return resource.text
  }
  return ''
}

function parseJson(result: McpToolResult | null): unknown {
  const text = extractToolText(result)
  if (!text) return null
  try {
    return JSON.parse(text) as unknown
  } catch {
    return null // non-JSON text (e.g. "Error: ..." that slipped past isError)
  }
}

/** Parse `root_cause_analysis` → RankedCause[], best cause first. */
export function parseRankedCauses(result: McpToolResult | null): RankedCause[] {
  const parsed = parseJson(result)
  if (!Array.isArray(parsed)) return []
  // The server sorts by score already — re-sort defensively (stable on ties).
  return [...(parsed as RankedCause[])].sort((a, b) => b.score - a.score)
}

/** Parse `impact_analysis` → ImpactResult, or null when unparseable. */
export function parseImpactResult(
  result: McpToolResult | null,
): ImpactResult | null {
  const parsed = parseJson(result)
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return null
  }
  const impact = parsed as ImpactResult
  return {
    ...impact,
    // Go marshals an empty slice as null — normalize for .map/.length use.
    affected_services: Array.isArray(impact.affected_services)
      ? impact.affected_services
      : [],
  }
}

/** Parse `get_anomaly_timeline` → AnomalyNode[]. */
export function parseAnomalies(result: McpToolResult | null): AnomalyNode[] {
  if (!result) return []
  // Structured fallback first (defensive — current server is text-only).
  const structured = (result as { anomalies?: unknown }).anomalies
  if (Array.isArray(structured)) return structured as AnomalyNode[]
  const parsed = parseJson(result)
  return Array.isArray(parsed) ? (parsed as AnomalyNode[]) : []
}
