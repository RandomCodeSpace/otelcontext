import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
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
  it('mounts the Triage home at /', async () => {
    renderApp('/')
    // Empty graph → the feed's connect empty state proves the mount.
    expect(
      await screen.findByRole('region', { name: /service triage feed/i }),
    ).toBeInTheDocument()
  })

  it('redirects unknown paths to /', async () => {
    const memory = renderApp('/nonsense')
    await waitFor(() => expect(memory.history).toContain('/'))
  })

  it('mounts the FlowMapView at /map', async () => {
    renderApp('/map')
    // Lazy chunk + empty graph → the view's empty state proves the mount.
    expect(
      await screen.findByText(/no services discovered yet/i),
    ).toBeInTheDocument()
  })

  it('mounts the Service Inspector when ?service= is present', async () => {
    renderApp('/map?service=ghost')
    expect(
      await screen.findByRole('button', { name: /close inspector/i }),
    ).toBeInTheDocument()
  })

  it('renders the trail bar when ?trail= is present', async () => {
    renderApp('/map?trail=svc:checkout')
    expect(
      await screen.findByRole('navigation', { name: /investigation trail/i }),
    ).toBeInTheDocument()
  })
})
