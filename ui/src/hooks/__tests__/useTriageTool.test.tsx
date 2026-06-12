import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { useTriageTool } from '../useTriageTool'
import type { TriageQueryOptions } from '@/lib/triageVerbs'

function makeClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
}

function wrapperFor(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  )
}

function options<T>(
  fn: (ctx: { signal: AbortSignal }) => Promise<T>,
  key = 'svc',
): TriageQueryOptions<T> {
  return {
    queryKey: ['mcp', 'tool', key] as const,
    queryFn: fn,
    staleTime: 300_000,
    gcTime: 600_000,
  }
}

afterEach(() => {
  vi.useRealTimers()
})

describe('useTriageTool', () => {
  it('stays idle until run() — no RPC on mount', async () => {
    const fn = vi.fn(async () => ['x'])
    const qc = makeClient()
    const { result } = renderHook(() => useTriageTool(options(fn)), {
      wrapper: wrapperFor(qc),
    })
    expect(result.current.status).toBe('idle')
    await new Promise((r) => setTimeout(r, 10))
    expect(fn).not.toHaveBeenCalled()
  })

  it('run() fires the RPC and lands in success with data', async () => {
    const fn = vi.fn(async () => [{ score: 1 }])
    const qc = makeClient()
    const { result } = renderHook(() => useTriageTool(options(fn)), {
      wrapper: wrapperFor(qc),
    })
    act(() => result.current.run())
    await waitFor(() => expect(result.current.status).toBe('success'))
    expect(result.current.data).toEqual([{ score: 1 }])
    expect(fn).toHaveBeenCalledTimes(1)
  })

  it('reports loading while the RPC is in flight', async () => {
    let resolve!: (v: string[]) => void
    const fn = vi.fn(
      () => new Promise<string[]>((res) => (resolve = res)),
    )
    const qc = makeClient()
    const { result } = renderHook(() => useTriageTool(options(fn)), {
      wrapper: wrapperFor(qc),
    })
    act(() => result.current.run())
    await waitFor(() => expect(result.current.status).toBe('loading'))
    act(() => resolve(['done']))
    await waitFor(() => expect(result.current.status).toBe('success'))
  })

  it('cancel() aborts the in-flight RPC and returns to idle', async () => {
    const fn = vi.fn(
      ({ signal }: { signal: AbortSignal }) =>
        new Promise<string[]>((_res, rej) => {
          signal.addEventListener('abort', () => rej(new Error('aborted')))
        }),
    )
    const qc = makeClient()
    const { result } = renderHook(() => useTriageTool(options(fn)), {
      wrapper: wrapperFor(qc),
    })
    act(() => result.current.run())
    await waitFor(() => expect(result.current.status).toBe('loading'))
    await act(async () => {
      await result.current.cancel()
    })
    await waitFor(() => expect(result.current.status).toBe('idle'))
    expect(result.current.data).toBeUndefined()
  })

  it('surfaces RPC failures as error with the message', async () => {
    const fn = vi.fn(async () => {
      throw new Error('GraphRAG not initialized')
    })
    const qc = makeClient()
    const { result } = renderHook(() => useTriageTool(options(fn)), {
      wrapper: wrapperFor(qc),
    })
    act(() => result.current.run())
    await waitFor(() => expect(result.current.status).toBe('error'))
    expect(result.current.error).toMatch(/GraphRAG not initialized/)
  })

  it('a remount with a cached entry shows the result without a new RPC', async () => {
    const fn = vi.fn(async () => ['cached'])
    const qc = makeClient()
    const opts = options(fn)
    const first = renderHook(() => useTriageTool(opts), {
      wrapper: wrapperFor(qc),
    })
    act(() => first.result.current.run())
    await waitFor(() => expect(first.result.current.status).toBe('success'))
    first.unmount()

    const second = renderHook(() => useTriageTool(opts), {
      wrapper: wrapperFor(qc),
    })
    await waitFor(() => expect(second.result.current.status).toBe('success'))
    expect(second.result.current.data).toEqual(['cached'])
    expect(fn).toHaveBeenCalledTimes(1) // staleTime 5min — no refetch
  })

  it('a palette prefetch in flight is picked up without a second RPC', async () => {
    let resolve!: (v: string[]) => void
    const fn = vi.fn(
      () => new Promise<string[]>((res) => (resolve = res)),
    )
    const qc = makeClient()
    const opts = options(fn)
    void qc.prefetchQuery(opts)
    const { result } = renderHook(() => useTriageTool(opts), {
      wrapper: wrapperFor(qc),
    })
    await waitFor(() => expect(result.current.status).toBe('loading'))
    act(() => resolve(['prefetched']))
    await waitFor(() => expect(result.current.status).toBe('success'))
    expect(fn).toHaveBeenCalledTimes(1)
  })

  it('run() with existing data re-runs the analysis', async () => {
    const fn = vi
      .fn<(ctx: { signal: AbortSignal }) => Promise<string[]>>()
      .mockResolvedValueOnce(['first'])
      .mockResolvedValueOnce(['second'])
    const qc = makeClient()
    const { result } = renderHook(() => useTriageTool(options(fn)), {
      wrapper: wrapperFor(qc),
    })
    act(() => result.current.run())
    await waitFor(() => expect(result.current.data).toEqual(['first']))
    act(() => result.current.run())
    await waitFor(() => expect(result.current.data).toEqual(['second']))
    expect(fn).toHaveBeenCalledTimes(2)
  })
})
