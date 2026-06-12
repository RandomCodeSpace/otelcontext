// Triage ranking + anomaly-strip transforms. Pure functions shared by the
// Triage feed (/), the xs service card list on /map, and the Inspector
// header — one source of truth for "how bad is this service".

import type { AnomalyNode, AnomalySeverity, SystemNode } from '@/types/api'

export type ServiceStatus = 'critical' | 'degraded' | 'healthy' | 'unknown'

/** Normalize the backend's status strings ("failing" is legacy critical). */
export function nodeStatus(status: string | undefined): ServiceStatus {
  if (status === 'healthy' || status === 'degraded' || status === 'critical') return status
  if (status === 'failing') return 'critical'
  return 'unknown'
}

/** Status → token color. Saturation is reserved for status — nothing else. */
export function statusToken(status: ServiceStatus): string {
  switch (status) {
    case 'critical':
      return 'var(--crit)'
    case 'degraded':
      return 'var(--warn)'
    case 'healthy':
      return 'var(--ok)'
    default:
      return 'var(--unknown)'
  }
}

export interface RankedServices {
  critical: SystemNode[]
  degraded: SystemNode[]
  /** Healthy/unknown but carrying alerts — still triage-worthy. */
  alerted: SystemNode[]
  /** Everything quiet — collapsed by default in the feed. */
  healthy: SystemNode[]
}

const worstFirst = (a: SystemNode, b: SystemNode) =>
  a.health_score - b.health_score || (a.id < b.id ? -1 : 1)

/** Bucket and rank services critical → degraded → alerted → healthy. */
export function rankServices(nodes: readonly SystemNode[]): RankedServices {
  const ranked: RankedServices = { critical: [], degraded: [], alerted: [], healthy: [] }
  for (const n of nodes) {
    const status = nodeStatus(n.status)
    if (status === 'critical') ranked.critical.push(n)
    else if (status === 'degraded') ranked.degraded.push(n)
    else if (n.alerts.length > 0) ranked.alerted.push(n)
    else ranked.healthy.push(n)
  }
  ranked.critical.sort(worstFirst)
  ranked.degraded.sort(worstFirst)
  ranked.alerted.sort(worstFirst)
  ranked.healthy.sort(worstFirst)
  return ranked
}

export interface AnomalyTick {
  /** Position on the strip, 0 (window start) .. 1 (now). */
  x: number
  severity: AnomalySeverity
  service: string
  /** Anomalies collapsed into this tick. */
  count: number
  /** RFC3339 of the newest anomaly in the cluster. */
  timestamp: string
}

const SEVERITY_RANK: Record<AnomalySeverity, number> = {
  critical: 3,
  warning: 2,
  info: 1,
}

const BUCKET_MS = 30 * 60_000

export const ANOMALY_WINDOW_MS = 24 * 3_600_000

/**
 * Cluster anomalies into (service, 30-min bucket) ticks for the 24h strip.
 * The anomaly loop re-fires every ~10s, so raw points would smear into an
 * unreadable band; a cluster keeps the worst severity and the count.
 */
export function anomalyTicks(
  anomalies: readonly AnomalyNode[],
  nowMs: number,
  windowMs = ANOMALY_WINDOW_MS,
): AnomalyTick[] {
  const start = nowMs - windowMs
  const clusters = new Map<string, AnomalyTick & { tMs: number }>()
  for (const a of anomalies) {
    const tMs = Date.parse(a.timestamp)
    if (Number.isNaN(tMs) || tMs < start) continue
    const clamped = Math.min(tMs, nowMs)
    const key = `${a.service}|${Math.floor(clamped / BUCKET_MS)}`
    const existing = clusters.get(key)
    if (!existing) {
      clusters.set(key, {
        x: (clamped - start) / windowMs,
        severity: a.severity,
        service: a.service,
        count: 1,
        timestamp: a.timestamp,
        tMs: clamped,
      })
      continue
    }
    existing.count += 1
    if (SEVERITY_RANK[a.severity] > SEVERITY_RANK[existing.severity]) {
      existing.severity = a.severity
    }
    if (clamped > existing.tMs) {
      existing.tMs = clamped
      existing.timestamp = a.timestamp
      existing.x = (clamped - start) / windowMs
    }
  }
  return [...clusters.values()]
    .sort((a, b) => a.tMs - b.tMs)
    .map((c) => ({
      x: c.x,
      severity: c.severity,
      service: c.service,
      count: c.count,
      timestamp: c.timestamp,
    }))
}
