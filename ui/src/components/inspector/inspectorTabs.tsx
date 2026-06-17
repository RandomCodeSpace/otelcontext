import { ChevronRight, TriangleAlert } from 'lucide-react'
import type { SystemEdge, SystemNode } from '@/types/api'
import { formatCount, formatMs, formatPercent } from '@/lib/format'
import { Metric } from '@/components/common/Metric'
import { nodeStatus, statusToken } from '@/lib/triage'
import styles from './ServiceInspector.module.css'

// Inspector tab CONTENT components (Overview, Dependencies). The registry
// that orders them lives in ./registry.ts; the MCP verb tabs ("Why",
// "Impact") live in WhyTab.tsx / ImpactTab.tsx.

export interface InspectorTabContext {
  node: SystemNode
  edges: readonly SystemEdge[]
  /** Drill into another service — pushes the investigation trail. */
  openService: (id: string) => void
  /** Hand a blast radius to the flow map's ?impact= cone overlay. */
  showImpactOnMap: (service: string) => void
}

// One instrument readout: uppercase legend cap above a mono numeral. `crit`
// tints the numeral with status saturation (used for a non-zero error rate).
function Stat({
  label,
  value,
  crit = false,
}: Readonly<{ label: string; value: string; crit?: boolean }>) {
  return (
    <div className={`${styles.stat}${crit ? ` ${styles.statCrit}` : ''}`}>
      <Metric label={label} value={value} />
    </div>
  )
}

export function OverviewTab({ ctx }: Readonly<{ ctx: InspectorTabContext }>) {
  const { node } = ctx
  const m = node.metrics
  const color = statusToken(nodeStatus(node.status))
  return (
    <div className={styles.tabBody}>
      <div className={styles.statGrid}>
        <Stat label="RPS" value={`${formatCount(m.request_rate_rps)}/s`} />
        <Stat label="ERR" value={formatPercent(m.error_rate)} crit={m.error_rate > 0} />
        <Stat label="AVG" value={formatMs(m.avg_latency_ms)} />
        <Stat label="P99" value={formatMs(m.p99_latency_ms)} />
      </div>

      <div className={styles.healthRow}>
        <span className={styles.statLabel}>HEALTH</span>
        <div
          className={styles.healthBar}
          role="meter"
          aria-label="Health score"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(node.health_score * 100)}
        >
          <div
            className={styles.healthFill}
            style={{
              width: `${Math.min(100, Math.max(0, node.health_score * 100))}%`,
              background: color,
            }}
          />
        </div>
        <span className={styles.statValue} style={{ color }}>
          {formatPercent(node.health_score)}
        </span>
      </div>

      <section aria-label="Alerts">
        <h3 className={styles.sectionTitle}>Alerts</h3>
        {node.alerts.length === 0 ? (
          <p className={styles.quiet}>No active alerts.</p>
        ) : (
          <ul className={styles.alertList}>
            {node.alerts.map((alert) => (
              <li key={alert} className={styles.alertItem}>
                <TriangleAlert size={13} className={styles.alertIcon} aria-hidden="true" />
                {alert}
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  )
}

function DepRow({
  id,
  edge,
  onOpen,
}: Readonly<{ id: string; edge: SystemEdge; onOpen: (id: string) => void }>) {
  return (
    <li>
      <button type="button" className={styles.depRow} onClick={() => onOpen(id)}>
        <span className={styles.depName}>{id}</span>
        <span className={styles.depMeta}>
          {formatCount(edge.call_count)} calls · err {formatPercent(edge.error_rate)}
        </span>
        <ChevronRight size={13} className={styles.depChevron} aria-hidden="true" />
      </button>
    </li>
  )
}

export function DependenciesTab({ ctx }: Readonly<{ ctx: InspectorTabContext }>) {
  const { node, edges, openService } = ctx
  const upstream = edges.filter((e) => e.target === node.id)
  const downstream = edges.filter((e) => e.source === node.id)
  return (
    <div className={styles.tabBody}>
      <section aria-label="Upstream callers">
        <h3 className={styles.sectionTitle}>↑ Upstream</h3>
        {upstream.length === 0 ? (
          <p className={styles.quiet}>No upstream callers.</p>
        ) : (
          <ul className={styles.depList}>
            {upstream.map((e) => (
              <DepRow key={e.source} id={e.source} edge={e} onOpen={openService} />
            ))}
          </ul>
        )}
      </section>
      <section aria-label="Downstream dependencies">
        <h3 className={styles.sectionTitle}>↓ Downstream</h3>
        {downstream.length === 0 ? (
          <p className={styles.quiet}>No downstream dependencies.</p>
        ) : (
          <ul className={styles.depList}>
            {downstream.map((e) => (
              <DepRow key={e.target} id={e.target} edge={e} onOpen={openService} />
            ))}
          </ul>
        )}
      </section>
    </div>
  )
}
