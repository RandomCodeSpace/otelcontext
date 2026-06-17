import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import HealthCore, { statusSentence, worstTier } from '../HealthCore'
import type { SystemSummary } from '@/types/api'

function summary(overrides: Partial<SystemSummary> = {}): SystemSummary {
  return {
    total_services: 12,
    healthy: 9,
    degraded: 2,
    critical: 1,
    overall_health_score: 0.94,
    total_error_rate: 0.042,
    avg_latency_ms: 18,
    uptime_seconds: 532297, // 6d 03:51:37
    ...overrides,
  }
}

describe('worstTier', () => {
  it('critical wins over degraded and healthy', () => {
    expect(worstTier(summary({ critical: 1, degraded: 3 }))).toBe('crit')
  })
  it('degraded when no critical', () => {
    expect(worstTier(summary({ critical: 0, degraded: 2 }))).toBe('warn')
  })
  it('ok when all quiet', () => {
    expect(worstTier(summary({ critical: 0, degraded: 0 }))).toBe('ok')
  })
})

describe('statusSentence', () => {
  it('reports the failing count when critical (singular/plural)', () => {
    expect(statusSentence(summary({ critical: 1, degraded: 0 }))).toMatch(
      /System critical: 1 service failing/,
    )
    expect(statusSentence(summary({ critical: 3, degraded: 2 }))).toMatch(
      /System critical: 3 services failing/,
    )
  })
  it('reports degraded when no critical', () => {
    expect(statusSentence(summary({ critical: 0, degraded: 1 }))).toMatch(
      /System degraded: 1 service unhealthy/,
    )
  })
  it('reports healthy when all quiet', () => {
    expect(statusSentence(summary({ critical: 0, degraded: 0 }))).toMatch(/System healthy/)
  })
  it('ignores health %, error rate and uptime churn (tier/counts only)', () => {
    const base = summary({ critical: 1, degraded: 0 })
    expect(statusSentence(base)).toBe(
      statusSentence({
        ...base,
        overall_health_score: 0.1,
        total_error_rate: 0.9,
        uptime_seconds: 999_999,
      }),
    )
  })
})

describe('HealthCore', () => {
  it('exposes the worst-tier status in a polite live region', () => {
    const { container } = render(<HealthCore summary={summary()} p99Ms={230} />)
    const live = container.querySelector('[aria-live="polite"]')
    expect(live).not.toBeNull()
    expect(live).toHaveTextContent(/System critical: 1 service failing/)
  })

  it('renders the central HEALTH gauge with the authoritative numeral', () => {
    render(<HealthCore summary={summary()} p99Ms={230} />)
    const health = screen.getByRole('meter', { name: /HEALTH 94%/ })
    expect(health).toHaveAttribute('aria-valuenow', '0.94')
    expect(health).toHaveAttribute('aria-valuemax', '1')
  })

  it('tints the central gauge by the worst active tier (critical present)', () => {
    render(<HealthCore summary={summary()} p99Ms={230} />)
    const health = screen.getByRole('meter', { name: /HEALTH/ })
    const fill = health.querySelector('path + path')
    expect(fill?.getAttribute('class')).toMatch(/crit/)
  })

  it('renders ERR, P99 and SVCS satellite readings', () => {
    render(<HealthCore summary={summary()} p99Ms={230} />)
    expect(screen.getByRole('meter', { name: /ERR 4\.2%/ })).toBeInTheDocument()
    expect(screen.getByRole('meter', { name: /P99 230ms/ })).toBeInTheDocument()
    expect(screen.getByRole('meter', { name: /SVCS 9\/12/ })).toBeInTheDocument()
  })

  it('falls back to a numeric-only P99 when latency is unavailable', () => {
    render(<HealthCore summary={summary()} p99Ms={null} />)
    expect(screen.queryByRole('meter', { name: /P99/ })).toBeNull()
    expect(screen.getByRole('group', { name: /P99 —/ })).toBeInTheDocument()
  })

  it('feeds the uptime odometer from uptime_seconds', () => {
    render(<HealthCore summary={summary()} p99Ms={230} />)
    const up = screen.getByRole('group', { name: /process uptime/i })
    expect(up.textContent).toMatch(/6d 03:51:37/)
  })
})
