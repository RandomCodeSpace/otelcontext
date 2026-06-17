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
}

function Section({
  title,
  nodes,
  onOpen,
  selectedId,
}: Readonly<{
  title: string
  nodes: SystemNode[]
  onOpen: (id: string) => void
  selectedId?: string | null
}>) {
  if (nodes.length === 0) return null
  return (
    <section aria-label={title}>
      <h3 className={`legend ${styles.sectionTitle}`}>{title}</h3>
      <ul className={styles.list}>
        {nodes.map((n) => (
          <li key={n.id}>
            <ServiceRow node={n} onOpen={onOpen} selectedId={selectedId} />
          </li>
        ))}
      </ul>
    </section>
  )
}

/**
 * Status-grouped service list, worst first; quiet healthy services collapsed
 * behind a disclosure. Hairline-separated rows (not cards). Doubles as the xs
 * default for /map.
 */
export default function ServiceGroups({ nodes, onOpen, selectedId }: Readonly<ServiceGroupsProps>) {
  const ranked = rankServices(nodes)
  const [healthyOpen, setHealthyOpen] = useState(false)

  return (
    <div className={styles.cards}>
      <Section title="Critical" nodes={ranked.critical} onOpen={onOpen} selectedId={selectedId} />
      <Section title="Degraded" nodes={ranked.degraded} onOpen={onOpen} selectedId={selectedId} />
      <Section title="Alerts" nodes={ranked.alerted} onOpen={onOpen} selectedId={selectedId} />
      {ranked.healthy.length > 0 && (
        <section aria-label="Healthy">
          <button
            type="button"
            className={styles.disclosure}
            aria-expanded={healthyOpen}
            onClick={() => setHealthyOpen((open) => !open)}
          >
            {healthyOpen ? (
              <ChevronDown size={13} aria-hidden="true" />
            ) : (
              <ChevronRight size={13} aria-hidden="true" />
            )}
            <span className="num">{ranked.healthy.length}</span> healthy service
            {ranked.healthy.length === 1 ? '' : 's'}
          </button>
          {healthyOpen && (
            <ul className={styles.list}>
              {ranked.healthy.map((n) => (
                <li key={n.id}>
                  <ServiceRow node={n} onOpen={onOpen} selectedId={selectedId} />
                </li>
              ))}
            </ul>
          )}
        </section>
      )}
    </div>
  )
}
