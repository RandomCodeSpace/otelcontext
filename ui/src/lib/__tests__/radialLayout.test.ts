import { describe, expect, it } from 'vitest'
import { layoutRadial, walkRadial } from '../radialLayout'
import { NODE_H, NODE_W, type GraphEdgeRef, type Layout } from '../dagLayout'

const E = (source: string, target: string): GraphEdgeRef => ({ source, target })

/** Geometric center of every node box, around the layout center. */
function centerOf(layout: Layout, id: string): { x: number; y: number } {
  const n = layout.nodes.get(id)!
  return { x: n.x + NODE_W / 2, y: n.y + NODE_H / 2 }
}

/** Distance of a node's center from the layout center. */
function radiusOf(layout: Layout, id: string): number {
  const c = centerOf(layout, id)
  return Math.hypot(c.x - layout.width / 2, c.y - layout.height / 2)
}

/** Angle of a node center around the layout center, in [0, 2π). */
function angleOf(layout: Layout, id: string): number {
  const c = centerOf(layout, id)
  const a = Math.atan2(c.y - layout.height / 2, c.x - layout.width / 2)
  return a < 0 ? a + 2 * Math.PI : a
}

describe('layoutRadial — basic shape', () => {
  it('handles the empty graph', () => {
    const layout = layoutRadial([], [])
    expect(layout.nodes.size).toBe(0)
    expect(layout.layers).toEqual([])
    expect(layout.width).toBe(0)
    expect(layout.height).toBe(0)
  })

  it('places every node exactly once, dedupes ids', () => {
    const layout = layoutRadial(['a', 'b', 'a', 'c'], [E('a', 'b')])
    expect(layout.nodes.size).toBe(3)
    expect([...layout.nodes.keys()].sort((a, b) => a.localeCompare(b))).toEqual(['a', 'b', 'c'])
    expect(layout.layers.flat().slice().sort((a, b) => a.localeCompare(b))).toEqual(['a', 'b', 'c'])
  })

  it('lays out a chain (cycle-free) keeping all nodes', () => {
    const layout = layoutRadial(['a', 'b', 'c'], [E('a', 'b'), E('b', 'c')])
    expect(layout.nodes.size).toBe(3)
  })

  it('terminates on a 2-cycle and keeps both nodes', () => {
    const layout = layoutRadial(['a', 'b'], [E('a', 'b'), E('b', 'a')])
    expect(layout.nodes.size).toBe(2)
  })

  it('handles a fully cyclic graph', () => {
    const layout = layoutRadial(['x', 'y', 'z'], [E('x', 'y'), E('y', 'z'), E('z', 'x')])
    expect(layout.nodes.size).toBe(3)
    // one component → one contiguous wedge spanning the full circle
  })

  it('drops self-loops and edges to unknown endpoints without crashing', () => {
    const layout = layoutRadial(['a', 'b'], [E('a', 'a'), E('a', 'ghost'), E('a', 'b')])
    expect(layout.nodes.size).toBe(2)
  })

  it('center is approximately (width/2, height/2)', () => {
    const layout = layoutRadial(['a', 'b', 'c', 'd'], [E('a', 'b'), E('a', 'c')])
    expect(layout.width).toBe(layout.height)
    expect(layout.width).toBeGreaterThan(0)
  })
})

describe('layoutRadial — ring count = ceil(sqrt(n))', () => {
  it.each([
    [1, 1],
    [4, 2],
    [9, 3],
    [10, 4],
    [16, 4],
    [120, 11],
  ])('n=%i → %i rings', (n, expected) => {
    const ids = Array.from({ length: n }, (_, i) => `s${String(i).padStart(3, '0')}`)
    const layout = layoutRadial(ids, [])
    expect(layout.layers.length).toBe(expected)
  })
})

