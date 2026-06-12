import { useCallback, useEffect, useRef, useState } from 'react'
import { callMcpTool, type McpToolResult } from '../lib/mcpClient'
import type { AnomalyNode } from '../types/api'

// Parse the MCP tool result into AnomalyNode[]. The backend's
// `get_anomaly_timeline` tool returns the anomaly slice JSON-encoded inside
// result.content[0].text (json.MarshalIndent of []*graphrag.AnomalyNode); an
// empty timeline marshals to the literal "null". We also tolerate a future
// structured shape where the array lands on result.anomalies directly.
export function parseAnomalies(result: McpToolResult | null): AnomalyNode[] {
  if (!result) return []
  // Structured fallback first (defensive — current server is text-only).
  const structured = (result as { anomalies?: unknown }).anomalies
  if (Array.isArray(structured)) return structured as AnomalyNode[]

  const text = result.content?.find((c) => c.type === 'text')?.text
  if (!text) return []
  try {
    const parsed = JSON.parse(text) as unknown
    return Array.isArray(parsed) ? (parsed as AnomalyNode[]) : []
  } catch {
    // Non-JSON text (e.g. an "Error: ..." payload that slipped past isError).
    return []
  }
}

// Recent-anomaly window + cap for the dashboard panel.
const WINDOW_MINUTES = 15
const MAX_ITEMS = 20

// Collapse repeated detections (the anomaly loop re-fires every ~10s, so the
// same service+type recurs) down to the newest occurrence, then return the 20
// most recent — keeps the "Recent anomalies" panel to 20 unique entries.
function dedupeRecent(list: AnomalyNode[]): AnomalyNode[] {
  const byKey = new Map<string, AnomalyNode>()
  for (const a of list) {
    const key = `${a.service}|${a.type}`
    const prev = byKey.get(key)
    if (!prev || a.timestamp > prev.timestamp) byKey.set(key, a)
  }
  return Array.from(byKey.values())
    .sort((x, y) => (x.timestamp < y.timestamp ? 1 : -1))
    .slice(0, MAX_ITEMS)
}

// Polls the MCP `get_anomaly_timeline` tool. Unlike the REST hooks this rides
// the JSON-RPC transport in lib/mcpClient (callMcpTool throws McpError on an
// RPC/HTTP failure), so we wrap the call and surface both transport errors and
// the tool's own isError flag.
export function useAnomalies(pollInterval = 30_000) {
  const [anomalies, setAnomalies] = useState<AnomalyNode[] | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const timerRef = useRef<ReturnType<typeof setInterval>>(undefined)

  const load = useCallback(async () => {
    try {
      const since = new Date(Date.now() - WINDOW_MINUTES * 60_000).toISOString()
      const result = await callMcpTool('get_anomaly_timeline', { since })
      if (result.isError) {
        const msg = result.content?.find((c) => c.type === 'text')?.text
        throw new Error(msg ?? 'anomaly tool error')
      }
      setAnomalies(dedupeRecent(parseAnomalies(result)))
      setError(null)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'fetch failed')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
    timerRef.current = setInterval(load, pollInterval)
    return () => clearInterval(timerRef.current)
  }, [load, pollInterval])

  return { anomalies, loading, error, reload: load }
}
