import { useQuery } from '@tanstack/react-query'
import AnomalyStrip from './AnomalyStrip'
import Sparkline from './Sparkline'
import ConnectInline from '@/components/shell/ConnectInline'
import ServiceGroups from '@/components/common/ServiceGroups'
import { useSystemGraph } from '@/hooks/useSystemGraph'
import { useInvestigation } from '@/hooks/useInvestigation'
import type { TrafficPoint } from '@/types/api'
import styles from './TriageView.module.css'

// / — the Triage home: anomaly strip (24h, MCP get_anomaly_timeline) over a
// ranked service feed (critical → degraded → alerted → healthy collapsed)
// recomposed from the shared /api/system/graph query. Row tap → Inspector.

/**
 * Observe the ['metrics-traffic'] cache WITHOUT fetching (enabled: false):
 * the sparkline renders only when another surface (Dashboard) has already
 * paid for the data — the feed adds zero polling of its own.
 */
function useCachedTraffic(): TrafficPoint[] | null {
  const { data } = useQuery<TrafficPoint[]>({
    queryKey: ['metrics-traffic'],
    enabled: false,
  })
  return data ?? null
}

function SkeletonFeed() {
  return (
    <div className={styles.skeleton} data-testid="feed-skeleton" aria-hidden="true">
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className={styles.skeletonRow} />
      ))}
    </div>
  )
}

export default function TriageView() {
  const { graph, loading, error, reload } = useSystemGraph()
  const { openService } = useInvestigation()
  const traffic = useCachedTraffic()

  const nodes = graph?.nodes ?? []

  let feed
  if (loading) {
    feed = <SkeletonFeed />
  } else if (error) {
    feed = (
      <div className={styles.statePanel} role="alert">
        <p>Couldn’t load services: {error}</p>
        <button type="button" className={styles.stateAction} onClick={reload}>
          Retry
        </button>
      </div>
    )
  } else if (nodes.length === 0) {
    feed = <ConnectInline />
  } else {
    feed = <ServiceGroups nodes={nodes} onOpen={openService} />
  }

  return (
    <div className={styles.view}>
      <AnomalyStrip onOpenService={openService} />
      <section className={styles.feed} aria-label="Service triage feed">
        <div className={styles.feedHeader}>
          <h2 className={styles.feedTitle}>Services</h2>
          {traffic !== null && traffic.length >= 2 && (
            <Sparkline
              values={traffic.map((p) => p.error_count)}
              label="Error trend, last 30 minutes"
            />
          )}
        </div>
        {feed}
      </section>
    </div>
  )
}
