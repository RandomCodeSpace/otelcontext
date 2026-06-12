import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import FlowMapView from '../FlowMapView'
import type { SystemGraphResponse, SystemNode } from '@/types/api'

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

const GRAPH: SystemGraphResponse = {
  timestamp: '2026-06-11T00:00:00Z',
  system: {
    total_services: 3,
    healthy: 1,
    degraded: 1,
    critical: 1,
    overall_health_score: 0.7,
    total_error_rate: 0.05,
    avg_latency_ms: 100,
    uptime_seconds: 60,
  },
  nodes: [node('checkout', 'healthy'), node('payments', 'degraded', 0.042), node('db', 'critical', 0.3)],
  edges: [
    { source: 'checkout', target: 'payments', call_count: 1200, avg_latency_ms: 90, error_rate: 0.01, status: 'healthy' },
    { source: 'payments', target: 'db', call_count: 800, avg_latency_ms: 250, error_rate: 0.3, status: 'critical' },
  ],
}

let graphResponder: () => Promise<Response> = () =>
  Promise.resolve(new Response(JSON.stringify(GRAPH), { status: 200 }))

const fetchMock = vi.fn<typeof fetch>((input) => {
  const url = String(input)
  if (url.includes('/api/system/graph')) return graphResponder()
  return Promise.resolve(new Response('{}', { status: 200 }))
})

beforeEach(() => {
  graphResponder = () =>
    Promise.resolve(new Response(JSON.stringify(GRAPH), { status: 200 }))
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

function stubXs() {
  vi.stubGlobal('matchMedia', (query: string): MediaQueryList =>
    ({
      matches: query === '(max-width: 767px)',
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }) as MediaQueryList,
  )
}

function renderView(path = '/map') {
  const memory = memoryLocation({ path, record: true })
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={qc}>
      <Router hook={memory.hook} searchHook={memory.searchHook}>
        <FlowMapView />
      </Router>
    </QueryClientProvider>,
  )
  return memory
}

async function findMap() {
  return await screen.findByRole('application', { name: /service flow map/i })
}

describe('FlowMapView — states', () => {
  it('shows a layout-mirroring skeleton while loading', () => {
    graphResponder = () => new Promise<Response>(() => {})
    renderView()
    expect(screen.getByTestId('map-skeleton')).toBeInTheDocument()
  })

  it('shows an error panel with retry on failure', async () => {
    graphResponder = () => Promise.resolve(new Response('x', { status: 500 }))
    renderView()
    expect(await screen.findByRole('alert')).toHaveTextContent(/couldn’t load/i)
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })

  it('shows a what-next empty state with zero services', async () => {
    graphResponder = () =>
      Promise.resolve(
        new Response(JSON.stringify({ ...GRAPH, nodes: [], edges: [] }), { status: 200 }),
      )
    renderView()
    expect(await screen.findByText(/no services discovered yet/i)).toBeInTheDocument()
    expect(screen.getByText(/otlp exporter/i)).toBeInTheDocument()
  })

  it('offers clear-filters when the filter matches nothing', async () => {
    const user = userEvent.setup()
    renderView()
    await findMap()
    await user.type(screen.getByLabelText(/filter services/i), 'zzz')
    expect(screen.getByText(/no services match/i)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /clear filters/i }))
    expect(await findMap()).toBeInTheDocument()
  })
})

describe('FlowMapView — map rendering', () => {
  it('renders a node per service and an edge per dependency', async () => {
    renderView()
    const map = await findMap()
    expect(within(map).getByText('checkout')).toBeInTheDocument()
    expect(within(map).getByText('payments')).toBeInTheDocument()
    expect(within(map).getByText('db')).toBeInTheDocument()
    const svg = screen.getByTestId('flow-map-svg')
    expect(svg.querySelectorAll('path')).toHaveLength(2)
  })

  it('marks failing edges and leaves healthy ones alone', async () => {
    renderView()
    await findMap()
    const svg = screen.getByTestId('flow-map-svg')
    const failing = [...svg.querySelectorAll('path')].filter((p) =>
      p.getAttribute('class')?.includes('edgeFailing'),
    )
    expect(failing).toHaveLength(1)
  })

  it('shows err%% labels at default zoom (semantic zoom ≥ 0.8)', async () => {
    renderView()
    await findMap()
    expect(screen.getByText('4.2%')).toBeInTheDocument()
  })

  it('filter input narrows the rendered node set', async () => {
    const user = userEvent.setup()
    renderView()
    const map = await findMap()
    await user.type(screen.getByLabelText(/filter services/i), 'pay')
    expect(within(map).queryByText('checkout')).not.toBeInTheDocument()
    expect(within(map).getByText('payments')).toBeInTheDocument()
  })

  it('status pills narrow by status', async () => {
    const user = userEvent.setup()
    renderView()
    const map = await findMap()
    await user.click(screen.getByRole('button', { name: 'Critical' }))
    expect(within(map).getByText('db')).toBeInTheDocument()
    expect(within(map).queryByText('checkout')).not.toBeInTheDocument()
  })

  it('node click opens the inspector and pushes the trail', async () => {
    const user = userEvent.setup()
    const memory = renderView()
    const map = await findMap()
    await user.click(within(map).getByText('payments'))
    expect(memory.history.at(-1)).toContain('service=payments')
    expect(memory.history.at(-1)).toContain('trail=svc%3Apayments')
  })

  it('dims nodes outside the selected 1-hop neighborhood', async () => {
    renderView('/map?service=checkout')
    await findMap()
    const svg = screen.getByTestId('flow-map-svg')
    const dbGroup = svg.querySelector('[data-node-id="db"]')
    const payGroup = svg.querySelector('[data-node-id="payments"]')
    expect(dbGroup?.getAttribute('class')).toContain('dimmed')
    expect(payGroup?.getAttribute('class')).not.toContain('dimmed')
  })

  it('shows the freshness label once data arrived', async () => {
    renderView()
    await findMap()
    // freshness paints on a macrotask (render purity) — await it
    expect(await screen.findByText(/updated/i)).toBeInTheDocument()
  })
})

