import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import type { ReactNode } from 'react'
import type { Span, Trace } from '@/types/api'
import TracesView from '../TracesView'

let autoId = 0
function trace(over: Partial<Trace> = {}): Trace {
  autoId += 1
  return {
    id: autoId,
    trace_id: `trace-${autoId}`,
    service_name: 'svc-a',
    duration: 1_000_000,
    duration_ms: 1000,
    span_count: 2,
    operation: `GET /op-${autoId}`,
    status: 'STATUS_CODE_UNSET',
    timestamp: '2026-06-11T10:00:00Z',
    ...over,
  }
}

function span(over: Partial<Span> = {}): Span {
  autoId += 1
  return {
    id: autoId,
    trace_id: 'trace-detail',
    span_id: `span-${autoId}`,
    parent_span_id: '',
    operation_name: `op-${autoId}`,
    start_time: '2026-06-11T10:00:00.000Z',
    end_time: '2026-06-11T10:00:01.000Z',
    duration: 1_000_000,
    service_name: 'svc-a',
    status: 'STATUS_CODE_UNSET',
    attributes_json: '',
    ...over,
  }
}

const fetchMock = vi.fn<typeof fetch>()

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** Route fetches by path prefix; everything is overridable per test. */
function mockRoutes(routes: Record<string, unknown>) {
  fetchMock.mockImplementation((input) => {
    const url = String(input)
    for (const [prefix, body] of Object.entries(routes)) {
      if (url.startsWith(prefix)) return Promise.resolve(jsonResponse(body))
    }
    return Promise.resolve(new Response('not found', { status: 404 }))
  })
}

function renderTraces(path = '/traces') {
  const loc = memoryLocation({ path, record: true })
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook} searchHook={loc.searchHook}>
        {children}
      </Router>
    </QueryClientProvider>
  )
  return { ...render(<TracesView />, { wrapper }), loc }
}

beforeEach(() => {
  fetchMock.mockReset()
  mockRoutes({
    '/api/metadata/services': ['svc-a', 'svc-b'],
    '/api/traces': { traces: [], total: 0, limit: 50, offset: 0 },
  })
  vi.stubGlobal('fetch', fetchMock)
  Object.defineProperty(HTMLElement.prototype, 'offsetHeight', {
    configurable: true,
    get: () => 600,
  })
  Object.defineProperty(HTMLElement.prototype, 'offsetWidth', {
    configurable: true,
    get: () => 800,
  })
})

afterEach(() => {
  vi.unstubAllGlobals()
  Reflect.deleteProperty(HTMLElement.prototype, 'offsetHeight')
  Reflect.deleteProperty(HTMLElement.prototype, 'offsetWidth')
})

describe('TracesView — list', () => {
  it('renders rows with status badge, service, operation, spans and duration', async () => {
    mockRoutes({
      '/api/metadata/services': ['svc-a'],
      '/api/traces': {
        traces: [
          trace({
            status: 'STATUS_CODE_ERROR',
            service_name: 'payments',
            operation: 'POST /charge',
            span_count: 7,
            duration_ms: 230,
          }),
        ],
        total: 1,
        limit: 50,
        offset: 0,
      },
    })
    renderTraces()

    const list = await screen.findByRole('list', { name: /traces/i })
    expect(within(list).getByText('ERROR')).toBeInTheDocument()
    expect(within(list).getByText('payments')).toBeInTheDocument()
    expect(within(list).getByText('POST /charge')).toBeInTheDocument()
    expect(within(list).getByText('7')).toBeInTheDocument()
    expect(within(list).getByText('230ms')).toBeInTheDocument()
  })

  it('shows the empty state with an OTLP hint when there are no filters', async () => {
    renderTraces()
    expect(await screen.findByText(/no traces found/i)).toBeInTheDocument()
    expect(screen.getByText(/4317/)).toBeInTheDocument()
  })

  it('offers Clear filters when filters are active and clears them', async () => {
    const user = userEvent.setup()
    const { loc } = renderTraces('/traces?status=ERROR')
    const clear = await screen.findByRole('button', { name: /clear filters/i })
    await user.click(clear)
    await waitFor(() => expect(loc.history.at(-1)).toBe('/traces'))
  })

  it('writes the service filter into the URL and the API query', async () => {
    const user = userEvent.setup()
    const { loc } = renderTraces()
    const select = await screen.findByRole('combobox', {
      name: /filter by service/i,
    })
    await waitFor(() =>
      expect(within(select).getByText('svc-b')).toBeInTheDocument(),
    )
    await user.selectOptions(select, 'svc-b')

    await waitFor(() => expect(loc.history.at(-1)).toContain('service=svc-b'))
    await waitFor(() => {
      const tracesCalls = fetchMock.mock.calls
        .map((c) => String(c[0]))
        .filter((u) => u.startsWith('/api/traces'))
      expect(tracesCalls.at(-1)).toContain('service_name=svc-b')
    })
  })

  it('keeps a deep-linked ?trace= and shows the detail pane', async () => {
    mockRoutes({
      '/api/metadata/services': [],
      '/api/traces/trace-detail': {
        ...trace({ trace_id: 'trace-detail', operation: 'GET /detail' }),
        spans: [span({ span_id: 'root' })],
        logs: [],
      },
      '/api/traces': { traces: [], total: 0, limit: 50, offset: 0 },
    })
    renderTraces('/traces?trace=trace-detail')
    expect(await screen.findByText('GET /detail')).toBeInTheDocument()
    expect(
      screen.getByRole('group', { name: /trace waterfall/i }),
    ).toBeInTheDocument()
  })

  it('pages with Load more', async () => {
    const user = userEvent.setup()
    const firstPage = Array.from({ length: 50 }, () => trace())
    mockRoutes({
      '/api/metadata/services': [],
      '/api/traces': {
        traces: firstPage,
        total: 80,
        limit: 50,
        offset: 0,
      },
    })
    renderTraces()

    const loadMore = await screen.findByRole('button', { name: /load more/i })
    await user.click(loadMore)
    await waitFor(() => {
      const calls = fetchMock.mock.calls
        .map((c) => String(c[0]))
        .filter((u) => u.startsWith('/api/traces?'))
      expect(calls.at(-1)).toContain('offset=50')
    })
  })

  it('shows an inline error panel with retry on list failure', async () => {
    fetchMock.mockImplementation((input) => {
      const url = String(input)
      if (url.startsWith('/api/traces')) {
        return Promise.resolve(new Response('boom', { status: 500 }))
      }
      return Promise.resolve(jsonResponse([]))
    })
    renderTraces()
    expect(await screen.findByRole('alert')).toHaveTextContent(
      /GET \/api\/traces failed/i,
    )
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })
})

