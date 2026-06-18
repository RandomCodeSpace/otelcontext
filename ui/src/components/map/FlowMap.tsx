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
  MarkerType,
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
import { layoutRadial } from '@/lib/radialLayout'
import { formatPercent } from '@/lib/format'
import { nodeStatus, statusToken } from '@/lib/triage'
import type { SystemEdge, SystemNode } from '@/types/api'
import styles from './FlowMap.module.css'

// React Flow service map. React Flow owns pan / zoom / fit, the grid, the
// minimap and the zoom controls; we feed it a deterministic phyllotaxis
// ("sunflower") layout (radialLayout.ts) for node positions — the most-critical
// service near the centre, the rest on a golden-angle spiral that fills the disc
// EVENLY from 7 to 120+ services. Unlike a layered DAG, a sunflower never
// collapses sparse/disconnected graphs into a single column, so the map stays a
// readable disc at scale instead of a 8000px-tall line. Two readability levers:
// edges carry a direction ARROWHEAD (markerEnd) so flow is legible statically —
// at any zoom and under reduced motion — and nodes drop to status DOTS below a
// zoom threshold (level-of-detail) so the 168px chips stop occluding the field
// when zoomed out. The layout seeds the positions; nodes stay DRAGGABLE for
// manual nudges — a drag survives metric polls, and a fresh layout (topology
// change) re-seeds.

export type LayoutMode = 'radial' | 'dag'

export interface FlowMapHandle {
  /** Fit the whole graph into the current viewport. */
  fit: () => void
}

interface FlowMapProps {
  nodes: readonly SystemNode[]
  edges: readonly SystemEdge[]
  /** Layout paradigm. The sunflower (phyllotaxis) layout is used today; kept for API stability. */
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
      (emphasis === 'neighbor' || (impactDepth !== null && impactDepth > 0)) && styles.dotNeighbor,
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
    emphasis === 'neighbor' && styles.neighbor,
    emphasis === 'dim' && styles.dimmed,
    impactDepth !== null && impactDepth > 0 && styles.inCone,
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

/** Straight edge between two nodes carrying a direction ARROWHEAD. The
 *  phyllotaxis layout has no routing waypoints, so edges are chords across the
 *  disc. At chip zoom each endpoint pins to the node BORDER facing the other
 *  node, so the arrowhead lands on the chip edge and follows a dragged node. At
 *  LOD/dot zoom the chips collapse to a centred dot — the 168×40 wrapper border
 *  would leave the line ~75px short of the dot — so we connect centre-to-centre
 *  instead. Colour/thickness ride the forwarded style; markerEnd is the
 *  forwarded arrowhead; the flow animation is React Flow's native `animated`
 *  flag (a secondary cue — the arrowhead is the load-bearing one). */
function RoutedEdge({ source, target, style, markerEnd }: EdgeProps) {
  const compact = useStore((s) => s.transform[2] < LOD_ZOOM)
  const s = useInternalNode(source)
  const t = useInternalNode(target)
  if (!s || !t) return null
  if (compact) {
    const a = nodeCenter(s)
    const b = nodeCenter(t)
    return <BaseEdge path={`M${a.x},${a.y} L${b.x},${b.y}`} style={style} markerEnd={markerEnd} />
  }
  const a = borderToward(s, nodeCenter(t))
  const b = borderToward(t, nodeCenter(s))
  return <BaseEdge path={`M${a.x},${a.y} L${b.x},${b.y}`} style={style} markerEnd={markerEnd} />
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
  useImperativeHandle(ref, () => ({ fit: () => { fitView(FIT_OPTIONS) } }), [fitView])

  const edgeRefs: GraphEdgeRef[] = useMemo(
    () => edges.map((e) => ({ source: e.source, target: e.target })),
    [edges],
  )

  // Layout: recomputed ONLY when the node/edge SET changes — the phyllotaxis
  // layout is deterministic for a given node/edge set.
  const shapeKey = useMemo(() => {
    const ids = nodes.map((n) => n.id).sort(compareIds)
    const es = edgeRefs.map((e) => `${e.source}->${e.target}`).sort((a, b) => a.localeCompare(b))
    return JSON.stringify({ ids, es })
  }, [nodes, edgeRefs])
  // Phyllotaxis ("sunflower") layout, computed synchronously — deterministic and
  // instant. Edges feed criticality ranking (the most-called service lands at
  // the disc centre); positions are stable for a given node/edge set.
  const layout = useMemo(() => {
    const { ids } = JSON.parse(shapeKey) as { ids: string[] }
    return layoutRadial(ids, edgeRefs)
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
    const raf = requestAnimationFrame(() => { fitView(FIT_OPTIONS) })
    return () => cancelAnimationFrame(raf)
  }, [layout, fitView])

  // Selection-aware animated edges. With a node selected (or a blast-radius
  // cone) the ACTIVE path — 1-hop edges of the selection, or cone edges whose
  // BOTH endpoints are in the cone — is drawn in the accent hue, thicker, fully
  // opaque and animated; every off-path edge recedes to a dim hairline and stops
  // animating, so the path reads as a clear through-line. A failing edge keeps
  // its --crit hue. With nothing selected it's the neutral resting style. Token
  // colour rides the inline style (CSS custom props resolve there, unlike SVG
  // presentation attributes); width is capped so high call-counts stay a
  // hairline. Every edge carries a direction arrowhead (markerEnd) coloured to
  // match its stroke, so flow direction is legible statically — at any zoom and
  // under reduced motion. Failing edges keep the `edgeFailing` class hook.
  const rfEdges: Edge[] = useMemo(() => {
    const focusing = selectedId !== null || impact != null
    return edges.map((e) => {
      const failing = isEdgeFailing(e.status)
      const key = `${e.source}->${e.target}`
      const active = impact
        ? impact.has(e.source) && impact.has(e.target)
        : selectedId !== null && (e.source === selectedId || e.target === selectedId)
      const dimmed = focusing && !active
      const stroke = failing ? 'var(--crit)' : active ? 'var(--accent)' : 'var(--text-3)'
      const width = active
        ? Math.min(3.5, edgeWidth(e.call_count) * 1.6)
        : Math.min(2.25, edgeWidth(e.call_count))
      return {
        id: key,
        source: e.source,
        target: e.target,
        type: 'routed',
        animated: !dimmed,
        className: failing ? 'edgeFailing' : undefined,
        markerEnd: { type: MarkerType.ArrowClosed, width: 16, height: 16, color: stroke },
        style: {
          stroke,
          strokeWidth: dimmed ? 1 : width,
          opacity: dimmed ? 0.14 : failing || active ? 1 : 0.7,
        } as CSSProperties,
      }
    })
  }, [edges, selectedId, impact])

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
