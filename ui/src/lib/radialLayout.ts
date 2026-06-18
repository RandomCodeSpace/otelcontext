// Deterministic phyllotaxis ("Sunflower Galaxy") layout for the service map.
//
// Produces the shared `Layout` shape (nodes / layers / width / height) defined
// in dagLayout, consumed by the React Flow map renderer. The mapping:
//
//   radius = criticality RANK — the most-critical node lands near center, the
//     least-critical at the rim. Ranked node i is placed on a Fermat/sunflower
//     spiral: r_i = R * sqrt((i + 0.5) / n), θ_i = i * GOLDEN_ANGLE. The golden
//     angle (~137.507°) is the divergence angle of phyllotaxis, so successive
//     nodes never line up into spokes and the disc fills EVENLY — no hollow
//     center, no ring banding, dense from 7 to 120+ services.
//   band   = floor(sqrt(rank)) — a pseudo-ring used only for the Layout meta
//     (aria "ring N", keyboard in/out steps). Band b holds 2b+1 nodes, matching
//     its circumference, and the band count is ceil(sqrt(n)).
//
// `layer` carries the band index and `row` the angular rank within the band,
// keeping the struct Layout-compatible. Everything is O(V log V) and fully
// deterministic: identical input set → identical coordinates, every tie broken
// via compareIds (UTF-16 codepoint order, never localeCompare).

import {
  NODE_W,
  NODE_H,
  GAP_X,
  compareIds,
  type GraphEdgeRef,
  type Layout,
  type LayoutNode,
} from './dagLayout'

// Criticality weights. distinctCallerIndegree dominates (blast potential),
// total fan-in/out is the secondary signal.
const W_CALLER = 0.6
const W_VOLUME = 0.4

// The golden angle in radians (2π / φ²). Placing successive nodes this far apart
// is what makes a sunflower seed head — and this map — fill its disc uniformly.
const GOLDEN_ANGLE = 2.399963229728653

// Sunflower spread: nearest-neighbor spacing ≈ R / sqrt(n) = SUNFLOWER_SPREAD
// layout units (R = SUNFLOWER_SPREAD * sqrt(n)). Tuned so the field reads as a
// dense even disc of small status dots when zoomed out (~120 services) while the
// few-node view (labeled chips) still spreads without chip overlap. ~NODE_W*0.6.
const SUNFLOWER_SPREAD = Math.round(NODE_W * 0.6)

// Margin past the outermost node so a rim chip/dot never clips the viewport
// edge. Half a node box (the wide axis) contains a rim chip; the gap adds air.
const SPAN_MARGIN = Math.round(GAP_X / 2)

interface Metrics {
  /** Distinct services that call this node (inbound edge sources). */
  distinctCallers: number
  /** Total inbound + outbound degree (call volume proxy). */
  degree: number
}

/** Keep edges whose endpoints both exist, no self-loops, deduped, sorted. */
function cleanEdges(ids: ReadonlySet<string>, edges: readonly GraphEdgeRef[]): GraphEdgeRef[] {
  const seen = new Set<string>()
  const out: GraphEdgeRef[] = []
  for (const e of edges) {
    if (e.source === e.target || !ids.has(e.source) || !ids.has(e.target)) continue
    const key = `${e.source}>${e.target}`
    if (seen.has(key)) continue
    seen.add(key)
    out.push({ source: e.source, target: e.target })
  }
  out.sort((a, b) => compareIds(a.source + '>' + a.target, b.source + '>' + b.target))
  return out
}

/** Per-node inbound/outbound metrics from the cleaned edge list. */
function computeMetrics(ids: readonly string[], edges: readonly GraphEdgeRef[]): Map<string, Metrics> {
  const callers = new Map<string, Set<string>>()
  const degree = new Map<string, number>()
  for (const id of ids) {
    callers.set(id, new Set())
    degree.set(id, 0)
  }
  for (const e of edges) {
    callers.get(e.target)!.add(e.source)
    degree.set(e.target, (degree.get(e.target) ?? 0) + 1)
    degree.set(e.source, (degree.get(e.source) ?? 0) + 1)
  }
  const out = new Map<string, Metrics>()
  for (const id of ids) {
    out.set(id, { distinctCallers: callers.get(id)!.size, degree: degree.get(id)! })
  }
  return out
}

/** norm(x) = x / max, 0 when max is 0. Keeps both terms in [0, 1]. */
function normFactor(max: number): (x: number) => number {
  return max > 0 ? (x: number) => x / max : () => 0
}

