import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import * as DropdownMenu from '@radix-ui/react-dropdown-menu'
import { Info, Maximize2, Search, X } from 'lucide-react'
import { useLocation, useSearch } from 'wouter'
import FlowMap, { type FlowMapHandle } from './FlowMap'
import ServiceGroups from '@/components/common/ServiceGroups'
import { useSystemGraph } from '@/hooks/useSystemGraph'
import { useInvestigation } from '@/hooks/useInvestigation'
import { useMediaQuery } from '@/hooks/useMediaQuery'
import { downstreamDepths } from '@/lib/impact'
import { nodeStatus, type ServiceStatus } from '@/lib/triage'
import { buildHref, readParam } from '@/lib/urlState'
import type { SystemNode } from '@/types/api'
import styles from './FlowMapView.module.css'

// /map — the deterministic layered flow map (md+) or a status-grouped card
// list (xs default, with a "Flow" toggle to the full-bleed SVG).

type StatusFilter = 'all' | Exclude<ServiceStatus, 'unknown'>

const STATUS_PILLS: readonly { id: StatusFilter; label: string }[] = [
  { id: 'all', label: 'All' },
  { id: 'critical', label: 'Critical' },
  { id: 'degraded', label: 'Degraded' },
  { id: 'healthy', label: 'Healthy' },
]

/** "Updated Ns ago" freshness from the query's dataUpdatedAt. */
export function Freshness({ updatedAt }: Readonly<{ updatedAt: number }>) {
  const [seconds, setSeconds] = useState<number | null>(null)
  useEffect(() => {
    if (updatedAt <= 0) return
    const update = () =>
      setSeconds(Math.max(0, Math.round((Date.now() - updatedAt) / 1000)))
    // First paint via a macrotask (render must stay pure), then every 5s.
    const first = setTimeout(update, 0)
    const timer = setInterval(update, 5000)
    return () => {
      clearTimeout(first)
      clearInterval(timer)
    }
  }, [updatedAt])
  if (updatedAt <= 0 || seconds === null) return null
  return (
    <span className={styles.freshness}>
      {seconds < 5 ? 'Updated just now' : `Updated ${seconds}s ago`}
    </span>
  )
}

function matchesFilters(node: SystemNode, query: string, status: StatusFilter): boolean {
  if (status !== 'all' && nodeStatus(node.status) !== status) return false
  return query === '' || node.id.toLowerCase().includes(query)
}

function LegendPopover() {
  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <button type="button" className={styles.toolButton} aria-label="Legend">
          <Info size={14} aria-hidden="true" />
        </button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content className={styles.legend} align="end" sideOffset={6}>
          <p className={styles.legendTitle}>Nodes</p>
          <p className={styles.legendRow}>
            <span className={styles.legendDot} style={{ background: 'var(--ok)' }} /> healthy
            <span className={styles.legendDot} style={{ background: 'var(--warn)' }} /> degraded
            <span className={styles.legendDot} style={{ background: 'var(--crit)' }} /> critical
          </p>
          <p className={styles.legendTitle}>Edges</p>
          <p className={styles.legendRow}>width = call volume (log) · red dash = failing</p>
          <p className={styles.legendTitle}>Keys</p>
          <p className={styles.legendRow}>←→↑↓ walk · Enter inspect · Esc clear · f fit · / filter</p>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  )
}

function SkeletonMap() {
  return (
    <div className={styles.skeleton} data-testid="map-skeleton" aria-hidden="true">
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className={styles.skeletonNode} />
      ))}
    </div>
  )
}

