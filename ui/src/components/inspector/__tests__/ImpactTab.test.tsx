import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ImpactTab } from '../ImpactTab'
import type { InspectorTabContext } from '../inspectorTabs'
import type { SystemNode } from '@/types/api'

const node: SystemNode = {
  id: 'checkout',
  type: 'service',
  health_score: 0.4,
  status: 'critical',
  metrics: {
    request_rate_rps: 1,
    error_rate: 0.2,
    avg_latency_ms: 10,
    p99_latency_ms: 50,
    span_count_1h: 100,
  },
  alerts: [],
}

const fetchMock = vi.fn<typeof fetch>()

function rpc(toolText: string, isError = false) {
  return new Response(
    JSON.stringify({
      jsonrpc: '2.0',
      id: 1,
      result: { isError, content: [{ type: 'text', text: toolText }] },
    }),
    { status: 200 },
  )
}

function renderTab(overrides: Partial<InspectorTabContext> = {}) {
  const ctx: InspectorTabContext = {
    node,
    edges: [],
    openService: vi.fn(),
    openTrace: vi.fn(),
    showImpactOnMap: vi.fn(),
    ...overrides,
  }
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={qc}>
      <ImpactTab ctx={ctx} />
    </QueryClientProvider>,
  )
  return ctx
}

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
  fetchMock.mockReset()
})

const IMPACT = {
  service: 'checkout',
  affected_services: [
    { service: 'payments', depth: 1, call_count: 12, impact_score: 0.6 },
    { service: 'ledger', depth: 2, call_count: 4, impact_score: 0.1 },
  ],
  total_downstream: 2,
}

describe('ImpactTab', () => {
  it('is idle until "Map blast radius" is pressed', () => {
    renderTab()
    expect(
      screen.getByRole('button', { name: /map blast radius/i }),
    ).toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('renders the affected services grouped by depth', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValueOnce(rpc(JSON.stringify(IMPACT)))
    renderTab()
    await user.click(screen.getByRole('button', { name: /map blast radius/i }))
    expect(
      await screen.findByText(/2 downstream services affected/i),
    ).toBeInTheDocument()
    expect(screen.getByText(/depth 1 — direct callees/i)).toBeInTheDocument()
    expect(screen.getByText(/depth 2 — 2 hops/i)).toBeInTheDocument()
    expect(screen.getByText('payments')).toBeInTheDocument()
  })

  it('affected rows drill into the service (trail push)', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValueOnce(rpc(JSON.stringify(IMPACT)))
    const ctx = renderTab()
    await user.click(screen.getByRole('button', { name: /map blast radius/i }))
    await user.click(await screen.findByRole('button', { name: /payments/i }))
    expect(ctx.openService).toHaveBeenCalledWith('payments')
  })

  it('"Show on map" hands the cone to the flow map', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValueOnce(rpc(JSON.stringify(IMPACT)))
    const ctx = renderTab()
    await user.click(screen.getByRole('button', { name: /map blast radius/i }))
    await user.click(await screen.findByRole('button', { name: /show on map/i }))
    expect(ctx.showImpactOnMap).toHaveBeenCalledWith('checkout')
  })

  it('designed empty state when the blast radius is contained', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValueOnce(
      rpc(
        JSON.stringify({
          service: 'checkout',
          affected_services: null,
          total_downstream: 0,
        }),
      ),
    )
    renderTab()
    await user.click(screen.getByRole('button', { name: /map blast radius/i }))
    expect(await screen.findByText(/stays contained/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /show on map/i })).toBeNull()
  })

  it('surfaces tool errors with retry', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValueOnce(rpc('Error: GraphRAG not initialized', true))
    renderTab()
    await user.click(screen.getByRole('button', { name: /map blast radius/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      /GraphRAG not initialized/,
    )
  })
})
