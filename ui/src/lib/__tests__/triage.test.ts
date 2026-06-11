import { describe, expect, it } from 'vitest'
import type { AnomalyNode, SystemNode } from '@/types/api'
import { anomalyTicks, nodeStatus, rankServices, statusToken } from '../triage'

function node(partial: Partial<SystemNode> & { id: string }): SystemNode {
  return {
    type: 'service',
    health_score: 1,
    status: 'healthy',
    metrics: {
      request_rate_rps: 0,
      error_rate: 0,
      avg_latency_ms: 0,
      p99_latency_ms: 0,
      span_count_1h: 0,
    },
    alerts: [],
    ...partial,
  }
}

describe('nodeStatus / statusToken', () => {
  it('normalizes backend status strings', () => {
    expect(nodeStatus('healthy')).toBe('healthy')
    expect(nodeStatus('degraded')).toBe('degraded')
    expect(nodeStatus('critical')).toBe('critical')
    expect(nodeStatus('failing')).toBe('critical')
    expect(nodeStatus('weird')).toBe('unknown')
    expect(nodeStatus(undefined)).toBe('unknown')
  })

  it('maps statuses onto status tokens only', () => {
    expect(statusToken('critical')).toBe('var(--crit)')
    expect(statusToken('degraded')).toBe('var(--warn)')
    expect(statusToken('healthy')).toBe('var(--ok)')
    expect(statusToken('unknown')).toBe('var(--unknown)')
  })
})

describe('rankServices', () => {
  it('buckets critical → degraded → healthy-with-alerts → healthy', () => {
    const ranked = rankServices([
      node({ id: 'ok1' }),
      node({ id: 'alerted', alerts: ['p99 high'] }),
      node({ id: 'bad', status: 'critical', health_score: 0.1 }),
      node({ id: 'meh', status: 'degraded', health_score: 0.6 }),
    ])
    expect(ranked.critical.map((n) => n.id)).toEqual(['bad'])
    expect(ranked.degraded.map((n) => n.id)).toEqual(['meh'])
    expect(ranked.alerted.map((n) => n.id)).toEqual(['alerted'])
    expect(ranked.healthy.map((n) => n.id)).toEqual(['ok1'])
  })

  it('sorts within a bucket worst-health first, ties by id', () => {
    const ranked = rankServices([
      node({ id: 'b', status: 'critical', health_score: 0.5 }),
      node({ id: 'a', status: 'critical', health_score: 0.5 }),
      node({ id: 'worst', status: 'critical', health_score: 0.05 }),
    ])
    expect(ranked.critical.map((n) => n.id)).toEqual(['worst', 'a', 'b'])
  })

  it('treats failing as critical and unknown-without-alerts as healthy', () => {
    const ranked = rankServices([
      node({ id: 'f', status: 'failing' }),
      node({ id: 'u', status: 'mystery' }),
    ])
    expect(ranked.critical.map((n) => n.id)).toEqual(['f'])
    expect(ranked.healthy.map((n) => n.id)).toEqual(['u'])
  })
})

describe('anomalyTicks', () => {
  const NOW = Date.parse('2026-06-11T12:00:00Z')
  const at = (hoursAgo: number) =>
    new Date(NOW - hoursAgo * 3_600_000).toISOString()
  const anomaly = (partial: Partial<AnomalyNode> & { id: string }): AnomalyNode => ({
    type: 'error_spike',
    severity: 'warning',
    service: 'svc',
    evidence: '',
    timestamp: at(1),
    ...partial,
  })

  it('positions ticks on the 0..1 axis by age', () => {
    const ticks = anomalyTicks([anomaly({ id: 'a', timestamp: at(12) })], NOW)
    expect(ticks).toHaveLength(1)
    expect(ticks[0].x).toBeCloseTo(0.5, 2)
  })

  it('drops anomalies outside the 24h window and clamps future ones', () => {
    const ticks = anomalyTicks(
      [
        anomaly({ id: 'old', timestamp: at(30) }),
        anomaly({ id: 'future', timestamp: at(-1) }),
      ],
      NOW,
    )
    expect(ticks).toHaveLength(1)
    expect(ticks[0].x).toBe(1)
  })

  it('clusters same service + 30min bucket, keeping the worst severity', () => {
    const ticks = anomalyTicks(
      [
        anomaly({ id: 'w', severity: 'warning', timestamp: at(2) }),
        anomaly({ id: 'c', severity: 'critical', timestamp: at(1.9) }),
        anomaly({ id: 'other', service: 'db', timestamp: at(2) }),
      ],
      NOW,
    )
    expect(ticks).toHaveLength(2)
    const svcTick = ticks.find((t) => t.service === 'svc')!
    expect(svcTick.severity).toBe('critical')
    expect(svcTick.count).toBe(2)
  })

  it('skips entries with invalid timestamps', () => {
    expect(anomalyTicks([anomaly({ id: 'bad', timestamp: 'nope' })], NOW)).toEqual([])
  })

  it('returns ticks ordered oldest first', () => {
    const ticks = anomalyTicks(
      [anomaly({ id: 'new', timestamp: at(1) }), anomaly({ id: 'old', service: 'db', timestamp: at(20) })],
      NOW,
    )
    expect(ticks[0].service).toBe('db')
  })
})
