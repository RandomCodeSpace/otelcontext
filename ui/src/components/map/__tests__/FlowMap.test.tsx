import { createRef } from 'react'
import { beforeAll, describe, expect, it, vi } from 'vitest'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import FlowMap, { type FlowMapHandle } from '../FlowMap'
import type { SystemEdge, SystemNode } from '@/types/api'

// Integration coverage for the React Flow service map. FlowMap feeds our own
// deterministic radial layout into @xyflow/react, which owns pan/zoom, the
// grid, the minimap, and a11y focus. React Flow renders nodes ASYNCHRONOUSLY
// (it measures the pane in a post-mount effect), so every node/edge assertion
// must go through an async query — never a synchronous getBy.
//
// EDGE RENDERING + jsdom: React Flow only draws an edge once BOTH endpoint
// nodes are *measured*. Its measurement pipeline is: a ResizeObserver fires →
// reads the node wrapper's offsetWidth/Height → writes `measured` dims →
// `nodesInitialized` flips true → edges render. The shared src/test-setup.ts
// stubs ResizeObserver as a no-op (its callback never fires) and its
// offsetWidth/Height getters are guarded out by an existing zero-returning
// getter in this jsdom build, so node dims stay 0 and NO edge ever renders.
// We close that gap locally (firing RO + non-zero offsets) so the edge tests
// exercise real React Flow geometry. Behaviour-neutral — it only feeds React
// Flow the measurements a real browser would. See report: test-setup's RO/
// offset shims should be hardened so this block can move there.
beforeAll(() => {
  Object.defineProperties(HTMLElement.prototype, {
    offsetWidth: {
      configurable: true,
      get(this: HTMLElement) {
        return parseFloat(this.style?.width) || 168
      },
    },
    offsetHeight: {
      configurable: true,
      get(this: HTMLElement) {
        return parseFloat(this.style?.height) || 40
      },
    },
  })
  class FiringResizeObserver {
    private readonly cb: ResizeObserverCallback
    constructor(cb: ResizeObserverCallback) {
      this.cb = cb
    }
    observe(target: Element) {
      // Fire synchronously with the element's box so React Flow records a
      // non-zero measured size (a no-op observer leaves nodes unmeasured).
      this.cb(
        [{ target, contentRect: target.getBoundingClientRect() } as ResizeObserverEntry],
        this as unknown as ResizeObserver,
      )
    }
    unobserve() {}
    disconnect() {}
  }
  ;(globalThis as Record<string, unknown>).ResizeObserver = FiringResizeObserver
})

function node(id: string, status: string, errorRate = 0): SystemNode {
  return {
    id,
    type: 'service',
    health_score: status === 'critical' ? 0.2 : status === 'degraded' ? 0.6 : 0.97,
    status,
    metrics: {
      request_rate_rps: 10,
      error_rate: errorRate,
      avg_latency_ms: 50,
      p99_latency_ms: 120,
      span_count_1h: 100,
    },
    alerts: [],
  }
}

const NODES: readonly SystemNode[] = [
  node('checkout', 'healthy'),
  node('payments', 'degraded', 0.042),
  node('db', 'critical', 0.3),
]

const EDGES: readonly SystemEdge[] = [
  {
    source: 'checkout',
    target: 'payments',
    call_count: 1200,
    avg_latency_ms: 90,
    error_rate: 0.01,
    status: 'healthy',
  },
  {
    source: 'payments',
    target: 'db',
    call_count: 800,
    avg_latency_ms: 250,
    error_rate: 0.3,
    status: 'critical',
  },
]

const noop = () => {}

type Overrides = Partial<React.ComponentProps<typeof FlowMap>>

function renderMap(overrides: Overrides = {}) {
  const onSelect = overrides.onSelect ?? vi.fn()
  const onClearSelection = overrides.onClearSelection ?? vi.fn()
  const utils = render(
    <FlowMap
      nodes={NODES}
      edges={EDGES}
      selectedId={null}
      onSelect={onSelect}
      onClearSelection={onClearSelection}
      {...overrides}
    />,
  )
  return { ...utils, onSelect, onClearSelection }
}

function getMap() {
  return screen.getByRole('application', { name: /service flow map/i })
}

