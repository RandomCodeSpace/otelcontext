import { useQuery } from '@tanstack/react-query'
import { callMcpTool } from '@/lib/mcpClient'
import { ANOMALY_WINDOW_MS } from '@/lib/triage'
import { parseAnomalies } from './useAnomalies'
import type { AnomalyNode } from '@/types/api'

export interface AnomalyTimeline {
  anomalies: AnomalyNode[]
  /** Epoch ms when the fetch ran — the strip's pure "now" for tick math. */
  fetchedAt: number
}

// 24h anomaly timeline via the MCP `get_anomaly_timeline` triage tool —
// the first MCP call the UI makes for humans. TanStack Query rides the
// shared client: visibility-aware polling, dedup, AbortSignal.
export function useAnomalyTimeline(pollInterval = 60_000) {
  return useQuery<AnomalyTimeline>({
    queryKey: ['anomaly-timeline'],
    queryFn: async ({ signal }) => {
      const now = Date.now()
      const since = new Date(now - ANOMALY_WINDOW_MS).toISOString()
      const result = await callMcpTool('get_anomaly_timeline', { since }, { signal })
      if (!result || result.isError) {
        const msg = result?.content?.find((c) => c.type === 'text')?.text
        throw new Error(msg ?? 'anomaly timeline tool error')
      }
      return { anomalies: parseAnomalies(result), fetchedAt: now }
    },
    refetchInterval: pollInterval > 0 ? pollInterval : false,
  })
}
