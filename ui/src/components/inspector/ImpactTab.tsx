import { useMemo } from 'react'
import { ChevronRight, Network } from 'lucide-react'
import { useTriageTool } from '@/hooks/useTriageTool'
import { impactQueryOptions } from '@/lib/triageVerbs'
import { groupAffectedByDepth } from '@/lib/impact'
import { formatCount, formatPercent } from '@/lib/format'
import type { AffectedEntry } from '@/types/api'
import type { InspectorTabContext } from './inspectorTabs'
import VerbTabFrame from './VerbTabFrame'
import styles from './ServiceInspector.module.css'

// "Impact" tab — MCP impact_analysis as a human verb. Depth-grouped list of
// the downstream blast radius; "Show on map" hands the cone to the flow
// map's ?impact= overlay.

function AffectedRow({
  entry,
  openService,
}: Readonly<{ entry: AffectedEntry; openService: (id: string) => void }>) {
  return (
    <li>
      <button
        type="button"
        className={styles.depRow}
        onClick={() => openService(entry.service)}
      >
        <span className={styles.depName}>{entry.service}</span>
        <span className={styles.depMeta}>
          {formatCount(entry.call_count)} calls · impact{' '}
          {formatPercent(entry.impact_score)}
        </span>
        <ChevronRight size={13} className={styles.depChevron} aria-hidden="true" />
      </button>
    </li>
  )
}

export function ImpactTab({ ctx }: Readonly<{ ctx: InspectorTabContext }>) {
  const service = ctx.node.id
  const options = useMemo(() => impactQueryOptions(service), [service])
  const tool = useTriageTool(options)
  const affected = useMemo(
    () => tool.data?.affected_services ?? [],
    [tool.data],
  )
  const groups = useMemo(() => groupAffectedByDepth(affected), [affected])

  return (
    <VerbTabFrame
      status={tool.status}
      error={tool.error}
      runLabel="Map blast radius"
      hint={`Walk the call graph downstream from ${service} to find every service a failure here can reach.`}
      onRun={tool.run}
      onCancel={() => void tool.cancel()}
    >
      {affected.length === 0 ? (
        <>
          <p className={styles.quiet}>
            No downstream services — a failure in{' '}
            <code className={styles.mono}>{service}</code> stays contained.
          </p>
          <button type="button" className={styles.stateAction} onClick={tool.run}>
            Re-run
          </button>
        </>
      ) : (
        <>
          <div className={styles.verbHeadRow}>
            <p className={styles.quiet}>
              {affected.length} downstream service
              {affected.length === 1 ? '' : 's'} affected
            </p>
            <button
              type="button"
              className={styles.stateAction}
              onClick={() => ctx.showImpactOnMap(service)}
            >
              <Network size={13} aria-hidden="true" /> Show on map
            </button>
          </div>
          <section aria-label="Affected services by depth">
            {groups.map(([depth, entries]) => (
              <div key={depth}>
                <h4 className={styles.sectionTitle}>
                  Depth {depth} — {depth === 1 ? 'direct callees' : `${depth} hops`}
                </h4>
                <ul className={styles.depList}>
                  {entries.map((entry) => (
                    <AffectedRow
                      key={entry.service}
                      entry={entry}
                      openService={ctx.openService}
                    />
                  ))}
                </ul>
              </div>
            ))}
          </section>
        </>
      )}
    </VerbTabFrame>
  )
}
