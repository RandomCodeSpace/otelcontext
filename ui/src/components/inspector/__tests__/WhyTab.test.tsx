import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { WhyTab } from '../WhyTab'
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
      <WhyTab ctx={ctx} />
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

const CAUSES = [
  {
    service: 'db',
    operation: 'SELECT spans',
    score: 3,
    evidence: ['error chain from trace t1', 'anomaly: error spike'],
    error_chain: [
      {
        id: 'span:s1',
        span_id: 's1',
        trace_id: 't1',
        service: 'db',
        operation: 'SELECT spans',
        timestamp: '2026-06-12T00:00:00Z',
      },
    ],
  },
  { service: 'cache', operation: '', score: 1, evidence: ['error chain from trace t2'] },
]

describe('WhyTab', () => {
  it('is idle until the operator runs the analysis (no RPC on mount)', () => {
    renderTab()
    expect(
      screen.getByRole('button', { name: /run root-cause analysis/i }),
    ).toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('shows skeleton + cancel during the RPC, then ranked causes', async () => {
    const user = userEvent.setup()
    let resolve!: (r: Response) => void
    fetchMock.mockReturnValueOnce(new Promise((res) => (resolve = res)))
    renderTab()

    await user.click(
      screen.getByRole('button', { name: /run root-cause analysis/i }),
    )
    expect(await screen.findByTestId('verb-skeleton')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /cancel/i })).toBeInTheDocument()

    resolve(rpc(JSON.stringify(CAUSES)))
    expect(
      await screen.findByRole('region', { name: /ranked probable causes/i }),
    ).toBeInTheDocument()
    // Best score first, with operation and evidence rendered.
    const items = screen.getAllByRole('listitem')
    expect(items[0]).toHaveTextContent('db')
    expect(screen.getByText(/anomaly: error spike/)).toBeInTheDocument()
  })

  it('error-chain spans deep-link via openTrace', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValueOnce(rpc(JSON.stringify(CAUSES)))
    const ctx = renderTab()
    await user.click(
      screen.getByRole('button', { name: /run root-cause analysis/i }),
    )
    await user.click(await screen.findByRole('button', { name: /open trace t1/i }))
    expect(ctx.openTrace).toHaveBeenCalledWith('t1')
  })

  it('designed empty state when no causes are found', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValueOnce(rpc('null'))
    renderTab()
    await user.click(
      screen.getByRole('button', { name: /run root-cause analysis/i }),
    )
    expect(
      await screen.findByText(/no probable causes in the window/i),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /re-run/i })).toBeInTheDocument()
  })

  it('surfaces tool errors with a retry affordance', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValueOnce(rpc('Error: GraphRAG not initialized', true))
    renderTab()
    await user.click(
      screen.getByRole('button', { name: /run root-cause analysis/i }),
    )
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/GraphRAG not initialized/)
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })

  it('cancel during the RPC returns to the idle run button', async () => {
    const user = userEvent.setup()
    fetchMock.mockImplementationOnce(
      (_input, init) =>
        new Promise((_res, rej) => {
          init?.signal?.addEventListener('abort', () =>
            rej(new DOMException('aborted', 'AbortError')),
          )
        }),
    )
    renderTab()
    await user.click(
      screen.getByRole('button', { name: /run root-cause analysis/i }),
    )
    await user.click(await screen.findByRole('button', { name: /cancel/i }))
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: /run root-cause analysis/i }),
      ).toBeInTheDocument(),
    )
  })
})
