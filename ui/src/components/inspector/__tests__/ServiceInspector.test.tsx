import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import ServiceInspector from '../ServiceInspector'
import type { SystemGraphResponse } from '@/types/api'

const GRAPH: SystemGraphResponse = {
  timestamp: '2026-06-11T00:00:00Z',
  system: {
    total_services: 3,
    healthy: 1,
    degraded: 1,
    critical: 1,
    overall_health_score: 0.7,
    total_error_rate: 0.05,
    avg_latency_ms: 120,
    uptime_seconds: 100,
  },
  nodes: [
    {
      id: 'payments',
      type: 'service',
      health_score: 0.73,
      status: 'degraded',
      metrics: {
        request_rate_rps: 42,
        error_rate: 0.042,
        avg_latency_ms: 120,
        p99_latency_ms: 230,
        span_count_1h: 1000,
      },
      alerts: ['p99 above 200ms'],
    },
    {
      id: 'checkout',
      type: 'service',
      health_score: 0.95,
      status: 'healthy',
      metrics: {
        request_rate_rps: 10,
        error_rate: 0,
        avg_latency_ms: 50,
        p99_latency_ms: 90,
        span_count_1h: 400,
      },
      alerts: [],
    },
    {
      id: 'db',
      type: 'service',
      health_score: 0.4,
      status: 'critical',
      metrics: {
        request_rate_rps: 5,
        error_rate: 0.2,
        avg_latency_ms: 300,
        p99_latency_ms: 900,
        span_count_1h: 200,
      },
      alerts: [],
    },
  ],
  edges: [
    {
      source: 'checkout',
      target: 'payments',
      call_count: 1200,
      avg_latency_ms: 100,
      error_rate: 0.01,
      status: 'healthy',
    },
    {
      source: 'payments',
      target: 'db',
      call_count: 900,
      avg_latency_ms: 280,
      error_rate: 0.2,
      status: 'critical',
    },
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

function renderInspector(path = '/?service=payments') {
  const memory = memoryLocation({ path, record: true })
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={qc}>
      <Router hook={memory.hook} searchHook={memory.searchHook}>
        <ServiceInspector />
      </Router>
    </QueryClientProvider>,
  )
  return memory
}

describe('ServiceInspector', () => {
  it('renders nothing without ?service', () => {
    renderInspector('/')
    expect(screen.queryByRole('complementary')).not.toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('the popup carries the name + category tabs, with no docked side rail', async () => {
    renderInspector()
    const popup = await screen.findByRole('dialog')
    expect(within(popup).getByRole('heading', { name: 'payments' })).toBeInTheDocument()
    const tablist = within(popup).getByRole('tablist', { name: /inspector sections/i })
    for (const name of ['Overview', 'Why', 'Impact', 'Dependencies']) {
      expect(within(tablist).getByRole('tab', { name })).toBeInTheDocument()
    }
    // The popup stands alone — there is no docked rail.
    expect(screen.queryByRole('complementary')).not.toBeInTheDocument()
  })

  it('opens a details popup with the active category content + a close button', async () => {
    renderInspector()
    const popup = await screen.findByRole('dialog')
    expect(await within(popup).findByText('42/s')).toBeInTheDocument()
    expect(within(popup).getByText('4.2%')).toBeInTheDocument()
    expect(within(popup).getByText('230ms')).toBeInTheDocument()
    expect(within(popup).getByRole('meter', { name: /health score/i })).toHaveAttribute(
      'aria-valuenow',
      '73',
    )
    expect(within(popup).getByText('p99 above 200ms')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /close inspector/i })).toBeInTheDocument()
  })

  it('shows a skeleton while the graph is loading', () => {
    graphResponder = () => new Promise<Response>(() => {}) // never resolves
    renderInspector()
    expect(screen.getByTestId('inspector-skeleton')).toBeInTheDocument()
  })

  it('picking a tab switches the popup content', async () => {
    const user = userEvent.setup()
    renderInspector()
    const popup = await screen.findByRole('dialog')
    await user.click(within(popup).getByRole('tab', { name: 'Dependencies' }))
    expect(within(popup).getByRole('region', { name: /upstream callers/i })).toBeInTheDocument()
    const downstream = within(popup).getByRole('region', { name: /downstream dependencies/i })
    expect(within(downstream).getByText('db')).toBeInTheDocument()
  })

  it('?tab= deep-links a category (palette verbs)', async () => {
    renderInspector('/?service=payments&tab=why')
    const popup = await screen.findByRole('dialog')
    expect(within(popup).getByRole('tab', { name: 'Why' })).toHaveAttribute('aria-selected', 'true')
    expect(
      await within(popup).findByRole('button', { name: /run root-cause analysis/i }),
    ).toBeInTheDocument()
  })

  it('an invalid ?tab= falls back to Overview', async () => {
    renderInspector('/?service=payments&tab=nope')
    const popup = await screen.findByRole('dialog')
    expect(within(popup).getByRole('tab', { name: 'Overview' })).toHaveAttribute(
      'aria-selected',
      'true',
    )
  })

  it('a downstream dependency click switches the inspected service', async () => {
    const user = userEvent.setup()
    const memory = renderInspector()
    const popup = await screen.findByRole('dialog')
    await user.click(within(popup).getByRole('tab', { name: 'Dependencies' }))
    const downstream = within(popup).getByRole('region', { name: /downstream dependencies/i })
    await user.click(within(downstream).getByRole('button', { name: /db/ }))
    await waitFor(() => expect(memory.history.at(-1)).toContain('service=db'))
  })

  it('close button clears ?service and unmounts the popup', async () => {
    const user = userEvent.setup()
    renderInspector()
    await screen.findByRole('dialog')
    await user.click(screen.getByRole('button', { name: /close inspector/i }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('shows a not-found state for a service missing from the graph', async () => {
    renderInspector('/?service=ghost')
    expect(
      await screen.findByText(/isn’t in the current service graph/i),
    ).toBeInTheDocument()
  })

  it('shows an error state with retry when the graph fails', async () => {
    graphResponder = () => Promise.resolve(new Response('boom', { status: 500 }))
    renderInspector()
    expect(await screen.findByRole('alert')).toHaveTextContent(/couldn’t load/i)
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })

})
