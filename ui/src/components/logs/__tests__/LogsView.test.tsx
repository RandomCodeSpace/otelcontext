import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import type { ReactNode } from 'react'
import type { LogEntry } from '@/types/api'
import LogsView from '../LogsView'

// ---- controllable WsManager fake (module-mocked singleton) ----------------
const wsFake = {
  subs: new Set<() => void>(),
  version: 0,
  total: 0,
  buffer: [] as LogEntry[],
  subscribeLogs(cb: () => void) {
    wsFake.subs.add(cb)
    return () => wsFake.subs.delete(cb)
  },
  getLogsVersion: () => wsFake.version,
  getLogs: () => wsFake.buffer as readonly LogEntry[],
  getLogsTotal: () => wsFake.total,
}

vi.mock('@/lib/wsManager', () => ({
  getWsManager: () => wsFake,
}))

function pushLogs(entries: LogEntry[]) {
  wsFake.buffer = [...wsFake.buffer, ...entries]
  wsFake.total += entries.length
  wsFake.version += 1
  for (const cb of wsFake.subs) cb()
}

let autoId = 0
function log(over: Partial<LogEntry> = {}): LogEntry {
  autoId += 1
  return {
    id: autoId,
    trace_id: '',
    span_id: '',
    severity: 'INFO',
    body: `line ${autoId}`,
    service_name: 'svc-a',
    attributes_json: '',
    timestamp: '2026-06-11T10:00:00.123Z',
    ...over,
  }
}

// ---- fetch mock ------------------------------------------------------------
const fetchMock = vi.fn<typeof fetch>()

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

function renderLogs(path = '/logs') {
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
  return { ...render(<LogsView />, { wrapper }), loc }
}

beforeEach(() => {
  wsFake.subs.clear()
  wsFake.version = 0
  wsFake.total = 0
  wsFake.buffer = []
  fetchMock.mockReset()
  fetchMock.mockResolvedValue(jsonResponse({ data: [], total: 0 }))
  vi.stubGlobal('fetch', fetchMock)
  // jsdom has no layout: the virtualizer reads offsetWidth/offsetHeight
  // from the scroll element — give every element a viewport.
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
  // Remove the offset overrides (they're configurable own props).
  Reflect.deleteProperty(HTMLElement.prototype, 'offsetHeight')
  Reflect.deleteProperty(HTMLElement.prototype, 'offsetWidth')
})

describe('LogsView — live tail', () => {
  it('shows the waiting empty state with an OTLP hint', () => {
    renderLogs()
    expect(screen.getByText(/waiting for logs/i)).toBeInTheDocument()
    expect(screen.getByText(/4317/)).toBeInTheDocument()
  })

  it('renders buffered rows with time, severity, service and body', () => {
    pushLogs([
      log({ severity: 'ERROR', body: 'kaboom', service_name: 'payments' }),
    ])
    renderLogs()
    const list = screen.getByRole('list', { name: /log entries/i })
    expect(within(list).getByText('kaboom')).toBeInTheDocument()
    expect(within(list).getByText('payments')).toBeInTheDocument()
    expect(within(list).getByText('ERROR')).toBeInTheDocument()
  })

  it('streams new rows on flush ticks', async () => {
    renderLogs()
    pushLogs([log({ body: 'first tick' })])
    await waitFor(() =>
      expect(screen.getByText('first tick')).toBeInTheDocument(),
    )
  })

  it('shows live counts on severity pills and filters on click', async () => {
    const user = userEvent.setup()
    pushLogs([
      log({ severity: 'ERROR', body: 'bad thing' }),
      log({ severity: 'INFO', body: 'fine thing' }),
      log({ severity: 'INFO', body: 'another fine thing' }),
    ])
    renderLogs()

    const pills = screen.getByRole('group', { name: /filter by severity/i })
    const errorPill = within(pills).getByRole('button', { name: /error\s*1/i })
    expect(
      within(pills).getByRole('button', { name: /info\s*2/i }),
    ).toBeInTheDocument()

    await user.click(errorPill)
    expect(screen.getByText('bad thing')).toBeInTheDocument()
    expect(screen.queryByText('fine thing')).not.toBeInTheDocument()

    // Toggle off again → everything is back.
    await user.click(errorPill)
    expect(screen.getByText('fine thing')).toBeInTheDocument()
  })

  it('pause freezes the visible buffer and counts missed entries', async () => {
    const user = userEvent.setup()
    pushLogs([log({ body: 'early entry' })])
    renderLogs()

    await user.click(screen.getByRole('button', { name: /^pause$/i }))
    pushLogs([log({ body: 'frozen-out entry' })])

    expect(screen.queryByText('frozen-out entry')).not.toBeInTheDocument()
    await waitFor(() =>
      expect(screen.getByText(/1 new entries since/i)).toBeInTheDocument(),
    )

    await user.click(screen.getByRole('button', { name: /^resume$/i }))
    expect(screen.getByText('frozen-out entry')).toBeInTheDocument()
  })

  it('scroll-up pauses follow and shows a "N new" pill that jumps back down', async () => {
    const user = userEvent.setup()
    pushLogs(Array.from({ length: 50 }, (_, i) => log({ body: `row ${i}` })))
    renderLogs()

    const list = screen.getByRole('list', { name: /log entries/i })
    // jsdom computes no scroll geometry — fake a long list scrolled up.
    Object.defineProperty(list, 'scrollHeight', {
      configurable: true,
      value: 2000,
    })
    Object.defineProperty(list, 'clientHeight', {
      configurable: true,
      value: 600,
    })
    list.scrollTop = 0
    fireEvent.scroll(list)

    // New arrivals while scrolled up surface as the pill, not a yank.
    pushLogs([log({ body: 'late arrival' })])
    const pill = await screen.findByRole('button', { name: /1 new/i })

    await user.click(pill)
    await waitFor(() =>
      expect(
        screen.queryByRole('button', { name: /new$/i }),
      ).not.toBeInTheDocument(),
    )
  })

  it('expands a row in place with attributes, insight and trace link', async () => {
    const user = userEvent.setup()
    pushLogs([
      log({
        body: 'expandable',
        trace_id: 'tr123',
        ai_insight: 'probably DNS',
        attributes_json: '{"http.method":"GET"}',
      }),
    ])
    renderLogs()

    await user.click(screen.getByRole('button', { name: /expandable/i }))
    expect(screen.getByText('http.method')).toBeInTheDocument()
    expect(screen.getByText('GET')).toBeInTheDocument()
    expect(screen.getByText('probably DNS')).toBeInTheDocument()
    const traceLink = screen.getByRole('link', { name: /open trace/i })
    expect(traceLink).toHaveAttribute('href', '/traces?trace=tr123')
  })

  it('fetches surrounding context when "Show context" is clicked', async () => {
    const user = userEvent.setup()
    const anchor = log({ body: 'anchor row' })
    fetchMock.mockResolvedValue(
      jsonResponse([
        log({ body: 'just before' }),
        anchor,
        log({ body: 'just after' }),
      ]),
    )
    pushLogs([anchor])
    renderLogs()

    await user.click(screen.getByRole('button', { name: /anchor row/i }))
    await user.click(screen.getByRole('button', { name: /show context/i }))

    await waitFor(() =>
      expect(screen.getByText('just before')).toBeInTheDocument(),
    )
    expect(screen.getByText('just after')).toBeInTheDocument()
    const calledPath = String(fetchMock.mock.calls[0]?.[0])
    expect(calledPath).toContain('/api/logs/context?timestamp=')
    expect(screen.getByText(/surrounding ±1min/i)).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /back to live/i }))
    expect(screen.getByText(/live ·/i)).toBeInTheDocument()
  })
})

