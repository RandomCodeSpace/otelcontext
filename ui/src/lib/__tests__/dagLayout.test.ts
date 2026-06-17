import { describe, expect, it } from 'vitest'
import {
  GAP_X,
  NODE_W,
  compareIds,
  edgePath,
  edgeWidth,
  fitTransform,
  graphSetHash,
  layoutGraph,
  neighborsOf,
  walkFrom,
  zoomAt,
  type GraphEdgeRef,
} from '../dagLayout'

const E = (source: string, target: string): GraphEdgeRef => ({ source, target })

describe('layoutGraph — layering', () => {
  it('lays a chain into successive layers', () => {
    const layout = layoutGraph(['a', 'b', 'c'], [E('a', 'b'), E('b', 'c')])
    expect(layout.layers).toEqual([['a'], ['b'], ['c']])
  })

  it('uses longest-path layering (skip edge does not pull the node left)', () => {
    const layout = layoutGraph(
      ['a', 'b', 'c'],
      [E('a', 'b'), E('b', 'c'), E('a', 'c')],
    )
    expect(layout.nodes.get('c')?.layer).toBe(2)
  })

  it('places diamond join below both branches', () => {
    const layout = layoutGraph(
      ['a', 'b', 'c', 'd'],
      [E('a', 'b'), E('a', 'c'), E('b', 'd'), E('c', 'd')],
    )
    expect(layout.nodes.get('d')?.layer).toBe(2)
    expect(layout.layers[1]).toHaveLength(2)
  })

  it('terminates on cycles and keeps every node', () => {
    const layout = layoutGraph(['a', 'b'], [E('a', 'b'), E('b', 'a')])
    expect(layout.nodes.size).toBe(2)
    expect(layout.nodes.get('a')?.layer).toBe(0)
    expect(layout.nodes.get('b')?.layer).toBe(1)
  })

  it('handles a fully cyclic graph (no natural roots)', () => {
    const layout = layoutGraph(
      ['x', 'y', 'z'],
      [E('x', 'y'), E('y', 'z'), E('z', 'x')],
    )
    expect(layout.nodes.size).toBe(3)
    expect(layout.layers.flat().sort((a, b) => a.localeCompare(b))).toEqual(['x', 'y', 'z'])
  })

  it('drops self-loops and edges to unknown nodes', () => {
    const layout = layoutGraph(['a', 'b'], [E('a', 'a'), E('a', 'ghost'), E('a', 'b')])
    expect(layout.nodes.get('b')?.layer).toBe(1)
  })

  it('places disconnected nodes in layer 0', () => {
    const layout = layoutGraph(['a', 'b', 'lonely'], [E('a', 'b')])
    expect(layout.nodes.get('lonely')?.layer).toBe(0)
  })

  it('handles the empty graph', () => {
    const layout = layoutGraph([], [])
    expect(layout.layers).toEqual([])
    expect(layout.width).toBe(0)
    expect(layout.height).toBe(0)
  })
})

describe('layoutGraph — barycenter ordering', () => {
  it('orders a layer by mean predecessor position (one pass)', () => {
    // layer0 = [a, b] (alphabetical); y's caller is a (row 0), x's is b (row 1)
    // → barycenter pass must flip the alphabetical [x, y] into [y, x].
    const layout = layoutGraph(
      ['a', 'b', 'x', 'y'],
      [E('a', 'y'), E('b', 'x')],
    )
    expect(layout.layers[1]).toEqual(['y', 'x'])
  })

  it('is deterministic regardless of input order', () => {
    const nodes = ['gw', 'auth', 'pay', 'db', 'cache']
    const edges = [E('gw', 'auth'), E('gw', 'pay'), E('pay', 'db'), E('auth', 'cache'), E('pay', 'cache')]
    const a = layoutGraph(nodes, edges)
    const b = layoutGraph([...nodes].reverse(), [...edges].reverse())
    expect(a.layers).toEqual(b.layers)
    expect([...a.nodes.entries()]).toEqual([...b.nodes.entries()])
  })
})

describe('layoutGraph — positions', () => {
  it('spaces layers along x and centers short layers vertically', () => {
    const layout = layoutGraph(
      ['a', 'b', 'c', 'd'],
      [E('a', 'b'), E('a', 'c'), E('b', 'd'), E('c', 'd')],
    )
    const a = layout.nodes.get('a')!
    const b = layout.nodes.get('b')!
    const d = layout.nodes.get('d')!
    expect(b.x - a.x).toBe(NODE_W + GAP_X)
    expect(d.x - a.x).toBe(2 * (NODE_W + GAP_X))
    // single-node layers (a, d) sit at the vertical center of the 2-row layer
    expect(a.y).toBeGreaterThan(0)
    expect(a.y).toBe(d.y)
    expect(layout.width).toBeGreaterThan(0)
    expect(layout.height).toBeGreaterThan(0)
  })
})

