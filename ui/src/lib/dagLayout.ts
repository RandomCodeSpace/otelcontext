// Deterministic layered DAG layout for the service flow map.
//
// Replaces the cytoscape/COSE-Bilkent physics layout with ~150 lines of our
// own: identical input → identical output, every time. Pipeline:
//   1. sanitize edges (drop self-loops/unknown endpoints, dedupe)
//   2. break cycles with a DFS over sorted ids (back-edges dropped — only
//      for layout; they still render)
//   3. longest-path layering from root callers
//   4. one stable barycenter pass for within-layer order
//   5. grid positions, layers centered vertically
//
// Pan/zoom math (fitTransform/zoomAt) and keyboard walking (walkFrom) live
// here too so they are unit-testable without a DOM.

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

export interface Transform {
  x: number
  y: number
  k: number
}

export const NODE_W = 168
export const NODE_H = 40
export const GAP_X = 56
export const GAP_Y = 16

export const MIN_ZOOM = 0.2
export const MAX_ZOOM = 2.5

/**
 * Locale-independent string compare (UTF-16 code units). The layout must be
 * deterministic across machines, so localeCompare is deliberately NOT used.
 */
export function compareIds(a: string, b: string): number {
  if (a < b) return -1
  return a > b ? 1 : 0
}

/** Order-insensitive identity of the node/edge SET (metrics excluded). */
export function graphSetHash(
  nodeIds: readonly string[],
  edges: readonly GraphEdgeRef[],
): string {
  const ns = [...nodeIds].sort(compareIds).join(',')
  const es = edges
    .map((e) => `${e.source}>${e.target}`)
    .sort(compareIds)
    .join(',')
  return `${ns}|${es}`
}

/** Keep edges whose endpoints both exist, no self-loops, deduped. Sorted. */
function sanitizeEdges(ids: ReadonlySet<string>, edges: readonly GraphEdgeRef[]): GraphEdgeRef[] {
  const seen = new Set<string>()
  const out: GraphEdgeRef[] = []
  for (const e of edges) {
    if (e.source === e.target || !ids.has(e.source) || !ids.has(e.target)) continue
    const key = `${e.source}>${e.target}`
    if (seen.has(key)) continue
    seen.add(key)
    out.push({ source: e.source, target: e.target })
  }
  out.sort((a, b) => (a.source + a.target < b.source + b.target ? -1 : 1))
  return out
}

/** Iterative DFS from one root: keeps forward edges, drops back-edges. */
function dfsKeepForwardEdges(
  root: string,
  out: ReadonlyMap<string, string[]>,
  state: Map<string, 1 | 2>,
  kept: GraphEdgeRef[],
): void {
  const stack: Array<{ id: string; next: number }> = [{ id: root, next: 0 }]
  state.set(root, 1)
  while (stack.length > 0) {
    const frame = stack[stack.length - 1]
    const targets = out.get(frame.id) ?? []
    if (frame.next >= targets.length) {
      state.set(frame.id, 2)
      stack.pop()
      continue
    }
    const target = targets[frame.next++]
    const s = state.get(target)
    if (s === 1) continue // back-edge → drop for layering
    kept.push({ source: frame.id, target })
    if (s === undefined) {
      state.set(target, 1)
      stack.push({ id: target, next: 0 })
    }
  }
}

/** Drop back-edges via iterative DFS from sorted ids — deterministic. */
function breakCycles(sortedIds: readonly string[], edges: readonly GraphEdgeRef[]): GraphEdgeRef[] {
  const out = new Map<string, string[]>()
  for (const e of edges) {
    const list = out.get(e.source)
    if (list) list.push(e.target)
    else out.set(e.source, [e.target])
  }
  const state = new Map<string, 1 | 2>() // 1 = on stack, 2 = done
  const kept: GraphEdgeRef[] = []
  for (const root of sortedIds) {
    if (!state.has(root)) dfsKeepForwardEdges(root, out, state, kept)
  }
  return kept
}

/** Longest path from root callers over the (acyclic) edge list. */
function assignLayers(sortedIds: readonly string[], dag: readonly GraphEdgeRef[]): Map<string, number> {
  const layer = new Map<string, number>()
  for (const id of sortedIds) layer.set(id, 0)
  // Relax in passes; the DAG's longest path bounds the pass count.
  let changed = true
  while (changed) {
    changed = false
    for (const e of dag) {
      const next = (layer.get(e.source) ?? 0) + 1
      if (next > (layer.get(e.target) ?? 0)) {
        layer.set(e.target, next)
        changed = true
      }
    }
  }
  return layer
}

