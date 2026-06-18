import { describe, expect, it } from 'vitest'
import {
  compareIds,
  edgeWidth,
  neighborsOf,
  type GraphEdgeRef,
} from '../dagLayout'

const E = (source: string, target: string): GraphEdgeRef => ({ source, target })

describe('edgeWidth', () => {
  it('grows with log of call count and clamps to [1, 5]', () => {
    expect(edgeWidth(0)).toBe(1)
    expect(edgeWidth(10)).toBeGreaterThan(edgeWidth(1))
    expect(edgeWidth(1e9)).toBe(5)
  })
})

describe('neighborsOf', () => {
  it('collects 1-hop neighbors in both directions', () => {
    const edges = [E('a', 'b'), E('c', 'a'), E('b', 'd')]
    expect(neighborsOf(edges, 'a')).toEqual(new Set(['b', 'c']))
  })
})

describe('compareIds (locale-independent, codepoint order)', () => {
  it('orders by UTF-16 code unit, not locale', () => {
    // Under many locales localeCompare puts 'a' before 'Z'; codepoint order is 'Z'(90) < 'a'(97).
    expect(compareIds('Z', 'a')).toBeLessThan(0)
    expect(['a', 'Z', 'B'].slice().sort(compareIds)).toEqual(['B', 'Z', 'a'])
  })

  it('is a total order with stable equality', () => {
    expect(compareIds('svc-1', 'svc-1')).toBe(0)
    expect(compareIds('svc-1', 'svc-2')).toBeLessThan(0)
    expect(compareIds('svc-2', 'svc-1')).toBeGreaterThan(0)
  })
})
