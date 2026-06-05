import { lazy, Suspense, useState } from 'react'
import { AppShell, Spin } from '@ossrandom/design-system'
import TopNav, { type OtelView } from './components/nav/TopNav'
import DashboardView from './components/dashboard/DashboardView'
import { useWebSocket } from './hooks/useWebSocket'
import type { Theme } from './hooks/useTheme'

// Dashboard is the default view and loads eagerly. The Service Map pulls in
// cytoscape (~434 KB) and the MCP console is a large secondary surface — both
// are code-split so they don't weigh down the initial dashboard-first load.
const ServicesView = lazy(() => import('./components/observability/ServicesView'))
const MCPConsoleView = lazy(() => import('./components/mcp/MCPConsoleView'))

interface AppProps {
  theme: Theme
  onToggleTheme: () => void
}

export default function App({ theme, onToggleTheme }: Readonly<AppProps>) {
  const [view, setView] = useState<OtelView>('dashboard')

  // WebSocket retained purely as the live/offline source for the header badge;
  // the pushed log batches are intentionally discarded.
  const ws = useWebSocket(() => undefined)
  const wsConnected = ws.status === 'connected'

  return (
    <AppShell
      header={
        <TopNav
          view={view}
          onNavigate={setView}
          wsConnected={wsConnected}
          theme={theme}
          onToggleTheme={onToggleTheme}
        />
      }
    >
      <Suspense fallback={<Spin label="Loading…" />}>
        {view === 'dashboard' && <DashboardView onNavigate={setView} />}
        {view === 'services' && <ServicesView />}
        {view === 'mcp' && <MCPConsoleView />}
      </Suspense>
    </AppShell>
  )
}
