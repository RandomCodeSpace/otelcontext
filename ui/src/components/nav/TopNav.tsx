import { useState } from 'react'
import {
  Badge,
  Drawer,
  IconButton,
  Menu,
  Space,
  Tabs,
} from '@ossrandom/design-system'
import { Menu as MenuIcon, Moon, Network, Radar, Search, Sun, Terminal } from 'lucide-react'
import { useTheme } from '../../hooks/useTheme'
import { useMediaQuery } from '../../hooks/useMediaQuery'

export type OtelView = 'services' | 'traces' | 'logs' | 'mcp'

interface TopNavProps {
  view: OtelView
  onNavigate: (view: OtelView) => void
  wsConnected: boolean
}

const tabs: { key: OtelView; label: string }[] = [
  { key: 'services', label: 'Service Map' },
  { key: 'traces', label: 'Traces' },
  { key: 'logs', label: 'Logs' },
  { key: 'mcp', label: 'MCP' },
]

const menuItems = [
  { key: 'services' as const, label: 'Service Map', icon: <Network size={14} /> },
  { key: 'traces' as const, label: 'Traces', icon: <Search size={14} /> },
  { key: 'logs' as const, label: 'Logs', icon: <Radar size={14} /> },
  { key: 'mcp' as const, label: 'MCP Endpoint', icon: <Terminal size={14} /> },
]

export default function TopNav({ view, onNavigate, wsConnected }: TopNavProps) {
  const { theme, toggle } = useTheme()
  const isCompact = useMediaQuery('(max-width: 760px)')
  const [drawerOpen, setDrawerOpen] = useState(false)

  const themeBtn = (
    <IconButton
      icon={theme === 'dark' ? <Sun size={15} /> : <Moon size={15} />}
      aria-label="Toggle theme"
      variant="ghost"
      size="sm"
      shape="circle"
      onClick={toggle}
    />
  )

  const liveBadge = (
    <Badge tone={wsConnected ? 'info' : 'danger'} size="sm">
      {wsConnected ? 'live' : 'offline'}
    </Badge>
  )

  if (isCompact) {
    return (
      <>
        <Space justify="between" align="center" style={{ padding: '0.5rem 0.75rem' }}>
          <Space size="sm" align="center">
            <IconButton
              icon={<MenuIcon size={16} />}
              aria-label="Open navigation"
              variant="ghost"
              size="sm"
              onClick={() => setDrawerOpen(true)}
            />
            <strong>OtelContext</strong>
          </Space>
          <Space size="xs" align="center">
            {liveBadge}
            {themeBtn}
          </Space>
        </Space>

        <Drawer
          open={drawerOpen}
          onClose={() => setDrawerOpen(false)}
          placement="left"
          width="min(280px, 86vw)"
          title="OtelContext"
        >
          <Menu<OtelView>
            mode="vertical"
            items={menuItems}
            selectedKeys={[view]}
            onSelect={(key) => {
              onNavigate(key)
              setDrawerOpen(false)
            }}
          />
        </Drawer>
      </>
    )
  }

  return (
    <Space justify="between" align="center" style={{ padding: '0.4rem 1rem' }}>
      <Space size="md" align="center">
        <strong>OtelContext</strong>
        <Tabs<OtelView>
          items={tabs}
          value={view}
          variant="line"
          onChange={(key) => onNavigate(key)}
        />
      </Space>
      <Space size="sm" align="center">
        {liveBadge}
        {themeBtn}
      </Space>
    </Space>
  )
}