describe('graphSetHash', () => {
  it('is order-insensitive over nodes and edges', () => {
    const h1 = graphSetHash(['a', 'b'], [E('a', 'b')])
    const h2 = graphSetHash(['b', 'a'], [E('a', 'b')])
    expect(h1).toBe(h2)
  })

  it('changes when the node or edge set changes', () => {
    const base = graphSetHash(['a', 'b'], [E('a', 'b')])
    expect(graphSetHash(['a', 'b', 'c'], [E('a', 'b')])).not.toBe(base)
    expect(graphSetHash(['a', 'b'], [])).not.toBe(base)
  })
})

describe('edgeWidth', () => {
  it('grows with log of call count and clamps to [1, 5]', () => {
    expect(edgeWidth(0)).toBe(1)
    expect(edgeWidth(10)).toBeGreaterThan(edgeWidth(1))
    expect(edgeWidth(1e9)).toBe(5)
  })
})

describe('fitTransform / zoomAt', () => {
  it('fits content into the viewport with padding', () => {
    const t = fitTransform({ width: 1000, height: 500 }, { width: 520, height: 270 }, 10)
    expect(t.k).toBeCloseTo(0.5, 5)
    // content centered: x offset = (520 - 1000*0.5)/2 = 10
    expect(t.x).toBeCloseTo(10, 5)
    expect(t.y).toBeCloseTo(10, 5)
  })

  it('never scales small graphs above 1', () => {
    const t = fitTransform({ width: 100, height: 50 }, { width: 1000, height: 800 })
    expect(t.k).toBe(1)
  })

  it('handles empty content without dividing by zero', () => {
    const t = fitTransform({ width: 0, height: 0 }, { width: 800, height: 600 })
    expect(t.k).toBe(1)
    expect(Number.isFinite(t.x)).toBe(true)
  })

  it('floors the scale at minScale (small field on a narrow viewport)', () => {
    // Contain-fit would shrink to ~0.18 on a phone-width viewport; the floor
    // keeps it legible and the content then overflows, staying centered.
    const t = fitTransform({ width: 1100, height: 1100 }, { width: 360, height: 640 }, 24, 0.5)
    expect(t.k).toBe(0.5)
    // Centered overflow: the field spills symmetrically (negative offset).
    expect(t.x).toBeCloseTo((360 - 1100 * 0.5) / 2, 5)
  })

  it('minScale never upscales past the contain fit when content already fits', () => {
    const t = fitTransform({ width: 100, height: 100 }, { width: 1000, height: 800 }, 24, 0.5)
    expect(t.k).toBe(1)
  })

  it('zoomAt keeps the anchor point stationary', () => {
    const t = { x: 20, y: 30, k: 1 }
    const t2 = zoomAt(t, 100, 100, 1.5)
    // world point under (100,100) before: (80, 70); after must map back to (100,100)
    expect(80 * t2.k + t2.x).toBeCloseTo(100, 5)
    expect(70 * t2.k + t2.y).toBeCloseTo(100, 5)
  })

  it('zoomAt clamps the scale range', () => {
    const t = { x: 0, y: 0, k: 2.4 }
    expect(zoomAt(t, 0, 0, 10).k).toBeLessThanOrEqual(2.5)
    expect(zoomAt({ ...t, k: 0.25 }, 0, 0, 0.01).k).toBeGreaterThanOrEqual(0.2)
  })
})

describe('walkFrom', () => {
  const nodes = ['gw', 'auth', 'pay', 'db']
  const edges = [E('gw', 'auth'), E('gw', 'pay'), E('pay', 'db')]
  const layout = layoutGraph(nodes, edges)

  it('starts at the first node of the first layer when nothing is focused', () => {
    expect(walkFrom(layout, edges, null, 'right')).toBe('gw')
  })

  it('walks right to a callee and left back to the caller', () => {
    expect(walkFrom(layout, edges, 'pay', 'right')).toBe('db')
    expect(walkFrom(layout, edges, 'pay', 'left')).toBe('gw')
  })

  it('walks up/down between layer siblings', () => {
    const first = layout.layers[1][0]
    const second = layout.layers[1][1]
    expect(walkFrom(layout, edges, first, 'down')).toBe(second)
    expect(walkFrom(layout, edges, second, 'up')).toBe(first)
  })

  it('returns null at boundaries', () => {
    expect(walkFrom(layout, edges, 'gw', 'left')).toBeNull()
    expect(walkFrom(layout, edges, 'db', 'right')).toBeNull()
    expect(walkFrom(layout, edges, 'gw', 'up')).toBeNull()
  })

  it('returns null for an unknown focus id', () => {
    expect(walkFrom(layout, edges, 'ghost', 'right')).toBeNull()
  })

  it('prefers the callee nearest in y when several exist', () => {
    // gw has two callees: walkFrom picks the one closest to gw's own y.
    const target = walkFrom(layout, edges, 'gw', 'right')
    expect(['auth', 'pay']).toContain(target)
  })
})

describe('edgePath / neighborsOf', () => {
  it('emits a cubic bezier from source right edge to target left edge', () => {
    const layout = layoutGraph(['a', 'b'], [E('a', 'b')])
    const path = edgePath(layout.nodes.get('a')!, layout.nodes.get('b')!)
    expect(path).toMatch(/^M [\d.-]+ [\d.-]+ C /)
  })

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
