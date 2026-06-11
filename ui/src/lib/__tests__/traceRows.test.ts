import { describe, expect, it } from 'vitest'
import {
  durationBarFrac,
  parseTraceFilters,
  percentile,
  statusTone,
  traceFiltersToSearch,
  visibleP99Ms,
  type TraceFilters,
} from '../traceRows'
import type { Trace } from '@/types/api'

function trace(over: Partial<Trace> = {}): Trace {
  return {
    id: 1,
    trace_id: 'abc123',
    service_name: 'svc-a',
    duration: 1_000_000,
    duration_ms: 1000,
    span_count: 3,
    operation: 'GET /x',
    status: 'STATUS_CODE_UNSET',
    timestamp: '2026-06-11T10:00:00Z',
    ...over,
  }
}

describe('percentile', () => {
  it('returns 0 for an empty list', () => {
    expect(percentile([], 0.99)).toBe(0)
  })

  it('returns the single value for a one-element list', () => {
    expect(percentile([42], 0.99)).toBe(42)
  })

  it('computes p99 via nearest-rank on a sorted copy', () => {
    const values = Array.from({ length: 100 }, (_, i) => i + 1) // 1..100
    expect(percentile(values, 0.99)).toBe(99)
    expect(percentile(values, 0.5)).toBe(50)
  })

  it('does not mutate its input', () => {
    const values = [3, 1, 2]
    percentile(values, 0.5)
    expect(values).toEqual([3, 1, 2])
  })
})

describe('visibleP99Ms', () => {
  it('returns 0 when no traces are loaded', () => {
    expect(visibleP99Ms([])).toBe(0)
  })

  it('computes p99 of duration_ms over the visible set', () => {
    const traces = [100, 200, 300, 400].map((ms) =>
      trace({ duration_ms: ms }),
    )
    expect(visibleP99Ms(traces)).toBe(400)
  })
})

describe('durationBarFrac', () => {
  it('scales relative to p99 and clamps to 1', () => {
    expect(durationBarFrac(50, 200)).toBeCloseTo(0.25, 5)
    expect(durationBarFrac(400, 200)).toBe(1)
  })

  it('returns 0 for a zero/invalid p99 (no division blowup)', () => {
    expect(durationBarFrac(50, 0)).toBe(0)
    expect(durationBarFrac(50, Number.NaN)).toBe(0)
  })

  it('floors negative/invalid durations to 0', () => {
    expect(durationBarFrac(-5, 200)).toBe(0)
    expect(durationBarFrac(Number.NaN, 200)).toBe(0)
  })
})

describe('statusTone', () => {
  it('maps OTLP status codes to tones', () => {
    expect(statusTone('STATUS_CODE_ERROR')).toBe('crit')
    expect(statusTone('STATUS_CODE_OK')).toBe('ok')
    expect(statusTone('STATUS_CODE_UNSET')).toBe('unknown')
  })

  it('is case-insensitive and defaults to unknown', () => {
    expect(statusTone('status_code_error')).toBe('crit')
    expect(statusTone('weird')).toBe('unknown')
    expect(statusTone('')).toBe('unknown')
  })
})

describe('trace filter URL round-trip', () => {
  it('parses an empty search string to empty filters', () => {
    expect(parseTraceFilters('')).toEqual({})
  })

  it('parses service, status, q and trace params', () => {
    const filters = parseTraceFilters(
      'service=checkout&status=ERROR&q=abc&trace=t1',
    )
    expect(filters).toEqual({
      service: 'checkout',
      status: 'ERROR',
      q: 'abc',
      trace: 't1',
    })
  })

  it('ignores unknown params and empty values', () => {
    expect(parseTraceFilters('foo=bar&service=')).toEqual({})
  })

  it('accepts a leading question mark', () => {
    expect(parseTraceFilters('?service=a')).toEqual({ service: 'a' })
  })

  it('serializes back to a stable query string', () => {
    const filters: TraceFilters = {
      service: 'checkout',
      status: 'ERROR',
      q: 'abc',
      trace: 't1',
    }
    expect(parseTraceFilters(traceFiltersToSearch(filters))).toEqual(filters)
  })

  it('serializes empty filters to an empty string', () => {
    expect(traceFiltersToSearch({})).toBe('')
  })

  it('URL-encodes reserved characters in values', () => {
    const filters: TraceFilters = { q: 'a b&c=d' }
    expect(parseTraceFilters(traceFiltersToSearch(filters))).toEqual(filters)
  })
})
