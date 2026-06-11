import { describe, expect, it } from 'vitest'
import { createQueryClient, retryDelayWithJitter } from '../queryClient'

describe('createQueryClient', () => {
  it('aligns staleTime to the server-side 10s cache TTL', () => {
    const qc = createQueryClient()
    const defaults = qc.getDefaultOptions().queries
    expect(defaults?.staleTime).toBe(10_000)
  })

  it('garbage-collects after 5 minutes', () => {
    const qc = createQueryClient()
    expect(qc.getDefaultOptions().queries?.gcTime).toBe(5 * 60_000)
  })

  it('stops polling in hidden tabs (refetchIntervalInBackground false)', () => {
    const qc = createQueryClient()
    expect(qc.getDefaultOptions().queries?.refetchIntervalInBackground).toBe(
      false,
    )
  })

  it('retries transient failures a bounded number of times', () => {
    const qc = createQueryClient()
    expect(qc.getDefaultOptions().queries?.retry).toBe(2)
  })
})

describe('retryDelayWithJitter', () => {
  it('grows exponentially with the attempt index', () => {
    // Jitter adds at most 20%, so attempt boundaries cannot overlap.
    const d0 = retryDelayWithJitter(0)
    const d2 = retryDelayWithJitter(2)
    expect(d0).toBeGreaterThanOrEqual(1000)
    expect(d0).toBeLessThanOrEqual(1200)
    expect(d2).toBeGreaterThanOrEqual(4000)
    expect(d2).toBeLessThanOrEqual(4800)
  })

  it('caps the base delay at 30s', () => {
    const d = retryDelayWithJitter(10)
    expect(d).toBeGreaterThanOrEqual(30_000)
    expect(d).toBeLessThanOrEqual(36_000)
  })

  it('is jittered (not deterministic across calls)', () => {
    const samples = new Set(
      Array.from({ length: 32 }, () => retryDelayWithJitter(3)),
    )
    expect(samples.size).toBeGreaterThan(1)
  })
})