describe('LogsView — server search mode', () => {
  it('debounces the query, hits /api/logs and replaces the tail', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValue(
      jsonResponse({ data: [log({ body: 'server hit' })], total: 1 }),
    )
    pushLogs([log({ body: 'live row' })])
    renderLogs()

    await user.type(screen.getByRole('searchbox', { name: /search/i }), 'panic')
    await waitFor(() =>
      expect(screen.getByText('server hit')).toBeInTheDocument(),
    )
    const calledPath = String(fetchMock.mock.calls[0]?.[0])
    expect(calledPath).toContain('/api/logs?')
    expect(calledPath).toContain('search=panic')
    expect(screen.queryByText('live row')).not.toBeInTheDocument()
  })

  it('returns to the live tail via "Back to live"', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValue(jsonResponse({ data: [], total: 0 }))
    pushLogs([log({ body: 'live row' })])
    renderLogs()

    await user.type(screen.getByRole('searchbox', { name: /search/i }), 'x')
    await waitFor(() =>
      expect(screen.getByText(/no matching logs/i)).toBeInTheDocument(),
    )
    await user.click(
      screen.getAllByRole('button', { name: /back to live/i })[0],
    )
    // The cleared search text still has to debounce out (300ms).
    await waitFor(() =>
      expect(screen.getByText('live row')).toBeInTheDocument(),
    )
  })

  it('shows an inline error panel with the failing endpoint and a retry', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValue(new Response('boom', { status: 500 }))
    renderLogs()

    await user.type(screen.getByRole('searchbox', { name: /search/i }), 'x')
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        /GET \/api\/logs failed/i,
      ),
    )
    expect(screen.getByRole('button', { name: /retry/i })).toBeInTheDocument()
  })

  it('enters trace mode from the ?trace= deep link and can leave it', async () => {
    const user = userEvent.setup()
    fetchMock.mockResolvedValue(
      jsonResponse({ data: [log({ body: 'trace-scoped' })], total: 1 }),
    )
    const { loc } = renderLogs('/logs?trace=abcd1234')
    await waitFor(() =>
      expect(screen.getByText('trace-scoped')).toBeInTheDocument(),
    )
    const calledPath = String(fetchMock.mock.calls[0]?.[0])
    expect(calledPath).toContain('trace_id=abcd1234')
    expect(screen.getByText(/trace abcd1234/i)).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /back to live/i }))
    await waitFor(() => expect(loc.history.at(-1)).toBe('/logs'))
  })

  it('pages older results with Load older', async () => {
    const user = userEvent.setup()
    const first = Array.from({ length: 100 }, (_, i) =>
      log({ body: `hit ${i}` }),
    )
    fetchMock
      .mockResolvedValueOnce(jsonResponse({ data: first, total: 101 }))
      .mockResolvedValueOnce(
        jsonResponse({ data: [log({ body: 'older hit' })], total: 101 }),
      )
    renderLogs()

    await user.type(screen.getByRole('searchbox', { name: /search/i }), 'hit')
    await waitFor(() => expect(screen.getByText('hit 0')).toBeInTheDocument())

    await user.click(screen.getByRole('button', { name: /load older/i }))
    await waitFor(() => {
      const second = String(fetchMock.mock.calls[1]?.[0])
      expect(second).toContain('offset=100')
    })
  })
})
