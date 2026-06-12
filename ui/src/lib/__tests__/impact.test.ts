import { describe, expect, it } from 'vitest'
import {
  IMPACT_MAX_DEPTH,
  downstreamDepths,
  groupAffectedByDepth,
  impactAlpha,
} from '../impact'
import type { AffectedEntry } from '@/types/api'

const edge = (source: string, target: string) => ({ source, target })

describe('downstreamDepths', () => {
  it('BFS-labels the downstream cone with min depth from the root', () => {
    const edges = [
      edge('a', 'b'),
      edge('b', 'c'),
      edge('a', 'c'), // shorter path to c wins
      edge('c', 'd'),
      edge('x', 'y'), // disconnected — not in the cone
    ]
    const depths = downstreamDepths(edges, 'a')
    expect(depths.get('a')).toBe(0)
    expect(depths.get('b')).toBe(1)
    expect(depths.get('c')).toBe(1)
    expect(depths.get('d')).toBe(2)
    expect(depths.has('x')).toBe(false)
    expect(depths.has('y')).toBe(false)
  })

  it('tolerates cycles without looping', () => {
    const depths = downstreamDepths([edge('a', 'b'), edge('b', 'a')], 'a')
    expect(depths.get('b')).toBe(1)
    expect(depths.get('a')).toBe(0)
  })

  it('caps the walk at maxDepth (default matches the MCP tool)', () => {
    const chain = Array.from({ length: 8 }, (_, i) => edge(`s${i}`, `s${i + 1}`))
    const depths = downstreamDepths(chain, 's0')
    expect(Math.max(...depths.values())).toBe(IMPACT_MAX_DEPTH)
    expect(depths.has(`s${IMPACT_MAX_DEPTH + 1}`)).toBe(false)
  })

  it('returns just the root for a leaf service', () => {
    const depths = downstreamDepths([edge('a', 'b')], 'b')
    expect([...depths.keys()]).toEqual(['b'])
  })
})

describe('impactAlpha', () => {
  it('fades with depth but never below the floor', () => {
    expect(impactAlpha(1)).toBeGreaterThan(impactAlpha(2))
    expect(impactAlpha(2)).toBeGreaterThan(impactAlpha(3))
    expect(impactAlpha(50)).toBeGreaterThan(0)
    expect(impactAlpha(1)).toBeLessThanOrEqual(1)
  })
})

describe('groupAffectedByDepth', () => {
  it('groups entries by depth, ascending, stable within a depth', () => {
    const entries: AffectedEntry[] = [
      { service: 'c', depth: 2, call_count: 1, impact_score: 0.1 },
      { service: 'a', depth: 1, call_count: 5, impact_score: 0.5 },
      { service: 'b', depth: 1, call_count: 2, impact_score: 0.3 },
    ]
    const groups = groupAffectedByDepth(entries)
    expect(groups.map(([depth]) => depth)).toEqual([1, 2])
    expect(groups[0][1].map((e) => e.service)).toEqual(['a', 'b'])
  })

  it('returns [] for no entries', () => {
    expect(groupAffectedByDepth([])).toEqual([])
  })
})
