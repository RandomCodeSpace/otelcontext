import type { CoverageType } from '@/types/api'
import styles from './CoverageBadge.module.css'

interface CoverageBadgeProps {
  coverage?: CoverageType
  coverage_note?: string
  className?: string
}

/**
 * Coverage badge: renders full/sampled/exemplar states with coverage_note as tooltip.
 * When coverage is absent, renders nothing (legacy mode compatibility).
 */
export default function CoverageBadge({
  coverage,
  coverage_note,
  className,
}: Readonly<CoverageBadgeProps>) {
  if (!coverage) return null

  const tone: 'full' | 'sampled' | 'exemplar' = coverage

  return (
    <span
      className={`${styles.badge} ${styles[tone]} ${className || ''}`}
      title={coverage_note || `Data coverage: ${coverage}`}
      role="img"
      aria-label={`${coverage}${coverage_note ? `: ${coverage_note}` : ''}`}
    >
      {coverage.charAt(0).toUpperCase() + coverage.slice(1)}
    </span>
  )
}
