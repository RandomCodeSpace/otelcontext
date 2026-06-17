import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import ConstellationHome from '../ConstellationHome'
import type { AnomalyNode, SystemGraphResponse, SystemNode, TrafficPoint } from '@/types/api'

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
    alerts: status === 'healthy' && id === 'cache' ? ['evictions high'] : [],
  }
}

const GRAPH: SystemGraphResponse = {
  timestamp: '2026-06-11T00:00:00Z',
  system: {
    total_services: 4,
    healthy: 2,
    degraded: 1,
    critical: 1,
    overall_health_score: 0.7,
    total_error_rate: 0.05,
    avg_latency_ms: 100,
    uptime_seconds: 60,
  },
  nodes: [
    node('checkout', 'healthy'),
    node('payments', 'degraded', 0.042),
    node('db', 'critical', 0.3),
    node('cache', 'healthy'),
  ],
  edges: [
    { source: 'checkout', target: 'payments', call_count: 1200, avg_latency_ms: 90, error_rate: 0.01, status: 'healthy' },
    { source: 'payments', target: 'db', call_count: 800, avg_latency_ms: 250, error_rate: 0.3, status: 'critical' },
  ],
}

const ANOMALIES: AnomalyNode[] = [
  {
    id: 'a1',
    type: 'error_spike',
    severity: 'critical',
    service: 'db',
    evidence: 'error rate 30%',
    timestamp: new Date(Date.now() - 60_000).toISOString(),
  },
]

let graphResponder: () => Promise<Response> = () =>
  Promise.resolve(new Response(JSON.stringify(GRAPH), { status: 200 }))
let mcpResponder: () => Promise<Response> = () =>
  Promise.resolve(
    new Response(
      JSON.stringify({
        jsonrpc: '2.0',
        id: 1,
        result: { content: [{ type: 'text', text: JSON.stringify(ANOMALIES) }] },
      }),
      { status: 200 },
    ),
  )

const fetchMock = vi.fn<typeof fetch>((input) => {
  const url = String(input)
  if (url.includes('/api/system/graph')) return graphResponder()
  if (url.includes('/mcp')) return mcpResponder()
  if (url.includes('/api/metrics/dashboard'))
    return Promise.resolve(
      new Response(JSON.stringify({ p99_latency_ms: 230 }), { status: 200 }),
    )
  return Promise.resolve(new Response('{}', { status: 200 }))
})

function mqlStub(matchesFor: (query: string) => boolean) {
  return (query: string): MediaQueryList =>
    ({
      matches: matchesFor(query),
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }) as MediaQueryList
}

function stubXs() {
  vi.stubGlobal('matchMedia', mqlStub((q) => q === '(max-width: 767px)'))
}

