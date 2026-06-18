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
    renderMap()
    // React Flow mounts nodes after measuring the pane — use findBy.
    expect(await screen.findByText('checkout')).toBeInTheDocument()
    expect(await screen.findByText('payments')).toBeInTheDocument()
    expect(await screen.findByText('db')).toBeInTheDocument()
  })

  it('wraps each service node with its stable React Flow data-id', async () => {
    const { container } = renderMap()
    await screen.findByText('payments')
    for (const id of ['checkout', 'payments', 'db']) {
      expect(container.querySelector(`.react-flow__node[data-id="${id}"]`)).not.toBeNull()
    }
  })

  it('renders one edge path per dependency', async () => {
    const { container } = renderMap()
    // Wait for nodes (edges render in the same pass once positions exist).
    await screen.findByText('payments')
    await waitFor(() => {
      expect(
        container.querySelectorAll('.react-flow__edges path').length,
      ).toBe(EDGES.length)
    })
  })

  it('marks exactly the failing edge and leaves the healthy one alone', async () => {
    const { container } = renderMap()
    await screen.findByText('payments')
    // React Flow joins the edge id with a NUL, not a space, so an exact
    // [data-id="payments db"] match fails — instead count the edge <path>s and
    // match the failing one by the *unhashed* CSS-module token (`edgeFailing`)
    // via [class*=…], which survives the hash suffix. EDGES has one critical
    // (payments→db) edge, so exactly one path should be marked failing.
    await waitFor(() => {
      const paths = container.querySelectorAll('.react-flow__edges path')
      expect(paths.length).toBe(EDGES.length)
    })
    expect(
      container.querySelectorAll('.react-flow__edges path[class*="edgeFailing"]'),
    ).toHaveLength(1)
  })
})

describe('FlowMap — selection', () => {
  it('node click fires onSelect with the id', async () => {
    const { onSelect } = renderMap()
    // fireEvent.click (not userEvent) — userEvent dispatches a full
    // mousedown→mouseup→click sequence, and d3-zoom's mousedown handler reads
    // event.view.document, which jsdom leaves null → an uncaught TypeError.
    // React Flow's onNodeClick fires on the click event alone, so a bare
    // click both avoids the crash and exercises the handler. The click on the
    // inner chip text bubbles to the React Flow node wrapper → onNodeClick.
    fireEvent.click(await screen.findByText('payments'))
    expect(onSelect).toHaveBeenCalledWith('payments')
  })

  it('pane click clears the selection', async () => {
    const { container, onClearSelection } = renderMap({ selectedId: 'payments' })
    await screen.findByText('payments')
    const pane = container.querySelector('.react-flow__pane')
    expect(pane).not.toBeNull()
    fireEvent.click(pane as Element)
    expect(onClearSelection).toHaveBeenCalled()
  })

  it('still renders the selected node (selection is a non-fatal data prop)', async () => {
    // Class names are hashed, so asserting the selected styling is brittle —
    // we only verify the selected node continues to render.
    const { container } = renderMap({ selectedId: 'payments' })
    await screen.findByText('payments')
    expect(
      container.querySelector('.react-flow__node[data-id="payments"]'),
    ).not.toBeNull()
  })
})

describe('FlowMap — imperative fit()', () => {
  it('exposes fit() on the handle and runs without throwing', async () => {
    const ref = createRef<FlowMapHandle>()
    render(
      <FlowMap
        ref={ref}
        nodes={NODES}
        edges={EDGES}
        selectedId={null}
        onSelect={noop}
        onClearSelection={noop}
      />,
    )
    await screen.findByText('payments')
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
