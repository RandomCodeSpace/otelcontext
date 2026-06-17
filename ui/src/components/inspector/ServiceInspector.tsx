import { useCallback, useRef, useState, type ReactNode } from 'react'
import * as Dialog from '@radix-ui/react-dialog'
import * as Tabs from '@radix-ui/react-tabs'
import { X } from 'lucide-react'
import { useLocation, useSearch } from 'wouter'
import { useInvestigation } from '@/hooks/useInvestigation'
import { useSystemGraph } from '@/hooks/useSystemGraph'
import { useMediaQuery } from '@/hooks/useMediaQuery'
import { formatPercent } from '@/lib/format'
import { Metric } from '@/components/common/Metric'
import { nodeStatus, statusToken } from '@/lib/triage'
import { nextSheetState, type SheetSnap } from '@/lib/sheet'
import { buildHref, readParam } from '@/lib/urlState'
import type { SystemNode } from '@/types/api'
import { INSPECTOR_TABS, isInspectorTabId } from './registry'
import styles from './ServiceInspector.module.css'

// Service Inspector — the omnipresent ?service=X panel:
//   xs  (<768)  bottom sheet (Radix dialog), snap 50/92dvh, swipe-down dismiss
//   md  (<1024) fixed overlay sheet on the right (CSS-only difference)
//   lg+         docked grid column (380px / 440px at xl), content reflows