beforeEach(() => {
  graphResponder = () =>
    Promise.resolve(new Response(JSON.stringify(GRAPH), { status: 200 }))
  mcpResponder = () =>
    Promise.resolve(
      new Response(
        JSON.stringify({
          jsonrpc: '2.0',
          id: 1,
          result: { content: [{ type: 'text', text: JSON.stringify(ANOMALIES) }] },
        }),
        { status: 200 },
      ),
    )
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

function renderHome(path = '/', seed?: (qc: QueryClient) => void) {
  const memory = memoryLocation({ path, record: true })
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  seed?.(qc)
  render(
    <QueryClientProvider client={qc}>
      <Router hook={memory.hook} searchHook={memory.searchHook}>
        <ConstellationHome />
      </Router>
    </QueryClientProvider>,
  )
  return memory
}

async function findMap() {
  return await screen.findByRole('application', { name: /service flow map/i })
}

describe('ConstellationHome — canvas + core', () => {
  it('renders the radial canvas as the hero with health demoted to a corner card', async () => {
    renderHome()
    const map = await findMap()
    // The map is the hero: nodes fill the field, the center is free.
    expect(within(map).getByText('db')).toBeInTheDocument()
    // No pinned center overlay and no on-page health card — vitals live in the
    // header now; the map owns the whole canvas.
    expect(screen.queryByTestId('flow-map-core')).toBeNull()
    expect(screen.queryByRole('meter', { name: /HEALTH/ })).toBeNull()
    // ringDepth is on → the cinematic dial backdrop renders.
    expect(screen.getByTestId('radial-dial')).toBeInTheDocument()
  })

  it('renders the worst-first rail alongside the canvas on md+', async () => {
    renderHome()
    await findMap()
    // The slim rail rides alongside the canvas (complementary landmark on md+).
    const rail = screen.getByRole('complementary', { name: /service triage feed/i })
    const critical = within(rail).getByRole('region', { name: 'Critical' })
    expect(within(critical).getByText('db')).toBeInTheDocument()
  })

  it('selecting a node from the canvas docks the inspector (?service=)', async () => {
    const user = userEvent.setup()
    const memory = renderHome()
    const map = await findMap()
    await user.click(within(map).getByText('payments'))
    expect(memory.history.at(-1)).toContain('service=payments')
  })

  it('selecting from the rail opens the same investigation', async () => {
    const user = userEvent.setup()
    const memory = renderHome()
    await findMap()
    const rail = screen.getByRole('complementary', { name: /service triage feed/i })
    await user.click(within(rail).getByRole('button', { name: /db/i }))
    expect(memory.history.at(-1)).toContain('service=db')
  })
})

describe('ConstellationHome — anomaly tape', () => {
  it('renders recent-anomaly service chips; tapping one opens the inspector', async () => {
    const user = userEvent.setup()
    const memory = renderHome()
    const strip = await screen.findByRole('region', { name: /recent anomalies/i })
    await user.click(await within(strip).findByRole('button', { name: /db/i }))
    expect(memory.history.at(-1)).toContain('service=db')
  })

  it('shows the quiet empty message when there are no anomalies', async () => {
    mcpResponder = () =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            jsonrpc: '2.0',
            id: 1,
            result: { content: [{ type: 'text', text: 'null' }] },
          }),
          { status: 200 },
        ),
      )
    renderHome()
    expect(await screen.findByText(/no anomalies in the last hour/i)).toBeInTheDocument()
  })

  it('shows an inline error with a working Retry when the MCP tool fails', async () => {
    const user = userEvent.setup()
    mcpResponder = () =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            jsonrpc: '2.0',
            id: 1,
            error: { code: -32000, message: 'graph not initialized' },
          }),
          { status: 200 },
        ),
      )
    renderHome()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/anomalies unavailable/i)
    // Retry refetches: once the responder recovers, the strip resolves to the
    // quiet empty message (proves the button re-runs the query, not just clears).
    mcpResponder = () =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            jsonrpc: '2.0',
            id: 1,
            result: { content: [{ type: 'text', text: 'null' }] },
          }),
          { status: 200 },
        ),
      )
    await user.click(within(alert).getByRole('button', { name: /retry/i }))
    expect(await screen.findByText(/no anomalies in the last hour/i)).toBeInTheDocument()
  })
})

describe('ConstellationHome — states', () => {
  it('shows a skeleton while loading', () => {
    graphResponder = () => new Promise<Response>(() => {})
    renderHome()
    expect(screen.getByTestId('home-skeleton')).toBeInTheDocument()
  })

  it('shows the OTLP connect empty state with zero services', async () => {
    graphResponder = () =>
      Promise.resolve(
        new Response(JSON.stringify({ ...GRAPH, nodes: [], edges: [] }), { status: 200 }),
      )
    renderHome()
    expect(await screen.findByText(/no telemetry yet/i)).toBeInTheDocument()
  })

  it('shows an error panel with retry on graph failure', async () => {
    graphResponder = () => Promise.resolve(new Response('x', { status: 500 }))
    renderHome()
    const alerts = await screen.findAllByRole('alert')
    expect(alerts.some((a) => /couldn’t load the service graph/i.test(a.textContent ?? ''))).toBe(
      true,
    )
  })
})

