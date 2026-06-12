// Central formatters for every numeric surface in the UI. All output is
// tabular-nums-friendly: plain ASCII digits, one unit suffix, no locale
// grouping — so columns of values line up in the mono/tabular font stacks.
//
// This module is the single fix point for the two audit formatting bugs:
//   - error_rate 0.042 (a 0–1 ratio)  must render "4.2%", not "0.04%"
//   - health_score 0.73 (a 0–1 ratio) must render "73%",  not "0.73%"

/** Placeholder for absent/invalid numbers. */
const DASH = '—'

/**
 * Unit of the raw value passed to {@link formatPercent}:
 *  - `ratio`   — 0–1 (graph node `error_rate`, `health_score`). The default.
 *  - `percent` — already ×100 server-side (`DashboardStats.error_rate`).
 *  - `auto`    — loose fields with inconsistent producers: ≤1 is treated as a
 *    ratio, >1 as a percent (the long-standing codebase heuristic).
 */
export type PercentUnit = 'ratio' | 'percent' | 'auto'

/** toFixed + trailing-zero trim: 73.0 → "73", 4.20 → "4.2". */
function trimFixed(value: number, digits: number): string {
  const s = value.toFixed(digits)
  if (!s.includes('.')) return s
  // Two anchored single-quantifier passes -- linear, no backtracking.
  return s.replace(/0+$/, '').replace(/\.$/, '')
}

/**
 * Format a percentage. `formatPercent(0.042)` → "4.2%",
 * `formatPercent(0.73)` → "73%".
 */
export function formatPercent(
  value: number,
  digits = 1,
  unit: PercentUnit = 'ratio',
): string {
  if (!Number.isFinite(value)) return DASH
  let pct: number
  if (unit === 'percent') {
    pct = value
  } else if (unit === 'auto') {
    pct = value <= 1 ? value * 100 : value
  } else {
    pct = value * 100
  }
  return `${trimFixed(pct, digits)}%`
}

/**
 * Format a duration given in milliseconds: "230ms", "1.5s", "1.5m".
 * Sub-millisecond values keep one decimal ("0.4ms").
 */
export function formatMs(ms: number): string {
  if (!Number.isFinite(ms)) return DASH
  if (ms < 1) return `${trimFixed(ms, 1)}ms`
  if (ms < 1000) return `${trimFixed(ms, 0)}ms`
  if (ms < 60_000) return `${trimFixed(ms / 1000, 1)}s`
  return `${trimFixed(ms / 60_000, 1)}m`
}

/** Abbreviate a count: 950 → "950", 1200 → "1.2K", 3400000 → "3.4M". */
export function formatCount(n: number): string {
  if (!Number.isFinite(n)) return DASH
  if (Math.abs(n) >= 1_000_000) return `${trimFixed(n / 1_000_000, 1)}M`
  if (Math.abs(n) >= 1_000) return `${trimFixed(n / 1_000, 1)}K`
  return trimFixed(n, 0)
}

/** Format a size given in megabytes: "3.4MB", "840MB", "1.2GB". */
export function formatMb(mb: number | undefined | null): string {
  if (typeof mb !== 'number' || !Number.isFinite(mb)) return DASH
  if (mb >= 1024) return `${trimFixed(mb / 1024, 1)}GB`
  if (mb < 10) return `${trimFixed(mb, 1)}MB`
  return `${trimFixed(mb, 0)}MB`
}
