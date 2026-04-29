import { useCallback, useRef, useState } from 'react'
import { AppShell } from '@ossrandom/design-system'
import TopNav, { type OtelView } from './components/nav/TopNav'
import ServicesView from './components/observability/ServicesView'
import TracesView from './components/observability/TracesView'
import LogsView from './components/observability/LogsView'
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
        <TopNav view={view} onNavigate={setView} wsConnected={wsConnected} />
      }
    >
      {view === 'services' && (
        <ServicesView
          graph={graph.graph}
          loading={graph.loading}
          error={graph.error}
          dashboard={dash.dashboard}
          stats={dash.stats}
          onNavigateToTraces={navigateToTraces}
          onNavigateToLogs={navigateToLogs}
        />
      )}
      {view === 'traces' && (
        <TracesView
          traces={traces.traces}
          selected={traces.selected}
          loading={traces.loading}
          error={traces.error}
          onSelect={(traceId: string) => void traces.selectTrace(traceId)}
          serviceFilter={serviceFilter}
          onClearFilter={clearFilter}
          dashboard={dash.dashboard}
        />
      )}
      {view === 'logs' && (
        <LogsView
          logs={logs.logs}
          similar={logs.similar}
          loading={logs.loading}
          error={logs.error}
          onSimilar={(query: string) => void logs.runSimilar(query)}
          serviceFilter={serviceFilter}
          onClearFilter={clearFilter}
          dashboard={dash.dashboard}
        />
      )}
      {view === 'mcp' && <MCPConsole />}
    </AppShell>
  )
}
