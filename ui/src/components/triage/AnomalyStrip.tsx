import { useAnomalyTimeline } from '@/hooks/useAnomalyTimeline'
import { anomalyTicks, type AnomalyTick } from '@/lib/triage'
import type { AnomalySeverity } from '@/types/api'
import styles from './AnomalyStrip.module.css'

// 24h anomaly timeline strip — severity-colored ticks on a horizontal axis;
// tap a tick to open the Inspector for that service.

const SEVERITY_COLOR: Record<AnomalySeverity, string> = {
  critical: 'var(--crit)',
  warning: 'var(--warn)',
  info: 'var(--unknown)',
}

function tickLabel(tick: AnomalyTick): string {
  const time = new Date(tick.timestamp).toLocaleTimeString()
  const noun = tick.count === 1 ? 'anomaly' : 'anomalies'
  return `${tick.service}: ${tick.count} ${tick.severity} ${noun}, latest ${time}`
}

interface AnomalyStripProps {
  onOpenService: (id: string) => void
}

export default function AnomalyStrip({ onOpenService }: Readonly<AnomalyStripProps>) {
  const { data, isPending, error, refetch } = useAnomalyTimeline()

  let body
  if (isPending) {
    body = <div className={styles.skeleton} data-testid="strip-skeleton" aria-hidden="true" />
  } else if (error) {
    body = (
      <p className={styles.stateText} role="alert">
        Anomaly timeline unavailable: {error.message}{' '}
        <button type="button" className={styles.retry} onClick={() => void refetch()}>
          Retry
        </button>
      </p>
    )
  } else {
    const ticks = anomalyTicks(data.anomalies, data.fetchedAt)
    body =
      ticks.length === 0 ? (
        <p className={styles.stateText}>No anomalies in the last 24h.</p>
      ) : (
        <div className={styles.axis}>
          {ticks.map((tick) => (
            <button
              key={`${tick.service}-${tick.timestamp}`}
              type="button"
              className={styles.tick}
              style={{
                left: `${tick.x * 100}%`,
                background: SEVERITY_COLOR[tick.severity],
              }}
              aria-label={tickLabel(tick)}
              title={tickLabel(tick)}
              onClick={() => onOpenService(tick.service)}
            />
          ))}
        </div>
      )
  }

  return (
    <section className={styles.strip} aria-label="Anomalies, last 24 hours">
      <div className={styles.header}>
        <h2 className={styles.title}>Anomalies</h2>
        <span className={styles.range}>24h ago</span>
        <span className={styles.rangeNow}>now</span>
      </div>
      {body}
    </section>
  )
}
