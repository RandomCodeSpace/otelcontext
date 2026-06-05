import { Badge, IconButton, Space, Tabs } from '@ossrandom/design-system'
import { Moon, Sun } from 'lucide-react'
import type { Theme } from '../../hooks/useTheme'

export type OtelView = 'dashboard' | 'services' | 'mcp'

interface TopNavProps {
  view: OtelView
  onNavigate: (view: OtelView) => void
  wsConnected: boolean
  theme: Theme
  onToggleTheme: () => void
}

const TABS = [
  { key: 'dashboard' as const, label: 'Dashboard' },
  { key: 'services' as const, label: 'Service Map' },
  { key: 'mcp' as const, label: 'MCP Trial' },
]

export default function TopNav({
  view,
  onNavigate,
  wsConnected,
  theme,
  onToggleTheme,
}: Readonly<TopNavProps>) {
  return (
    <Space
      justify="between"
      align="center"
      style={{ display: 'flex', width: '100%' }}
    >
      <strong style={{ whiteSpace: 'nowrap' }}>OtelContext</strong>

      <div style={{ flex: 1, minWidth: 0, display: 'flex', justifyContent: 'center' }}>
        <Tabs
          items={TABS}
          value={view}
          onChange={onNavigate}
          variant="segment"
          size="sm"
          scrollable
        />
      </div>

      <Space size="sm" align="center">
        <Badge tone={wsConnected ? 'info' : 'danger'} size="sm">
          {wsConnected ? 'live' : 'offline'}
        </Badge>
        <IconButton
          icon={theme === 'dark' ? <Sun size={15} /> : <Moon size={15} />}
          aria-label="Toggle theme"
          variant="ghost"
          size="sm"
          shape="circle"
          onClick={onToggleTheme}
        />
      </Space>
    </Space>
  )
}