describe('ConstellationHome — ?impact= blast-radius overlay', () => {
  it('tints the downstream cone and announces the overlay', async () => {
    renderHome('/?impact=checkout')
    const svg = within(await findMap()).getByTestId('flow-map-svg')
    const payTint = screen.getByTestId('impact-tint-payments')
    const dbTint = screen.getByTestId('impact-tint-db')
    expect(Number(payTint.style.fillOpacity)).toBeGreaterThan(Number(dbTint.style.fillOpacity))
    const rootRect = svg.querySelector('[data-node-id="checkout"] rect')
    expect(rootRect?.getAttribute('class')).toContain('nodeImpactRoot')
    const banner = screen.getByRole('status')
    expect(banner).toHaveTextContent(/blast radius of/i)
    expect(banner).toHaveTextContent(/2 downstream/i)
  })

  it('clears the overlay via the banner button', async () => {
    const user = userEvent.setup()
    const memory = renderHome('/?impact=checkout')
    await findMap()
    await user.click(screen.getByRole('button', { name: /clear blast radius overlay/i }))
    expect(memory.history.at(-1)).not.toContain('impact=')
  })
})

describe('ConstellationHome — xs canvas default', () => {
  it('defaults to the constellation canvas on xs for a reasonable node count', async () => {
    stubXs()
    renderHome()
    // The cinematic field is the phone home too; health rides as a corner card.
    expect(await findMap()).toBeInTheDocument()
    expect(screen.queryByTestId('flow-map-core')).toBeNull()
    // A "List" toggle drops to the dense worst-first card list.
    expect(screen.getByRole('button', { name: 'List' })).toBeInTheDocument()
  })

  it('the List toggle drops to the card list; Flow restores the canvas', async () => {
    stubXs()
    const user = userEvent.setup()
    renderHome()
    await findMap()
    await user.click(screen.getByRole('button', { name: 'List' }))
    // The worst-first card list (vitals live in the header, not here).
    expect(await screen.findByRole('region', { name: 'Critical' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Degraded' })).toBeInTheDocument()
    expect(screen.queryByRole('application', { name: /service flow map/i })).toBeNull()
    // healthy collapsed behind a disclosure
    expect(screen.queryByText('checkout')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /healthy service/i }))
    expect(screen.getByText('checkout')).toBeInTheDocument()
    // Flow toggle returns to the canvas.
    await user.click(screen.getByRole('button', { name: 'Flow' }))
    expect(await findMap()).toBeInTheDocument()
  })

  it('forces the canvas on xs when an impact cone is active', async () => {
    stubXs()
    renderHome('/?impact=checkout')
    expect(await findMap()).toBeInTheDocument()
  })
})

describe('ConstellationHome — error-trend sparkline gating', () => {
  const TRAFFIC: TrafficPoint[] = Array.from({ length: 10 }, (_, i) => ({
    timestamp: new Date(Date.now() - (10 - i) * 60_000).toISOString(),
    count: 100,
    error_count: i,
  }))

  it('renders the sparkline only when traffic is already cached (observe-only)', async () => {
    renderHome('/', (qc) => qc.setQueryData(['metrics-traffic'], TRAFFIC))
    await findMap()
    expect(screen.getByTestId('sparkline')).toBeInTheDocument()
    // It never fetches traffic itself — the cache is observed, not populated.
    expect(
      fetchMock.mock.calls.filter(([u]) => String(u).includes('/api/metrics/traffic')),
    ).toHaveLength(0)
  })

  it('omits the sparkline when no traffic data is cached', async () => {
    renderHome()
    await findMap()
    expect(screen.queryByTestId('sparkline')).not.toBeInTheDocument()
    expect(
      fetchMock.mock.calls.filter(([u]) => String(u).includes('/api/metrics/traffic')),
    ).toHaveLength(0)
  })
})
