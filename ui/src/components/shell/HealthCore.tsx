import { Gauge } from '@/components/common/Gauge'
import { Metric } from '@/components/common/Metric'
import { UptimeOdometer } from './UptimeOdometer'
import { formatCount, formatMs, formatPercent } from '@/lib/format'
import type { SystemSummary } from '@/types/api'
import styles from './HealthCore.module.css'

// Synthetic ceiling for the P99 arc — disclosed in the title so the arc
// saturating never reads as a real SLA threshold. The numeral is authoritative.
const P99_ARC_CEILING_MS = 1000

export type Tone = 'ok' | 'warn' | 'crit'

/** Worst active tier — never a rainbow. critical wins, then degraded, else ok. */
export function worstTier(s: SystemSummary): Tone {
  if (s.critical > 0) return 'crit'
  if (s.degraded > 0) return 'warn'
  return 'ok'
}

/**
 * One concise status sentence for the polite live region. Derived only from the
 * discrete tier + critical/degraded counts (never the health %, error rate, or
 * uptime tick) so the string mutates — and a screen reader announces — only on a
 * meaningful tier change, not on every poll.
 */
export function statusSentence(s: SystemSummary): string {
  if (s.critical > 0) {
    const svc = s.critical === 1 ? 'service' : 'services'
    return `System critical: ${s.critical} ${svc} failing.`
  }
  if (s.degraded > 0) {
    const svc = s.degraded === 1 ? 'service' : 'services'
    return `System degraded: ${s.degraded} ${svc} unhealthy.`
  }
  return 'System healthy: all services nominal.'
}

interface HealthCoreProps {
  summary: SystemSummary
  /** P99 latency in ms when the dashboard query has paid for it, else null. */
  p99Ms: number | null
  /** Compact variant for the canvas center (smaller gauge, tighter satellites). */
  compact?: boolean
}

/**
 * HealthCore — the fused platform-vitals instrument: overall-health gauge
 * (tinted by worst active tier; numeral authoritative) + uptime odometer +
 * ERR/P99/SVCS readouts. It is the center of the Constellation canvas, rendered
 * as the pinned HTML overlay (`compact`). Pure composition over
 * Gauge/Metric/UptimeOdometer — no data fetching of its own, so the host owns
 * loading/empty/error states and HealthCore only paints a live summary.
 */
export default function HealthCore({ summary, p99Ms, compact = false }: Readonly<HealthCoreProps>) {
  const tier = worstTier(summary)
  const healthPct = formatPercent(summary.overall_health_score)
  const errText = formatPercent(summary.total_error_rate)

  return (
    <div className={`${styles.core} ${compact ? styles.compact : ''}`}>
      {/* Polite live region — the only auto-announcing surface on the core.
          Its text is derived from the discrete tier/counts (statusSentence), so
          a screen reader is notified when overall health flips tier, never on
          the per-poll health % / error-rate / 1Hz uptime churn around it. */}
      <p className={styles.srOnly} aria-live="polite">
        {statusSentence(summary)}
      </p>
      <div className={styles.hero}>
        <Gauge
          value={summary.overall_health_score}
          max={1}
          label="HEALTH"
          valueText={healthPct}
          tone={tier}
        />
        <UptimeOdometer uptimeSeconds={summary.uptime_seconds} />
      </div>

      <div className={styles.satellites}>
        <Gauge
          value={summary.total_error_rate}
          max={1}
          label="ERR"
          valueText={errText}
          tone={summary.total_error_rate > 0 ? 'warn' : 'ok'}
        />
        {p99Ms !== null ? (
          <Gauge
            value={p99Ms}
            max={P99_ARC_CEILING_MS}
            label="P99"
            valueText={formatMs(p99Ms)}
            tone="accent"
          />
        ) : (
          <div className={styles.numericOnly}>
            <Metric label="P99" value="—" />
          </div>
        )}
        <Gauge
          value={summary.healthy}
          max={summary.total_services}
          label="SVCS"
          valueText={`${formatCount(summary.healthy)}/${formatCount(summary.total_services)}`}
          tone={tier}
        />
      </div>
    </div>
  )
}
