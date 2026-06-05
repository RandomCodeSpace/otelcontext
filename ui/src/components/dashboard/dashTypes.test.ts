import { describe, expect, it } from 'vitest'
import type { TrafficPoint } from '../../types/api'
import {
  bucketErrorRatePct,
  coerceDbSizeMb,
  healthPct,
  readyCheckToStatus,
  severityToTone,
} from './dashTypes'

describe('severityToTone', () => {
  it('maps critical -> danger, warning -> warning, info -> neutral', () => {
    expect(severityToTone('critical')).toBe('danger')
    expect(severityToTone('warning')).toBe('warning')
    expect(severityToTone('info')).toBe('neutral')
  })
})

describe('readyCheckToStatus', () => {
  it('maps /ready check strings to StatusDot statuses', () => {
    expect(readyCheckToStatus('ok')).toBe('running')
    expect(readyCheckToStatus('skipped')).toBe('idle')
    expect(readyCheckToStatus('saturated 96%')).toBe('degraded')
    expect(readyCheckToStatus('not running')).toBe('failed')
    expect(readyCheckToStatus('ping failed: timeout')).toBe('failed')
    expect(readyCheckToStatus(undefined)).toBe('idle')
  })
})

describe('bucketErrorRatePct', () => {
  it('returns percentage and guards divide-by-zero', () => {
    const p = (count: number, error_count: number): TrafficPoint => ({
      timestamp: '2026-06-05T00:00:00Z',
      count,
      error_count,
    })
    expect(bucketErrorRatePct(p(100, 5))).toBe(5)
    expect(bucketErrorRatePct(p(0, 0))).toBe(0)
  })
})

describe('healthPct', () => {
  it('normalises 0-1 and 0-100 inputs and clamps', () => {
    expect(healthPct(0.92)).toBeCloseTo(92)
    expect(healthPct(87)).toBe(87)
    expect(healthPct(1)).toBe(100)
    expect(healthPct(150)).toBe(100)
    expect(healthPct(-5)).toBe(0)
    expect(healthPct(undefined)).toBe(0)
    expect(healthPct(Number.NaN)).toBe(0)
  })
})

describe('coerceDbSizeMb', () => {
  it('coerces number / numeric string / absent values', () => {
    expect(coerceDbSizeMb(42.5)).toBe(42.5)
    expect(coerceDbSizeMb('128.0')).toBe(128)
    expect(coerceDbSizeMb(undefined)).toBeNull()
    expect(coerceDbSizeMb('not-a-number')).toBeNull()
    expect(coerceDbSizeMb(Number.NaN)).toBeNull()
  })
})

describe('dashboard hooks are importable', () => {
  it('exposes useTraffic / useReady / useAnomalies as functions', async () => {
    const traffic = await import('../../hooks/useTraffic')
    const ready = await import('../../hooks/useReady')
    const anomalies = await import('../../hooks/useAnomalies')
    expect(traffic.useTraffic).toBeTypeOf('function')
    expect(ready.useReady).toBeTypeOf('function')
    expect(anomalies.useAnomalies).toBeTypeOf('function')
  })
})
