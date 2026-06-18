// Shared primitives for the service flow map.
//
// Node box dimensions, the deterministic id ordering, the shared Layout /
// LayoutNode shape (produced by radialLayout's layoutRadial), edge-stroke
// scaling and 1-hop neighbour lookup. The hand-rolled layered-DAG layout,
// pan/zoom math and keyboard-walk helpers that used to live here were removed
// when the map moved to React Flow (which owns layout interaction); only the
// primitives the map and radialLayout still consume remain.

export interface GraphEdgeRef {
  source: string
  target: string
}

export interface LayoutNode {
  id: string
  x: number
  y: number
  layer: number
  row: number
}

export interface Layout {
  nodes: Map<string, LayoutNode>
  layers: string[][]
  width: number
  height: number
}

export const NODE_W = 168
export const NODE_H = 40
export const GAP_X = 56

/**
 * Locale-independent string compare (UTF-16 code units). The layout must be
 * deterministic across machines, so localeCompare is deliberately NOT used.
 */
export function compareIds(a: string, b: string): number {
  if (a < b) return -1
  return a > b ? 1 : 0
}

/** Edge stroke width: log of call count, clamped to [1, 5]. */
export function edgeWidth(callCount: number): number {
  const w = 1 + 1.2 * Math.log10(1 + Math.max(0, callCount))
  return Math.min(5, Math.max(1, w))
}

/** 1-hop neighborhood (both directions) for selection dimming. */
export function neighborsOf(edges: readonly GraphEdgeRef[], id: string): Set<string> {
  const out = new Set<string>()
  for (const e of edges) {
    if (e.source === id) out.add(e.target)
    if (e.target === id) out.add(e.source)
  }
  return out
}
