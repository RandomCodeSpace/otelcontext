import { useCallback, useEffect, useRef, useState } from 'react'
import { callMcpTool } from '../lib/mcpClient'
import { parseAnomalies } from '../lib/mcpResults'
import type { AnomalyNode } from '../types/api'

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
