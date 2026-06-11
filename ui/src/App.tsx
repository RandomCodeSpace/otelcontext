import { lazy, Suspense } from 'react'
import { Redirect, Route, Switch, useLocation } from 'wouter'
import { Spin } from '@ossrandom/design-system'
import Shell from './components/shell/Shell'
import type { OtelView } from './components/dashboard/DashboardView'
import type { Theme } from './hooks/useTheme'

// All routes are code-split: /map pulls in cytoscape (~434 KB) and the
// other views carry design-system surfaces the shell itself doesn't need.
const ServicesView = lazy(() => import('./components/observability/ServicesView'))
const DashboardView = lazy(() => import('./components/dashboard/DashboardView'))
const MCPConsoleView = lazy(() => import('./components/mcp/MCPConsoleView'))
const TracesView = lazy(() => import('./components/traces/TracesView'))
const LogsView = lazy(() => import('./components/logs/LogsView'))

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

  return (
    <Shell theme={theme} onToggleTheme={onToggleTheme}>
      <Suspense fallback={<Spin label="Loading…" />}>
        <Switch>
          <Route path="/map" component={ServicesView} />
          <Route path="/traces" component={TracesView} />
          <Route path="/logs" component={LogsView} />
          <Route path="/dashboard">
            <DashboardView onNavigate={(view) => navigate(VIEW_PATHS[view])} />
          </Route>
          <Route path="/mcp" component={MCPConsoleView} />
          {/* "/" and anything unknown → /map until the Triage home lands. */}
          <Route>
            <Redirect to="/map" replace />
          </Route>
        </Switch>
      </Suspense>
    </Shell>
  )
}
