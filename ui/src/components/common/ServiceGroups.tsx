import { useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import ServiceRow from './ServiceRow'
import { rankServices } from '@/lib/triage'
import type { SystemNode } from '@/types/api'
import styles from './ServiceGroups.module.css'

interface ServiceGroupsProps {
  nodes: readonly SystemNode[]
  onOpen: (id: string) => void
  /** Id of the inspected service — drives row focus-distortion. Optional so
   * the xs flow-map fallback (no inspector) renders a plain list. */
  selectedId?: string | null
  /** Ids of services with an active anomaly. When present, a collapsible
   * "Anomalies" group lists them worst-first — a cross-cut of "what's spiking
   * now", distinct from the status groups (current health). */
  anomalyServiceIds?: ReadonlySet<string>
}

/**
 * A collapsible status group: a disclosure header (chevron + caps title +
 * count) over its worst-first rows. Renders nothing when empty. Each group
 * owns its open state so an operator can fold away groups they don't care
 * about — at 120 services the panel stays navigable.
 */
function CollapsibleSection({
  title,
  nodes,
  onOpen,
  selectedId,
  defaultOpen = true,
}: Readonly<{
  title: string
  nodes: SystemNode[]
  onOpen: (id: string) => void
  selectedId?: string | null
  defaultOpen?: boolean
}>) {
  const [open, setOpen] = useState(defaultOpen)
  if (nodes.length === 0) return null
  return (
    <section aria-label={title}>
      <button
        type="button"
        className={styles.disclosure}
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        {open ? (
          <ChevronDown size={13} aria-hidden="true" />
        ) : (
          <ChevronRight size={13} aria-hidden="true" />
        )}
        <span className="legend">{title}</span>
        <span className={`num ${styles.count}`}>{nodes.length}</span>
      </button>
      {open && (
        <ul className={styles.list}>
          {nodes.map((n) => (
            <li key={n.id}>
              <ServiceRow node={n} onOpen={onOpen} selectedId={selectedId} />
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

/**
 * Status-grouped service list, worst first. Every group is collapsible behind a
 * disclosure header; the quiet healthy group starts collapsed. Hairline-
 * separated rows (not cards). Doubles as the xs default for /map.
 */
export default function ServiceGroups({
  nodes,
  onOpen,
  selectedId,
  anomalyServiceIds,
}: Readonly<ServiceGroupsProps>) {
  const ranked = rankServices(nodes)
  // Worst-first list of services carrying an active anomaly.
  const anomalous = anomalyServiceIds
    ? [...ranked.critical, ...ranked.degraded, ...ranked.alerted, ...ranked.healthy].filter((n) =>
        anomalyServiceIds.has(n.id),
      )
    : []
  return (
    <div className={styles.cards}>
      <CollapsibleSection title="Anomalies" nodes={anomalous} onOpen={onOpen} selectedId={selectedId} />
      <CollapsibleSection title="Critical" nodes={ranked.critical} onOpen={onOpen} selectedId={selectedId} />
      <CollapsibleSection title="Degraded" nodes={ranked.degraded} onOpen={onOpen} selectedId={selectedId} />
      <CollapsibleSection title="Alerts" nodes={ranked.alerted} onOpen={onOpen} selectedId={selectedId} />
      <CollapsibleSection
        title="Healthy"
        nodes={ranked.healthy}
        onOpen={onOpen}
        selectedId={selectedId}
        defaultOpen={false}
      />
    </div>
  )
}
