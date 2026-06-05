import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import cytoscape from 'cytoscape'
import coseBilkent from 'cytoscape-cose-bilkent'
import { Card, Stat } from '@ossrandom/design-system'
import { readChartTheme, onThemeChange } from '@ossrandom/design-system/charts'
import type { ChartTheme } from '@ossrandom/design-system/charts'
import type { SystemNode, SystemEdge } from '../../types/api'
import { LAYER } from '../../lib/layers'

// Note: the ambient type for 'cytoscape-cose-bilkent' lives in
// src/types/cytoscape-cose-bilkent.d.ts (a standalone declaration — declaring it
// inline here would be an illegal augmentation of an untyped module).

// Register the layout once. cytoscape.use() throws if the same extension name
// is registered twice (StrictMode double-mount / HMR), so guard with a flag.
let coseRegistered = false
function ensureCose(): void {
  if (coseRegistered) return
  try {
    cytoscape.use(coseBilkent)
  } catch {
    // already registered in this module realm — safe to ignore
  }
  coseRegistered = true
}

// Status → on-brand fill. Mirrors ServicesView.toNodeStatus output values.
type NodeStatus = 'healthy' | 'degraded' | 'failing' | 'unknown'

interface ServiceGraphProps {
  nodes: SystemNode[]
  edges: SystemEdge[]
  /** Maps a raw node status string to one of the 4 canonical buckets. */
  toNodeStatus: (status: string | undefined) => NodeStatus
  /** True when edge.status is critical/failing (danger styling). */
  isEdgeFailing: (status: string | undefined) => boolean
  height: number
  /** Click (tap) on a node id — opens the side panel in the parent. */
  onSelect: (id: string) => void
}

// Above this many nodes, labels stay hidden until hover/focus or deep zoom —
// keeps 200 dots legible. Below it, labels show whenever they'd be readable.
const LABEL_LOD_NODE_CAP = 120
// Don't paint a label smaller than this on-screen px size; below it cytoscape
// drops the text entirely (also our zoom-based LOD).
const MIN_LABEL_PX = 11

// Degree → diameter. sqrt keeps very high-degree hubs from dwarfing the rest;
// clamp bounds the range so a 0-degree node is still visible and a 200-degree
// hub stays on-screen. diameter = clamp(16 + 7*sqrt(degree), 16, 64).
function degreeToDiameter(degree: number): number {
  const d = 16 + 7 * Math.sqrt(degree)
  return Math.max(16, Math.min(64, Math.round(d)))
}

interface HoverState {
  id: string
  node: SystemNode
  x: number
  y: number
}

