// View-model helpers for the /logs live tail: severity normalization for
// badge tones and filter pills, HH:mm:ss.SSS time formatting, per-tick
// buffer filtering/counting, and lazy attribute parsing (only invoked when
// a row expands — never on the render hot path). Pure functions.

import type { LogEntry } from '@/types/api'

export type Tone = 'ok' | 'warn' | 'crit' | 'unknown'

/** The normalized severities the filter pills offer (display order). */
export const SEVERITY_PILLS = ['ERROR', 'WARN', 'INFO', 'DEBUG'] as const
export type PillSeverity = (typeof SEVERITY_PILLS)[number]

/**
 * Normalize a free-form severity string (SeverityText or OTLP
 * SEVERITY_NUMBER_* fallback) onto one of the pill buckets.
 * Unknown severities land in DEBUG so every row stays countable.
 */
export function normalizeSeverity(severity: string): PillSeverity {
  const s = severity.toUpperCase()
  if (s.includes('FATAL') || s.includes('ERROR')) return 'ERROR'
  if (s.includes('WARN')) return 'WARN'
  if (s.includes('INFO')) return 'INFO'
  return 'DEBUG'
}

/** Badge tone for a severity: ERROR/FATAL → crit, WARN → warn, INFO → ok. */
export function severityTone(severity: string): Tone {
  switch (normalizeSeverity(severity)) {
    case 'ERROR':
      return 'crit'
    case 'WARN':
      return 'warn'
    case 'INFO':
      return 'ok'
    default:
      return 'unknown'
  }
}

/** Local-time HH:mm:ss.SSS for the tail's time gutter. */
export function formatLogTime(timestamp: string): string {
  const ms = Date.parse(timestamp)
  if (!Number.isFinite(ms)) return '—'
  const d = new Date(ms)
  const pad = (n: number, w = 2) => String(n).padStart(w, '0')
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`
}

/**
 * Filter the ring-buffer snapshot by one normalized severity (null = all).
 * Called once per flush tick, not per keystroke.
 */
export function filterBySeverity(
  logs: readonly LogEntry[],
  severity: PillSeverity | null,
): LogEntry[] {
  if (severity === null) return [...logs]
  return logs.filter((l) => normalizeSeverity(l.severity) === severity)
}

/** Live counts per normalized severity for the filter pills. */
export function severityCounts(
  logs: readonly LogEntry[],
): Record<PillSeverity, number> {
  const counts: Record<PillSeverity, number> = {
    ERROR: 0,
    WARN: 0,
    INFO: 0,
    DEBUG: 0,
  }
  for (const l of logs) counts[normalizeSeverity(l.severity)] += 1
  return counts
}

/**
 * "N new" pill count from the WsManager's monotonic appended total:
 * delta between now and the moment auto-follow paused.
 */
export function countNewSince(totalNow: number, totalAtPause: number): number {
  return Math.max(0, totalNow - totalAtPause)
}

/** A parsed attribute as a [key, displayValue] pair. */
export type AttributeEntry = [string, string]

function leafString(value: unknown): string {
  if (value === null || value === undefined) return ''
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') {
    return String(value)
  }
  return JSON.stringify(value)
}

/**
 * Lazily parse an attributes_json payload into displayable entries.
 * Two producer shapes exist:
 *  - a plain JSON object (map form), and
 *  - the OTLP []KeyValue array the ingester marshals verbatim
 *    (internal/ingest/otlp.go: json.Marshal(span.Attributes)).
 * Returns null when there is nothing displayable.
 */
export function parseAttributes(raw: string): AttributeEntry[] | null {
  if (!raw) return null
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return null
  }
  if (Array.isArray(parsed)) {
    const entries: AttributeEntry[] = []
    for (const item of parsed) {
      if (
        item !== null &&
        typeof item === 'object' &&
        typeof (item as { key?: unknown }).key === 'string'
      ) {
        const { key, value } = item as { key: string; value?: unknown }
        entries.push([key, leafString(value)])
      }
    }
    return entries.length > 0 ? entries : null
  }
  if (parsed !== null && typeof parsed === 'object') {
    const entries = Object.entries(parsed as Record<string, unknown>).map(
      ([k, v]): AttributeEntry => [k, leafString(v)],
    )
    return entries.length > 0 ? entries : null
  }
  return null
}