/** criticality score in [0, 1]; higher = more critical = closer to center. */
function criticalityScores(
  ids: readonly string[],
  metrics: ReadonlyMap<string, Metrics>,
): Map<string, number> {
  let maxCallers = 0
  let maxLogDeg = 0
  const logDeg = new Map<string, number>()
  for (const id of ids) {
    const m = metrics.get(id)!
    const ld = Math.log(1 + m.degree)
    logDeg.set(id, ld)
    if (m.distinctCallers > maxCallers) maxCallers = m.distinctCallers
    if (ld > maxLogDeg) maxLogDeg = ld
  }
  const nc = normFactor(maxCallers)
  const nv = normFactor(maxLogDeg)
  const scores = new Map<string, number>()
  for (const id of ids) {
    const m = metrics.get(id)!
    scores.set(id, W_CALLER * nc(m.distinctCallers) + W_VOLUME * nv(logDeg.get(id)!))
  }
  return scores
}

/**
 * Phyllotaxis layout. Returns a dagLayout-compatible Layout where `layer` is the
 * pseudo-ring band (0 = center, most critical) and `row` is the angular rank
 * within that band. Center is (width/2, height/2).
 */
export function layoutRadial(
  nodeIds: readonly string[],
  edges: readonly GraphEdgeRef[],
): Layout {
  const sortedIds = [...new Set(nodeIds)].sort(compareIds)
  const n = sortedIds.length
  if (n === 0) return { nodes: new Map(), layers: [], width: 0, height: 0 }

  const idSet = new Set(sortedIds)
  const clean = cleanEdges(idSet, edges)
  const metrics = computeMetrics(sortedIds, clean)
  const scores = criticalityScores(sortedIds, metrics)

  // Rank by criticality DESC; compareIds breaks ties → fully deterministic, no
  // Math.random. Rank 0 (most critical) seeds the center of the spiral.
  const ranked = [...sortedIds].sort((a, b) => {
    const d = scores.get(b)! - scores.get(a)!
    return d !== 0 ? d : compareIds(a, b)
  })

  // Disc radius. Nearest-neighbor spacing in a sunflower ≈ R/sqrt(n), so a fixed
  // R/sqrt(n) = SUNFLOWER_SPREAD keeps node spacing constant as n grows — the
  // disc just gets bigger, never more crowded.
  const R = SUNFLOWER_SPREAD * Math.sqrt(n)
  // Square span, symmetric about the polar center so width/2 == height/2 == the
  // center the dial and Health Core are drawn around.
  const span = 2 * (R + NODE_W / 2 + SPAN_MARGIN)
  const cx = span / 2
  const cy = span / 2

  // Pseudo-ring bands: band b = floor(sqrt(rank)) holds ranks [b², (b+1)²) → 2b+1
  // nodes, matching the band's circumference. Band count = ceil(sqrt(n)); every
  // band 0..max is non-empty, so the keyboard walk never dead-ends on a gap.
  const bandCount = Math.floor(Math.sqrt(n - 1)) + 1
  const bands: { id: string; angle: number }[][] = Array.from({ length: bandCount }, () => [])
  const placed = new Map<string, { x: number; y: number }>()

  ranked.forEach((id, i) => {
    const r = R * Math.sqrt((i + 0.5) / n)
    const theta = i * GOLDEN_ANGLE
    // SVG y grows downward; angle 0 points right. Node box top-left is offset by
    // half a box so the node CENTER (where edges connect) sits on the spiral.
    const x = cx + r * Math.cos(theta) - NODE_W / 2
    const y = cy + r * Math.sin(theta) - NODE_H / 2
    placed.set(id, { x, y })
    // Normalize the spiral angle into [0, 2π) for stable angular ordering.
    let a = theta % (2 * Math.PI)
    if (a < 0) a += 2 * Math.PI
    bands[Math.floor(Math.sqrt(i))].push({ id, angle: a })
  })

  // Order each band by angle (compareIds tiebreak) so left/right keyboard steps
  // move to true angular neighbors and `row` reads as angular rank.
  const layers: string[][] = bands.map((entries) => {
    entries.sort((p, q) => p.angle - q.angle || compareIds(p.id, q.id))
    return entries.map((e) => e.id)
  })

  const nodes = new Map<string, LayoutNode>()
  layers.forEach((ids, band) => {
    ids.forEach((id, row) => {
      const p = placed.get(id)!
      nodes.set(id, { id, x: p.x, y: p.y, layer: band, row })
    })
  })

  return { nodes, layers, width: span, height: span }
}