describe('TracesView — waterfall detail', () => {
  function mockDetail(spans: Span[], logs: unknown[] = []) {
    mockRoutes({
      '/api/metadata/services': [],
      '/api/traces/trace-detail': {
        ...trace({
          trace_id: 'trace-detail',
          operation: 'GET /detail',
          status: 'STATUS_CODE_ERROR',
        }),
        spans,
        logs,
      },
      '/api/traces': { traces: [], total: 0, limit: 50, offset: 0 },
    })
  }

  it('renders one keyboard-walkable bar per span, depth-ordered', async () => {
    mockDetail([
      span({ span_id: 'root', operation_name: 'root-op' }),
      span({
        span_id: 'child',
        parent_span_id: 'root',
        operation_name: 'child-op',
        start_time: '2026-06-11T10:00:00.250Z',
        duration: 500_000,
      }),
    ])
    renderTraces('/traces?trace=trace-detail')

    const bars = await screen.findAllByRole('button', { name: /·/ })
    const names = bars.map((b) => b.getAttribute('aria-label'))
    expect(names[0]).toContain('root-op')
    expect(names[1]).toContain('child-op')
  })

  it('marks error spans in the aria-label', async () => {
    mockDetail([
      span({
        span_id: 'bad',
        operation_name: 'explode',
        status: 'STATUS_CODE_ERROR',
      }),
    ])
    renderTraces('/traces?trace=trace-detail')
    const bar = await screen.findByRole('button', { name: /explode/ })
    expect(bar.getAttribute('aria-label')).toContain('error')
  })

  it('span click reveals lazily-parsed attributes and correlated logs', async () => {
    const user = userEvent.setup()
    mockDetail(
      [
        span({
          span_id: 'sel',
          operation_name: 'select-me',
          attributes_json: '{"db.system":"sqlite"}',
        }),
      ],
      [
        {
          id: 1,
          trace_id: 'trace-detail',
          span_id: 'sel',
          severity: 'ERROR',
          body: 'span log line',
          service_name: 'svc-a',
          attributes_json: '',
          timestamp: '2026-06-11T10:00:00.500Z',
        },
      ],
    )
    renderTraces('/traces?trace=trace-detail')

    await user.click(await screen.findByRole('button', { name: /select-me/ }))
    expect(screen.getByText('db.system')).toBeInTheDocument()
    expect(screen.getByText('sqlite')).toBeInTheDocument()
    expect(screen.getByText('span log line')).toBeInTheDocument()
  })

  it('links back into the logs view scoped to the trace', async () => {
    mockDetail([span({ span_id: 'root' })])
    renderTraces('/traces?trace=trace-detail')
    const link = await screen.findByRole('link', { name: /open in logs/i })
    expect(link).toHaveAttribute('href', '/logs?trace=trace-detail')
  })

  it('explains an empty trace (sampling/retention) instead of a blank pane', async () => {
    mockDetail([])
    renderTraces('/traces?trace=trace-detail')
    expect(
      await screen.findByText(/no spans stored for this trace/i),
    ).toBeInTheDocument()
    expect(screen.getByText(/SAMPLING_RATE/)).toBeInTheDocument()
  })
})
