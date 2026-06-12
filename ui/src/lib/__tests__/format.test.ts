import { describe, expect, it } from 'vitest'
import {
  formatCount,
  formatMb,
  formatMs,
  formatPercent,
} from '../format'

describe('formatPercent', () => {
  // The two audit bugs this module exists to fix, verbatim:
  it('renders a 0–1 error-rate ratio as a percentage (audit bug #1)', () => {
    expect(formatPercent(0.042)).toBe('4.2%')
  })

  it('renders a 0–1 health score without a stray decimal (audit bug #2)', () => {
    expect(formatPercent(0.73)).toBe('73%')
  })

  it('trims trailing zeros but keeps meaningful decimals', () => {
    expect(formatPercent(1)).toBe('100%')
    expect(formatPercent(0)).toBe('0%')
    expect(formatPercent(0.005)).toBe('0.5%')
    expect(formatPercent(0.9999, 1)).toBe('100%')
  })

  it('respects an explicit digits argument', () => {
    expect(formatPercent(0.04217, 2)).toBe('4.22%')
    expect(formatPercent(0.73, 0)).toBe('73%')
  })

  it('treats values already scaled to percent via unit "percent"', () => {
    // DashboardStats.error_rate arrives pre-multiplied by 100 server-side.
    expect(formatPercent(4.2, 1, 'percent')).toBe('4.2%')
    expect(formatPercent(0.5, 1, 'percent')).toBe('0.5%')
  })

  it('unit "auto" applies the codebase ≤1 heuristic for loose fields', () => {
    expect(formatPercent(0.042, 1, 'auto')).toBe('4.2%')
    expect(formatPercent(4.2, 1, 'auto')).toBe('4.2%')
  })

  it('returns the em dash for non-finite input', () => {
    expect(formatPercent(Number.NaN)).toBe('—')
    expect(formatPercent(Number.POSITIVE_INFINITY)).toBe('—')
  })
})

describe('formatMs', () => {
  it('keeps sub-second values in milliseconds', () => {
    expect(formatMs(230)).toBe('230ms')
    expect(formatMs(0)).toBe('0ms')
    expect(formatMs(0.4)).toBe('0.4ms')
  })

  it('rolls up to seconds and minutes', () => {
    expect(formatMs(1500)).toBe('1.5s')
    expect(formatMs(60_000)).toBe('1m')
    expect(formatMs(90_000)).toBe('1.5m')
  })

  it('returns the em dash for non-finite input', () => {
    expect(formatMs(Number.NaN)).toBe('—')
  })
})

describe('formatCount', () => {
  it('abbreviates thousands and millions', () => {
    expect(formatCount(950)).toBe('950')
    expect(formatCount(1_200)).toBe('1.2K')
    expect(formatCount(3_400_000)).toBe('3.4M')
  })

  it('trims trailing zeros on round thousands', () => {
    expect(formatCount(2_000)).toBe('2K')
  })

  it('returns the em dash for non-finite input', () => {
    expect(formatCount(Number.NaN)).toBe('—')
  })
})

describe('formatMb', () => {
  it('keeps small sizes in MB and rolls up to GB', () => {
    expect(formatMb(840)).toBe('840MB')
    expect(formatMb(1_228.8)).toBe('1.2GB')
  })

  it('shows one decimal under 10MB', () => {
    expect(formatMb(3.4)).toBe('3.4MB')
  })

  it('returns the em dash for non-finite input', () => {
    expect(formatMb(Number.NaN)).toBe('—')
    expect(formatMb(undefined)).toBe('—')
  })
})
