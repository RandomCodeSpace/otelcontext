import { lazy, Suspense, useCallback, useState } from 'react'
import { Redirect, Route, Switch } from 'wouter'
import RouteFallback from './components/common/RouteFallback'
import Shell from './components/shell/Shell'
import TrailBar from './components/trail/TrailBar'
import { useGlobalKeys } from './hooks/useGlobalKeys'
import { useInvestigation } from './hooks/useInvestigation'
import type { Theme } from './hooks/useTheme'
import styles from './App.module.css'

// All routes are code-split; the Inspector is split too and only fetched
// once a ?service= drill-down actually happens. The ⌘K palette + shortcut
// sheet chunk loads on the first open request — zero cost until then.
const TriageView = lazy(() => import('./components/triage/TriageView'))
const FlowMapView = lazy(() => import('./components/map/FlowMapView'))
const TracesView = lazy(() => import('./components/traces/TracesView'))
const LogsView = lazy(() => import('./components/logs/LogsView'))
const ServiceInspector = lazy(() => import('./components/inspector/ServiceInspector'))
const PaletteHost = lazy(() => import('./components/palette/PaletteHost'))

interface AppProps {
  theme: Theme
  onToggleTheme: () => void
}

export default function App({ theme, onToggleTheme }: Readonly<AppProps>) {
  const { service, trail, popToFrame, popOne } = useInvestigation()

  const [paletteOpen, setPaletteOpen] = useState(false)
  const [shortcutsOpen, setShortcutsOpen] = useState(false)
  // Latches true on the first open request and stays — the chunk loads
  // once, after which open/close is instant.
  const [overlaysWanted, setOverlaysWanted] = useState(false)

  const openPalette = useCallback(() => {
    setOverlaysWanted(true)
    setPaletteOpen(true)
  }, [])
  const togglePalette = useCallback(() => {
    setOverlaysWanted(true)
    setPaletteOpen((open) => !open)
  }, [])
  const openShortcuts = useCallback(() => {
    setOverlaysWanted(true)
    setShortcutsOpen(true)
  }, [])
  useGlobalKeys({ onPalette: togglePalette, onShortcuts: openShortcuts })

  return (
    <Shell
      theme={theme}
      onToggleTheme={onToggleTheme}
      onOpenPalette={openPalette}
    >
      <div className={styles.layout}>
        <div className={styles.routes}>
          <Suspense fallback={<RouteFallback />}>
            <Switch>
              <Route path="/" component={TriageView} />
              <Route path="/map" component={FlowMapView} />
              <Route path="/traces" component={TracesView} />
              <Route path="/logs" component={LogsView} />
              {/* Unknown paths (incl. the retired /dashboard and /mcp)
                  land on the Triage home. */}
              <Route>
                <Redirect to="/" replace />
              </Route>
            </Switch>
          </Suspense>
        </div>
        {service !== null && (
          <Suspense fallback={null}>
            <ServiceInspector />
          </Suspense>
        )}
      </div>
      <TrailBar frames={trail} onPopTo={popToFrame} onPopOne={popOne} />
      {overlaysWanted && (
        <Suspense fallback={null}>
          <PaletteHost
            paletteOpen={paletteOpen}
            onPaletteOpenChange={setPaletteOpen}
            shortcutsOpen={shortcutsOpen}
            onShortcutsOpenChange={setShortcutsOpen}
            onToggleTheme={onToggleTheme}
          />
        </Suspense>
      )}
    </Shell>
  )
}
