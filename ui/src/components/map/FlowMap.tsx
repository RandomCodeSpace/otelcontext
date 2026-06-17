import {
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type ReactNode,
  type Ref,
} from 'react'
import {
  NODE_H,
  NODE_W,
  compareIds,
  edgePath,
  edgeWidth,
  fitTransform,
  layoutGraph,
  neighborsOf,
  walkFrom,
  zoomAt,
  type GraphEdgeRef,
  type Layout,
  type LayoutNode,
  type Transform,
  type WalkDir,
} from '@/lib/dagLayout'
import { layoutRadial, walkRadial } from '@/lib/radialLayout'
import { formatPercent } from '@/lib/format'
import { impactAlpha } from '@/lib/impact'
import { nodeStatus, statusToken } from '@/lib/triage'
import type { SystemEdge, SystemNode } from '@/types/api'
import styles from './FlowMap.module.css'

export type LayoutMode = 'radial' | 'dag'

// SVG flow map. Layout is recomputed ONLY when the node/edge SET hash
// changes — metric-only poll updates just re-render fills/widths through
// CSS variables on stable positions. Pan/zoom is a CSS transform on the
// inner <g>; wheel/pinch/drag never trigger React layout work beyond one
// state update.

export interface FlowMapHandle {
  /** Fit the whole graph into the current viewport. */
  fit: () => void
}

interface FlowMapProps {
  nodes: readonly SystemNode[]
  edges: readonly SystemEdge[]
  /** Layout paradigm: radial gravity constellation (default) or layered DAG. */
  mode?: LayoutMode
  /** The inspected service — accent ring + 1-hop neighborhood emphasis. */
  selectedId: string | null
  /**
   * Blast-radius cone (?impact=): service → BFS depth, root at 0. When set
   * it owns the emphasis — cone nodes tint --crit by depth, the rest dims.
   */
  impact?: ReadonlyMap<string, number> | null
  onSelect: (id: string) => void
  onClearSelection: () => void
  onFocusFilter: () => void
  /**
   * Health Core overlay — an absolutely-centered HTML sibling of the <svg>
   * (NOT a foreignObject), so HTML gauges/odometers compose unchanged and the
   * core stays PINNED at viewport center while the field pans/zooms behind it.
   * When undefined, the in-<svg> <text> coreLabel/coreUnit fallback renders.
   */
  core?: ReactNode
  /**
   * In-<svg> center health-count fallback (the "X/Y HEALTHY" text). Defaults
   * on; pass false to keep the dial center FREE (e.g. when health lives in a
   * corner card and the map owns the center). Independent of `core`.
   */
  centerLabel?: boolean
  /**
   * Field-wide focus-distortion: non-neighbor fade. Defaults to
   * `selectedId != null` to preserve the current selection-dims behavior.
   */
  dim?: boolean
  /** Opt-in cinematic ring-depth + core glow. Default flat (false). */
  ringDepth?: boolean
  ref?: Ref<FlowMapHandle>
}

const KEY_DIRS: Record<string, WalkDir> = {
  ArrowLeft: 'left',
  ArrowRight: 'right',
  ArrowUp: 'up',
  ArrowDown: 'down',
}

/** Threshold for semantic zoom: err% labels appear at and above this scale. */
export const SEMANTIC_ZOOM = 0.8

/** Below this zoom a large field hides node names to avoid label pile-up at
 *  ~120 services; small fields always label (and the selected/focused/neighbor
 *  nodes always label regardless of zoom). */
export const NAME_ZOOM = 0.5
const NAME_ALWAYS_MAX = 24

/** Radius of the status-dot marker rendered for un-labelled nodes in a large,
 *  zoomed-out field — the "galaxy" shape. Centered on the node so edges connect
 *  identically whether the node draws as a dot or a chip. */
const DOT_R = 16

/** Fit padding (px) and the small-field legibility floor for the initial fit. */
const FIT_PADDING = 24
const FIT_FLOOR = 0.5
const FIT_FLOOR_MAX_NODES = 24

const IDENTITY: Transform = { x: 0, y: 0, k: 1 }

function isEdgeFailing(status: string | undefined): boolean {
  return status === 'critical' || status === 'failing'
}

/** Center of a node box in layout coordinates. */
function nodeCenter(n: LayoutNode): { x: number; y: number } {
  return { x: n.x + NODE_W / 2, y: n.y + NODE_H / 2 }
}

