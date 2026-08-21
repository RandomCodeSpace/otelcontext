import type { AccuracyMetadata } from '@/types/api'
import styles from './AccuracyLabel.module.css'

interface AccuracyLabelProps {
  value: number | string
  accuracy?: AccuracyMetadata
  className?: string
}

/**
 * Accuracy label: displays a value with relative_error_bound (e.g. "±2.2%")
 * and a degraded indicator when accuracy.degraded is true.
 * When accuracy is absent, renders just the value (legacy mode compatibility).
 */
export default function AccuracyLabel({
  value,
  accuracy,
  className,
}: Readonly<AccuracyLabelProps>) {
  if (!accuracy) {
    return <span className={className}>{value}</span>
  }

  const boundPercent = accuracy.relative_error_bound
    ? (accuracy.relative_error_bound * 100).toFixed(1)
    : null

  return (
    <span
      className={`${styles.container} ${accuracy.degraded ? styles.degraded : ''} ${className || ''}`}
    >
      <span>{value}</span>
      {boundPercent && (
        <span
          className={styles.bound}
          title={`Relative error bound: ±${boundPercent}%${accuracy.degraded ? ' (degraded estimate)' : ''}`}
        >
          ±{boundPercent}%
        </span>
      )}
      {accuracy.degraded && (
        <span
          className={styles.degradedIndicator}
          title="Estimate is outside the reported bound"
          aria-label="degraded"
        >
          ⚠
        </span>
      )}
    </span>
  )
}
