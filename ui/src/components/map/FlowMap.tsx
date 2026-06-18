import {
  useCallback,
  useImperativeHandle,
  useMemo,
  type CSSProperties,
  type ReactNode,
  type Ref,
} from 'react'
import {
  Background,
  BackgroundVariant,
  Handle,
  Position,
  ReactFlow,
  ReactFlowProvider,
  getStraightPath,
  useInternalNode,
  useReactFlow,
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

// React Flow service map. We keep our OWN deterministic radial layout
// (radialLayout.ts — phyllotaxis, no physics) and feed those fixed positions to
// React Flow, which owns the boilerplate: pan / zoom / pinch, fit-view, the grid
// background, the minimap and a11y focus. Nodes are React components, so the
// token-themed status chips port over directly; edges are straight, drawn
// centre-to-centre (chips are opaque, so the segment under a chip is occluded).

export type LayoutMode = 'radial' | 'dag'

export interface FlowMapHandle {
  /** Fit the whole graph into the current viewport. */
  fit: () => void
}

interface FlowMapProps {
  nodes: readonly SystemNode[]
  edges: readonly SystemEdge[]
  /** Layout paradigm. Only 'radial' is used today; kept for API stability. */
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

/** Centre of an internal node in flow coordinates. */
function nodeCentre(n: InternalNode): { x: number; y: number } {
  return {
    x: n.internals.positionAbsolute.x + (n.measured?.width ?? NODE_W) / 2,
    y: n.internals.positionAbsolute.y + (n.measured?.height ?? NODE_H) / 2,
  }
}

/** The token-themed status chip — a React Flow custom node. */
function ServiceMapNode({ data }: NodeProps<ServiceNode>) {
  const { node, emphasis, impactDepth } = data
  const status = nodeStatus(node.status)
  const cls = [
    styles.chip,
    emphasis === 'selected' && styles.selected,
    emphasis === 'dim' && styles.dimmed,
    impactDepth === 0 && styles.impactRoot,
  ]
    .filter(Boolean)
    .join(' ')
  return (
    <div
      className={cls}
      style={{ '--node-color': statusToken(status), width: NODE_W, height: NODE_H } as CSSProperties}
    >
      {/* Hidden centred handles — edges attach here; the floating edge draws
          centre-to-centre regardless, so position is cosmetic. */}
      <Handle type="target" position={Position.Top} className={styles.handle} isConnectable={false} />
      <Handle type="source" position={Position.Top} className={styles.handle} isConnectable={false} />
      <span className={styles.dot} aria-hidden="true" />
      <span className={styles.name}>{node.id}</span>
      <span className={styles.err}>{formatPercent(node.metrics.error_rate)}</span>
    </div>
  )
}

/** Straight edge drawn centre-to-centre between the two node boxes. */
function FloatingEdge({ source, target, data }: EdgeProps) {
  const s = useInternalNode(source)
  const t = useInternalNode(target)
  if (!s || !t) return null
  const a = nodeCentre(s)
  const b = nodeCentre(t)
  const [d] = getStraightPath({ sourceX: a.x, sourceY: a.y, targetX: b.x, targetY: b.y })
  const failing = Boolean(data?.failing)
  return (
    <path
      d={d}
      fill="none"
      className={`${styles.edge} ${failing ? styles.edgeFailing : ''}`}
      style={{ '--edge-w': data?.w as number } as CSSProperties}
    />
  )
}

const NODE_TYPES: NodeTypes = { service: ServiceMapNode }
const EDGE_TYPES: EdgeTypes = { floating: FloatingEdge }

function isEdgeFailing(status: string | undefined): boolean {
  return status === 'critical' || status === 'failing'
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

  // Positions: recomputed ONLY when the node/edge SET changes (deterministic
  // radial layout). Metric-only polls reuse these via the data memo below.
  const shapeKey = useMemo(() => {
    const ids = nodes.map((n) => n.id).sort(compareIds)
    const es = edgeRefs.map((e) => `${e.source}->${e.target}`).sort()
    return JSON.stringify({ ids, es })
  }, [nodes, edgeRefs])
  const positions = useMemo(() => {
    const { ids } = JSON.parse(shapeKey) as { ids: string[] }
    return layoutRadial(ids, edgeRefs).nodes
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shapeKey])

  const dimField = dim ?? selectedId !== null
  const neighbors = useMemo(
    () => (selectedId ? neighborsOf(edgeRefs, selectedId) : null),
    [edgeRefs, selectedId],
  )

  const rfNodes: ServiceNode[] = useMemo(() => {
    return nodes.map((n) => {
      const p = positions.get(n.id)
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
        data: { node: n, emphasis, impactDepth: depth ?? null },
        draggable: false,
        selectable: false,
        connectable: false,
        focusable: true,
      }
    })
  }, [nodes, positions, selectedId, neighbors, impact, dimField])

  const rfEdges: Edge[] = useMemo(
    () =>
      edges.map((e) => ({
        id: `${e.source}->${e.target}`,
        source: e.source,
        target: e.target,
        type: 'floating',
        data: { failing: isEdgeFailing(e.status), w: edgeWidth(e.call_count) },
      })),
    [edges],
  )

  const onNodeClick = useCallback(
    (_: unknown, n: Node) => onSelect(n.id),
    [onSelect],
  )

  return (
    <ReactFlow
      nodes={rfNodes}
      edges={rfEdges}
      nodeTypes={NODE_TYPES}
      edgeTypes={EDGE_TYPES}
      onNodeClick={onNodeClick}
      onPaneClick={onClearSelection}
      nodesDraggable={false}
      nodesConnectable={false}
      elementsSelectable={false}
      fitView
      fitViewOptions={FIT_OPTIONS}
      minZoom={0.2}
      maxZoom={2.5}
      proOptions={{ hideAttribution: true }}
    >
      <Background variant={BackgroundVariant.Lines} gap={32} className={styles.grid} />
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
