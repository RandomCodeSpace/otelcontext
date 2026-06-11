import type { ReactNode } from 'react'
import { Moon, Sun } from 'lucide-react'
import { useSystemGraph } from '@/hooks/useSystemGraph'
import { useDashboard } from '@/hooks/useDashboard'
import { formatMb, formatMs, formatPercent } from '@/lib/format'
import type { RepoStats, SystemSummary } from '@/types/api'
import type { Theme } from '@/hooks/useTheme'
import LiveDot from './LiveDot'
import ConnectPopover from './ConnectPopover'
import styles from './PulseBar.module.css'

// Operator honesty about the SQLite growth incident: tint the DB segment
// as a warning once the database crosses this size.
export const DB_SIZE_WARN_MB = 2048

interface PulseBarProps {
  theme?: Theme
  onToggleTheme?: () => void
  /** Injectable for tests; defaults to the singleton-backed LiveDot. */
  liveSlot?: ReactNode
}

type Tone = 'ok' | 'warn' | 'crit' | 'unknown'

function systemTone(s: SystemSummary): Tone {
  if (s.critical > 0) return 'crit'
  if (s.degraded > 0) return 'warn'
  return 'ok'
}

// /api/stats is loosely shaped (driver-dependent key casing) — accept both
// db_size_mb and DBSizeMB, as numbers or numeric strings.
function dbSizeMb(stats: RepoStats | null): number | null {
  const raw =
    stats?.db_size_mb ?? (stats as Record<string, unknown> | null)?.DBSizeMB
  if (typeof raw === 'number' && Number.isFinite(raw)) return raw
  if (typeof raw === 'string') {
    const n = Number.parseFloat(raw)
    return Number.isFinite(n) ? n : null
  }
  return null
}

function Sep() {
  return (
    <span className={styles.sep} aria-hidden="true">
      ·
    </span>
  )
}

/**
 * System Pulse bar — always-on header: health %, degraded/critical counts,
 * error rate, p99 and DB size, plus the live dot, Connect popover and theme
 * toggle. Collapses to "● N issues" on xs (CSS-only).
 */
export default function PulseBar({
  theme = 'dark',
  onToggleTheme,
  liveSlot,
}: Readonly<PulseBarProps>) {
  const { graph } = useSystemGraph()
  const { dashboard, stats } = useDashboard()

  const summary = graph?.system ?? null
  const dbMb = dbSizeMb(stats)
  const issues = summary ? summary.degraded + summary.critical : 0

  return (
    <header className={styles.bar}>
      <span className={styles.brand}>OtelContext</span>

      <div className={styles.summary}>
        {summary ? (
          <>
            <span
              className={`${styles.sysDot} ${styles[systemTone(summary)]}`}
              aria-hidden="true"
            />
            <span className={styles.summaryFull}>
              <span>{formatPercent(summary.overall_health_score)} healthy</span>
              {summary.degraded > 0 && (
                <>
                  <Sep />
                  <span className={styles.warnText}>
                    {summary.degraded} degraded
                  </span>
                </>
              )}
              {summary.critical > 0 && (
                <>
                  <Sep />
                  <span className={styles.critText}>
                    {summary.critical} critical
                  </span>
                </>
              )}
              {dashboard && (
                <>
                  <Sep />
                  {/* error_rate from /api/metrics/dashboard is already ×100 */}
                  <span>
                    err {formatPercent(dashboard.error_rate, 1, 'percent')}
                  </span>
                  <Sep />
                  <span>p99 {formatMs(dashboard.p99_latency_ms)}</span>
                </>
              )}
              {dbMb !== null && (
                <>
                  <Sep />
                  <span
                    className={dbMb >= DB_SIZE_WARN_MB ? styles.warnText : undefined}
                  >
                    DB {formatMb(dbMb)}
                  </span>
                </>
              )}
            </span>
            <span className={styles.summaryCompact}>
              {issues > 0 ? `${issues} issues` : 'all clear'}
            </span>
          </>
        ) : (
          <span className={styles.placeholder}>—</span>
        )}
      </div>

      <div className={styles.actions}>
        {liveSlot === undefined ? <LiveDot /> : liveSlot}
        <ConnectPopover />
        {onToggleTheme && (
          <button
            type="button"
            className={styles.iconButton}
            aria-label={
              theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'
            }
            onClick={onToggleTheme}
          >
            {theme === 'dark' ? (
              <Sun size={15} aria-hidden="true" />
            ) : (
              <Moon size={15} aria-hidden="true" />
            )}
          </button>
        )}
      </div>
    </header>
  )
}
