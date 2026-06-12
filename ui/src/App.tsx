import { lazy, Suspense, useCallback, useState } from 'react'
import { Redirect, Route, Switch, useLocation } from 'wouter'
import { Spin } from '@ossrandom/design-system'
import Shell from './components/shell/Shell'
import TrailBar from './components/trail/TrailBar'
import { useGlobalKeys } from './hooks/useGlobalKeys'
import { useInvestigation } from './hooks/useInvestigation'
import type { OtelView } from './components/dashboard/DashboardView'
import type { Theme } from './hooks/useTheme'
import styles from './App.module.css'

// All routes are code-split; the Inspector is split too and only fetched
// once a ?service= drill-down actually happens. The ⌘K palette + shortcut
// sheet chunk loads on the first open request — zero cost until then.
const TriageView = lazy(() => import('./components/triage/TriageView'))
const FlowMapView = lazy(() => import('./components/map/FlowMapView'))
const DashboardView = lazy(() => import('./components/dashboard/DashboardView'))
const MCPConsoleView = lazy(() => import('./components/mcp/MCPConsoleView'))
const TracesView = lazy(() => import('./components/traces/TracesView'))
const LogsView = lazy(() => import('./components/logs/LogsView'))
const ServiceInspector = lazy(() => import('./components/inspector/ServiceInspector'))
const PaletteHost = lazy(() => import('./components/palette/PaletteHost'))

// Legacy view ids (DashboardView's onNavigate) → router paths.
const VIEW_PATHS: Record<OtelView, string> = {
  dashboard: '/dashboard',
  services: '/map',
  mcp: '/mcp',
}

interface AppProps {
  theme: Theme
  onToggleTheme: () => void
}

export default function App({ theme, onToggleTheme }: Readonly<AppProps>) {
  const [, navigate] = useLocation()
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
          <Suspense fallback={<Spin label="Loading…" />}>
            <Switch>
              <Route path="/" component={TriageView} />
              <Route path="/map" component={FlowMapView} />
              <Route path="/traces" component={TracesView} />
              <Route path="/logs" component={LogsView} />
              <Route path="/dashboard">
                <DashboardView onNavigate={(view) => navigate(VIEW_PATHS[view])} />
              </Route>
              <Route path="/mcp" component={MCPConsoleView} />
              {/* Unknown paths land on the Triage home. */}
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