describe('FlowMap — React Flow render', () => {
  it('renders a node per service', async () => {
    const { container } = renderMap()
    // React Flow mounts nodes after measuring the pane — wait for the first to
    // appear. Assert on the `.react-flow__node` wrappers, NOT on the service
    // NAME text: at fitView zoom (and at 120-service scale generally) the map
    // is below LOD_ZOOM, so each node renders as a textless status dot. The
    // wrapper exists in BOTH the dot and the labelled-chip variant.
    await waitFor(() =>
      expect(container.querySelectorAll('.react-flow__node')).toHaveLength(NODES.length),
    )
  })

  it('wraps each service node with its stable React Flow data-id', async () => {
    const { container } = renderMap()
    await waitFor(() =>
      expect(container.querySelectorAll('.react-flow__node')).toHaveLength(NODES.length),
    )
    // Each service is addressable by its stable data-id (set from SystemNode.id),
    // which React Flow stamps on the node wrapper in both LOD variants.
    for (const id of ['checkout', 'payments', 'db']) {
      expect(container.querySelector(`.react-flow__node[data-id="${id}"]`)).not.toBeNull()
    }
  })

  it('renders one edge per dependency', async () => {
    const { container } = renderMap()
    // Wait for nodes (edges render in the same pass once positions exist).
    // Gate on the node wrappers, not name text — the dots are textless at this zoom.
    await waitFor(() =>
      expect(container.querySelectorAll('.react-flow__node')).toHaveLength(NODES.length),
    )
    // Each native edge is a `<g class="react-flow__edge">` group that holds TWO
    // <path>s (the visible `.react-flow__edge-path` + an invisible wide
    // `.react-flow__edge-interaction` hit area), so counting raw `path`s gives
    // 2× the edges. Count the edge GROUP instead — exactly one per dependency.
    await waitFor(() => {
      expect(
        container.querySelectorAll('.react-flow__edges .react-flow__edge').length,
      ).toBe(EDGES.length)
    })
  })

  it('marks exactly the failing edge and leaves the healthy one alone', async () => {
    const { container } = renderMap()
    await waitFor(() =>
      expect(container.querySelectorAll('.react-flow__node')).toHaveLength(NODES.length),
    )
    // The `edgeFailing` hook now rides the React Flow edge GROUP (the `<g
    // class="react-flow__edge …">`), not a custom edge <path> — the native
    // bezier path is React Flow's own. The class is a plain string literal
    // (not a CSS-module token), so it is unhashed and matches exactly. EDGES
    // has one critical (payments→db) edge, so exactly one group is failing.
    await waitFor(() => {
      const groups = container.querySelectorAll('.react-flow__edges .react-flow__edge')
      expect(groups.length).toBe(EDGES.length)
    })
    expect(
      container.querySelectorAll('.react-flow__edge.edgeFailing'),
    ).toHaveLength(1)
  })
})

describe('FlowMap — selection', () => {
  it('node click fires onSelect with the id', async () => {
    const { container, onSelect } = renderMap()
    // Click the node WRAPPER by its stable data-id, not by name text — the
    // textless LOD dot renders at this zoom, so there is no name to click.
    // fireEvent.click (not userEvent) — userEvent dispatches a full
    // mousedown→mouseup→click sequence, and d3-zoom's mousedown handler reads
    // event.view.document, which jsdom leaves null → an uncaught TypeError.
    // React Flow's onNodeClick fires on the click event alone, so a bare
    // click both avoids the crash and exercises the handler.
    const payments = await waitFor(() => {
      const el = container.querySelector('.react-flow__node[data-id="payments"]')
      expect(el).not.toBeNull()
      return el as Element
    })
    fireEvent.click(payments)
    expect(onSelect).toHaveBeenCalledWith('payments')
  })

  it('pane click clears the selection', async () => {
    const { container, onClearSelection } = renderMap({ selectedId: 'payments' })
    await waitFor(() =>
      expect(container.querySelectorAll('.react-flow__node')).toHaveLength(NODES.length),
    )
    const pane = container.querySelector('.react-flow__pane')
    expect(pane).not.toBeNull()
    fireEvent.click(pane as Element)
    expect(onClearSelection).toHaveBeenCalled()
  })

  it('still renders the selected node (selection is a non-fatal data prop)', async () => {
    // Class names are hashed, so asserting the selected styling is brittle —
    // we only verify the selected node continues to render (by data-id, which is
    // LOD-independent).
    const { container } = renderMap({ selectedId: 'payments' })
    await waitFor(() =>
      expect(
        container.querySelector('.react-flow__node[data-id="payments"]'),
      ).not.toBeNull(),
    )
  })
})

describe('FlowMap — imperative fit()', () => {
  it('exposes fit() on the handle and runs without throwing', async () => {
    const ref = createRef<FlowMapHandle>()
    const { container } = render(
      <FlowMap
        ref={ref}
        nodes={NODES}
        edges={EDGES}
        selectedId={null}
        onSelect={noop}
        onClearSelection={noop}
      />,
    )
    // Gate on the node wrappers (textless dots at this zoom), not name text.
    await waitFor(() =>
      expect(container.querySelectorAll('.react-flow__node')).toHaveLength(NODES.length),
    )
    expect(ref.current).not.toBeNull()
    expect(() => act(() => ref.current?.fit())).not.toThrow()
  })
})

describe('FlowMap — smoke', () => {
  it('mounts the React Flow application container', () => {
    renderMap()
    // The container itself is synchronous (only the nodes are async).
    expect(getMap()).toBeInTheDocument()
    expect(getMap()).toHaveAttribute('data-testid', 'flow-map')
  })
})
