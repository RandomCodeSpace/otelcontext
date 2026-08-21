import type { CoverageType } from '@/types/api'

/**
 * Determines if exemplar data should be rendered as "not retained" rather than "0 events".
 * Missing exemplars are absence of raw exemplars with coverage=exemplar, not zero events.
 */
export function isExemplarNotRetained(
  exemplarCount: number | undefined | null,
  coverage?: CoverageType,
): boolean {
  // If coverage explicitly says exemplar, but count is 0 or missing,
  // it means exemplars are not retained
  if (coverage === 'exemplar' && (!exemplarCount || exemplarCount === 0)) {
    return true
  }
  return false
}

/**
 * Format exemplar count for display, respecting "not retained" semantics.
 */
export function formatExemplarCount(
  count: number | undefined | null,
  coverage?: CoverageType,
): string {
  if (isExemplarNotRetained(count, coverage)) {
    return 'not retained'
  }
  return `${count || 0} events`
}
