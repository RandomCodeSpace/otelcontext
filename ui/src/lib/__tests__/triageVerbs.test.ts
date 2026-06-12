import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { impactQueryOptions, rootCauseQueryOptions } from '../triageVerbs'

// The options builders ride lib/mcpClient's fetch transport — stub fetch and
// exercise queryFn end-to-end (RPC envelope, isError surfacing, parsing).

const fetchMock = vi.fn<typeof fetch>()

function rpcResult(toolText: string, isError = false) {
  return new Response(
    JSON.stringify({
      jsonrpc: '2.0',
      id: 1,
      result: { isError, content: [{ type: 'text', text: toolText }] },
    }),
    { status: 200 },
  )
}

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.unstubAllGlobals()
  fetchMock.mockReset()
})

describe('rootCauseQueryOptions', () => {
  it('keys the cache per service and caches for 5 minutes', () => {
    const opts = rootCauseQueryOptions('checkout')
    expect(opts.queryKey).toEqual(['mcp', 'root_cause_analysis', 'checkout'])
    expect(opts.staleTime).toBe(300_000)
    expect(opts.gcTime).toBeGreaterThanOrEqual(300_000)
  })

  it('calls the root_cause_analysis tool and parses ranked causes', async () => {
    fetchMock.mockResolvedValueOnce(
      rpcResult(
        JSON.stringify([
          { service: 'db', operation: 'query', score: 2, evidence: ['x'] },
        ]),
      ),
    )
    const opts = rootCauseQueryOptions('checkout')
    const data = await opts.queryFn({ signal: new AbortController().signal })
    expect(data).toHaveLength(1)
    expect(data[0].service).toBe('db')

    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body))
    expect(body.method).toBe('tools/call')
    expect(body.params).toEqual({
      name: 'root_cause_analysis',
      arguments: { service: 'checkout' },
    })
  })

  it('throws the tool error text when the tool flags isError', async () => {
    fetchMock.mockResolvedValueOnce(rpcResult('Error: GraphRAG not initialized', true))
    const opts = rootCauseQueryOptions('checkout')
    await expect(
      opts.queryFn({ signal: new AbortController().signal }),
    ).rejects.toThrow(/GraphRAG not initialized/)
  })
})

describe('impactQueryOptions', () => {
  it('keys the cache per service', () => {
    expect(impactQueryOptions('checkout').queryKey).toEqual([
      'mcp',
      'impact_analysis',
      'checkout',
    ])
  })

  it('calls impact_analysis and parses the impact result', async () => {
    fetchMock.mockResolvedValueOnce(
      rpcResult(
        JSON.stringify({
          service: 'checkout',
          affected_services: [
            { service: 'payments', depth: 1, call_count: 3, impact_score: 0.2 },
          ],
          total_downstream: 1,
        }),
      ),
    )
    const data = await impactQueryOptions('checkout').queryFn({
      signal: new AbortController().signal,
    })
    expect(data.total_downstream).toBe(1)
    expect(data.affected_services[0].service).toBe('payments')
  })

  it('throws when the payload is not a valid impact result', async () => {
    fetchMock.mockResolvedValueOnce(rpcResult('garbage'))
    await expect(
      impactQueryOptions('checkout').queryFn({
        signal: new AbortController().signal,
      }),
    ).rejects.toThrow(/unexpected/i)
  })
})
