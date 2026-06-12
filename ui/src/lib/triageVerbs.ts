// Query-option builders for the Inspector's MCP triage verbs ("Why" =
// root_cause_analysis, "Impact" = impact_analysis). One source of truth for
// the cache keys so the Inspector tabs and the command palette's prefetch
// land on the same entries: results cache per service for 5 minutes, making
// tab flips instant after the first run.

import { callMcpTool } from './mcpClient'
import { extractToolText, parseImpactResult, parseRankedCauses } from './mcpResults'
import type { ImpactResult, RankedCause } from '@/types/api'

/** Per-service result cache window (ms). */
export const TRIAGE_STALE_MS = 300_000
const TRIAGE_GC_MS = 600_000

interface QueryFnCtx {
  signal: AbortSignal
}

export interface TriageQueryOptions<T> {
  queryKey: readonly [string, string, string]
  queryFn: (ctx: QueryFnCtx) => Promise<T>
  staleTime: number
  gcTime: number
}

async function callTriageTool(
  tool: string,
  args: Record<string, unknown>,
  signal: AbortSignal,
) {
  const result = await callMcpTool(tool, args, { signal })
  if (result.isError) {
    throw new Error(extractToolText(result) || `${tool} failed`)
  }
  return result
}

/** `root_cause_analysis {service}` → ranked causes, best first. */
export function rootCauseQueryOptions(
  service: string,
): TriageQueryOptions<RankedCause[]> {
  return {
    queryKey: ['mcp', 'root_cause_analysis', service] as const,
    queryFn: async ({ signal }) =>
      parseRankedCauses(
        await callTriageTool('root_cause_analysis', { service }, signal),
      ),
    staleTime: TRIAGE_STALE_MS,
    gcTime: TRIAGE_GC_MS,
  }
}

/** `impact_analysis {service}` → blast radius. */
export function impactQueryOptions(
  service: string,
): TriageQueryOptions<ImpactResult> {
  return {
    queryKey: ['mcp', 'impact_analysis', service] as const,
    queryFn: async ({ signal }) => {
      const result = await callTriageTool('impact_analysis', { service }, signal)
      const impact = parseImpactResult(result)
      if (impact === null) {
        throw new Error('impact_analysis returned an unexpected payload')
      }
      return impact
    },
    staleTime: TRIAGE_STALE_MS,
    gcTime: TRIAGE_GC_MS,
  }
}
