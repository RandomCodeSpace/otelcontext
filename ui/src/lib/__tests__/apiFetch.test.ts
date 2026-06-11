import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, apiFetch, apiFetchWithResponse } from '../apiFetch'

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
  fetchMock.mockReset()
})

function jsonResponse(body: unknown, init?: ResponseInit): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

describe('apiFetch', () => {
  it('returns parsed JSON on success', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({ ok: true }))
    await expect(apiFetch<{ ok: boolean }>('/api/x')).resolves.toEqual({
      ok: true,
    })
    expect(fetchMock).toHaveBeenCalledWith('/api/x', expect.any(Object))
  })

  it('forwards the AbortSignal to fetch', async () => {
    fetchMock.mockResolvedValueOnce(jsonResponse({}))
    const controller = new AbortController()
    await apiFetch('/api/x', { signal: controller.signal })
    const init = fetchMock.mock.calls[0][1]
    expect(init?.signal).toBe(controller.signal)
  })

  it('throws ApiError carrying the HTTP status on non-2xx', async () => {
    fetchMock.mockResolvedValueOnce(
      new Response('{"error":"boom"}', { status: 503 }),
    )
    const err = await apiFetch('/api/x').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).status).toBe(503)
    expect((err as ApiError).message).toContain('503')
  })

  it('normalizes network failures into ApiError with status 0', async () => {
    fetchMock.mockRejectedValueOnce(new TypeError('Failed to fetch'))
    const err = await apiFetch('/api/x').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).status).toBe(0)
    expect((err as ApiError).message).toBe('Failed to fetch')
  })

  it('re-throws aborts untouched so callers/Query can recognize them', async () => {
    const abort = new DOMException('Aborted', 'AbortError')
    fetchMock.mockRejectedValueOnce(abort)
    await expect(apiFetch('/api/x')).rejects.toBe(abort)
  })

  it('throws ApiError when the body is not valid JSON', async () => {
    fetchMock.mockResolvedValueOnce(new Response('<html>', { status: 200 }))
    const err = await apiFetch('/api/x').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).status).toBe(200)
  })
})

describe('apiFetchWithResponse', () => {
  it('exposes the Response alongside the parsed body', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ n: 1 }, { headers: { 'X-Cache': 'HIT' } }),
    )
    const { data, response } = await apiFetchWithResponse<{ n: number }>(
      '/api/system/graph',
    )
    expect(data).toEqual({ n: 1 })
    expect(response.headers.get('X-Cache')).toBe('HIT')
  })
})