function Header({
  node,
  service,
  onClose,
}: Readonly<{ node: SystemNode | null; service: string; onClose: () => void }>) {
  const status = nodeStatus(node?.status)
  return (
    <header className={styles.header}>
      <span
        className={styles.statusDot}
        style={{ background: statusToken(status) }}
        aria-hidden="true"
      />
      <h2 className={styles.name}>{service}</h2>
      {node && (
        <span className={styles.health} style={{ color: statusToken(status) }}>
          <Metric value={formatPercent(node.health_score)} />
        </span>
      )}
      <button
        type="button"
        className={styles.close}
        aria-label="Close inspector"
        onClick={onClose}
      >
        <X size={16} aria-hidden="true" />
      </button>
    </header>
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

function InspectorBody({
  service,
  onClose,
}: Readonly<{ service: string; onClose: () => void }>) {
  const { openService } = useInvestigation()
  const { graph, loading, error, reload } = useSystemGraph()
  const node = graph?.nodes.find((n) => n.id === service) ?? null

  const search = useSearch()
  const [, navigate] = useLocation()
  // ?tab= targets a registry tab (palette verbs / shared links). The param
  // seeds local state; manual tab flips stay local — re-seeded whenever the
  // param or the inspected service changes (adjust-during-render pattern,
  // not an effect: https://react.dev/learn/you-might-not-need-an-effect).
  const tabParam = readParam(search, 'tab')
  const seedTab = isInspectorTabId(tabParam) ? tabParam : INSPECTOR_TABS[0].id
  const [tab, setTab] = useState(seedTab)
  const [seedKey, setSeedKey] = useState({ tabParam, service })
  if (seedKey.tabParam !== tabParam || seedKey.service !== service) {
    setSeedKey({ tabParam, service })
    setTab(seedTab)
  }

  const showImpactOnMap = useCallback(
    (svc: string) => {
      // Cone overlay lives on /map; drop inspector params so the map is
      // unobstructed. History push — Back returns to the inspector.
      navigate(
        buildHref('/map', search, { impact: svc, service: null, tab: null }),
      )
    },
    [navigate, search],
  )

  if (loading) {
    return (
      <>
        <Header node={null} service={service} onClose={onClose} />
        <SkeletonBody />
      </>
    )
  }

  if (error) {
    return (
      <>
        <Header node={null} service={service} onClose={onClose} />
        <div className={styles.statePanel} role="alert">
          <p>Couldn’t load the service graph: {error}</p>
          <button type="button" className={styles.stateAction} onClick={reload}>
            Retry
          </button>
        </div>
      </>
    )
  }

  if (!node) {
    return (
      <>
        <Header node={null} service={service} onClose={onClose} />
        <div className={styles.statePanel}>
          <p>
            <code className={styles.mono}>{service}</code> isn’t in the current
            service graph — it may have stopped reporting.
          </p>
        </div>
      </>
    )
  }

  const ctx = {
    node,
    edges: graph?.edges ?? [],
    openService,
    showImpactOnMap,
  }
  return (
    <>
      <Header node={node} service={service} onClose={onClose} />
      <Tabs.Root value={tab} onValueChange={setTab} className={styles.tabs}>
        <Tabs.List className={styles.tabList} aria-label="Inspector sections">
          {INSPECTOR_TABS.map((tab) => (
            <Tabs.Trigger key={tab.id} value={tab.id} className={styles.tabTrigger}>
              {tab.label}
            </Tabs.Trigger>
          ))}
        </Tabs.List>
        {INSPECTOR_TABS.map((tab) => (
          <Tabs.Content key={tab.id} value={tab.id} className={styles.tabContent}>
            <tab.Content ctx={ctx} />
          </Tabs.Content>
        ))}
      </Tabs.Root>
    </>
  )
}

function BottomSheet({
  onDismiss,
  service,
  children,
}: Readonly<{ onDismiss: () => void; service: string; children: ReactNode }>) {
  const [snap, setSnap] = useState<SheetSnap>(50)
  const [dragOffset, setDragOffset] = useState(0)
  const dragStart = useRef<number | null>(null)

  const onPointerDown = useCallback((e: React.PointerEvent<HTMLDivElement>) => {
    dragStart.current = e.clientY
    e.currentTarget.setPointerCapture(e.pointerId)
  }, [])

  const onPointerMove = useCallback((e: React.PointerEvent<HTMLDivElement>) => {
    if (dragStart.current === null) return
    setDragOffset(e.clientY - dragStart.current)
  }, [])

  const onPointerUp = useCallback(
    (e: React.PointerEvent<HTMLDivElement>) => {
      if (dragStart.current === null) return
      const delta = e.clientY - dragStart.current
      dragStart.current = null
      setDragOffset(0)
      const next = nextSheetState(snap, delta, window.innerHeight)
      if (next === 'dismiss') onDismiss()
      else setSnap(next)
    },
    [onDismiss, snap],
  )

  return (
    <Dialog.Root open onOpenChange={(open) => !open && onDismiss()}>
      <Dialog.Portal>
        <Dialog.Overlay className={styles.sheetOverlay} />
        <Dialog.Content
          className={styles.sheet}
          style={{
            height: `${snap}dvh`,
            transform: dragOffset > 0 ? `translateY(${dragOffset}px)` : undefined,
          }}
          aria-describedby={undefined}
        >
          <Dialog.Title className={styles.srOnly}>
            Service inspector: {service}
          </Dialog.Title>
          <div
            className={styles.sheetHandle}
            data-testid="sheet-handle"
            onPointerDown={onPointerDown}
            onPointerMove={onPointerMove}
            onPointerUp={onPointerUp}
          >
            <span className={styles.sheetGrip} aria-hidden="true" />
          </div>
          <div className={styles.sheetScroll}>{children}</div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}

/** Mounts when ?service= is present (App gates the lazy import on that). */
export default function ServiceInspector() {
  const { service, closeInspector } = useInvestigation()
  const isXs = useMediaQuery('(max-width: 767px)')

  if (!service) return null

  if (isXs) {
    return (
      <BottomSheet onDismiss={closeInspector} service={service}>
        <InspectorBody service={service} onClose={closeInspector} />
      </BottomSheet>
    )
  }

  return (
    <aside
      className={styles.panel}
      aria-label={`Service inspector: ${service}`}
      onKeyDown={(e) => {
        if (e.key === 'Escape') closeInspector()
      }}
    >
      <InspectorBody service={service} onClose={closeInspector} />
    </aside>
  )
}
