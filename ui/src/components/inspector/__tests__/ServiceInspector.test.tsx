import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
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

function renderInspector(path = '/map?service=payments&trail=svc:payments') {
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
    renderInspector('/map')
    expect(screen.queryByRole('complementary')).not.toBeInTheDocument()
  })

  it('shows header with status, mono name, health % and a close button', async () => {
    renderInspector()
    expect(
      await screen.findByRole('heading', { name: 'payments' }),
    ).toBeInTheDocument()
    // header health % + overview health row both show 73%
    expect(await screen.findAllByText('73%')).not.toHaveLength(0)
    expect(screen.getByRole('button', { name: /close inspector/i })).toBeInTheDocument()
  })

  it('shows a skeleton while the graph is loading', () => {
    graphResponder = () => new Promise<Response>(() => {}) // never resolves
    renderInspector()
    expect(screen.getByTestId('inspector-skeleton')).toBeInTheDocument()
  })

  it('overview tab renders the stat grid, health meter and alerts', async () => {
    renderInspector()
    expect(await screen.findByText('42/s')).toBeInTheDocument()
    expect(screen.getByText('4.2%')).toBeInTheDocument()
    expect(screen.getByText('230ms')).toBeInTheDocument()
    expect(screen.getByRole('meter', { name: /health score/i })).toHaveAttribute(
      'aria-valuenow',
      '73',
    )
    expect(screen.getByText('p99 above 200ms')).toBeInTheDocument()
  })

  it('dependencies tab lists upstream/downstream; row click pushes the trail', async () => {
    const user = userEvent.setup()
    const memory = renderInspector()
    await screen.findByText('42/s') // graph loaded, tabs mounted

    await user.click(screen.getByRole('tab', { name: /dependencies/i }))
    const upstream = screen.getByRole('region', { name: /upstream callers/i })
    const downstream = screen.getByRole('region', { name: /downstream dependencies/i })
    expect(within(upstream).getByText('checkout')).toBeInTheDocument()
    expect(within(downstream).getByText('db')).toBeInTheDocument()

    await user.click(within(downstream).getByRole('button', { name: /db/ }))
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'db' })).toBeInTheDocument(),
    )
    // pushed, not replaced: both frames in the URL trail
    expect(memory.history.at(-1)).toContain('trail=svc%3Apayments%2Csvc%3Adb')
  })

  it('close button clears ?service and unmounts the panel', async () => {
    const user = userEvent.setup()
    renderInspector()
    await screen.findByRole('heading', { name: 'payments' })
    await user.click(screen.getByRole('button', { name: /close inspector/i }))
    expect(screen.queryByRole('complementary')).not.toBeInTheDocument()
  })

  it('Escape inside the panel closes it', async () => {
    const user = userEvent.setup()
    renderInspector()
    await screen.findByRole('heading', { name: 'payments' })
    screen.getByRole('button', { name: /close inspector/i }).focus()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('complementary')).not.toBeInTheDocument()
  })

  it('shows a not-found state for a service missing from the graph', async () => {
    renderInspector('/map?service=ghost')
    expect(
      await screen.findByText(/isn’t in the current service graph/i),
    ).toBeInTheDocument()
  })

  it('shows an error state with retry when the graph fails', async () => {
    graphResponder = () =>
      Promise.resolve(new Response('boom', { status: 500 }))
    renderInspector()
    expect(await screen.findByRole('alert')).toHaveTextContent(/couldn’t load/i)
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })

  describe('xs bottom sheet', () => {
    beforeEach(() => {
      const mql = (query: string): MediaQueryList =>
        ({
          matches: query === '(max-width: 767px)',
          media: query,
          onchange: null,
          addListener: () => {},
          removeListener: () => {},
          addEventListener: () => {},
          removeEventListener: () => {},
          dispatchEvent: () => false,
        }) as MediaQueryList
      vi.stubGlobal('matchMedia', mql)
    })

    it('renders as a bottom-sheet dialog with a drag handle at 50dvh', async () => {
      renderInspector()
      const dialog = await screen.findByRole('dialog')
      expect(dialog).toHaveStyle({ height: '50dvh' })
      expect(screen.getByTestId('sheet-handle')).toBeInTheDocument()
    })

    it('drag up snaps the sheet to 92dvh', async () => {
      renderInspector()
      const dialog = await screen.findByRole('dialog')
      const handle = screen.getByTestId('sheet-handle')
      // jsdom innerHeight = 768 → threshold 115px; -200px is a real drag up
      fireEvent.pointerDown(handle, { pointerId: 1, clientY: 600 })
      fireEvent.pointerMove(handle, { pointerId: 1, clientY: 500 })
      fireEvent.pointerUp(handle, { pointerId: 1, clientY: 400 })
      expect(dialog).toHaveStyle({ height: '92dvh' })
    })

    it('swipe down past the threshold dismisses the sheet', async () => {
      renderInspector()
      await screen.findByRole('dialog')
      const handle = screen.getByTestId('sheet-handle')
      fireEvent.pointerDown(handle, { pointerId: 1, clientY: 300 })
      fireEvent.pointerMove(handle, { pointerId: 1, clientY: 450 })
      fireEvent.pointerUp(handle, { pointerId: 1, clientY: 600 })
      await waitFor(() =>
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument(),
      )
    })

    it('a small drag stays at the current snap', async () => {
      renderInspector()
      const dialog = await screen.findByRole('dialog')
      const handle = screen.getByTestId('sheet-handle')
      fireEvent.pointerDown(handle, { pointerId: 1, clientY: 300 })
      fireEvent.pointerUp(handle, { pointerId: 1, clientY: 330 })
      expect(dialog).toHaveStyle({ height: '50dvh' })
    })
  })
})