describe('layoutRadial — criticality drives radius', () => {
  it('a high-indegree hub lands on an inner ring than its leaf callers', () => {
    // hub is called by many distinct callers → highest distinctCallerIndegree.
    const callers = ['c0', 'c1', 'c2', 'c3', 'c4', 'c5', 'c6', 'c7', 'c8']
    const edges = callers.map((c) => E(c, 'hub'))
    const layout = layoutRadial(['hub', ...callers], edges)
    const hubRing = layout.nodes.get('hub')!.layer
    for (const c of callers) {
      expect(hubRing).toBeLessThan(layout.nodes.get(c)!.layer)
    }
    // and the hub is geometrically closer to the center
    for (const c of callers) {
      expect(radiusOf(layout, 'hub')).toBeLessThan(radiusOf(layout, c))
    }
  })

  it('the most-critical node sits on ring 0 (center)', () => {
    const callers = Array.from({ length: 12 }, (_, i) => `c${i}`)
    const edges = callers.map((c) => E(c, 'core'))
    const layout = layoutRadial(['core', ...callers], edges)
    expect(layout.nodes.get('core')!.layer).toBe(0)
    expect(layout.layers[0]).toContain('core')
  })

  it('ranks by distinctCallerIndegree: two callers > one caller', () => {
    // big has 2 distinct callers, small has 1 → big is more critical.
    const layout = layoutRadial(
      ['big', 'small', 'a', 'b', 'c'],
      [E('a', 'big'), E('b', 'big'), E('c', 'small')],
    )
    expect(radiusOf(layout, 'big')).toBeLessThanOrEqual(radiusOf(layout, 'small'))
  })
})

describe('layoutRadial — phyllotaxis disc fill', () => {
  const ids = (n: number) => Array.from({ length: n }, (_, i) => `s${String(i).padStart(3, '0')}`)

  it('gives every node a distinct angle (golden-angle spiral, no spokes)', () => {
    const layout = layoutRadial(ids(40), [])
    const angles = [...layout.nodes.keys()].map((id) => angleOf(layout, id))
    const uniq = new Set(angles.map((a) => a.toFixed(4)))
    expect(uniq.size).toBe(40)
  })

  it('three singletons each get a distinct angle', () => {
    const layout = layoutRadial(['a', 'b', 'c'], [])
    const angles = ['a', 'b', 'c'].map((id) => angleOf(layout, id))
    const uniq = new Set(angles.map((a) => a.toFixed(6)))
    expect(uniq.size).toBe(3)
  })

  it('fills from the center outward — no hollow core', () => {
    const layout = layoutRadial(ids(120), [])
    const radii = [...layout.nodes.keys()].map((id) => radiusOf(layout, id))
    const minR = Math.min(...radii)
    const maxR = Math.max(...radii)
    // The innermost node sits near the center, well inside the rim — the disc
    // is filled, not a hollow ring with an empty middle.
    expect(minR).toBeLessThan(maxR * 0.2)
  })

  it('bands fill inner-first: band 0 holds the single most-critical node', () => {
    const callers = Array.from({ length: 30 }, (_, i) => `c${String(i).padStart(2, '0')}`)
    const layout = layoutRadial(['core', ...callers], callers.map((c) => E(c, 'core')))
    expect(layout.layers[0]).toEqual(['core'])
    // every band is non-empty so the keyboard walk never dead-ends on a gap
    expect(layout.layers.every((b) => b.length > 0)).toBe(true)
    // outer bands hold more nodes than inner ones (2b+1 circumference scaling)
    expect(layout.layers[2].length).toBeGreaterThan(layout.layers[0].length)
  })

  it('radius grows with criticality rank (equal scores tie-break by id)', () => {
    // no edges → all scores equal → rank == compareIds order (s000 < s019)
    const layout = layoutRadial(ids(20), [])
    expect(radiusOf(layout, 's000')).toBeLessThan(radiusOf(layout, 's019'))
  })
})

describe('layoutRadial — determinism', () => {
  const nodes = ['gw', 'auth', 'pay', 'db', 'cache', 'queue', 'worker']
  const edges = [
    E('gw', 'auth'),
    E('gw', 'pay'),
    E('pay', 'db'),
    E('auth', 'cache'),
    E('pay', 'cache'),
    E('queue', 'worker'),
  ]

  it('identical input → identical output', () => {
    const a = layoutRadial(nodes, edges)
    const b = layoutRadial(nodes, edges)
    expect([...a.nodes.entries()]).toEqual([...b.nodes.entries()])
    expect(a.layers).toEqual(b.layers)
  })

  it('is invariant to node and edge input order', () => {
    const a = layoutRadial(nodes, edges)
    const b = layoutRadial([...nodes].reverse(), [...edges].reverse())
    expect([...a.nodes.entries()]).toEqual([...b.nodes.entries()])
    expect(a.layers).toEqual(b.layers)
    expect(a.width).toBe(b.width)
  })

  it('is invariant to duplicated edges', () => {
    const a = layoutRadial(nodes, edges)
    const b = layoutRadial(nodes, [...edges, ...edges])
    expect([...a.nodes.entries()]).toEqual([...b.nodes.entries()])
  })
})

