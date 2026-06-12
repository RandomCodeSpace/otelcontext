// View-model helpers for the /traces route: visible-set percentile math
// for the inline duration bars, OTLP status → badge tone mapping, and the
// URL-param round-trip for filter state. Pure functions, unit-tested.

import type { Trace } from '@/types/api'

export type Tone = 'ok' | 'warn' | 'crit' | 'unknown'

/**
 * Nearest-rank percentile over an unsorted list. Returns 0 for an empty
 * list. Does not mutate the input.
 */
export function percentile(values: readonly number[], p: number): number {
  if (values.length === 0) return 0
  const sorted = [...values].sort((a, b) => a - b)
  const rank = Math.min(
    sorted.length - 1,
    Math.max(0, Math.ceil(p * sorted.length) - 1),
  )
  return sorted[rank]
}

/** p99 of duration_ms across the currently loaded (visible) trace rows. */
export function visibleP99Ms(traces: readonly Trace[]): number {
  return percentile(
    traces.map((t) => t.duration_ms),
    0.99,
  )
}

/** Inline duration-bar width as a 0..1 fraction of the visible-set p99. */
export function durationBarFrac(durationMs: number, p99Ms: number): number {
  if (!Number.isFinite(p99Ms) || p99Ms <= 0) return 0
  if (!Number.isFinite(durationMs) || durationMs <= 0) return 0
  return Math.min(1, durationMs / p99Ms)
}

/** Map an OTLP status code string onto a status tone token. */
export function statusTone(status: string): Tone {
  const s = status.toUpperCase()
  if (s.includes('ERROR')) return 'crit'
  if (s.includes('OK')) return 'ok'
  return 'unknown'
}

/** Short human label for an OTLP status code: STATUS_CODE_ERROR → ERROR. */
export function statusLabel(status: string): string {
  const s = status.toUpperCase().replace(/^STATUS_CODE_/, '')
  return s === '' ? 'UNSET' : s
}

// ---- URL filter state ------------------------------------------------------

/** Filter state carried in the /traces query string. */
export interface TraceFilters {
  /** service_name= API param */
  service?: string
  /** status= API param (LIKE contains match server-side) */
  status?: string
  /** search= API param (trace_id contains match) */
  q?: string
  /** selected trace id — drives the detail pane */
  trace?: string
}

const FILTER_KEYS = ['service', 'status', 'q', 'trace'] as const

/** Parse a location search string ("?a=b" or "a=b") into trace filters. */
export function parseTraceFilters(search: string): TraceFilters {
  const params = new URLSearchParams(
    search.startsWith('?') ? search.slice(1) : search,
  )
  const filters: TraceFilters = {}
  for (const key of FILTER_KEYS) {
    const value = params.get(key)
    if (value) filters[key] = value
  }
  return filters
}

/** Serialize filters to a query string (no leading "?"; stable key order). */
export function traceFiltersToSearch(filters: TraceFilters): string {
  const params = new URLSearchParams()
  for (const key of FILTER_KEYS) {
    const value = filters[key]
    if (value) params.set(key, value)
  }
  return params.toString()
}
