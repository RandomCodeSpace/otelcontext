import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import PulseBar from '../PulseBar'

const fetchMock = vi.fn<typeof fetch>()

function jsonResponse(body: unknown) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

function routeFetches(overrides?: {
  summary?: Partial<Record<string, number>>
  dashboard?: Partial<Record<string, unknown>>
  stats?: Record<string, unknown>
}) {
  const summary = {
    total_services: 12,
    healthy: 9,
    degraded: 2,
    critical: 1,
    overall_health_score: 0.94,
    total_error_rate: 0.042,
    avg_latency_ms: 18,
    uptime_seconds: 3600,
    ...overrides?.summary,
  }
  const dashboard = {
    total_traces: 100,
    total_logs: 500,
    total_errors: 4,
    avg_latency_ms: 18,
    error_rate: 4.2, // /api/metrics/dashboard sends percent, not ratio
    active_services: 12,
    p99_latency_ms: 230,
    top_failing_services: [],
    ...overrides?.dashboard,
  }
  const stats = overrides?.stats ?? { db_size_mb: 1228.8 }

  fetchMock.mockImplementation((input) => {
    const url = String(input)
    if (url.includes('/api/system/graph')) {
      return Promise.resolve(
        jsonResponse({
          timestamp: '2026-06-11T00:00:00Z',
          system: summary,
          nodes: [],
          edges: [],
        }),
      )
    }
    if (url.includes('/api/metrics/dashboard')) {
      return Promise.resolve(jsonResponse(dashboard))
    }
    if (url.includes('/api/stats')) {
      return Promise.resolve(jsonResponse(stats))
    }
    return Promise.resolve(new Response('nf', { status: 404 }))
  })
}

function renderPulseBar() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  )
  return render(<PulseBar liveSlot={null} />, { wrapper })
}

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
  fetchMock.mockReset()
})

describe('PulseBar', () => {
  it('renders health, error rate, p99 and DB size with correct formatting', async () => {
    routeFetches()
    renderPulseBar()

    await waitFor(() =>
      expect(screen.getByText(/94% healthy/)).toBeInTheDocument(),
    )
    // err 4.2% — already-percent field must NOT be re-scaled.
    expect(screen.getByText(/err 4\.2%/)).toBeInTheDocument()
    expect(screen.getByText(/p99 230ms/)).toBeInTheDocument()
    expect(screen.getByText(/DB 1\.2GB/)).toBeInTheDocument()
  })

  it('lists degraded and critical counts only when non-zero', async () => {
    routeFetches()
    renderPulseBar()
    await waitFor(() =>
      expect(screen.getByText(/2 degraded/)).toBeInTheDocument(),
    )
    expect(screen.getByText(/1 critical/)).toBeInTheDocument()
  })

  it('omits zero degraded/critical segments', async () => {
    routeFetches({ summary: { degraded: 0, critical: 0 } })
    renderPulseBar()
    await waitFor(() =>
      expect(screen.getByText(/94% healthy/)).toBeInTheDocument(),
    )
    expect(screen.queryByText(/degraded/)).not.toBeInTheDocument()
    expect(screen.queryByText(/critical/)).not.toBeInTheDocument()
  })

  it('shows the compact issue count for the xs layout', async () => {
    routeFetches()
    renderPulseBar()
    await waitFor(() => expect(screen.getByText(/3 issues/)).toBeInTheDocument())
  })

  it('shows "all clear" in the compact slot when nothing is degraded', async () => {
    routeFetches({ summary: { degraded: 0, critical: 0 } })
    renderPulseBar()
    await waitFor(() =>
      expect(screen.getByText(/all clear/)).toBeInTheDocument(),
    )
  })

  it('omits the DB segment when stats carry no db_size_mb', async () => {
    routeFetches({ stats: {} })
    renderPulseBar()
    await waitFor(() =>
      expect(screen.getByText(/94% healthy/)).toBeInTheDocument(),
    )
    expect(screen.queryByText(/DB /)).not.toBeInTheDocument()
  })

  it('tints the DB segment as a warning above the size threshold', async () => {
    routeFetches({ stats: { db_size_mb: 4096 } })
    renderPulseBar()
    const db = await screen.findByText(/DB 4GB/)
    expect(db.className).toContain('warn')
  })
})