/**
 * Edge path bowed along the dial: a quadratic whose control point is pushed
 * outward, perpendicular to the chord, so dependencies arc around the center
 * instead of cutting straight across it. Endpoints sit on the node-box edges
 * facing each other. Deterministic — no randomness, derived only from coords.
 */
function radialEdgePath(from: LayoutNode, to: LayoutNode, cx: number, cy: number): string {
  const a = nodeCenter(from)
  const b = nodeCenter(to)
  const mx = (a.x + b.x) / 2
  const my = (a.y + b.y) / 2
  // Bow the control point away from the dial center (outward), so arcs hug
  // their wedge rather than crossing the core. Magnitude scales with chord
  // length but is capped so short links stay nearly straight.
  let nx = mx - cx
  let ny = my - cy
  const nlen = Math.hypot(nx, ny) || 1
  nx /= nlen
  ny /= nlen
  const chord = Math.hypot(b.x - a.x, b.y - a.y)
  const bow = Math.min(chord * 0.18, 60)
  const ctrlX = mx + nx * bow
  const ctrlY = my + ny * bow
  return `M ${a.x} ${a.y} Q ${ctrlX} ${ctrlY} ${b.x} ${b.y}`
}

export default function FlowMap({
  nodes,
  edges,
  mode = 'radial',
  selectedId,
  impact = null,
  onSelect,
  onClearSelection,
  onFocusFilter,
  core,
  centerLabel = true,
  dim,
  ringDepth = false,
  ref,
}: Readonly<FlowMapProps>) {
  const containerRef = useRef<HTMLDivElement>(null)
  const svgRef = useRef<SVGSVGElement>(null)
  const [transform, setTransform] = useState<Transform>(IDENTITY)
  const [focusedId, setFocusedId] = useState<string | null>(null)

  // ---- layout, memoized on the SET hash (never on metric churn) ----------
  // The set hash is a JSON-decodable key: the layout memo depends on the key
  // alone and reconstructs its inputs from it, so a metrics-only poll (new
  // array identities, same sets) provably cannot trigger a re-layout.
  const edgeRefs: GraphEdgeRef[] = useMemo(
    () => edges.map((e) => ({ source: e.source, target: e.target })),
    [edges],
  )
  const shapeKey = useMemo(
    () =>
      JSON.stringify({
        ids: [...new Set(nodes.map((n) => n.id))].sort(compareIds),
        edges: edgeRefs
          .map((e): [string, string] => [e.source, e.target])
          .sort((a, b) => compareIds(a[0] + a[1], b[0] + b[1])),
      }),
    [nodes, edgeRefs],
  )
  const layout: Layout = useMemo(() => {
    const shape = JSON.parse(shapeKey) as { ids: string[]; edges: [string, string][] }
    const refs = shape.edges.map(([source, target]) => ({ source, target }))
    return mode === 'radial' ? layoutRadial(shape.ids, refs) : layoutGraph(shape.ids, refs)
  }, [shapeKey, mode])

  // ---- fit ----------------------------------------------------------------
  const fit = useCallback(() => {
    const rect = containerRef.current?.getBoundingClientRect()
    if (!rect || rect.width <= 0 || rect.height <= 0) return
    // Legibility floor for SMALL fields: contain-fit on a narrow phone viewport
    // shrinks a few nodes to dust AND lets the fixed-size pinned core swallow the
    // inner ring. A floor keeps nodes readable and the core clear; the field then
    // overflows the narrow axis and is reached by pan/pinch. Large graphs (which
    // must zoom out to be seen whole) skip the floor.
    const minScale = layout.nodes.size > 0 && layout.nodes.size <= FIT_FLOOR_MAX_NODES ? FIT_FLOOR : 0
    setTransform(fitTransform(layout, rect, FIT_PADDING, minScale))
  }, [layout])

  useImperativeHandle(ref, () => ({ fit }), [fit])

  // Initial fit, re-run when the node/edge set changes shape.
  useEffect(() => {
    fit()
  }, [fit])

  // ---- wheel zoom (native listener — React's is passive) ------------------
  useEffect(() => {
    const svg = svgRef.current
    if (!svg) return
    const onWheel = (e: WheelEvent) => {
      e.preventDefault()
      const rect = svg.getBoundingClientRect()
      const factor = Math.exp(-e.deltaY * 0.0015)
      setTransform((t) => zoomAt(t, e.clientX - rect.left, e.clientY - rect.top, factor))
    }
    svg.addEventListener('wheel', onWheel, { passive: false })
    return () => svg.removeEventListener('wheel', onWheel)
  }, [])

  // ---- pointer pan + pinch -------------------------------------------------
  const pointers = useRef(new Map<number, { x: number; y: number }>())
  const pinchDist = useRef(0)

  const onPointerDown = useCallback((e: React.PointerEvent<SVGSVGElement>) => {
    e.currentTarget.setPointerCapture(e.pointerId)
    pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY })
    if (pointers.current.size === 2) {
      const [a, b] = [...pointers.current.values()]
      pinchDist.current = Math.hypot(a.x - b.x, a.y - b.y)
    }
  }, [])

  const onPointerMove = useCallback((e: React.PointerEvent<SVGSVGElement>) => {
    const prev = pointers.current.get(e.pointerId)
    if (!prev) return
    const next = { x: e.clientX, y: e.clientY }
    pointers.current.set(e.pointerId, next)
    if (pointers.current.size === 1) {
      setTransform((t) => ({ ...t, x: t.x + next.x - prev.x, y: t.y + next.y - prev.y }))
      return
    }
    if (pointers.current.size === 2) {
      const [a, b] = [...pointers.current.values()]
      const dist = Math.hypot(a.x - b.x, a.y - b.y)
      if (pinchDist.current > 0) {
        const rect = e.currentTarget.getBoundingClientRect()
        const cx = (a.x + b.x) / 2 - rect.left
        const cy = (a.y + b.y) / 2 - rect.top
        const factor = dist / pinchDist.current
        setTransform((t) => zoomAt(t, cx, cy, factor))
      }
      pinchDist.current = dist
    }
  }, [])

  const onPointerEnd = useCallback((e: React.PointerEvent<SVGSVGElement>) => {
    pointers.current.delete(e.pointerId)
    pinchDist.current = 0
  }, [])

  // ---- ambient-motion gate: pause animations when the tab is hidden --------
  // The ONLY physics-side effect. Toggles a data attribute the CSS keys
  // `animation-play-state` off, so the failing-edge march (and any future
  // ambient motion) stops costing frames in a backgrounded tab. No rAF loop.
  useEffect(() => {
    const el = containerRef.current
    if (!el) return
    const sync = () => {
      el.dataset.visible = document.visibilityState === 'visible' ? 'true' : 'false'
    }
    sync()
    document.addEventListener('visibilitychange', sync)
    return () => document.removeEventListener('visibilitychange', sync)
  }, [])

  // ---- keyboard walking ----------------------------------------------------
  const onKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      const dir = KEY_DIRS[e.key]
      if (dir) {
        e.preventDefault()
        setFocusedId((cur) => {
          const from = cur ?? selectedId
          const next =
            mode === 'radial'
              ? walkRadial(layout, from, dir)
              : walkFrom(layout, edgeRefs, from, dir)
          return next ?? cur
        })
        return
      }
      if (e.key === 'Enter' && focusedId) {
        e.preventDefault()
        onSelect(focusedId)
      } else if (e.key === 'Escape') {
        setFocusedId(null)
        onClearSelection()
      } else if (e.key === 'f') {
        e.preventDefault()
        fit()
      } else if (e.key === '/') {
        e.preventDefault()
        onFocusFilter()
      }
    },
    [layout, edgeRefs, mode, selectedId, focusedId, onSelect, onClearSelection, onFocusFilter, fit],
  )

  const neighbors = selectedId ? neighborsOf(edgeRefs, selectedId) : null
  const showErrLabels = transform.k >= SEMANTIC_ZOOM
  const showNames = transform.k >= NAME_ZOOM || nodes.length <= NAME_ALWAYS_MAX
  const impactOn = impact !== null
  const isRadial = mode === 'radial'
  // Field-wide focus-distortion. `dim` defaults to "something is selected" so
  // existing callers keep their selection-dims; a caller can force it off
  // (e.g. a cinematic idle home) by passing dim={false}. Impact overlay always
  // owns its own dimming regardless.
  const dimField = dim ?? selectedId !== null

  // Radial backdrop geometry: concentric ring guides at each layer radius and a
  // health-core label at center. Derived from the layout (which already encodes
  // ring spacing) so it stays in lockstep.
  const cx = layout.width / 2
  const cy = layout.height / 2
  const ringRadii = useMemo(() => {
    if (!isRadial) return []
    // One faint guide circle per pseudo-ring band (node.layer), at the band's
    // mean node radius. The phyllotaxis disc gives every node a distinct radius,
    // so guiding off the bands draws ceil(sqrt(n)) rings instead of ~n.
    const acc = new Map<number, { total: number; count: number }>()
    for (const n of layout.nodes.values()) {
      const r = Math.hypot(n.x + NODE_W / 2 - cx, n.y + NODE_H / 2 - cy)
      const a = acc.get(n.layer) ?? { total: 0, count: 0 }
      a.total += r
      a.count += 1
      acc.set(n.layer, a)
    }
    return [...acc.entries()]
      .sort((p, q) => p[0] - q[0])
      .map(([, a]) => Math.round(a.total / a.count))
  }, [isRadial, layout, cx, cy])

  // Health-core label: count of nodes still healthy vs total (numerals are the
  // source of truth; the dial is decoration).
  // Only the in-<svg> fallback core label consumes this. Skip the O(n) scan
  // entirely on the live (core-provided) canvas, and memoize so pan/zoom
  // re-renders don't re-scan up to ~720 nodes per frame.
  const healthyCount = useMemo(
    () =>
      core === undefined && centerLabel
        ? nodes.filter((n) => nodeStatus(n.status) === 'healthy').length
        : 0,
    [core, centerLabel, nodes],
  )

  return (
    <div
      ref={containerRef}
      className={`${styles.container} ${ringDepth ? styles.depth : ''}`}
      role="application"
      aria-label="Service flow map"
      aria-activedescendant={focusedId ? `map-node-${focusedId}` : undefined}
      tabIndex={0}
      onKeyDown={onKeyDown}
    >
      <svg
        ref={svgRef}
        className={styles.svg}
        data-testid="flow-map-svg"
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerEnd}
        onPointerCancel={onPointerEnd}
      >
        <g transform={`translate(${transform.x} ${transform.y}) scale(${transform.k})`}>
          {isRadial && (
            <g className={styles.dial} aria-hidden="true" data-testid="radial-dial">
              {ringRadii.map((r, i) => (
                <circle
                  key={r}
                  className={styles.ring}
                  cx={cx}
                  cy={cy}
                  r={r}
                  style={
                    {
                      // Depth cue (ringDepth-gated in CSS): inner rings read
                      // bright, outer rings recede. 0 = innermost.
                      '--ring-depth': ringRadii.length > 1 ? i / (ringRadii.length - 1) : 0,
                    } as CSSProperties
                  }
                />
              ))}
              {core === undefined && centerLabel && (
                <>
                  <text className={styles.coreLabel} x={cx} y={cy - 4}>
                    {`${healthyCount}/${nodes.length}`}
                  </text>
                  <text className={styles.coreUnit} x={cx} y={cy + 12}>
                    HEALTHY
                  </text>
                </>
              )}
            </g>
          )}
          {edges.map((e) => {
            const from = layout.nodes.get(e.source)
            const to = layout.nodes.get(e.target)
            if (!from || !to || e.source === e.target) return null
            const failing = isEdgeFailing(e.status)
            const dimmed = impactOn
              ? !(impact.has(e.source) && impact.has(e.target))
              : dimField &&
                neighbors !== null &&
                e.source !== selectedId &&
                e.target !== selectedId
            // March failing dashes core-ward. The path runs source→target, so
            // the default offset animation flows that way; reverse it when the
            // source is the endpoint nearer the core (target is outward).
            const coreward =
              failing && isRadial
                ? Math.hypot(nodeCenter(from).x - cx, nodeCenter(from).y - cy) <
                  Math.hypot(nodeCenter(to).x - cx, nodeCenter(to).y - cy)
                : false
            return (
              <path
                key={`${e.source}>${e.target}`}
                d={isRadial ? radialEdgePath(from, to, cx, cy) : edgePath(from, to)}
                data-coreward={coreward ? 'reverse' : undefined}
                className={`${styles.edge} ${failing ? styles.edgeFailing : ''} ${dimmed ? styles.dimmed : ''}`}
                style={{ '--edge-w': edgeWidth(e.call_count) } as CSSProperties}
              />
            )
          })}
          {nodes.map((n) => {
            const pos = layout.nodes.get(n.id)
            if (!pos) return null
            const status = nodeStatus(n.status)
            const selected = n.id === selectedId
            const depth = impactOn ? impact.get(n.id) : undefined
            const dimmed = impactOn
              ? depth === undefined
              : dimField && neighbors !== null && !selected && !neighbors.has(n.id)
            const focused = n.id === focusedId
            // In radial mode position carries meaning (ring = criticality),
            // so spell it out for non-visual users. Ring 0 is most critical.
            const ariaLabel = isRadial
              ? `${n.id}, ${status}, ring ${pos.layer}${pos.layer === 0 ? ', most critical' : ''}`
              : `${n.id}, ${status}`
            // Shape: a labeled chip when names show (few nodes / zoomed in) or
            // when this node is individually surfaced (selected / focused / a
            // neighbor of the selection); otherwise a small status dot, so a
            // large zoomed-out field reads as a dense even galaxy, not a smear
            // of overlapping chips. Both shapes share the node center, so edges
            // (which use layout coords) connect identically either way.
            const labelled = showNames || selected || focused || (neighbors?.has(n.id) ?? false)
            return (
              <g
                key={n.id}
                id={`map-node-${n.id}`}
                data-node-id={n.id}
                data-status={status}
                data-selected={selected ? '' : undefined}
                role="img"
                aria-label={ariaLabel}
                className={`${styles.node} ${dimmed ? styles.dimmed : ''}`}
                style={
                  {
                    '--node-color': statusToken(status),
                    transform: `translate(${pos.x}px, ${pos.y}px)`,
                  } as CSSProperties
                }
                onClick={() => onSelect(n.id)}
              >
                {/* Anomaly beacon: failing services pulse so a glance at the map
                    answers "what's on fire now". Behind the chip/dot; pure
                    opacity/scale, gated off on coarse pointers + reduced motion
                    + hidden tab (see CSS). Suppressed under the impact overlay,
                    which owns its own emphasis. */}
                {status === 'critical' && !impactOn &&
                  (labelled ? (
                    <rect
                      className={styles.nodePulse}
                      data-testid={`node-pulse-${n.id}`}
                      width={NODE_W}
                      height={NODE_H}
                      rx={6}
                      aria-hidden="true"
                    />
                  ) : (
                    <circle
                      className={styles.nodePulse}
                      data-testid={`node-pulse-${n.id}`}
                      cx={NODE_W / 2}
                      cy={NODE_H / 2}
                      r={DOT_R}
                      aria-hidden="true"
                    />
                  ))}
                {labelled ? (
                  <>
                    <rect
                      className={`${styles.nodeRect} ${selected ? styles.nodeSelected : ''} ${focused ? styles.nodeFocused : ''} ${depth === 0 ? styles.nodeImpactRoot : ''}`}
                      width={NODE_W}
                      height={NODE_H}
                      rx={6}
                    />
                    {depth !== undefined && depth > 0 && (
                      <rect
                        className={styles.nodeImpact}
                        data-testid={`impact-tint-${n.id}`}
                        width={NODE_W}
                        height={NODE_H}
                        rx={6}
                        style={{ fillOpacity: impactAlpha(depth) }}
                      />
                    )}
                    <circle className={styles.nodeDot} cx={16} cy={NODE_H / 2} r={4} />
                    <text className={styles.nodeName} x={28} y={NODE_H / 2 + 4}>
                      {n.id.length > 16 ? `${n.id.slice(0, 15)}…` : n.id}
                    </text>
                    {showErrLabels && (
                      <text className={styles.nodeErr} x={NODE_W - 8} y={NODE_H / 2 + 4}>
                        {formatPercent(n.metrics.error_rate)}
                      </text>
                    )}
                  </>
                ) : (
                  <>
                    <circle
                      className={`${styles.nodeMarker} ${selected ? styles.nodeSelected : ''} ${focused ? styles.nodeFocused : ''} ${depth === 0 ? styles.nodeImpactRoot : ''}`}
                      cx={NODE_W / 2}
                      cy={NODE_H / 2}
                      r={DOT_R}
                    />
                    {depth !== undefined && depth > 0 && (
                      <circle
                        className={styles.nodeImpact}
                        data-testid={`impact-tint-${n.id}`}
                        cx={NODE_W / 2}
                        cy={NODE_H / 2}
                        r={DOT_R}
                        style={{ fillOpacity: impactAlpha(depth) }}
                      />
                    )}
                  </>
                )}
              </g>
            )
          })}
        </g>
      </svg>
      {core !== undefined && (
        <div className={styles.core} data-testid="flow-map-core">
          {core}
        </div>
      )}
    </div>
  )
}
