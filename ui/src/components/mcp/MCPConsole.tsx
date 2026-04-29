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
    <Space direction="vertical" size="md">
      <Card
        bordered
        padding="lg"
        radius="md"
        title={
          <Space size="xs" align="center">
            <Terminal size={14} />
            <span>MCP Endpoint</span>
          </Space>
        }
        subtitle="Plug any MCP-compatible client (Claude Desktop, Cursor, custom agents) into the URL below."
        extra={<Badge tone="info" size="sm">live</Badge>}
      >
        <Space direction="vertical" size="md">
          <Space size="sm" wrap>
            <Input value={url} readOnly type="url" />
            <Button
              variant="primary"
              size="sm"
              iconLeft={copied ? <Check size={12} /> : <Copy size={12} />}
              onClick={copyUrl}
            >
              {copied ? 'Copied' : 'Copy'}
            </Button>
          </Space>

          <p>
            HTTP Streamable MCP · JSON-RPC 2.0 over POST + Server-Sent Events.
            If <code>API_KEY</code> is set on the server, send{' '}
            <code>Authorization: Bearer &lt;API_KEY&gt;</code> on every request.
          </p>
        </Space>
      </Card>
    </Space>
  )
}
