// Layered service-map layout via dagre (@dagrejs/dagre, MIT). Unlike our
// hand-rolled dagLayout, dagre does crossing-minimisation AND emits edge
// ROUTING waypoints (g.edge().points) — polylines that thread between ranks
// instead of cutting straight through node boxes. That routing is what keeps
// edges visible once the graph grows to ~120 services. Left→right, deterministic
// (ids + edges sorted before insertion, dagre is stable for fixed input order).

import dagre from '@dagrejs/dagre'
import { NODE_H, NODE_W, compareIds, type GraphEdgeRef } from './dagLayout'

export interface DagreLayout {
  /** id → node top-left position in React Flow flow coords. */
  nodes: Map<string, { x: number; y: number }>
  /** "source->target" → routed polyline waypoints (same flow coords). */
  edges: Map<string, { x: number; y: number }[]>
  width: number
  height: number
}

const RANKSEP = 110 // gap between dependency ranks (the long axis, LR)
const NODESEP = 26 // gap between siblings within a rank
const EDGESEP = 14 // gap between parallel edges in a routing channel

const EMPTY: DagreLayout = { nodes: new Map(), edges: new Map(), width: 0, height: 0 }

/** Deterministic left→right layered layout with edge routing. */
export function layoutDagre(nodeIds: readonly string[], edges: readonly GraphEdgeRef[]): DagreLayout {
  const ids = [...new Set(nodeIds)].sort(compareIds)
  if (ids.length === 0) return EMPTY

  const g = new dagre.graphlib.Graph()
  g.setGraph({ rankdir: 'LR', nodesep: NODESEP, ranksep: RANKSEP, edgesep: EDGESEP, marginx: NODE_W / 2, marginy: NODE_H })
  g.setDefaultEdgeLabel(() => ({}))
  for (const id of ids) g.setNode(id, { width: NODE_W, height: NODE_H })

  // Sanitize → dedupe → sort for a deterministic build.
  const idSet = new Set(ids)
  const seen = new Set<string>()
  const clean: GraphEdgeRef[] = []
  for (const e of edges) {
    if (e.source === e.target || !idSet.has(e.source) || !idSet.has(e.target)) continue
    const k = `${e.source}->${e.target}`
    if (seen.has(k)) continue
    seen.add(k)
    clean.push(e)
  }
  clean.sort((a, b) => compareIds(`${a.source}->${a.target}`, `${b.source}->${b.target}`))
  for (const e of clean) g.setEdge(e.source, e.target)

  dagre.layout(g)

  const nodes = new Map<string, { x: number; y: number }>()
  for (const id of ids) {
    const n = g.node(id)
    if (n) nodes.set(id, { x: n.x - NODE_W / 2, y: n.y - NODE_H / 2 })
  }
  const edgeMap = new Map<string, { x: number; y: number }[]>()
  for (const e of clean) {
    const ed = g.edge(e.source, e.target)
    if (ed && Array.isArray(ed.points)) {
      edgeMap.set(`${e.source}->${e.target}`, ed.points.map((p: { x: number; y: number }) => ({ x: p.x, y: p.y })))
    }
  }
  const gr = g.graph()
  return { nodes, edges: edgeMap, width: gr.width ?? 0, height: gr.height ?? 0 }
}
