import { useCallback, useState } from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import { X } from 'lucide-react'
import { useLocation, useSearch } from 'wouter'
import { useInvestigation } from '@/hooks/useInvestigation'
import { useSystemGraph } from '@/hooks/useSystemGraph'
import { formatPercent } from '@/lib/format'
import { Metric } from '@/components/common/Metric'
import { nodeStatus, statusToken } from '@/lib/triage'
import { buildHref, readParam } from '@/lib/urlState'
import { INSPECTOR_TABS, isInspectorTabId } from './registry'
import styles from './ServiceInspector.module.css'

// Service Inspector — two surfaces driven by ?service= (set on a node click):
//   • CATEGORY PANEL (md+ docked): the service name + the category list
//     (Overview / Why / Impact / Dependencies). NO stats — picking a category
//     just drives the popup. A slim, read-light rail.
//   • DETAILS POPUP (Radix Dialog, every breakpoint): the active category's
//     full content (stats / why / impact / deps). On xs the categories ride
//     inside the popup itself (a phone has no room for a side rail).

function StatusDot({ status }: Readonly<{ status: ReturnType<typeof nodeStatus> }>) {
  return (
    <span className={styles.statusDot} style={{ background: statusToken(status) }} aria-hidden="true" />
  )
}

function SkeletonBody() {
  return (
    <div className={styles.tabBody} aria-hidden="true" data-testid="inspector-skeleton">
      <div className={styles.statGrid}>
        {Array.from({ length: 4 }, (_, i) => (
          <div key={i} className={`${styles.stat} ${styles.skeleton}`} />
        ))}
      </div>
      <div className={`${styles.skeleton} ${styles.skeletonRow}`} />
      <div className={`${styles.skeleton} ${styles.skeletonRow}`} />
    </div>
  )
}

/** The details popup — the active category's content, opened on node click. */
function DetailsPopup({
  service,
  tab,
  onTab,
  onClose,
}: Readonly<{
  service: string
  tab: string
  onTab: (id: string) => void
  onClose: () => void
}>) {
  const { openService } = useInvestigation()
  const { graph, loading, error, reload } = useSystemGraph()
  const node = graph?.nodes.find((n) => n.id === service) ?? null
  const search = useSearch()
  const [, navigate] = useLocation()
  const showImpactOnMap = useCallback(
    // Cone overlay lives on /map; drop inspector params so the map is
    // unobstructed. History push — Back returns to the inspector.
    (svc: string) => navigate(buildHref('/map', search, { impact: svc, service: null, tab: null })),
    [navigate, search],
  )
  const status = nodeStatus(node?.status)
  const active = INSPECTOR_TABS.find((t) => t.id === tab) ?? INSPECTOR_TABS[0]

  // Non-modal: the side panel + map stay interactive behind the popup, and
  // clicking a category in the panel must NOT dismiss it — only the X / Escape
  // close (onInteractOutside is prevented below).
  return (
    <Dialog.Root open modal={false} onOpenChange={(open) => !open && onClose()}>
      <Dialog.Portal>
        <Dialog.Content
          className={styles.popup}
          aria-describedby={undefined}
          onInteractOutside={(e) => e.preventDefault()}
        >
          <header className={styles.header}>
            <StatusDot status={status} />
            <Dialog.Title className={styles.name}>{service}</Dialog.Title>
            {node && (
              <span className={styles.health} style={{ color: statusToken(status) }}>
                <Metric value={formatPercent(node.health_score)} />
              </span>
            )}
            <Dialog.Close asChild>
              <button type="button" className={styles.close} aria-label="Close inspector">
                <X size={16} aria-hidden="true" />
              </button>
            </Dialog.Close>
          </header>

          {/* Tabs switch Overview / Why / Impact / Dependencies inside the popup
              on every breakpoint, so the popup is self-sufficient; the desktop
              side rail mirrors the same nav (both drive the shared tab state). */}
          <div className={styles.tabList} role="tablist" aria-label="Inspector sections">
            {INSPECTOR_TABS.map((t) => (
              <button
                key={t.id}
                type="button"
                role="tab"
                aria-selected={t.id === tab}
                data-state={t.id === tab ? 'active' : undefined}
                className={styles.tabTrigger}
                onClick={() => onTab(t.id)}
              >
                {t.label}
              </button>
            ))}
          </div>

          <div className={styles.tabContent}>
            {loading ? (
              <SkeletonBody />
            ) : error ? (
              <div className={styles.statePanel} role="alert">
                <p>Couldn’t load the service graph: {error}</p>
                <button type="button" className={styles.stateAction} onClick={reload}>
                  Retry
                </button>
              </div>
            ) : !node ? (
              <div className={styles.statePanel}>
                <p>
                  <code className={styles.mono}>{service}</code> isn’t in the current service graph — it
                  may have stopped reporting.
                </p>
              </div>
            ) : (
              <active.Content ctx={{ node, edges: graph?.edges ?? [], openService, showImpactOnMap }} />
            )}
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}

/** Mounts when ?service= is present (App gates the lazy import on that).
 * The popup is the sole surface — it carries the category tabs + content on
 * every breakpoint; there is no docked side rail. */
export default function ServiceInspector() {
  const { service, closeInspector } = useInvestigation()
  const search = useSearch()
  // ?tab= targets a registry category (palette verbs / shared links); it seeds
  // local state, re-seeded whenever the param or inspected service changes
  // (adjust-during-render, not an effect).
  const tabParam = readParam(search, 'tab')
  const seedTab = isInspectorTabId(tabParam) ? tabParam : INSPECTOR_TABS[0].id
  const [tab, setTab] = useState(seedTab)
  const [seedKey, setSeedKey] = useState({ tabParam, service })
  if (seedKey.tabParam !== tabParam || seedKey.service !== service) {
    setSeedKey({ tabParam, service })
    setTab(seedTab)
  }

  if (!service) return null

  return <DetailsPopup service={service} tab={tab} onTab={setTab} onClose={closeInspector} />
}
