// Note: MCPConsole is no longer mounted in App.tsx — the MCP endpoint URL
// + Copy button now live in the TopNav header. This file is kept temporarily
// as orphaned source pending a follow-up cleanup pass; it is tree-shaken out
// of the production bundle.
import { useState } from 'react'
import { Badge, Button, Card, Input, Space } from '@ossrandom/design-system'
import { Check, Copy, Terminal } from 'lucide-react'

export default function MCPConsole() {
  const url = `${window.location.origin}/mcp`
  const [copied, setCopied] = useState(false)

  const copyUrl = async () => {
    await navigator.clipboard.writeText(url)
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1500)
  }

  return (
    <Space direction="vertical" size="md" style={{ display: 'flex', width: '100%' }}>
      <Card bordered padding="lg" radius="md">
        <Space direction="vertical" size="sm" style={{ display: 'flex', width: '100%' }}>
          <Space size="xs" align="center">
            <Terminal size={14} />
            <strong>MCP Endpoint</strong>
            <Badge tone="info" size="sm">live</Badge>
          </Space>
          <Space size="sm" wrap style={{ display: 'flex', width: '100%' }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <Input value={url} readOnly type="url" />
            </div>
            <Button
              variant="primary"
              size="sm"
              iconLeft={copied ? <Check size={12} /> : <Copy size={12} />}
              onClick={copyUrl}
            >
              {copied ? 'Copied' : 'Copy'}
            </Button>
          </Space>
        </Space>
      </Card>
    </Space>
  )
}
