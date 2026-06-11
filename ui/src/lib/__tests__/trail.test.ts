import { describe, expect, it } from 'vitest'
import {
  MAX_TRAIL_FRAMES,
  parseTrail,
  popTo,
  pushFrame,
  serializeTrail,
  topService,
  type TrailFrame,
} from '../trail'

const svc = (id: string): TrailFrame => ({ kind: 'svc', id })
const trace = (id: string): TrailFrame => ({ kind: 'trace', id })

describe('parseTrail', () => {
  it('parses the documented ?trail= format', () => {
    expect(parseTrail('svc:checkout,svc:payments,trace:a1b2')).toEqual([
      svc('checkout'),
      svc('payments'),
      trace('a1b2'),
    ])
  })

  it('returns [] for null, empty and whitespace input', () => {
    expect(parseTrail(null)).toEqual([])
    expect(parseTrail('')).toEqual([])
    expect(parseTrail('  ')).toEqual([])
  })

  it('drops malformed entries but keeps valid ones', () => {
    expect(parseTrail('svc:a,bogus,:x,svc:,unknown:y,trace:t1')).toEqual([
      svc('a'),
      trace('t1'),
    ])
  })

  it('decodes percent-encoded ids (URL round-trip safety)', () => {
    expect(parseTrail('svc:a%2Cb')).toEqual([svc('a,b')])
  })

  it('survives malformed percent-encoding without throwing', () => {
    expect(parseTrail('svc:%E0%A4%A')).toEqual([])
  })
})

describe('serializeTrail', () => {
  it('round-trips through parseTrail', () => {
    const frames = [svc('checkout'), svc('pay,ments'), trace('a1b2')]
    expect(parseTrail(serializeTrail(frames))).toEqual(frames)
  })

  it('returns empty string for an empty trail', () => {
    expect(serializeTrail([])).toBe('')
  })
})

describe('pushFrame', () => {
  it('appends a new frame', () => {
    expect(pushFrame([svc('a')], svc('b'))).toEqual([svc('a'), svc('b')])
  })

  it('is a no-op when the top frame is identical', () => {
    const frames = [svc('a'), svc('b')]
    expect(pushFrame(frames, svc('b'))).toBe(frames)
  })

  it('allows re-visiting an earlier (non-top) frame', () => {
    expect(pushFrame([svc('a'), svc('b')], svc('a'))).toEqual([
      svc('a'),
      svc('b'),
      svc('a'),
    ])
  })

  it('caps the stack by dropping the oldest frame', () => {
    let frames: readonly TrailFrame[] = []
    for (let i = 0; i < MAX_TRAIL_FRAMES + 3; i++) {
      frames = pushFrame(frames, svc(`s${i}`))
    }
    expect(frames).toHaveLength(MAX_TRAIL_FRAMES)
    expect(frames[0]).toEqual(svc('s3'))
    expect(frames.at(-1)).toEqual(svc(`s${MAX_TRAIL_FRAMES + 2}`))
  })
})

describe('popTo', () => {
  const frames = [svc('a'), svc('b'), trace('t')]

  it('truncates the stack to the tapped frame', () => {
    expect(popTo(frames, 1)).toEqual([svc('a'), svc('b')])
  })

  it('returns the same array when tapping the top frame', () => {
    expect(popTo(frames, 2)).toBe(frames)
  })

  it('clamps out-of-range indices', () => {
    expect(popTo(frames, -1)).toEqual([svc('a')])
    expect(popTo(frames, 99)).toBe(frames)
  })
})

describe('topService', () => {
  it('returns the id of the top svc frame', () => {
    expect(topService([svc('a'), svc('b')])).toBe('b')
  })

  it('returns null when the top frame is not a service', () => {
    expect(topService([svc('a'), trace('t')])).toBeNull()
  })

  it('returns null for an empty trail', () => {
    expect(topService([])).toBeNull()
  })
})
