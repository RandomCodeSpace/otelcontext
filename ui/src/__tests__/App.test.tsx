import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Router } from 'wouter'
import { memoryLocation } from 'wouter/memory-location'
import App from '../App'

class StubWebSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3
  readyState = StubWebSocket.CONNECTING
  onopen: ((ev: Event) => void) | null = null
  onmessage: ((ev: MessageEvent<string>) => void) | null = null
  onerror: ((ev: Event) => void) | null = null
  onclose: ((ev: CloseEvent) => void) | null = null
  send = vi.fn()
  close = vi.fn()
  constructor(public url: string) {}
}

const fetchMock = vi.fn<typeof fetch>((input) => {
  const url = String(input)
  if (url.includes('/api/system/graph')) {
    return Promise.resolve(
      new Response(
        JSON.stringify({
          timestamp: '2026-06-11T00:00:00Z',
          system: {
            total_services: 0,
            healthy: 0,
            degraded: 0,
            critical: 0,
            overall_health_score: 1,
            total_error_rate: 0,
            avg_latency_ms: 0,
            uptime_seconds: 1,
          },
          nodes: [],
          edges: [],
        }),
        { status: 200 },
      ),
    )
  }
  return Promise.resolve(new Response('{}', { status: 200 }))
})

beforeEach(() => {
  vi.stubGlobal('WebSocket', StubWebSocket as unknown as typeof WebSocket)
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

function renderApp(path: string) {
  const memory = memoryLocation({ path, record: true })
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={qc}>
      <Router hook={memory.hook}>
        <App theme="dark" onToggleTheme={() => {}} />
      </Router>
    </QueryClientProvider>,
  )
  return memory
}

describe('App routing', () => {
  it('mounts the Constellation home at /', async () => {
    renderApp('/')
    // Empty graph → the home's OTLP connect empty state proves the
    // ConstellationHome lazy chunk mounted.
    expect(await screen.findByText(/no telemetry yet/i)).toBeInTheDocument()
  })

  it('redirects unknown paths to /', async () => {
    const memory = renderApp('/nonsense')
    await waitFor(() => expect(memory.history).toContain('/'))
  })

  it('redirects the retired /dashboard route to the home', async () => {
    const memory = renderApp('/dashboard')
    await waitFor(() => expect(memory.history).toContain('/'))
  })

  it('redirects the retired /mcp route to the home', async () => {
    const memory = renderApp('/mcp')
    await waitFor(() => expect(memory.history).toContain('/'))
  })

  it('redirects the removed /traces screen to the home', async () => {
    const memory = renderApp('/traces')
    await waitFor(() => expect(memory.history).toContain('/'))
  })

  it('redirects the removed /logs screen to the home', async () => {
    const memory = renderApp('/logs')
    await waitFor(() => expect(memory.history).toContain('/'))
  })

  it('folds /map into the home canvas', async () => {
    const memory = renderApp('/map')
    await waitFor(() => expect(memory.history.at(-1)).toBe('/'))
  })

  it('preserves ?service= when folding /map into the home', async () => {
    const memory = renderApp('/map?service=ghost')
    // The redirect rebuilds the query so the deep link survives the fold.
    await waitFor(() => expect(memory.history.at(-1)).toContain('service=ghost'))
    // …and the Inspector docks for that service.
    expect(
      await screen.findByRole('button', { name: /close inspector/i }),
    ).toBeInTheDocument()
  })

  it('preserves ?impact= when folding /map into the home', async () => {
    const memory = renderApp('/map?impact=ghost')
    await waitFor(() => expect(memory.history.at(-1)).toContain('impact=ghost'))
  })

  it('mounts the Service Inspector when ?service= is present', async () => {
    renderApp('/?service=ghost')
    expect(
      await screen.findByRole('button', { name: /close inspector/i }),
    ).toBeInTheDocument()
  })

  it('opens the command palette on the global ⌘K shortcut', async () => {
    const user = userEvent.setup()
    renderApp('/')
    await screen.findByText(/no telemetry yet/i)
    await user.keyboard('{Meta>}k{/Meta}')
    expect(
      await screen.findByRole('dialog', { name: /command palette/i }),
    ).toBeInTheDocument()
  })

  it('opens the shortcut sheet on "?"', async () => {
    const user = userEvent.setup()
    renderApp('/')
    await screen.findByText(/no telemetry yet/i)
    await user.keyboard('?')
    expect(
      await screen.findByRole('dialog', { name: /keyboard shortcuts/i }),
    ).toBeInTheDocument()
  })
})
