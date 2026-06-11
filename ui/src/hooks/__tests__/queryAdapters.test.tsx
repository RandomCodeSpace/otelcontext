import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { useSystemGraph } from '../useSystemGraph'
import { useDashboard } from '../useDashboard'

const fetchMock = vi.fn<typeof fetch>()

function makeWrapper() {
  // retry: false so error paths settle immediately under test.
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  )
}

function jsonResponse(body: unknown, headers?: Record<string, string>) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json', ...headers },
  })
}

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
  fetchMock.mockReset()
})

const GRAPH = {
  timestamp: '2026-06-11T00:00:00Z',
  system: {
    total_services: 1,
    healthy: 1,
    degraded: 0,
    critical: 0,
    overall_health_score: 0.94,
    total_error_rate: 0.01,
    avg_latency_ms: 12,
    uptime_seconds: 60,
  },
  nodes: [],
  edges: [],
}

describe('useSystemGraph (TanStack Query adapter)', () => {
  it('keeps the legacy return shape: graph, cache, loading, error, reload', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse(GRAPH, { 'X-Cache': 'HIT' }))
    const { result } = renderHook(() => useSystemGraph(0), {
      wrapper: makeWrapper(),
    })

    expect(result.current.loading).toBe(true)
    expect(result.current.graph).toBeNull()

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.graph).toEqual(GRAPH)
    expect(result.current.cache).toBe('HIT')
    expect(result.current.error).toBeNull()
    expect(typeof result.current.reload).toBe('function')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/system/graph',
      expect.any(Object),
    )
  })

  it('surfaces HTTP failures as an error string', async () => {
    fetchMock.mockResolvedValueOnce(new Response('down', { status: 503 }))
    const { result } = renderHook(() => useSystemGraph(0), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.graph).toBeNull()
    expect(result.current.error).toContain('503')
  })

  it('reload() triggers a refetch', async () => {
    fetchMock.mockResolvedValue(jsonResponse(GRAPH, { 'X-Cache': 'MISS' }))
    const { result } = renderHook(() => useSystemGraph(0), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.loading).toBe(false))
    const calls = fetchMock.mock.calls.length
    result.current.reload()
    await waitFor(() =>
      expect(fetchMock.mock.calls.length).toBeGreaterThan(calls),
    )
  })
})

describe('useDashboard (TanStack Query adapter)', () => {
  const DASH = {
    total_traces: 10,
    total_logs: 20,
    total_errors: 1,
    avg_latency_ms: 5,
    error_rate: 4.2,
    active_services: 3,
    p99_latency_ms: 230,
    top_failing_services: [],
  }
  const STATS = { db_size_mb: 1228.8 }

  function routeFetches() {
    fetchMock.mockImplementation((input) => {
      const url = String(input)
      if (url.includes('/api/metrics/dashboard')) {
        return Promise.resolve(jsonResponse(DASH))
      }
      if (url.includes('/api/stats')) {
        return Promise.resolve(jsonResponse(STATS))
      }
      return Promise.resolve(new Response('not found', { status: 404 }))
    })
  }

  it('keeps the legacy return shape: dashboard, stats, loading, error, reload', async () => {
    routeFetches()
    const { result } = renderHook(() => useDashboard(0), {
      wrapper: makeWrapper(),
    })

    expect(result.current.loading).toBe(true)
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.dashboard).toEqual(DASH)
    expect(result.current.stats).toEqual(STATS)
    expect(result.current.error).toBeNull()
  })

  it('reports an error when either endpoint fails', async () => {
    fetchMock.mockImplementation((input) => {
      const url = String(input)
      if (url.includes('/api/metrics/dashboard')) {
        return Promise.resolve(jsonResponse(DASH))
      }
      return Promise.resolve(new Response('boom', { status: 500 }))
    })
    const { result } = renderHook(() => useDashboard(0), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.dashboard).toEqual(DASH)
    expect(result.current.error).toContain('500')
  })

  it('reload() refetches both endpoints', async () => {
    routeFetches()
    const { result } = renderHook(() => useDashboard(0), {
      wrapper: makeWrapper(),
    })
    await waitFor(() => expect(result.current.loading).toBe(false))
    const calls = fetchMock.mock.calls.length
    result.current.reload()
    await waitFor(() =>
      expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(calls + 2),
    )
  })
})