/** One barycenter pass, left → right, stable on ties. */
function orderLayers(layers: string[][], dag: readonly GraphEdgeRef[]): string[][] {
  const preds = new Map<string, string[]>()
  for (const e of dag) {
    const list = preds.get(e.target)
    if (list) list.push(e.source)
    else preds.set(e.target, [e.source])
  }
  const row = new Map<string, number>()
  layers[0]?.forEach((id, i) => row.set(id, i))
  for (let k = 1; k < layers.length; k++) {
    const scored = layers[k].map((id, i) => {
      const ps = (preds.get(id) ?? []).map((p) => row.get(p)).filter((r): r is number => r !== undefined)
      const bary = ps.length > 0 ? ps.reduce((a, b) => a + b, 0) / ps.length : i
      return { id, bary, i }
    })
    scored.sort((a, b) => a.bary - b.bary || a.i - b.i)
    layers[k] = scored.map((s) => s.id)
    layers[k].forEach((id, i) => row.set(id, i))
  }
  return layers
}

export function layoutGraph(
  nodeIds: readonly string[],
  edges: readonly GraphEdgeRef[],
): Layout {
  const sortedIds = [...new Set(nodeIds)].sort(compareIds)
  const idSet = new Set(sortedIds)
  const clean = sanitizeEdges(idSet, edges)
  const dag = breakCycles(sortedIds, clean)
  const layerOf = assignLayers(sortedIds, dag)

  const layerCount = sortedIds.length === 0 ? 0 : Math.max(...layerOf.values()) + 1
  const layers: string[][] = Array.from({ length: layerCount }, () => [])
  for (const id of sortedIds) layers[layerOf.get(id) ?? 0].push(id)
  orderLayers(layers, dag)

  const maxRows = layers.reduce((m, l) => Math.max(m, l.length), 0)
  const height = maxRows > 0 ? maxRows * (NODE_H + GAP_Y) - GAP_Y : 0
  const nodes = new Map<string, LayoutNode>()
  layers.forEach((layerIds, layer) => {
    const layerH = layerIds.length * (NODE_H + GAP_Y) - GAP_Y
    const offsetY = (height - layerH) / 2
    layerIds.forEach((id, rowIdx) => {
      nodes.set(id, {
        id,
        x: layer * (NODE_W + GAP_X),
        y: offsetY + rowIdx * (NODE_H + GAP_Y),
        layer,
        row: rowIdx,
      })
    })
  })
  const width = layerCount > 0 ? layerCount * (NODE_W + GAP_X) - GAP_X : 0
  return { nodes, layers, width, height }
}

/** Edge stroke width: log of call count, clamped to [1, 5]. */
export function edgeWidth(callCount: number): number {
  const w = 1 + 1.2 * Math.log10(1 + Math.max(0, callCount))
  return Math.min(5, Math.max(1, w))
}

/** Transform that fits the content bbox into the viewport (never upscales). */
export function fitTransform(
  content: { width: number; height: number },
  viewport: { width: number; height: number },
  padding = 24,
): Transform {
  if (content.width <= 0 || content.height <= 0) return { x: padding, y: padding, k: 1 }
  const k = Math.min(
    1,
    (viewport.width - padding * 2) / content.width,
    (viewport.height - padding * 2) / content.height,
  )
  return {
    k,
    x: (viewport.width - content.width * k) / 2,
    y: (viewport.height - content.height * k) / 2,
  }
}

/** Zoom by `factor` keeping the viewport point (cx, cy) stationary. */
export function zoomAt(t: Transform, cx: number, cy: number, factor: number): Transform {
  const k = Math.min(MAX_ZOOM, Math.max(MIN_ZOOM, t.k * factor))
  const ratio = k / t.k
  return {
    k,
    x: cx - (cx - t.x) * ratio,
    y: cy - (cy - t.y) * ratio,
  }
}

export type WalkDir = 'left' | 'right' | 'up' | 'down'

/** Keyboard walk: ←/→ caller/callee (nearest in y), ↑/↓ layer siblings. */
export function walkFrom(
  layout: Layout,
  edges: readonly GraphEdgeRef[],
  current: string | null,
  dir: WalkDir,
): string | null {
  if (current === null || !layout.nodes.has(current)) {
    return current === null ? (layout.layers[0]?.[0] ?? null) : null
  }
  const node = layout.nodes.get(current)!
  if (dir === 'up' || dir === 'down') {
    const siblings = layout.layers[node.layer]
    return siblings[node.row + (dir === 'down' ? 1 : -1)] ?? null
  }
  const candidates = edges
    .filter((e) => (dir === 'right' ? e.source === current : e.target === current))
    .map((e) => layout.nodes.get(dir === 'right' ? e.target : e.source))
    .filter((n): n is LayoutNode => n !== undefined)
  if (candidates.length === 0) return null
  candidates.sort(
    (a, b) => Math.abs(a.y - node.y) - Math.abs(b.y - node.y) || (a.id < b.id ? -1 : 1),
  )
  return candidates[0].id
}

/** Cubic bezier from the source's right edge to the target's left edge. */
export function edgePath(from: LayoutNode, to: LayoutNode): string {
  const x1 = from.x + NODE_W
  const y1 = from.y + NODE_H / 2
  const x2 = to.x
  const y2 = to.y + NODE_H / 2
  const dx = Math.max(24, Math.abs(x2 - x1) / 2)
  return `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`
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
