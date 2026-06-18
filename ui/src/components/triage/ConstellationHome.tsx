import { useCallback, useDeferredValue, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Maximize2, Search, X } from 'lucide-react'
import { useLocation, useSearch } from 'wouter'
import FlowMap, { type FlowMapHandle } from '@/components/map/FlowMap'
import ConnectInline from '@/components/shell/ConnectInline'
import ServiceGroups from '@/components/common/ServiceGroups'
import Sparkline from './Sparkline'
import { useSystemGraph } from '@/hooks/useSystemGraph'
import { useInvestigation } from '@/hooks/useInvestigation'
import { useMediaQuery } from '@/hooks/useMediaQuery'
import { useAnomalyTimeline } from '@/hooks/useAnomalyTimeline'
import { downstreamDepths } from '@/lib/impact'
import { buildHref, readParam } from '@/lib/urlState'
import type { TrafficPoint } from '@/types/api'
import styles from './ConstellationHome.module.css'

/**
 * Observe the ['metrics-traffic'] cache WITHOUT fetching (enabled: false): the
 * error-trend sparkline renders only when another surface has already paid for
 * the data, so the home adds zero polling of its own (re-homed from the retired
 * TriageView per the spec's reuse map).
 */
function useCachedTraffic(): TrafficPoint[] | null {
  const { data } = useQuery<TrafficPoint[]>({
    queryKey: ['metrics-traffic'],
    enabled: false,
  })
  return data ?? null
}

// / — the Constellation home. The living radial map IS the canvas: services
// orbit the Health Core, the worst-first rail and the 24h anomaly tape ride
// alongside, and selecting a node (canvas or rail) docks the Inspector. On xs
// the cinematic canvas is gated behind a "Flow" toggle — the dense worst-first
// card list is the default so a phone never has to drive an SVG field.

function SkeletonCanvas() {
  return (
    <div className={styles.skeleton} data-testid="home-skeleton" aria-hidden="true">
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className={styles.skeletonNode} />
      ))}
    </div>
  )
}

