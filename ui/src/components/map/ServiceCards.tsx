import { useState } from 'react'
import { ChevronDown, ChevronRight } from 'lucide-react'
import ServiceRow from '@/components/common/ServiceRow'
import { rankServices } from '@/lib/triage'
import type { SystemNode } from '@/types/api'
import styles from './ServiceCards.module.css'

interface ServiceCardsProps {
  nodes: readonly SystemNode[]
  onOpen: (id: string) => void
}

function Section({
  title,
  nodes,
  onOpen,
}: Readonly<{ title: string; nodes: SystemNode[]; onOpen: (id: string) => void }>) {
  if (nodes.length === 0) return null
  return (
    <section aria-label={title}>
      <h3 className={styles.sectionTitle}>{title}</h3>
      <ul className={styles.list}>
        {nodes.map((n) => (
          <li key={n.id}>
            <ServiceRow node={n} onOpen={onOpen} />
          </li>
        ))}
      </ul>
    </section>
  )
}

/**
 * xs default for /map: status-grouped service cards, worst first; quiet
 * healthy services collapsed behind a disclosure.
 */
export default function ServiceCards({ nodes, onOpen }: Readonly<ServiceCardsProps>) {
  const ranked = rankServices(nodes)
  const [healthyOpen, setHealthyOpen] = useState(false)

  return (
    <div className={styles.cards}>
      <Section title="Critical" nodes={ranked.critical} onOpen={onOpen} />
      <Section title="Degraded" nodes={ranked.degraded} onOpen={onOpen} />
      <Section title="Alerts" nodes={ranked.alerted} onOpen={onOpen} />
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
            {ranked.healthy.length} healthy service
            {ranked.healthy.length === 1 ? '' : 's'}
          </button>
          {healthyOpen && (
            <ul className={styles.list}>
              {ranked.healthy.map((n) => (
                <li key={n.id}>
                  <ServiceRow node={n} onOpen={onOpen} />
                </li>
              ))}
            </ul>
          )}
        </section>
      )}
    </div>
  )
}
