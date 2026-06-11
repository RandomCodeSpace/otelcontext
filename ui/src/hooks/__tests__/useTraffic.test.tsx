import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useTraffic } from '../useTraffic'

const POINTS = [
  { timestamp: '2026-06-11T00:00:00Z', count: 100, error_count: 4 },
  { timestamp: '2026-06-11T00:01:00Z', count: 120, error_count: 2 },
]

let responder: () => Promise<Response> = () =>
  Promise.resolve(new Response(JSON.stringify(POINTS), { status: 200 }))

beforeEach(() => {
  responder = () =>
    Promise.resolve(new Response(JSON.stringify(POINTS), { status: 200 }))
  vi.stubGlobal(
    'fetch',
    vi.fn<typeof fetch>(() => responder()),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

function setup() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  )
  return { qc, ...renderHook(() => useTraffic(0), { wrapper }) }
}

describe('useTraffic (TanStack Query adapter)', () => {
  it('loads points into the shared ["metrics-traffic"] cache key', async () => {
    const { result, qc } = setup()
    expect(result.current.loading).toBe(true)
    await waitFor(() => expect(result.current.points).toEqual(POINTS))
    expect(result.current.error).toBeNull()
    expect(qc.getQueryData(['metrics-traffic'])).toEqual(POINTS)
  })

  it('normalizes a non-array payload to []', async () => {
    responder = () => Promise.resolve(new Response('{}', { status: 200 }))
    const { result } = setup()
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.points).toEqual([])
  })

  it('surfaces fetch errors and reloads on demand', async () => {
    responder = () => Promise.resolve(new Response('x', { status: 500 }))
    const { result } = setup()
    await waitFor(() => expect(result.current.error).not.toBeNull())
    responder = () =>
      Promise.resolve(new Response(JSON.stringify(POINTS), { status: 200 }))
    result.current.reload()
    await waitFor(() => expect(result.current.points).toEqual(POINTS))
  })
})
