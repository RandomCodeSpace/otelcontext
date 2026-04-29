import { useCallback, useRef, useState } from 'react'
import { AppShell } from '@ossrandom/design-system'
import TopNav, { type OtelView } from './components/nav/TopNav'
import ServiceMap from './components/observability/ServiceMap'
import TracesPage from './components/observability/TracesPage'
import LogsPage from './components/observability/LogsPage'
import MCPConsole from './components/mcp/MCPConsole'
import { useSystemGraph } from './hooks/useSystemGraph'
import { useDashboard } from './hooks/useDashboard'
import { useTraces } from './hooks/useTraces'
import { useLogs } from './hooks/useLogs'
import { useWebSocket } from './hooks/useWebSocket'
import type { LogEntry } from './types/api'

export default function App() {
  const [view, setView] = useState<OtelView>('services')
  const [serviceFilter, setServiceFilter] = useState<string | null>(null)

  const graph = useSystemGraph()
  const dash = useDashboard()
  const traces = useTraces()
  const logs = useLogs()

  const setLogsRef = useRef(logs.setLogs)
  setLogsRef.current = logs.setLogs
  const appendLogs = useCallback((incoming: LogEntry[]) => {
    setLogsRef.current((current) => [...incoming, ...current].slice(0, 200))
  }, [])

  const ws = useWebSocket(appendLogs)
  const wsConnected = !!ws.current

  const navigateToTraces = useCallback((service: string) => {
    setServiceFilter(service)
    setView('traces')
  }, [])

  const navigateToLogs = useCallback((service: string) => {
    setServiceFilter(service)
    setView('logs')
  }, [])

  const clearFilter = useCallback(() => {
    setServiceFilter(null)
  }, [])

  return (
    <AppShell
      header={
        <TopNav
          view={view}
          onNavigate={setView}
          dashboard={dash.dashboard}
          stats={dash.stats}
          wsConnected={wsConnected}
        />
      }
    >
      {view === 'services' && (
        <ServiceMap
          graph={graph.graph}
          cache={graph.cache}
          loading={graph.loading}
          error={graph.error}
          onNavigateToTraces={navigateToTraces}
          onNavigateToLogs={navigateToLogs}
        />
      )}
      {view === 'traces' && (
        <TracesPage
          traces={traces.traces}
          selected={traces.selected}
          loading={traces.loading}
          error={traces.error}
          onSelect={(traceId) => void traces.selectTrace(traceId)}
          serviceFilter={serviceFilter}
          onClearFilter={clearFilter}
        />
      )}
      {view === 'logs' && (
        <LogsPage
          logs={logs.logs}
          similar={logs.similar}
          loading={logs.loading}
          error={logs.error}
          onSimilar={(query) => void logs.runSimilar(query)}
          serviceFilter={serviceFilter}
          onClearFilter={clearFilter}
        />
      )}
      {view === 'mcp' && <MCPConsole />}
    </AppShell>
  )
}
