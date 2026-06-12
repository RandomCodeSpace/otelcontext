import { formatMs, formatPercent } from '@/lib/format'
import { nodeStatus, statusToken } from '@/lib/triage'
import type { SystemNode } from '@/types/api'
import styles from './ServiceRow.module.css'

interface ServiceRowProps {
  node: SystemNode
  /** Row tap — drill into the service (pushes the investigation trail). */
  onOpen: (id: string) => void
}

/**
 * One service row: status dot, mono name, inline 60px health bar, err%/p99
 * (tabular), alert-count badge. Shared by the Triage feed and the flow map's
 * xs card list so "how a service looks" has one source of truth.
 */
export default function ServiceRow({ node, onOpen }: Readonly<ServiceRowProps>) {
  const color = statusToken(nodeStatus(node.status))
  return (
    <button type="button" className={styles.row} onClick={() => onOpen(node.id)}>
      <span className={styles.dot} style={{ background: color }} aria-hidden="true" />
      <span className={styles.name}>{node.id}</span>
      {node.alerts.length > 0 && (
        <span className={styles.badge} aria-label={`${node.alerts.length} alerts`}>
          {node.alerts.length}
        </span>
      )}
      <span
        className={styles.healthBar}
        role="meter"
        aria-label={`${node.id} health`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(node.health_score * 100)}
      >
        <span
          className={styles.healthFill}
          style={{
            width: `${Math.min(100, Math.max(0, node.health_score * 100))}%`,
            background: color,
          }}
        />
      </span>
      <span className={styles.metrics}>
        <span className={styles.metric}>{formatPercent(node.metrics.error_rate)}</span>
        <span className={styles.metric}>{formatMs(node.metrics.p99_latency_ms)}</span>
      </span>
    </button>
  )
}