export default function FlowMapView() {
  const { graph, loading, error, reload, dataUpdatedAt } = useSystemGraph()
  const { service, openService, closeInspector } = useInvestigation()
  const search = useSearch()
  const [, navigate] = useLocation()
  const isXs = useMediaQuery('(max-width: 767px)')
  const [query, setQuery] = useState('')
  const [status, setStatus] = useState<StatusFilter>('all')
  // xs defaults to the card list; "Flow" toggles the full-bleed SVG.
  const [xsFlow, setXsFlow] = useState(false)
  const filterRef = useRef<HTMLInputElement>(null)
  const mapRef = useRef<FlowMapHandle>(null)

  const nodes = useMemo(() => graph?.nodes ?? [], [graph])
  const edges = useMemo(() => graph?.edges ?? [], [graph])
  const q = query.trim().toLowerCase()
  const filteredNodes = useMemo(
    () => nodes.filter((n) => matchesFilters(n, q, status)),
    [nodes, q, status],
  )

  // ?impact= blast-radius overlay (Inspector "Show on map" / shared links):
  // BFS the downstream cone client-side over the already-loaded edge set.
  const impactService = readParam(search, 'impact')
  const impactDepths = useMemo(() => {
    if (!impactService || edges.length === 0) return null
    return downstreamDepths(
      edges.map((e) => ({ source: e.source, target: e.target })),
      impactService,
    )
  }, [impactService, edges])
  const clearImpact = useCallback(() => {
    navigate(buildHref('/map', search, { impact: null }), { replace: true })
  }, [navigate, search])

  const focusFilter = useCallback(() => filterRef.current?.focus(), [])
  // The cone is a map-only affordance — entering impact mode on xs jumps
  // straight to the flow rendering instead of the card list.
  const showMap = !isXs || xsFlow || impactDepths !== null

  let body
  if (loading) {
    body = <SkeletonMap />
  } else if (error) {
    body = (
      <div className={styles.statePanel} role="alert">
        <p>Couldn’t load the service graph: {error}</p>
        <button type="button" className={styles.stateAction} onClick={reload}>
          Retry
        </button>
      </div>
    )
  } else if (nodes.length === 0) {
    body = (
      <div className={styles.statePanel}>
        <p>No services discovered yet.</p>
        <p className={styles.stateHint}>
          Point an OTLP exporter at this host — endpoints are in the Connect
          menu (top right).
        </p>
      </div>
    )
  } else if (filteredNodes.length === 0) {
    body = (
      <div className={styles.statePanel}>
        <p>No services match the current filters.</p>
        <button
          type="button"
          className={styles.stateAction}
          onClick={() => {
            setQuery('')
            setStatus('all')
          }}
        >
          Clear filters
        </button>
      </div>
    )
  } else if (showMap) {
    body = (
      <FlowMap
        ref={mapRef}
        nodes={filteredNodes}
        edges={edges}
        selectedId={service}
        impact={impactDepths}
        onSelect={openService}
        onClearSelection={closeInspector}
        onFocusFilter={focusFilter}
      />
    )
  } else {
    body = <ServiceGroups nodes={filteredNodes} onOpen={openService} />
  }

  return (
    <div className={styles.view}>
      <div className={styles.toolbar}>
        {isXs && (
          <div className={styles.pills} role="group" aria-label="View mode">
            <button
              type="button"
              className={`${styles.pill} ${!xsFlow ? styles.pillActive : ''}`}
              aria-pressed={!xsFlow}
              onClick={() => setXsFlow(false)}
            >
              List
            </button>
            <button
              type="button"
              className={`${styles.pill} ${xsFlow ? styles.pillActive : ''}`}
              aria-pressed={xsFlow}
              onClick={() => setXsFlow(true)}
            >
              Flow
            </button>
          </div>
        )}

        <div className={styles.filterWrap}>
          <Search size={13} className={styles.filterIcon} aria-hidden="true" />
          <input
            ref={filterRef}
            className={styles.filter}
            type="text"
            value={query}
            placeholder="Filter services"
            aria-label="Filter services"
            onChange={(e) => setQuery(e.target.value)}
          />
          {query !== '' && (
            <button
              type="button"
              className={styles.filterClear}
              aria-label="Clear filter"
              onClick={() => {
                setQuery('')
                focusFilter()
              }}
            >
              <X size={12} aria-hidden="true" />
            </button>
          )}
        </div>

        <div className={styles.pills} role="group" aria-label="Status filter">
          {STATUS_PILLS.map((pill) => (
            <button
              key={pill.id}
              type="button"
              className={`${styles.pill} ${status === pill.id ? styles.pillActive : ''}`}
              aria-pressed={status === pill.id}
              onClick={() => setStatus(pill.id)}
            >
              {pill.label}
            </button>
          ))}
        </div>

        <div className={styles.toolbarEnd}>
          <Freshness updatedAt={dataUpdatedAt} />
          {showMap && (
            <button
              type="button"
              className={styles.toolButton}
              aria-label="Fit graph to view"
              onClick={() => mapRef.current?.fit()}
            >
              <Maximize2 size={14} aria-hidden="true" />
            </button>
          )}
          <LegendPopover />
        </div>
      </div>
      {impactService && (
        <div className={styles.impactBanner} role="status">
          <span className={styles.impactText}>
            Blast radius of{' '}
            <code className={styles.impactService}>{impactService}</code>
            {impactDepths !== null && (
              <> — {impactDepths.size - 1} downstream</>
            )}
          </span>
          <button
            type="button"
            className={styles.toolButton}
            aria-label="Clear blast radius overlay"
            onClick={clearImpact}
          >
            <X size={13} aria-hidden="true" />
          </button>
        </div>
      )}
      {body}
    </div>
  )
}
