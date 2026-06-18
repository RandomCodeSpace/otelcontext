import { createRef } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import FlowMap, { type FlowMapHandle } from '../FlowMap'
import type { SystemEdge, SystemNode } from '@/types/api'

// Canvas-level coverage for the FlowMap component in isolation (the data /
// route / filter shell lives in ConstellationHome; this exercises the SVG
// field, polar keyboard, pan/zoom, selection→dim, the core slot + fallback,
// and the imperative fit()). Migrated from the retiring FlowMapView.test.

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
  const onFocusFilter = overrides.onFocusFilter ?? vi.fn()
  const utils = render(
    <FlowMap
      nodes={NODES}
      edges={EDGES}
      selectedId={null}
      onSelect={onSelect}
      onClearSelection={onClearSelection}
      onFocusFilter={onFocusFilter}
      {...overrides}
    />,
  )
  return { ...utils, onSelect, onClearSelection, onFocusFilter }
}

function getMap() {
  return screen.getByRole('application', { name: /service flow map/i })
}

function innerG(): SVGGElement {
  const svg = screen.getByTestId('flow-map-svg')
  return svg.querySelector('g') as SVGGElement
}

describe('FlowMap — radial render', () => {
  it('renders the radial dial, a node per service, and an edge per dependency', () => {
    renderMap()
    const map = getMap()
    expect(screen.getByTestId('radial-dial')).toBeInTheDocument()
    expect(within(map).getByText('checkout')).toBeInTheDocument()
    expect(within(map).getByText('payments')).toBeInTheDocument()
    expect(within(map).getByText('db')).toBeInTheDocument()
    const svg = screen.getByTestId('flow-map-svg')
    // Dial circles are <circle>; dependency edges are <path>.
    expect(svg.querySelectorAll('path')).toHaveLength(2)
  })

  it('places nodes via a CSS transform (interpolatable), not the SVG attr', () => {
    renderMap()
    const g = screen
      .getByTestId('flow-map-svg')
      .querySelector('[data-node-id="db"]') as SVGGElement
    expect(g.getAttribute('transform')).toBeNull()
    expect(g.style.transform).toMatch(/translate\(/)
  })

  it('marks the failing edge and leaves the healthy one alone', () => {
    renderMap()
    const svg = screen.getByTestId('flow-map-svg')
    const failing = [...svg.querySelectorAll('path')].filter((p) =>
      p.getAttribute('class')?.includes('edgeFailing'),
    )
    expect(failing).toHaveLength(1)
  })

  it('shows err% labels at default zoom (semantic zoom ≥ 0.8)', () => {
    renderMap()
    expect(screen.getByText('4.2%')).toBeInTheDocument()
  })

  it('truncates a long node name so it never runs under the err% label', () => {
    renderMap({
      nodes: [...NODES, node('inventory-service', 'critical', 0.456)],
    })
    // err% shows at default zoom, so the name is clipped with an ellipsis…
    expect(screen.getByText('inventory…')).toBeInTheDocument()
    // …and the full untruncated name is NOT rendered (would overlap "45.6%").
    expect(screen.queryByText('inventory-service')).not.toBeInTheDocument()
  })

  it('carries the ring/rank in node aria-labels (radial)', () => {
    renderMap()
    const svg = screen.getByTestId('flow-map-svg')
    const labels = [...svg.querySelectorAll('[data-node-id]')].map((g) =>
      g.getAttribute('aria-label'),
    )
    expect(labels.some((l) => l?.includes('ring 0') && l?.includes('most critical'))).toBe(true)
  })
})

describe('FlowMap — selection & dim', () => {
  it('node click fires onSelect with the id', async () => {
    const user = userEvent.setup()
    const { onSelect } = renderMap()
    await user.click(within(getMap()).getByText('payments'))
    expect(onSelect).toHaveBeenCalledWith('payments')
  })

  it('dims nodes outside the selected 1-hop neighborhood by default', () => {
    renderMap({ selectedId: 'checkout' })
    const svg = screen.getByTestId('flow-map-svg')
    expect(
      svg.querySelector('[data-node-id="db"]')?.getAttribute('class'),
    ).toContain('dimmed')
    expect(
      svg.querySelector('[data-node-id="payments"]')?.getAttribute('class'),
    ).not.toContain('dimmed')
  })

  it('dim={false} suppresses the field-wide focus-distortion even with a selection', () => {
    renderMap({ selectedId: 'checkout', dim: false })
    const svg = screen.getByTestId('flow-map-svg')
    expect(
      svg.querySelector('[data-node-id="db"]')?.getAttribute('class'),
    ).not.toContain('dimmed')
  })

  it('marks ONLY the selected node group with data-selected (the lift hook)', () => {
    renderMap({ selectedId: 'payments' })
    const svg = screen.getByTestId('flow-map-svg')
    expect(
      svg.querySelector('[data-node-id="payments"]')?.hasAttribute('data-selected'),
    ).toBe(true)
    expect(
      svg.querySelector('[data-node-id="checkout"]')?.hasAttribute('data-selected'),
    ).toBe(false)
    expect(
      svg.querySelector('[data-node-id="db"]')?.hasAttribute('data-selected'),
    ).toBe(false)
  })

  it('carries no data-selected on any node when nothing is selected', () => {
    renderMap()
    const svg = screen.getByTestId('flow-map-svg')
    expect(svg.querySelectorAll('[data-node-id][data-selected]')).toHaveLength(0)
  })
})

describe('FlowMap — core slot', () => {
  it('renders the HTML core overlay as a sibling of the <svg>', () => {
    renderMap({ core: <div>core content</div> })
    const overlay = screen.getByTestId('flow-map-core')
    expect(overlay).toHaveTextContent('core content')
    // It is NOT inside the svg — it pins to viewport center, field pans behind.
    expect(screen.getByTestId('flow-map-svg').contains(overlay)).toBe(false)
    // With a core provided, the in-svg text fallback is gone.
    expect(screen.queryByText('HEALTHY')).not.toBeInTheDocument()
  })

  it('falls back to the in-svg health-core text when core is undefined', () => {
    renderMap()
    expect(screen.queryByTestId('flow-map-core')).not.toBeInTheDocument()
    expect(screen.getByText('HEALTHY')).toBeInTheDocument()
    // healthyCount/total — 1 of 3 nodes is healthy.
    expect(screen.getByText('1/3')).toBeInTheDocument()
  })

  it('centerLabel={false} frees the dial center (no fallback text)', () => {
    renderMap({ centerLabel: false })
    expect(screen.queryByText('HEALTHY')).not.toBeInTheDocument()
    expect(screen.queryByText('1/3')).not.toBeInTheDocument()
    // The dial graticule still renders — only the center readout is suppressed.
    expect(screen.getByTestId('radial-dial')).toBeInTheDocument()
  })
})

describe('FlowMap — anomaly prominence', () => {
  it('tags each node with its status and pulses only the critical (failing) ones', () => {
    renderMap()
    const svg = screen.getByTestId('flow-map-svg')
    expect(svg.querySelector('[data-node-id="db"]')?.getAttribute('data-status')).toBe('critical')
    expect(svg.querySelector('[data-node-id="checkout"]')?.getAttribute('data-status')).toBe(
      'healthy',
    )
    // Only the critical node carries the pulse beacon.
    expect(screen.getByTestId('node-pulse-db')).toBeInTheDocument()
    expect(screen.queryByTestId('node-pulse-checkout')).toBeNull()
    expect(screen.queryByTestId('node-pulse-payments')).toBeNull()
  })

  it('suppresses the pulse under the impact overlay (which owns its emphasis)', () => {
    renderMap({ impact: new Map([['db', 0]]) })
    expect(screen.queryByTestId('node-pulse-db')).toBeNull()
  })
})

describe('FlowMap — galaxy dots at scale', () => {
  // A large field (> NAME_ALWAYS_MAX) so names are not force-shown; svc-00 is the
  // sole critical node.
  const many: readonly SystemNode[] = Array.from({ length: 30 }, (_, i) =>
    node(`svc-${String(i).padStart(2, '0')}`, i === 0 ? 'critical' : 'healthy'),
  )

  function renderMany(overrides: Overrides = {}) {
    return render(
      <FlowMap
        nodes={many}
        edges={[]}
        selectedId={null}
        onSelect={noop}
        onClearSelection={noop}
        onFocusFilter={noop}
        {...overrides}
      />,
    )
  }

  it('renders un-surfaced nodes as status dots (no chip label) when zoomed out', () => {
    renderMany()
    const svg = screen.getByTestId('flow-map-svg')
    // Zoom out below NAME_ZOOM (0.5): one wheel-out step → k ≈ 0.41.
    fireEvent.wheel(svg, { deltaY: 600, clientX: 0, clientY: 0 })
    // Galaxy markers (circles) replace the chips; no node-name labels remain.
    expect(svg.querySelectorAll('circle[class*="nodeMarker"]').length).toBeGreaterThan(0)
    expect(screen.queryByText('svc-01')).not.toBeInTheDocument()
    // The critical node stays obvious — it still pulses, now as a dot.
    expect(screen.getByTestId('node-pulse-svc-00')).toBeInTheDocument()
  })

  it('keeps the selected node a labeled chip even in the zoomed-out dot field', () => {
    renderMany({ selectedId: 'svc-05' })
    const svg = screen.getByTestId('flow-map-svg')
    fireEvent.wheel(svg, { deltaY: 600, clientX: 0, clientY: 0 })
    // The selection is surfaced as a chip (its label renders); the rest are dots.
    expect(screen.getByText('svc-05')).toBeInTheDocument()
    expect(svg.querySelectorAll('circle[class*="nodeMarker"]').length).toBeGreaterThan(0)
  })
})

describe('FlowMap — ringDepth', () => {
  it('opts the container into cinematic depth only when ringDepth is set', () => {
    const { rerender } = renderMap()
    expect(getMap().className).not.toMatch(/depth/)
    rerender(
      <FlowMap
        nodes={NODES}
        edges={EDGES}
        selectedId={null}
        ringDepth
        onSelect={noop}
        onClearSelection={noop}
        onFocusFilter={noop}
      />,
    )
    expect(getMap().className).toMatch(/depth/)
  })
})

describe('FlowMap — keyboard (polar walk)', () => {
  it('arrow keys walk via aria-activedescendant; Enter inspects', async () => {
    const user = userEvent.setup()
    const { onSelect } = renderMap()
    const map = getMap()
    map.focus()
    await user.keyboard('{ArrowRight}')
    const active = map.getAttribute('aria-activedescendant')
    expect(active).toMatch(/^map-node-/)
    await user.keyboard('{Enter}')
    expect(onSelect).toHaveBeenCalledWith(active!.replace('map-node-', ''))
  })

  it('a ring-out step (down) lands focus on a node', async () => {
    const user = userEvent.setup()
    renderMap()
    const map = getMap()
    map.focus()
    await user.keyboard('{ArrowRight}')
    await user.keyboard('{ArrowDown}')
    expect(map.getAttribute('aria-activedescendant')).toMatch(/^map-node-/)
  })

  // Cold-start bootstrap: Tabbing in with nothing selected, the FIRST arrow —
  // in any direction — must seed focus (not just ArrowRight). ArrowDown is the
  // most intuitive first key in a radial field and must not dead-end.
  it.each(['ArrowDown', 'ArrowUp', 'ArrowLeft', 'ArrowRight'])(
    'bootstraps focus from a cold start on %s',
    async (key) => {
      const user = userEvent.setup()
      renderMap()
      const map = getMap()
      map.focus()
      await user.keyboard(`{${key}}`)
      expect(map.getAttribute('aria-activedescendant')).toMatch(/^map-node-/)
    },
  )

  it('seeds the SAME entry node regardless of the first arrow key (deterministic)', async () => {
    const seed = async (key: string) => {
      const user = userEvent.setup()
      const { unmount } = renderMap()
      const map = getMap()
      map.focus()
      await user.keyboard(`{${key}}`)
      const active = map.getAttribute('aria-activedescendant')
      unmount()
      return active
    }
    const down = await seed('ArrowDown')
    const right = await seed('ArrowRight')
    expect(down).toMatch(/^map-node-/)
    expect(down).toBe(right)
  })

  it('Escape clears the selection', async () => {
    const user = userEvent.setup()
    const { onClearSelection } = renderMap({ selectedId: 'payments' })
    const map = getMap()
    map.focus()
    await user.keyboard('{Escape}')
    expect(onClearSelection).toHaveBeenCalled()
  })

  it('"/" focuses the filter and "f" fits without crashing', async () => {
    const user = userEvent.setup()
    const { onFocusFilter } = renderMap()
    const map = getMap()
    map.focus()
    await user.keyboard('f')
    await user.keyboard('/')
    expect(onFocusFilter).toHaveBeenCalled()
  })
})

describe('FlowMap — pan/zoom', () => {
  it('wheel zooms the inner <g> transform (no re-layout)', () => {
    renderMap()
    const svg = screen.getByTestId('flow-map-svg')
    const before = innerG().getAttribute('transform')
    fireEvent.wheel(svg, { deltaY: -100, clientX: 50, clientY: 50 })
    const after = innerG().getAttribute('transform')
    expect(after).not.toBe(before)
    expect(after).toMatch(/scale\(1\.1/)
  })

  it('zooming out below 0.8 hides the err% labels (semantic zoom)', () => {
    renderMap()
    expect(screen.getByText('4.2%')).toBeInTheDocument()
    const svg = screen.getByTestId('flow-map-svg')
    fireEvent.wheel(svg, { deltaY: 300, clientX: 0, clientY: 0 })
    expect(screen.queryByText('4.2%')).not.toBeInTheDocument()
  })

  it('single-pointer drag pans', () => {
    renderMap()
    const svg = screen.getByTestId('flow-map-svg')
    fireEvent.pointerDown(svg, { pointerId: 1, clientX: 10, clientY: 10 })
    fireEvent.pointerMove(svg, { pointerId: 1, clientX: 30, clientY: 25 })
    fireEvent.pointerUp(svg, { pointerId: 1 })
    expect(innerG().getAttribute('transform')).toContain('translate(20 15)')
  })

  it('two-pointer pinch zooms', () => {
    renderMap()
    const svg = screen.getByTestId('flow-map-svg')
    fireEvent.pointerDown(svg, { pointerId: 1, clientX: 0, clientY: 0 })
    fireEvent.pointerDown(svg, { pointerId: 2, clientX: 100, clientY: 0 })
    fireEvent.pointerMove(svg, { pointerId: 2, clientX: 200, clientY: 0 })
    fireEvent.pointerUp(svg, { pointerId: 1 })
    fireEvent.pointerUp(svg, { pointerId: 2 })
    expect(innerG().getAttribute('transform')).toMatch(/scale\(2\)/)
  })
})

describe('FlowMap — imperative fit()', () => {
  it('exposes fit() on the handle and runs without throwing', () => {
    const ref = createRef<FlowMapHandle>()
    render(
      <FlowMap
        ref={ref}
        nodes={NODES}
        edges={EDGES}
        selectedId={null}
        onSelect={noop}
        onClearSelection={noop}
        onFocusFilter={noop}
      />,
    )
    expect(ref.current).not.toBeNull()
    expect(() => ref.current!.fit()).not.toThrow()
  })
})

describe('FlowMap — ?impact= blast-radius overlay', () => {
  it('tints the downstream cone by depth and rings the root', () => {
    const impact = new Map([
      ['checkout', 0],
      ['payments', 1],
      ['db', 2],
    ])
    renderMap({ impact })
    const payTint = screen.getByTestId('impact-tint-payments')
    const dbTint = screen.getByTestId('impact-tint-db')
    expect(Number(payTint.style.fillOpacity)).toBeGreaterThan(
      Number(dbTint.style.fillOpacity),
    )
    expect(screen.queryByTestId('impact-tint-checkout')).toBeNull()
    const rootRect = screen
      .getByTestId('flow-map-svg')
      .querySelector('[data-node-id="checkout"] rect')
    expect(rootRect?.getAttribute('class')).toContain('nodeImpactRoot')
  })

  it('dims services outside the cone', () => {
    const impact = new Map([
      ['payments', 0],
      ['db', 1],
    ])
    renderMap({ impact })
    const svg = screen.getByTestId('flow-map-svg')
    expect(
      svg.querySelector('[data-node-id="checkout"]')?.getAttribute('class'),
    ).toContain('dimmed')
    expect(
      svg.querySelector('[data-node-id="db"]')?.getAttribute('class'),
    ).not.toContain('dimmed')
  })
})

describe('FlowMap — ambient-motion gate', () => {
  it('marks the container visible on mount and pauses on visibilitychange', () => {
    renderMap()
    expect(getMap().dataset.visible).toBe('true')
    const original = Object.getOwnPropertyDescriptor(
      Document.prototype,
      'visibilityState',
    )
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => 'hidden',
    })
    fireEvent(document, new Event('visibilitychange'))
    expect(getMap().dataset.visible).toBe('false')
    if (original) Object.defineProperty(document, 'visibilityState', original)
  })
})