const ServiceGraph: React.FC<ServiceGraphProps> = ({
  nodes,
  edges,
  toNodeStatus,
  isEdgeFailing,
  height,
  onSelect,
}) => {
  const containerRef = useRef<HTMLDivElement | null>(null)
  const cyRef = useRef<cytoscape.Core | null>(null)
  const [theme, setTheme] = useState<ChartTheme>(() => readChartTheme())
  const [hover, setHover] = useState<HoverState | null>(null)

  // Re-snapshot the chart theme when the user toggles light/dark so the canvas
  // re-themes in lockstep with the rest of the DS surface.
  useEffect(() => onThemeChange(() => setTheme(readChartTheme())), [])

  const nodeById = useMemo(() => {
    const m = new Map<string, SystemNode>()
    for (const n of nodes) m.set(n.id, n)
    return m
  }, [nodes])

  // Degree = count of edges touching the node (source OR target). Stable per
  // (nodes, edges) so the element array below doesn't churn on every render.
  const degreeById = useMemo(() => {
    const deg = new Map<string, number>()
    for (const n of nodes) deg.set(n.id, 0)
    for (const e of edges) {
      if (deg.has(e.source)) deg.set(e.source, (deg.get(e.source) ?? 0) + 1)
      if (deg.has(e.target)) deg.set(e.target, (deg.get(e.target) ?? 0) + 1)
    }
    return deg
  }, [nodes, edges])

  // Memoized element array — only rebuilt when topology actually changes, so
  // the cytoscape instance below is not torn down on unrelated parent renders.
  const elements = useMemo<cytoscape.ElementDefinition[]>(() => {
    const known = new Set(nodes.map((n) => n.id))
    const nodeEls: cytoscape.ElementDefinition[] = nodes.map((n) => ({
      group: 'nodes',
      data: {
        id: n.id,
        label: n.id,
        status: toNodeStatus(n.status),
        diameter: degreeToDiameter(degreeById.get(n.id) ?? 0),
      },
    }))
    const edgeEls: cytoscape.ElementDefinition[] = edges
      .filter((e) => known.has(e.source) && known.has(e.target))
      .map((e, i) => ({
        group: 'edges',
        data: {
          id: `e${i}-${e.source}-${e.target}`,
          source: e.source,
          target: e.target,
          callCount: e.call_count,
          failing: isEdgeFailing(e.status),
        },
      }))
    return [...nodeEls, ...edgeEls]
  }, [nodes, edges, degreeById, toNodeStatus, isEdgeFailing])

  // Number of nodes drives the label-LOD branch in the stylesheet below.
  const aboveLabelCap = nodes.length > LABEL_LOD_NODE_CAP

  const stylesheet = useMemo<cytoscape.StylesheetJson>(() => {
    const fillFor = (status: NodeStatus): string => {
      switch (status) {
        case 'healthy':
          return theme.success
        case 'degraded':
          return theme.warning
        case 'failing':
          return theme.danger
        default:
          return theme.fg4
      }
    }
    // Base label opacity: hidden above the node cap (revealed on focus/hover),
    // shown otherwise but still gated by min-zoomed-font-size for zoom LOD.
    const baseLabelOpacity = aboveLabelCap ? 0 : 1
    return [
      {
        selector: 'node',
        style: {
          width: 'data(diameter)',
          height: 'data(diameter)',
          'background-color': (ele) =>
            fillFor(ele.data('status') as NodeStatus),
          'border-width': 1,
          'border-color': theme.bg1,
          label: 'data(label)',
          color: theme.fg2,
          'font-size': 11,
          'font-family': theme.fontMono,
          'text-valign': 'bottom',
          'text-halign': 'center',
          'text-margin-y': 3,
          'text-opacity': baseLabelOpacity,
          'min-zoomed-font-size': MIN_LABEL_PX,
          'transition-property': 'opacity, text-opacity, background-color',
          'transition-duration': 0,
        } as cytoscape.Css.Node,
      },
      {
        selector: 'edge',
        style: {
          'curve-style': 'bezier',
          'target-arrow-shape': 'triangle',
          width: 'mapData(callCount, 0, 1000, 1, 6)',
          'line-color': theme.border2,
          'target-arrow-color': theme.border2,
          'arrow-scale': 0.8,
          opacity: 0.55,
          'transition-property': 'opacity, line-color',
          'transition-duration': 0,
        } as cytoscape.Css.Edge,
      },
      {
        selector: 'edge[?failing]',
        style: {
          'line-color': theme.danger,
          'target-arrow-color': theme.danger,
          opacity: 0.85,
        } as cytoscape.Css.Edge,
      },
      // Focus cluster: emphasised node, its neighbours, everything else faded.
      {
        selector: 'node.faded',
        style: { opacity: 0.18, 'text-opacity': 0 } as cytoscape.Css.Node,
      },
      {
        selector: 'edge.faded',
        style: { opacity: 0.06 } as cytoscape.Css.Edge,
      },
      {
        selector: 'node.focus',
        style: {
          'border-width': 3,
          'border-color': theme.accent,
          'text-opacity': 1,
          'z-index': 10,
        } as cytoscape.Css.Node,
      },
      {
        selector: 'node.neighbor',
        style: { 'text-opacity': 1, 'z-index': 5 } as cytoscape.Css.Node,
      },
      {
        selector: 'edge.highlight',
        style: {
          'line-color': theme.accent,
          'target-arrow-color': theme.accent,
          opacity: 1,
          'z-index': 5,
        } as cytoscape.Css.Edge,
      },
    ]
  }, [theme, aboveLabelCap])

  // Clear all focus/fade classes and the hover chip.
  const clearFocus = useCallback((cy: cytoscape.Core) => {
    cy.batch(() => {
      cy.elements().removeClass('faded focus neighbor highlight')
    })
    setHover(null)
  }, [])

  // Apply focus styling for one node id, optionally placing the hover chip.
  const applyFocus = useCallback(
    (cy: cytoscape.Core, id: string, withChip: boolean) => {
      const node = cy.getElementById(id)
      if (node.empty()) return
      const closed = node.closedNeighborhood()
      const neighbors = node.openNeighborhood().nodes()
      cy.batch(() => {
        cy.elements().addClass('faded')
        closed.removeClass('faded')
        neighbors.addClass('neighbor')
        node.removeClass('neighbor').addClass('focus')
        node.connectedEdges().addClass('highlight')
      })
      if (withChip) {
        const data = nodeById.get(id)
        const pos = node.renderedPosition()
        if (data) setHover({ id, node: data, x: pos.x, y: pos.y })
      }
    },
    [nodeById],
  )

  // Create the cytoscape instance once per (elements, stylesheet, height)
  // identity. Element/stylesheet arrays are memoized above so this effect does
  // not re-run on unrelated parent renders — the known perf trap.
  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    ensureCose()

    const cy = cytoscape({
      container,
      elements,
      style: stylesheet,
      // Smallest graphs read better as a tidy ring than a force layout.
      layout:
        elements.filter((e) => e.group === 'nodes').length <= 6
          ? ({ name: 'concentric', animate: false, padding: 24 } as cytoscape.LayoutOptions)
          : ({
              name: 'cose-bilkent',
              animate: false,
              fit: true,
              padding: 24,
              nodeDimensionsIncludeLabels: false,
              idealEdgeLength: 90,
              nodeRepulsion: 6500,
              randomize: true,
            } as unknown as cytoscape.LayoutOptions),
      // Perf at 200 nodes — single static layout, no motion, light raster.
      textureOnViewport: true,
      hideEdgesOnViewport: true,
      motionBlur: false,
      pixelRatio: 1,
      wheelSensitivity: 0.2,
      minZoom: 0.1,
      maxZoom: 4,
      autoungrabify: true,
    })
    cyRef.current = cy

    cy.on('layoutstop', () => cy.fit(undefined, 24))

    // Hover → focus + stat chip (chip never covers the node, see render).
    cy.on('mouseover', 'node', (evt: cytoscape.EventObjectNode) => {
      applyFocus(cy, evt.target.id(), true)
    })
    cy.on('mouseout', 'node', () => clearFocus(cy))
    // Keep the chip glued to the node while panning/zooming during hover.
    cy.on('pan zoom', () => {
      setHover((h) => {
        if (!h) return h
        const n = cy.getElementById(h.id)
        if (n.empty()) return h
        const p = n.renderedPosition()
        return { ...h, x: p.x, y: p.y }
      })
    })
    // Tap → persistent select (parent opens the Drawer).
    cy.on('tap', 'node', (evt: cytoscape.EventObjectNode) => {
      onSelect(evt.target.id())
    })

    // Keep every node on-screen as the container resizes (1 → 200 range).
    const ro = new ResizeObserver(() => {
      cy.resize()
      cy.fit(undefined, 24)
    })
    ro.observe(container)

    return () => {
      ro.disconnect()
      cy.destroy()
      cyRef.current = null
    }
  }, [elements, stylesheet, applyFocus, clearFocus, onSelect])

  // Hover chip position: offset up-right from the node, flipped to the other
  // side near a viewport edge so it NEVER overlaps the dot it describes.
  const chip = useMemo(() => {
    if (!hover) return null
    const w = 200
    const h = 96
    const pad = 12
    const cw = containerRef.current?.clientWidth ?? 0
    const ch = containerRef.current?.clientHeight ?? height
    let left = hover.x + 18
    let top = hover.y - h - 12
    if (left + w + pad > cw) left = hover.x - w - 18
    if (left < pad) left = pad
    if (top < pad) top = hover.y + 18
    if (top + h + pad > ch) top = Math.max(pad, ch - h - pad)
    return { left, top, w }
  }, [hover, height])

  return (
    <div style={{ position: 'relative', width: '100%', height }}>
      <div
        ref={containerRef}
        style={{ position: 'absolute', inset: 0 }}
        aria-hidden="true"
      />
      {hover && chip && (
        <div
          style={{
            position: 'absolute',
            left: chip.left,
            top: chip.top,
            width: chip.w,
            zIndex: LAYER.mapHoverPanel,
            pointerEvents: 'none',
          }}
        >
          <Card bordered padding="sm" radius="md" shadow="md">
            <Stat
              label={hover.id}
              value={Math.round(hover.node.metrics.request_rate_rps)}
              unit="req/s"
            />
            <Stat
              label="Error rate"
              value={(hover.node.metrics.error_rate * 100).toFixed(2)}
              unit="%"
            />
          </Card>
        </div>
      )}
    </div>
  )
}

export default React.memo(ServiceGraph)
