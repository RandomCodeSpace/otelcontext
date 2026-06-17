import type { CSSProperties } from 'react'
import { useAnomalyTimeline } from '@/hooks/useAnomalyTimeline'
import type { AnomalyNode, AnomalySeverity } from '@/types/api'
import styles from './AnomalyStrip.module.css'

// Recent-anomaly summary — counts in the last 15m / 30m / 1h (NOT a 24h
// timeline) plus the recently-anomalous services as tappable chips that open
// the Inspector. Answers "what's anomalous right now", not "what happened today".

const WINDOWS: readonly { label: string; ms: number }[] = [
  { label: '15M', ms: 15 * 60_000 },
  { label: '30M', ms: 30 * 60_000 },
  { label: '1H', ms: 60 * 60_000 },
]
const RECENT_MS = 60 * 60_000
const MAX_CHIPS = 8

const SEVERITY_RANK: Record<AnomalySeverity, number> = { critical: 3, warning: 2, info: 1 }
const SEVERITY_COLOR: Record<AnomalySeverity, string> = {
  critical: 'var(--crit)',
  warning: 'var(--warn)',
  info: 'var(--text-3)',
}

function worstSeverity(items: readonly AnomalySeverity[]): AnomalySeverity | null {
  let worst: AnomalySeverity | null = null
  for (const s of items) {
    if (worst === null || SEVERITY_RANK[s] > SEVERITY_RANK[worst]) worst = s
  }
  return worst
}

function ageLabel(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000))
  if (s < 60) return `${s}s`
  const m = Math.round(s / 60)
  if (m < 60) return `${m}m`
  return `${Math.round(m / 60)}h`
}

interface AnomalyStripProps {
  onOpenService: (id: string) => void
}

export default function AnomalyStrip({ onOpenService }: Readonly<AnomalyStripProps>) {
  const { data, isPending, error, refetch } = useAnomalyTimeline()
  const ready = !isPending && !error && data !== undefined

  const now = data?.fetchedAt ?? 0
  const timed: { a: AnomalyNode; t: number }[] = ready
    ? data.anomalies
        .map((a) => ({ a, t: Date.parse(a.timestamp) }))
        .filter((x) => !Number.isNaN(x.t))
    : []

  const windows = WINDOWS.map((w) => {
    const inWin = timed.filter((x) => now - x.t <= w.ms)
    return { label: w.label, count: inWin.length, severity: worstSeverity(inWin.map((x) => x.a.severity)) }
  })

  // Distinct services anomalous in the last hour, most-recent first.
  const byService = new Map<string, { latest: number; severity: AnomalySeverity }>()
  for (const { a, t } of timed) {
    if (now - t > RECENT_MS) continue
    const cur = byService.get(a.service)
    if (!cur) {
      byService.set(a.service, { latest: t, severity: a.severity })
    } else {
      if (t > cur.latest) cur.latest = t
      if (SEVERITY_RANK[a.severity] > SEVERITY_RANK[cur.severity]) cur.severity = a.severity
    }
  }
  const services = [...byService.entries()].sort((a, b) => b[1].latest - a[1].latest).slice(0, MAX_CHIPS)
  const overflow = byService.size - services.length

  return (
    <section className={styles.strip} aria-label="Recent anomalies">
      <div className={styles.header}>
        <h2 className={`legend ${styles.title}`}>Anomalies</h2>
        {ready && (
          <div className={styles.windows} aria-label="Anomaly counts by recent window">
            {windows.map((w) => (
              <span
                key={w.label}
                className={styles.window}
                style={w.severity ? ({ color: SEVERITY_COLOR[w.severity] } as CSSProperties) : undefined}
              >
                <span className="legend">{w.label}</span>
                <span className="num">{w.count}</span>
              </span>
            ))}
          </div>
        )}
      </div>

      {isPending ? (
        <div className={styles.skeleton} data-testid="strip-skeleton" aria-hidden="true" />
      ) : error ? (
        <p className={styles.stateText} role="alert">
          Anomalies unavailable: {error.message}{' '}
          <button type="button" className={styles.retry} onClick={() => void refetch()}>
            Retry
          </button>
        </p>
      ) : byService.size === 0 ? (
        <p className={styles.stateText}>No anomalies in the last hour.</p>
      ) : (
        <div className={styles.chips}>
          {services.map(([service, info]) => (
            <button
              key={service}
              type="button"
              className={styles.chip}
              style={{ '--chip-color': SEVERITY_COLOR[info.severity] } as CSSProperties}
              onClick={() => onOpenService(service)}
              title={`${service} — ${info.severity}, ${ageLabel(now - info.latest)} ago`}
            >
              <span className={styles.chipName}>{service}</span>
              <span className="num">{ageLabel(now - info.latest)}</span>
            </button>
          ))}
          {overflow > 0 && <span className={styles.more}>+{overflow}</span>}
        </div>
      )}
    </section>
  )
}
