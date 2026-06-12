import {
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type Ref,
} from 'react'
import {
  NODE_H,
  NODE_W,
  edgePath,
  edgeWidth,
  fitTransform,
  layoutGraph,
  neighborsOf,
  walkFrom,
  zoomAt,
  type GraphEdgeRef,
  type Layout,
  type Transform,
  type WalkDir,
} from '@/lib/dagLayout'
import { formatPercent } from '@/lib/format'
import { impactAlpha } from '@/lib/impact'
import { nodeStatus, statusToken } from '@/lib/triage'
import type { SystemEdge, SystemNode } from '@/types/api'
import styles from './FlowMap.module.css'

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

const IDENTITY: Transform = { x: 0, y: 0, k: 1 }

function isEdgeFailing(status: string | undefined): boolean {
  return status === 'critical' || status === 'failing'
}

export default function FlowMap({
  nodes,
  edges,
  selectedId,
  impact = null,
  onSelect,
  onClearSelection,
  onFocusFilter,
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
        ids: [...new Set(nodes.map((n) => n.id))].sort((a, b) => a.localeCompare(b)),
        edges: edgeRefs
          .map((e): [string, string] => [e.source, e.target])
          .sort((a, b) => (a[0] + a[1]).localeCompare(b[0] + b[1])),
      }),
    [nodes, edgeRefs],
  )
  const layout: Layout = useMemo(() => {
    const shape = JSON.parse(shapeKey) as { ids: string[]; edges: [string, string][] }
    return layoutGraph(
      shape.ids,
      shape.edges.map(([source, target]) => ({ source, target })),
    )
  }, [shapeKey])

  // ---- fit ----------------------------------------------------------------
  const fit = useCallback(() => {
    const rect = containerRef.current?.getBoundingClientRect()
    if (!rect || rect.width <= 0 || rect.height <= 0) return
    setTransform(fitTransform(layout, rect))
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

  // ---- keyboard walking ----------------------------------------------------
  const onKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      const dir = KEY_DIRS[e.key]
      if (dir) {
        e.preventDefault()
        setFocusedId((cur) => walkFrom(layout, edgeRefs, cur ?? selectedId, dir) ?? cur)
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
    [layout, edgeRefs, selectedId, focusedId, onSelect, onClearSelection, onFocusFilter, fit],
  )

  const neighbors = selectedId ? neighborsOf(edgeRefs, selectedId) : null
  const showErrLabels = transform.k >= SEMANTIC_ZOOM
  const impactOn = impact !== null

  return (
    <div
      ref={containerRef}
      className={styles.container}
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
          {edges.map((e) => {
            const from = layout.nodes.get(e.source)
            const to = layout.nodes.get(e.target)
            if (!from || !to || e.source === e.target) return null
            const failing = isEdgeFailing(e.status)
            const dimmed = impactOn
              ? !(impact.has(e.source) && impact.has(e.target))
              : neighbors !== null &&
                e.source !== selectedId &&
                e.target !== selectedId
            return (
              <path
                key={`${e.source}>${e.target}`}
                d={edgePath(from, to)}
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
              : neighbors !== null && !selected && !neighbors.has(n.id)
            const focused = n.id === focusedId
            return (
              <g
                key={n.id}
                id={`map-node-${n.id}`}
                data-node-id={n.id}
                transform={`translate(${pos.x} ${pos.y})`}
                className={`${styles.node} ${dimmed ? styles.dimmed : ''}`}
                style={{ '--node-color': statusToken(status) } as CSSProperties}
                onClick={() => onSelect(n.id)}
              >
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
              </g>
            )
          })}
        </g>
      </svg>
    </div>
  )
}
