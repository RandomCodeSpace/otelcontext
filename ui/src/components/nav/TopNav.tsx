import { useState } from 'react'
import {
  Badge,
  Button,
  IconButton,
  Input,
  Space,
} from '@ossrandom/design-system'
import { Check, Copy, Moon, Sun } from 'lucide-react'
import { useTheme } from '../../hooks/useTheme'
import { useMediaQuery } from '../../hooks/useMediaQuery'

// Single-view app: the only "view" is the service map. Kept as a type so the
// AppShell wiring in App.tsx stays open to additional views later.
export type OtelView = 'services'

interface TopNavProps {
  view: OtelView
  onNavigate: (view: OtelView) => void
  wsConnected: boolean
}

export default function TopNav({ wsConnected }: Readonly<TopNavProps>) {
  const { theme, toggle } = useTheme()
  const isCompact = useMediaQuery('(max-width: 760px)')
  const [copied, setCopied] = useState(false)

  // Resolved at render — works in any deployment because it's whatever the
  // browser is already pointed at. Empty during SSR (we run client-only, so
  // this is safe).
  const mcpUrl =
    typeof window !== 'undefined' ? `${window.location.origin}/mcp` : '/mcp'

  const copyMcpUrl = async () => {
    if (typeof navigator === 'undefined' || !navigator.clipboard) return
    await navigator.clipboard.writeText(mcpUrl)
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1500)
  }

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

  const copyBtn = (
    <Button
      variant="primary"
      size="sm"
      iconLeft={copied ? <Check size={12} /> : <Copy size={12} />}
      onClick={copyMcpUrl}
    >
      {copied ? 'Copied' : 'Copy MCP URL'}
    </Button>
  )

  if (isCompact) {
    // Compact: brand + copy-only button + indicators. Skip the URL field
    // because it eats horizontal real estate that we don't have on phones.
    return (
      <Space
        justify="between"
        align="center"
        style={{ display: 'flex', width: '100%', padding: '0.5rem 0.75rem' }}
      >
        <strong>OtelContext</strong>
        <Space size="xs" align="center">
          {copyBtn}
          {liveBadge}
          {themeBtn}
        </Space>
      </Space>
    )
  }

  // Desktop: brand on the left, MCP URL field grows to fill, copy button +
  // status indicators sit on the right. Single-row, no tabs (only one view).
  return (
    <Space
      justify="between"
      align="center"
      style={{ display: 'flex', width: '100%', padding: '0.5rem 1rem', gap: '0.75rem' }}
    >
      <Space size="md" align="center">
        <strong>OtelContext</strong>
      </Space>
      <div style={{ flex: 1, minWidth: 0, maxWidth: 640 }}>
        <Input
          value={mcpUrl}
          readOnly
          type="url"
          size="sm"
          aria-label="MCP endpoint URL"
        />
      </div>
      <Space size="sm" align="center">
        {copyBtn}
        {liveBadge}
        {themeBtn}
      </Space>
    </Space>
  )
}
