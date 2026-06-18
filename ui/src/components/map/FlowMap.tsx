import {
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  type CSSProperties,
  type ReactNode,
  type Ref,
} from 'react'
import {
  Background,
  BackgroundVariant,
  BaseEdge,
  Controls,
  Handle,
  MiniMap,
  Position,
  ReactFlow,
  ReactFlowProvider,
  useInternalNode,
  useNodesState,
  useReactFlow,
  useStore,
  type Edge,
  type EdgeProps,
  type EdgeTypes,
  type InternalNode,
  type Node,
  type NodeProps,
  type NodeTypes,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { NODE_H, NODE_W, compareIds, edgeWidth, neighborsOf, type GraphEdgeRef } from '@/lib/dagLayout'
import { layoutDagre } from '@/lib/dagreLayout'
import { formatPercent } from '@/lib/format'
import { nodeStatus, statusToken } from '@/lib/triage'
import type { SystemEdge, SystemNode } from '@/types/api'
import styles from './FlowMap.module.css'

// React Flow service map. React Flow owns pan / zoom / fit, the grid, the
// minimap and the zoom controls; we feed it a dagre layout (dagreLayout.ts —
// crossing-minimised, left→right) for node positions AND edge ROUTING
// waypoints, so links thread between ranks instead of disappearing under node
// boxes at ~120 services. Two readability levers for scale: edges are drawn
// through the dagre waypoints (routed, capped-thin), and nodes drop to status
// DOTS below a zoom threshold (level-of-detail) so the 168px chips stop
// occluding the field when zoomed out. dagre seeds the positions; nodes stay
// DRAGGABLE for manual nudges — a drag survives metric polls, and a fresh
// layout (topology change) re-seeds.

export type LayoutMode = 'radial' | 'dag'

export interface FlowMapHandle {
  /** Fit the whole graph into the current viewport. */
  fit: () => void
}

interface FlowMapProps {
  nodes: readonly SystemNode[]
  edges: readonly SystemEdge[]
  /** Layout paradigm. Only the layered dagre layout is used today; kept for API stability. */
  mode?: LayoutMode
  /** The inspected service — accent ring + 1-hop neighborhood emphasis. */
  selectedId: string | null
  /**
   * Blast-radius cone (?impact=): service → BFS depth, root at 0. When set it
   * owns the emphasis — cone nodes tint by depth, the rest dims.
   */
  impact?: ReadonlyMap<string, number> | null
  onSelect: (id: string) => void
  onClearSelection: () => void
  /** Reserved (palette filter). No-op in the React Flow map. */
  onFocusFilter?: () => void
  /** Reserved overlay slots — kept for API stability, unused here. */
  core?: ReactNode
  centerLabel?: boolean
  /** Field-wide focus-distortion: non-neighbor fade. Defaults to selected!=null. */
  dim?: boolean
  ringDepth?: boolean
  ref?: Ref<FlowMapHandle>
}

type Emphasis = 'normal' | 'selected' | 'neighbor' | 'dim'

interface ServiceNodeData {
  node: SystemNode
  emphasis: Emphasis
  impactDepth: number | null
  [key: string]: unknown
}
type ServiceNode = Node<ServiceNodeData, 'service'>

const FIT_OPTIONS = { padding: 0.18 } as const

// Below this zoom the chips collapse to status dots (level-of-detail) — at ~120
// services fitView lands well under this, so the dense view is dots, not a wall
// of overlapping 168px boxes; zooming in past it restores the labelled chips.
const LOD_ZOOM = 0.62

/** The token-themed status chip, with a level-of-detail dot variant when zoomed
 *  out. Handles are invisible border anchors (React Flow needs them; the routed
 *  edge draws its own path). */
function ServiceMapNode({ data }: NodeProps<ServiceNode>) {
  const compact = useStore((s) => s.transform[2] < LOD_ZOOM)
  const { node, emphasis, impactDepth } = data
  const status = nodeStatus(node.status)
  const styleVar = { '--node-color': statusToken(status), width: NODE_W, height: NODE_H } as CSSProperties
  const handles = (
    <>
      <Handle type="target" position={Position.Left} isConnectable={false} className={styles.handle} />
      <Handle type="source" position={Position.Right} isConnectable={false} className={styles.handle} />
    </>
  )
  if (compact) {
    const cls = [
      styles.dotNode,
      emphasis === 'dim' && styles.dimmed,
      (emphasis === 'selected' || impactDepth === 0) && styles.dotSelected,
    ]
      .filter(Boolean)
      .join(' ')
    return (
      <div className={cls} style={styleVar} title={node.id}>
        {handles}
      </div>
    )
  }
  const cls = [
    styles.chip,
    emphasis === 'selected' && styles.selected,
    emphasis === 'dim' && styles.dimmed,
    impactDepth === 0 && styles.impactRoot,
  ]
    .filter(Boolean)
    .join(' ')
  return (
    <div className={cls} style={styleVar}>
      <Handle type="target" position={Position.Left} isConnectable={false} className={styles.handle} />
      <span className={styles.dot} aria-hidden="true" />
      <span className={styles.name}>{node.id}</span>
      <span className={styles.err}>{formatPercent(node.metrics.error_rate)}</span>
      <Handle type="source" position={Position.Right} isConnectable={false} className={styles.handle} />
    </div>
  )
}

const NODE_TYPES: NodeTypes = { service: ServiceMapNode }

/** Unit vector (guards zero length). */
function unit(dx: number, dy: number): { x: number; y: number } {
  const m = Math.hypot(dx, dy) || 1
  return { x: dx / m, y: dy / m }
}

/** SVG path through the dagre waypoints with lightly rounded corners — the
 *  routed "circuit" look, radius clamped so it never overshoots a short leg. */
function routedPath(pts: { x: number; y: number }[], radius = 7): string {
  if (pts.length < 2) return ''
  if (pts.length === 2) return `M${pts[0].x},${pts[0].y} L${pts[1].x},${pts[1].y}`
  let d = `M${pts[0].x},${pts[0].y}`
  for (let i = 1; i < pts.length - 1; i++) {
    const p0 = pts[i - 1]
    const p1 = pts[i]
    const p2 = pts[i + 1]
    const r = Math.min(radius, Math.hypot(p1.x - p0.x, p1.y - p0.y) / 2, Math.hypot(p2.x - p1.x, p2.y - p1.y) / 2)
    const u1 = unit(p1.x - p0.x, p1.y - p0.y)
    const u2 = unit(p2.x - p1.x, p2.y - p1.y)
    d += ` L${p1.x - u1.x * r},${p1.y - u1.y * r} Q${p1.x},${p1.y} ${p1.x + u2.x * r},${p1.y + u2.y * r}`
  }
  const last = pts[pts.length - 1]
  d += ` L${last.x},${last.y}`
  return d
}

/** Centre of an internal node in flow coords (measured dims, NODE_* fallback). */
function nodeCenter(n: InternalNode): { x: number; y: number } {
  return {
    x: n.internals.positionAbsolute.x + (n.measured?.width ?? NODE_W) / 2,
    y: n.internals.positionAbsolute.y + (n.measured?.height ?? NODE_H) / 2,
  }
}

/** Point on a node's border along the ray toward `toward` (rectangle
 *  intersection, L∞). Computed from the node's CURRENT position, so edges meet
 *  the chip EDGE — not its centre — and follow the node when it's dragged. */
function borderToward(n: InternalNode, toward: { x: number; y: number }): { x: number; y: number } {
  const w = (n.measured?.width ?? NODE_W) / 2
  const h = (n.measured?.height ?? NODE_H) / 2
  const c = nodeCenter(n)
  const ex = toward.x - c.x
  const ey = toward.y - c.y
  const scale = Math.max(Math.abs(ex) / w, Math.abs(ey) / h)
  if (!Number.isFinite(scale) || scale === 0) return c
  return { x: c.x + ex / scale, y: c.y + ey / scale }
}

/** Edge drawn along dagre's routing waypoints (carried in data.points) so it
 *  threads between ranks at chip zoom. Two cases fall back to a centre-to-centre
 *  line instead: (1) LOD/dot zoom — chips collapse to a centred dot and the
 *  routed path ends ~84px away at the box border, so it would read as
 *  disconnected; (2) the cheap fallback layout, which has no waypoints. Either
 *  way the link visibly joins the two nodes. Colour/thickness ride the forwarded
 *  style; the flow animation is React Flow's native `animated` flag. */
function RoutedEdge({ source, target, data, style }: EdgeProps) {
  const compact = useStore((s) => s.transform[2] < LOD_ZOOM)
  const s = useInternalNode(source)
  const t = useInternalNode(target)
  if (!s || !t) return null
  const pts = (data?.points as { x: number; y: number }[] | undefined) ?? []
  // Routed (chip zoom, with waypoints): keep dagre's middle routing, but pin the
  // endpoints to each node's BORDER toward the adjacent waypoint — so links meet
  // the chip edge (not its centre) and still follow a dragged node.
  if (!compact && pts.length >= 2) {
    const a = borderToward(s, pts[1] ?? nodeCenter(t))
    const b = borderToward(t, pts[pts.length - 2] ?? nodeCenter(s))
    return <BaseEdge path={routedPath([a, ...pts.slice(1, -1), b])} style={style} />
  }
  // LOD dot zoom, or the fallback layout with no waypoints: plain centre line.
  const a = nodeCenter(s)
  const b = nodeCenter(t)
  return <BaseEdge path={`M${a.x},${a.y} L${b.x},${b.y}`} style={style} />
}

const EDGE_TYPES: EdgeTypes = { routed: RoutedEdge }

function isEdgeFailing(status: string | undefined): boolean {
  return status === 'critical' || status === 'failing'
}

/** Minimap dot colour by status — token fill via a CSS class (presentation
 *  attrs can't read CSS vars, so MiniMap's nodeColor can't carry a token). */
function miniNodeClass(n: Node): string {
  const status = nodeStatus((n.data as ServiceNodeData).node.status)
  if (status === 'critical') return styles.miniCrit
  if (status === 'degraded') return styles.miniWarn
  if (status === 'healthy') return styles.miniOk
  return styles.miniUnknown
}

function FlowMapInner({
  nodes,
  edges,
  selectedId,
  impact,
  dim,
  onSelect,
  onClearSelection,
  ref,
}: Readonly<
  Pick<FlowMapProps, 'nodes' | 'edges' | 'selectedId' | 'impact' | 'dim' | 'onSelect' | 'onClearSelection' | 'ref'>
>) {
  const { fitView } = useReactFlow()
  useImperativeHandle(ref, () => ({ fit: () => void fitView(FIT_OPTIONS) }), [fitView])

  const edgeRefs: GraphEdgeRef[] = useMemo(
    () => edges.map((e) => ({ source: e.source, target: e.target })),
    [edges],
  )

  // Layout: recomputed ONLY when the node/edge SET changes (deterministic
  // dagre). Carries both node positions and edge routing waypoints.
  const shapeKey = useMemo(() => {
    const ids = nodes.map((n) => n.id).sort(compareIds)
    const es = edgeRefs.map((e) => `${e.source}->${e.target}`).sort()
    return JSON.stringify({ ids, es })
  }, [nodes, edgeRefs])
  // dagre layout, computed synchronously. It's deterministic and instant at
  // normal scale; the previous Web Worker added cross-browser fragility for a
  // benefit (avoiding a ~0.5–1.2s freeze only at ~120 services) that doesn't
  // apply here, so it was removed for this simpler, robust path.
  const layout = useMemo(() => {
    const { ids } = JSON.parse(shapeKey) as { ids: string[] }
    return layoutDagre(ids, edgeRefs)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shapeKey])

  const dimField = dim ?? selectedId !== null
  const neighbors = useMemo(
    () => (selectedId ? neighborsOf(edgeRefs, selectedId) : null),
    [edgeRefs, selectedId],
  )

  const desired: ServiceNode[] = useMemo(() => {
    return nodes.map((n) => {
      const p = layout.nodes.get(n.id)
      const depth = impact?.get(n.id)
      let emphasis: Emphasis = 'normal'
      if (impact) emphasis = depth === undefined ? 'dim' : 'normal'
      else if (selectedId === n.id) emphasis = 'selected'
      else if (neighbors?.has(n.id)) emphasis = 'neighbor'
      else if (dimField) emphasis = 'dim'
      return {
        id: n.id,
        type: 'service',
        position: { x: p?.x ?? 0, y: p?.y ?? 0 },
        width: NODE_W,
        height: NODE_H,
        data: { node: n, emphasis, impactDepth: depth ?? null },
        connectable: false,
        focusable: true,
      }
    })
  }, [nodes, layout, selectedId, neighbors, impact, dimField])

  // React Flow owns node state; we reconcile `desired` into it. Re-seed
  // positions when a FRESH layout lands (worker result or topology change) or on
  // first mount; otherwise (a metric-only poll) keep the current positions so a
  // user's DRAG survives, and carry React Flow's measured dims (else every poll
  // forces a full re-measure of the graph).
  const [rfNodes, setRfNodes, onNodesChange] = useNodesState<ServiceNode>([])
  const layoutRef = useRef(layout)
  useEffect(() => {
    setRfNodes((prev) => {
      const reseed = layoutRef.current !== layout || prev.length === 0
      layoutRef.current = layout
      if (reseed) return desired
      const prevById = new Map(prev.map((n) => [n.id, n]))
      return desired.map((n) => {
        const p = prevById.get(n.id)
        return p ? { ...n, position: p.position, measured: p.measured, width: p.width, height: p.height } : n
      })
    })
  }, [desired, layout, setRfNodes])

  // Re-fit when the layout changes (topology change). Stable on metric-only
  // polls (layout ref unchanged), so it doesn't fight the user's zoom. rAF lets
  // the nodes settle into the new positions before fitting.
  useEffect(() => {
    const raf = requestAnimationFrame(() => void fitView(FIT_OPTIONS))
    return () => cancelAnimationFrame(raf)
  }, [layout, fitView])

  // Routed animated edges. Waypoints come from the dagre layout; token colour
  // rides the inline style (CSS custom props resolve in the style property,
  // unlike SVG presentation attributes); width is capped so high call-counts
  // stay a hairline. Failing edges carry --crit + an `edgeFailing` class hook.
  const rfEdges: Edge[] = useMemo(
    () =>
      edges.map((e) => {
        const failing = isEdgeFailing(e.status)
        const key = `${e.source}->${e.target}`
        return {
          id: key,
          source: e.source,
          target: e.target,
          type: 'routed',
          animated: true,
          className: failing ? 'edgeFailing' : undefined,
          data: { points: layout.edges.get(key) ?? [] },
          style: {
            stroke: failing ? 'var(--crit)' : 'var(--text-3)',
            strokeWidth: Math.min(2.25, edgeWidth(e.call_count)),
            opacity: failing ? 1 : 0.7,
          } as CSSProperties,
        }
      }),
    [edges, layout],
  )

  const onNodeClick = useCallback((_: unknown, n: Node) => onSelect(n.id), [onSelect])

  return (
    <ReactFlow
      nodes={rfNodes}
      edges={rfEdges}
      onNodesChange={onNodesChange}
      nodeTypes={NODE_TYPES}
      edgeTypes={EDGE_TYPES}
      onNodeClick={onNodeClick}
      onPaneClick={onClearSelection}
      nodesDraggable
      nodesConnectable={false}
      elementsSelectable={false}
      fitView
      fitViewOptions={FIT_OPTIONS}
      minZoom={0.2}
      maxZoom={2.5}
      proOptions={{ hideAttribution: true }}
    >
      <Background variant={BackgroundVariant.Lines} gap={32} className={styles.grid} />
      <Controls
        showInteractive={false}
        position="bottom-left"
        className={styles.controls}
        aria-label="Map zoom controls"
      />
      <MiniMap
        position="bottom-right"
        className={styles.minimap}
        nodeClassName={miniNodeClass}
        nodeStrokeWidth={0}
        pannable
        zoomable
        ariaLabel="Service map minimap"
      />
    </ReactFlow>
  )
}

/** Mounts inside its own ReactFlowProvider so the fit() ref handle works. */
export default function FlowMap({
  nodes,
  edges,
  selectedId,
  impact,
  dim,
  onSelect,
  onClearSelection,
  ref,
}: Readonly<FlowMapProps>) {
  return (
    <div
      className={styles.container}
      role="application"
      aria-label="Service flow map"
      data-testid="flow-map"
    >
      <ReactFlowProvider>
        <FlowMapInner
          nodes={nodes}
          edges={edges}
          selectedId={selectedId}
          impact={impact}
          dim={dim}
          onSelect={onSelect}
          onClearSelection={onClearSelection}
          ref={ref}
        />
      </ReactFlowProvider>
    </div>
  )
}
