import { useState } from 'react'
import { AppShell } from '@ossrandom/design-system'
import TopNav, { type OtelView } from './components/nav/TopNav'
import ServicesView from './components/observability/ServicesView'
import { useSystemGraph } from './hooks/useSystemGraph'
import { useDashboard } from './hooks/useDashboard'
import { useWebSocket } from './hooks/useWebSocket'

export default function App() {
  const [view, setView] = useState<OtelView>('services')

  const graph = useSystemGraph()
  const dash = useDashboard()

  // WebSocket retained as the live/offline source for the header indicator;
  // log batches it pushes are intentionally discarded.
  const ws = useWebSocket(() => undefined)
  const wsConnected = !!ws.current

  return (
    <AppShell
      header={
        <TopNav view={view} onNavigate={setView} wsConnected={wsConnected} />
      }
    >
      <ServicesView
        graph={graph.graph}
        loading={graph.loading}
        error={graph.error}
        dashboard={dash.dashboard}
        stats={dash.stats}
      />
    </AppShell>
  )
}
