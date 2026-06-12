import { describe, expect, it } from 'vitest'
import { buildWaterfall, serviceHue } from '../waterfall'
import type { Span } from '@/types/api'

// Full wire-shape span factory (types/api.ts Span structurally satisfies
// the lib's WaterfallSpan contract). Times are RFC3339; duration is
// microseconds — exactly what GET /api/traces/{id} returns.
let autoId = 0
function span(over: Partial<Span> = {}): Span {
  autoId += 1
  return {
    id: autoId,
    trace_id: 't1',
    span_id: `s${autoId}`,
    parent_span_id: '',
    operation_name: `op-${autoId}`,
    start_time: '2026-06-11T10:00:00.000Z',
    end_time: '2026-06-11T10:00:01.000Z',
    duration: 1_000_000, // 1s in µs
    service_name: 'svc-a',
    attributes_json: '',
    status: 'STATUS_CODE_UNSET',
    ...over,
  }
}

describe('buildWaterfall', () => {
  it('returns an empty layout for no spans', () => {
    const layout = buildWaterfall([])
    expect(layout.rows).toEqual([])
    expect(layout.totalMs).toBe(0)
  })

  it('positions a single root span at offset 0 with full width', () => {
    const layout = buildWaterfall([span({ span_id: 'root' })])
    expect(layout.rows).toHaveLength(1)
    const row = layout.rows[0]
    expect(row.depth).toBe(0)
    expect(row.offsetFrac).toBe(0)
    expect(row.widthFrac).toBeCloseTo(1, 5)
    expect(layout.totalMs).toBeCloseTo(1000, 3)
  })

  it('computes offset fractions relative to the trace start', () => {
    const layout = buildWaterfall([
      span({
        span_id: 'root',
        start_time: '2026-06-11T10:00:00.000Z',
        duration: 2_000_000, // 2s
      }),
      span({
        span_id: 'child',
        parent_span_id: 'root',
        start_time: '2026-06-11T10:00:01.000Z', // +1s
        duration: 500_000, // 0.5s
      }),
    ])
    expect(layout.totalMs).toBeCloseTo(2000, 3)
    const child = layout.rows.find((r) => r.span.span_id === 'child')!
    expect(child.offsetFrac).toBeCloseTo(0.5, 5)
    expect(child.widthFrac).toBeCloseTo(0.25, 5)
    expect(child.depth).toBe(1)
  })

  it('extends totalMs when a child outlives the root span', () => {
    const layout = buildWaterfall([
      span({ span_id: 'root', duration: 1_000_000 }),
      span({
        span_id: 'late',
        parent_span_id: 'root',
        start_time: '2026-06-11T10:00:00.500Z',
        duration: 2_000_000, // ends at +2.5s
      }),
    ])
    expect(layout.totalMs).toBeCloseTo(2500, 3)
  })

  it('orders rows depth-first with children sorted by start time', () => {
    const layout = buildWaterfall([
      span({ span_id: 'root', duration: 4_000_000 }),
      span({
        span_id: 'b',
        parent_span_id: 'root',
        start_time: '2026-06-11T10:00:02.000Z',
      }),
      span({
        span_id: 'a',
        parent_span_id: 'root',
        start_time: '2026-06-11T10:00:01.000Z',
      }),
      span({
        span_id: 'a1',
        parent_span_id: 'a',
        start_time: '2026-06-11T10:00:01.200Z',
      }),
    ])
    expect(layout.rows.map((r) => r.span.span_id)).toEqual([
      'root',
      'a',
      'a1',
      'b',
    ])
    expect(layout.rows.map((r) => r.depth)).toEqual([0, 1, 2, 1])
  })

  it('treats spans with unknown parents as roots (orphans)', () => {
    const layout = buildWaterfall([
      span({ span_id: 'root' }),
      span({
        span_id: 'orphan',
        parent_span_id: 'missing',
        start_time: '2026-06-11T10:00:00.300Z',
      }),
    ])
    expect(layout.rows).toHaveLength(2)
    const orphan = layout.rows.find((r) => r.span.span_id === 'orphan')!
    expect(orphan.depth).toBe(0)
  })

  it('survives parent cycles without infinite recursion', () => {
    const layout = buildWaterfall([
      span({ span_id: 'x', parent_span_id: 'y' }),
      span({
        span_id: 'y',
        parent_span_id: 'x',
        start_time: '2026-06-11T10:00:00.100Z',
      }),
    ])
    // Both spans appear exactly once.
    expect(layout.rows.map((r) => r.span.span_id).sort((a, b) => a.localeCompare(b))).toEqual(['x', 'y'])
  })

  it('is deterministic for identical start times (span_id tiebreak)', () => {
    const mk = () => [
      span({ span_id: 'root', duration: 2_000_000 }),
      span({ span_id: 'z', parent_span_id: 'root' }),
      span({ span_id: 'a', parent_span_id: 'root' }),
    ]
    const first = buildWaterfall(mk()).rows.map((r) => r.span.span_id)
    const second = buildWaterfall(mk()).rows.map((r) => r.span.span_id)
    expect(first).toEqual(second)
    expect(first).toEqual(['root', 'a', 'z'])
  })

  it('flags error spans', () => {
    const layout = buildWaterfall([
      span({ span_id: 'ok' }),
      span({
        span_id: 'bad',
        parent_span_id: 'ok',
        status: 'STATUS_CODE_ERROR',
      }),
    ])
    expect(layout.rows.find((r) => r.span.span_id === 'ok')!.isError).toBe(
      false,
    )
    expect(layout.rows.find((r) => r.span.span_id === 'bad')!.isError).toBe(
      true,
    )
  })

  it('clamps malformed timestamps and zero durations to a renderable layout', () => {
    const layout = buildWaterfall([
      span({ span_id: 'root', start_time: 'not-a-date', duration: 0 }),
    ])
    expect(layout.rows).toHaveLength(1)
    expect(layout.rows[0].offsetFrac).toBe(0)
    expect(layout.rows[0].widthFrac).toBeGreaterThanOrEqual(0)
    expect(Number.isFinite(layout.totalMs)).toBe(true)
  })

  it('clamps offset+width into the [0,1] track', () => {
    const layout = buildWaterfall([
      span({ span_id: 'root', duration: 1_000_000 }),
      span({
        span_id: 'tail',
        parent_span_id: 'root',
        // starts inside but reported duration overshoots wildly relative
        // to a separately-derived total — width must stay inside track
        start_time: '2026-06-11T10:00:00.900Z',
        duration: 100_000,
      }),
    ])
    for (const row of layout.rows) {
      expect(row.offsetFrac).toBeGreaterThanOrEqual(0)
      expect(row.offsetFrac).toBeLessThanOrEqual(1)
      expect(row.offsetFrac + row.widthFrac).toBeLessThanOrEqual(1.000001)
    }
  })
})

describe('serviceHue', () => {
  it('is deterministic per service name', () => {
    expect(serviceHue('payments')).toBe(serviceHue('payments'))
  })

  it('stays within 0..359', () => {
    for (const name of ['a', 'checkout', 'payments', 'shipping', '']) {
      const hue = serviceHue(name)
      expect(hue).toBeGreaterThanOrEqual(0)
      expect(hue).toBeLessThan(360)
    }
  })

  it('spreads distinct names across hues', () => {
    expect(serviceHue('payments')).not.toBe(serviceHue('checkout'))
  })
})
