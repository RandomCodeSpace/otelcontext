import { Badge, Button, Card, Space } from '@ossrandom/design-system'
import { Play, Terminal } from 'lucide-react'
import type { MCPTool } from '@/types/api'

interface Props {
  tool: MCPTool
  index: number
  onCall: (index: number) => void
  onRPC: (index: number) => void
}

export default function ToolCard({ tool, index, onCall, onRPC }: Props) {
  const props = tool.inputSchema?.properties || {}
  const req = tool.inputSchema?.required || []
  const paramCount = Object.keys(props).length

  return (
    <Card
      bordered
      hoverable
      padding="md"
      radius="md"
      title={<code>{tool.name}</code>}
      extra={paramCount > 0 ? <Badge tone="neutral" size="sm">{paramCount}p</Badge> : undefined}
      footer={
        <Space size="xs">
          <Button variant="primary" size="sm" iconLeft={<Play size={10} />} onClick={() => onCall(index)}>
            Call
          </Button>
          <Button variant="ghost" size="sm" iconLeft={<Terminal size={10} />} onClick={() => onRPC(index)}>
            JSON-RPC
          </Button>
        </Space>
      }
    >
      <Space direction="vertical" size="sm">
        <p>{tool.description || 'No description provided.'}</p>
        {paramCount > 0 && (
          <Space size="xs" wrap>
            {Object.entries(props).map(([key, value]) => (
              <Badge key={key} tone={req.includes(key) ? 'danger' : 'neutral'} size="sm">
                <code>{key}:{value.type ?? 'any'}</code>
              </Badge>
            ))}
          </Space>
        )}
      </Space>
    </Card>
  )
}
