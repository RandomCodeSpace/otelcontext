import styles from './RouteFallback.module.css'

/**
 * Suspense fallback for lazy route chunks: a small token-CSS spinner.
 * The rotation keyframe carries its own reduced-motion guard in the
 * module CSS (the global block only zeroes --dur-* transition tokens).
 */
export default function RouteFallback() {
  return (
    <div className={styles.wrap} role="status" aria-live="polite">
      <span className={styles.spinner} aria-hidden="true" />
      Loading…
    </div>
  )
}