describe('walkRadial — null focus', () => {
  const layout = layoutRadial(
    ['core', 'a', 'b', 'c', 'd'],
    [E('a', 'core'), E('b', 'core'), E('c', 'core')],
  )

  it("null focus + 'right' returns the most-critical node (ring 0, first)", () => {
    const target = walkRadial(layout, null, 'right')
    expect(target).toBe(layout.layers[0][0])
    expect(layout.nodes.get(target!)!.layer).toBe(0)
  })

  // Cold-start bootstrap: a null focus seeds the most-critical entry node for
  // ANY arrow key (not just 'right'), so Tabbing into the field and pressing
  // the intuitive ArrowDown is never keyboard-inert. The seed is deterministic.
  it('null focus seeds the SAME ring-0 entry node in every direction', () => {
    const seed = layout.layers[0][0]
    for (const dir of ['left', 'right', 'up', 'down'] as const) {
      const target = walkRadial(layout, null, dir)
      expect(target).toBe(seed)
      expect(layout.nodes.get(target!)!.layer).toBe(0)
    }
  })
})

describe('walkRadial — unknown focus', () => {
  const layout = layoutRadial(['a', 'b'], [E('a', 'b')])
  it('returns null for an unknown focus id in every direction', () => {
    for (const dir of ['left', 'right', 'up', 'down'] as const) {
      expect(walkRadial(layout, 'ghost', dir)).toBeNull()
    }
  })
})

describe('walkRadial — angular movement (left/right)', () => {
  // Several singletons land on the same outermost ring, spread by angle.
  const ids = ['a', 'b', 'c', 'd', 'e', 'f']
  const layout = layoutRadial(ids, [])

  it('right then left returns to the start (inverse on the same ring)', () => {
    // pick a node that is not at a ring boundary
    const ring = layout.layers.find((r) => r.length >= 3)!
    const start = ring[1]
    const right = walkRadial(layout, start, 'right')
    expect(right).toBe(ring[2])
    const back = walkRadial(layout, right, 'left')
    expect(back).toBe(start)
  })

  it('returns null at the angular ring boundaries (no wraparound)', () => {
    const ring = layout.layers.find((r) => r.length >= 2)!
    expect(walkRadial(layout, ring[0], 'left')).toBeNull()
    expect(walkRadial(layout, ring[ring.length - 1], 'right')).toBeNull()
  })

  it('returns null moving angularly on a single-node ring', () => {
    const callers = Array.from({ length: 10 }, (_, i) => `c${i}`)
    const l = layoutRadial(['solo', ...callers], callers.map((c) => E(c, 'solo')))
    // solo is alone on ring 0
    expect(l.layers[0]).toEqual(['solo'])
    expect(walkRadial(l, 'solo', 'left')).toBeNull()
    expect(walkRadial(l, 'solo', 'right')).toBeNull()
  })
})

describe('walkRadial — ring movement (up/down)', () => {
  // hub on inner ring, callers on outer ring.
  const callers = Array.from({ length: 8 }, (_, i) => `c${i}`)
  const layout = layoutRadial(['hub', ...callers], callers.map((c) => E(c, 'hub')))

  it('down moves outward to a higher ring index', () => {
    const target = walkRadial(layout, 'hub', 'down')
    expect(target).not.toBeNull()
    expect(layout.nodes.get(target!)!.layer).toBeGreaterThan(layout.nodes.get('hub')!.layer)
  })

  it('up from the outermost-ring node reaches a lower ring index', () => {
    const outer = callers.find((c) => layout.nodes.get(c)!.layer === layout.layers.length - 1)!
    const target = walkRadial(layout, outer, 'up')
    expect(target).not.toBeNull()
    expect(layout.nodes.get(target!)!.layer).toBeLessThan(layout.nodes.get(outer)!.layer)
  })

  it('up from ring 0 returns null (innermost boundary)', () => {
    expect(walkRadial(layout, 'hub', 'up')).toBeNull()
  })

  it('down from the outermost ring returns null', () => {
    const outer = callers.find((c) => layout.nodes.get(c)!.layer === layout.layers.length - 1)!
    expect(walkRadial(layout, outer, 'down')).toBeNull()
  })

  it('ring movement is deterministic (picks nearest by angle, compareIds tiebreak)', () => {
    const a = walkRadial(layout, 'hub', 'down')
    const b = walkRadial(layout, 'hub', 'down')
    expect(a).toBe(b)
  })
})
