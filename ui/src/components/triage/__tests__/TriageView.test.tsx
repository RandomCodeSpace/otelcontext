import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import TriageView from '../TriageView'
import type { AnomalyNode, SystemGraphResponse, SystemNode, TrafficPoint } from '@/types/api'

function node(id: string, status: string, alerts: string[] = []): SystemNode {
  return {
    id,
    type: 'service',
    health_score: status === 'critical' ? 0.2 : status === 'degraded' ? 0.6 : 0.97,
    status,
    metrics: {
      request_rate_rps: 10,
      error_rate: 0.05,
      avg_latency_ms: 50,
      p99_latency_ms: 120,
      span_count_1h: 100,
    },
    alerts,
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
    node('payments', 'degraded'),
    node('db', 'critical'),
    node('cache', 'healthy', ['evictions high']),
  ],
  edges: [],
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
  return Promise.resolve(new Response('{}', { status: 200 }))
})

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

function renderView(opts: { traffic?: TrafficPoint[] } = {}) {
  const memory = memoryLocation({ path: '/', record: true })
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  if (opts.traffic) qc.setQueryData(['metrics-traffic'], opts.traffic)
  render(
    <QueryClientProvider client={qc}>
      <Router hook={memory.hook} searchHook={memory.searchHook}>
        <TriageView />
      </Router>
    </QueryClientProvider>,
  )
  return memory
}

describe('TriageView — anomaly strip', () => {
  it('renders a severity tick per anomaly cluster; tap opens the inspector', async () => {
    const user = userEvent.setup()
    const memory = renderView()
    const tick = await screen.findByRole('button', { name: /db: 1 critical anomaly/i })
    await user.click(tick)
    expect(memory.history.at(-1)).toContain('service=db')
    expect(memory.history.at(-1)).toContain('trail=svc%3Adb')
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
    renderView()
    expect(await screen.findByText(/no anomalies in the last 24h/i)).toBeInTheDocument()
  })

  it('shows an inline error with retry when the MCP tool fails', async () => {
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
    renderView()
    expect(await screen.findByRole('alert')).toHaveTextContent(/anomaly timeline unavailable/i)
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })
})

describe('TriageView — feed', () => {
  it('ranks services critical → degraded → alerted, healthy collapsed', async () => {
    const user = userEvent.setup()
    renderView()
    const critical = await screen.findByRole('region', { name: 'Critical' })
    expect(within(critical).getByText('db')).toBeInTheDocument()
    expect(within(screen.getByRole('region', { name: 'Degraded' })).getByText('payments')).toBeInTheDocument()
    expect(within(screen.getByRole('region', { name: 'Alerts' })).getByText('cache')).toBeInTheDocument()
    // healthy collapsed
    expect(screen.queryByText('checkout')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /1 healthy service/i }))
    expect(screen.getByText('checkout')).toBeInTheDocument()
  })

  it('row click opens the inspector', async () => {
    const user = userEvent.setup()
    const memory = renderView()
    await screen.findByRole('region', { name: 'Critical' })
    await user.click(screen.getByRole('button', { name: /payments/ }))
    expect(memory.history.at(-1)).toContain('service=payments')
  })

  it('shows a skeleton while loading', () => {
    graphResponder = () => new Promise<Response>(() => {})
    renderView()
    expect(screen.getByTestId('feed-skeleton')).toBeInTheDocument()
  })

  it('shows the OTLP connect content as the empty state', async () => {
    graphResponder = () =>
      Promise.resolve(
        new Response(JSON.stringify({ ...GRAPH, nodes: [], edges: [] }), { status: 200 }),
      )
    renderView()
    expect(await screen.findByText(/no telemetry yet/i)).toBeInTheDocument()
    expect(screen.getByText('OTLP gRPC')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /copy otlp grpc/i })).toBeInTheDocument()
  })

  it('shows an error panel with retry on graph failure', async () => {
    graphResponder = () => Promise.resolve(new Response('x', { status: 500 }))
    renderView()
    const alerts = await screen.findAllByRole('alert')
    expect(alerts.some((a) => /couldn’t load services/i.test(a.textContent ?? ''))).toBe(true)
  })
})

describe('TriageView — sparkline gating', () => {
  const TRAFFIC: TrafficPoint[] = Array.from({ length: 10 }, (_, i) => ({
    timestamp: new Date(Date.now() - (10 - i) * 60_000).toISOString(),
    count: 100,
    error_count: i,
  }))

  it('renders the error sparkline only when traffic is already cached', async () => {
    renderView({ traffic: TRAFFIC })
    await screen.findByRole('region', { name: 'Critical' })
    expect(screen.getByTestId('sparkline')).toBeInTheDocument()
    expect(
      fetchMock.mock.calls.filter(([u]) => String(u).includes('/api/metrics/traffic')),
    ).toHaveLength(0) // observe-only: it never fetches
  })

  it('omits the sparkline when no traffic data is cached', async () => {
    renderView()
    await screen.findByRole('region', { name: 'Critical' })
    expect(screen.queryByTestId('sparkline')).not.toBeInTheDocument()
    expect(
      fetchMock.mock.calls.filter(([u]) => String(u).includes('/api/metrics/traffic')),
    ).toHaveLength(0)
  })
})
