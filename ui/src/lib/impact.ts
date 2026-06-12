// Blast-radius helpers shared by the Inspector "Impact" tab and the flow
// map's ?impact= cone overlay. Pure functions — the map tint walks the
// already-loaded system graph client-side (same BFS the backend's
// impact_analysis performs), so /map needs no extra RPC.

import type { GraphEdgeRef } from './dagLayout'
import type { AffectedEntry } from '@/types/api'

/** Matches the impact_analysis tool's default traversal depth. */
export const IMPACT_MAX_DEPTH = 5

/**
 * BFS downstream from `root` over CALLS edges: service → min depth.
 * The root is included at depth 0; services outside the cone are absent.
 */
export function downstreamDepths(
  edges: readonly GraphEdgeRef[],
  root: string,
  maxDepth = IMPACT_MAX_DEPTH,
): Map<string, number> {
  const out = new Map<string, string[]>()
  for (const e of edges) {
    const list = out.get(e.source)
    if (list) list.push(e.target)
    else out.set(e.source, [e.target])
  }
  const depths = new Map<string, number>([[root, 0]])
  let frontier = [root]
  for (let depth = 1; depth <= maxDepth && frontier.length > 0; depth++) {
    const next: string[] = []
    for (const id of frontier) {
      for (const target of out.get(id) ?? []) {
        if (depths.has(target)) continue
        depths.set(target, depth)
        next.push(target)
      }
    }
    frontier = next
  }
  return depths
}

/**
 * Tint strength for a cone node: --crit at this opacity over the node fill.
 * Fades with depth (closest = strongest), floored so depth-5 stays visible.
 */
export function impactAlpha(depth: number): number {
  return Math.max(0.08, 0.32 - 0.06 * (depth - 1))
}

/** Group affected services by depth, ascending; input order kept per depth. */
export function groupAffectedByDepth(
  entries: readonly AffectedEntry[],
): Array<[number, AffectedEntry[]]> {
  const byDepth = new Map<number, AffectedEntry[]>()
  for (const entry of entries) {
    const list = byDepth.get(entry.depth)
    if (list) list.push(entry)
    else byDepth.set(entry.depth, [entry])
  }
  return [...byDepth.entries()].sort((a, b) => a[0] - b[0])
}
