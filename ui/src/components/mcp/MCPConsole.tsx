import { useState } from 'react'
import { Alert, Badge, Button, Card, Grid, Space } from '@ossrandom/design-system'
import { Plug, RefreshCw } from 'lucide-react'
import { useMCP } from '@/hooks/useMCP'
import type { MCPTool } from '@/types/api'
import ToolCard from './ToolCard'
import ToolCallModal from './ToolCallModal'
import RPCPopup from './RPCPopup'

const statusTone: Record<string, 'info' | 'warning' | 'danger' | 'neutral'> = {
  idle: 'neutral',
  connecting: 'warning',
  connected: 'info',
  error: 'danger',
}

export default function MCPConsole() {
  const { status, tools, error, call, connect, send } = useMCP()
  const [callTool, setCallTool] = useState<MCPTool | null>(null)
  const [rpcTool, setRpcTool] = useState<MCPTool | null>(null)

  const header = (
    <Space justify="between" align="center" wrap>
      <Space size="xs" align="center" wrap>
        <Badge tone={statusTone[status]} size="sm">{status}</Badge>
        <Space size="xs" align="center">
          <Plug size={11} />
          <code>{window.location.origin}/mcp</code>
        </Space>
        <Badge tone="subtle" size="sm">HTTP Streamable MCP · JSON-RPC 2.0</Badge>
      </Space>
      <Button
        variant="ghost"
        size="sm"
        iconLeft={<RefreshCw size={12} />}
        onClick={() => void connect()}
      >
        Reconnect
      </Button>
    </Space>
  )

  return (
    <Space direction="vertical" size="md">
      <Card bordered padding="md" radius="md">
        {header}
      </Card>

      <Card
        bordered
        padding="md"
        radius="md"
        title="Available Tools"
        extra={<Badge tone="subtle" size="sm">{tools.length} discovered</Badge>}
      >
        {status === 'error' && (
          <Alert severity="danger" title="Connection failed">
            {error || 'Could not reach the MCP endpoint.'} <code>MCP_ENABLED=true</code>
          </Alert>
        )}
        {status === 'connected' && (
          <Grid columns={12} gap="md">
            {tools.map((tool, index) => (
              <Grid.Col key={tool.name} span={4}>
                <ToolCard
                  tool={tool}
                  index={index}
                  onCall={(next) => setCallTool(tools[next])}
                  onRPC={(next) => setRpcTool(tools[next])}
                />
              </Grid.Col>
            ))}
          </Grid>
        )}
      </Card>

      {callTool && (
        <ToolCallModal
          tool={callTool}
          onClose={() => setCallTool(null)}
          onCall={async (name, args) =>
            (await call('tools/call', { name, arguments: args })).result ?? null
          }
        />
      )}
      {rpcTool && <RPCPopup tool={rpcTool} onClose={() => setRpcTool(null)} onSend={send} />}
    </Space>
  )
}