describe('FlowMapView — keyboard', () => {
  it('arrow keys walk the graph via aria-activedescendant; Enter inspects', async () => {
    const user = userEvent.setup()
    const memory = renderView()
    const map = await findMap()
    map.focus()
    await user.keyboard('{ArrowRight}')
    expect(map).toHaveAttribute('aria-activedescendant', 'map-node-checkout')
    await user.keyboard('{ArrowRight}')
    expect(map).toHaveAttribute('aria-activedescendant', 'map-node-payments')
    await user.keyboard('{Enter}')
    expect(memory.history.at(-1)).toContain('service=payments')
  })

  it('Escape clears the selection', async () => {
    const user = userEvent.setup()
    const memory = renderView('/map?service=payments')
    const map = await findMap()
    map.focus()
    await user.keyboard('{Escape}')
    expect(memory.history.at(-1)).not.toContain('service=')
  })

  it('"/" focuses the filter input and "f" fits without crashing', async () => {
    const user = userEvent.setup()
    renderView()
    const map = await findMap()
    map.focus()
    await user.keyboard('f')
    await user.keyboard('/')
    expect(screen.getByLabelText(/filter services/i)).toHaveFocus()
  })
})

describe('FlowMapView — pan/zoom', () => {
  function innerG(): SVGGElement {
    const svg = screen.getByTestId('flow-map-svg')
    return svg.querySelector('g') as SVGGElement
  }

  it('wheel zooms the inner <g> transform (no re-layout)', async () => {
    renderView()
    await findMap()
    const svg = screen.getByTestId('flow-map-svg')
    const before = innerG().getAttribute('transform')
    fireEvent.wheel(svg, { deltaY: -100, clientX: 50, clientY: 50 })
    const after = innerG().getAttribute('transform')
    expect(after).not.toBe(before)
    expect(after).toMatch(/scale\(1\.1/)
  })

  it('zooming out below 0.8 hides the err%% labels (semantic zoom)', async () => {
    renderView()
    await findMap()
    expect(screen.getByText('4.2%')).toBeInTheDocument()
    const svg = screen.getByTestId('flow-map-svg')
    fireEvent.wheel(svg, { deltaY: 300, clientX: 0, clientY: 0 })
    expect(screen.queryByText('4.2%')).not.toBeInTheDocument()
  })

  it('single-pointer drag pans', async () => {
    renderView()
    await findMap()
    const svg = screen.getByTestId('flow-map-svg')
    fireEvent.pointerDown(svg, { pointerId: 1, clientX: 10, clientY: 10 })
    fireEvent.pointerMove(svg, { pointerId: 1, clientX: 30, clientY: 25 })
    fireEvent.pointerUp(svg, { pointerId: 1 })
    expect(innerG().getAttribute('transform')).toContain('translate(20 15)')
  })

  it('two-pointer pinch zooms', async () => {
    renderView()
    await findMap()
    const svg = screen.getByTestId('flow-map-svg')
    fireEvent.pointerDown(svg, { pointerId: 1, clientX: 0, clientY: 0 })
    fireEvent.pointerDown(svg, { pointerId: 2, clientX: 100, clientY: 0 })
    fireEvent.pointerMove(svg, { pointerId: 2, clientX: 200, clientY: 0 })
    fireEvent.pointerUp(svg, { pointerId: 1 })
    fireEvent.pointerUp(svg, { pointerId: 2 })
    expect(innerG().getAttribute('transform')).toMatch(/scale\(2\)/)
  })
})

describe('FlowMapView — xs', () => {
  it('defaults to the status-grouped card list with healthy collapsed', async () => {
    stubXs()
    const user = userEvent.setup()
    renderView()
    expect(await screen.findByRole('region', { name: 'Degraded' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Critical' })).toBeInTheDocument()
    // healthy collapsed behind a disclosure
    expect(screen.queryByText('checkout')).not.toBeInTheDocument()
    const disclosure = screen.getByRole('button', { name: /1 healthy service/i })
    await user.click(disclosure)
    expect(screen.getByText('checkout')).toBeInTheDocument()
  })

  it('the Flow toggle switches to the SVG map', async () => {
    stubXs()
    const user = userEvent.setup()
    renderView()
    await screen.findByRole('region', { name: 'Critical' })
    await user.click(screen.getByRole('button', { name: 'Flow' }))
    expect(await findMap()).toBeInTheDocument()
  })

  it('card tap opens the inspector', async () => {
    stubXs()
    const user = userEvent.setup()
    const memory = renderView()
    await screen.findByRole('region', { name: 'Critical' })
    await user.click(screen.getByRole('button', { name: /db/i }))
    expect(memory.history.at(-1)).toContain('service=db')
  })
})
