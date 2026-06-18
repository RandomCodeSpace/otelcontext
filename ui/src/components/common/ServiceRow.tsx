import { nodeStatus, statusToken } from '@/lib/triage'
import type { SystemNode } from '@/types/api'
import styles from './ServiceRow.module.css'

interface ServiceRowProps {
  node: SystemNode
  /** Row tap — drill into the service (pushes the investigation trail). */
  onOpen: (id: string) => void
  /**
   * The id of the single inspected service, if any. When this row is the
   * selected one it gets the focus-distortion (scale + accent ring); the
   * non-selected rows dim. Optional so the xs flow-map fallback (no
   * inspector) keeps the plain list.
   */
  selectedId?: string | null
}

/**
 * One service row: status dot, name, alert badge, and an inline health bar.
 * The numeric stats (rps / err% / p99) are intentionally NOT shown here — the
 * list stays name-first and legible on the narrow rail; the full stats live in
 * the Inspector popup, one click away. Shared by the Triage feed and the flow
 * map's xs card list so "how a service looks" has one source of truth.
 *
 * Focus-distortion: when a service is inspected, the selected row lifts
 * (scale 1.015) with an --accent-edge ring and the rest dim. This is a pure
 * CSS transition applied after the URL state commits (no JS animation loop),
 * so INP stays low; reduced-motion keeps the end-state (ring + dim), drops
 * only the travel.
 */
export default function ServiceRow({ node, onOpen, selectedId }: Readonly<ServiceRowProps>) {
  const color = statusToken(nodeStatus(node.status))
  const hasSelection = selectedId != null && selectedId !== ''
  const selected = hasSelection && selectedId === node.id
  const dimmed = hasSelection && !selected
  return (
    <button
      type="button"
      className={styles.row}
      data-selected={selected || undefined}
      data-dimmed={dimmed || undefined}
      aria-current={selected ? 'true' : undefined}
      onClick={() => onOpen(node.id)}
    >
      <span className={styles.dot} style={{ background: color }} aria-hidden="true" />
      <span className={styles.name}>{node.id}</span>
      {node.alerts.length > 0 && (
        <span className={styles.badge} aria-label={`${node.alerts.length} alerts`}>
          <span className="num">{node.alerts.length}</span>
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
    </button>
  )
}
