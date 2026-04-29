import { Badge, IconButton, PageHeader, Space, Stat, Tabs } from '@ossrandom/design-system'
import { Moon, Sun } from 'lucide-react'
import type { DashboardStats, RepoStats } from '../../types/api'
import { fmt } from '../../lib/utils'
import { useTheme } from '../../hooks/useTheme'

export type OtelView = 'services' | 'traces' | 'logs' | 'mcp'

interface TopNavProps {
  view: OtelView
  onNavigate: (view: OtelView) => void
  dashboard: DashboardStats | null
  stats: RepoStats | null
  wsConnected: boolean
}

const tabs: { key: OtelView; label: string }[] = [
  { key: 'services', label: 'Service Map' },
  { key: 'traces', label: 'Traces' },
  { key: 'logs', label: 'Logs' },
  { key: 'mcp', label: 'MCP' },
]

export default function TopNav({ view, onNavigate, dashboard, stats, wsConnected }: TopNavProps) {
  const { theme, toggle } = useTheme()
  const errorRate = dashboard?.error_rate ?? 0

  const actions = (
    <Space size="md" align="center">
      <Stat label="Services" value={dashboard?.active_services?.toString() ?? '--'} />
      <Stat label="Traces" value={fmt(dashboard?.total_traces ?? 0)} />
      <Stat label="Logs" value={fmt(dashboard?.total_logs ?? 0)} />
      <Stat
        label="Error Rate"
        value={dashboard?.error_rate != null ? dashboard.error_rate.toFixed(1) : '--'}
        unit="%"
        delta={
          errorRate > 0
            ? { value: errorRate, direction: 'up', tone: errorRate > 5 ? 'bad' : 'neutral' }
            : undefined
        }
      />
      <Stat label="DB" value={stats?.db_size_mb != null ? `${stats.db_size_mb}` : '--'} unit="MB" />
      <Badge tone={wsConnected ? 'info' : 'danger'} size="sm">
        {wsConnected ? 'WS' : 'WS · off'}
      </Badge>
      <IconButton
        icon={theme === 'dark' ? <Sun size={15} /> : <Moon size={15} />}
        aria-label="Toggle theme"
        variant="ghost"
        size="sm"
        shape="circle"
        onClick={toggle}
      />
    </Space>
  )

  return (
    <>
      <PageHeader size="xs" title="OtelContext" inlineSubtitle actions={actions} />
      <Tabs<OtelView>
        items={tabs}
        value={view}
        variant="line"
        onChange={(key) => onNavigate(key)}
      />
    </>
  )
}
