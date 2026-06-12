import { describe, expect, it } from 'vitest'
import { cryptoRandom } from '../random'

describe('cryptoRandom', () => {
  it('stays in [0, 1)', () => {
    for (let i = 0; i < 1000; i++) {
      const v = cryptoRandom()
      expect(v).toBeGreaterThanOrEqual(0)
      expect(v).toBeLessThan(1)
    }
  })

  it('is not constant across draws', () => {
    const draws = new Set(Array.from({ length: 32 }, () => cryptoRandom()))
    expect(draws.size).toBeGreaterThan(1)
  })
})