export default function ConstellationHome() {
  const { graph, loading, error, reload } = useSystemGraph()
  const { service, openService, closeInspector } = useInvestigation()
  const search = useSearch()
  const [, navigate] = useLocation()
  const isXs = useMediaQuery('(max-width: 767px)')
  // The cinematic field is the home on EVERY breakpoint, phone included: a
  // static radial field with tap-to-inspect + pinch-zoom IS the experience, and
  // ambient motion is already gated off on coarse pointers / reduced-motion. The
  // `flow` param is the explicit override — '1' forces the canvas, '0' forces
  // the dense card list — persisted in the URL so the choice survives reload +
  // Inspector round-trips. Absent, xs defaults to the canvas for a reasonable
  // service count and to the card list above it (a 100+ node SVG field is not a
  // phone surface).
  const xsFlowParam = readParam(search, 'flow')
  const traffic = useCachedTraffic()
  const mapRef = useRef<FlowMapHandle>(null)

  const nodes = useMemo(() => graph?.nodes ?? [], [graph])
  const edges = useMemo(() => graph?.edges ?? [], [graph])

  // Client-side service search over the already-loaded graph (no API call).
  // useDeferredValue keeps typing responsive — the match recompute + map
  // re-emphasis run at a lower priority than the keystroke. Matches drive the
  // map's `searchMatches` highlight; null when the box is empty (no highlight).
  const [query, setQuery] = useState('')
  const deferredQuery = useDeferredValue(query)
  const searchMatches = useMemo(() => {
    const q = deferredQuery.trim().toLowerCase()
    if (!q) return null
    const out = new Set<string>()
    for (const n of nodes) {
      if (n.id.toLowerCase().includes(q)) out.add(n.id)
    }
    return out
  }, [deferredQuery, nodes])

  // Services with an active anomaly — feeds the side panel's "Anomalies" group.
  // Reads the shared ['anomaly-timeline'] cache (no extra fetch of its own).
  const { data: anomalyData } = useAnomalyTimeline()
  const anomalyServiceIds = useMemo(
    () => new Set((anomalyData?.anomalies ?? []).map((a) => a.service)),
    [anomalyData],
  )

  // ?impact= blast-radius overlay (Inspector "Show on map" / shared links):
  // BFS the downstream cone client-side over the already-loaded edge set —
  // re-homed here from the retired /map view, so the cone survives the fold.
  const impactService = readParam(search, 'impact')
  const impactDepths = useMemo(() => {
    if (!impactService || edges.length === 0) return null
    return downstreamDepths(
      edges.map((e) => ({ source: e.source, target: e.target })),
      impactService,
    )
  }, [impactService, edges])
  const clearImpact = useCallback(() => {
    navigate(buildHref('/', search, { impact: null }), { replace: true })
  }, [navigate, search])

  const setXsFlow = useCallback(
    (on: boolean) => {
      navigate(buildHref('/', search, { flow: on ? '1' : '0' }), { replace: true })
    },
    [navigate, search],
  )

  // "List" must always reach the card list. The canvas is forced open while
  // ?impact= is set, so pin flow='0' AND clear impact in one navigation
  // (sequential setXsFlow + clearImpact would each read the same stale `search`
  // and clobber the other's change).
  const exitToList = useCallback(() => {
    navigate(buildHref('/', search, { flow: '0', impact: null }), { replace: true })
  }, [navigate, search])

  // xs canvas-vs-list resolution: explicit `flow` override wins, else default by
  // node count. The impact cone is a canvas affordance and always forces it.
  const xsCanvasFits = nodes.length <= 40
  const xsWantsCanvas = xsFlowParam === '1' || (xsFlowParam === null && xsCanvasFits)
  const showCanvas = !isXs || xsWantsCanvas || impactDepths !== null

  if (loading) {
    return (
      <div className={styles.view}>
        <SkeletonCanvas />
      </div>
    )
  }

  if (error) {
    return (
      <div className={styles.view}>
        <div className={styles.statePanel} role="alert">
          <p>Couldn’t load the service graph: {error}</p>
          <button type="button" className={styles.stateAction} onClick={reload}>
            Retry
          </button>
        </div>
      </div>
    )
  }

  if (nodes.length === 0) {
    return (
      <div className={styles.view}>
        <ConnectInline />
      </div>
    )
  }

  return (
    <div className={styles.view}>
      {impactService && (
        <div className={styles.impactBanner} role="status">
          <span className={styles.impactText}>
            Blast radius of{' '}
            <code className={styles.impactService}>{impactService}</code>
            {impactDepths !== null && <> — {impactDepths.size - 1} downstream</>}
          </span>
          <button
            type="button"
            className={styles.bannerButton}
            aria-label="Clear blast radius overlay"
            onClick={clearImpact}
          >
            <X size={13} aria-hidden="true" />
          </button>
        </div>
      )}

      <div className={styles.stage}>
        {showCanvas ? (
          <div className={styles.canvas}>
            <div className={styles.searchBar}>
              <Search size={14} aria-hidden="true" className={styles.searchIcon} />
              <input
                type="search"
                className={styles.searchInput}
                placeholder="Find a service…"
                aria-label="Find a service on the map"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
              {query && (
                <button
                  type="button"
                  className={styles.searchClear}
                  aria-label="Clear service search"
                  onClick={() => setQuery('')}
                >
                  <X size={13} aria-hidden="true" />
                </button>
              )}
              {searchMatches && (
                <span className={styles.searchCount} role="status">
                  {searchMatches.size === 0 ? 'No match' : `${searchMatches.size} match${searchMatches.size === 1 ? '' : 'es'}`}
                </span>
              )}
            </div>
            <FlowMap
              ref={mapRef}
              nodes={nodes}
              edges={edges}
              selectedId={service}
              impact={impactDepths}
              searchMatches={searchMatches}
              dim={service !== null}
              ringDepth
              centerLabel={false}
              onSelect={openService}
              onClearSelection={closeInspector}
              onFocusFilter={() => {}}
            />
            <button
              type="button"
              className={styles.fit}
              aria-label="Fit graph to view"
              onClick={() => mapRef.current?.fit()}
            >
              <Maximize2 size={14} aria-hidden="true" />
            </button>
            {isXs && (
              <button
                type="button"
                className={`${styles.viewToggle} ${styles.viewToggleActive}`}
                aria-pressed={showCanvas}
                onClick={exitToList}
              >
                List
              </button>
            )}
          </div>
        ) : (
          <section className={styles.rail} aria-label="Service triage feed">
            {/* Health vitals live in the header now — the card list goes
                straight to the worst-first services (no on-page health card). */}
            <div className={styles.railHead}>
              <h2 className={`legend ${styles.railTitle}`}>Services</h2>
              <button
                type="button"
                className={styles.viewToggle}
                aria-pressed={showCanvas}
                onClick={() => setXsFlow(true)}
              >
                Flow
              </button>
            </div>
            <ServiceGroups
              nodes={nodes}
              onOpen={openService}
              selectedId={service}
              anomalyServiceIds={anomalyServiceIds}
            />
          </section>
        )}

        {/* Desktop/tablet: the slim worst-first rail rides alongside the canvas.
            Hidden on xs (the card list IS the xs default above). It STAYS when the
            Inspector is open — the inspector is a popup floating over the map, not
            a docked column, so the rail no longer gets squeezed; keeping it put
            avoids the layout jolting on every node select. The selected service is
            reflected by the rail's focus-distortion (selectedId). */}
        {!isXs && (
          <aside className={styles.sideRail} aria-label="Service triage feed">
            <div className={styles.railHead}>
              <h2 className={`legend ${styles.railTitle}`}>Services</h2>
              {traffic !== null && traffic.length >= 2 && (
                <Sparkline
                  values={traffic.map((p) => p.error_count)}
                  label="Error trend, last 30 minutes"
                />
              )}
            </div>
            <ServiceGroups
              nodes={nodes}
              onOpen={openService}
              selectedId={service}
              anomalyServiceIds={anomalyServiceIds}
            />
          </aside>
        )}
      </div>
    </div>
  )
}
